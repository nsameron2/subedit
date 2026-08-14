package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"subedit/internal/tui"
	"subedit/internal/workspace"
)

func receiveEditorOpen(t *testing.T, backend *appBackend, request tui.EditorOpenRequest) tui.EditorOpenEvent {
	t.Helper()
	var final tui.EditorOpenEvent
	for event := range backend.OpenEditor(context.Background(), request) {
		if event.Done {
			final = event
		}
	}
	return final
}

func receiveEditorRefresh(t *testing.T, backend *appBackend, request tui.EditorRefreshRequest) tui.EditorOpenEvent {
	t.Helper()
	var final tui.EditorOpenEvent
	for event := range backend.RefreshEditor(context.Background(), request) {
		if event.Done {
			final = event
		}
	}
	return final
}

func receiveEditorSearch(t *testing.T, backend *appBackend, request tui.EditorSearchRequest) tui.EditorSearchEvent {
	t.Helper()
	var final tui.EditorSearchEvent
	for event := range backend.SearchEditor(context.Background(), request) {
		if event.Done {
			final = event
		}
	}
	return final
}

func receiveMutation(t *testing.T, events <-chan tui.MutationEvent) tui.MutationEvent {
	t.Helper()
	var final tui.MutationEvent
	for event := range events {
		if event.Done {
			final = event
		}
	}
	return final
}

func openClipEditor(t *testing.T, backend *appBackend, revision uint64) *tui.EditorDocument {
	t.Helper()
	event := receiveEditorOpen(t, backend, tui.EditorOpenRequest{
		FileID: "clip.srt", Path: "clip.srt", Revision: revision,
	})
	if event.Err != nil || event.Document == nil || event.Revision != revision {
		t.Fatalf("open editor = %#v", event)
	}
	return event.Document
}

func editorDeleteRequest(document *tui.EditorDocument, ids ...string) tui.MutationRequest {
	return tui.MutationRequest{
		Source: tui.MutationSourceEditor, Scope: tui.DeleteEditor,
		SnapshotID: document.SnapshotID,
		Targets: []tui.MutationTarget{{
			FileID: document.FileID, Path: document.Path, CueIDs: append([]string(nil), ids...),
		}},
	}
}

func TestEditorOpenSearchMutateRefreshAndReplayProtection(t *testing.T) {
	backend, _, root := openBackend(t)
	receiveDiscovery(t, backend)
	document := openClipEditor(t, backend, 41)
	if document.SnapshotID == "" || len(document.Cues) != 2 {
		t.Fatalf("editor document = %#v", document)
	}

	blank := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: document.SnapshotID, Revision: 42,
	})
	if blank.Err != nil || blank.Revision != 42 || len(blank.CueIDs) != 2 ||
		blank.CueIDs[0] != document.Cues[0].ID || blank.CueIDs[1] != document.Cues[1].ID {
		t.Fatalf("blank editor search = %#v", blank)
	}
	filtered := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: document.SnapshotID, Query: "KEEP ME", Revision: 43,
	})
	if filtered.Err != nil || len(filtered.CueIDs) != 1 || filtered.CueIDs[0] != document.Cues[1].ID {
		t.Fatalf("filtered editor search = %#v", filtered)
	}

	request := editorDeleteRequest(document, document.Cues[1].ID)
	mutation := receiveMutation(t, backend.Mutate(context.Background(), request))
	if mutation.Err != nil || mutation.Summary == nil || mutation.Summary.Succeeded != 1 ||
		mutation.Summary.UndoID == "" || !mutation.Summary.UndoAvailable {
		t.Fatalf("editor mutation = %#v", mutation)
	}
	if stale := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: document.SnapshotID, Revision: 44,
	}); stale.Err == nil {
		t.Fatalf("consumed token remained searchable: %#v", stale)
	}
	if replay := receiveMutation(t, backend.Mutate(context.Background(), request)); replay.Err == nil {
		t.Fatalf("replayed mutation was accepted: %#v", replay)
	}

	refreshed := receiveEditorRefresh(t, backend, tui.EditorRefreshRequest{Path: "clip.srt", Revision: 45})
	if refreshed.Err != nil || refreshed.Document == nil || refreshed.Revision != 45 ||
		refreshed.Document.SnapshotID == document.SnapshotID || len(refreshed.Document.Cues) != 1 {
		t.Fatalf("editor refresh = %#v", refreshed)
	}
	backend.mu.RLock()
	haveOuterScan, outerSearches := backend.haveScan, len(backend.searches)
	backend.mu.RUnlock()
	if haveOuterScan || outerSearches != 0 {
		t.Fatalf("editor refresh left outer snapshot usable: scan=%v searches=%d", haveOuterScan, outerSearches)
	}
	raw, err := os.ReadFile(filepath.Join(root, "clip.srt"))
	if err != nil || string(raw) == backendFixture {
		t.Fatalf("editor mutation did not persist: %q, %v", raw, err)
	}
}

func TestEditorMutationRejectsForgedRequestsWithoutConsumingSnapshot(t *testing.T) {
	backend, _, _ := openBackend(t)
	receiveDiscovery(t, backend)
	document := openClipEditor(t, backend, 1)
	validID := document.Cues[0].ID

	cases := []struct {
		name    string
		request tui.MutationRequest
	}{
		{"forged token", func() tui.MutationRequest {
			r := editorDeleteRequest(document, validID)
			r.SnapshotID = "editor-forged"
			return r
		}()},
		{"wrong scope", func() tui.MutationRequest {
			r := editorDeleteRequest(document, validID)
			r.Scope = tui.DeleteFocused
			return r
		}()},
		{"root query", func() tui.MutationRequest { r := editorDeleteRequest(document, validID); r.Query = "text"; return r }()},
		{"no target", func() tui.MutationRequest { r := editorDeleteRequest(document, validID); r.Targets = nil; return r }()},
		{"two targets", func() tui.MutationRequest {
			r := editorDeleteRequest(document, validID)
			r.Targets = append(r.Targets, r.Targets[0])
			return r
		}()},
		{"wrong file ID", func() tui.MutationRequest {
			r := editorDeleteRequest(document, validID)
			r.Targets[0].FileID = "other.srt"
			return r
		}()},
		{"wrong path", func() tui.MutationRequest {
			r := editorDeleteRequest(document, validID)
			r.Targets[0].Path = "other.srt"
			return r
		}()},
		{"empty cues", editorDeleteRequest(document)},
		{"duplicate cue", editorDeleteRequest(document, validID, validID)},
		{"unknown cue", editorDeleteRequest(document, "srt:forged")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := backend.mutationPlan(test.request); err == nil {
				t.Fatalf("mutationPlan accepted %#v", test.request)
			}
			stillCurrent := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
				SnapshotID: document.SnapshotID, Revision: 2,
			})
			if stillCurrent.Err != nil || len(stillCurrent.CueIDs) != 2 {
				t.Fatalf("rejected request consumed snapshot: %#v", stillCurrent)
			}
		})
	}

	plan, err := backend.mutationPlan(editorDeleteRequest(document, validID))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Scope != workspace.DeleteEditor || len(plan.Files) != 1 ||
		plan.Files[0].RelativePath != "clip.srt" || len(plan.Files[0].CueIDs) != 1 || plan.SearchRevision == 0 {
		t.Fatalf("editor plan = %#v", plan)
	}
	if current := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: document.SnapshotID, Revision: 3,
	}); current.Err == nil {
		t.Fatal("accepted plan did not consume snapshot")
	}
}

func TestEditorOpenAndRefreshRequireExactCurrentPath(t *testing.T) {
	backend, _, root := openBackend(t)
	if event := receiveEditorOpen(t, backend, tui.EditorOpenRequest{
		FileID: "clip.srt", Path: "clip.srt", Revision: 1,
	}); event.Err == nil {
		t.Fatalf("open before discovery succeeded: %#v", event)
	}
	receiveDiscovery(t, backend)
	if event := receiveEditorOpen(t, backend, tui.EditorOpenRequest{
		FileID: "different.srt", Path: "clip.srt", Revision: 2,
	}); event.Err == nil {
		t.Fatalf("mismatched identity succeeded: %#v", event)
	}
	if event := receiveEditorOpen(t, backend, tui.EditorOpenRequest{
		FileID: "missing.srt", Path: "missing.srt", Revision: 3,
	}); event.Err == nil {
		t.Fatalf("undiscovered path succeeded: %#v", event)
	}
	document := openClipEditor(t, backend, 4)
	if event := receiveEditorRefresh(t, backend, tui.EditorRefreshRequest{
		Path: "other.srt", Revision: 5,
	}); event.Err == nil {
		t.Fatalf("wrong refresh path succeeded: %#v", event)
	}
	if current := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: document.SnapshotID, Revision: 6,
	}); current.Err != nil {
		t.Fatalf("wrong refresh consumed current token: %#v", current)
	}

	// An invalid supported sibling proves refresh indexes only the editor path.
	if err := os.WriteFile(filepath.Join(root, "broken.srt"), []byte("not subtitles"), 0o600); err != nil {
		t.Fatal(err)
	}
	refreshed := receiveEditorRefresh(t, backend, tui.EditorRefreshRequest{Path: "clip.srt", Revision: 7})
	if refreshed.Err != nil || refreshed.Document == nil {
		t.Fatalf("exact refresh was affected by sibling: %#v", refreshed)
	}
	if old := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: document.SnapshotID, Revision: 8,
	}); old.Err == nil {
		t.Fatal("refresh did not invalidate old token")
	}
}

func TestFullDiscoveryClearsEditorSnapshot(t *testing.T) {
	backend, _, _ := openBackend(t)
	receiveDiscovery(t, backend)
	document := openClipEditor(t, backend, 1)
	receiveDiscovery(t, backend)
	search := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: document.SnapshotID, Revision: 2,
	})
	if search.Err == nil {
		t.Fatalf("full discovery retained editor token: %#v", search)
	}
}

func TestEditorKeepsOnlyNewestSnapshotAndConflictsOnExternalChange(t *testing.T) {
	backend, _, root := openBackend(t)
	receiveDiscovery(t, backend)
	first := openClipEditor(t, backend, 1)
	second := openClipEditor(t, backend, 2)
	if first.SnapshotID == second.SnapshotID {
		t.Fatal("two editor opens reused a snapshot token")
	}
	if stale := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: first.SnapshotID, Revision: 3,
	}); stale.Err == nil {
		t.Fatal("opening a replacement snapshot retained the old token")
	}
	if err := os.WriteFile(filepath.Join(root, "clip.srt"), []byte(backendFixture+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutation := receiveMutation(t, backend.Mutate(context.Background(), editorDeleteRequest(second, second.Cues[0].ID)))
	if mutation.Err != nil || mutation.Summary == nil || mutation.Summary.Succeeded != 0 || mutation.Summary.Skipped != 1 {
		t.Fatalf("external-change mutation = %#v", mutation)
	}
	raw, err := os.ReadFile(filepath.Join(root, "clip.srt"))
	if err != nil || string(raw) != backendFixture+"\n" {
		t.Fatalf("editor overwrote external change: %q, %v", raw, err)
	}
}

func TestEditorUndoIsExpectedIDAndPathScoped(t *testing.T) {
	t.Run("exact one-file undo", func(t *testing.T) {
		backend, _, _ := openBackend(t)
		receiveDiscovery(t, backend)
		document := openClipEditor(t, backend, 1)
		mutation := receiveMutation(t, backend.Mutate(context.Background(), editorDeleteRequest(document, document.Cues[0].ID)))
		if mutation.Err != nil || mutation.Summary == nil || mutation.Summary.UndoID == "" {
			t.Fatalf("mutation = %#v", mutation)
		}
		refreshed := receiveEditorRefresh(t, backend, tui.EditorRefreshRequest{Path: "clip.srt", Revision: 2})
		if refreshed.Err != nil || refreshed.Document == nil {
			t.Fatalf("refresh = %#v", refreshed)
		}
		wrongPath := receiveMutation(t, backend.UndoEditor(context.Background(), tui.EditorUndoRequest{
			Path: "other.srt", UndoID: mutation.Summary.UndoID, Revision: 3,
		}))
		if wrongPath.Err == nil {
			t.Fatalf("wrong-path undo succeeded: %#v", wrongPath)
		}
		if current := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
			SnapshotID: refreshed.Document.SnapshotID, Revision: 4,
		}); current.Err != nil {
			t.Fatalf("rejected undo consumed snapshot: %#v", current)
		}

		undone := receiveMutation(t, backend.UndoEditor(context.Background(), tui.EditorUndoRequest{
			Path: "clip.srt", UndoID: mutation.Summary.UndoID, Revision: 5,
		}))
		if undone.Err != nil || undone.Summary == nil || undone.Summary.Succeeded != 1 || undone.Summary.UndoAvailable {
			t.Fatalf("editor undo = %#v", undone)
		}
		reloaded := receiveEditorRefresh(t, backend, tui.EditorRefreshRequest{Path: "clip.srt", Revision: 6})
		if reloaded.Err != nil || reloaded.Document == nil || len(reloaded.Document.Cues) != 2 {
			t.Fatalf("refresh after undo = %#v", reloaded)
		}
	})

	t.Run("multi-file undo is out of scope", func(t *testing.T) {
		backend, _, root := openBackend(t)
		if err := os.WriteFile(filepath.Join(root, "other.srt"), []byte(backendFixture), 0o600); err != nil {
			t.Fatal(err)
		}
		receiveDiscovery(t, backend)
		search := receiveSearch(t, backend, tui.SearchRequest{Query: "thank you", Revision: 10})
		if len(search.Result.Files) != 2 {
			t.Fatalf("global search = %#v", search.Result)
		}
		targets := make([]tui.MutationTarget, len(search.Result.Files))
		for index, file := range search.Result.Files {
			targets[index] = tui.MutationTarget{FileID: file.ID, Path: file.Path, CueIDs: file.MatchIDs}
		}
		global := receiveMutation(t, backend.Mutate(context.Background(), tui.MutationRequest{
			Scope: tui.DeleteAll, Query: "thank you", Revision: 10, Targets: targets,
		}))
		if global.Err != nil || global.Summary == nil || global.Summary.UndoID == "" {
			t.Fatalf("global mutation = %#v", global)
		}
		receiveDiscovery(t, backend)
		openClipEditor(t, backend, 11)
		outOfScope := receiveMutation(t, backend.UndoEditor(context.Background(), tui.EditorUndoRequest{
			Path: "clip.srt", UndoID: global.Summary.UndoID, Revision: 12,
		}))
		if !errors.Is(outOfScope.Err, workspace.ErrRecoveryScope) {
			t.Fatalf("multi-file editor undo = %#v, want ErrRecoveryScope", outOfScope)
		}
	})
}

func TestCancelledEditorStreamStillClosesWithFinalEvent(t *testing.T) {
	backend, _, _ := openBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var final *tui.EditorOpenEvent
	for event := range backend.OpenEditor(ctx, tui.EditorOpenRequest{
		FileID: "clip.srt", Path: "clip.srt", Revision: 91,
	}) {
		if event.Done {
			copy := event
			final = &copy
		}
	}
	if final == nil || final.Revision != 91 || !errors.Is(final.Err, context.Canceled) {
		t.Fatalf("cancelled editor open = %#v", final)
	}
}

func TestSupersededOrCancelledEditorOpenCannotOverwriteLatestSnapshot(t *testing.T) {
	backend, _, _ := openBackend(t)
	receiveDiscovery(t, backend)
	request := tui.EditorOpenRequest{FileID: "clip.srt", Path: "clip.srt"}

	// Claim two opens in request order, then deliberately publish them in the
	// opposite order. This is the scheduling that used to let a late response
	// replace the snapshot already returned for the user's newer selection.
	olderGeneration, err := backend.claimEditorOpen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newerGeneration, err := backend.claimEditorOpen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newer, err := backend.openEditorDocument(context.Background(), request, newerGeneration)
	if err != nil || newer == nil {
		t.Fatalf("publish newer editor open = %#v, %v", newer, err)
	}
	older, err := backend.openEditorDocument(context.Background(), request, olderGeneration)
	if err == nil || older != nil {
		t.Fatalf("superseded editor open published = %#v, %v", older, err)
	}
	if current := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: newer.SnapshotID, Revision: 3,
	}); current.Err != nil || len(current.CueIDs) != len(newer.Cues) {
		t.Fatalf("late open replaced latest snapshot: %#v", current)
	}

	// Cancellation is checked again under the final publication lock. An open
	// that passed the goroutine's initial check but was cancelled while waiting
	// to publish therefore cannot replace the same current snapshot.
	cancelledGeneration, err := backend.claimEditorOpen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled, err := backend.openEditorDocument(ctx, request, cancelledGeneration)
	if !errors.Is(err, context.Canceled) || cancelled != nil {
		t.Fatalf("cancelled editor open published = %#v, %v", cancelled, err)
	}
	if current := receiveEditorSearch(t, backend, tui.EditorSearchRequest{
		SnapshotID: newer.SnapshotID, Revision: 4,
	}); current.Err != nil || len(current.CueIDs) != len(newer.Cues) {
		t.Fatalf("cancelled open replaced latest snapshot: %#v", current)
	}

	// A command whose context is already cancelled must not even supersede a
	// live open that has claimed, but not yet reached, final publication.
	liveGeneration, err := backend.claimEditorOpen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staleContext, staleCancel := context.WithCancel(context.Background())
	staleCancel()
	if _, err := backend.claimEditorOpen(staleContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stale claim error = %v", err)
	}
	live, err := backend.openEditorDocument(context.Background(), request, liveGeneration)
	if err != nil || live == nil {
		t.Fatalf("cancelled stale claim superseded live open = %#v, %v", live, err)
	}
}
