package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// Workspace confines filesystem access to one canonical directory tree.
type Workspace struct {
	path string
	root *os.Root
	// replaceForTest is an internal fault seam. Production always uses the
	// platform replaceFile implementation.
	replaceForTest func(*os.Root, string, string, string, string) error

	mu       sync.Mutex
	closed   bool
	active   bool
	revision uint64
	lock     *mutationLock
	session  string
}

// Open resolves root to a canonical absolute directory without creating any
// on-disk state. Mutation storage and locking are lazy.
func Open(rootPath string) (*Workspace, error) {
	if rootPath == "" {
		rootPath = "."
	}
	abs, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root is not a directory: %s", canonical)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, fmt.Errorf("open workspace: %w", err)
	}
	session, err := randomID("session")
	if err != nil {
		root.Close()
		return nil, err
	}
	return &Workspace{path: canonical, root: root, session: session}, nil
}

func (w *Workspace) Path() string { return w.path }

func (w *Workspace) checkOpen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return nil
}

func (w *Workspace) beginOperation(ctx context.Context, allowPending bool) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	if w.active {
		w.mu.Unlock()
		return ErrBusy
	}
	w.active = true
	w.mu.Unlock()

	if err := w.acquireMutationLock(ctx); err != nil {
		w.endOperation()
		return err
	}
	if !allowPending {
		state, err := w.reconcileRecoveryState()
		if err != nil {
			w.endOperation()
			return err
		}
		view, err := w.recoveryStateView(state)
		if err != nil {
			w.endOperation()
			return err
		}
		if view.Blocked {
			w.endOperation()
			return ErrRecoveryPending
		}
	}
	return nil
}

func (w *Workspace) endOperation() {
	w.mu.Lock()
	w.active = false
	w.mu.Unlock()
}

func (w *Workspace) acquireMutationLock(ctx context.Context) error {
	w.mu.Lock()
	if w.lock != nil {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	if err := w.ensureRecoveryDir(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lockFile, err := w.openLockFile()
	if err != nil {
		return err
	}
	candidate, locked, err := tryMutationLock(lockFile)
	if err != nil {
		_ = lockFile.Close()
		return fmt.Errorf("acquire workspace lock: %w", err)
	}
	if !locked {
		_ = lockFile.Close()
		return ErrMutationLocked
	}
	w.mu.Lock()
	if w.lock != nil {
		w.mu.Unlock()
		_ = candidate.Close()
		return nil
	}
	w.lock = candidate
	w.mu.Unlock()
	return nil
}

func (w *Workspace) openLockFile() (*os.File, error) {
	const lockName = RecoveryDir + "/mutation.lock"
	info, err := w.root.Lstat(lockName)
	if errors.Is(err, fs.ErrNotExist) {
		file, createErr := w.root.OpenFile(lockName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			if chmodErr := file.Chmod(0o600); chmodErr != nil {
				_ = file.Close()
				return nil, chmodErr
			}
			return file, nil
		}
		if !errors.Is(createErr, fs.ErrExist) {
			return nil, createErr
		}
		info, err = w.root.Lstat(lockName)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: recovery lock is not a regular file", ErrUnsafeFile)
	}
	file, err := w.root.OpenFile(lockName, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	current, currentErr := w.root.Lstat(lockName)
	if statErr != nil || currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(info, opened) || !os.SameFile(opened, current) {
		_ = file.Close()
		if statErr != nil {
			return nil, statErr
		}
		if currentErr != nil {
			return nil, currentErr
		}
		return nil, fmt.Errorf("%w: recovery lock changed while opening", ErrUnsafeFile)
	}
	linked, linkErr := hasMultipleLinks(file, opened)
	if linkErr != nil || linked {
		_ = file.Close()
		if linkErr != nil {
			return nil, linkErr
		}
		return nil, fmt.Errorf("%w: recovery lock is hard-linked", ErrUnsafeFile)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (w *Workspace) ensureRecoveryDir() error {
	info, err := w.root.Lstat(RecoveryDir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := w.root.Mkdir(RecoveryDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create recovery directory: %w", err)
		}
		info, err = w.root.Lstat(RecoveryDir)
	}
	if err != nil {
		return fmt.Errorf("inspect recovery directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s must be a real directory", ErrUnsafeFile, RecoveryDir)
	}
	if err := w.root.Chmod(RecoveryDir, 0o700); err != nil {
		return fmt.Errorf("secure recovery directory: %w", err)
	}
	return nil
}

func safeRelative(name string) (string, error) {
	if name == "" || name == "." || strings.ContainsRune(name, '\x00') || filepath.IsAbs(name) {
		return "", ErrUnsafePath
	}
	slash := filepath.ToSlash(name)
	if path.Clean(slash) != slash || slash == ".." || strings.HasPrefix(slash, "../") {
		return "", ErrUnsafePath
	}
	for _, part := range strings.Split(slash, "/") {
		if part == "" || part == "." || part == ".." {
			return "", ErrUnsafePath
		}
	}
	return slash, nil
}

// readSafe rejects symlinks in every component, non-regular files, hard links,
// oversized files, identity swaps while opening, and optional version
// conflicts. When expectedIdentity is supplied, an identical-byte replacement
// is still a conflict.
func (w *Workspace) readSafe(name string, expectedHash *[sha256.Size]byte, expectedIdentity *FileIdentity) ([]byte, fs.FileInfo, FileIdentity, error) {
	rel, err := safeRelative(name)
	if err != nil {
		return nil, nil, FileIdentity{}, err
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		component := strings.Join(parts[:i+1], "/")
		info, err := w.root.Lstat(filepath.FromSlash(component))
		if err != nil {
			return nil, nil, FileIdentity{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, FileIdentity{}, fmt.Errorf("%w: %s is a symlink", ErrUnsafeFile, component)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, nil, FileIdentity{}, fmt.Errorf("%w: %s is not a directory", ErrUnsafePath, component)
		}
	}

	native := filepath.FromSlash(rel)
	file, err := w.root.Open(native)
	if err != nil {
		return nil, nil, FileIdentity{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, nil, FileIdentity{}, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, FileIdentity{}, ErrUnsafeFile
	}
	identity, err := identityFromFile(file, before)
	if err != nil {
		return nil, nil, FileIdentity{}, fmt.Errorf("capture file identity: %w", err)
	}
	if expectedIdentity != nil && (!expectedIdentity.Valid() || identity != *expectedIdentity) {
		return nil, nil, FileIdentity{}, fmt.Errorf("%w: filesystem identity mismatch", ErrConflict)
	}
	linked, err := hasMultipleLinks(file, before)
	if err != nil {
		return nil, nil, FileIdentity{}, err
	}
	if linked {
		return nil, nil, FileIdentity{}, ErrHardlink
	}
	if before.Size() > MaxFileSize {
		return nil, nil, FileIdentity{}, fmt.Errorf("%w: file is larger than %d bytes", ErrUnsafeFile, MaxFileSize)
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxFileSize+1))
	if err != nil {
		return nil, nil, FileIdentity{}, err
	}
	if int64(len(raw)) > MaxFileSize {
		return nil, nil, FileIdentity{}, fmt.Errorf("%w: file grew beyond size limit", ErrUnsafeFile)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, FileIdentity{}, err
	}
	afterIdentity, err := identityFromFile(file, after)
	if err != nil {
		return nil, nil, FileIdentity{}, fmt.Errorf("recapture file identity: %w", err)
	}
	current, err := w.root.Lstat(native)
	if err != nil {
		return nil, nil, FileIdentity{}, err
	}
	if current.Mode()&os.ModeSymlink != 0 || identity != afterIdentity || !os.SameFile(before, after) || !os.SameFile(after, current) || after.Size() != int64(len(raw)) {
		return nil, nil, FileIdentity{}, fmt.Errorf("%w: file identity changed while reading", ErrConflict)
	}
	linked, err = hasMultipleLinks(file, current)
	if err != nil {
		return nil, nil, FileIdentity{}, err
	}
	if linked {
		return nil, nil, FileIdentity{}, fmt.Errorf("%w: file became hard-linked while reading", ErrHardlink)
	}
	hash := sha256.Sum256(raw)
	if expectedHash != nil && hash != *expectedHash {
		return nil, nil, FileIdentity{}, fmt.Errorf("%w: SHA-256 mismatch", ErrConflict)
	}
	return raw, after, identity, nil
}

func randomID(prefix string) (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate identifier: %w", err)
	}
	return fmt.Sprintf("%s-%x", prefix, value[:]), nil
}

// Close releases the mutation lock and root handle. Call CleanupSession first
// on a normal application exit.
func (w *Workspace) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	if w.active {
		w.mu.Unlock()
		return ErrBusy
	}
	w.closed = true
	lock := w.lock
	w.lock = nil
	w.mu.Unlock()

	var result error
	if lock != nil {
		result = errors.Join(result, lock.Close())
	}
	result = errors.Join(result, w.root.Close())
	return result
}
