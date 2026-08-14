package subtitle

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// NormalizeQuery creates the same literal-search representation used for cue
// text: NFC, locale-independent Unicode case folding, whitespace collapsing,
// and trimming.
func NormalizeQuery(query string) string {
	return normalizeSearch(query)
}

func normalizeSearch(text string) string {
	text = norm.NFC.String(text)
	// Caser is stateful and must not be shared between goroutines. Workspace
	// discovery/search intentionally runs concurrently, so create a value for
	// each complete string transformation.
	text = cases.Fold().String(text)
	text = norm.NFC.String(text)
	return collapseWhitespace(text)
}

func normalizeDisplay(text string) string {
	return collapseWhitespace(norm.NFC.String(text))
}

func collapseWhitespace(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	pendingSpace := false
	wrote := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			if wrote {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			out.WriteByte(' ')
			pendingSpace = false
		}
		out.WriteRune(r)
		wrote = true
	}
	return out.String()
}

// projectMarkupText removes recognized SRT/WebVTT tags, converts br tags to
// whitespace, strips WebVTT inline timestamp tags, and decodes character
// references. Unknown or malformed angle-bracket text remains visible; this is
// important for ordinary comparison text such as "2 < 3 > 1".
func projectMarkupText(text string, allowTimestamps bool) string {
	var out strings.Builder
	out.Grow(len(text))
	for cursor := 0; cursor < len(text); {
		openRel := strings.IndexByte(text[cursor:], '<')
		if openRel < 0 {
			out.WriteString(text[cursor:])
			break
		}
		open := cursor + openRel
		out.WriteString(text[cursor:open])
		closeRel := strings.IndexByte(text[open+1:], '>')
		if closeRel < 0 {
			// No later iteration can find a closer for this delimiter, so copying
			// the suffix at once is both faithful and linear-time.
			out.WriteString(text[open:])
			break
		}
		close := open + 1 + closeRel
		strip, lineBreak := classifySubtitleTag(text[open+1:close], allowTimestamps)
		if !strip {
			out.WriteString(text[open : close+1])
		} else if lineBreak {
			out.WriteByte(' ')
		}
		cursor = close + 1
	}
	return html.UnescapeString(out.String())
}

func classifySubtitleTag(content string, allowTimestamps bool) (strip, lineBreak bool) {
	// Whitespace surrounding a tag name is accepted by some SRT producers,
	// but a timestamp tag has a deliberately strict wire shape.
	if allowTimestamps && isVTTTimestampTag(content) {
		return true, false
	}
	content = strings.TrimSpace(content)
	if content == "" || strings.ContainsAny(content, "<>") {
		return false, false
	}
	if content[0] == '/' {
		content = strings.TrimSpace(content[1:])
	}
	content = strings.TrimSuffix(content, "/")
	content = strings.TrimSpace(content)
	if content == "" {
		return false, false
	}
	end := 0
	for end < len(content) {
		c := content[end]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return false, false
	}
	name := strings.ToLower(content[:end])
	remainder := content[end:]
	// A class suffix is part of WebVTT's <c.class> syntax. Other recognized
	// tags may be followed only by whitespace/attributes, not punctuation that
	// could turn visible comparison text into a tag by accident.
	if strings.HasPrefix(remainder, ".") {
		if name != "c" || !validVTTClasses(remainder) {
			return false, false
		}
	} else if remainder != "" && remainder[0] != ' ' && remainder[0] != '\t' {
		return false, false
	}

	switch name {
	case "b", "i", "u", "s", "strike", "strong", "em", "font", "span",
		"c", "v", "lang", "ruby", "rt":
		return true, false
	case "br":
		return true, true
	default:
		return false, false
	}
}

func validVTTClasses(value string) bool {
	// One or more nonempty dot-prefixed class names. WebVTT class names may
	// contain broad Unicode, but whitespace and markup delimiters terminate the
	// annotation and are never valid here.
	for len(value) > 0 {
		if value[0] != '.' {
			return false
		}
		value = value[1:]
		end := strings.IndexByte(value, '.')
		part := value
		if end >= 0 {
			part = value[:end]
		}
		if part == "" || strings.ContainsAny(part, " \t\r\n<>") {
			return false
		}
		if end < 0 {
			return true
		}
		value = value[end:]
	}
	return false
}

func isVTTTimestampTag(content string) bool {
	if content == "" || strings.TrimSpace(content) != content {
		return false
	}
	dot := strings.LastIndexByte(content, '.')
	if dot < 0 || len(content)-dot-1 != 3 || !asciiDigits(content[dot+1:]) {
		return false
	}
	clock := content[:dot]
	parts := strings.Split(clock, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	for i, part := range parts {
		minimumWidth := 2
		if len(parts) == 3 && i == 0 {
			if len(part) < minimumWidth || !asciiDigits(part) {
				return false
			}
			continue
		}
		if len(part) != minimumWidth || !asciiDigits(part) {
			return false
		}
	}
	minuteIndex := 0
	if len(parts) == 3 {
		minuteIndex = 1
	}
	return parts[minuteIndex] <= "59" && parts[minuteIndex+1] <= "59"
}

var assDrawingCommand = regexp.MustCompile(`(?i)\\p\s*([0-9]+)`) // within override blocks

func projectASSText(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	drawing := false
	braceBlocksPossible := true
	for i := 0; i < len(text); {
		if text[i] == '{' && braceBlocksPossible {
			if endRel := strings.IndexByte(text[i+1:], '}'); endRel >= 0 {
				end := i + 1 + endRel
				block := text[i+1 : end]
				for _, match := range assDrawingCommand.FindAllStringSubmatch(block, -1) {
					level, _ := strconv.Atoi(match[1])
					drawing = level != 0
				}
				i = end + 1
				continue
			}
			// No later '{' can have a closing brace either. Keep processing the
			// suffix for ASS escapes/drawing semantics, but never search it again.
			braceBlocksPossible = false
		}
		if text[i] == '\\' && i+1 < len(text) {
			switch text[i+1] {
			case 'N', 'n', 'h':
				if !drawing {
					out.WriteByte(' ')
				}
				i += 2
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if !drawing {
			out.WriteRune(r)
		}
		i += size
	}
	return out.String()
}

func cueText(projected string) (display, search string) {
	display = normalizeDisplay(projected)
	search = normalizeSearch(projected)
	return display, search
}
