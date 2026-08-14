package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"subedit/internal/tui"
	"subedit/internal/workspace"
)

const backendFixture = "1\n00:00:01,000 --> 00:00:02,000\nThank you for watching\n\n2\n00:00:03,000 --> 00:00:04,000\nKeep me\n"

func openBackend(t *testing.T) (*appBackend, *workspace.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "clip.srt"), []byte(backendFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	backend := newAppBackend(ws)
	t.Cleanup(func() {
		backend.Close()
		_ = ws.Close()
	})
	return backend, ws, root
}

func receiveDiscovery(t *testing.T, backend *appBackend) tui.DiscoveryEvent {
	t.Helper()
	var final tui.DiscoveryEvent
	for event := range backend.Discover(context.Background()) {
		if event.Done {
			final = event
		}
	}
	if final.Err != nil || final.Discovery == nil {
		t.Fatalf("discovery final = %#v", final)
	}
	return final
}

func receiveSearch(t *testing.T, backend *appBackend, request tui.SearchRequest) tui.SearchEvent {
	t.Helper()
	var final tui.SearchEvent
	for event := range backend.Search(context.Background(), request) {
		if event.Done {
			final = event
		}
	}
	if final.Err != nil {
		t.Fatal(final.Err)
	}
	return final
}

func TestBackendPinsMutationToReviewedSearch(t *testing.T) {
	backend, _, root := openBackend(t)
	receiveDiscovery(t, backend)
	search := receiveSearch(t, backend, tui.SearchRequest{Query: "thank you", Revision: 9})
	if len(search.Result.Files) != 1 || len(search.Result.Files[0].MatchIDs) != 1 {
		t.Fatalf("unexpected search result: %#v", search.Result)
	}
	target := tui.MutationTarget{
		FileID: search.Result.Files[0].ID,
		Path:   search.Result.Files[0].Path,
		CueIDs: append([]string(nil), search.Result.Files[0].MatchIDs...),
	}

	badQuery := tui.MutationRequest{Scope: tui.DeleteAll, Query: "different", Revision: 9, Targets: []tui.MutationTarget{target}}
	if _, err := backend.mutationPlan(badQuery); err == nil {
		t.Fatal("mutationPlan accepted a different query for the reviewed revision")
	}
	badCue := tui.MutationRequest{Scope: tui.DeleteAll, Query: "thank you", Revision: 9, Targets: []tui.MutationTarget{target}}
	badCue.Targets[0].CueIDs = []string{"srt:made-up"}
	if _, err := backend.mutationPlan(badCue); err == nil {
		t.Fatal("mutationPlan accepted a cue outside the reviewed result")
	}

	request := tui.MutationRequest{Scope: tui.DeleteAll, Query: "thank you", Revision: 9, Targets: []tui.MutationTarget{target}}
	var final tui.MutationEvent
	for event := range backend.Mutate(context.Background(), request) {
		if event.Done {
			final = event
		}
	}
	if final.Err != nil || final.Summary == nil || final.Summary.Succeeded != 1 || !final.Summary.UndoAvailable {
		t.Fatalf("mutation final = %#v", final)
	}
	raw, err := os.ReadFile(filepath.Join(root, "clip.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == backendFixture {
		t.Fatal("mutation did not change subtitle")
	}
}

func TestBackendCloseDoesNotBlockOnUnreadProgress(t *testing.T) {
	backend, _, _ := openBackend(t)
	receiveDiscovery(t, backend)
	search := receiveSearch(t, backend, tui.SearchRequest{Query: "thank you", Revision: 3})
	file := search.Result.Files[0]
	_ = backend.Mutate(context.Background(), tui.MutationRequest{
		Scope: tui.DeleteAll, Query: "thank you", Revision: 3,
		Targets: []tui.MutationTarget{{FileID: file.ID, Path: file.Path, CueIDs: file.MatchIDs}},
	})

	done := make(chan struct{})
	go func() {
		backend.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind an unread event stream")
	}
}

func TestCancelledMutationStillDeliversFinalSummary(t *testing.T) {
	backend, _, _ := openBackend(t)
	receiveDiscovery(t, backend)
	search := receiveSearch(t, backend, tui.SearchRequest{Query: "thank you", Revision: 12})
	file := search.Result.Files[0]
	ctx, cancel := context.WithCancel(context.Background())
	events := backend.Mutate(ctx, tui.MutationRequest{
		Scope: tui.DeleteAll, Query: "thank you", Revision: 12,
		Targets: []tui.MutationTarget{{FileID: file.ID, Path: file.Path, CueIDs: file.MatchIDs}},
	})
	cancel()

	var final *tui.MutationEvent
	for event := range events {
		if event.Done {
			copy := event
			final = &copy
		}
	}
	if final == nil || final.Summary == nil ||
		(!final.Summary.Cancelled && !errors.Is(final.Err, context.Canceled)) {
		t.Fatalf("cancelled operation did not deliver its final summary: %#v", final)
	}
}

func TestRecoverySummaryRetainsPartialRestore(t *testing.T) {
	backend, ws, root := openBackend(t)
	receiveDiscovery(t, backend)
	search := receiveSearch(t, backend, tui.SearchRequest{Query: "thank you", Revision: 4})
	file := search.Result.Files[0]
	var mutation tui.MutationEvent
	for event := range backend.Mutate(context.Background(), tui.MutationRequest{
		Scope: tui.DeleteAll, Query: "thank you", Revision: 4,
		Targets: []tui.MutationTarget{{FileID: file.ID, Path: file.Path, CueIDs: file.MatchIDs}},
	}) {
		if event.Done {
			mutation = event
		}
	}
	if mutation.Summary == nil || !mutation.Summary.UndoAvailable {
		t.Fatalf("mutation = %#v", mutation)
	}
	if err := os.WriteFile(filepath.Join(root, "clip.srt"), []byte("external change"), 0o600); err != nil {
		t.Fatal(err)
	}
	undoSummary, err := ws.Undo(context.Background(), nil)
	if err != nil || undoSummary.Conflicted != 1 || undoSummary.TransactionID == "" {
		t.Fatalf("prepare partial undo: summary=%#v err=%v", undoSummary, err)
	}

	var final tui.RecoveryEvent
	for event := range backend.ResolveRecovery(context.Background(), tui.RecoveryRequest{
		ID: undoSummary.TransactionID, Action: tui.RecoveryRestore,
	}) {
		if event.Done {
			final = event
		}
	}
	if final.Err != nil || final.Summary == nil || !final.Summary.Retained || final.Summary.Skipped != 1 {
		t.Fatalf("recovery final = %#v", final)
	}
	if final.Summary.Undo == nil || !final.Summary.Undo.Available ||
		final.Summary.Undo.RetainedUndoID != undoSummary.TransactionID {
		t.Fatalf("partial recovery undo snapshot = %#v", final.Summary.Undo)
	}
}

func TestRecoveryDiscardPublishesAuthoritativeUndoRetirement(t *testing.T) {
	backend, ws, _ := openBackend(t)
	receiveDiscovery(t, backend)
	search := receiveSearch(t, backend, tui.SearchRequest{Query: "thank you", Revision: 17})
	file := search.Result.Files[0]
	var mutation tui.MutationEvent
	for event := range backend.Mutate(context.Background(), tui.MutationRequest{
		Scope: tui.DeleteAll, Query: "thank you", Revision: 17,
		Targets: []tui.MutationTarget{{FileID: file.ID, Path: file.Path, CueIDs: file.MatchIDs}},
	}) {
		if event.Done {
			mutation = event
		}
	}
	if mutation.Summary == nil || !mutation.Summary.UndoAvailable {
		t.Fatalf("mutation = %#v", mutation)
	}
	state, err := ws.State(context.Background())
	if err != nil || state.Undo == nil {
		t.Fatalf("state after mutation = %#v, %v", state, err)
	}

	var final tui.RecoveryEvent
	for event := range backend.ResolveRecovery(context.Background(), tui.RecoveryRequest{
		ID: state.Undo.ID, Action: tui.RecoveryDiscard,
	}) {
		if event.Done {
			final = event
		}
	}
	if final.Err != nil || final.Summary == nil || final.Summary.Undo == nil {
		t.Fatalf("discard final = %#v", final)
	}
	if final.Summary.Undo.Available || final.Summary.Undo.RetainedUndoID != "" {
		t.Fatalf("discard undo snapshot = %#v, want unavailable", final.Summary.Undo)
	}
}

func TestMutationSummaryMapsOnlyExplicitPendingApplyGate(t *testing.T) {
	pending := mutationSummary("delete", workspace.MutationSummary{
		TransactionID: "tx-pending", BlockingRecoveryID: "tx-pending",
		Succeeded: 1, Failed: 1, UndoAvailable: true, RecoveryRetained: true,
	})
	if pending.RecoveryID != "tx-pending" || pending.RecoveryKind != tui.RecoveryGateApply {
		t.Fatalf("pending mapping = %#v", pending)
	}

	// RecoveryRetained can describe cleanup garbage unrelated to this apply;
	// it must never manufacture a blocking TUI gate without the explicit ID.
	garbageOnly := mutationSummary("delete", workspace.MutationSummary{
		TransactionID: "tx-complete", UndoAvailable: true, RecoveryRetained: true,
	})
	if garbageOnly.RecoveryID != "" || garbageOnly.RecoveryKind != "" {
		t.Fatalf("garbage-only retention became a gate: %#v", garbageOnly)
	}

	partialUndo := mutationSummary("undo", workspace.MutationSummary{
		TransactionID: "tx-undo", UndoID: "tx-undo", UndoAvailable: true, RecoveryRetained: true,
	})
	if partialUndo.RecoveryID != "tx-undo" || partialUndo.RecoveryKind != tui.RecoveryGateUndo {
		t.Fatalf("partial undo mapping = %#v", partialUndo)
	}
}

func TestTUIOptionsExposeBlockingInvalidCurrentUndo(t *testing.T) {
	state := workspace.RecoveryState{Items: []workspace.Recovery{
		{ID: "current-invalid", Role: workspace.RecoveryRoleUndo, BlocksMutation: true},
		{ID: "corrupt-separate", Role: workspace.RecoveryRoleCorrupt, BlocksMutation: true},
	}}
	options := tuiOptionsFromRecoveryState("/workspace", state)
	if options.InitialRetainedUndoID != "current-invalid" || !options.InitialUndoAvailable {
		t.Fatalf("options = %#v", options)
	}

	// A corrupt-current entry is handled by the normal recovery prompt and
	// must not be double-seeded as a current partial undo.
	corruptOnly := tuiOptionsFromRecoveryState("/workspace", workspace.RecoveryState{Items: []workspace.Recovery{
		{ID: "corrupt-current", Role: workspace.RecoveryRoleCorrupt, BlocksMutation: true},
	}})
	if corruptOnly.InitialRetainedUndoID != "" || corruptOnly.InitialUndoAvailable {
		t.Fatalf("corrupt recovery was double-seeded as undo: %#v", corruptOnly)
	}
}

func TestUserQuitRequestedTypeCheck(t *testing.T) {
	if userQuitRequested(nil) {
		t.Fatal("nil model considered a user quit")
	}
	model := tui.New(nil, tui.Options{})
	if userQuitRequested(model) || userQuitRequested(&model) {
		t.Fatal("fresh model considered a user quit")
	}
	updated, cmd := model.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil || !userQuitRequested(updated) {
		t.Fatal("explicit q was not recognized as a clean user quit")
	}
}
