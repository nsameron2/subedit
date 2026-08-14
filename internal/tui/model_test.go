package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type fakeBackend struct {
	discoveries chan DiscoveryEvent
	searches    map[uint64]chan SearchEvent
	mutations   chan MutationEvent
	undos       chan MutationEvent
	requests    []MutationRequest
	undoCalls   int
}

type fakeRecoveryBackend struct {
	*fakeBackend
	recovery chan RecoveryEvent
	request  RecoveryRequest
}

type fakeEditorBackend struct {
	*fakeBackend
	opens           chan EditorOpenEvent
	refreshes       chan EditorOpenEvent
	editorSearches  chan EditorSearchEvent
	editorUndos     chan MutationEvent
	openRequests    []EditorOpenRequest
	refreshRequests []EditorRefreshRequest
	searchRequests  []EditorSearchRequest
	undoRequests    []EditorUndoRequest
}

func newFakeEditorBackend() *fakeEditorBackend {
	return &fakeEditorBackend{
		fakeBackend: newFakeBackend(),
		opens:       make(chan EditorOpenEvent, 8), refreshes: make(chan EditorOpenEvent, 8),
		editorSearches: make(chan EditorSearchEvent, 8), editorUndos: make(chan MutationEvent, 8),
	}
}

func (f *fakeEditorBackend) OpenEditor(_ context.Context, request EditorOpenRequest) <-chan EditorOpenEvent {
	f.openRequests = append(f.openRequests, request)
	return f.opens
}

func (f *fakeEditorBackend) SearchEditor(_ context.Context, request EditorSearchRequest) <-chan EditorSearchEvent {
	f.searchRequests = append(f.searchRequests, request)
	return f.editorSearches
}

func (f *fakeEditorBackend) RefreshEditor(_ context.Context, request EditorRefreshRequest) <-chan EditorOpenEvent {
	f.refreshRequests = append(f.refreshRequests, request)
	return f.refreshes
}

func (f *fakeEditorBackend) UndoEditor(_ context.Context, request EditorUndoRequest) <-chan MutationEvent {
	f.undoRequests = append(f.undoRequests, request)
	return f.editorUndos
}

func newFakeRecoveryBackend() *fakeRecoveryBackend {
	return &fakeRecoveryBackend{fakeBackend: newFakeBackend(), recovery: make(chan RecoveryEvent, 8)}
}

func (f *fakeRecoveryBackend) ListRecoveries(context.Context) ([]RecoveryItem, error) {
	return nil, nil
}
func (f *fakeRecoveryBackend) ResolveRecovery(_ context.Context, request RecoveryRequest) <-chan RecoveryEvent {
	f.request = request
	return f.recovery
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		discoveries: make(chan DiscoveryEvent, 8),
		searches:    make(map[uint64]chan SearchEvent),
		mutations:   make(chan MutationEvent, 8),
		undos:       make(chan MutationEvent, 8),
	}
}

func (f *fakeBackend) Discover(context.Context) <-chan DiscoveryEvent { return f.discoveries }
func (f *fakeBackend) Search(_ context.Context, request SearchRequest) <-chan SearchEvent {
	ch := f.searches[request.Revision]
	if ch == nil {
		ch = make(chan SearchEvent, 8)
		f.searches[request.Revision] = ch
	}
	return ch
}
func (f *fakeBackend) Mutate(_ context.Context, request MutationRequest) <-chan MutationEvent {
	f.requests = append(f.requests, request)
	return f.mutations
}
func (f *fakeBackend) Undo(context.Context) <-chan MutationEvent {
	f.undoCalls++
	return f.undos
}

func key(value string) tea.KeyPressMsg {
	switch value {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+f":
		return tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "home":
		return tea.KeyPressMsg{Code: tea.KeyHome}
	case "end":
		return tea.KeyPressMsg{Code: tea.KeyEnd}
	default:
		return tea.KeyPressMsg{Code: []rune(value)[0], Text: value}
	}
}

func updateModel(t *testing.T, model Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := model.Update(msg)
	result, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", next)
	}
	return result, cmd
}

func completeDiscovery(t *testing.T, m Model, files []File) Model {
	t.Helper()
	event := DiscoveryEvent{Discovery: &Discovery{Files: files}, Completed: len(files), Total: len(files), Done: true}
	next, _ := updateModel(t, m, discoveryStreamMsg{event: event, ok: true})
	return next
}

func matchingFile() File {
	return File{
		ID:         "a.srt",
		Path:       "season/a.srt",
		Valid:      true,
		MatchCount: 2,
		MatchIDs:   []string{"1", "4"},
		Preview: []Cue{
			{ID: "1", Timestamp: "00:00:01.000 → 00:00:02.000", Text: "Thank you for watching"},
			{ID: "4", Timestamp: "00:00:08.000 → 00:00:09.000", Text: "thank you for watching!"},
		},
	}
}

func readyModel(t *testing.T) (Model, *fakeBackend) {
	t.Helper()
	backend := newFakeBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{matchingFile()})
	return m, backend
}

func editorDocument(snapshot string) EditorDocument {
	return EditorDocument{
		FileID: "a.srt", Path: "season/a.srt", SnapshotID: snapshot,
		Cues: []Cue{
			{ID: "1", Timestamp: "00:00:01.000", Text: "opening"},
			{ID: "2", Timestamp: "00:00:02.000", Text: "garbage phrase"},
			{ID: "3", Timestamp: "00:00:03.000", Text: "closing"},
		},
	}
}

func readyEditorModel(t *testing.T, query string, visibleIDs []string) (Model, *fakeEditorBackend) {
	t.Helper()
	backend := newFakeEditorBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{matchingFile()})
	m.state = StateResults
	m.input.Blur()
	if query != "" {
		m.input.SetValue(query)
		m.queryRevision = 2
		m.displayedRevision = 2
		m.result = SearchResult{
			Query: query, Revision: 2, Files: []File{matchingFile()},
			MatchingCues: 2, MatchingFiles: 1, TotalFiles: 1,
		}
	}

	var openCmd tea.Cmd
	m, openCmd = updateModel(t, m, key("enter"))
	if m.State() != StateEditorLoading || openCmd == nil {
		t.Fatalf("open state=%s cmd nil=%t", m.State(), openCmd == nil)
	}
	backend.opens <- EditorOpenEvent{Revision: 1, Document: ptrEditorDocument(editorDocument("snapshot-1")), Done: true}
	openMessage := openCmd()
	var searchCmd tea.Cmd
	m, searchCmd = updateModel(t, m, openMessage)
	if searchCmd == nil {
		t.Fatal("open completion did not start local editor search")
	}
	backend.editorSearches <- EditorSearchEvent{
		Revision: 1, SnapshotID: "snapshot-1", Query: query,
		CueIDs: append([]string(nil), visibleIDs...), Done: true,
	}
	m, _ = updateModel(t, m, searchCmd())
	if m.State() != StateEditor || m.editor.loading || m.editor.searching {
		t.Fatalf("ready editor state=%s loading=%t searching=%t", m.State(), m.editor.loading, m.editor.searching)
	}
	return m, backend
}

func ptrEditorDocument(document EditorDocument) *EditorDocument { return &document }

func TestStartsSearchFocusedAfterDiscovery(t *testing.T) {
	m, _ := readyModel(t)
	if m.State() != StateSearch {
		t.Fatalf("state = %s, want search", m.State())
	}
	if !m.input.Focused() {
		t.Fatal("search input is not focused")
	}
	if got := m.render(); !strings.Contains(got, "Search:") || !strings.Contains(got, "season/a.srt") {
		t.Fatalf("render missing search or card:\n%s", got)
	}
}

func TestResultsHelpExplainsDeleteScopes(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateResults
	m.input.Blur()

	compact := m.helpLine()
	if !strings.Contains(compact, "I delete all-file matches") ||
		!strings.Contains(compact, "P delete focused-file matches") {
		t.Fatalf("compact help does not explain delete scopes: %q", compact)
	}

	m.help = true
	expanded := m.render()
	if !strings.Contains(expanded, "P delete matches in focused file") ||
		!strings.Contains(expanded, "I delete matches in all files") {
		t.Fatalf("expanded help does not explain delete scopes:\n%s", expanded)
	}
}

func TestYellowThemeUsesDistinctMatchColor(t *testing.T) {
	m := New(newFakeBackend(), Options{Width: 90, Height: 28})
	p := m.palette()
	if p.accent != p.brand {
		t.Fatal("primary accent and wordmark colors differ")
	}
	if p.match == p.accent {
		t.Fatal("match color must remain distinct from the yellow accent")
	}
	if m.progress.FullColor != p.accent {
		t.Fatal("progress bar does not use the primary accent")
	}
	if m.input.Styles().Cursor.Color != p.accent {
		t.Fatal("search cursor does not use the primary accent")
	}
}

func TestActionKeysTypeWhileSearchFocused(t *testing.T) {
	m, backend := readyModel(t)
	m, _ = updateModel(t, m, key("i"))
	if got := m.Query(); got != "i" {
		t.Fatalf("query = %q, want i", got)
	}
	if m.State() != StateSearch {
		t.Fatalf("state = %s, action key escaped input", m.State())
	}
	if len(backend.requests) != 0 {
		t.Fatal("mutation started while search was focused")
	}
}

func TestTabEntersSelectionFromFocusedSearch(t *testing.T) {
	m, _ := readyModel(t)
	m.input.SetValue("garbage")
	m.queryRevision = 2
	m.displayedRevision = 2
	m.result = SearchResult{Revision: 2, Query: "garbage", Files: []File{matchingFile()}, MatchingCues: 2, MatchingFiles: 1, TotalFiles: 1}
	m.state = StateSearch
	m.input.Focus()
	m, _ = updateModel(t, m, key("tab"))
	if m.State() != StateSelection || m.input.Focused() {
		t.Fatalf("Tab state=%s focused=%t, want selection/unfocused", m.State(), m.input.Focused())
	}
}

func TestStaleSearchResultIsIgnored(t *testing.T) {
	m, _ := readyModel(t)
	m, _ = updateModel(t, m, key("x")) // revision 2
	m, _ = updateModel(t, m, key("y")) // revision 3
	stale := SearchEvent{Done: true, Result: SearchResult{Revision: 2, Query: "x", Files: []File{matchingFile()}, MatchingCues: 2}}
	m, _ = updateModel(t, m, searchStreamMsg{event: stale, revision: 2, ok: true})
	if m.displayedRevision != 0 {
		t.Fatalf("displayed stale revision %d", m.displayedRevision)
	}
	if m.actionsReady() {
		t.Fatal("destructive actions enabled for stale result")
	}
}

func TestDeleteAllConfirmationAndPinnedRequest(t *testing.T) {
	m, backend := readyModel(t)
	m.input.SetValue("Thank you for watching")
	m.queryRevision = 7
	m.result = SearchResult{
		Query: "Thank you for watching", Revision: 7, Files: []File{matchingFile()},
		MatchingCues: 2, MatchingFiles: 1, TotalFiles: 1,
	}
	m.displayedRevision = 7
	m.state = StateResults
	m.input.Blur()
	m.rebuildCards()

	m, _ = updateModel(t, m, key("i"))
	if m.State() != StateConfirmation || !strings.Contains(m.confirm.question, "2 matching cues from 1 files") {
		t.Fatalf("confirmation = state %s, question %q", m.State(), m.confirm.question)
	}
	m, cmd := updateModel(t, m, key("y"))
	if m.State() != StateMutation || cmd == nil {
		t.Fatalf("after confirm: state=%s cmd nil=%t", m.State(), cmd == nil)
	}
	// Run the initial command far enough for the fake backend to capture its
	// request. A buffered channel keeps the command from blocking.
	backend.mutations <- MutationEvent{Done: true, Summary: &MutationSummary{Succeeded: 1, UndoAvailable: true}}
	_ = cmd()
	if len(backend.requests) != 1 {
		t.Fatalf("mutation requests = %d, want 1", len(backend.requests))
	}
	request := backend.requests[0]
	if request.Revision != 7 || request.Query != "Thank you for watching" || len(request.Targets) != 1 || len(request.Targets[0].CueIDs) != 2 {
		t.Fatalf("unexpected request: %#v", request)
	}
}

func TestSelectionAndDeleteSelected(t *testing.T) {
	m, _ := readyModel(t)
	m.input.SetValue("garbage")
	m.queryRevision = 2
	m.displayedRevision = 2
	m.result = SearchResult{Revision: 2, Query: "garbage", Files: []File{matchingFile()}, MatchingCues: 2, MatchingFiles: 1, TotalFiles: 1}
	m.state = StateResults
	m.input.Blur()

	m, _ = updateModel(t, m, key("tab"))
	m, _ = updateModel(t, m, key("space"))
	if m.State() != StateSelection || m.selectedCount() != 1 {
		t.Fatalf("selection state=%s selected=%d", m.State(), m.selectedCount())
	}
	m, _ = updateModel(t, m, key("o"))
	if m.State() != StateConfirmation || m.confirm.request.Scope != DeleteSelected {
		t.Fatalf("selected delete state=%s request=%#v", m.State(), m.confirm.request)
	}
}

func TestMutationCancellationAndSummary(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateMutation
	m.op = opDelete
	_, cancel := context.WithCancel(context.Background())
	m.mutationCancel = cancel
	m, _ = updateModel(t, m, key("ctrl+c"))
	if !m.cancelRequested || !strings.Contains(m.render(), "Operation cancelled by user") {
		t.Fatalf("cancel not reflected: requested=%t\n%s", m.cancelRequested, m.render())
	}
	summary := &MutationSummary{Operation: "delete", Succeeded: 3, Cancelled: true, NotAttempted: 2, UndoAvailable: true}
	m, _ = updateModel(t, m, mutationStreamMsg{ok: true, event: MutationEvent{Done: true, Summary: summary}})
	if m.State() != StateSummary || !m.undoAvailable {
		t.Fatalf("summary state=%s undo=%t", m.State(), m.undoAvailable)
	}
	view := m.render()
	if !strings.Contains(view, "3 succeeded") || !strings.Contains(view, "2 not attempted") {
		t.Fatalf("summary view missing counts:\n%s", view)
	}
}

func TestMutationProgressAccumulatesOutcomeDeltas(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateMutation
	m.op = opDelete
	stream := make(chan MutationEvent)
	m, _ = updateModel(t, m, mutationStreamMsg{ok: true, ch: stream, event: MutationEvent{Progress: &MutationProgress{Completed: 1, Total: 3, Succeeded: 1}}})
	m, _ = updateModel(t, m, mutationStreamMsg{ok: true, ch: stream, event: MutationEvent{Progress: &MutationProgress{Completed: 2, Total: 3, Skipped: 1}}})
	m, _ = updateModel(t, m, mutationStreamMsg{ok: true, ch: stream, event: MutationEvent{Progress: &MutationProgress{Completed: 3, Total: 3, Succeeded: 1}}})
	if m.mutProg.Succeeded != 2 || m.mutProg.Skipped != 1 || m.mutProg.Failed != 0 {
		t.Fatalf("progress = %#v, want 2 succeeded / 1 skipped", m.mutProg)
	}
}

func TestSummaryWaitsForRefreshAndOffersUndo(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateSummary
	m.summary = &MutationSummary{Succeeded: 1, UndoAvailable: true}
	m.undoAvailable = true
	m.refreshBehindSummary = true
	m.discovering = true
	m, _ = updateModel(t, m, key("u"))
	if m.State() != StateSummary {
		t.Fatalf("left summary before refresh: %s", m.State())
	}
	m.refreshBehindSummary = false
	m.discovering = false
	m, _ = updateModel(t, m, key("u"))
	if m.State() != StateConfirmation || m.confirm.kind != confirmUndo {
		t.Fatalf("undo not offered after refresh: state=%s confirm=%v", m.State(), m.confirm.kind)
	}
}

func TestResizeBlocksDestructiveConfirmation(t *testing.T) {
	m, _ := readyModel(t)
	m.input.SetValue("garbage")
	m.queryRevision = 2
	m.displayedRevision = 2
	m.result = SearchResult{Revision: 2, Files: []File{matchingFile()}, MatchingCues: 2, MatchingFiles: 1, TotalFiles: 1}
	m.state = StateResults
	m.input.Blur()
	m, _ = updateModel(t, m, tea.WindowSizeMsg{Width: 40, Height: 10})
	m, _ = updateModel(t, m, key("i"))
	if m.State() == StateConfirmation {
		t.Fatal("destructive confirmation opened in unsafe terminal size")
	}
	if !strings.Contains(m.render(), "Destructive actions are disabled") {
		t.Fatalf("resize view missing safety message:\n%s", m.render())
	}
}

func TestMutationCanBeCancelledAfterResizeTooSmall(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateMutation
	m.op = opDelete
	_, cancel := context.WithCancel(context.Background())
	m.mutationCancel = cancel
	m, _ = updateModel(t, m, tea.WindowSizeMsg{Width: 40, Height: 10})
	m, _ = updateModel(t, m, key("ctrl+c"))
	if !m.cancelRequested {
		t.Fatal("mutation cancellation was blocked by small terminal")
	}
	if !strings.Contains(m.render(), "Operation cancelled by user") {
		t.Fatalf("mutation progress hidden after resize:\n%s", m.render())
	}
}

func TestExplicitQuitFlagAndMutationCtrlC(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateResults
	m.input.Blur()
	m, _ = updateModel(t, m, key("q"))
	if !m.UserQuitRequested() {
		t.Fatal("explicit Q did not mark clean user quit")
	}
	m, _ = readyModel(t)
	m.state = StateConfirmation
	m.confirm = confirmation{kind: confirmDelete, back: StateResults}
	m, _ = updateModel(t, m, key("q"))
	if !m.UserQuitRequested() {
		t.Fatal("explicit Q from confirmation did not mark clean user quit")
	}

	m, _ = readyModel(t)
	m.state = StateMutation
	m.op = opDelete
	_, cancel := context.WithCancel(context.Background())
	m.mutationCancel = cancel
	m, _ = updateModel(t, m, key("ctrl+c"))
	if m.UserQuitRequested() || !m.cancelRequested {
		t.Fatalf("mutation Ctrl+C cleanQuit=%t cancel=%t", m.UserQuitRequested(), m.cancelRequested)
	}
}

func TestInitialDurableUndoSkipsRecoveryPromptAndEnablesUndo(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{
		Width: 90, Height: 28, DisableColor: true,
		InitialUndoAvailable: true,
	})
	if m.State() != StateRecovery {
		t.Fatalf("initial state = %s, want recovery inspection", m.State())
	}

	// The durable undo target is deliberately absent from ListRecoveries; an
	// empty list must proceed to ordinary discovery rather than presenting it
	// as a crash remnant.
	m, cmd := updateModel(t, m, recoveriesMsg{})
	if m.State() != StateDiscovery || cmd == nil {
		t.Fatalf("after recovery inspection state=%s cmd nil=%t", m.State(), cmd == nil)
	}
	m = completeDiscovery(t, m, []File{matchingFile()})
	if m.State() != StateSearch || !m.undoAvailable || m.retainedUndo != "" {
		t.Fatalf("startup state=%s undo=%t retained=%q", m.State(), m.undoAvailable, m.retainedUndo)
	}
	m, _ = updateModel(t, m, key("enter"))
	m, _ = updateModel(t, m, key("u"))
	if m.State() != StateConfirmation || m.confirm.kind != confirmUndo {
		t.Fatalf("durable undo was not exposed: state=%s confirm=%v", m.State(), m.confirm.kind)
	}
}

func TestInitialPartialDurableUndoUsesRetryDiscardGate(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{
		Width: 90, Height: 28, DisableColor: true,
		InitialRetainedUndoID: "tx-durable-partial",
	})
	m, _ = updateModel(t, m, recoveriesMsg{})
	m = completeDiscovery(t, m, []File{matchingFile()})
	if !m.undoAvailable || m.retainedUndo != "tx-durable-partial" {
		t.Fatalf("partial startup undo=%t retained=%q", m.undoAvailable, m.retainedUndo)
	}
	m.input.SetValue("garbage")
	m.queryRevision = 2
	m.displayedRevision = 2
	m.result = SearchResult{Revision: 2, Query: "garbage", Files: []File{matchingFile()}, MatchingCues: 2, MatchingFiles: 1, TotalFiles: 1}
	m.state = StateResults
	m.input.Blur()
	if got := m.helpLine(); !strings.Contains(got, "U retry undo") || !strings.Contains(got, "D discard recovery") {
		t.Fatalf("partial undo gate is not visible: %q", got)
	}
	m, _ = updateModel(t, m, key("i"))
	if m.State() == StateConfirmation {
		t.Fatal("initial partial undo did not block a ready delete")
	}

	m, _ = updateModel(t, m, key("u"))
	if m.State() != StateConfirmation || m.confirm.kind != confirmUndo {
		t.Fatalf("partial retry unavailable: state=%s confirm=%v", m.State(), m.confirm.kind)
	}
	m, _ = updateModel(t, m, key("n"))
	m, _ = updateModel(t, m, key("d"))
	if m.State() != StateConfirmation || m.confirm.kind != confirmRecoveryDiscard ||
		m.confirm.recovery.ID != "tx-durable-partial" || m.confirm.recovery.Action != RecoveryDiscard {
		t.Fatalf("partial discard confirmation = %#v", m.confirm)
	}
}

func TestPartialUndoBlocksDeleteAndCanBeDiscarded(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{matchingFile()})
	m.input.SetValue("garbage")
	m.queryRevision = 2
	m.displayedRevision = 2
	m.result = SearchResult{Revision: 2, Query: "garbage", Files: []File{matchingFile()}, MatchingCues: 2, MatchingFiles: 1, TotalFiles: 1}
	m.state = StateResults
	m.input.Blur()
	m.undoAvailable = true
	m.retainedUndo = "tx-partial"

	m, _ = updateModel(t, m, key("i"))
	if m.State() == StateConfirmation {
		t.Fatal("delete was allowed while partial undo recovery remained")
	}
	m, _ = updateModel(t, m, key("d"))
	if m.State() != StateConfirmation || m.confirm.recovery.ID != "tx-partial" {
		t.Fatalf("discard confirmation missing: state=%s request=%#v", m.State(), m.confirm.recovery)
	}
	m, cmd := updateModel(t, m, key("y"))
	backend.recovery <- RecoveryEvent{Done: true}
	message := cmd()
	if backend.request.Action != RecoveryDiscard || backend.request.ID != "tx-partial" {
		t.Fatalf("discard request = %#v", backend.request)
	}
	m, _ = updateModel(t, m, message)
	if m.retainedUndo != "" || m.undoAvailable || m.State() != StateSummary {
		t.Fatalf("discard did not clear gate: retained=%q undo=%t state=%s", m.retainedUndo, m.undoAvailable, m.State())
	}
}

func TestPendingApplyGateRoutesRetryThroughRecoveryBackend(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{matchingFile()})
	m.state = StateMutation
	m.op = opDelete
	m.finishMutation(&MutationSummary{
		Operation: "delete", RecoveryID: "tx-pending-apply", RecoveryKind: RecoveryGateApply,
		Succeeded: 1, Failed: 1, UndoAvailable: true,
	}, errors.New("recovery remains pending"))

	if m.pendingRecovery != "tx-pending-apply" || m.retainedUndo != "" || !m.undoAvailable {
		t.Fatalf("pending gate not recorded: pending=%q undoID=%q undo=%t", m.pendingRecovery, m.retainedUndo, m.undoAvailable)
	}
	if got := m.renderSummary(); !strings.Contains(got, "pending delete recovery") || !strings.Contains(got, "R restore pending delete") {
		t.Fatalf("pending recovery wording/controls missing:\n%s", got)
	}

	// U is accepted as a compatibility alias, but it must restore this exact
	// pending recovery rather than invoking ordinary Backend.Undo.
	m, _ = updateModel(t, m, key("u"))
	if m.State() != StateConfirmation || m.confirm.kind != confirmRecoveryRestore ||
		m.confirm.recovery.ID != "tx-pending-apply" {
		t.Fatalf("pending retry confirmation = %#v", m.confirm)
	}
	m, cmd := updateModel(t, m, key("y"))
	backend.recovery <- RecoveryEvent{Done: true, Summary: &RecoverySummary{Undo: &UndoSnapshot{Available: true}}}
	message := cmd()
	if backend.request.ID != "tx-pending-apply" || backend.request.Action != RecoveryRestore {
		t.Fatalf("pending retry request = %#v", backend.request)
	}
	m, refresh := updateModel(t, m, message)
	if m.pendingRecovery != "" || !m.undoAvailable || m.State() != StateSummary {
		t.Fatalf("resolved pending state: pending=%q undo=%t state=%s", m.pendingRecovery, m.undoAvailable, m.State())
	}
	if refresh == nil || !m.refreshBehindSummary {
		t.Fatal("restored files were not scheduled for a safe post-recovery rescan")
	}
}

func TestPendingApplyDiscardUsesExactIDAndAuthoritativeUndo(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{matchingFile()})
	m.state = StateMutation
	m.op = opDelete
	m.finishMutation(&MutationSummary{
		Operation: "delete", RecoveryID: "tx-pending-discard", RecoveryKind: RecoveryGateApply,
		UndoAvailable: true,
	}, errors.New("recovery remains pending"))

	// The explicit pending identity gates deletes even with a zero-success
	// summary; outcome counts alone are not a safety signal.
	m.state = StateResults
	m.input.SetValue("garbage")
	m.queryRevision, m.displayedRevision = 2, 2
	m.result = SearchResult{Revision: 2, Files: []File{matchingFile()}}
	m.discDone = true
	m, _ = updateModel(t, m, key("i"))
	if m.State() == StateConfirmation {
		t.Fatal("I delete was allowed while pending apply recovery remained")
	}
	m, _ = updateModel(t, m, key("p"))
	if m.State() == StateConfirmation {
		t.Fatal("P delete was allowed while pending apply recovery remained")
	}
	m.state = StateSelection
	m.selected[matchingFile().ID] = true
	m, _ = updateModel(t, m, key("o"))
	if m.State() == StateConfirmation {
		t.Fatal("O delete was allowed while pending apply recovery remained")
	}
	m.state = StateResults

	m, _ = updateModel(t, m, key("d"))
	if m.State() != StateConfirmation || m.confirm.recovery.ID != "tx-pending-discard" ||
		m.confirm.recovery.Action != RecoveryDiscard {
		t.Fatalf("pending discard confirmation = %#v", m.confirm)
	}
	m, cmd := updateModel(t, m, key("y"))
	backend.recovery <- RecoveryEvent{Done: true, Summary: &RecoverySummary{Undo: &UndoSnapshot{}}}
	m, refresh := updateModel(t, m, cmd())
	if backend.request.ID != "tx-pending-discard" || backend.request.Action != RecoveryDiscard {
		t.Fatalf("pending discard request = %#v", backend.request)
	}
	if m.pendingRecovery != "" || m.undoAvailable || m.State() != StateSummary || refresh != nil {
		t.Fatalf("discard result: pending=%q undo=%t state=%s refresh nil=%t", m.pendingRecovery, m.undoAvailable, m.State(), refresh == nil)
	}
}

func TestPartialPendingApplyRestoreRemainsReachable(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{matchingFile()})
	m.pendingRecovery = "tx-pending"
	m.state = StateSummary
	m.summary = &MutationSummary{RecoveryID: "tx-pending", RecoveryKind: RecoveryGateApply}

	m, _ = updateModel(t, m, key("r"))
	m, cmd := updateModel(t, m, key("y"))
	backend.recovery <- RecoveryEvent{Done: true, Summary: &RecoverySummary{
		Succeeded: 1, Failed: 1, Retained: true, Undo: &UndoSnapshot{Available: true},
	}}
	m, refresh := updateModel(t, m, cmd())
	if m.pendingRecovery != "tx-pending" || m.State() != StateSummary {
		t.Fatalf("partial pending recovery lost gate: pending=%q state=%s", m.pendingRecovery, m.State())
	}
	if refresh == nil || !strings.Contains(m.status, "remains unresolved") {
		t.Fatalf("partial pending recovery refresh/status: refresh nil=%t status=%q", refresh == nil, m.status)
	}
}

func TestPendingApplyRecoveryErrorPreservesGateAndControls(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{matchingFile()})
	m.pendingRecovery = "tx-pending-error"
	m.state = StateSummary
	m.summary = &MutationSummary{RecoveryID: "tx-pending-error", RecoveryKind: RecoveryGateApply}

	m, _ = updateModel(t, m, key("d"))
	m, cmd := updateModel(t, m, key("y"))
	backend.recovery <- RecoveryEvent{
		Done: true, Err: errors.New("discard failed"),
		Summary: &RecoverySummary{Undo: &UndoSnapshot{}},
	}
	m, _ = updateModel(t, m, cmd())
	if m.pendingRecovery != "tx-pending-error" || m.State() != StateSummary {
		t.Fatalf("failed pending recovery lost gate: pending=%q state=%s", m.pendingRecovery, m.State())
	}
	if got := m.renderSummary(); !strings.Contains(got, "R restore pending delete") || !strings.Contains(got, "D discard recovery") {
		t.Fatalf("pending retry controls unavailable after error:\n%s", got)
	}
}

func TestGracefulCompleteDeleteDoesNotCreatePendingGate(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateMutation
	m.op = opDelete
	m.finishMutation(&MutationSummary{Operation: "delete", Succeeded: 1, UndoAvailable: true}, nil)
	if m.pendingRecovery != "" || m.retainedUndo != "" || !m.undoAvailable {
		t.Fatalf("ordinary delete state: pending=%q retained=%q undo=%t", m.pendingRecovery, m.retainedUndo, m.undoAvailable)
	}
	if normalizedRecoveryKind(*m.summary) != "" || strings.Contains(m.renderSummary(), "pending delete recovery") {
		t.Fatalf("ordinary delete falsely rendered a recovery gate: %#v", m.summary)
	}
}

func TestRetainedUndoRecoveryFailureKeepsRetryControlsReachable(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{matchingFile()})
	m.state = StateResults
	m.input.Blur()
	m.undoAvailable = true
	m.retainedUndo = "tx-partial"

	m, _ = updateModel(t, m, key("d"))
	m, cmd := updateModel(t, m, key("y"))
	backend.recovery <- RecoveryEvent{
		Done: true,
		Err:  errors.New("discard failed"),
		// Even an intermediate snapshot that no longer exposes this as current
		// undo must not make a failed recovery operation silently dismiss it.
		Summary: &RecoverySummary{Undo: &UndoSnapshot{}},
	}
	m, _ = updateModel(t, m, cmd())
	if m.State() != StateSummary || !m.undoAvailable || m.retainedUndo != "tx-partial" {
		t.Fatalf("failed discard lost retry gate: state=%s undo=%t retained=%q", m.State(), m.undoAvailable, m.retainedUndo)
	}
	if got := m.renderSummary(); !strings.Contains(got, "U retry undo") || !strings.Contains(got, "D discard recovery") {
		t.Fatalf("retry controls not visible after failed discard:\n%s", got)
	}
	m, _ = updateModel(t, m, key("d"))
	if m.State() != StateConfirmation || m.confirm.recovery.ID != "tx-partial" {
		t.Fatalf("discard retry unavailable: state=%s confirmation=%#v", m.State(), m.confirm)
	}
}

func TestPartialStartupRestoreStaysGated(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m.recoveries = []RecoveryItem{{ID: "crash", Files: 3}}
	m.state = StateMutation
	m.op = opRecovery
	event := RecoveryEvent{Done: true, Summary: &RecoverySummary{Succeeded: 1, Skipped: 2, Retained: true}}
	m, _ = updateModel(t, m, recoveryStreamMsg{event: event, ok: true})
	if m.State() != StateRecovery || len(m.recoveries) != 1 {
		t.Fatalf("partial restore advanced gate: state=%s recoveries=%d", m.State(), len(m.recoveries))
	}
	if !strings.Contains(m.status, "remains unresolved") {
		t.Fatalf("missing retained recovery status: %q", m.status)
	}
}

func TestStartupRecoveryPreservesCoexistingRetainedUndoByIdentity(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{
		Width: 90, Height: 28, DisableColor: true,
		InitialRetainedUndoID: "undo-partial",
	})
	m, _ = updateModel(t, m, recoveriesMsg{items: []RecoveryItem{
		{ID: "crash-newest", Files: 2},
		{ID: "crash-older", Files: 1},
	}})

	// Resolve the first listed crash item without an authoritative undo
	// snapshot. Compatibility backends may omit it, in which case the seeded
	// partial undo must be preserved rather than mistaken for the crash item.
	m, _ = updateModel(t, m, key("d"))
	m, cmd := updateModel(t, m, key("y"))
	backend.recovery <- RecoveryEvent{Done: true, Summary: &RecoverySummary{}}
	m, _ = updateModel(t, m, cmd())
	if len(m.recoveries) != 1 || m.recoveries[0].ID != "crash-older" {
		t.Fatalf("wrong recovery item removed: %#v", m.recoveries)
	}
	if !m.undoAvailable || m.retainedUndo != "undo-partial" {
		t.Fatalf("listed recovery consumed partial undo: undo=%t retained=%q", m.undoAvailable, m.retainedUndo)
	}
	if m.State() != StateRecovery {
		t.Fatalf("state after first recovery = %s, want recovery", m.State())
	}

	// An authoritative snapshot may replace the coexisting undo identity.
	m, _ = updateModel(t, m, key("r"))
	m, cmd = updateModel(t, m, key("y"))
	backend.recovery <- RecoveryEvent{Done: true, Summary: &RecoverySummary{Undo: &UndoSnapshot{
		Available: true, RetainedUndoID: "undo-replacement",
	}}}
	m, _ = updateModel(t, m, cmd())
	if len(m.recoveries) != 0 || m.State() != StateDiscovery {
		t.Fatalf("final crash recovery was not removed: state=%s items=%#v", m.State(), m.recoveries)
	}
	if !m.undoAvailable || m.retainedUndo != "undo-replacement" {
		t.Fatalf("authoritative undo replacement ignored: undo=%t retained=%q", m.undoAvailable, m.retainedUndo)
	}
}

func TestStartupRecoveryAuthoritativelyRetiresCoexistingUndo(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{
		Width: 90, Height: 28, DisableColor: true,
		InitialRetainedUndoID: "undo-partial",
	})
	m, _ = updateModel(t, m, recoveriesMsg{items: []RecoveryItem{{ID: "crash", Files: 1}}})
	m, _ = updateModel(t, m, key("d"))
	m, cmd := updateModel(t, m, key("y"))
	backend.recovery <- RecoveryEvent{Done: true, Summary: &RecoverySummary{Undo: &UndoSnapshot{}}}
	m, _ = updateModel(t, m, cmd())

	if len(m.recoveries) != 0 || m.State() != StateDiscovery {
		t.Fatalf("listed recovery not resolved: state=%s items=%#v", m.State(), m.recoveries)
	}
	if m.undoAvailable || m.retainedUndo != "" {
		t.Fatalf("authoritative undo retirement ignored: undo=%t retained=%q", m.undoAvailable, m.retainedUndo)
	}
}

func TestRecoveryCompletionRemovesExactRequestedID(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m.recoveries = []RecoveryItem{{ID: "first"}, {ID: "requested"}, {ID: "last"}}
	m.state = StateMutation
	m.op = opRecovery
	m.activeRecovery = RecoveryRequest{ID: "requested", Action: RecoveryDiscard}
	event := RecoveryEvent{Done: true, Summary: &RecoverySummary{Undo: &UndoSnapshot{}}}
	m, _ = updateModel(t, m, recoveryStreamMsg{
		event: event, request: m.activeRecovery, ok: true,
	})

	if len(m.recoveries) != 2 || m.recoveries[0].ID != "first" || m.recoveries[1].ID != "last" {
		t.Fatalf("completion removed by position instead of ID: %#v", m.recoveries)
	}
	if m.State() != StateRecovery {
		t.Fatalf("state = %s, want recovery for remaining items", m.State())
	}
}

func TestRecoveryRestoreCtrlCCancelsAndRetainsGate(t *testing.T) {
	backend := newFakeRecoveryBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m.recoveries = []RecoveryItem{{ID: "crash", Files: 3}}
	m.state = StateMutation
	m.op = opRecovery
	m.recoveryAction = RecoveryRestore
	ctx, cancel := context.WithCancel(context.Background())
	m.mutationCancel = cancel
	_ = ctx
	m, _ = updateModel(t, m, key("ctrl+c"))
	if !m.cancelRequested || m.UserQuitRequested() {
		t.Fatalf("recovery Ctrl+C cancel=%t quit=%t", m.cancelRequested, m.UserQuitRequested())
	}
	event := RecoveryEvent{Done: true, Summary: &RecoverySummary{Succeeded: 1, Retained: true}}
	m, _ = updateModel(t, m, recoveryStreamMsg{event: event, ok: true})
	if m.State() != StateRecovery || len(m.recoveries) != 1 {
		t.Fatalf("cancelled recovery removed gate: state=%s recoveries=%d", m.State(), len(m.recoveries))
	}
}

func TestFailedDeletePreservesEarlierUndo(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateMutation
	m.op = opDelete
	m.undoAvailable = true
	m.retainedUndo = "prior"
	m.finishMutation(&MutationSummary{Operation: "delete", Failed: 1}, nil)
	if !m.undoAvailable || m.retainedUndo != "prior" || m.summary.RecoveryID != "prior" {
		t.Fatalf("earlier undo was lost: undo=%t retained=%q summary=%#v", m.undoAvailable, m.retainedUndo, m.summary)
	}
}

func TestFailedUndoPreservesRetainedRecovery(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateMutation
	m.op = opUndo
	m.undoAvailable = true
	m.retainedUndo = "invalid-current"
	m.finishMutation(&MutationSummary{Operation: "undo", Failed: 1}, errors.New("undo target is invalid"))
	if !m.undoAvailable || m.retainedUndo != "invalid-current" || m.summary.RecoveryID != "invalid-current" {
		t.Fatalf("failed undo lost safety gate: undo=%t retained=%q summary=%#v", m.undoAvailable, m.retainedUndo, m.summary)
	}
}

func TestEditorOpenUsesFocusedFileAndOuterQueryOnlyAsLocalFilter(t *testing.T) {
	m, backend := readyEditorModel(t, "garbage", []string{"2"})
	if len(backend.openRequests) != 1 {
		t.Fatalf("open requests = %d, want 1", len(backend.openRequests))
	}
	open := backend.openRequests[0]
	if open.FileID != "a.srt" || open.Path != "season/a.srt" || open.Revision != 1 {
		t.Fatalf("open request = %#v", open)
	}
	if len(backend.searchRequests) != 1 {
		t.Fatalf("editor searches = %d, want 1", len(backend.searchRequests))
	}
	search := backend.searchRequests[0]
	if search.SnapshotID != "snapshot-1" || search.Query != "garbage" || search.Revision != 1 {
		t.Fatalf("editor search = %#v", search)
	}
	if m.Query() != "garbage" || m.editor.input.Value() != "garbage" {
		t.Fatalf("outer/local queries = %q / %q", m.Query(), m.editor.input.Value())
	}
	if len(m.editor.visible) != 1 || m.editor.visible[0] != 1 {
		t.Fatalf("visible source indices = %#v", m.editor.visible)
	}
}

func TestEditorUnavailableAndInvalidFilesFailWithoutOpening(t *testing.T) {
	m, _ := readyModel(t)
	m.state = StateResults
	m.input.Blur()
	m, cmd := updateModel(t, m, key("enter"))
	if cmd != nil || m.State() != StateResults || !strings.Contains(m.status, "unavailable") {
		t.Fatalf("missing backend result: state=%s cmd=%v status=%q", m.State(), cmd, m.status)
	}

	backend := newFakeEditorBackend()
	m = New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{{ID: "bad", Path: "bad.srt", Valid: false}})
	m.state = StateResults
	m.input.Blur()
	m, cmd = updateModel(t, m, key("enter"))
	if cmd != nil || len(backend.openRequests) != 0 || !strings.Contains(m.status, "invalid") {
		t.Fatalf("invalid file opened: cmd=%v requests=%d status=%q", cmd, len(backend.openRequests), m.status)
	}
}

func TestEditorMarksPersistAcrossFiltersAndFocusTracksSourcePosition(t *testing.T) {
	m, backend := readyEditorModel(t, "garbage", []string{"2"})
	m, _ = updateModel(t, m, key("space"))
	if !m.editor.marks["2"] {
		t.Fatal("focused cue was not marked")
	}

	m.captureEditorAnchor()
	before := m.editor.input.Value()
	m.editor.input.SetValue("closing")
	next, searchCmd := m.afterEditorInputChange(before, nil)
	m = next.(Model)
	backend.editorSearches <- EditorSearchEvent{
		Revision: 2, SnapshotID: "snapshot-1", Query: "closing", CueIDs: []string{"3"}, Done: true,
	}
	m, _ = updateModel(t, m, searchCmd())
	if !m.editor.marks["2"] || m.editorHiddenMarkedCount() != 1 {
		t.Fatalf("filter lost hidden mark: marks=%#v hidden=%d", m.editor.marks, m.editorHiddenMarkedCount())
	}
	if cue, ok := m.editorFocusedCue(); !ok || cue.ID != "3" {
		t.Fatalf("nearest source focus = %#v, %t", cue, ok)
	}

	before = m.editor.input.Value()
	m.editor.input.SetValue("")
	next, searchCmd = m.afterEditorInputChange(before, nil)
	m = next.(Model)
	backend.editorSearches <- EditorSearchEvent{
		Revision: 3, SnapshotID: "snapshot-1", Query: "", CueIDs: []string{"1", "2", "3"}, Done: true,
	}
	m, _ = updateModel(t, m, searchCmd())
	if cue, ok := m.editorFocusedCue(); !ok || cue.ID != "3" {
		t.Fatalf("source-position focus after clear = %#v, %t", cue, ok)
	}
	if !m.editor.marks["2"] {
		t.Fatal("mark did not survive restoring the cue to the filter")
	}
}

func TestStaleEditorSearchIsIgnoredAndDrained(t *testing.T) {
	m, _ := readyEditorModel(t, "", []string{"1", "2", "3"})
	m.editor.searchRevision = 3
	m.editor.input.SetValue("new")
	stream := make(chan EditorSearchEvent)
	stale := editorSearchStreamMsg{
		event: EditorSearchEvent{Revision: 2, SnapshotID: "snapshot-1", Query: "old", CueIDs: []string{"1"}},
		ch:    stream, revision: 2, snapshotID: "snapshot-1", query: "old", ok: true,
	}
	before := append([]int(nil), m.editor.visible...)
	m, cmd := updateModel(t, m, stale)
	if cmd == nil || len(m.editor.visible) != len(before) {
		t.Fatalf("stale search was not ignored/drained: cmd nil=%t visible=%#v", cmd == nil, m.editor.visible)
	}
}

func TestEditorRecoveryGateBlocksMutationButAllowsClearingMarks(t *testing.T) {
	m, _ := readyEditorModel(t, "", []string{"1", "2", "3"})
	m.editor.marks["1"] = true
	m.retainedUndo = "partial"
	m, _ = updateModel(t, m, key("space"))
	if len(m.editor.marks) != 1 || !strings.Contains(m.status, "Resolve") {
		t.Fatalf("gate allowed mark: marks=%#v status=%q", m.editor.marks, m.status)
	}
	m, _ = updateModel(t, m, key("s"))
	if m.State() == StateConfirmation {
		t.Fatal("gate allowed editor save confirmation")
	}
	m.editor.undoID = "editor-undo"
	m, _ = updateModel(t, m, key("u"))
	if m.State() == StateConfirmation {
		t.Fatal("gate allowed scoped editor undo")
	}
	m, _ = updateModel(t, m, key("n"))
	if m.editorDirty() {
		t.Fatal("recovery gate prevented clearing staged marks")
	}
}

func TestEditorSaveConfirmationDisclosesHiddenZeroAndUndoReplacement(t *testing.T) {
	m, _ := readyEditorModel(t, "garbage", []string{"2"})
	for _, cue := range m.editor.document.Cues {
		m.editor.marks[cue.ID] = true
	}
	m.editor.undoID = "prior-editor-undo"
	m, _ = updateModel(t, m, key("s"))
	if m.confirm.kind != confirmEditorSave {
		t.Fatalf("confirmation kind = %v", m.confirm.kind)
	}
	for _, text := range []string{"2 marked cues are hidden", "zero cues", "replace the current one-level undo"} {
		if !strings.Contains(m.confirm.question, text) {
			t.Fatalf("save confirmation missing %q: %q", text, m.confirm.question)
		}
	}
	request := m.confirm.request
	if request.Scope != DeleteEditor || request.Source != MutationSourceEditor || request.SnapshotID != "snapshot-1" || request.Query != "" {
		t.Fatalf("editor mutation request = %#v", request)
	}
	if got := request.Targets[0].CueIDs; strings.Join(got, ",") != "1,2,3" {
		t.Fatalf("cue IDs not in source order: %#v", got)
	}

	// Save chosen from a dirty-exit prompt must pass through this same detailed
	// destructive confirmation instead of bypassing its disclosures.
	m.state = StateEditor
	m.confirm = confirmation{}
	m, _ = updateModel(t, m, key("esc"))
	if m.confirm.kind != confirmEditorDirtyExit {
		t.Fatalf("dirty exit confirmation = %v", m.confirm.kind)
	}
	m, _ = updateModel(t, m, key("s"))
	if m.confirm.kind != confirmEditorSave || !strings.Contains(m.confirm.question, "zero cues") {
		t.Fatalf("save-on-exit bypassed detailed confirmation: %#v", m.confirm)
	}
	if m.editor.afterSave != editorAfterExit {
		t.Fatalf("save-on-exit action = %v", m.editor.afterSave)
	}
}

func TestAcceptedEditorSaveExactRefreshAndReturn(t *testing.T) {
	m, backend := readyEditorModel(t, "", []string{"1", "2", "3"})
	m, _ = updateModel(t, m, key("space"))
	m, _ = updateModel(t, m, key("s"))
	m, mutationCmd := updateModel(t, m, key("y"))
	if m.State() != StateMutation || mutationCmd == nil {
		t.Fatalf("save start state=%s cmd nil=%t", m.State(), mutationCmd == nil)
	}
	backend.mutations <- MutationEvent{Done: true, Summary: &MutationSummary{
		Operation: "delete", Succeeded: 1, UndoAvailable: true, UndoID: "editor-tx-1",
	}}
	m, refreshCmd := updateModel(t, m, mutationCmd())
	if m.State() != StateSummary || m.summaryBack != StateEditor || refreshCmd == nil || !m.editor.loading {
		t.Fatalf("post-save state=%s back=%s refresh nil=%t loading=%t", m.State(), m.summaryBack, refreshCmd == nil, m.editor.loading)
	}
	if len(backend.requests) != 1 {
		t.Fatalf("mutation requests = %d", len(backend.requests))
	}
	request := backend.requests[0]
	if request.Source != MutationSourceEditor || request.SnapshotID != "snapshot-1" || request.Query != "" {
		t.Fatalf("save request = %#v", request)
	}

	refreshed := editorDocument("snapshot-2")
	refreshed.Cues = refreshed.Cues[1:]
	backend.refreshes <- EditorOpenEvent{Revision: 2, Document: ptrEditorDocument(refreshed), Done: true}
	m, searchCmd := updateModel(t, m, refreshCmd())
	if len(backend.refreshRequests) != 1 || backend.refreshRequests[0].Path != "season/a.srt" {
		t.Fatalf("refresh requests = %#v", backend.refreshRequests)
	}
	backend.editorSearches <- EditorSearchEvent{
		Revision: 2, SnapshotID: "snapshot-2", Query: "", CueIDs: []string{"2", "3"}, Done: true,
	}
	m, _ = updateModel(t, m, searchCmd())
	if m.State() != StateSummary || m.editor.loading || m.editorDirty() || m.editor.undoID != "editor-tx-1" {
		t.Fatalf("refreshed summary state=%s loading=%t dirty=%t undo=%q", m.State(), m.editor.loading, m.editorDirty(), m.editor.undoID)
	}
	m, _ = updateModel(t, m, key("enter"))
	if m.State() != StateEditor || m.editor.document.SnapshotID != "snapshot-2" {
		t.Fatalf("summary did not return to editor: state=%s snapshot=%q", m.State(), m.editor.document.SnapshotID)
	}
}

func TestFailedEditorSaveClearsMarksAfterFreshReview(t *testing.T) {
	m, backend := readyEditorModel(t, "", []string{"1", "2", "3"})
	m, _ = updateModel(t, m, key("space"))
	m, _ = updateModel(t, m, key("s"))
	m, mutationCmd := updateModel(t, m, key("y"))
	backend.mutations <- MutationEvent{Done: true, Err: errors.New("conflict"), Summary: &MutationSummary{Failed: 1}}
	m, refreshCmd := updateModel(t, m, mutationCmd())
	backend.refreshes <- EditorOpenEvent{Revision: 2, Document: ptrEditorDocument(editorDocument("snapshot-2")), Done: true}
	m, searchCmd := updateModel(t, m, refreshCmd())
	backend.editorSearches <- EditorSearchEvent{
		Revision: 2, SnapshotID: "snapshot-2", Query: "", CueIDs: []string{"1", "2", "3"}, Done: true,
	}
	m, _ = updateModel(t, m, searchCmd())
	if m.editorDirty() || m.editor.marks["1"] {
		t.Fatalf("failed save rebased stale marks: %#v", m.editor.marks)
	}
}

func TestCommittedEditorSaveRefreshErrorRequiresReloadBeforeResume(t *testing.T) {
	m, backend := readyEditorModel(t, "", []string{"1", "2", "3"})
	m, _ = updateModel(t, m, key("space"))
	m, _ = updateModel(t, m, key("esc"))
	m, _ = updateModel(t, m, key("s")) // choose save-and-exit, then review detailed warning
	m, mutationCmd := updateModel(t, m, key("y"))
	backend.mutations <- MutationEvent{Done: true, Summary: &MutationSummary{
		Succeeded: 1, UndoAvailable: true, UndoID: "committed",
	}}
	m, refreshCmd := updateModel(t, m, mutationCmd())
	backend.refreshes <- EditorOpenEvent{Revision: 2, Err: errors.New("refresh failed"), Done: true}
	m, _ = updateModel(t, m, refreshCmd())
	if m.State() != StateSummary || m.editor.loading || !m.editor.stale {
		t.Fatalf("refresh error state=%s loading=%t stale=%t", m.State(), m.editor.loading, m.editor.stale)
	}
	m, _ = updateModel(t, m, key("enter"))
	if m.State() != StateEditor || m.UserQuitRequested() || !m.editor.stale {
		t.Fatalf("failed refresh executed deferred exit: state=%s quit=%t stale=%t", m.State(), m.UserQuitRequested(), m.editor.stale)
	}
	m, _ = updateModel(t, m, key("space"))
	if m.editorDirty() || !strings.Contains(m.status, "Reload") && !strings.Contains(m.status, "reload") {
		t.Fatalf("stale editor allowed marking or omitted reload guidance: dirty=%t status=%q", m.editorDirty(), m.status)
	}
	m, retry := updateModel(t, m, key("r"))
	if retry == nil || m.State() != StateEditorLoading {
		t.Fatalf("R did not retry exact refresh: state=%s cmd nil=%t", m.State(), retry == nil)
	}
}

func TestEditorSummaryUndoIsExactAndCannotHitNewerGlobalUndo(t *testing.T) {
	m, backend := readyEditorModel(t, "", []string{"1", "2", "3"})
	m.state = StateSummary
	m.summaryBack = StateEditor
	m.summary = &MutationSummary{Succeeded: 1, UndoAvailable: true, UndoID: "editor-tx"}
	m.undoAvailable = true // may now refer to a newer unrelated global undo
	m.editor.undoID = "editor-tx"
	m, _ = updateModel(t, m, key("u"))
	if m.confirm.kind != confirmEditorUndo {
		t.Fatalf("editor summary routed U to confirmation %v", m.confirm.kind)
	}
	m, undoCmd := updateModel(t, m, key("y"))
	backend.editorUndos <- MutationEvent{Done: true, Summary: &MutationSummary{Operation: "undo", Succeeded: 1}}
	m, refreshCmd := updateModel(t, m, undoCmd())
	if backend.fakeBackend.undoCalls != 0 || len(backend.undoRequests) != 1 {
		t.Fatalf("broad/scoped undo calls = %d/%d", backend.fakeBackend.undoCalls, len(backend.undoRequests))
	}
	request := backend.undoRequests[0]
	if request.Path != "season/a.srt" || request.UndoID != "editor-tx" {
		t.Fatalf("scoped undo request = %#v", request)
	}
	if refreshCmd == nil || m.State() != StateSummary {
		t.Fatalf("scoped undo did not schedule exact refresh: state=%s cmd nil=%t", m.State(), refreshCmd == nil)
	}
}

func TestPartialEditorUndoRetryRemainsTransactionAndPathScoped(t *testing.T) {
	m, backend := readyEditorModel(t, "", []string{"1", "2", "3"})
	m.state = StateSummary
	m.summaryBack = StateEditor
	m.summary = &MutationSummary{
		Operation: "undo", Skipped: 1, UndoAvailable: true,
		UndoID: "editor-tx", RecoveryID: "editor-tx", RecoveryKind: RecoveryGateUndo,
	}
	m.editor.undoID = "editor-tx"
	m.retainedUndo = "editor-tx"
	m.undoAvailable = true

	m, _ = updateModel(t, m, key("u"))
	if m.confirm.kind != confirmEditorUndo {
		t.Fatalf("partial editor retry routed to confirmation %v", m.confirm.kind)
	}
	m, undoCmd := updateModel(t, m, key("y"))
	if undoCmd == nil {
		t.Fatal("partial editor retry did not start")
	}
	backend.editorUndos <- MutationEvent{Done: true, Summary: &MutationSummary{
		Operation: "undo", Skipped: 1, UndoAvailable: true,
		UndoID: "editor-tx", RecoveryID: "editor-tx", RecoveryKind: RecoveryGateUndo,
	}}
	m, refreshCmd := updateModel(t, m, undoCmd())
	if backend.fakeBackend.undoCalls != 0 || len(backend.undoRequests) != 1 {
		t.Fatalf("partial retry broad/scoped calls = %d/%d", backend.fakeBackend.undoCalls, len(backend.undoRequests))
	}
	request := backend.undoRequests[0]
	if request.Path != "season/a.srt" || request.UndoID != "editor-tx" {
		t.Fatalf("partial retry request = %#v", request)
	}
	if refreshCmd == nil || m.retainedUndo != "editor-tx" || m.editor.undoID != "editor-tx" {
		t.Fatalf("partial retry lost recovery identity: refresh nil=%t retained=%q editor=%q", refreshCmd == nil, m.retainedUndo, m.editor.undoID)
	}

	// A mismatched retained identity can arise when another process changes the
	// durable undo chain. The stale editor must fail closed instead of invoking
	// broad workspace undo.
	m, backend = readyEditorModel(t, "", []string{"1", "2", "3"})
	m.state = StateSummary
	m.summaryBack = StateEditor
	m.editor.undoID = "stale-editor-tx"
	m.retainedUndo = "different-current-tx"
	m.undoAvailable = true
	m, cmd := updateModel(t, m, key("u"))
	if cmd != nil || m.confirm.kind != confirmNone || backend.fakeBackend.undoCalls != 0 || !strings.Contains(m.status, "exact scoped") {
		t.Fatalf("mismatched recovery did not fail closed: confirm=%v broad=%d status=%q", m.confirm.kind, backend.fakeBackend.undoCalls, m.status)
	}
}

func TestDiscardedEditorUndoRecoveryClearsLocalUndoProvenance(t *testing.T) {
	m, _ := readyEditorModel(t, "", []string{"1", "2", "3"})
	m.state = StateMutation
	m.summaryBack = StateEditor
	m.retainedUndo = "editor-tx"
	m.undoAvailable = true
	m.editor.undoID = "editor-tx"
	m.activeRecovery = RecoveryRequest{ID: "editor-tx", Action: RecoveryDiscard}

	var refreshCmd tea.Cmd
	m, refreshCmd = updateModel(t, m, recoveryStreamMsg{
		request: m.activeRecovery,
		event: RecoveryEvent{Done: true, Summary: &RecoverySummary{
			Undo: &UndoSnapshot{},
		}},
		ok: true,
	})
	if refreshCmd == nil || m.editor.undoID != "" || m.retainedUndo != "" || m.undoAvailable {
		t.Fatalf("discard retained stale editor undo: refresh nil=%t editor=%q retained=%q available=%t", refreshCmd == nil, m.editor.undoID, m.retainedUndo, m.undoAvailable)
	}
}

func TestEditorDirtyQuitRequiresDisposition(t *testing.T) {
	m, _ := readyEditorModel(t, "", []string{"1", "2", "3"})
	m, _ = updateModel(t, m, key("space"))
	m, _ = updateModel(t, m, key("q"))
	if m.UserQuitRequested() || m.confirm.kind != confirmEditorDirtyQuit {
		t.Fatalf("dirty Q quit=%t confirmation=%v", m.UserQuitRequested(), m.confirm.kind)
	}
	m, _ = updateModel(t, m, key("d"))
	if !m.UserQuitRequested() || m.editorDirty() {
		t.Fatalf("discard-and-quit quit=%t dirty=%t", m.UserQuitRequested(), m.editorDirty())
	}
}

func TestEditorStaleExitRescansAndRestoresSelectionFocusByPath(t *testing.T) {
	m, backend := readyEditorModel(t, "", []string{"1", "2", "3"})
	m.editor.returnState = StateSelection
	m.editor.outerStale = true
	m, discoveryCmd := updateModel(t, m, key("esc"))
	if m.State() != StateDiscovery || discoveryCmd == nil {
		t.Fatalf("stale exit state=%s cmd nil=%t", m.State(), discoveryCmd == nil)
	}
	other := File{ID: "0.srt", Path: "season/0.srt", Valid: true}
	backend.discoveries <- DiscoveryEvent{
		Discovery: &Discovery{Files: []File{other, matchingFile()}}, Completed: 2, Total: 2, Done: true,
	}
	m, _ = updateModel(t, m, discoveryCmd())
	if m.State() != StateSelection {
		t.Fatalf("post-rescan state=%s, want selection", m.State())
	}
	file, ok := m.focusedFile()
	if !ok || file.Path != "season/a.srt" {
		t.Fatalf("post-rescan focus = %#v, %t", file, ok)
	}
}

func TestCleanEditorExitAvoidsUnnecessaryRescan(t *testing.T) {
	m, _ := readyEditorModel(t, "", []string{"1", "2", "3"})
	m, cmd := updateModel(t, m, key("esc"))
	if cmd != nil || m.State() != StateResults {
		t.Fatalf("clean exit state=%s cmd nil=%t", m.State(), cmd == nil)
	}
}

func TestEditorRecoveryResolutionUsesExactRefreshAndResumes(t *testing.T) {
	m, editorBackend := readyEditorModel(t, "", []string{"1", "2", "3"})
	recoveryBackend := newFakeRecoveryBackend()
	m.recoveryBackend = recoveryBackend
	m.pendingRecovery = "pending-editor-save"
	m.summaryBack = StateEditor
	m.state = StateSummary
	m.summary = &MutationSummary{RecoveryID: "pending-editor-save", RecoveryKind: RecoveryGateApply}

	m, _ = updateModel(t, m, key("r"))
	m, recoveryCmd := updateModel(t, m, key("y"))
	recoveryBackend.recovery <- RecoveryEvent{Done: true, Summary: &RecoverySummary{Undo: &UndoSnapshot{Available: true}}}
	m, refreshCmd := updateModel(t, m, recoveryCmd())
	if m.pendingRecovery != "" || refreshCmd == nil || !m.editor.loading || m.refreshBehindSummary {
		t.Fatalf("resolved editor recovery: pending=%q refresh nil=%t loading=%t rootRefresh=%t", m.pendingRecovery, refreshCmd == nil, m.editor.loading, m.refreshBehindSummary)
	}
	if recoveryBackend.request.ID != "pending-editor-save" || recoveryBackend.request.Action != RecoveryRestore {
		t.Fatalf("recovery request = %#v", recoveryBackend.request)
	}

	editorBackend.refreshes <- EditorOpenEvent{Revision: 2, Document: ptrEditorDocument(editorDocument("snapshot-2")), Done: true}
	m, searchCmd := updateModel(t, m, refreshCmd())
	editorBackend.editorSearches <- EditorSearchEvent{
		Revision: 2, SnapshotID: "snapshot-2", Query: "", CueIDs: []string{"1", "2", "3"}, Done: true,
	}
	m, _ = updateModel(t, m, searchCmd())
	m, _ = updateModel(t, m, key("enter"))
	if m.State() != StateEditor || m.editor.document.SnapshotID != "snapshot-2" {
		t.Fatalf("recovery did not resume editor: state=%s snapshot=%q", m.State(), m.editor.document.SnapshotID)
	}
}

func TestEditorSummaryHonorsExitAndQuitAfterSave(t *testing.T) {
	m, _ := readyEditorModel(t, "", []string{"1", "2", "3"})
	m.state = StateSummary
	m.summaryBack = StateEditor
	m.summary = &MutationSummary{Succeeded: 1}
	m.editor.afterSave = editorAfterExit
	m.editor.outerStale = true
	m, cmd := updateModel(t, m, key("enter"))
	if m.State() != StateDiscovery || cmd == nil {
		t.Fatalf("save-and-exit state=%s cmd nil=%t", m.State(), cmd == nil)
	}

	m, _ = readyEditorModel(t, "", []string{"1", "2", "3"})
	m.state = StateSummary
	m.summaryBack = StateEditor
	m.summary = &MutationSummary{Succeeded: 1}
	m.editor.afterSave = editorAfterQuit
	m, _ = updateModel(t, m, key("enter"))
	if !m.UserQuitRequested() {
		t.Fatal("save-and-quit did not issue a clean explicit quit")
	}
}

func TestEditorLongCueRightClampAndLeftMovesImmediately(t *testing.T) {
	m, _ := readyEditorModel(t, "", []string{"1", "2", "3"})
	m.editor.document.Cues[0].Text = strings.Repeat("a long wrapped cue segment ", 80)
	for range 500 {
		m, _ = updateModel(t, m, key("right"))
	}
	last := m.editor.lineOffset
	m, _ = updateModel(t, m, key("right"))
	if m.editor.lineOffset != last {
		t.Fatalf("Right accumulated beyond EOF: before=%d after=%d", last, m.editor.lineOffset)
	}
	m, _ = updateModel(t, m, key("left"))
	if m.editor.lineOffset != max(0, last-1) {
		t.Fatalf("Left did not move immediately at EOF: before=%d after=%d", last, m.editor.lineOffset)
	}
}

func TestEditorHomeEndAndCtrlFFilterNavigation(t *testing.T) {
	m, _ := readyEditorModel(t, "", []string{"1", "2", "3"})
	m, _ = updateModel(t, m, key("end"))
	if cue, ok := m.editorFocusedCue(); !ok || cue.ID != "3" {
		t.Fatalf("End focus = %#v, %t", cue, ok)
	}
	m.editor.lineOffset = 7
	m, _ = updateModel(t, m, key("home"))
	if cue, ok := m.editorFocusedCue(); !ok || cue.ID != "1" || m.editor.lineOffset != 0 {
		t.Fatalf("Home focus = %#v, %t offset=%d", cue, ok, m.editor.lineOffset)
	}
	m, cmd := updateModel(t, m, key("ctrl+f"))
	if m.State() != StateEditorSearch || !m.editor.input.Focused() || cmd == nil {
		t.Fatalf("Ctrl+F state=%s focused=%t cmd nil=%t", m.State(), m.editor.input.Focused(), cmd == nil)
	}
}

func TestCancelledEditorOpenCannotReenterAfterExit(t *testing.T) {
	backend := newFakeEditorBackend()
	m := New(backend, Options{Width: 90, Height: 28, DisableColor: true})
	m = completeDiscovery(t, m, []File{matchingFile()})
	m.state = StateResults
	m.input.Blur()
	m, _ = updateModel(t, m, key("enter"))
	if m.State() != StateEditorLoading {
		t.Fatalf("open state = %s", m.State())
	}
	revision := m.editor.openRevision
	m, _ = updateModel(t, m, key("esc"))
	if m.State() != StateResults || m.editor.active {
		t.Fatalf("cancelled open state=%s active=%t", m.State(), m.editor.active)
	}
	late := editorOpenStreamMsg{
		event:    EditorOpenEvent{Revision: revision, Document: ptrEditorDocument(editorDocument("late")), Done: true},
		revision: revision, mode: editorLoadOpen, fileID: "a.srt", path: "season/a.srt", ok: true,
	}
	m, _ = updateModel(t, m, late)
	if m.State() != StateResults || m.editor.active || m.editor.document.SnapshotID != "" {
		t.Fatalf("late open reentered editor: state=%s active=%t document=%#v", m.State(), m.editor.active, m.editor.document)
	}
}

func TestDisplaySanitizerNeutralizesTerminalAndBidiControls(t *testing.T) {
	hostile := "safe\x1b]8;;https://evil.invalid\aCLICK\x1b]8;;\a\nnext\t\u202eevil\u2066name\u009b"
	got := sanitizeDisplay(hostile)
	for _, forbidden := range []string{"\x1b", "\a", "\n", "\t", "\u202e", "\u2066", "\u009b"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitized display retained %q in %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "safe") || !strings.Contains(got, "CLICK") || !strings.Contains(got, "name") {
		t.Fatalf("sanitizer removed ordinary Unicode/text: %q", got)
	}
}

func TestHostileBackendStringsCannotInjectTerminalControls(t *testing.T) {
	m, _ := readyModel(t)
	m.opts.Root = "root\x1b]0;pwned\a"
	m.discovery.Files = []File{{
		ID: "bad", Path: "a\n\u202eb.srt", Valid: false, Error: "oops\x1b[2J",
		Preview: []Cue{{Timestamp: "00:00\t", Text: "cue\x1b]8;;x\a link\nrow"}},
	}}
	m.status = "status\x1b[31m\u2066"
	m.err = context.Canceled
	m.rebuildCards()
	view := m.render()
	for _, forbidden := range []string{"\x1b]0;pwned", "\x1b[2J", "\x1b]8;;x", "\u202e", "\u2066", "a\nb.srt"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("render retained hostile sequence %q:\n%s", forbidden, view)
		}
	}
}

func TestInputRenderingSanitizesWithoutChangingQuery(t *testing.T) {
	m, _ := readyModel(t)
	query := "match\x1b]0;title\a\u202e"
	m.input.SetValue(query)
	stored := m.Query() // textinput itself may reject C0 controls on input.
	view := m.inputView()
	if m.Query() != stored {
		t.Fatalf("render sanitizer changed search semantics: %q", m.Query())
	}
	if strings.Contains(view, "\x1b]0;title") || strings.Contains(view, "\u202e") {
		t.Fatalf("input view retained terminal controls: %q", view)
	}
}

func TestCardHasFiveRowsAndFilenameBelow(t *testing.T) {
	m, _ := readyModel(t)
	file := matchingFile()
	file.Preview = append(file.Preview,
		Cue{Text: "three"}, Cue{Text: "four"}, Cue{Text: "five"}, Cue{Text: "six should be hidden"},
	)
	m.result = SearchResult{Revision: 1, Files: []File{file}}
	m.input.SetValue("match")
	m.displayedRevision = 1
	card := m.renderCard(0, file)
	if strings.Contains(card, "six should be hidden") {
		t.Fatalf("card rendered more than five rows:\n%s", card)
	}
	if strings.LastIndex(card, "season/a.srt") < strings.LastIndex(card, "five") {
		t.Fatalf("filename is not below preview rows:\n%s", card)
	}
}
