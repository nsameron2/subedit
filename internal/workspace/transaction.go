package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"subedit/internal/subtitle"
)

// Transaction applies one plan a file at a time. A caller may call ApplyOne
// from successive Bubble Tea commands; each invocation completes at most one
// atomic target replacement.
type Transaction struct {
	workspace *Workspace
	plan      MutationPlan
	manifest  *transactionManifest
	next      int
	summary   MutationSummary
	finished  bool
}

// Begin creates and durably records a transaction. No subtitle file is changed
// until ApplyOne is called.
func (w *Workspace) Begin(ctx context.Context, plan MutationPlan) (*Transaction, error) {
	if err := w.beginOperation(ctx, false); err != nil {
		return nil, err
	}
	fail := func(err error) (*Transaction, error) {
		w.endOperation()
		return nil, err
	}
	if len(plan.Files) == 0 {
		return fail(errors.New("mutation plan has no files"))
	}
	state, err := w.reconcileRecoveryState()
	if err != nil {
		return fail(err)
	}
	view, err := w.recoveryStateView(state)
	if err != nil {
		return fail(err)
	}
	if view.Blocked {
		return fail(ErrRecoveryPending)
	}
	if current := atomic.LoadUint64(&w.revision); plan.SearchRevision != 0 && plan.SearchRevision != current {
		return fail(fmt.Errorf("%w: plan revision %d, current revision %d", ErrStalePlan, plan.SearchRevision, current))
	}
	seen := make(map[string]struct{}, len(plan.Files))
	for i := range plan.Files {
		rel, err := safeRelative(plan.Files[i].RelativePath)
		if err != nil {
			return fail(fmt.Errorf("invalid mutation path: %w", err))
		}
		if !subtitle.SupportedExtension(rel) {
			return fail(fmt.Errorf("%w: %s", ErrUnsupportedFile, rel))
		}
		if !plan.Files[i].ExpectedIdentity.Valid() {
			return fail(fmt.Errorf("mutation path %s has no filesystem identity", rel))
		}
		if _, duplicate := seen[rel]; duplicate {
			return fail(fmt.Errorf("duplicate mutation path: %s", rel))
		}
		seen[rel] = struct{}{}
		plan.Files[i].RelativePath = rel
		plan.Files[i].CueIDs = slices.Clone(plan.Files[i].CueIDs)
	}
	id, err := randomID("tx")
	if err != nil {
		return fail(err)
	}
	manifest := &transactionManifest{
		Version: manifestVersion, ID: id, SessionID: w.session,
		CreatedAt: time.Now().UTC(), Status: txActive, Scope: plan.Scope,
		PreviousUndoID: state.Current, Files: make([]manifestFile, len(plan.Files)),
	}
	for i, mutation := range plan.Files {
		cueIDs := make([]string, len(mutation.CueIDs))
		for j := range mutation.CueIDs {
			cueIDs[j] = string(mutation.CueIDs[j])
		}
		manifest.Files[i] = manifestFile{
			RelativePath: mutation.RelativePath, OriginalHash: mutation.ExpectedSHA256,
			CueIDs: cueIDs, State: statePlanned,
		}
	}
	if err := w.saveManifest(manifest); err != nil {
		return fail(fmt.Errorf("prepare transaction manifest: %w", err))
	}
	nextState := cloneRecoveryState(state)
	nextState.Pending = id
	if err := w.commitRecoveryState(nextState); err != nil {
		// The manifest is an unreferenced, provably non-writing active remnant.
		// Reconciliation will retire it; never start writes without durable
		// pending ownership.
		return fail(fmt.Errorf("record pending transaction: %w", err))
	}
	return &Transaction{
		workspace: w, plan: plan, manifest: manifest,
		summary: MutationSummary{TransactionID: id},
	}, nil
}

// Apply is the convenient synchronous wrapper around Begin/ApplyOne/Finish.
func (w *Workspace) Apply(ctx context.Context, plan MutationPlan, progress func(Progress)) (MutationSummary, error) {
	tx, err := w.Begin(ctx, plan)
	if err != nil {
		return MutationSummary{}, err
	}
	for {
		result, err := tx.ApplyOne(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_, finishErr := tx.Finish()
			return tx.summary, errors.Join(err, finishErr)
		}
		if progress != nil {
			progress(Progress{Completed: tx.next, Total: len(tx.plan.Files), Result: result})
		}
	}
	return tx.Finish()
}

// ApplyOne performs the next file operation. Context cancellation is checked
// only before starting a file; once backup preparation begins, that one file is
// carried through an atomic decision before cancellation is observed again.
func (tx *Transaction) ApplyOne(ctx context.Context) (FileResult, error) {
	if tx.finished {
		return FileResult{}, ErrTransactionClosed
	}
	if tx.next >= len(tx.plan.Files) {
		return FileResult{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		tx.summary.Cancelled = true
		return FileResult{}, io.EOF
	}

	mutation := tx.plan.Files[tx.next]
	index := tx.next
	tx.next++
	result := tx.workspace.applyFile(tx.manifest, index, mutation)
	tx.summary.add(result)
	return result, nil
}

func (w *Workspace) applyFile(manifest *transactionManifest, index int, mutation FileMutation) FileResult {
	result := FileResult{RelativePath: mutation.RelativePath}
	entry := &manifest.Files[index]

	raw, info, _, err := w.readSafe(mutation.RelativePath, &mutation.ExpectedSHA256, &mutation.ExpectedIdentity)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			result.Status = FileConflicted
			entry.State = stateConflict
		} else if errors.Is(err, ErrUnsafeFile) || errors.Is(err, ErrUnsafePath) {
			result.Status = FileSkipped
			entry.State = stateFailed
		} else {
			result.Status = FileFailed
			entry.State = stateFailed
		}
		result.Err = err
		entry.Error = err.Error()
		_ = w.saveManifest(manifest)
		return result
	}
	document, err := subtitle.Parse(mutation.RelativePath, raw)
	if err != nil {
		result.Status, result.Err = FileFailed, fmt.Errorf("reparse current subtitle: %w", err)
		entry.State, entry.Error = stateFailed, result.Err.Error()
		_ = w.saveManifest(manifest)
		return result
	}
	rendered, err := document.Delete(mutation.CueIDs)
	if err != nil {
		result.Status, result.Err = FileFailed, fmt.Errorf("render deletion: %w", err)
		entry.State, entry.Error = stateFailed, result.Err.Error()
		_ = w.saveManifest(manifest)
		return result
	}
	if bytes.Equal(raw, rendered) {
		result.Status = FileSkipped
		entry.State = stateFailed
		entry.Error = "deletion rendered no change"
		_ = w.saveManifest(manifest)
		return result
	}
	if err := document.ValidateDeletion(rendered, mutation.CueIDs); err != nil {
		result.Status, result.Err = FileFailed, fmt.Errorf("validate rendered subtitle: %w", err)
		entry.State, entry.Error = stateFailed, result.Err.Error()
		_ = w.saveManifest(manifest)
		return result
	}

	entry.Mode = uint32(info.Mode())
	entry.ModTime = info.ModTime()
	entry.UID, entry.GID = ownership(info)
	entry.PostHash = sha256.Sum256(rendered)
	entry.BackupPath = path.Join(RecoveryDir, manifest.ID, "backup", fmt.Sprintf("%06d.bin", index))
	if err := w.writeBackup(entry.BackupPath, raw); err != nil {
		result.Status, result.Err = FileFailed, fmt.Errorf("write recovery backup: %w", err)
		entry.State, entry.Error = stateFailed, result.Err.Error()
		_ = w.saveManifest(manifest)
		return result
	}
	entry.State = stateBackedUp
	if err := w.saveManifest(manifest); err != nil {
		result.Status, result.Err = FileFailed, fmt.Errorf("record durable backup: %w", err)
		entry.Error = result.Err.Error()
		return result
	}
	entry.State = statePrepared
	if err := w.saveManifest(manifest); err != nil {
		result.Status, result.Err = FileFailed, fmt.Errorf("record prepared replacement: %w", err)
		entry.State = stateBackedUp
		entry.Error = result.Err.Error()
		return result
	}

	warnings, attempted, err := w.atomicReplace(mutation.RelativePath, mutation.ExpectedSHA256, &mutation.ExpectedIdentity, rendered, savedMetadata{
		mode: info.Mode(), modTime: info.ModTime(), uid: entry.UID, gid: entry.GID,
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			result.Status = FileConflicted
			entry.State = stateConflict
		} else if attempted {
			result.Status = FileFailed
			// Preserve prepared: once replacement was attempted, portable APIs do
			// not give us a transactional proof that every error means the target
			// stayed unchanged. Recovery will compare both known hashes.
			entry.State = statePrepared
		} else {
			result.Status = FileFailed
			entry.State = stateFailed
		}
		result.Err, result.Warnings = err, warnings
		entry.Error = err.Error()
		_ = w.saveManifest(manifest)
		return result
	}
	entry.State = stateCommitted
	entry.Error = ""
	if err := w.saveManifest(manifest); err != nil {
		// The target is already safely changed. Keep the prepared manifest: hash
		// inference during recovery can prove it committed.
		result.Status, result.Err = FileSucceeded, fmt.Errorf("subtitle changed, but commit state could not be recorded: %w", err)
		result.DeletedCues = len(mutation.CueIDs)
		result.Warnings = append(warnings, result.Err.Error())
		return result
	}
	result.Status = FileSucceeded
	result.DeletedCues = len(mutation.CueIDs)
	result.Warnings = warnings
	return result
}

func (w *Workspace) writeBackup(rel string, raw []byte) error {
	directory := path.Dir(rel)
	if err := w.root.MkdirAll(filepath.FromSlash(directory), 0o700); err != nil {
		return err
	}
	if err := w.root.Chmod(filepath.FromSlash(directory), 0o700); err != nil {
		return err
	}
	file, err := w.root.OpenFile(filepath.FromSlash(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
		return errors.Join(writeErr, closeErr)
	}
	return syncRootDirectory(w.root, filepath.FromSlash(directory))
}

type savedMetadata struct {
	mode    fs.FileMode
	modTime time.Time
	uid     int
	gid     int
}

func (w *Workspace) atomicReplace(rel string, expected [32]byte, expectedIdentity *FileIdentity, content []byte, metadata savedMetadata) ([]string, bool, error) {
	native := filepath.FromSlash(rel)
	target := filepath.Join(w.path, native)
	directoryRel := filepath.Dir(native)
	if directoryRel == "." {
		directoryRel = ""
	}
	tempName, err := randomID(".subedit-tmp")
	if err != nil {
		return nil, false, err
	}
	tempRel := tempName
	if directoryRel != "" {
		tempRel = filepath.Join(directoryRel, tempName)
	}
	tempFile, err := w.root.OpenFile(tempRel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	temp := filepath.Join(w.path, tempRel)
	committed := false
	defer func() {
		if !committed {
			_ = w.root.Remove(tempRel)
		}
	}()
	if err := tempFile.Chmod(metadata.mode.Perm()); err != nil {
		tempFile.Close()
		return nil, false, err
	}
	if _, err := tempFile.Write(content); err != nil {
		tempFile.Close()
		return nil, false, err
	}
	sourceFile, sourceErr := w.root.Open(native)
	var warnings []string
	if sourceErr != nil {
		warnings = append(warnings, "could not open source metadata: "+sourceErr.Error())
	} else {
		warnings = copyExtendedMetadata(sourceFile, tempFile)
		_ = sourceFile.Close()
	}
	if warning := setOwnershipAndMode(tempFile, metadata.uid, metadata.gid, metadata.mode); warning != "" {
		warnings = append(warnings, warning)
	}

	// Validate the actual staged handle, not an absolute path that may have
	// diverged if the workspace directory was renamed.
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		tempFile.Close()
		return nil, false, err
	}
	staged, err := io.ReadAll(io.LimitReader(tempFile, int64(len(content))+1))
	if err != nil {
		tempFile.Close()
		return nil, false, err
	}
	if !bytes.Equal(staged, content) {
		tempFile.Close()
		return nil, false, errors.New("staged replacement failed byte validation")
	}
	if err := setModificationTime(tempFile, metadata.modTime); err != nil {
		warnings = append(warnings, "could not preserve modification time: "+err.Error())
	}
	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		return nil, false, err
	}
	if err := tempFile.Close(); err != nil {
		return nil, false, err
	}
	if _, _, _, err := w.readSafe(rel, &expected, expectedIdentity); err != nil {
		return nil, false, err
	}
	replace := replaceFile
	if w.replaceForTest != nil {
		replace = w.replaceForTest
	}
	if err := replace(w.root, tempRel, native, temp, target); err != nil {
		return warnings, true, err
	}
	committed = true
	if err := syncRootDirectory(w.root, directoryRel); err != nil {
		warnings = append(warnings, "replacement committed but parent sync failed: "+err.Error())
	}
	return warnings, true, nil
}

// Finish closes a transaction, marks not-attempted files, and establishes the
// one-level undo point whenever at least one replacement succeeded.
func (tx *Transaction) Finish() (MutationSummary, error) {
	if tx.finished {
		return tx.summary, ErrTransactionClosed
	}
	tx.finished = true
	for tx.next < len(tx.plan.Files) {
		result := FileResult{RelativePath: tx.plan.Files[tx.next].RelativePath, Status: FileNotAttempted}
		tx.summary.add(result)
		tx.next++
	}
	ambiguous := manifestHasPrepared(tx.manifest)
	if ambiguous {
		tx.manifest.Status = txApplyPartial
	} else {
		tx.manifest.Status = txComplete
	}
	err := tx.workspace.saveManifest(tx.manifest)
	state, _, stateErr := tx.workspace.loadRecoveryState()
	if stateErr != nil {
		err = errors.Join(err, stateErr)
	} else if state.Pending != tx.manifest.ID {
		err = errors.Join(err, fmt.Errorf("%w: transaction %s is not the pending transaction", ErrRecoveryState, tx.manifest.ID))
	} else if state.Current != tx.manifest.PreviousUndoID {
		err = errors.Join(err, fmt.Errorf("%w: previous undo pointer changed", ErrRecoveryState))
	} else if err == nil && ambiguous {
		// At least one replacement returned an error after preparation. Keep
		// the whole operation pending so its exact backups cannot be superseded
		// until the user restores or explicitly accepts/discards that state.
		err = ErrRecoveryPending
	} else if err == nil {
		next := cloneRecoveryState(state)
		next.Pending = ""
		if tx.summary.Succeeded > 0 {
			previous := next.Current
			next.Current = tx.manifest.ID
			appendGarbage(next, previous)
		} else if manifestHasUncertainChange(tx.manifest) {
			// A zero-success summary is not enough to discard a transaction whose
			// manifest reached preparation. Keep it pending for explicit recovery.
			err = errors.Join(err, ErrRecoveryPending)
		} else {
			appendGarbage(next, tx.manifest.ID)
		}
		if err != nil {
			stateUndoSummary(&tx.summary, state)
		} else if transitionErr := tx.workspace.commitRecoveryState(next); transitionErr != nil {
			err = errors.Join(err, fmt.Errorf("publish transaction outcome: %w", transitionErr))
		} else {
			state = next
			if cleanupErr := tx.workspace.cleanupGarbage(state); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
	}
	if state != nil {
		stateUndoSummary(&tx.summary, state)
	}
	if err != nil {
		tx.summary.RecoveryRetained = true
	}
	tx.workspace.endOperation()
	return tx.summary, err
}
