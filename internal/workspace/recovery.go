package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Recoveries returns unresolved crash-recovery material newest-first. The
// official persistent undo point is intentionally excluded; use State or
// UndoInfo to inspect it.
func (w *Workspace) Recoveries(ctx context.Context) ([]Recovery, error) {
	state, err := w.State(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Recovery, 0, len(state.Items))
	for _, item := range state.Items {
		if item.Role == RecoveryRoleUndo {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

// Undo restores the persistent one-level undo point, including one created by
// an earlier process invocation.
func (w *Workspace) Undo(ctx context.Context, progress func(Progress)) (MutationSummary, error) {
	return w.UndoScoped(ctx, "", nil, progress)
}

// UndoScoped restores the current undo only if expectedID is empty or still
// current and every transaction target is in allowedPaths. A nil allowedPaths
// permits the whole workspace; an empty non-nil slice permits no target.
func (w *Workspace) UndoScoped(ctx context.Context, expectedID string, allowedPaths []string, progress func(Progress)) (MutationSummary, error) {
	if err := w.beginOperation(ctx, true); err != nil {
		return MutationSummary{}, err
	}
	defer w.endOperation()
	state, err := w.reconcileRecoveryState()
	if err != nil {
		return MutationSummary{}, err
	}
	if state.Current == "" {
		return MutationSummary{}, ErrNoUndo
	}
	if expectedID != "" && expectedID != state.Current {
		return MutationSummary{}, fmt.Errorf("%w: expected %s, current is %s", ErrUndoChanged, expectedID, state.Current)
	}
	if blocked, err := w.hasNonUndoBlocker(state); err != nil {
		return MutationSummary{}, err
	} else if blocked {
		return MutationSummary{}, ErrRecoveryPending
	}
	manifest, err := w.loadManifest(state.Current)
	if err != nil {
		return MutationSummary{}, err
	}
	if manifest.Status != txComplete && manifest.Status != txUndoPartial && manifest.Status != txUndoing {
		return MutationSummary{}, fmt.Errorf("%w: current undo has status %s", ErrRecoveryState, manifest.Status)
	}
	if err := ensureRecoveryScope(manifest, allowedPaths); err != nil {
		return MutationSummary{}, err
	}
	return w.restoreLocked(ctx, state, manifest, RecoveryRoleUndo, progress)
}

// RestoreRecovery restores exact backup bytes for pending or orphan recovery
// material. Passing the current undo ID has normal undo semantics.
func (w *Workspace) RestoreRecovery(ctx context.Context, id string, progress func(Progress)) (MutationSummary, error) {
	return w.RestoreRecoveryScoped(ctx, id, nil, progress)
}

// RestoreRecoveryScoped is RestoreRecovery with an exact root-relative target
// allow-list. It refuses an operation whose manifest names any other file.
func (w *Workspace) RestoreRecoveryScoped(ctx context.Context, id string, allowedPaths []string, progress func(Progress)) (MutationSummary, error) {
	if err := w.beginOperation(ctx, true); err != nil {
		return MutationSummary{}, err
	}
	defer w.endOperation()
	if !validRecoveryID(id) {
		return MutationSummary{}, ErrUnsafePath
	}
	state, err := w.reconcileRecoveryState()
	if err != nil {
		return MutationSummary{}, err
	}
	role := recoveryRoleForID(state, id)
	if role == RecoveryRoleGarbage {
		return MutationSummary{}, ErrRecoveryPending
	}
	exists, existsErr := w.hasManifestID(id)
	if existsErr != nil {
		return MutationSummary{}, existsErr
	}
	if role == "" && !exists {
		return MutationSummary{}, fs.ErrNotExist
	}
	manifest, err := w.loadManifest(id)
	if err != nil {
		return MutationSummary{}, err
	}
	if err := ensureRecoveryScope(manifest, allowedPaths); err != nil {
		return MutationSummary{}, err
	}
	if role == "" {
		role = RecoveryRoleOrphan
	}
	retireCurrent := role == RecoveryRoleOrphan || (role == RecoveryRolePending && manifest.PreviousUndoID != state.Current)
	if allowedPaths != nil && retireCurrent && state.Current != "" && state.Current != id {
		currentManifest, currentErr := w.loadManifest(state.Current)
		if currentErr != nil {
			return MutationSummary{}, fmt.Errorf("%w: cannot prove paths in current undo %s", ErrRecoveryScope, state.Current)
		}
		if err := ensureRecoveryScope(currentManifest, allowedPaths); err != nil {
			return MutationSummary{}, err
		}
	}
	if role == RecoveryRoleUndo && manifest.Status != txComplete && manifest.Status != txUndoPartial && manifest.Status != txUndoing {
		return MutationSummary{}, fmt.Errorf("%w: current undo has status %s", ErrRecoveryState, manifest.Status)
	}
	return w.restoreLocked(ctx, state, manifest, role, progress)
}

func (w *Workspace) restoreLocked(ctx context.Context, state *recoveryStateFile, manifest *transactionManifest, role RecoveryRole, progress func(Progress)) (MutationSummary, error) {
	if role == RecoveryRoleUndo {
		manifest.Status = txUndoing
	} else {
		manifest.Status = txRecoveryPartial
	}
	if err := w.saveManifest(manifest); err != nil {
		return MutationSummary{}, fmt.Errorf("record recovery attempt: %w", err)
	}

	summary := MutationSummary{TransactionID: manifest.ID}
	for index := range manifest.Files {
		if err := ctx.Err(); err != nil {
			summary.Cancelled = true
			for rest := index; rest < len(manifest.Files); rest++ {
				summary.add(FileResult{RelativePath: manifest.Files[rest].RelativePath, Status: FileNotAttempted})
			}
			break
		}
		result := w.restoreFile(manifest, index)
		summary.add(result)
		if progress != nil {
			progress(Progress{Completed: index + 1, Total: len(manifest.Files), Result: result})
		}
	}

	if summary.Conflicted > 0 || summary.Failed > 0 || summary.Cancelled {
		if role == RecoveryRoleUndo {
			manifest.Status = txUndoPartial
		} else {
			manifest.Status = txRecoveryPartial
		}
		if err := w.saveManifest(manifest); err != nil {
			summary.RecoveryRetained = true
			stateUndoSummary(&summary, state)
			return summary, err
		}
		summary.RecoveryRetained = true
		stateUndoSummary(&summary, state)
		return summary, nil
	}

	next := cloneRecoveryState(state)
	switch role {
	case RecoveryRoleUndo:
		if next.Current != manifest.ID {
			return summary, fmt.Errorf("%w: undo pointer changed", ErrUndoChanged)
		}
		next.Current = ""
	case RecoveryRolePending:
		if next.Pending != manifest.ID {
			return summary, fmt.Errorf("%w: pending pointer changed", ErrRecoveryState)
		}
		next.Pending = ""
		if manifest.PreviousUndoID != next.Current {
			// The pending transaction does not prove it began from the current
			// undo chain. Restoring it is safe for file contents, but accepting
			// that chronology invalidates the older undo point.
			oldCurrent := next.Current
			next.Current = ""
			appendGarbage(next, oldCurrent)
		}
	case RecoveryRoleOrphan:
		// An orphan's chronology relative to current cannot be proven. Accepting
		// its restored state invalidates the earlier one-level undo chain.
		oldCurrent := next.Current
		next.Current = ""
		appendGarbage(next, oldCurrent)
	default:
		return summary, fmt.Errorf("%w: cannot restore role %s", ErrRecoveryState, role)
	}
	appendGarbage(next, manifest.ID)
	if err := w.commitRecoveryState(next); err != nil {
		summary.RecoveryRetained = true
		stateUndoSummary(&summary, state)
		return summary, fmt.Errorf("publish completed recovery: %w", err)
	}
	state = next
	if err := w.cleanupGarbage(state); err != nil {
		summary.RecoveryRetained = true
		stateUndoSummary(&summary, state)
		return summary, err
	}
	stateUndoSummary(&summary, state)
	return summary, nil
}

func (w *Workspace) restoreFile(manifest *transactionManifest, index int) FileResult {
	entry := &manifest.Files[index]
	result := FileResult{RelativePath: entry.RelativePath}
	if entry.BackupPath == "" {
		result.Status = FileSkipped
		return result
	}
	// Planned/failed files were never replaced, except that a failed state from
	// a prior partial restore is retried while the manifest is undoing/partial.
	partialRestore := manifest.Status == txUndoing || manifest.Status == txUndoPartial || manifest.Status == txRecoveryPartial
	if entry.State == statePlanned || entry.State == stateBackedUp || (entry.State == stateFailed && !partialRestore) {
		result.Status = FileSkipped
		return result
	}
	if entry.State == stateConflict && !partialRestore {
		result.Status = FileSkipped
		return result
	}
	backup, _, err := w.readRecoveryFile(entry.BackupPath, MaxFileSize)
	if err != nil {
		result.Status, result.Err = FileFailed, fmt.Errorf("read backup: %w", err)
		entry.State, entry.Error = stateFailed, result.Err.Error()
		_ = w.saveManifest(manifest)
		return result
	}
	if sha256.Sum256(backup) != entry.OriginalHash {
		result.Status, result.Err = FileFailed, errors.New("backup digest does not match manifest")
		entry.State, entry.Error = stateFailed, result.Err.Error()
		_ = w.saveManifest(manifest)
		return result
	}
	current, _, _, err := w.readSafe(entry.RelativePath, nil, nil)
	if err != nil {
		result.Status, result.Err = FileConflicted, fmt.Errorf("inspect current target: %w", err)
		entry.State, entry.Error = stateConflict, result.Err.Error()
		_ = w.saveManifest(manifest)
		return result
	}
	currentHash := sha256.Sum256(current)
	if currentHash == entry.OriginalHash {
		result.Status = FileRestored
		entry.State, entry.Error = stateRestored, ""
		_ = w.saveManifest(manifest)
		return result
	}
	if currentHash != entry.PostHash {
		result.Status, result.Err = FileConflicted, fmt.Errorf("%w: current file matches neither transaction hash", ErrConflict)
		entry.State, entry.Error = stateConflict, result.Err.Error()
		_ = w.saveManifest(manifest)
		return result
	}
	warnings, attempted, err := w.atomicReplace(entry.RelativePath, entry.PostHash, nil, backup, savedMetadata{
		mode: fs.FileMode(entry.Mode), modTime: entry.ModTime, uid: entry.UID, gid: entry.GID,
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			result.Status = FileConflicted
			entry.State = stateConflict
		} else if attempted {
			result.Status = FileFailed
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
	result.Status, result.Warnings = FileRestored, warnings
	entry.State, entry.Error = stateRestored, ""
	if err := w.saveManifest(manifest); err != nil {
		result.Warnings = append(result.Warnings, "restored file but could not update manifest: "+err.Error())
	}
	return result
}

// DiscardRecovery permanently deletes one explicitly identified backup set.
// Confirmation belongs in the caller/UI.
func (w *Workspace) DiscardRecovery(ctx context.Context, id string) error {
	return w.DiscardRecoveryScoped(ctx, id, nil)
}

// DiscardRecoveryScoped is DiscardRecovery with an exact root-relative target
// allow-list. Pending/orphan discard also retires the old undo because an
// accepted ambiguous later state can invalidate that earlier undo chain.
func (w *Workspace) DiscardRecoveryScoped(ctx context.Context, id string, allowedPaths []string) error {
	if err := w.beginOperation(ctx, true); err != nil {
		return err
	}
	defer w.endOperation()
	if !validRecoveryID(id) {
		return ErrUnsafePath
	}
	state, err := w.reconcileRecoveryState()
	if err != nil {
		return err
	}
	role := recoveryRoleForID(state, id)
	exists, existsErr := w.hasManifestID(id)
	if existsErr != nil {
		return existsErr
	}
	if role == "" && !exists {
		return fs.ErrNotExist
	}
	manifest, manifestErr := w.loadManifest(id)
	if allowedPaths != nil {
		if manifestErr != nil {
			return fmt.Errorf("%w: cannot prove paths in unreadable recovery %s", ErrRecoveryScope, id)
		}
		if err := ensureRecoveryScope(manifest, allowedPaths); err != nil {
			return err
		}
		if (role == RecoveryRolePending || role == RecoveryRoleOrphan || role == "") && state.Current != "" && state.Current != id {
			currentManifest, currentErr := w.loadManifest(state.Current)
			if currentErr != nil {
				return fmt.Errorf("%w: cannot prove paths in current undo %s", ErrRecoveryScope, state.Current)
			}
			if err := ensureRecoveryScope(currentManifest, allowedPaths); err != nil {
				return err
			}
		}
	}

	next := cloneRecoveryState(state)
	var retire []string
	switch role {
	case RecoveryRoleUndo:
		next.Current = ""
		retire = append(retire, id)
	case RecoveryRolePending:
		next.Pending = ""
		retire = append(retire, id, next.Current)
		next.Current = ""
	case RecoveryRoleGarbage:
		// It is already durably retired. Explicit discard first publishes that
		// its directory is no longer expected, then removes it; a delete failure
		// is reported but no longer blocks unrelated future work.
		removeGarbageID(next, id)
	case RecoveryRoleOrphan, "":
		retire = append(retire, id, next.Current)
		next.Current = ""
	default:
		return fmt.Errorf("%w: unknown recovery role %s", ErrRecoveryState, role)
	}
	appendGarbage(next, retire...)
	if err := w.commitRecoveryState(next); err != nil {
		return fmt.Errorf("publish recovery discard: %w", err)
	}
	state = next
	if role == RecoveryRoleGarbage {
		if err := w.removeTransaction(id); err != nil {
			return err
		}
	}
	return w.cleanupGarbage(state)
}

func recoveryRoleForID(state *recoveryStateFile, id string) RecoveryRole {
	switch id {
	case state.Current:
		return RecoveryRoleUndo
	case state.Pending:
		return RecoveryRolePending
	}
	if slices.Contains(state.Garbage, id) {
		return RecoveryRoleGarbage
	}
	return ""
}

func (w *Workspace) hasManifestID(id string) (bool, error) {
	ids, err := w.manifestIDs()
	if err != nil {
		return false, err
	}
	return slices.Contains(ids, id), nil
}

func (w *Workspace) hasNonUndoBlocker(state *recoveryStateFile) (bool, error) {
	view, err := w.recoveryStateView(state)
	if err != nil {
		return false, err
	}
	for _, item := range view.Items {
		if item.ID != state.Current && item.BlocksMutation {
			return true, nil
		}
	}
	return false, nil
}

func (w *Workspace) removeTransaction(id string) error {
	if !validRecoveryID(id) {
		return ErrUnsafePath
	}
	rel := filepath.FromSlash(RecoveryDir + "/" + id)
	info, err := w.root.Lstat(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		// Explicit discard may remove an invalid transaction entry, but must
		// never follow it. Root.Remove removes the link/file itself.
		if err := w.root.Remove(rel); err != nil {
			return err
		}
		return syncRootDirectory(w.root, RecoveryDir)
	}
	if err := w.root.RemoveAll(rel); err != nil {
		return err
	}
	return syncRootDirectory(w.root, RecoveryDir)
}

// CleanupSession performs safe recovery maintenance without deleting the
// persistent undo point. It may retire proven no-op remnants and garbage;
// ambiguous or corrupt recovery material is retained for explicit handling.
func (w *Workspace) CleanupSession(ctx context.Context) error {
	if err := w.beginOperation(ctx, true); err != nil {
		return err
	}
	defer w.endOperation()
	state, err := w.reconcileRecoveryState()
	if err != nil {
		return err
	}
	view, err := w.recoveryStateView(state)
	if err != nil {
		return err
	}
	var warnings []error
	for _, item := range view.Items {
		if item.Role == RecoveryRoleCorrupt {
			warnings = append(warnings, fmt.Errorf("recovery transaction %s is unreadable and was retained: %w", item.ID, item.Err))
		}
	}
	return errors.Join(warnings...)
}
