package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"subedit/internal/workspace"
)

const cliFixture = "1\n00:00:00,000 --> 00:00:01,000\nThank you for watching\n\n2\n00:00:02,000 --> 00:00:03,000\nKeep me\n"

func cliRuntime(root string) (runtimeIO, *bytes.Buffer, *bytes.Buffer) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return runtimeIO{
		stdin: strings.NewReader(""), stdout: stdout, stderr: stderr, workingDir: root,
		lookupEnv: func(string) (string, bool) { return "", false }, openWorkspace: workspace.Open,
	}, stdout, stderr
}

func writeCLIFixture(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHeadlessScanSearchAndDryRunAreReadOnly(t *testing.T) {
	root := t.TempDir()
	file := writeCLIFixture(t, root, "clip.srt", cliFixture)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"scan", []string{"scan", "-d", root}, "FILE clip.srt"},
		{"search", []string{"search", "-d", root, "thank you", "missing"}, "MATCH clip.srt"},
		{"dry run", []string{"remove", "-d", root, "--dry-run", "thank you"}, "DRY-RUN cues=1"},
		{"exact file", []string{"search", "-f", file, "thank you"}, "MATCH clip.srt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, stdout, stderr := cliRuntime(root)
			if code := execute(context.Background(), test.args, runtime); code != exitSuccess {
				t.Fatalf("exit = %d; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !strings.Contains(stdout.String(), test.want) || stderr.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
			}
			if _, err := os.Stat(filepath.Join(root, workspace.RecoveryDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read-only command created recovery state: %v", err)
			}
			raw, err := os.ReadFile(file)
			if err != nil || string(raw) != cliFixture {
				t.Fatalf("read-only command changed file: %q, %v", raw, err)
			}
		})
	}
}

func TestHeadlessSearchNoMatchAndPartialPrecedence(t *testing.T) {
	root := t.TempDir()
	writeCLIFixture(t, root, "clip.srt", cliFixture)
	runtime, stdout, stderr := cliRuntime(root)
	if code := execute(context.Background(), []string{"search", "-d", root, "absent"}, runtime); code != exitEmpty {
		t.Fatalf("no-match exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	writeCLIFixture(t, root, "bad.vtt", "not webvtt")
	runtime, stdout, stderr = cliRuntime(root)
	if code := execute(context.Background(), []string{"search", "-d", root, "absent"}, runtime); code != exitPartial {
		t.Fatalf("partial precedence exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr.String(), "ISSUE bad.vtt") || strings.Contains(stdout.String(), "ISSUE") {
		t.Fatalf("stream split stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestHeadlessRemoveThenPersistentUndo(t *testing.T) {
	root := t.TempDir()
	file := writeCLIFixture(t, root, "clip.srt", cliFixture)
	runtime, stdout, stderr := cliRuntime(root)
	if code := execute(context.Background(), []string{"remove", "-f", file, "thank you"}, runtime); code != exitSuccess {
		t.Fatalf("remove exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout.String(), "deleted=1") || !strings.Contains(stdout.String(), "subedit undo --file") {
		t.Fatalf("remove output=%q", stdout)
	}
	removed, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(removed), "Thank you") || !strings.Contains(string(removed), "Keep me") {
		t.Fatalf("removed file=%q err=%v", removed, err)
	}

	// execute opens a fresh workspace, proving undo is durable across invocations.
	runtime, stdout, stderr = cliRuntime(root)
	if code := execute(context.Background(), []string{"undo", "-f", file}, runtime); code != exitSuccess {
		t.Fatalf("undo exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	restored, err := os.ReadFile(file)
	if err != nil || string(restored) != cliFixture {
		t.Fatalf("restored file=%q err=%v", restored, err)
	}

	runtime, stdout, stderr = cliRuntime(root)
	if code := execute(context.Background(), []string{"undo", "-f", file}, runtime); code != exitEmpty {
		t.Fatalf("empty undo exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestFileScopeDoesNotScanSibling(t *testing.T) {
	root := t.TempDir()
	file := writeCLIFixture(t, root, "target.srt", cliFixture)
	writeCLIFixture(t, root, "sibling.srt", cliFixture)
	runtime, stdout, stderr := cliRuntime(root)
	if code := execute(context.Background(), []string{"scan", "-f", file}, runtime); code != exitSuccess {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout.String(), "sibling.srt") || !strings.Contains(stdout.String(), "target.srt") {
		t.Fatalf("exact file scan output=%q", stdout)
	}
}

func TestHeadlessJSONContract(t *testing.T) {
	root := t.TempDir()
	writeCLIFixture(t, root, "clip.srt", cliFixture)
	runtime, stdout, stderr := cliRuntime(root)
	if code := execute(context.Background(), []string{"search", "-d", root, "--json", "thank"}, runtime); code != exitSuccess {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stderr.Len() != 0 || strings.Contains(stdout.String(), "\x1b") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	var document map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "command", "status", "root", "target", "queries", "summary", "matches"} {
		if _, ok := document[key]; !ok {
			t.Errorf("missing JSON key %q", key)
		}
	}
	if document["status"] != "ok" || document["command"] != "search" {
		t.Fatalf("JSON envelope=%#v", document)
	}
}

func TestRemoveDryRunJSONFlagAndNoRecovery(t *testing.T) {
	root := t.TempDir()
	writeCLIFixture(t, root, "clip.srt", cliFixture)
	runtime, stdout, stderr := cliRuntime(root)
	if code := execute(context.Background(), []string{"remove", "-d", root, "--dry-run", "--json", "thank"}, runtime); code != exitSuccess {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var document map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if dry, ok := document["dry_run"].(bool); !ok || !dry {
		t.Fatalf("dry_run=%#v", document["dry_run"])
	}
	if _, err := os.Stat(filepath.Join(root, workspace.RecoveryDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created recovery state: %v", err)
	}
}

func TestJSONApplicableEmptyCollectionsAreArrays(t *testing.T) {
	root := t.TempDir()
	writeCLIFixture(t, root, "clip.srt", cliFixture)
	for _, test := range []struct {
		name string
		args []string
		keys []string
	}{
		{"scan", []string{"scan", "-d", root, "--json"}, []string{"files", "issues"}},
		{"search no match", []string{"search", "-d", root, "--json", "absent"}, []string{"queries", "matches", "issues"}},
		{"recovery list empty", []string{"recovery", "list", "-d", root, "--json"}, []string{"recoveries"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, stdout, stderr := cliRuntime(root)
			code := execute(context.Background(), test.args, runtime)
			if code != exitSuccess && code != exitEmpty {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			var document map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			for _, key := range test.keys {
				value, ok := document[key]
				if !ok {
					t.Errorf("missing applicable collection %q: %#v", key, document)
					continue
				}
				if _, ok := value.([]any); !ok {
					t.Errorf("%s=%#v (%T), want array", key, value, value)
				}
			}
		})
	}
}

func TestQuietSuppressesNormalAndOperationalOutput(t *testing.T) {
	root := t.TempDir()
	writeCLIFixture(t, root, "clip.srt", cliFixture)
	runtime, stdout, stderr := cliRuntime(root)
	if code := execute(context.Background(), []string{"search", "-d", root, "--quiet", "missing"}, runtime); code != exitEmpty {
		t.Fatalf("exit=%d", code)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("quiet stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestQuietAndJSONTargetResolutionFailures(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "does-not-exist")
	runtime, stdout, stderr := cliRuntime(root)
	if code := execute(context.Background(), []string{"scan", "--quiet", "-d", missing}, runtime); code != exitUsage {
		t.Fatalf("quiet exit=%d", code)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("quiet resolution output stdout=%q stderr=%q", stdout, stderr)
	}

	runtime, stdout, stderr = cliRuntime(root)
	if code := execute(context.Background(), []string{"scan", "--json", "-d", missing}, runtime); code != exitUsage {
		t.Fatalf("JSON exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON resolution wrote stderr=%q", stderr)
	}
	var document map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("resolution output is not one JSON document: %v; %q", err, stdout)
	}
	if document["status"] != "error" || document["root"] != missing || document["error"] == "" {
		t.Fatalf("resolution envelope=%#v", document)
	}

	runtime, stdout, stderr = cliRuntime(root)
	if code := execute(context.Background(), []string{"scan", "--json", "-f", filepath.Join(root, "bad.txt")}, runtime); code != exitUsage {
		t.Fatalf("unsupported JSON exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil || stderr.Len() != 0 {
		t.Fatalf("unsupported JSON stdout=%q stderr=%q error=%v", stdout, stderr, err)
	}
}

func TestFileScopedRecoveryFilterRequiresExactSingleton(t *testing.T) {
	t.Parallel()
	items := []workspace.Recovery{
		{ID: "single", FilesList: []string{"alpha.srt"}},
		{ID: "multi", FilesList: []string{"alpha.srt", "beta.srt"}},
		{ID: "other", FilesList: []string{"beta.srt"}},
	}
	got := filterRecoveries(items, "alpha.srt")
	if len(got) != 1 || got[0].ID != "single" {
		t.Fatalf("filtered recoveries=%#v", got)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriteFailureIsOperational(t *testing.T) {
	root := t.TempDir()
	writeCLIFixture(t, root, "clip.srt", cliFixture)
	runtime, _, stderr := cliRuntime(root)
	runtime.stdout = failingWriter{err: io.ErrClosedPipe}
	if code := execute(context.Background(), []string{"scan", "-d", root}, runtime); code != exitOperational {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

func TestInterruptedContextHasExit130Precedence(t *testing.T) {
	root := t.TempDir()
	writeCLIFixture(t, root, "clip.srt", cliFixture)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime, _, _ := cliRuntime(root)
	if code := execute(ctx, []string{"scan", "-d", root}, runtime); code != exitInterrupted {
		t.Fatalf("exit=%d", code)
	}
}

func TestBlockingRecoverySummaryHasPartialPrecedence(t *testing.T) {
	t.Parallel()
	summary := workspace.MutationSummary{BlockingRecoveryID: "tx-pending", RecoveryRetained: true}
	if got := mutationExitCode(summary, false); got != exitPartial {
		t.Fatalf("mutationExitCode=%d, want %d", got, exitPartial)
	}
	if !summaryHasOutcome(summary) {
		t.Fatal("blocking recovery was not treated as a durable mutation outcome")
	}
}

func TestCancelledJSONStatusMatchesExit130(t *testing.T) {
	root := t.TempDir()
	writeCLIFixture(t, root, "clip.srt", cliFixture)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime, stdout, stderr := cliRuntime(root)
	if code := execute(ctx, []string{"scan", "-d", root, "--json"}, runtime); code != exitInterrupted {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stderr.Len() != 0 {
		t.Fatalf("cancelled JSON stderr=%q", stderr)
	}
	var document map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document["status"] != "interrupted" || document["error"] == "" {
		t.Fatalf("cancelled JSON=%#v", document)
	}
}
