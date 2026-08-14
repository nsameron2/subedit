package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const manifestVersion = 1

const (
	txActive          = "active"
	txComplete        = "complete"
	txApplyPartial    = "apply-partial"
	txUndoing         = "undoing"
	txUndoPartial     = "undo-partial"
	txRecoveryPartial = "recovery-partial"
)

const (
	statePlanned   = "planned"
	stateBackedUp  = "backed-up"
	statePrepared  = "prepared"
	stateCommitted = "committed"
	stateRestored  = "restored"
	stateConflict  = "conflict"
	stateFailed    = "failed"
)

type transactionManifest struct {
	Version        int            `json:"version"`
	ID             string         `json:"id"`
	SessionID      string         `json:"session_id"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	Status         string         `json:"status"`
	Scope          DeleteScope    `json:"scope,omitempty"`
	PreviousUndoID string         `json:"previous_undo_id,omitempty"`
	Files          []manifestFile `json:"files"`
}

type manifestFile struct {
	RelativePath string    `json:"relative_path"`
	OriginalHash [32]byte  `json:"original_sha256"`
	PostHash     [32]byte  `json:"post_sha256"`
	Mode         uint32    `json:"mode"`
	ModTime      time.Time `json:"mod_time"`
	UID          int       `json:"uid,omitempty"`
	GID          int       `json:"gid,omitempty"`
	CueIDs       []string  `json:"cue_ids,omitempty"`
	BackupPath   string    `json:"backup_path,omitempty"`
	State        string    `json:"state"`
	Error        string    `json:"error,omitempty"`
}

func (w *Workspace) saveManifest(manifest *transactionManifest) error {
	manifest.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := path.Join(RecoveryDir, manifest.ID)
	if err := w.root.MkdirAll(filepath.FromSlash(dir), 0o700); err != nil {
		return err
	}
	if err := w.root.Chmod(filepath.FromSlash(dir), 0o700); err != nil {
		return err
	}
	temp := path.Join(dir, "manifest.tmp")
	final := path.Join(dir, "manifest.json")
	file, err := w.root.OpenFile(filepath.FromSlash(temp), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// A previous crash may leave the private staging name behind. Only remove
		// it after proving it is an ordinary file below our confined root.
		if info, statErr := w.root.Lstat(filepath.FromSlash(temp)); statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			if statErr != nil {
				return statErr
			}
			return ErrUnsafeFile
		}
		if removeErr := w.root.Remove(filepath.FromSlash(temp)); removeErr != nil {
			return removeErr
		}
		file, err = w.root.OpenFile(filepath.FromSlash(temp), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if err != nil {
		return err
	}
	writeErr := func() error {
		if err := file.Chmod(0o600); err != nil {
			return err
		}
		if _, err := file.Write(raw); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = w.root.Remove(filepath.FromSlash(temp))
		return errors.Join(writeErr, closeErr)
	}
	if err := w.root.Rename(filepath.FromSlash(temp), filepath.FromSlash(final)); err != nil {
		_ = w.root.Remove(filepath.FromSlash(temp))
		return err
	}
	return syncRootDirectory(w.root, filepath.FromSlash(dir))
}

func (w *Workspace) loadManifest(id string) (*transactionManifest, error) {
	if !validRecoveryID(id) {
		return nil, ErrUnsafePath
	}
	rel := path.Join(RecoveryDir, id, "manifest.json")
	raw, info, err := w.readRecoveryFile(rel, 4<<20)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("manifest has unsafe permissions %o", info.Mode().Perm())
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest transactionManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("manifest contains trailing JSON values")
		}
		return nil, fmt.Errorf("decode manifest trailer: %w", err)
	}
	if manifest.Version != manifestVersion || manifest.ID != id || manifest.SessionID == "" {
		return nil, errors.New("invalid recovery manifest identity or version")
	}
	if manifest.PreviousUndoID != "" && (!validRecoveryID(manifest.PreviousUndoID) || !strings.HasPrefix(manifest.PreviousUndoID, "tx-")) {
		return nil, errors.New("invalid previous undo transaction ID")
	}
	for _, file := range manifest.Files {
		if _, err := safeRelative(file.RelativePath); err != nil {
			return nil, fmt.Errorf("invalid manifest target: %w", err)
		}
		if file.BackupPath != "" {
			prefix := path.Join(RecoveryDir, id) + "/"
			if !strings.HasPrefix(file.BackupPath, prefix) {
				return nil, errors.New("backup path escapes transaction directory")
			}
		}
	}
	return &manifest, nil
}

func (w *Workspace) readRecoveryFile(rel string, limit int64) ([]byte, fs.FileInfo, error) {
	clean, err := safeRelative(rel)
	if err != nil || !strings.HasPrefix(clean, RecoveryDir+"/") {
		return nil, nil, ErrUnsafePath
	}
	parts := strings.Split(clean, "/")
	for i := range parts {
		component := filepath.FromSlash(strings.Join(parts[:i+1], "/"))
		info, err := w.root.Lstat(component)
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, ErrUnsafeFile
		}
	}
	file, err := w.root.Open(filepath.FromSlash(clean))
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, nil, ErrUnsafeFile
	}
	raw := make([]byte, info.Size())
	if _, err := io.ReadFull(file, raw); err != nil {
		return nil, nil, err
	}
	return raw, info, nil
}

func validRecoveryID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
			return false
		}
	}
	return true
}

func (w *Workspace) manifestIDs() ([]string, error) {
	info, err := w.root.Lstat(RecoveryDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafeFile
	}
	dir, err := w.root.Open(RecoveryDir)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if !validRecoveryID(entry.Name()) || !strings.HasPrefix(entry.Name(), "tx-") {
			continue
		}
		// Include unexpected file types and symlinks so loadManifest surfaces a
		// corrupt recovery item and blocks mutation instead of silently ignoring
		// potentially tampered transaction state.
		ids = append(ids, entry.Name())
	}
	sort.Strings(ids)
	return ids, nil
}
