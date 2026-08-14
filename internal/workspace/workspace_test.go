package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"subedit/internal/subtitle"
)

const sampleSRT = "1\n00:00:01,000 --> 00:00:02,000\nThank you for watching\n\n2\n00:00:03,000 --> 00:00:04,000\nKeep me\n"

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func openTestWorkspace(t *testing.T, root string) *Workspace {
	t.Helper()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestDiscoverSearchAndSort(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "z/Last.VTT", "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nNo match\n")
	writeTestFile(t, root, "A.srt", sampleSRT)
	writeTestFile(t, root, "b.ASS", "[Events]\nFormat: Start, End, Text\nDialogue: 0:00:01.00,0:00:02.00,Thank you for watching\n")
	writeTestFile(t, root, ".hidden/ignored.srt", sampleSRT)
	writeTestFile(t, root, "not.txt", sampleSRT)
	writeTestFile(t, root, "bad.srt", "not an srt")

	w := openTestWorkspace(t, root)
	discovery, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(discovery.Files), 3; got != want {
		t.Fatalf("files = %d, want %d", got, want)
	}
	if got := []string{discovery.Files[0].RelativePath, discovery.Files[1].RelativePath, discovery.Files[2].RelativePath}; got[0] != "A.srt" || got[1] != "b.ASS" || got[2] != "z/Last.VTT" {
		t.Fatalf("unexpected order: %v", got)
	}
	if len(discovery.Issues) != 1 || discovery.Issues[0].RelativePath != "bad.srt" || discovery.Issues[0].Kind != IssueInvalid {
		t.Fatalf("unexpected issues: %#v", discovery.Issues)
	}

	result := discovery.Search("THANK YOU   FOR WATCHING")
	if result.MatchingCues != 2 || len(result.Matches) != 2 {
		t.Fatalf("unexpected search result: %#v", result)
	}
	plan := result.Plan(DeleteAll, nil)
	if len(plan.Files) != 2 || plan.SearchRevision != discovery.Revision {
		t.Fatalf("unexpected plan: %#v", plan)
	}
}

func TestDiscoverWithProgressAccountsForFilesAndIssues(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "one.srt", sampleSRT)
	writeTestFile(t, root, "two.vtt", "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nVisible\n")
	writeTestFile(t, root, "invalid.ass", "[Events]\nnot an event record\n")
	writeTestFile(t, root, "ignored.txt", sampleSRT)

	w := openTestWorkspace(t, root)
	var events []DiscoveryProgress
	discovery, err := w.DiscoverWithProgress(context.Background(), func(progress DiscoveryProgress) {
		events = append(events, progress)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(discovery.Files)+len(discovery.Issues) || len(events) != 3 {
		t.Fatalf("progress events = %#v; discovery = %#v", events, discovery)
	}
	paths := make(map[string]bool, len(events))
	issues := 0
	for index, event := range events {
		if event.Completed != index+1 || event.Total != len(events) {
			t.Fatalf("event %d has counters %d/%d, want %d/%d", index, event.Completed, event.Total, index+1, len(events))
		}
		if event.CurrentPath == "" || paths[event.CurrentPath] {
			t.Fatalf("invalid or duplicate progress path: %#v", event)
		}
		paths[event.CurrentPath] = true
		if event.Issue != nil {
			issues++
			if event.Issue.RelativePath != event.CurrentPath {
				t.Fatalf("issue path does not match event: %#v", event)
			}
		}
	}
	if issues != 1 || !paths["one.srt"] || !paths["two.vtt"] || !paths["invalid.ass"] {
		t.Fatalf("unexpected progress coverage: %#v", events)
	}
}

func TestDiscoverWithProgressCancellationIsPartialAndJoined(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.srt", "b.srt", "c.srt", "d.srt"} {
		writeTestFile(t, root, name, sampleSRT)
	}
	w := openTestWorkspace(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	callbacks := 0
	_, err := w.DiscoverWithProgress(ctx, func(progress DiscoveryProgress) {
		callbacks++
		if progress.Completed != 1 || progress.Total != 4 {
			t.Fatalf("first progress = %#v", progress)
		}
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if callbacks != 1 {
		t.Fatalf("callbacks = %d after cancellation, want 1", callbacks)
	}
	// A completed call has joined its workers, so a subsequent scan can safely
	// start immediately and receives a complete independent progress sequence.
	secondCallbacks := 0
	if _, err := w.DiscoverWithProgress(context.Background(), func(DiscoveryProgress) { secondCallbacks++ }); err != nil {
		t.Fatal(err)
	}
	if secondCallbacks != 4 {
		t.Fatalf("second scan callbacks = %d, want 4", secondCallbacks)
	}
}

func TestDiscoverWithProgressIncludesTraversalIssues(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "safe.srt", sampleSRT)
	if err := os.Symlink("safe.srt", filepath.Join(root, "unsafe.srt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	w := openTestWorkspace(t, root)
	var events []DiscoveryProgress
	discovery, err := w.DiscoverWithProgress(context.Background(), func(progress DiscoveryProgress) {
		events = append(events, progress)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || len(discovery.Files) != 1 || len(discovery.Issues) != 1 {
		t.Fatalf("events = %#v; discovery = %#v", events, discovery)
	}
	foundTraversalIssue := false
	for _, event := range events {
		if event.Total != 2 {
			t.Fatalf("event total = %d, want 2", event.Total)
		}
		if event.CurrentPath == "unsafe.srt" && event.Issue != nil && event.Issue.Kind == IssueSymlink {
			foundTraversalIssue = true
		}
	}
	if !foundTraversalIssue {
		t.Fatalf("symlink traversal issue missing from progress: %#v", events)
	}
}

func TestDiscoverSkipsSymlinkAndHardlink(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "safe.srt", sampleSRT)
	if err := os.Symlink("safe.srt", filepath.Join(root, "link.srt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Link(filepath.Join(root, "safe.srt"), filepath.Join(root, "hard.srt")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	w := openTestWorkspace(t, root)
	discovery, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Files) != 0 {
		t.Fatalf("hard-linked original must also be skipped: %#v", discovery.Files)
	}
	kinds := map[IssueKind]int{}
	for _, issue := range discovery.Issues {
		kinds[issue.Kind]++
	}
	if kinds[IssueSymlink] != 1 || kinds[IssueHardlink] != 2 {
		t.Fatalf("issues = %#v", discovery.Issues)
	}
}

func TestRenamedRootDoesNotSplitDiscoveryOrMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows may prevent renaming an open directory handle")
	}
	parent := t.TempDir()
	originalPath := filepath.Join(parent, "workspace")
	if err := os.Mkdir(originalPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, originalPath, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, originalPath)

	movedPath := filepath.Join(parent, "moved-workspace")
	if err := os.Rename(originalPath, movedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(originalPath, 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := "1\n00:00:01,000 --> 00:00:02,000\nDecoy at stale root path\n"
	writeTestFile(t, originalPath, "episode.srt", decoy)

	discovery, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Files) != 1 || len(discovery.Files[0].Document.Search("thank you")) != 1 {
		t.Fatalf("discovery followed stale absolute root: %#v", discovery)
	}
	summary, err := w.Apply(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Succeeded != 1 {
		t.Fatalf("mutation = %#v", summary)
	}
	moved, err := os.ReadFile(filepath.Join(movedPath, "episode.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(moved) != "1\n00:00:03,000 --> 00:00:04,000\nKeep me\n" {
		t.Fatalf("moved root was not edited: %q", moved)
	}
	stale, err := os.ReadFile(filepath.Join(originalPath, "episode.srt"))
	if err != nil || string(stale) != decoy {
		t.Fatalf("stale root path was touched: %q, %v", stale, err)
	}
	if _, err := os.Stat(filepath.Join(movedPath, RecoveryDir, summary.TransactionID)); err != nil {
		t.Fatalf("recovery data not confined to moved root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(originalPath, RecoveryDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery data appeared under stale path: %v", err)
	}
}

func TestMutationLockUsesRootConfinedHandle(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	first := openTestWorkspace(t, root)
	second := openTestWorkspace(t, root)
	firstDiscovery, _ := first.Discover(context.Background())
	secondDiscovery, _ := second.Discover(context.Background())
	firstTx, err := first.Begin(context.Background(), firstDiscovery.Search("thank you").Plan(DeleteAll, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Begin(context.Background(), secondDiscovery.Search("thank you").Plan(DeleteAll, nil)); !errors.Is(err, ErrMutationLocked) {
		t.Fatalf("second mutation error = %v, want ErrMutationLocked", err)
	}
	if _, err := firstTx.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyUndoExactBytesAndCleanup(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := discovery.Search("thank you for watching").Plan(DeleteAll, nil)
	summary, err := w.Apply(context.Background(), plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Succeeded != 1 || summary.DeletedCues != 1 || !summary.UndoAvailable {
		t.Fatalf("unexpected mutation summary: %#v", summary)
	}
	changed, err := os.ReadFile(filepath.Join(root, "episode.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(changed) != "1\n00:00:03,000 --> 00:00:04,000\nKeep me\n" {
		t.Fatalf("changed bytes = %q", changed)
	}
	if recoveries, _ := w.Recoveries(context.Background()); len(recoveries) != 0 {
		t.Fatalf("live undo point must not appear as crash recovery: %#v", recoveries)
	}

	undo, err := w.Undo(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Restored != 1 {
		t.Fatalf("undo = %#v", undo)
	}
	restored, err := os.ReadFile(filepath.Join(root, "episode.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != sampleSRT {
		t.Fatalf("undo changed bytes: %q", restored)
	}
	if err := w.CleanupSession(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessfulOperationReplacesPreviousUndoPoint(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)

	first, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstSummary, err := w.Apply(context.Background(), first.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondSummary, err := w.Apply(context.Background(), second.Search("keep me").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if firstSummary.TransactionID == secondSummary.TransactionID {
		t.Fatal("operations unexpectedly shared a transaction ID")
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, firstSummary.TransactionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous undo backup still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, secondSummary.TransactionID)); err != nil {
		t.Fatalf("latest undo backup missing: %v", err)
	}
	undo, err := w.Undo(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Restored != 1 {
		t.Fatalf("undo = %#v", undo)
	}
	raw, err := os.ReadFile(filepath.Join(root, "episode.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "1\n00:00:03,000 --> 00:00:04,000\nKeep me\n" {
		t.Fatalf("undo restored more than the latest operation: %q", raw)
	}
}

func TestNoOpTransactionRetainsAndReportsPreviousUndo(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)

	first, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstSummary, err := w.Apply(context.Background(), first.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	noopPlan := MutationPlan{
		SearchRevision: current.Revision,
		Scope:          DeleteFocused,
		Files: []FileMutation{{
			RelativePath:     current.Files[0].RelativePath,
			ExpectedSHA256:   current.Files[0].SHA256,
			ExpectedIdentity: current.Files[0].Identity,
		}},
	}
	noopSummary, err := w.Apply(context.Background(), noopPlan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if noopSummary.Succeeded != 0 || noopSummary.Skipped != 1 || !noopSummary.UndoAvailable {
		t.Fatalf("no-op summary lost previous undo state: %#v", noopSummary)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, noopSummary.TransactionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op transaction recovery still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, firstSummary.TransactionID)); err != nil {
		t.Fatalf("previous undo recovery was removed: %v", err)
	}
	undo, err := w.Undo(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Restored != 1 {
		t.Fatalf("retained undo failed: %#v", undo)
	}
	raw, err := os.ReadFile(filepath.Join(root, "episode.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != sampleSRT {
		t.Fatalf("retained undo restored %q", raw)
	}
}

func TestConflictedTransactionRetainsAndReportsPreviousUndo(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w := openTestWorkspace(t, root)

	first, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstPlan := first.Search("thank you").Plan(DeleteSelected, map[string]bool{"a.srt": true})
	firstSummary, err := w.Apply(context.Background(), firstPlan, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conflictPlan := second.Search("thank you").Plan(DeleteSelected, map[string]bool{"b.srt": true})
	writeTestFile(t, root, "b.srt", sampleSRT+"external change")
	conflictSummary, err := w.Apply(context.Background(), conflictPlan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if conflictSummary.Conflicted != 1 || !conflictSummary.UndoAvailable {
		t.Fatalf("conflict summary lost previous undo: %#v", conflictSummary)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, conflictSummary.TransactionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicted transaction recovery still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, firstSummary.TransactionID)); err != nil {
		t.Fatalf("previous undo recovery was removed: %v", err)
	}
	undo, err := w.Undo(context.Background(), nil)
	if err != nil || undo.Restored != 1 {
		t.Fatalf("retained undo = %#v, %v", undo, err)
	}
	a, err := os.ReadFile(filepath.Join(root, "a.srt"))
	if err != nil || string(a) != sampleSRT {
		t.Fatalf("retained undo restored %q, %v", a, err)
	}
	b, err := os.ReadFile(filepath.Join(root, "b.srt"))
	if err != nil || string(b) != sampleSRT+"external change" {
		t.Fatalf("undo touched conflict target: %q, %v", b, err)
	}
}

func TestApplyDetectsDigestConflict(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := discovery.Search("thank you").Plan(DeleteAll, nil)
	writeTestFile(t, root, "episode.srt", sampleSRT+"externally changed")
	summary, err := w.Apply(context.Background(), plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Conflicted != 1 || summary.Succeeded != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "episode.srt"))
	if string(raw) != sampleSRT+"externally changed" {
		t.Fatal("conflicting file was overwritten")
	}
}

func TestApplyDetectsIdenticalByteIdentityReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Rename cannot portably replace an existing Windows target; production uses MoveFileEx")
	}
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := discovery.Search("thank you").Plan(DeleteAll, nil)
	replacement := filepath.Join(root, "replacement.tmp")
	if err := os.WriteFile(replacement, []byte(sampleSRT), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(root, "episode.srt")); err != nil {
		t.Fatal(err)
	}
	current, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current.Files[0].SHA256 != discovery.Files[0].SHA256 {
		t.Fatal("test replacement did not preserve bytes")
	}
	if current.Files[0].Identity == discovery.Files[0].Identity {
		t.Fatal("test filesystem reused identity for two simultaneously existing objects")
	}
	// Use revision zero to isolate filesystem identity validation from the
	// separate discovery-revision guard.
	plan.SearchRevision = 0
	summary, err := w.Apply(context.Background(), plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Conflicted != 1 || summary.Succeeded != 0 {
		t.Fatalf("identical-byte replacement was not rejected: %#v", summary)
	}
	raw, err := os.ReadFile(filepath.Join(root, "episode.srt"))
	if err != nil || string(raw) != sampleSRT {
		t.Fatalf("replacement was overwritten: %q, %v", raw, err)
	}
}

func TestBeginRequiresFilesystemIdentity(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, err := w.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := discovery.Search("thank you").Plan(DeleteAll, nil)
	plan.Files[0].ExpectedIdentity = FileIdentity{}
	if _, err := w.Begin(context.Background(), plan); err == nil {
		t.Fatal("mutation without a filesystem identity unexpectedly succeeded")
	}
}

func TestUndoConflictRetainsRecovery(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "episode.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, _ := w.Discover(context.Background())
	plan := discovery.Search("thank you").Plan(DeleteAll, nil)
	if _, err := w.Apply(context.Background(), plan, nil); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "episode.srt", "external replacement")
	summary, err := w.Undo(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Conflicted != 1 || !summary.UndoAvailable {
		t.Fatalf("summary = %#v", summary)
	}
	items, err := w.Recoveries(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("current partial undo must not be a crash recovery: %#v, %v", items, err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo == nil || !state.Undo.Partial || !state.Undo.BlocksRemove || !state.Blocked {
		t.Fatalf("partial undo state = %#v, %v", state, err)
	}
	if err := w.CleanupSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = w.State(context.Background())
	if err != nil || state.Undo == nil || !state.Undo.Partial {
		t.Fatal("partial undo recovery was cleaned up")
	}
}

func TestCancellationStopsBetweenFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	writeTestFile(t, root, "b.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, _ := w.Discover(context.Background())
	plan := discovery.Search("thank you").Plan(DeleteAll, nil)
	ctx, cancel := context.WithCancel(context.Background())
	summary, err := w.Apply(ctx, plan, func(progress Progress) {
		if progress.Completed == 1 {
			cancel()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Cancelled || summary.Succeeded != 1 || summary.NotAttempted != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestStaleRevisionRejected(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	first, _ := w.Discover(context.Background())
	plan := first.Search("thank you").Plan(DeleteAll, nil)
	if _, err := w.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := w.Begin(context.Background(), plan)
	if !errors.Is(err, ErrStalePlan) {
		t.Fatalf("err = %v", err)
	}
}

func TestSafeRelative(t *testing.T) {
	for _, unsafe := range []string{"", ".", "../x.srt", "a/../x.srt", "/x.srt", "a//x.srt"} {
		if _, err := safeRelative(unsafe); err == nil {
			t.Errorf("safeRelative(%q) unexpectedly succeeded", unsafe)
		}
	}
	if got, err := safeRelative("a/b.srt"); err != nil || got != "a/b.srt" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRecoveryManifestRejectsTamperedBackup(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.srt", sampleSRT)
	w := openTestWorkspace(t, root)
	discovery, _ := w.Discover(context.Background())
	summary, err := w.Apply(context.Background(), discovery.Search("thank you").Plan(DeleteAll, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := w.loadManifest(summary.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, filepath.FromSlash(manifest.Files[0].BackupPath))
	if err := os.WriteFile(backup, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	undo, err := w.Undo(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if undo.Failed != 1 {
		t.Fatalf("undo = %#v", undo)
	}
	retry, err := w.Undo(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Failed != 1 {
		t.Fatalf("corrupt recovery must remain failed on retry, got %#v", retry)
	}
	items, err := w.Recoveries(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("partial current undo leaked into recoveries: %#v, %v", items, err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo == nil || !state.Undo.Partial {
		t.Fatalf("corrupt backup undo was not retained: %#v, %v", state, err)
	}
}

func TestCleanupSessionPreservesCurrentAndUnsafeRemnants(t *testing.T) {
	root := t.TempDir()
	w := openTestWorkspace(t, root)
	active := &transactionManifest{
		Version: manifestVersion, ID: "tx-active-test", SessionID: w.session,
		CreatedAt: time.Now().UTC(), Status: txActive,
	}
	unknown := &transactionManifest{
		Version: manifestVersion, ID: "tx-unknown-test", SessionID: w.session,
		CreatedAt: time.Now().UTC(), Status: "future-state",
	}
	complete := &transactionManifest{
		Version: manifestVersion, ID: "tx-complete-test", SessionID: w.session,
		CreatedAt: time.Now().UTC(), Status: txComplete,
		Files: []manifestFile{{
			RelativePath: "current.srt", BackupPath: RecoveryDir + "/tx-complete-test/backup/000000.bin",
			State: stateCommitted,
		}},
	}
	for _, manifest := range []*transactionManifest{active, unknown, complete} {
		if err := w.saveManifest(manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.CleanupSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, complete.ID)); err != nil {
		t.Fatalf("persistent current transaction was cleaned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, active.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provably inactive transaction was not retired: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, RecoveryDir, unknown.ID)); err != nil {
		t.Fatalf("unknown-state transaction %s was removed: %v", unknown.ID, err)
	}
	state, err := w.State(context.Background())
	if err != nil || state.Undo == nil || state.Undo.ID != complete.ID || !state.Blocked {
		t.Fatalf("cleanup state = %#v, %v", state, err)
	}
}

func TestCleanupSessionReportsAndRetainsCorruptManifest(t *testing.T) {
	root := t.TempDir()
	w := openTestWorkspace(t, root)
	if err := w.ensureRecoveryDir(); err != nil {
		t.Fatal(err)
	}
	id := "tx-corrupt-test"
	directory := filepath.Join(root, RecoveryDir, id)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := w.CleanupSession(context.Background())
	if err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("cleanup error = %v", err)
	}
	if _, statErr := os.Stat(directory); statErr != nil {
		t.Fatalf("corrupt recovery was removed: %v", statErr)
	}
}

func TestPlansCarryDocumentDigest(t *testing.T) {
	doc, err := subtitle.Parse("x.srt", []byte(sampleSRT))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(sampleSRT))
	identity := FileIdentity{kind: "test", volume: 1, object: 2}
	file := File{RelativePath: "x.srt", SHA256: hash, Identity: identity, Document: doc}
	result := SearchResult{Revision: 7, Matches: []FileMatch{{File: file, Cues: doc.Search("thank you")}}}
	plan := result.Plan(DeleteFocused, nil)
	if len(plan.Files) != 1 || plan.Files[0].ExpectedSHA256 != hash || plan.Files[0].ExpectedIdentity != identity || plan.SearchRevision != 7 {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestWindowsPathBehaviorDocumented(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by cross compilation; temporary-file semantics vary under antivirus")
	}
}
