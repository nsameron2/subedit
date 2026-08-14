// Package subtitle parses, searches, and source-preservingly edits subtitle files.
//
// The package deliberately models deletions as operations on the original byte
// stream. Parsing creates semantic cue indexes, but rendering removes only the
// indexed byte spans (and, for SRT, rewrites cue-number tokens). This keeps
// headers, comments, line endings, whitespace, and unrelated formatting intact.
package subtitle

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// Format is a supported subtitle container format.
type Format string

const (
	FormatSRT    Format = "srt"
	FormatASS    Format = "ass"
	FormatWebVTT Format = "webvtt"
)

// Encoding is the source file's character encoding.
type Encoding string

const (
	EncodingUTF8    Encoding = "utf-8"
	EncodingUTF8BOM Encoding = "utf-8-bom"
	EncodingUTF16LE Encoding = "utf-16le-bom"
	EncodingUTF16BE Encoding = "utf-16be-bom"
)

// CueID identifies a cue within one parsed version of a file. Cue IDs are not
// persistent identifiers: callers must reparse a file after it changes.
type CueID string

// ByteSpan is a half-open [Start, End) range in the original encoded file.
type ByteSpan struct {
	Start int
	End   int
}

// Cue is a timed, searchable subtitle unit. DeleteSpan covers the complete
// source record that is removed when the cue is deleted. IndexSpan is set only
// for SRT and identifies the numeric cue-token that may be renumbered.
type Cue struct {
	ID          CueID
	Start       time.Duration
	End         time.Duration
	DisplayText string
	SearchText  string
	DeleteSpan  ByteSpan
	IndexSpan   *ByteSpan
}

// Document is an immutable index over one exact version of a subtitle file.
// OriginalSHA256 lets callers cheaply associate a mutation plan with that
// version. The original bytes are held privately and never exposed mutably.
type Document struct {
	Format         Format
	Encoding       Encoding
	Cues           []Cue
	OriginalSHA256 [sha256.Size]byte

	raw            []byte
	indexed        []Cue
	sourceFormat   Format
	sourceEncoding Encoding
}

// ParseError describes invalid input while retaining the underlying cause.
type ParseError struct {
	Path   string
	Format Format
	Line   int
	Err    error
}

func (e *ParseError) Error() string {
	where := e.Path
	if where == "" {
		where = string(e.Format)
	}
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %v", where, e.Line, e.Err)
	}
	return fmt.Sprintf("%s: %v", where, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }
