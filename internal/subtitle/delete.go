package subtitle

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

type byteEdit struct {
	start       int
	end         int
	replacement []byte
}

// Delete returns a new source buffer with the requested whole cues removed.
// It never mutates the Document. Empty ids are an exact byte-for-byte no-op.
// When an SRT deletion occurs, all retained numeric cue IDs are renumbered from
// one while every other retained source byte remains unchanged.
func (d *Document) Delete(ids []CueID) ([]byte, error) {
	if d == nil {
		return nil, errors.New("nil subtitle document")
	}
	wanted, err := d.resolveCueIDs(ids)
	if err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		return bytes.Clone(d.raw), nil
	}

	edits := make([]byteEdit, 0, len(wanted)+len(d.indexed))
	nextIndex := 1
	for _, cue := range d.indexed {
		if _, remove := wanted[cue.ID]; remove {
			edits = append(edits, byteEdit{start: cue.DeleteSpan.Start, end: cue.DeleteSpan.End})
			continue
		}
		if d.sourceFormat == FormatSRT {
			if cue.IndexSpan == nil {
				return nil, fmt.Errorf("SRT cue %q has no index span", cue.ID)
			}
			replacement, err := encodeReplacement(strconv.Itoa(nextIndex), d.sourceEncoding)
			if err != nil {
				return nil, err
			}
			edits = append(edits, byteEdit{
				start: cue.IndexSpan.Start, end: cue.IndexSpan.End, replacement: replacement,
			})
			nextIndex++
		}
	}
	return applyByteEdits(d.raw, edits)
}

func (d *Document) resolveCueIDs(ids []CueID) (map[CueID]struct{}, error) {
	available := make(map[CueID]struct{}, len(d.indexed))
	for _, cue := range d.indexed {
		available[cue.ID] = struct{}{}
	}
	wanted := make(map[CueID]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := available[id]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownCue, id)
		}
		wanted[id] = struct{}{}
	}
	return wanted, nil
}

func applyByteEdits(raw []byte, edits []byteEdit) ([]byte, error) {
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start == edits[j].start {
			return edits[i].end < edits[j].end
		}
		return edits[i].start < edits[j].start
	})
	var out bytes.Buffer
	out.Grow(len(raw))
	cursor := 0
	for _, edit := range edits {
		if edit.start < cursor || edit.end < edit.start || edit.end > len(raw) {
			return nil, fmt.Errorf("invalid or overlapping source edit [%d,%d)", edit.start, edit.end)
		}
		out.Write(raw[cursor:edit.start])
		out.Write(edit.replacement)
		cursor = edit.end
	}
	out.Write(raw[cursor:])
	return out.Bytes(), nil
}
