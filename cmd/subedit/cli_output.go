package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	charmterm "github.com/charmbracelet/x/term"

	"subedit/internal/subtitle"
	"subedit/internal/workspace"
)

const outputSchemaVersion = 1

type outputStyle struct{ color bool }

func newOutputStyle(writer io.Writer, lookupEnv func(string) (string, bool)) outputStyle {
	_, noColor := lookupEnv("NO_COLOR")
	term, _ := lookupEnv("TERM")
	return outputStyle{color: writerIsTerminal(writer) && !noColor && !strings.EqualFold(term, "dumb")}
}

func writerIsTerminal(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	return charmterm.IsTerminal(file.Fd())
}

func (s outputStyle) heading(value string) string {
	if !s.color {
		return value
	}
	return "\x1b[1;33m" + value + "\x1b[0m"
}

func (s outputStyle) match(value string) string {
	if !s.color {
		return value
	}
	return "\x1b[36m" + value + "\x1b[0m"
}

func (s outputStyle) success(value string) string {
	if !s.color {
		return value
	}
	return "\x1b[32m" + value + "\x1b[0m"
}

func (s outputStyle) warning(value string) string {
	if !s.color {
		return value
	}
	return "\x1b[33m" + value + "\x1b[0m"
}

func (s outputStyle) failure(value string) string {
	if !s.color {
		return value
	}
	return "\x1b[31m" + value + "\x1b[0m"
}

// sanitizeCLI neutralizes terminal controls and visual-ordering controls at
// the human rendering boundary. Search queries and workspace requests always
// retain the original strings. JSON uses encoding/json's control escaping.
func sanitizeCLI(value string) string {
	var output strings.Builder
	output.Grow(len(value))
	for _, r := range value {
		if unsafeCLIRune(r) {
			output.WriteRune('�')
		} else {
			output.WriteRune(r)
		}
	}
	return output.String()
}

func unsafeCLIRune(r rune) bool {
	if r <= 0x1f || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '\u2028' || r == '\u2029' {
		return true
	}
	switch r {
	case '\u061c', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069',
		'\u206a', '\u206b', '\u206c', '\u206d', '\u206e', '\u206f':
		return true
	}
	return false
}

type jsonTarget struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type jsonFile struct {
	Path     string            `json:"path"`
	Format   subtitle.Format   `json:"format"`
	Encoding subtitle.Encoding `json:"encoding"`
	Cues     int               `json:"cues"`
	Size     int64             `json:"size"`
}

type jsonIssue struct {
	Path  string              `json:"path"`
	Kind  workspace.IssueKind `json:"kind"`
	Error string              `json:"error,omitempty"`
}

type jsonCue struct {
	Path           string   `json:"path"`
	ID             string   `json:"id"`
	StartMS        int64    `json:"start_ms"`
	EndMS          int64    `json:"end_ms"`
	Text           string   `json:"text"`
	MatchedPhrases []string `json:"matched_phrases"`
}

type jsonFileResult struct {
	Path        string               `json:"path"`
	Status      workspace.FileStatus `json:"status"`
	DeletedCues int                  `json:"deleted_cues"`
	Warnings    []string             `json:"warnings,omitempty"`
	Error       string               `json:"error,omitempty"`
}

type jsonSummary struct {
	TransactionID      string `json:"transaction_id,omitempty"`
	BlockingRecoveryID string `json:"blocking_recovery_id,omitempty"`
	Files              *int   `json:"files,omitempty"`
	Issues             *int   `json:"issues,omitempty"`
	MatchingCues       *int   `json:"matching_cues,omitempty"`
	MatchingFiles      *int   `json:"matching_files,omitempty"`
	ScannedFiles       *int   `json:"scanned_files,omitempty"`
	SkippedFiles       *int   `json:"skipped_files,omitempty"`
	Succeeded          *int   `json:"succeeded,omitempty"`
	Restored           *int   `json:"restored,omitempty"`
	Skipped            *int   `json:"skipped,omitempty"`
	Conflicted         *int   `json:"conflicted,omitempty"`
	Failed             *int   `json:"failed,omitempty"`
	NotAttempted       *int   `json:"not_attempted,omitempty"`
	DeletedCues        *int   `json:"deleted_cues,omitempty"`
	Cancelled          *bool  `json:"cancelled,omitempty"`
	Recoveries         *int   `json:"recoveries,omitempty"`
}

type jsonUndo struct {
	ID        string `json:"id"`
	Available bool   `json:"available"`
	Retained  bool   `json:"recovery_retained"`
}

type jsonRecovery struct {
	ID             string                 `json:"id"`
	Role           workspace.RecoveryRole `json:"role"`
	CreatedAt      string                 `json:"created_at,omitempty"`
	Status         string                 `json:"status"`
	Files          int                    `json:"files"`
	Changed        int                    `json:"changed"`
	FilePaths      []string               `json:"file_paths"`
	Corrupt        bool                   `json:"corrupt"`
	BlocksMutation bool                   `json:"blocks_mutation"`
	Error          string                 `json:"error,omitempty"`
}

type jsonDocument struct {
	SchemaVersion int               `json:"schema_version"`
	Command       string            `json:"command"`
	Status        string            `json:"status"`
	Root          string            `json:"root"`
	Target        jsonTarget        `json:"target"`
	DryRun        bool              `json:"dry_run,omitempty"`
	Queries       *[]string         `json:"queries,omitempty"`
	Summary       *jsonSummary      `json:"summary,omitempty"`
	Files         *[]jsonFile       `json:"files,omitempty"`
	Matches       *[]jsonCue        `json:"matches,omitempty"`
	Issues        *[]jsonIssue      `json:"issues,omitempty"`
	Results       *[]jsonFileResult `json:"results,omitempty"`
	Undo          *jsonUndo         `json:"undo,omitempty"`
	Recoveries    *[]jsonRecovery   `json:"recoveries,omitempty"`
	Error         string            `json:"error,omitempty"`
}

func baseJSON(command string, target resolvedTarget) jsonDocument {
	kind := string(targetDirectory)
	path := target.root
	if target.relativeFile != "" {
		kind = string(targetFile)
		path = filepath.Join(target.root, filepath.FromSlash(target.relativeFile))
	}
	return jsonDocument{
		SchemaVersion: outputSchemaVersion,
		Command:       command,
		Status:        "ok",
		Root:          target.root,
		Target:        jsonTarget{Kind: kind, Path: path},
	}
}

func writeJSON(writer io.Writer, document jsonDocument) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return err
	}
	return writeAll(writer, []byte(escapeJSONBidi(buffer.String())))
}

func escapeJSONBidi(value string) string {
	// encoding/json escapes C0 controls but deliberately leaves DEL and C1
	// controls intact. Escape them in the serialized stream so JSON remains
	// valid data yet cannot inject terminal control sequences when printed.
	for r := rune(0x7f); r <= 0x9f; r++ {
		value = strings.ReplaceAll(value, string(r), fmt.Sprintf("\\u%04x", r))
	}
	for _, r := range []rune{
		'\u061c', '\u200e', '\u200f', '\u2028', '\u2029',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069', '\u206a', '\u206b', '\u206c', '\u206d', '\u206e', '\u206f',
	} {
		value = strings.ReplaceAll(value, string(r), fmt.Sprintf("\\u%04x", r))
	}
	return value
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func scanJSON(command string, target resolvedTarget, discovery workspace.Discovery) jsonDocument {
	document := baseJSON(command, target)
	files := make([]jsonFile, 0, len(discovery.Files))
	for _, file := range discovery.Files {
		files = append(files, jsonFile{
			Path: file.RelativePath, Format: file.Document.Format, Encoding: file.Document.Encoding,
			Cues: len(file.Document.Cues), Size: file.Size,
		})
	}
	issues := issueJSON(discovery.Issues)
	if issues == nil {
		issues = []jsonIssue{}
	}
	document.Files = &files
	document.Issues = &issues
	document.Summary = &jsonSummary{Files: intPointer(len(discovery.Files)), Issues: intPointer(len(discovery.Issues))}
	return document
}

func issueJSON(issues []workspace.Issue) []jsonIssue {
	if len(issues) == 0 {
		return nil
	}
	output := make([]jsonIssue, len(issues))
	for index, issue := range issues {
		output[index] = jsonIssue{Path: issue.RelativePath, Kind: issue.Kind}
		if issue.Err != nil {
			output[index].Error = issue.Err.Error()
		}
	}
	return output
}

func searchJSON(command string, target resolvedTarget, discovery workspace.Discovery, result workspace.SearchResult) jsonDocument {
	document := scanJSON(command, target, discovery)
	// Search output is cue-focused; retaining a separate files list duplicates
	// every path and makes streaming consumers do needless work.
	document.Files = nil
	cues := make([]jsonCue, 0, result.MatchingCues)
	for _, match := range result.Matches {
		for _, cueMatch := range match.CueMatches {
			phrases := make([]string, 0, len(cueMatch.QueryIndexes))
			for _, index := range cueMatch.QueryIndexes {
				if index >= 0 && index < len(result.Queries) {
					phrases = append(phrases, result.Queries[index])
				}
			}
			cues = append(cues, jsonCue{
				Path: match.File.RelativePath, ID: string(cueMatch.Cue.ID),
				StartMS: cueMatch.Cue.Start.Milliseconds(), EndMS: cueMatch.Cue.End.Milliseconds(),
				Text: cueMatch.Cue.DisplayText, MatchedPhrases: phrases,
			})
		}
	}
	queries := append([]string(nil), result.Queries...)
	if queries == nil {
		queries = []string{}
	}
	if cues == nil {
		cues = []jsonCue{}
	}
	document.Queries = &queries
	document.Matches = &cues
	document.Summary = &jsonSummary{
		MatchingCues: intPointer(result.MatchingCues), MatchingFiles: intPointer(len(result.Matches)),
		ScannedFiles: intPointer(result.TotalFiles), SkippedFiles: intPointer(result.SkippedFiles),
	}
	return document
}

func addMutationJSON(document *jsonDocument, summary workspace.MutationSummary) {
	results := make([]jsonFileResult, 0, len(summary.Results))
	document.Summary = &jsonSummary{
		TransactionID: summary.TransactionID, BlockingRecoveryID: summary.BlockingRecoveryID,
		Succeeded: intPointer(summary.Succeeded), Restored: intPointer(summary.Restored), Skipped: intPointer(summary.Skipped),
		Conflicted: intPointer(summary.Conflicted), Failed: intPointer(summary.Failed), NotAttempted: intPointer(summary.NotAttempted),
		DeletedCues: intPointer(summary.DeletedCues), Cancelled: boolPointer(summary.Cancelled),
	}
	document.Undo = &jsonUndo{ID: summary.UndoID, Available: summary.UndoAvailable, Retained: summary.RecoveryRetained}
	for _, file := range summary.Results {
		item := jsonFileResult{
			Path: file.RelativePath, Status: file.Status, DeletedCues: file.DeletedCues,
			Warnings: append([]string(nil), file.Warnings...),
		}
		if file.Err != nil {
			item.Error = file.Err.Error()
		}
		results = append(results, item)
	}
	document.Results = &results
}

func intPointer(value int) *int    { return &value }
func boolPointer(value bool) *bool { return &value }

func recoveriesJSON(items []workspace.Recovery) []jsonRecovery {
	output := make([]jsonRecovery, 0, len(items))
	for _, item := range items {
		created := ""
		if !item.CreatedAt.IsZero() {
			created = item.CreatedAt.UTC().Format(time.RFC3339Nano)
		}
		entry := jsonRecovery{
			ID: item.ID, Role: item.Role, CreatedAt: created, Status: item.Status,
			Files: item.Files, Changed: item.Changed, FilePaths: append([]string(nil), item.FilesList...),
			Corrupt: item.Corrupt, BlocksMutation: item.BlocksMutation,
		}
		if entry.FilePaths == nil {
			entry.FilePaths = []string{}
		}
		sort.Strings(entry.FilePaths)
		if item.Err != nil {
			entry.Error = item.Err.Error()
		}
		output = append(output, entry)
	}
	return output
}

func renderScanHuman(stdout, stderr io.Writer, stdoutStyle, stderrStyle outputStyle, discovery workspace.Discovery) error {
	var stdoutBuffer, stderrBuffer bytes.Buffer
	for _, file := range discovery.Files {
		fmt.Fprintf(&stdoutBuffer, "%s %s  format=%s encoding=%s cues=%d size=%d\n",
			stdoutStyle.success("FILE"), sanitizeCLI(file.RelativePath), file.Document.Format,
			file.Document.Encoding, len(file.Document.Cues), file.Size)
	}
	renderIssuesBuffer(&stderrBuffer, stderrStyle, discovery.Issues)
	fmt.Fprintf(&stdoutBuffer, "%s files=%d issues=%d\n", stdoutStyle.heading("SUMMARY"), len(discovery.Files), len(discovery.Issues))
	return errors.Join(writeAll(stdout, stdoutBuffer.Bytes()), writeAll(stderr, stderrBuffer.Bytes()))
}

func renderIssuesBuffer(writer *bytes.Buffer, style outputStyle, issues []workspace.Issue) {
	for _, issue := range issues {
		message := string(issue.Kind)
		if issue.Err != nil {
			message += ": " + issue.Err.Error()
		}
		fmt.Fprintf(writer, "%s %s  %s\n", style.warning("ISSUE"), sanitizeCLI(issue.RelativePath), sanitizeCLI(message))
	}
}

func renderIssuesHuman(writer io.Writer, style outputStyle, issues []workspace.Issue) error {
	var buffer bytes.Buffer
	renderIssuesBuffer(&buffer, style, issues)
	return writeAll(writer, buffer.Bytes())
}

func renderSearchHuman(stdout, stderr io.Writer, stdoutStyle, stderrStyle outputStyle, result workspace.SearchResult, issues []workspace.Issue, dryRun bool) error {
	var stdoutBuffer, stderrBuffer bytes.Buffer
	for _, match := range result.Matches {
		for _, cueMatch := range match.CueMatches {
			phrases := make([]string, 0, len(cueMatch.QueryIndexes))
			for _, index := range cueMatch.QueryIndexes {
				if index >= 0 && index < len(result.Queries) {
					phrases = append(phrases, sanitizeCLI(result.Queries[index]))
				}
			}
			fmt.Fprintf(&stdoutBuffer, "%s %s  %s --> %s  %s  phrases=%s\n",
				stdoutStyle.match("MATCH"), sanitizeCLI(match.File.RelativePath), formatTimestamp(cueMatch.Cue.Start),
				formatTimestamp(cueMatch.Cue.End), sanitizeCLI(cueMatch.Cue.DisplayText), strings.Join(phrases, ","))
		}
	}
	renderIssuesBuffer(&stderrBuffer, stderrStyle, issues)
	label := "SUMMARY"
	if dryRun {
		label = "DRY-RUN"
	}
	fmt.Fprintf(&stdoutBuffer, "%s cues=%d files=%d scanned=%d issues=%d\n", stdoutStyle.heading(label),
		result.MatchingCues, len(result.Matches), result.TotalFiles, result.SkippedFiles)
	return errors.Join(writeAll(stdout, stdoutBuffer.Bytes()), writeAll(stderr, stderrBuffer.Bytes()))
}

func renderMutationHuman(stdout, stderr io.Writer, stdoutStyle, stderrStyle outputStyle, action string, summary workspace.MutationSummary) error {
	var stdoutBuffer, stderrBuffer bytes.Buffer
	for _, result := range summary.Results {
		status := strings.ToUpper(string(result.Status))
		style := stdoutStyle
		if result.Status == workspace.FileFailed || result.Status == workspace.FileConflicted ||
			result.Status == workspace.FileSkipped || result.Status == workspace.FileNotAttempted {
			style = stderrStyle
		}
		styled := style.success(status)
		if result.Status == workspace.FileFailed || result.Status == workspace.FileConflicted {
			styled = style.failure(status)
		} else if result.Status == workspace.FileSkipped || result.Status == workspace.FileNotAttempted {
			styled = style.warning(status)
		}
		writer := &stdoutBuffer
		if result.Status == workspace.FileFailed || result.Status == workspace.FileConflicted ||
			result.Status == workspace.FileSkipped || result.Status == workspace.FileNotAttempted {
			writer = &stderrBuffer
		}
		fmt.Fprintf(writer, "%s %s", styled, sanitizeCLI(result.RelativePath))
		if result.DeletedCues != 0 {
			fmt.Fprintf(writer, "  deleted=%d", result.DeletedCues)
		}
		if result.Err != nil {
			fmt.Fprintf(writer, "  error=%s", sanitizeCLI(result.Err.Error()))
		}
		fmt.Fprintln(writer)
		for _, warning := range result.Warnings {
			fmt.Fprintf(&stderrBuffer, "%s %s  %s\n", stderrStyle.warning("WARNING"), sanitizeCLI(result.RelativePath), sanitizeCLI(warning))
		}
	}
	fmt.Fprintf(&stdoutBuffer, "%s action=%s succeeded=%d restored=%d skipped=%d conflicted=%d failed=%d not_attempted=%d deleted=%d cancelled=%t",
		stdoutStyle.heading("SUMMARY"), action, summary.Succeeded, summary.Restored, summary.Skipped,
		summary.Conflicted, summary.Failed, summary.NotAttempted, summary.DeletedCues, summary.Cancelled)
	if summary.UndoID != "" {
		fmt.Fprintf(&stdoutBuffer, " undo_id=%s", sanitizeCLI(summary.UndoID))
	}
	if summary.BlockingRecoveryID != "" {
		fmt.Fprintf(&stdoutBuffer, " blocking_recovery_id=%s", sanitizeCLI(summary.BlockingRecoveryID))
	}
	fmt.Fprintf(&stdoutBuffer, " undo_available=%t recovery_retained=%t", summary.UndoAvailable, summary.RecoveryRetained)
	fmt.Fprintln(&stdoutBuffer)
	return errors.Join(writeAll(stdout, stdoutBuffer.Bytes()), writeAll(stderr, stderrBuffer.Bytes()))
}

func renderUndoCommand(writer io.Writer, style outputStyle, target resolvedTarget) error {
	option := "--directory"
	path := target.root
	if target.relativeFile != "" {
		option = "--file"
		path = filepath.Join(target.root, filepath.FromSlash(target.relativeFile))
	}
	return writeAll(writer, []byte(fmt.Sprintf("%s subedit undo %s %s\n", style.heading("UNDO"), option, quoteShellArgument(path))))
}

func renderBlockingRecoveryCommands(stdout, stderr io.Writer, stdoutStyle, stderrStyle outputStyle, target resolvedTarget, id string) error {
	if id == "" {
		return nil
	}
	option := "--directory"
	path := target.root
	if target.relativeFile != "" {
		option = "--file"
		path = filepath.Join(target.root, filepath.FromSlash(target.relativeFile))
	}
	restore := fmt.Sprintf("%s subedit recovery restore %s %s %s\n",
		stdoutStyle.heading("RECOVERY"), option, quoteShellArgument(path), quoteShellArgument(id))
	discard := fmt.Sprintf("%s Recovery %s blocks further removal. Restore is recommended. To permanently discard its backups instead, run:\n  subedit recovery discard %s %s %s\n",
		stderrStyle.warning("WARNING"), sanitizeCLI(id), option, quoteShellArgument(path), quoteShellArgument(id))
	return errors.Join(writeAll(stdout, []byte(restore)), writeAll(stderr, []byte(discard)))
}

func quoteShellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func renderRecoveriesHuman(writer io.Writer, style outputStyle, items []workspace.Recovery) error {
	var buffer bytes.Buffer
	for _, item := range items {
		created := "unknown"
		if !item.CreatedAt.IsZero() {
			created = item.CreatedAt.UTC().Format(time.RFC3339Nano)
		}
		fmt.Fprintf(&buffer, "%s %s  role=%s status=%s created=%s files=%d changed=%d blocks=%t",
			style.heading("RECOVERY"), sanitizeCLI(item.ID), item.Role, sanitizeCLI(item.Status), created,
			item.Files, item.Changed, item.BlocksMutation)
		if item.Err != nil {
			fmt.Fprintf(&buffer, " error=%s", sanitizeCLI(item.Err.Error()))
		}
		fmt.Fprintln(&buffer)
		paths := append([]string(nil), item.FilesList...)
		sort.Strings(paths)
		for _, path := range paths {
			fmt.Fprintf(&buffer, "  FILE %s\n", sanitizeCLI(path))
		}
	}
	fmt.Fprintf(&buffer, "%s recoveries=%d\n", style.heading("SUMMARY"), len(items))
	return writeAll(writer, buffer.Bytes())
}
