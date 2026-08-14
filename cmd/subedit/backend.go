package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"subedit/internal/subtitle"
	"subedit/internal/tui"
	"subedit/internal/workspace"
)

// appBackend adapts the synchronous, safety-focused workspace API to the
// event streams consumed by Bubble Tea. The latest discovery snapshot is kept
// immutable under a mutex; filesystem conflict protection is still enforced
// independently by workspace hashes immediately before every replacement.
type appBackend struct {
	workspace *workspace.Workspace
	ctx       context.Context
	cancel    context.CancelFunc

	mu         sync.RWMutex
	discovery  workspace.Discovery
	haveScan   bool
	searches   map[uint64]searchSnapshot
	editor     *editorSnapshot
	editorPath string
	editorBusy string
	// editorOpenGeneration makes the latest OpenEditor call authoritative.
	// Request cancellation alone cannot prevent an older goroutine that already
	// passed its first context check from publishing after a newer editor opens.
	editorOpenGeneration uint64
	wg                   sync.WaitGroup
}

var _ tui.EditorBackend = (*appBackend)(nil)

// searchSnapshot is the exact backend result revision the UI reviewed. It is
// kept independently from Bubble Tea's view structs so a mutation request can
// be proven to contain only current matches for the same literal query.
type searchSnapshot struct {
	query           string
	normalizedQuery string
	workspace       uint64
	discovery       workspace.Discovery
	matches         map[string]map[subtitle.CueID]struct{}
}

// editorSnapshot is the sole document version authorized for immersive
// editing. Its opaque token is consumed when a mutation or scoped undo is
// accepted, preventing replay. The complete cue set, rather than the outer
// search matches, authorizes local editor deletions.
type editorSnapshot struct {
	token             string
	path              string
	workspaceRevision uint64
	file              workspace.File
	allowed           map[subtitle.CueID]struct{}
}

func newEditorToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate editor snapshot token: %w", err)
	}
	return "editor-" + hex.EncodeToString(value[:]), nil
}

func newAppBackend(ws *workspace.Workspace) *appBackend {
	ctx, cancel := context.WithCancel(context.Background())
	return &appBackend{workspace: ws, ctx: ctx, cancel: cancel, searches: make(map[uint64]searchSnapshot)}
}

func (b *appBackend) Close() {
	b.cancel()
	b.wg.Wait()
}

func (b *appBackend) operationContext(request context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(request)
	stop := context.AfterFunc(b.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// send delivers one stream event without letting a stopped TUI strand a
// producer in Close. A request cancellation also stops delivery promptly.
func send[T any](ctx context.Context, events chan<- T, event T) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// sendFinal publishes the durable outcome even when a user-request context was
// cancelled to stop between files. Backend shutdown still wins, preventing a
// producer from stranding Close after the UI has gone away.
func sendFinal[T any](shutdown context.Context, events chan<- T, event T) bool {
	return send(shutdown, events, event)
}

func (b *appBackend) Discover(request context.Context) <-chan tui.DiscoveryEvent {
	// A full rescan invalidates every exact editor version immediately. This is
	// deliberately done before its goroutine starts so a replay cannot race the
	// request and mutate through a snapshot the caller has chosen to replace.
	b.mu.Lock()
	b.editor = nil
	b.editorPath = ""
	b.editorBusy = ""
	b.invalidateOuterLocked()
	b.mu.Unlock()
	events := make(chan tui.DiscoveryEvent, 1)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(events)
		ctx, cancel := b.operationContext(request)
		defer cancel()

		discovery, err := b.workspace.DiscoverWithProgress(ctx, func(progress workspace.DiscoveryProgress) {
			send(ctx, events, tui.DiscoveryEvent{
				Completed:   progress.Completed,
				Total:       progress.Total,
				CurrentPath: progress.CurrentPath,
			})
		})
		if err != nil {
			sendFinal(b.ctx, events, tui.DiscoveryEvent{Err: err, Done: true})
			return
		}
		b.mu.Lock()
		b.discovery = discovery
		b.haveScan = true
		b.searches = make(map[uint64]searchSnapshot)
		b.mu.Unlock()

		view := discoveryView(discovery)
		sendFinal(b.ctx, events, tui.DiscoveryEvent{
			Completed: len(view.Files),
			Total:     len(view.Files),
			Discovery: &view,
			Done:      true,
		})
	}()
	return events
}

func (b *appBackend) Search(requestContext context.Context, request tui.SearchRequest) <-chan tui.SearchEvent {
	events := make(chan tui.SearchEvent, 1)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(events)
		ctx, cancel := b.operationContext(requestContext)
		defer cancel()
		if err := ctx.Err(); err != nil {
			sendFinal(b.ctx, events, tui.SearchEvent{Err: err, Done: true})
			return
		}

		b.mu.RLock()
		discovery, ok := b.discovery, b.haveScan
		b.mu.RUnlock()
		if !ok {
			sendFinal(b.ctx, events, tui.SearchEvent{Err: errors.New("workspace has not finished discovery"), Done: true})
			return
		}
		result := discovery.Search(request.Query)
		view := searchView(discovery, result, request)
		b.mu.Lock()
		// Only publish a search snapshot if discovery is still the one searched.
		if b.haveScan && b.discovery.Revision == discovery.Revision {
			b.searches[request.Revision] = makeSearchSnapshot(discovery, result)
		}
		b.mu.Unlock()
		sendFinal(b.ctx, events, tui.SearchEvent{Result: view, Done: true})
	}()
	return events
}

func (b *appBackend) OpenEditor(requestContext context.Context, request tui.EditorOpenRequest) <-chan tui.EditorOpenEvent {
	events := make(chan tui.EditorOpenEvent, 1)
	generation, claimErr := b.claimEditorOpen(requestContext)
	if claimErr != nil {
		events <- tui.EditorOpenEvent{Revision: request.Revision, Err: claimErr, Done: true}
		close(events)
		return events
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(events)
		ctx, cancel := b.operationContext(requestContext)
		defer cancel()
		if err := ctx.Err(); err != nil {
			sendFinal(b.ctx, events, tui.EditorOpenEvent{Revision: request.Revision, Err: err, Done: true})
			return
		}

		document, err := b.openEditorDocument(ctx, request, generation)
		sendFinal(b.ctx, events, tui.EditorOpenEvent{
			Revision: request.Revision,
			Document: document,
			Err:      err,
			Done:     true,
		})
	}()
	return events
}

func (b *appBackend) SearchEditor(requestContext context.Context, request tui.EditorSearchRequest) <-chan tui.EditorSearchEvent {
	events := make(chan tui.EditorSearchEvent, 1)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(events)
		ctx, cancel := b.operationContext(requestContext)
		defer cancel()

		result := tui.EditorSearchEvent{
			Revision: request.Revision, SnapshotID: request.SnapshotID,
			Query: request.Query, Done: true,
		}
		if err := ctx.Err(); err != nil {
			result.Err = err
			sendFinal(b.ctx, events, result)
			return
		}
		snapshot, err := b.currentEditorSnapshot(request.SnapshotID)
		if err != nil {
			result.Err = err
			sendFinal(b.ctx, events, result)
			return
		}
		cues := snapshot.file.Document.Cues
		if subtitle.NormalizeQuery(request.Query) != "" {
			cues = snapshot.file.Document.Search(request.Query)
		}
		result.CueIDs = cueIDs(cues)
		if err := ctx.Err(); err != nil {
			result.Err = err
			result.CueIDs = nil
		} else if !b.editorTokenCurrent(request.SnapshotID) {
			result.Err = errors.New("editor snapshot is stale")
			result.CueIDs = nil
		}
		sendFinal(b.ctx, events, result)
	}()
	return events
}

func (b *appBackend) RefreshEditor(requestContext context.Context, request tui.EditorRefreshRequest) <-chan tui.EditorOpenEvent {
	events := make(chan tui.EditorOpenEvent, 1)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(events)
		ctx, cancel := b.operationContext(requestContext)
		defer cancel()

		operation, err := b.beginEditorRefresh(request.Path)
		if err != nil {
			sendFinal(b.ctx, events, tui.EditorOpenEvent{Revision: request.Revision, Err: err, Done: true})
			return
		}
		discovery, discoverErr := b.workspace.DiscoverFile(ctx, request.Path, nil)
		var document *tui.EditorDocument
		if discoverErr == nil {
			document, discoverErr = b.publishEditorRefresh(operation, request.Path, discovery)
		}
		if discoverErr != nil {
			b.finishEditorOperation(operation)
		}
		sendFinal(b.ctx, events, tui.EditorOpenEvent{
			Revision: request.Revision,
			Document: document,
			Err:      discoverErr,
			Done:     true,
		})
	}()
	return events
}

func (b *appBackend) UndoEditor(requestContext context.Context, request tui.EditorUndoRequest) <-chan tui.MutationEvent {
	events := make(chan tui.MutationEvent, 1)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(events)
		ctx, cancel := b.operationContext(requestContext)
		defer cancel()

		operation, err := b.beginEditorUndo(request.Path, request.UndoID)
		if err != nil {
			sendFinal(b.ctx, events, tui.MutationEvent{Err: err, Done: true})
			return
		}
		summary, err := b.workspace.UndoScoped(ctx, request.UndoID, []string{request.Path}, func(progress workspace.Progress) {
			send(ctx, events, tui.MutationEvent{Progress: mutationProgress(progress)})
		})
		b.finishEditorOperation(operation)
		view := mutationSummary("undo", summary)
		sendFinal(b.ctx, events, tui.MutationEvent{Summary: &view, Err: err, Done: true})
	}()
	return events
}

func (b *appBackend) Mutate(requestContext context.Context, request tui.MutationRequest) <-chan tui.MutationEvent {
	events := make(chan tui.MutationEvent, 1)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(events)
		ctx, cancel := b.operationContext(requestContext)
		defer cancel()

		plan, err := b.mutationPlan(request)
		if err != nil {
			sendFinal(b.ctx, events, tui.MutationEvent{Err: err, Done: true})
			return
		}
		summary, err := b.workspace.Apply(ctx, plan, func(progress workspace.Progress) {
			send(ctx, events, tui.MutationEvent{Progress: mutationProgress(progress)})
		})
		if request.Source == tui.MutationSourceEditor {
			b.finishEditorOperation(request.SnapshotID)
		}
		view := mutationSummary("delete", summary)
		sendFinal(b.ctx, events, tui.MutationEvent{Summary: &view, Err: err, Done: true})
	}()
	return events
}

func (b *appBackend) Undo(requestContext context.Context) <-chan tui.MutationEvent {
	events := make(chan tui.MutationEvent, 1)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(events)
		ctx, cancel := b.operationContext(requestContext)
		defer cancel()
		summary, err := b.workspace.Undo(ctx, func(progress workspace.Progress) {
			send(ctx, events, tui.MutationEvent{Progress: mutationProgress(progress)})
		})
		view := mutationSummary("undo", summary)
		sendFinal(b.ctx, events, tui.MutationEvent{Summary: &view, Err: err, Done: true})
	}()
	return events
}

func (b *appBackend) ListRecoveries(ctx context.Context) ([]tui.RecoveryItem, error) {
	recoveries, err := b.workspace.Recoveries(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]tui.RecoveryItem, 0, len(recoveries))
	for _, recovery := range recoveries {
		// The durable undo point, including its retained partial state, is
		// initialized separately through tui.Options. Startup recovery lists
		// contain crash remnants only so the same transaction is never gated
		// twice.
		if recovery.Role == workspace.RecoveryRoleUndo {
			continue
		}
		summary := fmt.Sprintf("%d files; %d may have changed", recovery.Files, recovery.Changed)
		if recovery.Corrupt {
			summary = "Recovery metadata is damaged; restore is unavailable, but explicit discard is permitted."
			if recovery.Err != nil {
				summary += " " + recovery.Err.Error()
			}
		}
		items = append(items, tui.RecoveryItem{
			ID: recovery.ID, CreatedAt: recovery.CreatedAt, Files: recovery.Files, Summary: summary,
		})
	}
	return items, nil
}

func (b *appBackend) ResolveRecovery(requestContext context.Context, request tui.RecoveryRequest) <-chan tui.RecoveryEvent {
	events := make(chan tui.RecoveryEvent, 1)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer close(events)
		ctx, cancel := b.operationContext(requestContext)
		defer cancel()
		switch request.Action {
		case tui.RecoveryRestore:
			summary, err := b.workspace.RestoreRecovery(ctx, request.ID, func(progress workspace.Progress) {
				send(ctx, events, tui.RecoveryEvent{Progress: &tui.RecoveryProgress{
					Completed:   progress.Completed,
					Total:       progress.Total,
					CurrentPath: progress.Result.RelativePath,
				}})
			})
			view := tui.RecoverySummary{
				Succeeded: summary.Restored,
				Skipped:   summary.Skipped + summary.Conflicted,
				Failed:    summary.Failed,
				Retained: summary.BlockingRecoveryID == request.ID || summary.Cancelled ||
					summary.Conflicted > 0 || summary.Failed > 0,
			}
			undo, stateErr := b.recoveryUndoSnapshot()
			view.Undo = undo
			if stateErr != nil {
				// Without an authoritative post-operation state the UI must keep
				// the recovery gate rather than infer that it was resolved.
				view.Retained = true
			}
			sendFinal(b.ctx, events, tui.RecoveryEvent{Summary: &view, Err: errors.Join(err, stateErr), Done: true})
		case tui.RecoveryDiscard:
			err := b.workspace.DiscardRecovery(ctx, request.ID)
			undo, stateErr := b.recoveryUndoSnapshot()
			view := tui.RecoverySummary{Undo: undo, Retained: err != nil || stateErr != nil}
			sendFinal(b.ctx, events, tui.RecoveryEvent{Summary: &view, Err: errors.Join(err, stateErr), Done: true})
		default:
			sendFinal(b.ctx, events, tui.RecoveryEvent{Err: fmt.Errorf("unknown recovery action %q", request.Action), Done: true})
		}
	}()
	return events
}

// recoveryUndoSnapshot reads durable state after a recovery operation has
// released the workspace operation lock. It deliberately uses the backend
// lifetime context: a user cancellation stops additional file restores, but
// the UI still needs the authoritative partial-undo state produced so far.
func (b *appBackend) recoveryUndoSnapshot() (*tui.UndoSnapshot, error) {
	state, err := b.workspace.State(b.ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect durable undo after recovery: %w", err)
	}
	snapshot := &tui.UndoSnapshot{}
	if state.Undo != nil {
		snapshot.Available = true
		if state.Undo.BlocksRemove {
			snapshot.RetainedUndoID = state.Undo.ID
		}
	}
	return snapshot, nil
}

func (b *appBackend) mutationPlan(request tui.MutationRequest) (workspace.MutationPlan, error) {
	switch request.Source {
	case tui.MutationSourceSearch:
		return b.searchMutationPlan(request)
	case tui.MutationSourceEditor:
		return b.editorMutationPlan(request)
	default:
		return workspace.MutationPlan{}, fmt.Errorf("unknown mutation source %q", request.Source)
	}
}

func (b *appBackend) searchMutationPlan(request tui.MutationRequest) (workspace.MutationPlan, error) {
	b.mu.RLock()
	discovery, ok := b.discovery, b.haveScan
	snapshot, searched := b.searches[request.Revision]
	b.mu.RUnlock()
	if !ok {
		return workspace.MutationPlan{}, errors.New("workspace has not finished discovery")
	}
	if !searched || snapshot.workspace != discovery.Revision ||
		snapshot.query != request.Query || snapshot.normalizedQuery == "" {
		return workspace.MutationPlan{}, errors.New("mutation is not based on the current reviewed search result")
	}
	files := make(map[string]workspace.File, len(snapshot.discovery.Files))
	for _, file := range snapshot.discovery.Files {
		files[file.RelativePath] = file
	}
	plan := workspace.MutationPlan{
		SearchRevision: discovery.Revision,
		Scope:          workspace.DeleteScope(request.Scope),
		Files:          make([]workspace.FileMutation, 0, len(request.Targets)),
	}
	seen := make(map[string]struct{}, len(request.Targets))
	for _, target := range request.Targets {
		if target.FileID != target.Path {
			return workspace.MutationPlan{}, fmt.Errorf("mutation target identity mismatch for %q", target.Path)
		}
		if _, duplicate := seen[target.Path]; duplicate {
			return workspace.MutationPlan{}, fmt.Errorf("duplicate mutation target %q", target.Path)
		}
		seen[target.Path] = struct{}{}
		file, exists := files[target.Path]
		if !exists {
			return workspace.MutationPlan{}, fmt.Errorf("mutation target %q is not in the indexed workspace", target.Path)
		}
		cueIDs := make([]subtitle.CueID, len(target.CueIDs))
		allowed := snapshot.matches[target.Path]
		if len(allowed) == 0 {
			return workspace.MutationPlan{}, fmt.Errorf("mutation target %q did not match the reviewed search", target.Path)
		}
		seenCues := make(map[subtitle.CueID]struct{}, len(target.CueIDs))
		for index, id := range target.CueIDs {
			cueID := subtitle.CueID(id)
			if _, exists := allowed[cueID]; !exists {
				return workspace.MutationPlan{}, fmt.Errorf("cue %q in %q did not match the reviewed search", id, target.Path)
			}
			if _, duplicate := seenCues[cueID]; duplicate {
				return workspace.MutationPlan{}, fmt.Errorf("duplicate cue %q in %q", id, target.Path)
			}
			seenCues[cueID] = struct{}{}
			cueIDs[index] = cueID
		}
		if len(cueIDs) == 0 {
			return workspace.MutationPlan{}, fmt.Errorf("mutation target %q contains no cues", target.Path)
		}
		plan.Files = append(plan.Files, workspace.FileMutation{
			RelativePath:     file.RelativePath,
			ExpectedSHA256:   file.SHA256,
			ExpectedIdentity: file.Identity,
			CueIDs:           cueIDs,
		})
	}
	if len(plan.Files) == 0 {
		return workspace.MutationPlan{}, errors.New("mutation contains no files")
	}
	return plan, nil
}

func (b *appBackend) editorMutationPlan(request tui.MutationRequest) (workspace.MutationPlan, error) {
	if request.Scope != tui.DeleteEditor {
		return workspace.MutationPlan{}, fmt.Errorf("editor mutation has scope %q, want %q", request.Scope, tui.DeleteEditor)
	}
	if request.Query != "" {
		return workspace.MutationPlan{}, errors.New("editor mutation must not carry a workspace search query")
	}
	if request.SnapshotID == "" {
		return workspace.MutationPlan{}, errors.New("editor mutation has no snapshot token")
	}
	if len(request.Targets) != 1 {
		return workspace.MutationPlan{}, fmt.Errorf("editor mutation has %d targets, want exactly one", len(request.Targets))
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.editorBusy != "" {
		return workspace.MutationPlan{}, errors.New("editor operation is already in progress")
	}
	snapshot := b.editor
	if snapshot == nil || snapshot.token != request.SnapshotID {
		return workspace.MutationPlan{}, errors.New("editor snapshot is stale")
	}
	target := request.Targets[0]
	if target.FileID != snapshot.path || target.Path != snapshot.path || b.editorPath != snapshot.path {
		return workspace.MutationPlan{}, fmt.Errorf("editor mutation target does not match snapshot path %q", snapshot.path)
	}
	if len(target.CueIDs) == 0 {
		return workspace.MutationPlan{}, errors.New("editor mutation contains no cues")
	}
	cues := make([]subtitle.CueID, len(target.CueIDs))
	seen := make(map[subtitle.CueID]struct{}, len(target.CueIDs))
	for index, value := range target.CueIDs {
		id := subtitle.CueID(value)
		if _, duplicate := seen[id]; duplicate {
			return workspace.MutationPlan{}, fmt.Errorf("duplicate editor cue %q", value)
		}
		if _, authorized := snapshot.allowed[id]; !authorized {
			return workspace.MutationPlan{}, fmt.Errorf("cue %q is outside editor snapshot %q", value, snapshot.path)
		}
		seen[id] = struct{}{}
		cues[index] = id
	}

	plan := workspace.MutationPlan{
		SearchRevision: snapshot.workspaceRevision,
		Scope:          workspace.DeleteEditor,
		Files: []workspace.FileMutation{{
			RelativePath:     snapshot.file.RelativePath,
			ExpectedSHA256:   snapshot.file.SHA256,
			ExpectedIdentity: snapshot.file.Identity,
			CueIDs:           cues,
		}},
	}
	// Acceptance consumes the token before any asynchronous filesystem work.
	// Replays and concurrent editor requests therefore fail closed even if the
	// transaction later conflicts or is cancelled.
	b.editor = nil
	b.editorBusy = request.SnapshotID
	b.invalidateOuterLocked()
	return plan, nil
}

func (b *appBackend) claimEditorOpen(ctx context.Context) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A cancelled stale Bubble Tea command can be invoked after a newer live
	// command has already claimed publication. It must not advance the
	// generation and thereby suppress that newer editor.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	b.editorOpenGeneration++
	return b.editorOpenGeneration, nil
}

func (b *appBackend) openEditorDocument(ctx context.Context, request tui.EditorOpenRequest, generation uint64) (*tui.EditorDocument, error) {
	if request.FileID == "" || request.Path == "" || request.FileID != request.Path {
		return nil, errors.New("editor file identity and path must be equal and non-empty")
	}
	token, err := newEditorToken()
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if generation != b.editorOpenGeneration {
		return nil, errors.New("editor open was superseded")
	}
	if !b.haveScan {
		return nil, errors.New("workspace has not finished discovery")
	}
	if b.editorBusy != "" {
		return nil, errors.New("editor operation is already in progress")
	}
	var file *workspace.File
	for index := range b.discovery.Files {
		if b.discovery.Files[index].RelativePath == request.Path {
			copy := b.discovery.Files[index]
			file = &copy
			break
		}
	}
	if file == nil {
		return nil, fmt.Errorf("editor path %q is not in the current discovery", request.Path)
	}
	snapshot, document, err := makeEditorSnapshot(*file, b.discovery.Revision, token)
	if err != nil {
		return nil, err
	}
	b.editor = snapshot
	b.editorPath = snapshot.path
	return document, nil
}

func makeEditorSnapshot(file workspace.File, revision uint64, token string) (*editorSnapshot, *tui.EditorDocument, error) {
	if token == "" {
		return nil, nil, errors.New("editor snapshot token is empty")
	}
	if file.RelativePath == "" || file.Document == nil {
		return nil, nil, errors.New("editor file has no parsed document")
	}
	allowed := make(map[subtitle.CueID]struct{}, len(file.Document.Cues))
	for _, cue := range file.Document.Cues {
		if cue.ID == "" {
			return nil, nil, errors.New("editor document contains an empty cue ID")
		}
		if _, duplicate := allowed[cue.ID]; duplicate {
			return nil, nil, fmt.Errorf("editor document contains duplicate cue ID %q", cue.ID)
		}
		allowed[cue.ID] = struct{}{}
	}
	snapshot := &editorSnapshot{
		token: token, path: file.RelativePath, workspaceRevision: revision,
		file: file, allowed: allowed,
	}
	document := &tui.EditorDocument{
		FileID: file.RelativePath, Path: file.RelativePath, SnapshotID: token,
		Cues: cueViews(file.Document.Cues),
	}
	return snapshot, document, nil
}

func (b *appBackend) currentEditorSnapshot(token string) (editorSnapshot, error) {
	if token == "" {
		return editorSnapshot{}, errors.New("editor search has no snapshot token")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.editorBusy != "" || b.editor == nil || b.editor.token != token {
		return editorSnapshot{}, errors.New("editor snapshot is stale")
	}
	return *b.editor, nil
}

func (b *appBackend) editorTokenCurrent(token string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.editorBusy == "" && b.editor != nil && b.editor.token == token
}

func (b *appBackend) beginEditorRefresh(relativePath string) (string, error) {
	operation, err := newEditorToken()
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if relativePath == "" || b.editorPath == "" || relativePath != b.editorPath {
		return "", fmt.Errorf("editor refresh path %q does not match active path", relativePath)
	}
	if b.editorBusy != "" {
		return "", errors.New("editor operation is already in progress")
	}
	b.editor = nil
	b.editorBusy = operation
	b.invalidateOuterLocked()
	return operation, nil
}

func (b *appBackend) publishEditorRefresh(operation, relativePath string, discovery workspace.Discovery) (*tui.EditorDocument, error) {
	if len(discovery.Files) != 1 || discovery.Files[0].RelativePath != relativePath {
		if len(discovery.Issues) == 1 {
			return nil, fmt.Errorf("refresh editor file %q: %w", relativePath, discovery.Issues[0])
		}
		return nil, fmt.Errorf("refresh editor file %q returned no indexed file", relativePath)
	}
	token, err := newEditorToken()
	if err != nil {
		return nil, err
	}
	snapshot, document, err := makeEditorSnapshot(discovery.Files[0], discovery.Revision, token)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.editorBusy != operation || b.editorPath != relativePath {
		return nil, errors.New("editor refresh was superseded")
	}
	b.editor = snapshot
	b.editorBusy = ""
	return document, nil
}

func (b *appBackend) beginEditorUndo(relativePath, undoID string) (string, error) {
	if undoID == "" {
		return "", errors.New("editor undo has no expected transaction ID")
	}
	operation, err := newEditorToken()
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if relativePath == "" || b.editorPath == "" || relativePath != b.editorPath {
		return "", fmt.Errorf("editor undo path %q does not match active path", relativePath)
	}
	if b.editorBusy != "" {
		return "", errors.New("editor operation is already in progress")
	}
	b.editor = nil
	b.editorBusy = operation
	b.invalidateOuterLocked()
	return operation, nil
}

func (b *appBackend) finishEditorOperation(operation string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.editorBusy == operation {
		b.editorBusy = ""
	}
}

func (b *appBackend) invalidateOuterLocked() {
	// Invalidate editor opens that were claimed before the operation which made
	// the workspace snapshot stale. A late goroutine must not republish one from
	// either the old discovery or an earlier editor selection.
	b.editorOpenGeneration++
	b.discovery = workspace.Discovery{}
	b.haveScan = false
	b.searches = make(map[uint64]searchSnapshot)
}

func cueIDs(cues []subtitle.Cue) []string {
	ids := make([]string, len(cues))
	for index := range cues {
		ids[index] = string(cues[index].ID)
	}
	return ids
}

func makeSearchSnapshot(discovery workspace.Discovery, result workspace.SearchResult) searchSnapshot {
	snapshot := searchSnapshot{
		query: result.Query, normalizedQuery: result.NormalizedQuery, workspace: result.Revision,
		discovery: cloneDiscoverySnapshot(discovery),
		matches:   make(map[string]map[subtitle.CueID]struct{}, len(result.Matches)),
	}
	for _, match := range result.Matches {
		ids := make(map[subtitle.CueID]struct{}, len(match.Cues))
		for _, cue := range match.Cues {
			ids[cue.ID] = struct{}{}
		}
		snapshot.matches[match.File.RelativePath] = ids
	}
	return snapshot
}

func cloneDiscoverySnapshot(discovery workspace.Discovery) workspace.Discovery {
	discovery.Files = append([]workspace.File(nil), discovery.Files...)
	discovery.Issues = append([]workspace.Issue(nil), discovery.Issues...)
	return discovery
}

func discoveryView(discovery workspace.Discovery) tui.Discovery {
	files := make([]tui.File, 0, len(discovery.Files)+len(discovery.Issues))
	for _, file := range discovery.Files {
		files = append(files, tui.File{
			ID:      file.RelativePath,
			Path:    file.RelativePath,
			Valid:   true,
			Preview: cueViews(firstCues(file.Document.Cues, 5)),
		})
	}
	for _, issue := range discovery.Issues {
		files = append(files, tui.File{
			ID:    issue.RelativePath,
			Path:  issue.RelativePath,
			Valid: false,
			Error: issueMessage(issue),
		})
	}
	// Discovery has already sorted each group; a combined deterministic sort is
	// needed so skipped cards appear at their natural alphabetical position.
	sortTUIFiles(files)
	return tui.Discovery{Files: files, Skipped: len(discovery.Issues)}
}

func searchView(discovery workspace.Discovery, result workspace.SearchResult, request tui.SearchRequest) tui.SearchResult {
	files := make([]tui.File, 0, len(result.Matches))
	for _, match := range result.Matches {
		ids := make([]string, len(match.Cues))
		for index, cue := range match.Cues {
			ids[index] = string(cue.ID)
		}
		files = append(files, tui.File{
			ID:         match.File.RelativePath,
			Path:       match.File.RelativePath,
			Valid:      true,
			Preview:    cueViews(firstCues(match.Cues, 5)),
			MatchIDs:   ids,
			MatchCount: len(match.Cues),
		})
	}
	return tui.SearchResult{
		Query:         request.Query,
		Revision:      request.Revision,
		Files:         files,
		MatchingCues:  result.MatchingCues,
		MatchingFiles: len(result.Matches),
		TotalFiles:    len(discovery.Files) + len(discovery.Issues),
		Skipped:       len(discovery.Issues),
	}
}

func firstCues(cues []subtitle.Cue, count int) []subtitle.Cue {
	if len(cues) <= count {
		return cues
	}
	return cues[:count]
}

func cueViews(cues []subtitle.Cue) []tui.Cue {
	views := make([]tui.Cue, len(cues))
	for index, cue := range cues {
		views[index] = tui.Cue{
			ID:        string(cue.ID),
			Timestamp: formatTimestamp(cue.Start) + " → " + formatTimestamp(cue.End),
			Text:      cue.DisplayText,
		}
	}
	return views
}

func formatTimestamp(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	hours := int64(value / time.Hour)
	value -= time.Duration(hours) * time.Hour
	minutes := int64(value / time.Minute)
	value -= time.Duration(minutes) * time.Minute
	seconds := int64(value / time.Second)
	millis := int64((value - time.Duration(seconds)*time.Second) / time.Millisecond)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, millis)
}

func mutationProgress(progress workspace.Progress) *tui.MutationProgress {
	view := &tui.MutationProgress{
		Completed:   progress.Completed,
		Total:       progress.Total,
		CurrentPath: progress.Result.RelativePath,
	}
	switch progress.Result.Status {
	case workspace.FileSucceeded, workspace.FileRestored:
		view.Succeeded = 1
	case workspace.FileSkipped, workspace.FileConflicted:
		view.Skipped = 1
	case workspace.FileFailed:
		view.Failed = 1
	}
	return view
}

func mutationSummary(operation string, summary workspace.MutationSummary) tui.MutationSummary {
	warnings := make([]string, 0)
	for _, result := range summary.Results {
		for _, warning := range result.Warnings {
			warnings = append(warnings, result.RelativePath+": "+warning)
		}
		if result.Err != nil {
			warnings = append(warnings, result.RelativePath+": "+result.Err.Error())
		}
	}
	recoveryID, recoveryKind := retainedRecoveryGate(operation, summary)
	return tui.MutationSummary{
		Operation:     operation,
		UndoID:        summary.UndoID,
		RecoveryID:    recoveryID,
		RecoveryKind:  recoveryKind,
		Succeeded:     summary.Succeeded + summary.Restored,
		Skipped:       summary.Skipped + summary.Conflicted,
		Failed:        summary.Failed,
		Cancelled:     summary.Cancelled,
		NotAttempted:  summary.NotAttempted,
		Warnings:      warnings,
		UndoAvailable: summary.UndoAvailable,
	}
}

func retainedRecoveryGate(operation string, summary workspace.MutationSummary) (string, tui.RecoveryGateKind) {
	if summary.BlockingRecoveryID != "" {
		return summary.BlockingRecoveryID, tui.RecoveryGateApply
	}
	if operation == "undo" && summary.UndoAvailable {
		id := summary.UndoID
		if id == "" {
			id = summary.TransactionID
		}
		if id != "" {
			return id, tui.RecoveryGateUndo
		}
	}
	return "", ""
}

func issueMessage(issue workspace.Issue) string {
	if issue.Err != nil {
		return issue.Err.Error()
	}
	return string(issue.Kind)
}

func sortTUIFiles(files []tui.File) {
	sort.SliceStable(files, func(left, right int) bool {
		leftFolded := strings.ToLower(files[left].Path)
		rightFolded := strings.ToLower(files[right].Path)
		if leftFolded == rightFolded {
			return files[left].Path < files[right].Path
		}
		return leftFolded < rightFolded
	})
}
