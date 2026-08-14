package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"subedit/internal/subtitle"
	"subedit/internal/workspace"
)

func TestSanitizeCLI(t *testing.T) {
	t.Parallel()
	input := "safe\x1b]8;;evil\a\n\u202eName\u2069"
	got := sanitizeCLI(input)
	if strings.ContainsAny(got, "\x1b\a\n") || strings.ContainsRune(got, '\u202e') || strings.ContainsRune(got, '\u2069') {
		t.Fatalf("unsafe controls remain in %q", got)
	}
	if !strings.Contains(got, "safe") || !strings.Contains(got, "Name") {
		t.Fatalf("safe text lost from %q", got)
	}
}

func TestJSONEnvelopeAndBidiEscaping(t *testing.T) {
	t.Parallel()
	target := resolvedTarget{root: "/media", relativeFile: "bad\u202ename.srt"}
	document := baseJSON("search", target)
	queries := []string{"hello"}
	matches := []jsonCue{{Path: "bad\u202ename.srt", ID: "1", Text: "hello", MatchedPhrases: []string{"hello"}}}
	document.Queries = &queries
	document.Matches = &matches
	document.Summary = &jsonSummary{MatchingCues: intPointer(1), MatchingFiles: intPointer(1), ScannedFiles: intPointer(1)}
	var output bytes.Buffer
	if err := writeJSON(&output, document); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("\x1b")) || strings.ContainsRune(output.String(), '\u202e') || !strings.Contains(output.String(), `\u202e`) {
		t.Fatalf("JSON does not safely escape display controls: %q", output.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "command", "status", "root", "target", "queries", "summary", "matches"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("JSON envelope missing %q: %v", key, decoded)
		}
	}
	targetValue := decoded["target"].(map[string]any)
	if targetValue["kind"] != "file" || !strings.Contains(targetValue["path"].(string), "\u202e") {
		t.Errorf("decoded target = %#v", targetValue)
	}
}

func TestJSONEscapesDELAndC1Controls(t *testing.T) {
	t.Parallel()
	document := baseJSON("search", resolvedTarget{root: "/media"})
	queries := []string{"probe\u009b31mred\u007f"}
	document.Queries = &queries
	var output bytes.Buffer
	if err := writeJSON(&output, document); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(output.String(), '\u009b') || strings.ContainsRune(output.String(), '\u007f') ||
		!strings.Contains(output.String(), `\u009b`) || !strings.Contains(output.String(), `\u007f`) {
		t.Fatalf("unsafe JSON controls: %q", output.String())
	}
	var decoded jsonDocument
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Queries == nil || len(*decoded.Queries) != 1 || (*decoded.Queries)[0] != queries[0] {
		t.Fatalf("decoded queries=%#v", decoded.Queries)
	}
}

func TestHumanIssuesGoToStderr(t *testing.T) {
	t.Parallel()
	discovery := workspace.Discovery{
		Files:  []workspace.File{{RelativePath: "ok.srt", Size: 10, Document: &subtitle.Document{Format: subtitle.FormatSRT, Encoding: subtitle.EncodingUTF8}}},
		Issues: []workspace.Issue{{RelativePath: "bad\x1b.srt", Kind: workspace.IssueInvalid, Err: errors.New("bad\nparse")}},
	}
	var stdout, stderr bytes.Buffer
	if err := renderScanHuman(&stdout, &stderr, outputStyle{}, outputStyle{}, discovery); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "ISSUE") || !strings.Contains(stderr.String(), "ISSUE") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.ContainsAny(stderr.String(), "\x1b\n") {
		// The renderer's record-ending newline is expected; controls inside
		// values must not create a second row.
		if strings.Count(stderr.String(), "\n") != 1 {
			t.Fatalf("unsafe stderr output: %q", stderr.String())
		}
	}
}

func TestSearchJSONDeterministicPhraseMetadata(t *testing.T) {
	t.Parallel()
	cue := subtitle.Cue{ID: "cue", Start: time.Second, End: 2 * time.Second, DisplayText: "hello world"}
	file := workspace.File{RelativePath: "clip.srt"}
	result := workspace.SearchResult{
		Queries: []string{"hello", "world"}, MatchingCues: 1, TotalFiles: 1,
		Matches: []workspace.FileMatch{{File: file, CueMatches: []workspace.CueMatch{{Cue: cue, QueryIndexes: []int{0, 1}}}}},
	}
	document := searchJSON("search", resolvedTarget{root: "/media"}, workspace.Discovery{}, result)
	if document.Matches == nil || len(*document.Matches) != 1 || strings.Join((*document.Matches)[0].MatchedPhrases, ",") != "hello,world" {
		t.Fatalf("matches = %#v", document.Matches)
	}
}

func TestRecoveryJSONFilePathsAreNeverNull(t *testing.T) {
	t.Parallel()
	entries := recoveriesJSON([]workspace.Recovery{{ID: "corrupt", Role: workspace.RecoveryRoleCorrupt, Corrupt: true}})
	if len(entries) != 1 || entries[0].FilePaths == nil || len(entries[0].FilePaths) != 0 {
		t.Fatalf("entries=%#v", entries)
	}
	document := baseJSON("recovery list", resolvedTarget{root: "/media"})
	document.Recoveries = &entries
	var output bytes.Buffer
	if err := writeJSON(&output, document); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"file_paths":[]`) {
		t.Fatalf("recovery JSON=%q", output.String())
	}
}

func TestRenderUndoCommandQuotesCanonicalTarget(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := renderUndoCommand(&output, outputStyle{}, resolvedTarget{root: "/media/Fiji's files"}); err != nil {
		t.Fatal(err)
	}
	want := "UNDO subedit undo --directory '/media/Fiji'\\''s files'\n"
	if output.String() != want {
		t.Fatalf("undo output = %q, want %q", output.String(), want)
	}
}

func TestBlockingRecoveryJSONAndHumanGuidance(t *testing.T) {
	t.Parallel()
	summary := workspace.MutationSummary{
		TransactionID: "tx-blocking", BlockingRecoveryID: "tx-blocking",
		Succeeded: 1, RecoveryRetained: true,
	}
	document := baseJSON("remove", resolvedTarget{root: "/media"})
	addMutationJSON(&document, summary)
	if document.Summary == nil || document.Summary.BlockingRecoveryID != "tx-blocking" {
		t.Fatalf("JSON summary=%#v", document.Summary)
	}
	var encoded bytes.Buffer
	if err := writeJSON(&encoded, document); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded.String(), `"blocking_recovery_id":"tx-blocking"`) {
		t.Fatalf("JSON=%q", encoded.String())
	}

	target := resolvedTarget{root: "/media/Fiji's files"}
	var stdout, stderr bytes.Buffer
	if err := renderMutationHuman(&stdout, &stderr, outputStyle{}, outputStyle{}, "remove", summary); err != nil {
		t.Fatal(err)
	}
	if err := renderBlockingRecoveryCommands(&stdout, &stderr, outputStyle{}, outputStyle{}, target, summary.BlockingRecoveryID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "blocking_recovery_id=tx-blocking") ||
		!strings.Contains(stdout.String(), "subedit recovery restore --directory '/media/Fiji'\\''s files' 'tx-blocking'") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "permanently discard") ||
		!strings.Contains(stderr.String(), "subedit recovery discard --directory '/media/Fiji'\\''s files' 'tx-blocking'") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestBlockingRecoveryGuidanceUsesExactFileScope(t *testing.T) {
	t.Parallel()
	target := resolvedTarget{root: "/media", relativeFile: "clip.srt"}
	var stdout, stderr bytes.Buffer
	if err := renderBlockingRecoveryCommands(&stdout, &stderr, outputStyle{}, outputStyle{}, target, "tx-id"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "restore --file '/media/clip.srt' 'tx-id'") ||
		!strings.Contains(stderr.String(), "discard --file '/media/clip.srt' 'tx-id'") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestOutputStyleHonorsNOColorAndDumbTerminal(t *testing.T) {
	t.Parallel()
	for _, env := range []map[string]string{
		{"NO_COLOR": ""},
		{"TERM": "dumb"},
	} {
		lookup := func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
		}
		style := newOutputStyle(&bytes.Buffer{}, lookup)
		if style.color || strings.Contains(style.heading("SUMMARY"), "\x1b") {
			t.Fatalf("style=%#v env=%v", style, env)
		}
	}
}

func TestDevNullIsNotTerminal(t *testing.T) {
	t.Parallel()
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skip(err)
	}
	defer file.Close()
	if writerIsTerminal(file) {
		t.Fatal("/dev/null was incorrectly classified as a terminal")
	}
}
