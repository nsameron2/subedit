package subtitle

import (
	"fmt"
	"strings"
)

type assEventFormat struct {
	count int
	start int
	end   int
	text  int
}

func parseASS(view sourceView) ([]Cue, error) {
	lines := splitSourceLines(view.text)
	cues := make([]Cue, 0)
	inEvents := false
	var active *assEventFormat
	for _, line := range lines {
		trimmed := strings.TrimSpace(line.text)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			inEvents = strings.EqualFold(section, "Events")
			if inEvents {
				active = nil
			}
			continue
		}
		if !inEvents || trimmed == "" || strings.HasPrefix(trimmed, ";") {
			continue
		}
		key, value, found := strings.Cut(line.text, ":")
		if !found {
			// Unknown non-key content in an Events section is malformed rather
			// than silently editable.
			return nil, lineError(line.number, "invalid ASS Events record")
		}
		switch {
		case strings.EqualFold(strings.TrimSpace(key), "Format"):
			parsed, err := parseASSEventFormat(value)
			if err != nil {
				return nil, lineWrap(line.number, err)
			}
			active = &parsed
		case strings.EqualFold(strings.TrimSpace(key), "Dialogue"):
			if active == nil {
				return nil, lineError(line.number, "ASS Dialogue appears before an Events Format declaration")
			}
			fields := strings.SplitN(value, ",", active.count)
			if len(fields) != active.count {
				return nil, lineError(line.number, "ASS Dialogue has fewer fields than the active Format")
			}
			start, err := parseASSTime(fields[active.start])
			if err != nil {
				return nil, lineWrap(line.number, fmt.Errorf("invalid ASS start time: %w", err))
			}
			end, err := parseASSTime(fields[active.end])
			if err != nil {
				return nil, lineWrap(line.number, fmt.Errorf("invalid ASS end time: %w", err))
			}
			if end < start {
				return nil, lineError(line.number, "ASS Dialogue ends before it starts")
			}
			deleteSpan, err := view.span(line.start, line.fullEnd)
			if err != nil {
				return nil, err
			}
			display, search := cueText(projectASSText(fields[active.text]))
			cues = append(cues, Cue{
				ID:          makeCueID(FormatASS, deleteSpan, len(cues)),
				Start:       start,
				End:         end,
				DisplayText: display,
				SearchText:  search,
				DeleteSpan:  deleteSpan,
			})
		default:
			// Comments and other ASS event types are source-preserved but not
			// searchable or deletable in v1.
		}
	}
	return cues, nil
}

func parseASSEventFormat(value string) (assEventFormat, error) {
	fields := strings.Split(value, ",")
	if len(fields) == 0 {
		return assEventFormat{}, fmt.Errorf("empty ASS Events Format")
	}
	format := assEventFormat{count: len(fields), start: -1, end: -1, text: -1}
	seen := make(map[string]struct{}, len(fields))
	for i, field := range fields {
		name := strings.ToLower(strings.TrimSpace(field))
		if name == "" {
			return assEventFormat{}, fmt.Errorf("ASS Events Format contains an empty field")
		}
		if _, duplicate := seen[name]; duplicate {
			return assEventFormat{}, fmt.Errorf("ASS Events Format repeats field %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "start":
			format.start = i
		case "end":
			format.end = i
		case "text":
			format.text = i
		}
	}
	if format.start < 0 || format.end < 0 || format.text < 0 {
		return assEventFormat{}, fmt.Errorf("ASS Events Format must contain Start, End, and Text")
	}
	if format.text != len(fields)-1 {
		return assEventFormat{}, fmt.Errorf("ASS Events Format Text field must be last")
	}
	return format, nil
}
