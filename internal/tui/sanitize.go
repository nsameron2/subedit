package tui

import "strings"

// sanitizeDisplay neutralizes code points that can alter terminal state,
// visually reorder trusted UI, or inject extra rows. It is intentionally only
// used at rendering boundaries; search and mutation requests retain their
// exact original strings.
func sanitizeDisplay(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if unsafeDisplayRune(r) {
			b.WriteRune('�')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unsafeDisplayRune(r rune) bool {
	// C0/C1, DEL, ESC, BEL, OSC introducers, newlines and tabs.
	if r <= 0x1f || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return true
	}
	// Unicode line/paragraph separators can inject terminal rows after text
	// shaping even though they are not ASCII control characters.
	if r == '\u2028' || r == '\u2029' {
		return true
	}
	// Directional marks, embeddings, overrides, isolates, and deprecated
	// directional controls can spoof filenames and confirmation text.
	switch r {
	case '\u061c', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069',
		'\u206a', '\u206b', '\u206c', '\u206d', '\u206e', '\u206f':
		return true
	}
	return false
}

func (m Model) inputView() string {
	prompt := sanitizeDisplay(m.input.Prompt)
	value := sanitizeDisplay(m.input.Value())
	if prompt == m.input.Prompt && value == m.input.Value() {
		return m.input.View()
	}
	// Bubble Tea's input widget sanitizes controls in SetValue, so assigning a
	// copied model would still alias its rune backing array and mutate the live
	// query. Render a presentation-only line only for hostile values; normal
	// queries retain the widget's real cursor and editing presentation.
	if value == "" {
		value = sanitizeDisplay(m.input.Placeholder)
	}
	return prompt + truncate(value, max(1, m.input.Width()-len([]rune(prompt))))
}
