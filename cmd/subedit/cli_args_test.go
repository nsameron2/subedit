package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseInvocationModesAndFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want invocation
	}{
		{name: "bare tui", want: invocation{command: commandTUI}},
		{name: "legacy tui directory", args: []string{"media"}, want: invocation{command: commandTUI, tuiDirectory: "media"}},
		{name: "explicit tui", args: []string{"tui", "media"}, want: invocation{command: commandTUI, tuiDirectory: "media"}},
		{name: "scan directory", args: []string{"scan", "-d", "media"}, want: invocation{command: commandScan, target: targetDirectory, targetValue: "media"}},
		{name: "scan long equals", args: []string{"scan", "--directory=media", "--json"}, want: invocation{command: commandScan, target: targetDirectory, targetValue: "media", json: true}},
		{name: "search OR", args: []string{"search", "--file", "clip.srt", "one", "two"}, want: invocation{command: commandSearch, target: targetFile, targetValue: "clip.srt", phrases: []string{"one", "two"}}},
		{name: "phrase after separator", args: []string{"search", "-d", "media", "--", "--literal"}, want: invocation{command: commandSearch, target: targetDirectory, targetValue: "media", phrases: []string{"--literal"}}},
		{name: "remove dry run", args: []string{"remove", "--dry-run", "-f=clip.vtt", "phrase"}, want: invocation{command: commandRemove, target: targetFile, targetValue: "clip.vtt", phrases: []string{"phrase"}, dryRun: true}},
		{name: "undo quiet", args: []string{"undo", "-q", "-d", "media"}, want: invocation{command: commandUndo, target: targetDirectory, targetValue: "media", quiet: true}},
		{name: "recovery list", args: []string{"recovery", "list", "-d", "media"}, want: invocation{command: commandRecovery, recoveryAction: "list", target: targetDirectory, targetValue: "media"}},
		{name: "recovery restore", args: []string{"recovery", "restore", "--file", "clip.ass", "tx-123"}, want: invocation{command: commandRecovery, recoveryAction: "restore", target: targetFile, targetValue: "clip.ass", phrases: []string{"tx-123"}}},
		{name: "nested help", args: []string{"help", "recovery", "restore"}, want: invocation{command: commandHelp, helpTopic: "recovery restore"}},
		{name: "recovery group help", args: []string{"recovery", "--help"}, want: invocation{command: commandHelp, helpTopic: "recovery"}},
		{name: "version", args: []string{"--version"}, want: invocation{command: commandVersion}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseInvocation(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if got.command != test.want.command || got.recoveryAction != test.want.recoveryAction ||
				got.target != test.want.target || got.targetValue != test.want.targetValue ||
				got.tuiDirectory != test.want.tuiDirectory || got.json != test.want.json ||
				got.quiet != test.want.quiet || got.dryRun != test.want.dryRun ||
				got.helpTopic != test.want.helpTopic || strings.Join(got.phrases, "\x00") != strings.Join(test.want.phrases, "\x00") {
				t.Fatalf("parseInvocation = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseInvocationRejectsInvalidArguments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"headless target required", []string{"scan"}, "exactly one"},
		{"two scopes", []string{"scan", "-d", ".", "-f", "x.srt"}, "exactly one"},
		{"repeated directory", []string{"scan", "-d", ".", "-d", "."}, "exactly one"},
		{"JSON quiet conflict", []string{"scan", "-d", ".", "--json", "--quiet"}, "mutually exclusive"},
		{"dry-run wrong command", []string{"scan", "-d", ".", "--dry-run"}, "only valid"},
		{"phrase missing", []string{"search", "-d", "."}, "at least one phrase"},
		{"empty phrase", []string{"search", "-d", ".", "one", " \t"}, "must not normalize"},
		{"remove empty phrase", []string{"remove", "-f", "x.srt", "\n"}, "must not normalize"},
		{"flag used as directory value", []string{"scan", "-d", "--json"}, "requires a path"},
		{"unknown option", []string{"scan", "-d", ".", "--wat"}, "unknown option"},
		{"scan positional", []string{"scan", "-d", ".", "extra"}, "does not accept"},
		{"recovery action missing", []string{"recovery"}, "requires list"},
		{"recovery ID missing", []string{"recovery", "restore", "-d", "."}, "exactly one"},
		{"recovery list ID", []string{"recovery", "list", "-d", ".", "tx"}, "does not accept"},
		{"legacy multiple directories", []string{"one", "two"}, "expected one"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseInvocation(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestResolveFileTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	parsed := invocation{target: targetFile, targetValue: filepath.Join(root, "clip.SRT")}
	target, err := parsed.resolveTarget("/unused")
	if err != nil {
		t.Fatal(err)
	}
	if target.root != root || target.relativeFile != "clip.SRT" {
		t.Fatalf("target = %#v", target)
	}
	parsed.targetValue = filepath.Join(root, "clip.txt")
	if _, err := parsed.resolveTarget("/unused"); err == nil || !strings.Contains(err.Error(), "unsupported subtitle extension") {
		t.Fatalf("unsupported target error = %v", err)
	}
}

func TestQuoteShellArgument(t *testing.T) {
	t.Parallel()
	if got, want := quoteShellArgument("/media/Fiji's files"), "'/media/Fiji'\\''s files'"; got != want {
		t.Fatalf("quoteShellArgument = %q, want %q", got, want)
	}
}
