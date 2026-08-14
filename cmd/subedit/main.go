package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"subedit/internal/tui"
	"subedit/internal/workspace"
)

const version = "0.1.0"

const usage = `Usage:
  subedit [DIRECTORY]
  subedit tui [DIRECTORY]
  subedit scan (-d DIRECTORY | -f FILE) [--json | --quiet]
  subedit search (-d DIRECTORY | -f FILE) [--json | --quiet] PHRASE...
  subedit remove (-d DIRECTORY | -f FILE) [--dry-run] [--json | --quiet] PHRASE...
  subedit undo (-d DIRECTORY | -f FILE) [--json | --quiet]
  subedit recovery list (-d DIRECTORY | -f FILE) [--json | --quiet]
  subedit recovery restore (-d DIRECTORY | -f FILE) [--json | --quiet] ID
  subedit recovery discard (-d DIRECTORY | -f FILE) [--json | --quiet] ID

TUI mode recursively edits SRT, ASS, and WebVTT files. Headless search and
remove treat multiple phrases as literal OR alternatives. Headless remove is
immediate; use --dry-run to review its exact current matches without writing.

Exit codes: 0 success, 1 empty result, 2 usage, 3 operational failure,
4 partial/skipped result, 130 interrupted.`

var helpTopics = map[string]string{
	"tui": `Usage: subedit tui [DIRECTORY]

Launch the interactive subtitle editor. DIRECTORY defaults to the current directory.`,
	"scan": `Usage: subedit scan (-d DIRECTORY | -f FILE) [--json | --quiet]

Index subtitles without modifying them. --file indexes exactly that file.`,
	"search": `Usage: subedit search (-d DIRECTORY | -f FILE) [--json | --quiet] PHRASE...

Print cues matching any literal phrase. Every phrase must contain visible text.`,
	"remove": `Usage: subedit remove (-d DIRECTORY | -f FILE) [--dry-run] [--json | --quiet] PHRASE...

Immediately remove whole cues matching any phrase. --dry-run performs no writes.`,
	"undo": `Usage: subedit undo (-d DIRECTORY | -f FILE) [--json | --quiet]

Restore the current persistent undo point. --file restricts restoration to that target.`,
	"recovery": `Usage: subedit recovery <list|restore|discard> (-d DIRECTORY | -f FILE) ...

Inspect or explicitly resolve durable crash-recovery transactions.`,
	"recovery list":    `Usage: subedit recovery list (-d DIRECTORY | -f FILE) [--json | --quiet]`,
	"recovery restore": `Usage: subedit recovery restore (-d DIRECTORY | -f FILE) [--json | --quiet] ID`,
	"recovery discard": `Usage: subedit recovery discard (-d DIRECTORY | -f FILE) [--json | --quiet] ID`,
}

const (
	exitSuccess     = 0
	exitEmpty       = 1
	exitUsage       = 2
	exitOperational = 3
	exitPartial     = 4
	exitInterrupted = 130
)

type runtimeIO struct {
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	workingDir string
	lookupEnv  func(string) (string, bool)

	openWorkspace func(string) (*workspace.Workspace, error)
	runTUI        func(context.Context, *workspace.Workspace, string, runtimeIO) (tea.Model, error)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	workingDirectory, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "subedit: determine current directory:", err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return execute(ctx, args, runtimeIO{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, workingDir: workingDirectory,
		lookupEnv: os.LookupEnv, openWorkspace: workspace.Open, runTUI: runBubbleTea,
	})
}

func execute(ctx context.Context, args []string, runtime runtimeIO) int {
	if runtime.lookupEnv == nil {
		runtime.lookupEnv = os.LookupEnv
	}
	if runtime.openWorkspace == nil {
		runtime.openWorkspace = workspace.Open
	}
	if runtime.runTUI == nil {
		runtime.runTUI = runBubbleTea
	}
	parsed, err := parseInvocation(args)
	if err != nil {
		_ = writeAll(runtime.stderr, []byte("subedit: "+sanitizeCLI(err.Error())+"\n"+usage+"\n"))
		return exitUsage
	}
	switch parsed.command {
	case commandHelp:
		text := usage
		if parsed.helpTopic == "" {
		} else {
			text = helpTopics[parsed.helpTopic]
		}
		if err := writeAll(runtime.stdout, []byte(text+"\n")); err != nil {
			return renderWriteError(runtime, err)
		}
		return exitSuccess
	case commandVersion:
		if err := writeAll(runtime.stdout, []byte("subedit "+version+"\n")); err != nil {
			return renderWriteError(runtime, err)
		}
		return exitSuccess
	case commandTUI:
		return executeTUI(ctx, parsed, runtime)
	default:
		return executeHeadless(ctx, parsed, runtime)
	}
}

func executeTUI(ctx context.Context, parsed invocation, runtime runtimeIO) int {
	root, err := resolveDirectory(parsed.tuiDirectory, runtime.workingDir)
	if err != nil {
		_ = writeAll(runtime.stderr, []byte("subedit: "+sanitizeCLI(err.Error())+"\n"))
		return exitUsage
	}
	ws, err := runtime.openWorkspace(root)
	if err != nil {
		return renderOperationalError(runtime, parsed, resolvedTarget{root: root}, err)
	}
	finalModel, runErr := runtime.runTUI(ctx, ws, root, runtime)
	cleanExit := runErr == nil && userQuitRequested(finalModel)
	var cleanupErr error
	if cleanExit {
		cleanupErr = ws.CleanupSession(context.Background())
	} else if runErr == nil {
		runErr = errors.New("terminal UI ended without an explicit user quit; recovery data was retained")
	}
	closeErr := ws.Close()
	if ctx.Err() != nil {
		return exitInterrupted
	}
	for _, item := range []struct {
		label string
		err   error
	}{{"terminal UI", runErr}, {"cleanup recovery data", cleanupErr}, {"close workspace", closeErr}} {
		if item.err != nil {
			fmt.Fprintf(runtime.stderr, "subedit: %s: %s\n", item.label, sanitizeCLI(item.err.Error()))
		}
	}
	if runErr != nil || cleanupErr != nil || closeErr != nil {
		return exitOperational
	}
	return exitSuccess
}

func runBubbleTea(ctx context.Context, ws *workspace.Workspace, root string, runtime runtimeIO) (tea.Model, error) {
	state, err := ws.State(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect recovery state: %w", err)
	}
	backend := newAppBackend(ws)
	defer backend.Close()
	options := tuiOptionsFromRecoveryState(root, state)
	model := tui.New(backend, options)
	return tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(runtime.stdin), tea.WithOutput(runtime.stdout),
		tea.WithEnvironment(os.Environ()), tea.WithoutSignalHandler()).Run()
}

func tuiOptionsFromRecoveryState(root string, state workspace.RecoveryState) tui.Options {
	options := tui.Options{Root: root}
	if state.Undo != nil {
		options.InitialUndoAvailable = true
		if state.Undo.Partial || state.Undo.BlocksRemove {
			options.InitialRetainedUndoID = state.Undo.ID
		}
		return options
	}
	// A current pointer can be structurally loadable yet invalid as an undo
	// target (for example, a complete manifest that changed no file). State
	// intentionally omits it from Undo, but it still blocks mutations and must
	// remain explicitly discardable instead of disappearing from both startup
	// recovery and undo UI.
	for _, item := range state.Items {
		if item.Role == workspace.RecoveryRoleUndo && item.BlocksMutation {
			options.InitialUndoAvailable = true
			options.InitialRetainedUndoID = item.ID
			break
		}
	}
	return options
}

func executeHeadless(ctx context.Context, parsed invocation, runtime runtimeIO) int {
	target, err := parsed.resolveTarget(runtime.workingDir)
	if err != nil {
		if parsed.json {
			target = unresolvedTarget(parsed, runtime.workingDir)
			document := baseJSON(commandName(parsed), target)
			document.Status = "error"
			document.Error = err.Error()
			return writeJSONResult(runtime, document, exitUsage)
		}
		if !parsed.quiet {
			_ = writeAll(runtime.stderr, []byte("subedit: "+sanitizeCLI(err.Error())+"\n"))
		}
		return exitUsage
	}
	ws, err := runtime.openWorkspace(target.root)
	if err != nil {
		return renderOperationalError(runtime, parsed, target, err)
	}
	stdoutStyle := newOutputStyle(runtime.stdout, runtime.lookupEnv)
	stderrStyle := newOutputStyle(runtime.stderr, runtime.lookupEnv)

	var code int
	wroteJSON := parsed.json
	switch parsed.command {
	case commandScan:
		code = executeScan(ctx, parsed, runtime, stdoutStyle, stderrStyle, ws, target)
	case commandSearch:
		code = executeSearch(ctx, parsed, runtime, stdoutStyle, stderrStyle, ws, target, false)
	case commandRemove:
		if parsed.dryRun {
			code = executeSearch(ctx, parsed, runtime, stdoutStyle, stderrStyle, ws, target, true)
		} else {
			code = executeRemove(ctx, parsed, runtime, stdoutStyle, stderrStyle, ws, target)
		}
	case commandUndo:
		code = executeUndo(ctx, parsed, runtime, stdoutStyle, stderrStyle, ws, target)
	case commandRecovery:
		code = executeRecovery(ctx, parsed, runtime, stdoutStyle, stderrStyle, ws, target)
	default:
		code = exitUsage
	}
	if ctx.Err() != nil {
		code = exitInterrupted
	}
	if closeErr := ws.Close(); closeErr != nil && code != exitInterrupted {
		if !parsed.quiet && !wroteJSON {
			_ = writeAll(runtime.stderr, []byte("subedit: close workspace: "+sanitizeCLI(closeErr.Error())+"\n"))
		}
		code = exitOperational
	}
	return code
}

func discoverTarget(ctx context.Context, ws *workspace.Workspace, target resolvedTarget) (workspace.Discovery, error) {
	if target.relativeFile != "" {
		return ws.DiscoverFile(ctx, target.relativeFile, nil)
	}
	return ws.Discover(ctx)
}

func executeScan(ctx context.Context, parsed invocation, runtime runtimeIO, stdoutStyle, stderrStyle outputStyle, ws *workspace.Workspace, target resolvedTarget) int {
	discovery, err := discoverTarget(ctx, ws, target)
	if err != nil {
		return renderOperationalError(runtime, parsed, target, err)
	}
	code := exitSuccess
	if len(discovery.Issues) > 0 {
		code = exitPartial
	}
	if parsed.json {
		document := scanJSON("scan", target, discovery)
		document.Status = statusForExit(code)
		return writeJSONResult(runtime, document, code)
	}
	if !parsed.quiet {
		if err := renderScanHuman(runtime.stdout, runtime.stderr, stdoutStyle, stderrStyle, discovery); err != nil {
			return renderWriteError(runtime, err)
		}
	}
	return code
}

func executeSearch(ctx context.Context, parsed invocation, runtime runtimeIO, stdoutStyle, stderrStyle outputStyle, ws *workspace.Workspace, target resolvedTarget, dryRun bool) int {
	discovery, err := discoverTarget(ctx, ws, target)
	if err != nil {
		return renderOperationalError(runtime, parsed, target, err)
	}
	result := discovery.SearchAny(parsed.phrases)
	code := resultExitCode(result.MatchingCues, discovery.Issues)
	command := "search"
	if dryRun {
		command = "remove"
	}
	if parsed.json {
		document := searchJSON(command, target, discovery, result)
		document.DryRun = dryRun
		document.Status = statusForExit(code)
		return writeJSONResult(runtime, document, code)
	}
	if !parsed.quiet {
		if err := renderSearchHuman(runtime.stdout, runtime.stderr, stdoutStyle, stderrStyle, result, discovery.Issues, dryRun); err != nil {
			return renderWriteError(runtime, err)
		}
	}
	return code
}

func executeRemove(ctx context.Context, parsed invocation, runtime runtimeIO, stdoutStyle, stderrStyle outputStyle, ws *workspace.Workspace, target resolvedTarget) int {
	discovery, err := discoverTarget(ctx, ws, target)
	if err != nil {
		return renderOperationalError(runtime, parsed, target, err)
	}
	result := discovery.SearchAny(parsed.phrases)
	if result.MatchingCues == 0 {
		code := resultExitCode(0, discovery.Issues)
		if parsed.json {
			document := searchJSON("remove", target, discovery, result)
			document.Status = statusForExit(code)
			return writeJSONResult(runtime, document, code)
		}
		if !parsed.quiet {
			if err := renderSearchHuman(runtime.stdout, runtime.stderr, stdoutStyle, stderrStyle, result, discovery.Issues, false); err != nil {
				return renderWriteError(runtime, err)
			}
		}
		return code
	}
	summary, err := ws.Apply(ctx, result.Plan(workspace.DeleteAll, nil), nil)
	if err != nil && !summaryHasOutcome(summary) {
		return renderOperationalError(runtime, parsed, target, err)
	}
	code := mutationExitCode(summary, len(discovery.Issues) > 0 || err != nil)
	if parsed.json {
		document := baseJSON("remove", target)
		queries := append([]string(nil), result.Queries...)
		issues := issueJSON(discovery.Issues)
		if issues == nil {
			issues = []jsonIssue{}
		}
		document.Queries = &queries
		document.Issues = &issues
		addMutationJSON(&document, summary)
		if err != nil {
			document.Error = err.Error()
		}
		document.Status = statusForExit(code)
		return writeJSONResult(runtime, document, code)
	}
	if !parsed.quiet {
		if issueErr := renderIssuesHuman(runtime.stderr, stderrStyle, discovery.Issues); issueErr != nil {
			return renderWriteError(runtime, issueErr)
		}
		if renderErr := renderMutationHuman(runtime.stdout, runtime.stderr, stdoutStyle, stderrStyle, "remove", summary); renderErr != nil {
			return renderWriteError(runtime, renderErr)
		}
		if err != nil {
			if writeErr := writeAll(runtime.stderr, []byte("subedit: "+sanitizeCLI(err.Error())+"\n")); writeErr != nil {
				return renderWriteError(runtime, writeErr)
			}
		}
		if summary.UndoID != "" || summary.UndoAvailable {
			if renderErr := renderUndoCommand(runtime.stdout, stdoutStyle, target); renderErr != nil {
				return renderWriteError(runtime, renderErr)
			}
		}
		if renderErr := renderBlockingRecoveryCommands(runtime.stdout, runtime.stderr, stdoutStyle, stderrStyle, target, summary.BlockingRecoveryID); renderErr != nil {
			return renderWriteError(runtime, renderErr)
		}
	}
	return code
}

func executeUndo(ctx context.Context, parsed invocation, runtime runtimeIO, stdoutStyle, stderrStyle outputStyle, ws *workspace.Workspace, target resolvedTarget) int {
	info, err := ws.UndoInfo(ctx)
	if errors.Is(err, workspace.ErrNoUndo) || (err == nil && info == nil) {
		return renderEmptyUndo(runtime, parsed, target)
	}
	if err != nil {
		return renderOperationalError(runtime, parsed, target, err)
	}
	allowed := allowedPaths(target)
	summary, err := ws.UndoScoped(ctx, info.ID, allowed, nil)
	if errors.Is(err, workspace.ErrNoUndo) {
		return renderEmptyUndo(runtime, parsed, target)
	}
	if err != nil && !summaryHasOutcome(summary) {
		return renderOperationalError(runtime, parsed, target, err)
	}
	code := mutationExitCode(summary, err != nil)
	if parsed.json {
		document := baseJSON("undo", target)
		addMutationJSON(&document, summary)
		if err != nil {
			document.Error = err.Error()
		}
		document.Status = statusForExit(code)
		return writeJSONResult(runtime, document, code)
	}
	if !parsed.quiet {
		if renderErr := renderMutationHuman(runtime.stdout, runtime.stderr, stdoutStyle, stderrStyle, "undo", summary); renderErr != nil {
			return renderWriteError(runtime, renderErr)
		}
		if err != nil {
			if writeErr := writeAll(runtime.stderr, []byte("subedit: "+sanitizeCLI(err.Error())+"\n")); writeErr != nil {
				return renderWriteError(runtime, writeErr)
			}
		}
		if renderErr := renderBlockingRecoveryCommands(runtime.stdout, runtime.stderr, stdoutStyle, stderrStyle, target, summary.BlockingRecoveryID); renderErr != nil {
			return renderWriteError(runtime, renderErr)
		}
	}
	return code
}

func renderEmptyUndo(runtime runtimeIO, parsed invocation, target resolvedTarget) int {
	if parsed.json {
		document := baseJSON("undo", target)
		document.Status = "empty"
		document.Undo = &jsonUndo{Available: false}
		return writeJSONResult(runtime, document, exitEmpty)
	}
	if !parsed.quiet {
		if err := writeAll(runtime.stdout, []byte("No undo is available for this target.\n")); err != nil {
			return renderWriteError(runtime, err)
		}
	}
	return exitEmpty
}

func executeRecovery(ctx context.Context, parsed invocation, runtime runtimeIO, stdoutStyle, stderrStyle outputStyle, ws *workspace.Workspace, target resolvedTarget) int {
	allowed := allowedPaths(target)
	switch parsed.recoveryAction {
	case "list":
		state, err := ws.State(ctx)
		if err != nil {
			return renderOperationalError(runtime, parsed, target, err)
		}
		items := filterRecoveries(state.Items, target.relativeFile)
		if parsed.json {
			document := baseJSON("recovery list", target)
			recoveries := recoveriesJSON(items)
			document.Recoveries = &recoveries
			document.Summary = &jsonSummary{Recoveries: intPointer(len(items))}
			return writeJSONResult(runtime, document, exitSuccess)
		}
		if !parsed.quiet {
			if renderErr := renderRecoveriesHuman(runtime.stdout, stdoutStyle, items); renderErr != nil {
				return renderWriteError(runtime, renderErr)
			}
		}
		return exitSuccess
	case "restore":
		summary, err := ws.RestoreRecoveryScoped(ctx, parsed.phrases[0], allowed, nil)
		if err != nil && !summaryHasOutcome(summary) {
			return renderOperationalError(runtime, parsed, target, err)
		}
		return renderRecoveryMutation(runtime, parsed, stdoutStyle, stderrStyle, target, "recovery restore", summary, err)
	case "discard":
		err := ws.DiscardRecoveryScoped(ctx, parsed.phrases[0], allowed)
		if err != nil {
			return renderOperationalError(runtime, parsed, target, err)
		}
		if parsed.json {
			document := baseJSON("recovery discard", target)
			document.Summary = &jsonSummary{Succeeded: intPointer(1)}
			return writeJSONResult(runtime, document, exitSuccess)
		}
		if !parsed.quiet {
			if writeErr := writeAll(runtime.stdout, []byte(fmt.Sprintf("%s id=%s\n", stdoutStyle.success("DISCARDED"), sanitizeCLI(parsed.phrases[0])))); writeErr != nil {
				return renderWriteError(runtime, writeErr)
			}
		}
		return exitSuccess
	default:
		return exitUsage
	}
}

func renderRecoveryMutation(runtime runtimeIO, parsed invocation, stdoutStyle, stderrStyle outputStyle, target resolvedTarget, action string, summary workspace.MutationSummary, operationErr error) int {
	code := mutationExitCode(summary, operationErr != nil)
	if parsed.json {
		document := baseJSON(action, target)
		addMutationJSON(&document, summary)
		if operationErr != nil {
			document.Error = operationErr.Error()
		}
		document.Status = statusForExit(code)
		return writeJSONResult(runtime, document, code)
	}
	if !parsed.quiet {
		if renderErr := renderMutationHuman(runtime.stdout, runtime.stderr, stdoutStyle, stderrStyle, action, summary); renderErr != nil {
			return renderWriteError(runtime, renderErr)
		}
		if operationErr != nil {
			if writeErr := writeAll(runtime.stderr, []byte("subedit: "+sanitizeCLI(operationErr.Error())+"\n")); writeErr != nil {
				return renderWriteError(runtime, writeErr)
			}
		}
		if renderErr := renderBlockingRecoveryCommands(runtime.stdout, runtime.stderr, stdoutStyle, stderrStyle, target, summary.BlockingRecoveryID); renderErr != nil {
			return renderWriteError(runtime, renderErr)
		}
	}
	return code
}

func filterRecoveries(items []workspace.Recovery, file string) []workspace.Recovery {
	if file == "" {
		return items
	}
	filtered := make([]workspace.Recovery, 0)
	for _, item := range items {
		// File-scoped recovery intentionally hides multi-file transactions. The
		// corresponding destructive scoped APIs apply the same exact-set rule,
		// so list never advertises an operation the target cannot safely resolve.
		if len(item.FilesList) == 1 && item.FilesList[0] == file {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func allowedPaths(target resolvedTarget) []string {
	if target.relativeFile == "" {
		return nil
	}
	return []string{target.relativeFile}
}

func resultExitCode(matches int, issues []workspace.Issue) int {
	if len(issues) > 0 {
		return exitPartial
	}
	if matches == 0 {
		return exitEmpty
	}
	return exitSuccess
}

func mutationExitCode(summary workspace.MutationSummary, discoveryIssues bool) int {
	if summary.Cancelled {
		return exitInterrupted
	}
	if discoveryIssues || summary.BlockingRecoveryID != "" || summary.Skipped > 0 || summary.Conflicted > 0 || summary.Failed > 0 || summary.NotAttempted > 0 {
		return exitPartial
	}
	return exitSuccess
}

func summaryHasOutcome(summary workspace.MutationSummary) bool {
	return len(summary.Results) > 0 || summary.Succeeded > 0 || summary.Restored > 0 ||
		summary.Cancelled || summary.UndoID != "" || summary.BlockingRecoveryID != "" || summary.RecoveryRetained
}

func statusForExit(code int) string {
	switch code {
	case exitSuccess:
		return "ok"
	case exitEmpty:
		return "empty"
	case exitPartial:
		return "partial"
	case exitInterrupted:
		return "interrupted"
	default:
		return "error"
	}
}

func renderOperationalError(runtime runtimeIO, parsed invocation, target resolvedTarget, err error) int {
	code := exitOperational
	status := "error"
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = exitInterrupted
		status = "interrupted"
	}
	if parsed.json {
		command := string(parsed.command)
		if parsed.command == commandRecovery {
			command += " " + parsed.recoveryAction
		}
		document := baseJSON(command, target)
		document.Status = status
		document.Error = err.Error()
		return writeJSONResult(runtime, document, code)
	}
	if !parsed.quiet {
		_ = writeAll(runtime.stderr, []byte("subedit: "+sanitizeCLI(err.Error())+"\n"))
	}
	return code
}

func writeJSONResult(runtime runtimeIO, document jsonDocument, desiredCode int) int {
	if err := writeJSON(runtime.stdout, document); err != nil {
		_ = writeAll(runtime.stderr, []byte("subedit: write JSON: "+sanitizeCLI(err.Error())+"\n"))
		return exitOperational
	}
	return desiredCode
}

func renderWriteError(runtime runtimeIO, err error) int {
	// Best effort only: the diagnostic stream may be the same broken pipe.
	_ = writeAll(runtime.stderr, []byte("subedit: write output: "+sanitizeCLI(err.Error())+"\n"))
	return exitOperational
}

func userQuitRequested(model tea.Model) bool {
	switch final := model.(type) {
	case tui.Model:
		return final.UserQuitRequested()
	case *tui.Model:
		return final != nil && final.UserQuitRequested()
	default:
		return false
	}
}

func commandName(parsed invocation) string {
	if parsed.command != commandRecovery {
		return string(parsed.command)
	}
	return strings.TrimSpace(string(parsed.command) + " " + parsed.recoveryAction)
}

func unresolvedTarget(parsed invocation, workingDirectory string) resolvedTarget {
	if parsed.target == targetDirectory {
		path := parsed.targetValue
		if !filepath.IsAbs(path) {
			path = filepath.Join(workingDirectory, path)
		}
		return resolvedTarget{root: filepath.Clean(path)}
	}
	path := parsed.targetValue
	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDirectory, path)
	}
	path = filepath.Clean(path)
	return resolvedTarget{root: filepath.Dir(path), relativeFile: filepath.Base(path)}
}
