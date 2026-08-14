package subtitle

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var srtTimingLine = regexp.MustCompile(`^\s*(\S+)\s*-->\s*(\S+)(?:\s+.*)?$`)

func parseSRT(view sourceView) ([]Cue, error) {
	lines := splitSourceLines(view.text)
	cues := make([]Cue, 0)
	for i := 0; i < len(lines); {
		for i < len(lines) && isBlankLine(lines[i].text) {
			i++
		}
		if i == len(lines) {
			break
		}

		blockStart := i
		// Assign leading blank lines to the first cue. This avoids leaving an
		// invalid whitespace-only SRT when every cue is removed while preserving
		// those exact bytes whenever the first cue survives.
		if len(cues) == 0 {
			blockStart = 0
		}
		indexLine := lines[i]
		tokenStart, tokenEnd, ok := numericToken(indexLine.text)
		if !ok {
			return nil, lineError(indexLine.number, "expected a numeric SRT cue ID")
		}
		i++
		if i >= len(lines) || isBlankLine(lines[i].text) {
			return nil, lineError(indexLine.number, "SRT cue is missing a timing line")
		}
		timingLine := lines[i]
		matches := srtTimingLine.FindStringSubmatch(timingLine.text)
		if matches == nil {
			return nil, lineError(timingLine.number, "invalid SRT timing line")
		}
		start, err := parseClock(matches[1], false, ",.")
		if err != nil {
			return nil, lineWrap(timingLine.number, err)
		}
		end, err := parseClock(matches[2], false, ",.")
		if err != nil {
			return nil, lineWrap(timingLine.number, err)
		}
		if end < start {
			return nil, lineError(timingLine.number, "SRT cue ends before it starts")
		}
		i++
		payloadStart := i
		for i < len(lines) && !isBlankLine(lines[i].text) {
			i++
		}
		if payloadStart == i {
			return nil, lineError(timingLine.number, "SRT cue has no payload")
		}
		for j := payloadStart; j+1 < i; j++ {
			if _, _, numeric := numericToken(lines[j].text); numeric && srtTimingLine.MatchString(lines[j+1].text) {
				return nil, lineError(lines[j].number, "SRT cues must be separated by a blank line")
			}
		}
		payload := joinLineText(lines[payloadStart:i])

		// A cue owns the separator blank lines following it. This makes deleting
		// all cues leave only any intentionally leading whitespace (and the BOM).
		for i < len(lines) && isBlankLine(lines[i].text) {
			i++
		}
		blockEnd := lines[i-1].fullEnd
		deleteSpan, err := view.span(lines[blockStart].start, blockEnd)
		if err != nil {
			return nil, err
		}
		indexSpan, err := view.span(indexLine.start+tokenStart, indexLine.start+tokenEnd)
		if err != nil {
			return nil, err
		}
		display, search := cueText(projectMarkupText(payload, false))
		cue := Cue{
			ID:          makeCueID(FormatSRT, deleteSpan, len(cues)),
			Start:       start,
			End:         end,
			DisplayText: display,
			SearchText:  search,
			DeleteSpan:  deleteSpan,
			IndexSpan:   &indexSpan,
		}
		cues = append(cues, cue)
	}
	return cues, nil
}

func numericToken(line string) (start, end int, ok bool) {
	start = 0
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	end = start
	for end < len(line) && line[end] >= '0' && line[end] <= '9' {
		end++
	}
	if end == start {
		return 0, 0, false
	}
	for i := end; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return 0, 0, false
		}
	}
	return start, end, true
}

func isBlankLine(text string) bool { return strings.TrimSpace(text) == "" }

func joinLineText(lines []sourceLine) string {
	var out strings.Builder
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(line.text)
	}
	return out.String()
}

func makeCueID(format Format, span ByteSpan, ordinal int) CueID {
	return CueID(fmt.Sprintf("%s:%d:%d:%d", format, span.Start, span.End, ordinal))
}

func lineError(line int, message string) error {
	return &ParseError{Line: line, Err: errors.New(message)}
}

func lineWrap(line int, err error) error { return &ParseError{Line: line, Err: err} }
