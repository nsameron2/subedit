package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	recoveryStateVersion = 1
	recoveryStatePath    = RecoveryDir + "/state.json"
)

// recoveryStateFile is the sole durable owner of the one-level undo pointer.
// A transaction directory is never deleted until its ID is first recorded in
// Garbage. Pending survives any ambiguous failure between Begin and Finish.
type recoveryStateFile struct {
	Version    int      `json:"version"`
	Generation uint64   `json:"generation"`
	Current    string   `json:"current,omitempty"`
	Pending    string   `json:"pending,omitempty"`
	Garbage    []string `json:"garbage,omitempty"`
}

func newRecoveryState() *recoveryStateFile {
	return &recoveryStateFile{Version: recoveryStateVersion}
}

func (w *Workspace) loadRecoveryState() (*recoveryStateFile, bool, error) {
	raw, info, err := w.readRecoveryFile(recoveryStatePath, 1<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return newRecoveryState(), false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read recovery state: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("%w: recovery state has unsafe permissions %o", ErrRecoveryState, info.Mode().Perm())
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var state recoveryStateFile
	if err := decoder.Decode(&state); err != nil {
		return nil, false, fmt.Errorf("%w: decode recovery state: %v", ErrRecoveryState, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, false, fmt.Errorf("%w: recovery state contains trailing JSON", ErrRecoveryState)
		}
		return nil, false, fmt.Errorf("%w: decode recovery state trailer: %v", ErrRecoveryState, err)
	}
	if err := validateRecoveryState(&state); err != nil {
		return nil, false, err
	}
	return &state, true, nil
}

func validateRecoveryState(state *recoveryStateFile) error {
	if state == nil || state.Version != recoveryStateVersion {
		return fmt.Errorf("%w: unsupported state version", ErrRecoveryState)
	}
	seen := make(map[string]string)
	check := func(id, role string) error {
		if id == "" {
			return nil
		}
		if !validRecoveryID(id) || !strings.HasPrefix(id, "tx-") {
			return fmt.Errorf("%w: invalid %s transaction ID", ErrRecoveryState, role)
		}
		if previous, duplicate := seen[id]; duplicate {
			return fmt.Errorf("%w: transaction %s is both %s and %s", ErrRecoveryState, id, previous, role)
		}
		seen[id] = role
		return nil
	}
	if err := check(state.Current, "current"); err != nil {
		return err
	}
	if err := check(state.Pending, "pending"); err != nil {
		return err
	}
	for _, id := range state.Garbage {
		if err := check(id, "garbage"); err != nil {
			return err
		}
	}
	return nil
}

func (w *Workspace) saveRecoveryState(state *recoveryStateFile) error {
	if err := validateRecoveryState(state); err != nil {
		return err
	}
	if err := w.ensureRecoveryDir(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tempID, err := randomID("state")
	if err != nil {
		return err
	}
	temp := filepath.FromSlash(RecoveryDir + "/." + tempID + ".tmp")
	file, err := w.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
		_ = w.root.Remove(temp)
		return errors.Join(writeErr, closeErr)
	}
	if err := w.root.Rename(temp, filepath.FromSlash(recoveryStatePath)); err != nil {
		_ = w.root.Remove(temp)
		return err
	}
	return syncRootDirectory(w.root, RecoveryDir)
}

func cloneRecoveryState(state *recoveryStateFile) *recoveryStateFile {
	copy := *state
	copy.Garbage = slices.Clone(state.Garbage)
	return &copy
}

// commitRecoveryState increments the generation exactly once for a logical
// pointer transition. Callers mutate a clone and publish it atomically.
func (w *Workspace) commitRecoveryState(state *recoveryStateFile) error {
	state.Generation++
	return w.saveRecoveryState(state)
}

func appendGarbage(state *recoveryStateFile, ids ...string) {
	seen := make(map[string]bool, len(state.Garbage)+len(ids))
	for _, id := range state.Garbage {
		seen[id] = true
	}
	for _, id := range ids {
		if id != "" && id != state.Current && id != state.Pending && !seen[id] {
			state.Garbage = append(state.Garbage, id)
			seen[id] = true
		}
	}
	sort.Strings(state.Garbage)
}

func removeGarbageID(state *recoveryStateFile, id string) {
	for index, item := range state.Garbage {
		if item == id {
			state.Garbage = slices.Delete(state.Garbage, index, index+1)
			return
		}
	}
}

func (w *Workspace) loadOrMigrateRecoveryState() (*recoveryStateFile, error) {
	state, exists, err := w.loadRecoveryState()
	if err != nil || exists {
		return state, err
	}
	ids, err := w.manifestIDs()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return state, nil
	}

	// Pre-state releases had an implicit newest complete transaction as their
	// session undo. Adopt only the newest eligible, restorable manifest; every other
	// potentially meaningful directory remains an explicit orphan.
	type manifestRef struct {
		id       string
		manifest *transactionManifest
	}
	var current, pending []manifestRef
	var retire []string
	for _, id := range ids {
		manifest, loadErr := w.loadManifest(id)
		if loadErr != nil {
			continue
		}
		switch manifest.Status {
		case txComplete:
			if manifestCanAffectTargets(manifest) {
				current = append(current, manifestRef{id, manifest})
			} else {
				retire = append(retire, id)
			}
		case txActive:
			if manifestAutoRetirable(manifest) {
				retire = append(retire, id)
			} else {
				pending = append(pending, manifestRef{id, manifest})
			}
		}
	}
	newest := func(refs []manifestRef) string {
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].manifest.CreatedAt.Equal(refs[j].manifest.CreatedAt) {
				return refs[i].id > refs[j].id
			}
			return refs[i].manifest.CreatedAt.After(refs[j].manifest.CreatedAt)
		})
		if len(refs) == 0 {
			return ""
		}
		return refs[0].id
	}
	state.Current = newest(current)
	state.Pending = newest(pending)
	if state.Current != "" && state.Current == state.Pending {
		state.Pending = ""
	}
	for _, ref := range current {
		if ref.id != state.Current {
			retire = append(retire, ref.id)
		}
	}
	appendGarbage(state, retire...)
	if err := w.commitRecoveryState(state); err != nil {
		return nil, fmt.Errorf("migrate recovery state: %w", err)
	}
	if err := w.cleanupGarbage(state); err != nil {
		return state, err
	}
	return state, nil
}

func manifestHasCommitted(manifest *transactionManifest) bool {
	for _, file := range manifest.Files {
		if file.State == stateCommitted {
			return true
		}
	}
	return false
}

func manifestHasUncertainChange(manifest *transactionManifest) bool {
	for _, file := range manifest.Files {
		switch file.State {
		case statePrepared, stateCommitted:
			return true
		}
	}
	return false
}

func manifestHasPrepared(manifest *transactionManifest) bool {
	for _, file := range manifest.Files {
		if file.State == statePrepared {
			return true
		}
	}
	return false
}

func manifestAutoRetirable(manifest *transactionManifest) bool {
	if manifest.Status != txActive && manifest.Status != txComplete {
		return false
	}
	return !manifestHasUncertainChange(manifest)
}

// reconcileRecoveryState resolves only transitions that can be proven from
// durable evidence. Ambiguous active/prepared, partial, corrupt, and orphaned
// data is retained and reported as blocking.
func (w *Workspace) reconcileRecoveryState() (*recoveryStateFile, error) {
	state, err := w.loadOrMigrateRecoveryState()
	if err != nil {
		return nil, err
	}

	// Garbage is already durably retired, so physical deletion is always safe.
	if err := w.cleanupGarbage(state); err != nil {
		return state, err
	}

	if state.Pending != "" {
		manifest, loadErr := w.loadManifest(state.Pending)
		if loadErr == nil {
			switch {
			case manifest.Status == txActive && manifestAutoRetirable(manifest):
				next := cloneRecoveryState(state)
				retired := next.Pending
				next.Pending = ""
				appendGarbage(next, retired)
				if err := w.commitRecoveryState(next); err != nil {
					return state, fmt.Errorf("retire inactive pending transaction: %w", err)
				}
				state = next
				if err := w.cleanupGarbage(state); err != nil {
					return state, err
				}
			case manifestHasCommitted(manifest):
				if manifest.Status != txComplete {
					break
				}
				if manifest.PreviousUndoID != state.Current {
					break
				}
				next := cloneRecoveryState(state)
				old := next.Current
				next.Current, next.Pending = next.Pending, ""
				appendGarbage(next, old)
				if err := w.commitRecoveryState(next); err != nil {
					return state, fmt.Errorf("promote completed pending transaction: %w", err)
				}
				state = next
				if err := w.cleanupGarbage(state); err != nil {
					return state, err
				}
			case manifest.Status == txComplete && !manifestHasUncertainChange(manifest):
				if manifest.PreviousUndoID != state.Current {
					break
				}
				next := cloneRecoveryState(state)
				retired := next.Pending
				next.Pending = ""
				appendGarbage(next, retired)
				if err := w.commitRecoveryState(next); err != nil {
					return state, fmt.Errorf("retire completed no-op transaction: %w", err)
				}
				state = next
				if err := w.cleanupGarbage(state); err != nil {
					return state, err
				}
			}
		}
	}

	ids, err := w.manifestIDs()
	if err != nil {
		return state, err
	}
	known := make(map[string]bool, len(state.Garbage)+2)
	known[state.Current], known[state.Pending] = true, true
	for _, id := range state.Garbage {
		known[id] = true
	}
	for _, id := range ids {
		if known[id] {
			continue
		}
		manifest, loadErr := w.loadManifest(id)
		if loadErr != nil || !manifestAutoRetirable(manifest) {
			continue
		}
		next := cloneRecoveryState(state)
		appendGarbage(next, id)
		if err := w.commitRecoveryState(next); err != nil {
			return state, fmt.Errorf("retire unused transaction %s: %w", id, err)
		}
		state = next
		if err := w.cleanupGarbage(state); err != nil {
			return state, err
		}
	}
	return state, nil
}

func (w *Workspace) cleanupGarbage(state *recoveryStateFile) error {
	for len(state.Garbage) > 0 {
		id := state.Garbage[0]
		if err := w.removeTransaction(id); err != nil {
			return fmt.Errorf("delete retired recovery transaction %s: %w", id, err)
		}
		next := cloneRecoveryState(state)
		next.Garbage = slices.Delete(next.Garbage, 0, 1)
		if err := w.commitRecoveryState(next); err != nil {
			return fmt.Errorf("record retired recovery transaction %s deletion: %w", id, err)
		}
		*state = *next
	}
	return nil
}

func recoveryTargetPaths(manifest *transactionManifest) []string {
	seen := make(map[string]bool)
	paths := make([]string, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		if seen[file.RelativePath] {
			continue
		}
		seen[file.RelativePath] = true
		paths = append(paths, file.RelativePath)
	}
	sort.Slice(paths, func(i, j int) bool { return pathLess(paths[i], paths[j]) })
	return paths
}

func manifestCanAffectTargets(manifest *transactionManifest) bool {
	for _, file := range manifest.Files {
		if file.BackupPath != "" && (file.State == statePrepared || file.State == stateCommitted || file.State == stateRestored || file.State == stateConflict || file.State == stateFailed) {
			return true
		}
	}
	return false
}

func ensureRecoveryScope(manifest *transactionManifest, allowedPaths []string) error {
	if allowedPaths == nil {
		return nil
	}
	allowed := make(map[string]bool, len(allowedPaths))
	for _, name := range allowedPaths {
		rel, err := safeRelative(filepath.ToSlash(name))
		if err != nil {
			return fmt.Errorf("%w: invalid allowed path: %v", ErrRecoveryScope, err)
		}
		allowed[rel] = true
	}
	for _, rel := range recoveryTargetPaths(manifest) {
		if !allowed[rel] {
			return fmt.Errorf("%w: transaction also contains %s", ErrRecoveryScope, rel)
		}
	}
	return nil
}

func manifestRecovery(id string, role RecoveryRole, manifest *transactionManifest, loadErr error) Recovery {
	if loadErr != nil {
		return Recovery{ID: id, Role: RecoveryRoleCorrupt, Corrupt: true, Err: loadErr, BlocksMutation: true}
	}
	item := Recovery{
		ID: id, Role: role, CreatedAt: manifest.CreatedAt, Status: manifest.Status,
		Files: len(manifest.Files), FilesList: recoveryTargetPaths(manifest),
	}
	for _, file := range manifest.Files {
		switch file.State {
		case stateCommitted, statePrepared, stateRestored, stateConflict:
			item.Changed++
		}
	}
	switch role {
	case RecoveryRolePending, RecoveryRoleOrphan, RecoveryRoleGarbage, RecoveryRoleCorrupt:
		item.BlocksMutation = true
	case RecoveryRoleUndo:
		item.BlocksMutation = manifest.Status != txComplete || !manifestCanAffectTargets(manifest)
	}
	return item
}

func (w *Workspace) recoveryStateView(state *recoveryStateFile) (RecoveryState, error) {
	view := RecoveryState{Generation: state.Generation}
	ids, err := w.manifestIDs()
	if err != nil {
		return view, err
	}
	seen := make(map[string]bool, len(ids)+len(state.Garbage)+2)
	for _, id := range ids {
		seen[id] = true
	}
	for _, id := range append(append([]string{}, state.Current, state.Pending), state.Garbage...) {
		if id != "" && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	sort.Strings(ids)
	garbage := make(map[string]bool, len(state.Garbage))
	for _, id := range state.Garbage {
		garbage[id] = true
	}
	for _, id := range ids {
		role := RecoveryRoleOrphan
		switch {
		case id == state.Current:
			role = RecoveryRoleUndo
		case id == state.Pending:
			role = RecoveryRolePending
		case garbage[id]:
			role = RecoveryRoleGarbage
		}
		manifest, loadErr := w.loadManifest(id)
		item := manifestRecovery(id, role, manifest, loadErr)
		view.Items = append(view.Items, item)
		view.Blocked = view.Blocked || item.BlocksMutation
		if role == RecoveryRoleUndo && loadErr == nil && manifestCanAffectTargets(manifest) {
			view.Undo = &UndoInfo{
				ID: id, CreatedAt: manifest.CreatedAt, Files: slices.Clone(item.FilesList),
				Partial: manifest.Status != txComplete, BlocksRemove: manifest.Status != txComplete,
			}
		}
	}
	sort.SliceStable(view.Items, func(i, j int) bool {
		if view.Items[i].CreatedAt.Equal(view.Items[j].CreatedAt) {
			return view.Items[i].ID > view.Items[j].ID
		}
		return view.Items[i].CreatedAt.After(view.Items[j].CreatedAt)
	})
	return view, nil
}

// State reconciles provably completed pointer transitions and returns the
// persistent undo point plus all retained recovery material.
func (w *Workspace) State(ctx context.Context) (RecoveryState, error) {
	if err := w.beginOperation(ctx, true); err != nil {
		return RecoveryState{}, err
	}
	defer w.endOperation()
	state, err := w.reconcileRecoveryState()
	if err != nil {
		return RecoveryState{}, err
	}
	return w.recoveryStateView(state)
}

// UndoInfo returns the current persistent undo point, or nil when absent.
func (w *Workspace) UndoInfo(ctx context.Context) (*UndoInfo, error) {
	if err := w.beginOperation(ctx, true); err != nil {
		return nil, err
	}
	defer w.endOperation()
	internal, err := w.reconcileRecoveryState()
	if err != nil {
		return nil, err
	}
	view, err := w.recoveryStateView(internal)
	if err != nil {
		return nil, err
	}
	if view.Undo != nil {
		return view.Undo, nil
	}
	if internal.Current == "" {
		return nil, nil
	}
	for _, item := range view.Items {
		if internal.Current == item.ID {
			return nil, fmt.Errorf("%w: current undo %s is not safely restorable: %v", ErrRecoveryState, item.ID, item.Err)
		}
	}
	return nil, fmt.Errorf("%w: current undo %s has no transaction manifest", ErrRecoveryState, internal.Current)
}

// stateUndoSummary annotates a summary from authoritative durable state.
func stateUndoSummary(summary *MutationSummary, state *recoveryStateFile) {
	summary.UndoID = state.Current
	summary.UndoAvailable = state.Current != ""
	summary.BlockingRecoveryID = state.Pending
	summary.RecoveryRetained = summary.RecoveryRetained || state.Pending != "" || len(state.Garbage) != 0
}
