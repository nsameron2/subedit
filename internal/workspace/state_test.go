package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDiscoverFileIndexesOnlyExactTarget(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "target.srt", sampleSRT)
	writeTestFile(t, root, "sibling.srt", sampleSRT)
	w := openTestWorkspace(t, root)

	var events []DiscoveryProgress
	discovery, err := w.DiscoverFile(context.Background(), "target.srt", func(event DiscoveryProgress) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Files) != 1 || discovery.Files[0].RelativePath != "target.srt" || len(discovery.Issues) != 0 {
		t.Fatalf("exact discovery = %#v", discovery)
	}
	if len(events) != 1 || events[0].Completed != 1 || events[0].Total != 1 || events[0].CurrentPath != "target.srt" || events[0].Issue != nil {
		t.Fatalf("progress = %#v", events)
	}

	missing, err := w.DiscoverFile(context.Background(), "missing.srt", nil)
	if err != nil || len(missing.Files) != 0 || len(missing.Issues) != 1 || missing.Issues[0].Kind != IssueUnreadable {
		t.Fatalf("missing exact discovery = %#v, %v", missing, err)
	}
	if _, err := w.DiscoverFile(context.Background(), "sibling.txt", nil); !errors.Is(err, ErrUnsupportedFile) {
		t.Fatalf("unsupported error = %v", err)
	}
	if _, err := w.DiscoverFile(context.Background(), "../target.srt", nil); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("unsafe path error = %v", err)
	}
}

func TestSearchAnyUsesORSemanticsAndReportsQueryMatches(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := discovery.SearchAny([]string{"thank you", "KEEP ME", "  THANK   YOU  ", "   "})
	if !reflect.DeepEqual(result.Queries, []string{"thank you", "KEEP ME"}) || len(result.NormalizedQueries) != 2 {
		t.Fatalf("effective queries = %#v / %#v", result.Queries, result.NormalizedQueries)
	}
	if result.Query != result.Queries[0] || result.NormalizedQuery != result.NormalizedQueries[0] {
		t.Fatalf("single-query compatibility fields = %#v", result)
	}
	if result.MatchingCues != 2 || len(result.Matches) != 1 || len(result.Matches[0].Cues) != 2 || len(result.Matches[0].CueMatches) != 2 {
		t.Fatalf("OR result = %#v", result)
	}
	if !reflect.DeepEqual(result.Matches[0].CueMatches[0].QueryIndexes, []int{0}) || !reflect.DeepEqual(result.Matches[0].CueMatches[1].QueryIndexes, []int{1}) {
		t.Fatalf("cue query metadata = %#v", result.Matches[0].CueMatches)
	}
	plan := result.Plan(DeleteAll, nil)
	if len(plan.Files) != 1 || len(plan.Files[0].CueIDs) != 2 {
		t.Fatalf("OR deletion plan = %#v", plan)
	}
	empty := discovery.SearchAny([]string{"", " \t "})
	if len(empty.Queries) != 0 || empty.MatchingCues != 0 {
		t.Fatalf("empty phrases should be ignored: %#v", empty)
	}
}

func TestUndoPersistsAcrossCleanupCloseAndReopen(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := w.Apply(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if mutation.UndoID != mutation.TransactionID || !mutation.UndoAvailable || mutation.RecoveryRetained {
		t.Fatalf("mutation summary = %#v", mutation)
	}
	if err := w.CleanupSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestWorkspace(t, root)
	info, err := reopened.UndoInfo(context.Background())
	if err != nil || info == nil || info.ID != mutation.TransactionID || info.Partial || info.BlocksRemove {
		t.Fatalf("persistent undo = %#v, %v", info, err)
	}
	if recoveries, err := reopened.Recoveries(context.Background()); err != nil || len(recoveries) != 0 {
		t.Fatalf("live undo appeared as crash recovery: %#v, %v", recoveries, err)
	}
	undo, err := reopened.Undo(context.Background(), nil)
	if err != nil || undo.Restored != 1 || undo.UndoAvailable || undo.UndoID != "" {
		t.Fatalf("undo = %#v, %v", undo, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "episode.srt"))
	if err != nil || string(raw) != sampleSRT {
		t.Fatalf("restored bytes = %q, %v", raw, err)
	}
	stateInfo, err := os.Stat(filepath.Join(root, filepath.FromSlash(recoveryStatePath)))
	if err != nil || stateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("state permissions = %v, %v", stateInfo, err)
	}
}

func TestPersistentUndoSupersedesAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	firstWorkspace, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	firstDiscovery, _ := firstWorkspace.Discover(context.Background())
	first, err := firstWorkspace.Apply(context.Background(), firstDiscovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstWorkspace.Close(); err != nil {
		t.Fatal(err)
	}

	secondWorkspace := openTestWorkspace(t, root)
	secondDiscovery, _ := secondWorkspace.Discover(context.Background())
	second, err := secondWorkspace.Apply(context.Background(), secondDiscovery.Search("keep me").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.UndoID != second.TransactionID || second.TransactionID == first.TransactionID {
		t.Fatalf("second summary = %#v", second)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, first.TransactionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded undo still exists: %v", err)
	}
	undo, err := secondWorkspace.Undo(context.Background(), nil)
	if err != nil || undo.Restored != 1 {
		t.Fatalf("second undo = %#v, %v", undo, err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "episode.srt"))
	want := "1\n00:00:03,000 --> 00:00:04,000\nKeep me\n"
	if string(raw) != want {
		t.Fatalf("undo restored beyond one level: %q", raw)
	}
}

func TestPendingCrashRestorePreservesPreviousUndo(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	firstDiscovery, _ := w.Discover(context.Background())
	first, err := w.Apply(context.Background(), firstDiscovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	secondDiscovery, _ := w.Discover(context.Background())
	tx, err := w.Begin(context.Background(), secondDiscovery.Search("keep me").Plan(DeleteAll, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := tx.ApplyOne(context.Background()); err != nil || result.Status != FileSucceeded {
		t.Fatalf("apply pending file = %#v, %v", result, err)
	}
	// Simulate a process exit after one atomic commit but before Finish.
	w.endOperation()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestWorkspace(t, root)
	state, err := reopened.State(context.Background())
	if err != nil || state.Undo == nil || state.Undo.ID != first.TransactionID || !state.Blocked {
		t.Fatalf("crash state = %#v, %v", state, err)
	}
	recoveries, err := reopened.Recoveries(context.Background())
	if err != nil || len(recoveries) != 1 || recoveries[0].ID != tx.manifest.ID || recoveries[0].Role != RecoveryRolePending || !recoveries[0].BlocksMutation {
		t.Fatalf("pending recovery = %#v, %v", recoveries, err)
	}
	if _, err := reopened.Begin(context.Background(), MutationPlan{}); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("mutation with pending recovery error = %v", err)
	}
	restored, err := reopened.RestoreRecovery(context.Background(), tx.manifest.ID, nil)
	if err != nil || restored.Restored != 1 || restored.UndoID != first.TransactionID || !restored.UndoAvailable {
		t.Fatalf("restore pending = %#v, %v", restored, err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "episode.srt"))
	postFirst := "1\n00:00:03,000 --> 00:00:04,000\nKeep me\n"
	if string(raw) != postFirst {
		t.Fatalf("pending restore bytes = %q", raw)
	}
	if undo, err := reopened.Undo(context.Background(), nil); err != nil || undo.Restored != 1 {
		t.Fatalf("preserved prior undo = %#v, %v", undo, err)
	}
	raw, _ = os.ReadFile(filepath.Join(root, "episode.srt"))
	if string(raw) != sampleSRT {
		t.Fatalf("prior undo bytes = %q", raw)
	}
}

func TestCompletedPendingTransactionIsPromotedOnReopen(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	discovery, _ := w.Discover(context.Background())
	tx, err := w.Begin(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ApplyOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Finish's manifest write became durable, then the process died before the
	// pointer promotion. Reconciliation can prove and finish that transition.
	tx.manifest.Status = txComplete
	if err := w.saveManifest(tx.manifest); err != nil {
		t.Fatal(err)
	}
	w.endOperation()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestWorkspace(t, root)
	state, err := reopened.State(context.Background())
	if err != nil || state.Undo == nil || state.Undo.ID != tx.manifest.ID || state.Blocked {
		t.Fatalf("reconciled state = %#v, %v", state, err)
	}
	if undo, err := reopened.Undo(context.Background(), nil); err != nil || undo.Restored != 1 {
		t.Fatalf("reconciled undo = %#v, %v", undo, err)
	}
}

func TestPendingActiveNoWriteIsAutoRetired(t *testing.T) {
	root := t.TempDir()
	w := openTestWorkspace(t, root)
	manifest := &transactionManifest{
		Version: manifestVersion, ID: "tx-no-write-test", SessionID: w.session,
		CreatedAt: time.Now().UTC(), Status: txActive,
		Files: []manifestFile{{RelativePath: "episode.srt", State: statePlanned}},
	}
	if err := w.saveManifest(manifest); err != nil {
		t.Fatal(err)
	}
	if err := w.saveRecoveryState(&recoveryStateFile{Version: recoveryStateVersion, Pending: manifest.ID}); err != nil {
		t.Fatal(err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo != nil || state.Blocked || len(state.Items) != 0 {
		t.Fatalf("reconciled no-write state = %#v, %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, manifest.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-write pending transaction still exists: %v", err)
	}
}

func TestLegacyMigrationAdoptsNewestCompleteAndRetainsPartial(t *testing.T) {
	root := t.TempDir()
	w := openTestWorkspace(t, root)
	older := &transactionManifest{
		Version: manifestVersion, ID: "tx-legacy-older", SessionID: "legacy-session",
		CreatedAt: time.Now().Add(-time.Hour).UTC(), Status: txComplete,
	}
	newer := &transactionManifest{
		Version: manifestVersion, ID: "tx-legacy-newer", SessionID: "legacy-session",
		CreatedAt: time.Now().UTC(), Status: txComplete,
	}
	older.Files = []manifestFile{{RelativePath: "older.srt", BackupPath: RecoveryDir + "/tx-legacy-older/backup/000000.bin", State: stateCommitted}}
	newer.Files = []manifestFile{{RelativePath: "newer.srt", BackupPath: RecoveryDir + "/tx-legacy-newer/backup/000000.bin", State: stateCommitted}}
	partial := &transactionManifest{
		Version: manifestVersion, ID: "tx-legacy-partial", SessionID: "legacy-session",
		CreatedAt: time.Now().Add(time.Minute).UTC(), Status: txUndoPartial,
		Files: []manifestFile{{RelativePath: "episode.srt", State: stateConflict}},
	}
	for _, manifest := range []*transactionManifest{older, newer, partial} {
		if err := w.saveManifest(manifest); err != nil {
			t.Fatal(err)
		}
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo == nil || state.Undo.ID != newer.ID || !state.Blocked {
		t.Fatalf("legacy state = %#v, %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, older.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("older legacy complete was not retired: %v", err)
	}
	recoveries, err := w.Recoveries(context.Background())
	if err != nil || len(recoveries) != 1 || recoveries[0].ID != partial.ID || recoveries[0].Role != RecoveryRoleOrphan {
		t.Fatalf("legacy recovery list = %#v, %v", recoveries, err)
	}
}

func TestCorruptShortStateIDReturnsErrorWithoutPanic(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, RecoveryDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(recoveryStatePath)), []byte("{\"version\":1,\"generation\":1,\"current\":\"x\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := openTestWorkspace(t, root)
	if _, err := w.State(context.Background()); !errors.Is(err, ErrRecoveryState) {
		t.Fatalf("state error = %v, want ErrRecoveryState", err)
	}
}

func TestUndoScopedRequiresEveryTransactionTarget(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	discovery, _ := w.Discover(context.Background())
	mutation, err := w.Apply(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestWorkspace(t, root)
	if _, err := reopened.UndoScoped(context.Background(), mutation.TransactionID, []string{"a.srt"}, nil); !errors.Is(err, ErrRecoveryScope) {
		t.Fatalf("narrow undo error = %v", err)
	}
	if _, err := reopened.UndoScoped(context.Background(), "tx-stale-undo", []string{"a.srt", "b.srt"}, nil); !errors.Is(err, ErrUndoChanged) {
		t.Fatalf("stale undo error = %v", err)
	}
	undo, err := reopened.UndoScoped(context.Background(), mutation.TransactionID, []string{"a.srt", "b.srt"}, nil)
	if err != nil || undo.Restored != 2 {
		t.Fatalf("scoped undo = %#v, %v", undo, err)
	}
}

func TestPendingDiscardScopeIncludesPlannedTargetsAndClearsOldUndo(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	firstDiscovery, _ := w.DiscoverFile(context.Background(), "a.srt", nil)
	first, err := w.Apply(context.Background(), firstDiscovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	discovery, _ := w.Discover(context.Background())
	tx, err := w.Begin(context.Background(), discovery.Search("keep me").Plan(DeleteAll, nil))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := tx.ApplyOne(context.Background()); err != nil || result.Status != FileSucceeded {
		t.Fatalf("pending apply = %#v, %v", result, err)
	}
	w.endOperation()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestWorkspace(t, root)
	state, err := reopened.State(context.Background())
	if err != nil || state.Undo == nil || state.Undo.ID != first.TransactionID {
		t.Fatalf("pre-discard state = %#v, %v", state, err)
	}
	recoveries, err := reopened.Recoveries(context.Background())
	if err != nil || len(recoveries) != 1 || !reflect.DeepEqual(recoveries[0].FilesList, []string{"a.srt", "b.srt"}) {
		t.Fatalf("pending target list = %#v, %v", recoveries, err)
	}
	if err := reopened.DiscardRecoveryScoped(context.Background(), tx.manifest.ID, []string{"a.srt"}); !errors.Is(err, ErrRecoveryScope) {
		t.Fatalf("narrow discard error = %v", err)
	}
	if err := reopened.DiscardRecovery(context.Background(), tx.manifest.ID); err != nil {
		t.Fatal(err)
	}
	state, err = reopened.State(context.Background())
	if err != nil || state.Undo != nil || state.Blocked || len(state.Items) != 0 {
		t.Fatalf("post-discard state = %#v, %v", state, err)
	}
	if _, err := reopened.Undo(context.Background(), nil); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("discarded prior undo error = %v", err)
	}
}

func TestCancellationDuringPersistentUndoRetainsCurrentPointer(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, _ := w.Discover(context.Background())
	mutation, err := w.Apply(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	undo, err := w.Undo(ctx, func(progress Progress) {
		if progress.Completed == 1 {
			cancel()
		}
	})
	if err != nil || !undo.Cancelled || undo.Restored != 1 || !undo.UndoAvailable || undo.UndoID != mutation.TransactionID || !undo.RecoveryRetained {
		t.Fatalf("cancelled undo = %#v, %v", undo, err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo == nil || state.Undo.ID != mutation.TransactionID || !state.Undo.Partial || !state.Blocked {
		t.Fatalf("cancelled undo state = %#v, %v", state, err)
	}
	retry, err := w.Undo(context.Background(), nil)
	if err != nil || retry.Restored != 2 || retry.UndoAvailable {
		t.Fatalf("undo retry = %#v, %v", retry, err)
	}
}

func TestRestoreRecoveryCancellationRetainsPending(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	discovery, _ := w.Discover(context.Background())
	tx, err := w.Begin(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := tx.ApplyOne(context.Background()); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	}
	w.endOperation()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestWorkspace(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	summary, err := reopened.RestoreRecovery(ctx, tx.manifest.ID, func(progress Progress) {
		if progress.Completed == 1 {
			cancel()
		}
	})
	if err != nil || !summary.Cancelled || summary.Restored != 1 || !summary.RecoveryRetained {
		t.Fatalf("cancelled recovery = %#v, %v", summary, err)
	}
	recoveries, err := reopened.Recoveries(context.Background())
	if err != nil || len(recoveries) != 1 || recoveries[0].Role != RecoveryRolePending || recoveries[0].Status != txRecoveryPartial {
		t.Fatalf("retained pending = %#v, %v", recoveries, err)
	}
	retry, err := reopened.RestoreRecovery(context.Background(), tx.manifest.ID, nil)
	if err != nil || retry.Restored != 2 {
		t.Fatalf("recovery retry = %#v, %v", retry, err)
	}
}

func TestFinishRetainsExplicitPreparedAmbiguityAsPending(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	replaceCalls := 0
	replaceFailure := errors.New("injected replace failure")
	w.replaceForTest = func(root *os.Root, tempRel, targetRel, tempPath, targetPath string) error {
		replaceCalls++
		if replaceCalls == 2 {
			return replaceFailure
		}
		return replaceFile(root, tempRel, targetRel, tempPath, targetPath)
	}
	discovery, _ := w.Discover(context.Background())
	summary, err := w.Apply(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if !errors.Is(err, ErrRecoveryPending) || summary.Succeeded != 1 || summary.Failed != 1 || !summary.RecoveryRetained || summary.UndoAvailable || summary.UndoID != "" || summary.BlockingRecoveryID != summary.TransactionID {
		t.Fatalf("ambiguous finish = %#v, %v", summary, err)
	}
	if replaceCalls != 2 || !errors.Is(summary.Results[1].Err, replaceFailure) {
		t.Fatalf("replacement seam calls/results = %d, %#v", replaceCalls, summary.Results)
	}
	manifest, err := w.loadManifest(summary.TransactionID)
	if err != nil || manifest.Status != txApplyPartial || manifest.Files[1].State != statePrepared {
		t.Fatalf("durable ambiguous manifest = %#v, %v", manifest, err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo != nil || !state.Blocked || len(state.Items) != 1 || state.Items[0].Role != RecoveryRolePending || state.Items[0].Status != txApplyPartial {
		t.Fatalf("ambiguous state = %#v, %v", state, err)
	}
	current, _ := w.Discover(context.Background())
	if _, err := w.Begin(context.Background(), current.Search("keep me").Plan(DeleteAll, nil)); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("Begin with ambiguous pending = %v", err)
	}
	w.replaceForTest = nil
	restored, err := w.RestoreRecovery(context.Background(), summary.TransactionID, nil)
	if err != nil || restored.Restored != 2 || restored.Failed != 0 || restored.RecoveryRetained || restored.BlockingRecoveryID != "" {
		t.Fatalf("ambiguous restore retry = %#v, %v", restored, err)
	}
	state, err = w.State(context.Background())
	if err != nil || state.Blocked || len(state.Items) != 0 {
		t.Fatalf("resolved retry state = %#v, %v", state, err)
	}
}

func TestGracefulCancelledApplyPublishesCompleteUndo(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, _ := w.Discover(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	summary, err := w.Apply(ctx, discovery.Search("thank you").Plan(DeleteAll, nil), func(progress Progress) {
		if progress.Completed == 1 {
			cancel()
		}
	})
	if err != nil || !summary.Cancelled || summary.Succeeded != 1 || summary.NotAttempted != 1 || !summary.UndoAvailable || summary.RecoveryRetained || summary.BlockingRecoveryID != "" {
		t.Fatalf("gracefully cancelled apply = %#v, %v", summary, err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo == nil || state.Undo.Partial || state.Blocked {
		t.Fatalf("cancelled apply state = %#v, %v", state, err)
	}
}

func TestAtomicReplacePreRenameConflictIsNotAttempted(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	_, info, _, err := w.readSafe("episode.srt", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	badExpected := sha256.Sum256([]byte("different indexed bytes"))
	_, attempted, err := w.atomicReplace("episode.srt", badExpected, nil, []byte("replacement"), savedMetadata{
		mode: info.Mode(), modTime: info.ModTime(), uid: -1, gid: -1,
	})
	if !errors.Is(err, ErrConflict) || attempted {
		t.Fatalf("pre-rename result attempted=%v, err=%v", attempted, err)
	}
	raw, readErr := os.ReadFile(filepath.Join(root, "episode.srt"))
	if readErr != nil || string(raw) != sampleSRT {
		t.Fatalf("pre-rename conflict changed target: %q, %v", raw, readErr)
	}
}

func TestBeginRejectsBlockingOrphanAndPartialCurrent(t *testing.T) {
	t.Run("orphan", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, root, "episode.srt", sampleSRT)
		w := openTestWorkspace(t, root)
		if err := w.saveRecoveryState(newRecoveryState()); err != nil {
			t.Fatal(err)
		}
		orphan := &transactionManifest{
			Version: manifestVersion, ID: "tx-blocking-orphan", SessionID: "old-session",
			CreatedAt: time.Now().UTC(), Status: txRecoveryPartial,
			Files: []manifestFile{{RelativePath: "episode.srt", State: stateConflict}},
		}
		if err := w.saveManifest(orphan); err != nil {
			t.Fatal(err)
		}
		discovery, _ := w.Discover(context.Background())
		if _, err := w.Begin(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil)); !errors.Is(err, ErrRecoveryPending) {
			t.Fatalf("Begin with orphan = %v", err)
		}
	})

	t.Run("partial-current", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, root, "episode.srt", sampleSRT)
		w := openTestWorkspace(t, root)
		discovery, _ := w.Discover(context.Background())
		mutation, err := w.Apply(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil), nil)
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := w.loadManifest(mutation.TransactionID)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Status = txUndoPartial
		if err := w.saveManifest(manifest); err != nil {
			t.Fatal(err)
		}
		current, _ := w.Discover(context.Background())
		plan := MutationPlan{
			SearchRevision: current.Revision, Scope: DeleteFocused,
			Files: []FileMutation{{
				RelativePath: "episode.srt", ExpectedSHA256: current.Files[0].SHA256,
				ExpectedIdentity: current.Files[0].Identity,
			}},
		}
		if _, err := w.Begin(context.Background(), plan); !errors.Is(err, ErrRecoveryPending) {
			t.Fatalf("Begin with partial current = %v", err)
		}
	})
}

func TestScopedDiscardCannotRetireOutOfScopeCurrentUndo(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w := openTestWorkspace(t, root)

	current := &transactionManifest{
		Version: manifestVersion, ID: "tx-current-b", SessionID: "old-session",
		CreatedAt: time.Now().Add(-time.Minute).UTC(), Status: txComplete,
		Files: []manifestFile{{RelativePath: "b.srt", BackupPath: RecoveryDir + "/tx-current-b/backup/000000.bin", State: stateCommitted}},
	}
	pending := &transactionManifest{
		Version: manifestVersion, ID: "tx-pending-a", SessionID: "old-session",
		CreatedAt: time.Now().UTC(), Status: txRecoveryPartial, PreviousUndoID: current.ID,
		Files: []manifestFile{{RelativePath: "a.srt", BackupPath: RecoveryDir + "/tx-pending-a/backup/000000.bin", State: stateConflict}},
	}
	for _, manifest := range []*transactionManifest{current, pending} {
		if err := w.saveManifest(manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.saveRecoveryState(&recoveryStateFile{
		Version: recoveryStateVersion, Current: current.ID, Pending: pending.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.DiscardRecoveryScoped(context.Background(), pending.ID, []string{"a.srt"}); !errors.Is(err, ErrRecoveryScope) {
		t.Fatalf("scoped discard error = %v", err)
	}
	state, _, err := w.loadRecoveryState()
	if err != nil || state.Current != current.ID || state.Pending != pending.ID {
		t.Fatalf("state changed after refused discard: %#v, %v", state, err)
	}
	for _, id := range []string{current.ID, pending.ID} {
		if _, err := os.Stat(filepath.Join(root, RecoveryDir, id)); err != nil {
			t.Fatalf("transaction %s removed after refused discard: %v", id, err)
		}
	}
}

func TestScopedOrphanRestoreCannotRetireOutOfScopeCurrentUndo(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	current := &transactionManifest{
		Version: manifestVersion, ID: "tx-restore-current-b", SessionID: "old-session",
		CreatedAt: time.Now().Add(-time.Minute).UTC(), Status: txComplete,
		Files: []manifestFile{{RelativePath: "b.srt", BackupPath: RecoveryDir + "/tx-restore-current-b/backup/000000.bin", State: stateCommitted}},
	}
	orphan := &transactionManifest{
		Version: manifestVersion, ID: "tx-restore-orphan-a", SessionID: "old-session",
		CreatedAt: time.Now().UTC(), Status: txRecoveryPartial,
		Files: []manifestFile{{RelativePath: "a.srt", State: stateConflict}},
	}
	for _, manifest := range []*transactionManifest{current, orphan} {
		if err := w.saveManifest(manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.saveRecoveryState(&recoveryStateFile{Version: recoveryStateVersion, Current: current.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RestoreRecoveryScoped(context.Background(), orphan.ID, []string{"a.srt"}, nil); !errors.Is(err, ErrRecoveryScope) {
		t.Fatalf("scoped orphan restore error = %v", err)
	}
	state, _, err := w.loadRecoveryState()
	if err != nil || state.Current != current.ID {
		t.Fatalf("state changed after refused restore: %#v, %v", state, err)
	}
	for _, id := range []string{current.ID, orphan.ID} {
		if _, err := os.Stat(filepath.Join(root, RecoveryDir, id)); err != nil {
			t.Fatalf("transaction %s removed after refused restore: %v", id, err)
		}
	}
}

func TestPendingRestoreMismatchedUndoChainRetiresCurrent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	current := &transactionManifest{
		Version: manifestVersion, ID: "tx-chain-current", SessionID: "old-session",
		CreatedAt: time.Now().Add(-time.Minute).UTC(), Status: txComplete,
		Files: []manifestFile{{RelativePath: "a.srt", BackupPath: RecoveryDir + "/tx-chain-current/backup/000000.bin", State: stateCommitted}},
	}
	pending := &transactionManifest{
		Version: manifestVersion, ID: "tx-chain-pending", SessionID: "old-session",
		CreatedAt: time.Now().UTC(), Status: txRecoveryPartial,
		PreviousUndoID: "tx-different-chain",
	}
	for _, manifest := range []*transactionManifest{current, pending} {
		if err := w.saveManifest(manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.saveRecoveryState(&recoveryStateFile{Version: recoveryStateVersion, Current: current.ID, Pending: pending.ID}); err != nil {
		t.Fatal(err)
	}
	summary, err := w.RestoreRecovery(context.Background(), pending.ID, nil)
	if err != nil || summary.UndoAvailable || summary.UndoID != "" {
		t.Fatalf("mismatched pending restore = %#v, %v", summary, err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo != nil || state.Blocked || len(state.Items) != 0 {
		t.Fatalf("post-restore mismatched state = %#v, %v", state, err)
	}
}

func TestScopedMismatchedPendingRestoreValidatesRetiredCurrent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	current := &transactionManifest{
		Version: manifestVersion, ID: "tx-chain-current-b", SessionID: "old-session",
		CreatedAt: time.Now().Add(-time.Minute).UTC(), Status: txComplete,
		Files: []manifestFile{{RelativePath: "b.srt", BackupPath: RecoveryDir + "/tx-chain-current-b/backup/000000.bin", State: stateCommitted}},
	}
	pending := &transactionManifest{
		Version: manifestVersion, ID: "tx-chain-pending-a", SessionID: "old-session",
		CreatedAt: time.Now().UTC(), Status: txRecoveryPartial,
		PreviousUndoID: "tx-different-chain",
		Files:          []manifestFile{{RelativePath: "a.srt", State: stateConflict}},
	}
	for _, manifest := range []*transactionManifest{current, pending} {
		if err := w.saveManifest(manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.saveRecoveryState(&recoveryStateFile{Version: recoveryStateVersion, Current: current.ID, Pending: pending.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RestoreRecoveryScoped(context.Background(), pending.ID, []string{"a.srt"}, nil); !errors.Is(err, ErrRecoveryScope) {
		t.Fatalf("scoped mismatched pending restore = %v", err)
	}
	state, _, err := w.loadRecoveryState()
	if err != nil || state.Current != current.ID || state.Pending != pending.ID {
		t.Fatalf("state changed after refused pending restore: %#v, %v", state, err)
	}
}

func TestSuccessfulOrphanRestoreRetiresCurrentUndo(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	current := &transactionManifest{
		Version: manifestVersion, ID: "tx-current-empty", SessionID: "old-session",
		CreatedAt: time.Now().Add(-time.Minute).UTC(), Status: txComplete,
		Files: []manifestFile{{RelativePath: "a.srt", BackupPath: RecoveryDir + "/tx-current-empty/backup/000000.bin", State: stateCommitted}},
	}
	orphan := &transactionManifest{
		Version: manifestVersion, ID: "tx-orphan-empty", SessionID: "old-session",
		CreatedAt: time.Now().UTC(), Status: txRecoveryPartial,
	}
	for _, manifest := range []*transactionManifest{current, orphan} {
		if err := w.saveManifest(manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.saveRecoveryState(&recoveryStateFile{Version: recoveryStateVersion, Current: current.ID}); err != nil {
		t.Fatal(err)
	}
	summary, err := w.RestoreRecovery(context.Background(), orphan.ID, nil)
	if err != nil || summary.UndoAvailable || summary.UndoID != "" {
		t.Fatalf("orphan restore = %#v, %v", summary, err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo != nil || len(state.Items) != 0 {
		t.Fatalf("post-orphan state = %#v, %v", state, err)
	}
}

func TestUndoInfoErrorsForCorruptCurrentManifest(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, _ := w.Discover(context.Background())
	mutation, err := w.Apply(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, RecoveryDir, mutation.TransactionID, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := w.UndoInfo(context.Background())
	if info != nil || !errors.Is(err, ErrRecoveryState) {
		t.Fatalf("UndoInfo = %#v, %v; want ErrRecoveryState", info, err)
	}
	state, err := w.State(context.Background())
	if err != nil || !state.Blocked || state.Undo != nil || len(state.Items) != 1 || state.Items[0].Role != RecoveryRoleCorrupt {
		t.Fatalf("corrupt current state = %#v, %v", state, err)
	}
}

func TestCurrentCompleteWithoutRestorableTargetBlocksMutation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	invalid := &transactionManifest{
		Version: manifestVersion, ID: "tx-invalid-current", SessionID: "old-session",
		CreatedAt: time.Now().UTC(), Status: txComplete,
		Files: []manifestFile{{RelativePath: "episode.srt", State: statePlanned}},
	}
	if err := w.saveManifest(invalid); err != nil {
		t.Fatal(err)
	}
	if err := w.saveRecoveryState(&recoveryStateFile{Version: recoveryStateVersion, Current: invalid.ID}); err != nil {
		t.Fatal(err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo != nil || !state.Blocked || len(state.Items) != 1 || !state.Items[0].BlocksMutation {
		t.Fatalf("invalid current state = %#v, %v", state, err)
	}
	discovery, _ := w.Discover(context.Background())
	if _, err := w.Begin(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil)); !errors.Is(err, ErrRecoveryPending) {
		t.Fatalf("Begin with invalid current = %v", err)
	}
	if info, err := w.UndoInfo(context.Background()); info != nil || !errors.Is(err, ErrRecoveryState) {
		t.Fatalf("UndoInfo with invalid current = %#v, %v", info, err)
	}
}
