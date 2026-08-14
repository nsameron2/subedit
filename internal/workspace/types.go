// Package workspace discovers subtitle files and applies recoverable,
// conflict-checked edits to them.
package workspace

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"subedit/internal/subtitle"
)

const (
	// MaxFileSize is the largest subtitle file subedit will parse or modify.
	MaxFileSize int64 = 64 << 20
	RecoveryDir       = ".subedit-recovery"
)

var (
	ErrClosed            = errors.New("workspace is closed")
	ErrBusy              = errors.New("workspace already has an active operation")
	ErrMutationLocked    = errors.New("another subedit process is modifying this workspace")
	ErrRecoveryPending   = errors.New("unresolved recovery data must be restored or discarded first")
	ErrNoUndo            = errors.New("there is no operation to undo")
	ErrUndoChanged       = errors.New("the current undo point changed")
	ErrRecoveryScope     = errors.New("recovery transaction is outside the allowed file scope")
	ErrRecoveryState     = errors.New("recovery state is invalid")
	ErrStalePlan         = errors.New("mutation plan is based on a stale discovery")
	ErrConflict          = errors.New("file changed since it was indexed")
	ErrUnsafePath        = errors.New("unsafe path")
	ErrUnsafeFile        = errors.New("unsafe file")
	ErrHardlink          = errors.New("hard-linked file")
	ErrUnsupportedFile   = errors.New("unsupported subtitle file")
	ErrTransactionClosed = errors.New("transaction is already finished")
)

// IssueKind classifies a file that discovery deliberately did not index.
type IssueKind string

const (
	IssueUnreadable IssueKind = "unreadable"
	IssueInvalid    IssueKind = "invalid"
	IssueTooLarge   IssueKind = "too-large"
	IssueSymlink    IssueKind = "symlink"
	IssueHardlink   IssueKind = "hardlink"
	IssueUnsafe     IssueKind = "unsafe"
)

// Issue describes one supported-looking file that was excluded from edits.
type Issue struct {
	RelativePath string
	Kind         IssueKind
	Size         int64
	Err          error
}

func (i Issue) Error() string {
	if i.Err == nil {
		return fmt.Sprintf("%s: %s", i.RelativePath, i.Kind)
	}
	return fmt.Sprintf("%s: %v", i.RelativePath, i.Err)
}

// File is one safely indexed subtitle file. Document is immutable.
type File struct {
	RelativePath string
	Size         int64
	Mode         fs.FileMode
	ModTime      time.Time
	SHA256       [sha256.Size]byte
	Identity     FileIdentity
	Document     *subtitle.Document
}

// FileIdentity is an opaque, comparable filesystem object identity. On Unix
// it contains the device and inode; on Windows it contains the volume serial
// number and file index. It distinguishes replacement files even when their
// path and contents are identical.
type FileIdentity struct {
	kind   string
	volume uint64
	object uint64
}

// Valid reports whether the identity was captured from an open file handle.
func (id FileIdentity) Valid() bool { return id.kind != "" }

// Discovery is an immutable snapshot of a recursive scan.
type Discovery struct {
	Root     string
	Revision uint64
	Files    []File
	Issues   []Issue
}

// DiscoveryProgress reports one supported file after it has either been
// indexed or classified as an issue. Callbacks are serialized even though
// candidate parsing is concurrent. Issue is nil for a successfully indexed
// file and points to a per-callback copy otherwise.
type DiscoveryProgress struct {
	Completed   int
	Total       int
	CurrentPath string
	Issue       *Issue
}

// FileMatch contains all matching cues in one indexed file.
type FileMatch struct {
	File       File
	Cues       []subtitle.Cue
	CueMatches []CueMatch
}

// CueMatch records which normalized SearchResult phrases matched one cue.
// QueryIndexes index SearchResult.Queries and NormalizedQueries.
type CueMatch struct {
	Cue          subtitle.Cue
	QueryIndexes []int
}

// SearchResult is a query evaluated against one Discovery revision.
type SearchResult struct {
	Query             string
	NormalizedQuery   string
	Queries           []string
	NormalizedQueries []string
	Revision          uint64
	TotalFiles        int
	SkippedFiles      int
	MatchingCues      int
	Matches           []FileMatch
}

// DeleteScope records which UI command produced a transaction.
type DeleteScope string

const (
	DeleteAll      DeleteScope = "all"
	DeleteSelected DeleteScope = "selected"
	DeleteFocused  DeleteScope = "focused"
	DeleteEditor   DeleteScope = "editor"
)

// FileMutation ties cue IDs to the exact version from which they were found.
type FileMutation struct {
	RelativePath     string
	ExpectedSHA256   [sha256.Size]byte
	ExpectedIdentity FileIdentity
	CueIDs           []subtitle.CueID
}

// MutationPlan is a snapshot-safe batch deletion request.
type MutationPlan struct {
	SearchRevision uint64
	Scope          DeleteScope
	Files          []FileMutation
}

// Plan returns a plan for all matches, or only paths present in selected.
// A nil selected map means all matches.
func (s SearchResult) Plan(scope DeleteScope, selected map[string]bool) MutationPlan {
	plan := MutationPlan{SearchRevision: s.Revision, Scope: scope}
	for _, match := range s.Matches {
		if selected != nil && !selected[match.File.RelativePath] {
			continue
		}
		ids := make([]subtitle.CueID, len(match.Cues))
		for i := range match.Cues {
			ids[i] = match.Cues[i].ID
		}
		plan.Files = append(plan.Files, FileMutation{
			RelativePath:     match.File.RelativePath,
			ExpectedSHA256:   match.File.SHA256,
			ExpectedIdentity: match.File.Identity,
			CueIDs:           ids,
		})
	}
	return plan
}

type FileStatus string

const (
	FileSucceeded    FileStatus = "succeeded"
	FileSkipped      FileStatus = "skipped"
	FileConflicted   FileStatus = "conflicted"
	FileFailed       FileStatus = "failed"
	FileRestored     FileStatus = "restored"
	FileNotAttempted FileStatus = "not-attempted"
)

// FileResult is the durable outcome for one requested file.
type FileResult struct {
	RelativePath string
	Status       FileStatus
	DeletedCues  int
	Warnings     []string
	Err          error
}

// MutationSummary separates partial success, conflicts, failures, and
// cancellation so callers never need to infer safety from a single error.
type MutationSummary struct {
	TransactionID string
	UndoID        string
	// BlockingRecoveryID is the authoritative pending transaction that must
	// be restored or discarded before another mutation. It is empty for an
	// ordinary current undo and for garbage-only cleanup retention.
	BlockingRecoveryID string
	Results            []FileResult
	Succeeded          int
	Restored           int
	Skipped            int
	Conflicted         int
	Failed             int
	NotAttempted       int
	DeletedCues        int
	Cancelled          bool
	UndoAvailable      bool
	RecoveryRetained   bool
}

func (s *MutationSummary) add(result FileResult) {
	s.Results = append(s.Results, result)
	s.DeletedCues += result.DeletedCues
	switch result.Status {
	case FileSucceeded:
		s.Succeeded++
	case FileRestored:
		s.Restored++
	case FileSkipped:
		s.Skipped++
	case FileConflicted:
		s.Conflicted++
	case FileFailed:
		s.Failed++
	case FileNotAttempted:
		s.NotAttempted++
	}
}

// Progress is emitted after each atomic file attempt.
type Progress struct {
	Completed int
	Total     int
	Result    FileResult
}

// Recovery describes a durable transaction left on disk.
type Recovery struct {
	ID             string
	Role           RecoveryRole
	CreatedAt      time.Time
	Status         string
	Files          int
	Changed        int
	Corrupt        bool
	Err            error
	FilesList      []string
	BlocksMutation bool
}

// RecoveryRole describes how a transaction relates to durable workspace
// state. Exactly one transaction may have RoleUndo and one RolePending.
type RecoveryRole string

const (
	RecoveryRoleUndo    RecoveryRole = "undo"
	RecoveryRolePending RecoveryRole = "pending"
	RecoveryRoleOrphan  RecoveryRole = "orphan"
	RecoveryRoleGarbage RecoveryRole = "garbage"
	RecoveryRoleCorrupt RecoveryRole = "corrupt"
)

// UndoInfo is a stable snapshot of the persistent one-level undo point.
type UndoInfo struct {
	ID           string
	CreatedAt    time.Time
	Files        []string
	Partial      bool
	BlocksRemove bool
}

// RecoveryState is the reconciled durable recovery state for a workspace.
type RecoveryState struct {
	Generation uint64
	Undo       *UndoInfo
	Items      []Recovery
	Blocked    bool
}
