package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParseRootArgDefaultsToWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	got, err := parseRootArg(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
}

func TestParseRootArgAcceptsOneDirectory(t *testing.T) {
	root := t.TempDir()
	got, err := parseRootArg([]string{root}, "/not/used")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(root) {
		t.Fatalf("root = %q, want %q", got, filepath.Clean(root))
	}
}

func TestParseRootArgResolvesRootSymlink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := parseRootArg([]string{link}, "/not/used")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("root = %q, want %q", got, want)
	}
}

func TestParseRootArgErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "subtitle.srt")
	if err := os.WriteFile(file, []byte("subtitle"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		want error
	}{
		{name: "help short", args: []string{"-h"}, want: errHelp},
		{name: "help long", args: []string{"--help"}, want: errHelp},
		{name: "unknown option", args: []string{"--wat"}},
		{name: "too many", args: []string{"one", "two"}},
		{name: "not directory", args: []string{file}},
		{name: "missing", args: []string{filepath.Join(t.TempDir(), "missing")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRootArg(tt.args, t.TempDir())
			if err == nil {
				t.Fatal("expected an error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}
