// Package tui implements subedit's keyboard-only Bubble Tea user interface.
//
// It deliberately depends on a small, asynchronous Backend contract. The
// command package is expected to adapt the workspace and subtitle packages to
// these presentation-oriented types.
package tui

import (
	"context"
	"errors"
	"time"
)

// Backend supplies discovery, searching, and safe mutations. Every returned
// channel must eventually be closed. Long-running operations should send a
// final event with Done set; closed channels are also treated as completion.
// Implementations must honor cancellation between atomic file replacements.
type Backend interface {
	Discover(context.Context) <-chan DiscoveryEvent
	Search(context.Context, SearchRequest) <-chan SearchEvent
	Mutate(context.Context, MutationRequest) <-chan MutationEvent
	Undo(context.Context) <-chan MutationEvent
}

// RecoveryBackend is an optional extension implemented by backends with
// crash-recovery support. New checks it before starting normal discovery.
type RecoveryBackend interface {
	ListRecoveries(context.Context) ([]RecoveryItem, error)
	ResolveRecovery(context.Context, RecoveryRequest) <-chan RecoveryEvent
}

// EditorBackend is an optional, on-demand single-file editor extension. Open
// returns every cue for one already-discovered file behind an opaque snapshot.
// Search filters that exact snapshot (an empty query returns every cue), while
// Refresh rereads one exact path and mints a new snapshot after the previous
// token has been consumed by a mutation. Undo is restricted to the exact undo
// transaction and path supplied by the editor.
type EditorBackend interface {
	OpenEditor(context.Context, EditorOpenRequest) <-chan EditorOpenEvent
	SearchEditor(context.Context, EditorSearchRequest) <-chan EditorSearchEvent
	RefreshEditor(context.Context, EditorRefreshRequest) <-chan EditorOpenEvent
	UndoEditor(context.Context, EditorUndoRequest) <-chan MutationEvent
}

// Options configures the initial application model.
type Options struct {
	Root         string
	InitialQuery string
	// InitialUndoAvailable exposes a validated durable undo point from this
	// workspace. It does not participate in crash-recovery startup.
	InitialUndoAvailable bool
	// InitialRetainedUndoID is set only when that durable undo is partial and
	// must be retried or explicitly discarded before another delete. Supplying
	// it implies InitialUndoAvailable.
	InitialRetainedUndoID string
	Width                 int
	Height                int
	// DisableColor produces deterministic, accessible output without ANSI
	// color. Bold and borders remain available as non-color indicators.
	DisableColor bool
}

// File is one rendered result card. For an empty search Preview contains the
// first cues; for an active search it contains matching cues only.
type File struct {
	ID         string
	Path       string
	Valid      bool
	Error      string
	Preview    []Cue
	MatchIDs   []string
	MatchCount int
}

// Cue is one preview row.
type Cue struct {
	ID        string
	Timestamp string
	Text      string
}

// EditorOpenRequest opens one valid file from the backend's current discovery
// snapshot. Revision is a UI request generation echoed by EditorOpenEvent so a
// cancelled or superseded asynchronous response can be ignored safely.
type EditorOpenRequest struct {
	FileID   string
	Path     string
	Revision uint64
}

// EditorRefreshRequest rereads one exact editor path after a save, undo, or
// explicit reload. It intentionally carries no snapshot token: a mutation
// consumes the old token, and a successful refresh returns a new one.
type EditorRefreshRequest struct {
	Path     string
	Revision uint64
}

// EditorDocument is a complete, source-ordered cue snapshot for one file.
// SnapshotID is opaque, non-empty, and valid only until it is consumed by a
// mutation or invalidated by the backend.
type EditorDocument struct {
	FileID     string
	Path       string
	SnapshotID string
	Cues       []Cue
}

// EditorOpenEvent is shared by initial open and exact-file refresh streams.
// Document is required on a successful final event.
type EditorOpenEvent struct {
	Revision uint64
	Document *EditorDocument
	Err      error
	Done     bool
}

// EditorSearchRequest filters one exact editor snapshot. Query is editor-local
// and does not mutate the outer workspace search input or revision.
type EditorSearchRequest struct {
	SnapshotID string
	Query      string
	Revision   uint64
}

// EditorSearchEvent returns source-ordered cue IDs visible for the query. An
// empty query must return every cue ID. Partial events may replace the prior
// visible set; the final event must contain the complete result.
type EditorSearchEvent struct {
	Revision   uint64
	SnapshotID string
	Query      string
	CueIDs     []string
	Err        error
	Done       bool
}

// EditorUndoRequest scopes one-level undo to both the exact transaction and
// the editor path. Revision is carried privately through the mutation stream by
// the model to reject stale completions.
type EditorUndoRequest struct {
	Path     string
	UndoID   string
	Revision uint64
}

// Discovery is a complete, deterministically sorted workspace snapshot.
type Discovery struct {
	Files   []File
	Skipped int
}

// DiscoveryEvent reports startup or rescan progress. Discovery should be set
// on the final event. CurrentPath is display-only.
type DiscoveryEvent struct {
	Completed   int
	Total       int
	CurrentPath string
	Discovery   *Discovery
	Err         error
	Done        bool
}

// SearchRequest identifies the input revision so stale results can be safely
// ignored by both the backend and UI.
type SearchRequest struct {
	Query    string
	Revision uint64
}

// SearchResult contains only files matching a non-empty query.
type SearchResult struct {
	Query         string
	Revision      uint64
	Files         []File
	MatchingCues  int
	MatchingFiles int
	TotalFiles    int
	Skipped       int
}

// SearchEvent allows a backend to stream partial results. The TUI renders the
// newest event for the exact current revision and ignores all stale events.
type SearchEvent struct {
	Result SearchResult
	Err    error
	Done   bool
}

// DeleteScope identifies the destructive command that created a plan.
type DeleteScope string

const (
	DeleteAll      DeleteScope = "all"
	DeleteFocused  DeleteScope = "focused"
	DeleteSelected DeleteScope = "selected"
	DeleteEditor   DeleteScope = "editor"
)

// MutationSource identifies the reviewed snapshot that authorizes a delete.
// The zero value is the legacy/root search source for compatibility.
type MutationSource string

const (
	MutationSourceSearch MutationSource = ""
	MutationSourceEditor MutationSource = "editor"
)

// MutationTarget snapshots the file and exact matching cue IDs shown during
// confirmation. Backends should reject stale hashes/identities independently.
type MutationTarget struct {
	FileID string
	Path   string
	CueIDs []string
}

// MutationRequest is a revision-pinned batch deletion plan.
type MutationRequest struct {
	Scope      DeleteScope
	Query      string
	Revision   uint64
	Source     MutationSource
	SnapshotID string
	Targets    []MutationTarget
}

// MutationProgress is emitted after each atomic file attempt. Completed and
// Total are absolute; Succeeded, Skipped, and Failed are deltas for this event.
type MutationProgress struct {
	Completed   int
	Total       int
	CurrentPath string
	Succeeded   int
	Skipped     int
	Failed      int
}

// MutationSummary is the durable outcome of delete or undo. Backends must
// preserve the prior undo point when a delete changes no files and report its
// availability. RecoveryID is reserved for a transaction that actively gates
// another delete; the model preserves its last known state defensively.
type MutationSummary struct {
	Operation string
	// UndoID is the exact durable undo transaction created by this operation.
	// The editor uses it with EditorBackend.UndoEditor so it cannot undo a newer
	// or unrelated workspace transaction.
	UndoID string
	// RecoveryID identifies the exact retained transaction described by
	// RecoveryKind. While set, the TUI gates new deletion operations until that
	// transaction is retried or explicitly discarded with confirmation.
	// For compatibility, an empty RecoveryKind with a non-empty RecoveryID is
	// treated as RecoveryGateUndo.
	RecoveryID    string
	RecoveryKind  RecoveryGateKind
	Succeeded     int
	Skipped       int
	Failed        int
	Cancelled     bool
	NotAttempted  int
	Warnings      []string
	UndoAvailable bool
}

// RecoveryGateKind determines how a retained transaction must be retried.
// A partial undo uses Backend.Undo; a pending apply uses
// RecoveryBackend.ResolveRecovery with RecoveryRestore.
type RecoveryGateKind string

const (
	RecoveryGateUndo  RecoveryGateKind = "undo"
	RecoveryGateApply RecoveryGateKind = "apply"
)

// MutationEvent streams sequential mutation progress and a final summary.
type MutationEvent struct {
	Progress *MutationProgress
	Summary  *MutationSummary
	Err      error
	Done     bool
}

// RecoveryItem describes one incomplete transaction, newest first.
type RecoveryItem struct {
	ID        string
	CreatedAt time.Time
	Files     int
	Summary   string
}

// RecoveryAction is a safe startup resolution.
type RecoveryAction string

const (
	RecoveryRestore RecoveryAction = "restore"
	RecoveryDiscard RecoveryAction = "discard"
)

// RecoveryRequest resolves exactly one transaction.
type RecoveryRequest struct {
	ID     string
	Action RecoveryAction
}

// RecoveryProgress and RecoveryEvent support the same sequential progress UI
// used by normal mutations.
type RecoveryProgress struct {
	Completed   int
	Total       int
	CurrentPath string
}

type RecoveryEvent struct {
	Progress *RecoveryProgress
	Summary  *RecoverySummary
	Err      error
	Done     bool
}

// UndoSnapshot is an authoritative view of the workspace's durable one-level
// undo point after a recovery operation. RetainedUndoID is set only when that
// undo point is partial and must gate new deletes. A non-empty RetainedUndoID
// implies Available.
type UndoSnapshot struct {
	Available      bool
	RetainedUndoID string
}

// RecoverySummary tells the startup gate whether a restore fully resolved the
// transaction. Retained must be true for partial/conflicted recovery sets.
// Undo is optional: nil preserves the model's last known undo state, while a
// non-nil snapshot replaces it. Backends should populate it from durable state
// after every recovery restore or discard.
type RecoverySummary struct {
	Succeeded int
	Skipped   int
	Failed    int
	Retained  bool
	Undo      *UndoSnapshot
}

// ErrClosedStream is shown when a backend closes an operation stream without
// a final event or result.
var ErrClosedStream = errors.New("backend operation ended without a final result")

// State is the root UI state machine.
type State uint8

const (
	StateRecovery State = iota
	StateDiscovery
	StateSearch
	StateResults
	StateSelection
	StateEditorLoading
	StateEditor
	StateEditorSearch
	StateConfirmation
	StateMutation
	StateSummary
)

func (s State) String() string {
	switch s {
	case StateRecovery:
		return "recovery"
	case StateDiscovery:
		return "discovery"
	case StateSearch:
		return "search"
	case StateResults:
		return "results"
	case StateSelection:
		return "selection"
	case StateEditorLoading:
		return "editor-loading"
	case StateEditor:
		return "editor"
	case StateEditorSearch:
		return "editor-search"
	case StateConfirmation:
		return "confirmation"
	case StateMutation:
		return "mutation"
	case StateSummary:
		return "summary"
	default:
		return "unknown"
	}
}
