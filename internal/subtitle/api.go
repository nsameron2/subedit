package subtitle

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

var (
	// ErrUnsupportedFormat means the path extension or explicit format is not
	// one of SRT, ASS, or WebVTT.
	ErrUnsupportedFormat = errors.New("unsupported subtitle format")
	// ErrUnknownCue means a deletion requested an ID outside this document.
	ErrUnknownCue = errors.New("unknown cue ID")
)

// SupportedExtension reports whether path has a supported extension. Extension
// comparison is ASCII case-insensitive.
func SupportedExtension(path string) bool {
	_, err := DetectFormat(path)
	return err == nil
}

// DetectFormat determines a subtitle format from a filename extension.
func DetectFormat(path string) (Format, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".srt":
		return FormatSRT, nil
	case ".ass":
		return FormatASS, nil
	case ".vtt":
		return FormatWebVTT, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedFormat, filepath.Ext(path))
	}
}

// Parse detects the format from path and parses raw. The returned Document owns
// a copy of raw, so the caller may safely reuse or mutate its input buffer.
func Parse(path string, raw []byte) (*Document, error) {
	format, err := DetectFormat(path)
	if err != nil {
		return nil, err
	}
	doc, err := parse(format, raw)
	if err != nil {
		return nil, annotateParseError(path, format, err)
	}
	return doc, nil
}

// ParseFormat parses raw as an explicitly selected format.
func ParseFormat(format Format, raw []byte) (*Document, error) {
	doc, err := parse(format, raw)
	if err != nil {
		return nil, annotateParseError("", format, err)
	}
	return doc, nil
}

func parse(format Format, raw []byte) (*Document, error) {
	owned := bytes.Clone(raw)
	view, encoding, err := decodeSource(format, owned)
	if err != nil {
		return nil, err
	}

	var cues []Cue
	switch format {
	case FormatSRT:
		cues, err = parseSRT(view)
	case FormatASS:
		cues, err = parseASS(view)
	case FormatWebVTT:
		cues, err = parseWebVTT(view)
	default:
		return nil, ErrUnsupportedFormat
	}
	if err != nil {
		return nil, err
	}
	return &Document{
		Format:         format,
		Encoding:       encoding,
		Cues:           cloneCues(cues),
		OriginalSHA256: sha256.Sum256(owned),
		raw:            owned,
		indexed:        cues,
		sourceFormat:   format,
		sourceEncoding: encoding,
	}, nil
}

func annotateParseError(path string, format Format, err error) error {
	var parseErr *ParseError
	if errors.As(err, &parseErr) {
		copyErr := *parseErr
		copyErr.Path = path
		copyErr.Format = format
		return &copyErr
	}
	return &ParseError{Path: path, Format: format, Err: err}
}

// Search returns each cue whose normalized visible text contains the normalized
// literal query. A query that normalizes to empty has no matches.
func (d *Document) Search(query string) []Cue {
	if d == nil {
		return nil
	}
	normalized := NormalizeQuery(query)
	if normalized == "" {
		return nil
	}
	matches := make([]Cue, 0)
	for _, cue := range d.indexed {
		if strings.Contains(cue.SearchText, normalized) {
			matches = append(matches, cloneCue(cue))
		}
	}
	return matches
}

func cloneCues(cues []Cue) []Cue {
	cloned := make([]Cue, len(cues))
	for i, cue := range cues {
		cloned[i] = cloneCue(cue)
	}
	return cloned
}

func cloneCue(cue Cue) Cue {
	if cue.IndexSpan != nil {
		span := *cue.IndexSpan
		cue.IndexSpan = &span
	}
	return cue
}

// OriginalBytes returns a defensive copy of the exact parsed source. It is
// primarily useful to identify and verify no-op renders.
func (d *Document) OriginalBytes() []byte {
	if d == nil {
		return nil
	}
	return bytes.Clone(d.raw)
}

// ValidateDeletion proves that rendered is the exact output this document
// would produce for ids and that the result is structurally parseable.
func (d *Document) ValidateDeletion(rendered []byte, ids []CueID) error {
	if d == nil {
		return errors.New("nil subtitle document")
	}
	expected, err := d.Delete(ids)
	if err != nil {
		return err
	}
	if !bytes.Equal(expected, rendered) {
		return errors.New("rendered bytes do not match deletion plan")
	}
	result, err := ParseFormat(d.sourceFormat, rendered)
	if err != nil {
		return fmt.Errorf("rendered subtitle is invalid: %w", err)
	}
	wanted, err := d.resolveCueIDs(ids)
	if err != nil {
		return err
	}
	if len(result.indexed) != len(d.indexed)-len(wanted) {
		return fmt.Errorf("rendered cue count is %d, want %d", len(result.indexed), len(d.indexed)-len(wanted))
	}
	return nil
}
