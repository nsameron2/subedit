package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"subedit/internal/subtitle"
)

var errHelp = errors.New("help requested")

type commandKind string

const (
	commandTUI      commandKind = "tui"
	commandScan     commandKind = "scan"
	commandSearch   commandKind = "search"
	commandRemove   commandKind = "remove"
	commandUndo     commandKind = "undo"
	commandRecovery commandKind = "recovery"
	commandHelp     commandKind = "help"
	commandVersion  commandKind = "version"
)

type targetKind string

const (
	targetDirectory targetKind = "directory"
	targetFile      targetKind = "file"
)

type invocation struct {
	command        commandKind
	recoveryAction string
	target         targetKind
	targetValue    string
	tuiDirectory   string
	phrases        []string
	json           bool
	quiet          bool
	dryRun         bool
	helpTopic      string
}

type resolvedTarget struct {
	root         string
	relativeFile string
}

func parseInvocation(args []string) (invocation, error) {
	if len(args) == 0 {
		return invocation{command: commandTUI}, nil
	}
	switch args[0] {
	case "-h", "--help":
		if len(args) != 1 {
			return invocation{}, fmt.Errorf("--help does not accept arguments")
		}
		return invocation{command: commandHelp}, nil
	case "--version", "version":
		if len(args) != 1 {
			return invocation{}, fmt.Errorf("%s does not accept arguments", args[0])
		}
		return invocation{command: commandVersion}, nil
	case "help":
		if len(args) > 3 {
			return invocation{}, fmt.Errorf("help accepts at most a command and subcommand")
		}
		parsed := invocation{command: commandHelp}
		if len(args) >= 2 {
			parsed.helpTopic = strings.Join(args[1:], " ")
			if !validHelpTopic(parsed.helpTopic) {
				return invocation{}, fmt.Errorf("unknown help topic %q", parsed.helpTopic)
			}
		}
		return parsed, nil
	case "tui":
		return parseTUIInvocation(args[1:])
	case "scan", "search", "remove", "undo", "recovery":
		return parseHeadlessInvocation(commandKind(args[0]), args[1:])
	default:
		// Backward compatibility: a single unflagged positional path launches
		// the TUI exactly as pre-command versions did.
		if strings.HasPrefix(args[0], "-") {
			return invocation{}, fmt.Errorf("unknown option %q", args[0])
		}
		if len(args) > 1 {
			return invocation{}, fmt.Errorf("expected one TUI directory; headless commands require an explicit command")
		}
		return invocation{command: commandTUI, tuiDirectory: args[0]}, nil
	}
}

func parseTUIInvocation(args []string) (invocation, error) {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		return invocation{command: commandHelp, helpTopic: "tui"}, nil
	}
	if len(args) > 1 {
		return invocation{}, fmt.Errorf("tui accepts at most one directory")
	}
	parsed := invocation{command: commandTUI}
	if len(args) == 1 {
		if strings.HasPrefix(args[0], "-") {
			return invocation{}, fmt.Errorf("unknown tui option %q", args[0])
		}
		parsed.tuiDirectory = args[0]
	}
	return parsed, nil
}

func parseHeadlessInvocation(command commandKind, args []string) (invocation, error) {
	parsed := invocation{command: command}
	if command == commandRecovery {
		if len(args) == 0 {
			return invocation{}, errors.New("recovery requires list, restore, or discard")
		}
		if args[0] == "-h" || args[0] == "--help" {
			if len(args) != 1 {
				return invocation{}, errors.New("recovery --help does not accept arguments")
			}
			return invocation{command: commandHelp, helpTopic: "recovery"}, nil
		}
		parsed.recoveryAction = args[0]
		switch parsed.recoveryAction {
		case "list", "restore", "discard":
		default:
			return invocation{}, fmt.Errorf("unknown recovery action %q", parsed.recoveryAction)
		}
		args = args[1:]
	}

	positionals := make([]string, 0, len(args))
	flagsDone := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !flagsDone && arg == "--" {
			flagsDone = true
			continue
		}
		if !flagsDone && strings.HasPrefix(arg, "-") && arg != "-" {
			name, value, hasValue := splitOption(arg)
			switch name {
			case "-h", "--help":
				if hasValue {
					return invocation{}, fmt.Errorf("%s does not accept a value", name)
				}
				topic := string(command)
				if command == commandRecovery {
					topic += " " + parsed.recoveryAction
				}
				return invocation{command: commandHelp, helpTopic: topic}, nil
			case "-d", "--directory", "-f", "--file":
				if !hasValue {
					index++
					if index >= len(args) {
						return invocation{}, fmt.Errorf("%s requires a path", name)
					}
					value = args[index]
					if strings.HasPrefix(value, "-") {
						return invocation{}, fmt.Errorf("%s requires a path; use %s=PATH for a path beginning with '-'", name, name)
					}
				}
				if value == "" {
					return invocation{}, fmt.Errorf("%s requires a nonempty path", name)
				}
				kind := targetDirectory
				if name == "-f" || name == "--file" {
					kind = targetFile
				}
				if parsed.target != "" {
					return invocation{}, errors.New("exactly one -d/--directory or -f/--file must be provided")
				}
				parsed.target, parsed.targetValue = kind, value
			case "--json":
				if hasValue {
					return invocation{}, errors.New("--json does not accept a value")
				}
				parsed.json = true
			case "-q", "--quiet":
				if hasValue {
					return invocation{}, fmt.Errorf("%s does not accept a value", name)
				}
				parsed.quiet = true
			case "--dry-run":
				if hasValue {
					return invocation{}, errors.New("--dry-run does not accept a value")
				}
				parsed.dryRun = true
			default:
				return invocation{}, fmt.Errorf("unknown option %q", name)
			}
			continue
		}
		positionals = append(positionals, arg)
	}

	if parsed.target == "" {
		return invocation{}, errors.New("exactly one -d/--directory or -f/--file is required")
	}
	if parsed.json && parsed.quiet {
		return invocation{}, errors.New("--json and --quiet are mutually exclusive")
	}
	if parsed.dryRun && command != commandRemove {
		return invocation{}, errors.New("--dry-run is only valid with remove")
	}

	switch command {
	case commandSearch, commandRemove:
		if len(positionals) == 0 {
			return invocation{}, fmt.Errorf("%s requires at least one phrase", command)
		}
		for _, phrase := range positionals {
			if subtitle.NormalizeQuery(phrase) == "" {
				return invocation{}, fmt.Errorf("%s phrases must not normalize to empty", command)
			}
		}
		parsed.phrases = positionals
	case commandScan, commandUndo:
		if len(positionals) != 0 {
			return invocation{}, fmt.Errorf("%s does not accept positional arguments", command)
		}
	case commandRecovery:
		switch parsed.recoveryAction {
		case "list":
			if len(positionals) != 0 {
				return invocation{}, errors.New("recovery list does not accept an ID")
			}
		case "restore", "discard":
			if len(positionals) != 1 {
				return invocation{}, fmt.Errorf("recovery %s requires exactly one transaction ID", parsed.recoveryAction)
			}
			parsed.phrases = positionals // one opaque recovery ID
		}
	}
	return parsed, nil
}

func splitOption(arg string) (name, value string, hasValue bool) {
	if before, after, found := strings.Cut(arg, "="); found {
		return before, after, true
	}
	return arg, "", false
}

func validHelpTopic(topic string) bool {
	switch topic {
	case "tui", "scan", "search", "remove", "undo", "recovery",
		"recovery list", "recovery restore", "recovery discard":
		return true
	default:
		return false
	}
}

func (i invocation) resolveTarget(workingDirectory string) (resolvedTarget, error) {
	switch i.target {
	case targetDirectory:
		root, err := resolveDirectory(i.targetValue, workingDirectory)
		return resolvedTarget{root: root}, err
	case targetFile:
		path := i.targetValue
		if !filepath.IsAbs(path) {
			path = filepath.Join(workingDirectory, path)
		}
		path = filepath.Clean(path)
		base := filepath.Base(path)
		if base == "." || base == string(filepath.Separator) || base == "" {
			return resolvedTarget{}, fmt.Errorf("invalid file target %q", i.targetValue)
		}
		if !subtitle.SupportedExtension(base) {
			return resolvedTarget{}, fmt.Errorf("unsupported subtitle extension for --file %q (expected .srt, .ass, or .vtt)", i.targetValue)
		}
		root, err := resolveDirectory(filepath.Dir(path), workingDirectory)
		if err != nil {
			return resolvedTarget{}, fmt.Errorf("resolve parent of file %q: %w", i.targetValue, err)
		}
		return resolvedTarget{root: root, relativeFile: filepath.ToSlash(base)}, nil
	default:
		return resolvedTarget{}, errors.New("headless command has no target")
	}
}

func resolveDirectory(directory, workingDirectory string) (string, error) {
	if directory == "" {
		directory = workingDirectory
	}
	if !filepath.IsAbs(directory) {
		directory = filepath.Join(workingDirectory, directory)
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve directory: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve directory %q: %w", directory, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect directory %q: %w", directory, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", directory)
	}
	return filepath.Clean(canonical), nil
}

// parseRootArg is retained as the narrow legacy TUI path parser.
func parseRootArg(args []string, workingDirectory string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("expected at most one directory, got %d", len(args))
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		return "", errHelp
	}
	if len(args) == 1 && strings.HasPrefix(args[0], "-") {
		return "", fmt.Errorf("unknown option %q", args[0])
	}
	directory := ""
	if len(args) == 1 {
		directory = args[0]
	}
	return resolveDirectory(directory, workingDirectory)
}
