package subtitle

import (
	"fmt"
	"strings"
	"time"
)

func parseWebVTT(view sourceView) ([]Cue, error) {
	lines := splitSourceLines(view.text)
	if len(lines) == 0 {
		return nil, lineError(1, "WebVTT file is missing the WEBVTT header")
	}
	header := lines[0].text
	if header != "WEBVTT" && !strings.HasPrefix(header, "WEBVTT ") && !strings.HasPrefix(header, "WEBVTT\t") {
		return nil, lineError(lines[0].number, "invalid WebVTT header")
	}
	if len(lines) == 1 {
		return nil, nil
	}

	// Everything between the signature and first blank line is header metadata.
	i := 1
	for i < len(lines) && !isBlankLine(lines[i].text) {
		i++
	}
	if i == len(lines) {
		return nil, lineError(lines[len(lines)-1].number, "WebVTT header must be followed by a blank line")
	}
	for i < len(lines) && isBlankLine(lines[i].text) {
		i++
	}

	cues := make([]Cue, 0)
	for i < len(lines) {
		blockStart := i
		blockPayloadEnd := i
		for blockPayloadEnd < len(lines) && !isBlankLine(lines[blockPayloadEnd].text) {
			blockPayloadEnd++
		}
		for i = blockPayloadEnd; i < len(lines) && isBlankLine(lines[i].text); i++ {
		}
		blockEnd := lines[i-1].fullEnd
		first := strings.TrimSpace(lines[blockStart].text)
		if isVTTMetadataBlock(first) {
			continue
		}

		timingIndex := blockStart
		if !strings.Contains(lines[timingIndex].text, "-->") {
			timingIndex++ // optional cue identifier
		}
		if timingIndex >= blockPayloadEnd || !strings.Contains(lines[timingIndex].text, "-->") {
			return nil, lineError(lines[blockStart].number, "invalid WebVTT block: expected cue timing")
		}
		if timingIndex+1 >= blockPayloadEnd {
			return nil, lineError(lines[timingIndex].number, "WebVTT cue has no payload")
		}
		start, end, err := parseVTTTiming(lines[timingIndex].text)
		if err != nil {
			return nil, lineWrap(lines[timingIndex].number, err)
		}
		deleteSpan, err := view.span(lines[blockStart].start, blockEnd)
		if err != nil {
			return nil, err
		}
		payload := joinLineText(lines[timingIndex+1 : blockPayloadEnd])
		display, search := cueText(projectMarkupText(payload, true))
		cues = append(cues, Cue{
			ID:          makeCueID(FormatWebVTT, deleteSpan, len(cues)),
			Start:       start,
			End:         end,
			DisplayText: display,
			SearchText:  search,
			DeleteSpan:  deleteSpan,
		})
	}
	return cues, nil
}

func isVTTMetadataBlock(first string) bool {
	upper := strings.ToUpper(first)
	return upper == "NOTE" || strings.HasPrefix(upper, "NOTE ") || strings.HasPrefix(upper, "NOTE\t") ||
		upper == "STYLE" || upper == "REGION"
}

func parseVTTTiming(line string) (start, end time.Duration, err error) {
	if strings.Count(line, "-->") != 1 {
		return 0, 0, fmt.Errorf("invalid WebVTT timing separator")
	}
	left, right, _ := strings.Cut(line, "-->")
	left = strings.TrimSpace(left)
	rightFields := strings.Fields(right)
	if left == "" || len(rightFields) == 0 {
		return 0, 0, fmt.Errorf("invalid WebVTT timing line")
	}
	start, err = parseClock(left, true, ".")
	if err != nil {
		return 0, 0, fmt.Errorf("invalid WebVTT start time: %w", err)
	}
	end, err = parseClock(rightFields[0], true, ".")
	if err != nil {
		return 0, 0, fmt.Errorf("invalid WebVTT end time: %w", err)
	}
	if end < start {
		return 0, 0, fmt.Errorf("WebVTT cue ends before it starts")
	}
	return start, end, nil
}
