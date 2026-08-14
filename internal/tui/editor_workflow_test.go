package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func completeEditorFilter(t *testing.T, m Model, backend *fakeEditorBackend, cmd tea.Cmd, query string, ids ...string) Model {
	t.Helper()
	backend.editorSearches <- EditorSearchEvent{
		Revision: m.editor.searchRevision, SnapshotID: m.editor.document.SnapshotID,
		Query: query, CueIDs: append([]string(nil), ids...), Done: true,
	}
	message := cmd()
	next, _ := updateModel(t, m, message)
	return next
}

func completeEditorRefresh(t *testing.T, m Model, backend *fakeEditorBackend, refreshCmd tea.Cmd, document EditorDocument) Model {
	t.Helper()
	backend.refreshes <- EditorOpenEvent{
		Revision: m.editor.openRevision, Document: ptrEditorDocument(document), Done: true,
	}
	message := refreshCmd()
	next, searchCmd := updateModel(t, m, message)
	if searchCmd == nil {
		t.Fatal("editor refresh did not start local filter")
	}
	ids := make([]string, len(document.Cues))
	for index := range document.Cues {
		ids[index] = document.Cues[index].ID
	}
	backend.editorSearches <- EditorSearchEvent{
		Revision: next.editor.searchRevision, SnapshotID: document.SnapshotID,
		Query: next.editor.input.Value(), CueIDs: ids, Done: true,
	}
	next, _ = updateModel(t, next, searchCmd())
	return next
}

func TestImmersiveEditorMarksSurviveLocalFilterAndSaveOnePinnedBatch(t *testing.T) {
	m, backend := readyEditorModel(t, "garbage", []string{"2"})

	m, _ = updateModel(t, m, key("space"))
	if !m.editor.marks["2"] || m.editorMarkedCount() != 1 {
		t.Fatalf("focused cue was not marked: %#v", m.editor.marks)
	}

	// Resetting the editor-only filter exposes every cue without changing the
	// outer query or losing a hidden/visible deletion mark.
	m, filterCmd := updateModel(t, m, key("c"))
	if filterCmd == nil || m.input.Value() != "garbage" {
		t.Fatalf("local reset changed outer query or skipped search: outer=%q cmd nil=%t", m.input.Value(), filterCmd == nil)
	}
	backend.editorSearches <- EditorSearchEvent{
		Revision: m.editor.searchRevision, SnapshotID: m.editor.document.SnapshotID,
		Query: "", CueIDs: []string{"1", "2", "3"}, Done: true,
	}
	m, _ = updateModel(t, m, filterCmd())
	if len(m.editor.visible) != 3 || !m.editor.marks["2"] {
		t.Fatalf("reset filter lost cues/marks: visible=%v marks=%v", m.editor.visible, m.editor.marks)
	}

	// Add a second noncontiguous cue and confirm that Save snapshots one exact
	// target containing both IDs in source order.
	m.moveEditorFocus(1) // anchor remaps to cue 2; move to cue 3
	m, _ = updateModel(t, m, key("space"))
	if !m.editor.marks["3"] || m.editorMarkedCount() != 2 {
		t.Fatalf("second cue was not staged: %#v", m.editor.marks)
	}
	m, _ = updateModel(t, m, key("s"))
	if m.State() != StateConfirmation || m.confirm.kind != confirmEditorSave {
		t.Fatalf("save state=%s confirmation=%#v", m.State(), m.confirm)
	}
	request := m.confirm.request
	if request.Source != MutationSourceEditor || request.Scope != DeleteEditor || request.SnapshotID != "snapshot-1" || len(request.Targets) != 1 {
		t.Fatalf("editor mutation request = %#v", request)
	}
	if got := strings.Join(request.Targets[0].CueIDs, ","); got != "2,3" {
		t.Fatalf("staged cue IDs = %q, want 2,3", got)
	}
}

func TestImmersiveEditorSaveRefreshAndSummaryUndoStayScoped(t *testing.T) {
	m, backend := readyEditorModel(t, "", []string{"1", "2", "3"})
	m, _ = updateModel(t, m, key("space"))
	m, _ = updateModel(t, m, key("s"))
	m, mutationCmd := updateModel(t, m, key("y"))
	if mutationCmd == nil || m.State() != StateMutation {
		t.Fatalf("confirmed save state=%s cmd nil=%t", m.State(), mutationCmd == nil)
	}
	backend.mutations <- MutationEvent{Done: true, Summary: &MutationSummary{
		Operation: "delete", Succeeded: 1, UndoAvailable: true, UndoID: "tx-editor",
	}}
	m, refreshCmd := updateModel(t, m, mutationCmd())
	if refreshCmd == nil || m.State() != StateSummary || !m.editor.stale {
		t.Fatalf("save completion state=%s stale=%t refresh nil=%t", m.State(), m.editor.stale, refreshCmd == nil)
	}
	if len(backend.requests) != 1 || backend.requests[0].SnapshotID != "snapshot-1" {
		t.Fatalf("backend mutation requests = %#v", backend.requests)
	}

	refreshed := EditorDocument{
		FileID: "a.srt", Path: "season/a.srt", SnapshotID: "snapshot-2",
		Cues: []Cue{{ID: "2", Timestamp: "00:00:02.000", Text: "garbage phrase"}, {ID: "3", Timestamp: "00:00:03.000", Text: "closing"}},
	}
	m = completeEditorRefresh(t, m, backend, refreshCmd, refreshed)
	if m.State() != StateSummary || m.editor.loading || m.editorDirty() || m.editor.undoID != "tx-editor" {
		t.Fatalf("post-refresh state=%s loading=%t marks=%d undo=%q", m.State(), m.editor.loading, m.editorMarkedCount(), m.editor.undoID)
	}

	// U from the editor summary must never call broad Backend.Undo. It carries
	// the exact editor transaction and path to EditorBackend.UndoEditor.
	m, _ = updateModel(t, m, key("u"))
	if m.State() != StateConfirmation || m.confirm.kind != confirmEditorUndo {
		t.Fatalf("summary U state=%s confirmation=%#v", m.State(), m.confirm)
	}
	m, undoCmd := updateModel(t, m, key("y"))
	backend.editorUndos <- MutationEvent{Done: true, Summary: &MutationSummary{Operation: "undo", Succeeded: 1}}
	m, _ = updateModel(t, m, undoCmd())
	if len(backend.undoRequests) != 1 || backend.undoRequests[0].UndoID != "tx-editor" || backend.undoRequests[0].Path != "season/a.srt" {
		t.Fatalf("scoped undo requests = %#v", backend.undoRequests)
	}
	if len(backend.undos) != 0 {
		t.Fatal("editor summary U unexpectedly used broad undo stream")
	}
}

func TestImmersiveEditorConflictClearsMarksBeforeFreshReview(t *testing.T) {
	m, backend := readyEditorModel(t, "", []string{"1", "2", "3"})
	m, _ = updateModel(t, m, key("space"))
	m, _ = updateModel(t, m, key("s"))
	m, mutationCmd := updateModel(t, m, key("y"))
	backend.mutations <- MutationEvent{Done: true, Err: errors.New("file changed"), Summary: &MutationSummary{
		Operation: "delete", Skipped: 1, UndoAvailable: true,
	}}
	m, refreshCmd := updateModel(t, m, mutationCmd())
	if refreshCmd == nil {
		t.Fatal("conflicted editor snapshot was not refreshed")
	}
	// Use the same cue IDs deliberately: carrying a mark through this refresh
	// would be an unsafe implicit rebase even when an ID happens to compare equal.
	m = completeEditorRefresh(t, m, backend, refreshCmd, editorDocument("snapshot-conflict"))
	if m.editorDirty() || len(m.editor.marks) != 0 {
		t.Fatalf("conflict automatically rebased marks: %#v", m.editor.marks)
	}
}

func TestImmersiveEditorDirtyExitAndRecoveryGate(t *testing.T) {
	m, _ := readyEditorModel(t, "garbage", []string{"2"})
	m.undoAvailable = true
	m, _ = updateModel(t, m, key("space"))
	m, _ = updateModel(t, m, key("esc"))
	if m.State() != StateConfirmation || m.confirm.kind != confirmEditorDirtyExit {
		t.Fatalf("dirty escape state=%s confirmation=%#v", m.State(), m.confirm)
	}
	if !strings.Contains(m.confirm.question, "season/a.srt") || !strings.Contains(m.confirm.question, "one-level undo") {
		t.Fatalf("dirty save prompt lacks safety disclosure: %q", m.confirm.question)
	}
	m, _ = updateModel(t, m, key("c"))
	if m.State() != StateEditor || !m.editorDirty() {
		t.Fatalf("dirty cancel state=%s dirty=%t", m.State(), m.editorDirty())
	}

	m.pendingRecovery = "tx-pending"
	m, _ = updateModel(t, m, key("d"))
	if !m.editor.marks["2"] || !strings.Contains(m.status, "Resolve") {
		t.Fatalf("recovery gate changed marks or omitted status: marks=%v status=%q", m.editor.marks, m.status)
	}
	m, _ = updateModel(t, m, key("n"))
	if m.editorDirty() {
		t.Fatal("recovery gate prevented clearing harmless local marks")
	}
}
