package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func editorRenderModel(width, height int) Model {
	m := New(nil, Options{Width: width, Height: height, DisableColor: true})
	m.state = StateEditor
	m.editor.document = EditorDocument{
		FileID: "season/episode.srt", Path: "season/episode.srt", SnapshotID: "snapshot-1",
		Cues: []Cue{
			{ID: "one", Timestamp: "00:00:01.000 → 00:00:02.000", Text: "first visible cue"},
			{ID: "two", Timestamp: "00:00:03.000 → 00:00:04.000", Text: "hidden cue sentinel"},
			{ID: "three", Timestamp: "00:00:05.000 → 00:00:06.000", Text: "third visible cue wraps over more than one row when the terminal is narrow"},
		},
	}
	m.editor.visible = []int{0, 1, 2}
	m.editor.marks = make(map[string]bool)
	return m
}

func TestRenderEditorFrameShowsPathFilterAndMarkCounts(t *testing.T) {
	m := editorRenderModel(100, 28)
	m.editor.visible = []int{0, 2}
	m.editor.focus = 1
	m.editor.marks["one"] = true
	m.editor.marks["two"] = true // marked but hidden by the active filter
	m.editor.input.SetValue("visible")

	view := ansi.Strip(m.renderEditor())
	for _, want := range []string{
		"subedit editor", "season/episode.srt", "2/3 visible", `filter "visible"`,
		"2 pending deletion", "1 hidden mark", "first visible cue", "third visible cue", "#1", "#3",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("editor frame missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "hidden cue sentinel") {
		t.Fatalf("filtered cue was rendered:\n%s", view)
	}
	if strings.Index(view, "first visible cue") > strings.Index(view, "third visible cue") {
		t.Fatalf("visible cues were not rendered in source order:\n%s", view)
	}
}

func TestRenderEditorCommandBarStaysOnBottomBaseline(t *testing.T) {
	for _, height := range []int{24, 36} {
		t.Run(fmt.Sprintf("height-%d", height), func(t *testing.T) {
			models := map[string]Model{}

			short := editorRenderModel(80, height)
			short.editor.document.Cues = short.editor.document.Cues[:1]
			short.editor.visible = []int{0}
			models["short-file"] = short

			empty := editorRenderModel(80, height)
			empty.editor.document.Cues = nil
			empty.editor.visible = nil
			models["empty-file"] = empty

			noMatch := editorRenderModel(80, height)
			noMatch.editor.visible = nil
			noMatch.editor.input.SetValue("absent")
			models["no-match"] = noMatch

			for name, model := range models {
				t.Run(name, func(t *testing.T) {
					view := ansi.Strip(model.renderEditor())
					footer := ansi.Strip(model.editorHelpLine())
					if !strings.HasSuffix(view, footer) {
						t.Fatalf("editor command bar is not the final section:\n%s", view)
					}
					// The workspace viewport deliberately leaves two terminal rows
					// unused. Matching that same baseline keeps the command bar from
					// jumping when switching between workspace and editor screens.
					if rows := strings.Count(view, "\n") + 1; rows != model.height-2 {
						t.Fatalf("editor frame has %d rows, want stable %d-row baseline:\n%s", rows, model.height-2, view)
					}
				})
			}
		})
	}
}

func TestRenderEditorVirtualizesLargeVariableHeightList(t *testing.T) {
	m := editorRenderModel(72, 24)
	const count = 10_000
	m.editor.document.Cues = make([]Cue, count)
	m.editor.visible = make([]int, count)
	for index := range count {
		m.editor.document.Cues[index] = Cue{
			ID: fmt.Sprintf("cue-%05d", index), Text: fmt.Sprintf("cue-%05d %s", index, strings.Repeat("wrapped ", index%7)),
		}
		m.editor.visible[index] = index
	}
	m.editor.top = 5_000
	m.editor.focus = 5_001

	view := ansi.Strip(m.renderEditor())
	if !strings.Contains(view, "cue-05000") || !strings.Contains(view, "cue-05001") {
		t.Fatalf("visible/focused cues missing:\n%s", view)
	}
	for _, outside := range []string{"cue-00000", "cue-04999", "cue-09999"} {
		if strings.Contains(view, outside) {
			t.Fatalf("non-intersecting cue %q was eagerly rendered:\n%s", outside, view)
		}
	}
	if height := strings.Count(view, "\n") + 1; height > m.height {
		t.Fatalf("rendered %d rows into a %d-row terminal", height, m.height)
	}
	if len(view) > m.width*m.height*4 {
		t.Fatalf("virtualized frame unexpectedly large: %d bytes", len(view))
	}
}

func TestBoundedDisplayLinesPreservesGraphemesAndSanitizes(t *testing.T) {
	input := "A🏳️‍🌈e\u0301B\x1b]8;;evil\a" + strings.Repeat("界", 100)
	lines := boundedDisplayLines(input, 6, 2)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %#v", len(lines), lines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "🏳️‍🌈") || !strings.Contains(joined, "e\u0301") {
		t.Fatalf("grapheme cluster was split or lost: %q", joined)
	}
	if !strings.HasSuffix(joined, "…") {
		t.Fatalf("bounded content lacks truncation marker: %q", joined)
	}
	for _, forbidden := range []string{"\x1b", "\a"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("wrapped content retained terminal control %q: %q", forbidden, joined)
		}
	}
	for index, line := range lines {
		if !utf8.ValidString(line) || ansi.StringWidth(line) > 6 {
			t.Fatalf("line %d is invalid or too wide (%d): %q", index, ansi.StringWidth(line), line)
		}
	}
}

func TestDisplayLineWindowBoundsLongZeroWidthRuns(t *testing.T) {
	window := displayLineWindow(strings.Repeat("\u200b", 1<<18), 20, 0, 3)
	if !window.after {
		t.Fatalf("zero-width adversarial input was not bounded: %#v", window)
	}
	if len(window.lines) != 3 {
		t.Fatalf("got %d visible lines, want 3", len(window.lines))
	}
	joined := strings.Join(window.lines, "\n")
	if len(joined) > 20*3*16 {
		t.Fatalf("zero-width window amplified to %d bytes", len(joined))
	}
	for index, line := range window.lines {
		if width := ansi.StringWidth(line); width > 20 {
			t.Fatalf("line %d is %d cells wide: %q", index, width, line)
		}
	}

	oversizedCluster := "A" + strings.Repeat("\u0301", 300) + "B"
	clusterWindow := displayLineWindow(oversizedCluster, 4, 0, 1)
	if got := strings.Join(clusterWindow.lines, ""); got != "�B" {
		t.Fatalf("oversized grapheme was not safely bounded: %q", got)
	}
}

func TestDisplayLineWindowScrollsAndClampsAtEOF(t *testing.T) {
	first := displayLineWindow("1111222233334444", 4, 0, 2)
	if got := strings.Join(first.lines, ","); got != "1111,2222" || first.before || !first.after {
		t.Fatalf("first window = %#v", first)
	}
	middle := displayLineWindow("1111222233334444", 4, 1, 2)
	if got := strings.Join(middle.lines, ","); got != "2222,3333" || !middle.before || !middle.after {
		t.Fatalf("middle window = %#v", middle)
	}
	last := displayLineWindow("1111222233334444", 4, 99, 2)
	if got := strings.Join(last.lines, ","); got != "3333,4444" || !last.before || last.after || last.offset != 2 {
		t.Fatalf("clamped final window = %#v", last)
	}
}

func TestDisplayLineWindowHonorsCueLineBreaks(t *testing.T) {
	window := displayLineWindow("first line\n  indented\r\n\nfourth", 20, 0, 10)
	want := []string{"first line", "  indented", "", "fourth"}
	if got := strings.Join(window.lines, "|"); got != strings.Join(want, "|") {
		t.Fatalf("semantic cue lines = %#v, want %#v", window.lines, want)
	}
	if window.before || window.after {
		t.Fatalf("complete semantic cue unexpectedly reports continuation: %#v", window)
	}

	// An explicit line break after an exactly full wrapped line must not add a
	// phantom blank row.
	exact := displayLineWindow("1234\nnext", 4, 0, 10)
	if got := strings.Join(exact.lines, ","); got != "1234,next" {
		t.Fatalf("exact-width line break = %#v", exact.lines)
	}
}

func TestRenderEditorPathologicalCueIsFrameBounded(t *testing.T) {
	m := editorRenderModel(52, 16)
	m.editor.document.Path = "very/long/字幕/" + strings.Repeat("segment/", 100) + "episode.srt"
	m.editor.document.Cues = []Cue{{
		ID: "huge", Timestamp: "00:00:00.000 → 00:01:00.000",
		Text: "REACHABLE_HEAD " + strings.Repeat("🏳️‍🌈界", 1<<14) + " REACHABLE_TAIL",
	}}
	m.editor.visible = []int{0}
	m.editor.focus = 0

	view := ansi.Strip(m.renderEditor())
	if !strings.Contains(view, "REACHABLE_HEAD") || strings.Contains(view, "REACHABLE_TAIL") || !strings.Contains(view, "→ [ ]") {
		t.Fatalf("first cue-text window lacks bounded continuation:\n%s", view)
	}
	if rows := strings.Count(view, "\n") + 1; rows > m.height {
		t.Fatalf("rendered %d rows into a %d-row terminal", rows, m.height)
	}
	for lineNumber, line := range strings.Split(view, "\n") {
		if width := ansi.StringWidth(line); width > m.width {
			t.Fatalf("line %d is %d cells wide, terminal is %d:\n%s", lineNumber+1, width, m.width, view)
		}
	}

	// An intentionally excessive offset must clamp to the final wrapped window,
	// proving content beyond the first screen remains reachable without ever
	// constructing the intervening card lines.
	m.editor.lineOffset = 1 << 20
	last := ansi.Strip(m.renderEditor())
	if !strings.Contains(last, "REACHABLE_TAIL") || !strings.Contains(last, "← [ ]") {
		t.Fatalf("last cue-text window is not reachable:\n%s", last)
	}
	if rows := strings.Count(last, "\n") + 1; rows > m.height {
		t.Fatalf("last window rendered %d rows into a %d-row terminal", rows, m.height)
	}
}

func TestRenderEditorLoadingEmptyNoMatchAndError(t *testing.T) {
	t.Run("loading", func(t *testing.T) {
		m := editorRenderModel(80, 24)
		m.state = StateEditorLoading
		m.editor.loading = true
		if got := ansi.Strip(m.renderEditor()); !strings.Contains(got, "Loading cues") {
			t.Fatalf("loading view missing status:\n%s", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		m := editorRenderModel(80, 24)
		m.editor.document.Cues = nil
		m.editor.visible = nil
		if got := ansi.Strip(m.renderEditor()); !strings.Contains(got, "has no cues") {
			t.Fatalf("empty view missing message:\n%s", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		m := editorRenderModel(80, 24)
		m.editor.visible = nil
		m.editor.input.SetValue("absent")
		if got := ansi.Strip(m.renderEditor()); !strings.Contains(got, "No cues match this filter") {
			t.Fatalf("no-match view missing guidance:\n%s", got)
		}
	})

	t.Run("error", func(t *testing.T) {
		m := editorRenderModel(48, 14)
		m.editor.document.Cues = nil
		m.editor.visible = nil
		m.editor.err = errors.New("bad\x1b[2J subtitle")
		got := ansi.Strip(m.renderEditor())
		if !strings.Contains(got, "Unable to load") || strings.Contains(got, "\x1b[2J") {
			t.Fatalf("error view is missing or unsafe:\n%s", got)
		}
		for lineNumber, line := range strings.Split(got, "\n") {
			if width := ansi.StringWidth(line); width > m.width {
				t.Fatalf("error line %d is %d cells wide, terminal is %d:\n%s", lineNumber+1, width, m.width, got)
			}
		}
	})
}

func TestRenderEditorSanitizesBackendControlledFields(t *testing.T) {
	m := editorRenderModel(90, 24)
	m.editor.document.Path = "safe\x1b]0;owned\a\u202efile.srt"
	m.editor.document.Cues[0].Timestamp = "00:00\x1b[2J"
	m.editor.document.Cues[0].Text = "visible\x1b]8;;https://evil.invalid\alink\u2066text"
	m.editor.visible = []int{0}
	m.editor.err = errors.New("warning\x1b[31m\u202a")

	view := m.renderEditor()
	for _, forbidden := range []string{"\x1b]0;owned", "\x1b[2J", "\x1b]8;;https://evil.invalid", "\u202e", "\u2066", "\u202a"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("editor retained hostile sequence %q:\n%s", forbidden, view)
		}
	}
}

func TestRenderEditorStatusAndDirtyConfirmationActions(t *testing.T) {
	m := editorRenderModel(90, 24)
	m.status = "Reload required before saving"
	if got := ansi.Strip(m.renderEditor()); !strings.Contains(got, m.status) {
		t.Fatalf("editor guidance status is missing:\n%s", got)
	}

	m.state = StateConfirmation
	m.confirm = confirmation{
		kind:     confirmEditorDirtyExit,
		question: "Two cues are marked. Save, discard, or cancel?",
		back:     StateEditor,
	}
	dirty := ansi.Strip(m.renderConfirmation())
	for _, want := range []string{"Unsaved deletion marks", "S save deletions", "D discard marks", "C / Esc cancel"} {
		if !strings.Contains(dirty, want) {
			t.Errorf("dirty confirmation missing %q:\n%s", want, dirty)
		}
	}
	if strings.Contains(dirty, "Y / Enter") {
		t.Fatalf("dirty three-way prompt advertised binary confirmation:\n%s", dirty)
	}

	m.confirm.kind = confirmEditorSave
	save := ansi.Strip(m.renderConfirmation())
	if !strings.Contains(save, "Y / Enter confirm") || !strings.Contains(save, "N / Esc cancel") {
		t.Fatalf("editor save lost binary confirmation labels:\n%s", save)
	}
}
