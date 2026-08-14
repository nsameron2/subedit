package tui

import (
	"fmt"
	"image/color"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type palette struct {
	brand  color.Color
	accent color.Color
	dim    color.Color
	warn   color.Color
	bad    color.Color
	good   color.Color
	match  color.Color
}

func (m Model) palette() palette {
	if m.opts.DisableColor {
		no := lipgloss.NoColor{}
		return palette{no, no, no, no, no, no, no}
	}
	pick := lipgloss.LightDark(m.dark)
	return palette{
		brand:  pick(lipgloss.Color("#8A5A00"), lipgloss.Color("#FFD75F")),
		accent: pick(lipgloss.Color("#8A5A00"), lipgloss.Color("#FFD75F")),
		dim:    pick(lipgloss.Color("#666666"), lipgloss.Color("#999999")),
		warn:   pick(lipgloss.Color("#915D00"), lipgloss.Color("#F6C177")),
		bad:    pick(lipgloss.Color("#B00020"), lipgloss.Color("#FF7B72")),
		good:   pick(lipgloss.Color("#006B5C"), lipgloss.Color("#5EEAD4")),
		match:  pick(lipgloss.Color("#00677A"), lipgloss.Color("#67E8F9")),
	}
}

// View renders a full-window alternate-screen interface.
func (m Model) View() tea.View {
	content := m.render()
	view := tea.NewView(content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = "subedit"
	return view
}

func (m Model) render() string {
	// Mutation/recovery progress remains visible after a resize; cancellation
	// must still be requestable while an atomic file is finishing.
	if m.tooSmall() && m.state != StateMutation {
		return m.renderTooSmall()
	}
	switch m.state {
	case StateRecovery:
		return m.renderRecovery()
	case StateDiscovery:
		return m.renderDiscovery()
	case StateEditorLoading, StateEditor, StateEditorSearch:
		return m.renderEditor()
	case StateMutation:
		return m.renderMutation()
	case StateSummary:
		return m.renderSummary()
	case StateConfirmation:
		return m.renderConfirmation()
	default:
		return m.renderWorkspace()
	}
}

const editorMinimumCardHeight = 4

// renderEditor is deliberately independent of the workspace viewport. Editor
// cue cards have variable heights, so handing one giant string to viewport
// would eagerly wrap every cue on every resize. The editor session instead
// owns a top visible-cue index and this renderer admits only cards that can
// intersect the remaining terminal rows.
func (m Model) renderEditor() string {
	p := m.palette()
	path := m.editor.document.Path
	if path == "" {
		path = "Opening subtitle…"
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(p.brand).Render("subedit editor")
	pathWidth := max(8, m.width-lipgloss.Width(title)-4)
	pathLine := lipgloss.NewStyle().Bold(true).Render(truncate(sanitizeDisplay(path), pathWidth))

	visible := len(m.editor.visible)
	total := len(m.editor.document.Cues)
	filter := strings.TrimSpace(m.editor.input.Value())
	filterLabel := "all cues"
	if filter != "" {
		filterLabel = `"` + truncate(sanitizeDisplay(filter), max(8, m.width/3)) + `"`
	}
	visibility := fmt.Sprintf("%d/%d visible • filter %s", visible, total, filterLabel)
	if m.editor.searching {
		visibility += " • filtering…"
	}
	visibility = truncate(sanitizeDisplay(visibility), max(8, m.width-2))
	markedCount := m.editorMarkedCount()
	hiddenMarks := max(0, markedCount-m.editorVisibleMarkedCount())
	marks := fmt.Sprintf("%d pending deletion • %d hidden mark", markedCount, hiddenMarks)
	if hiddenMarks != 1 {
		marks += "s"
	}
	marks = truncate(marks, max(8, m.width-2))

	input := m.editorInputView()
	header := strings.Join([]string{title + "  " + pathLine, visibility, marks, input}, "\n")
	if m.editor.stale {
		header += "\n" + lipgloss.NewStyle().Bold(true).Foreground(p.warn).
			Render("Snapshot is stale; reload before saving.")
	}
	if m.status != "" && (m.editor.err == nil || m.status != m.editor.err.Error()) {
		header += "\n" + lipgloss.NewStyle().Foreground(p.warn).
			Render(truncate(sanitizeDisplay(m.status), max(8, m.width-2)))
	}
	if m.editor.err != nil {
		header += "\n" + lipgloss.NewStyle().Foreground(p.bad).
			Render(truncate(sanitizeDisplay(m.editor.err.Error()), max(8, m.width-2)))
	}

	footer := m.editorHelpLine()
	available := max(1, m.height-lipgloss.Height(header)-lipgloss.Height(footer)-2)
	body := m.renderEditorBody(max(12, m.width-2), available)
	// The workspace view gives its viewport a fixed middle height, which keeps
	// its command bar on a stable bottom baseline even when there are only a few
	// cards. Editor cards are variable-height and rendered without a viewport,
	// so explicitly fill the unused middle rows to give both screens the same
	// layout behavior.
	if missing := available - lipgloss.Height(body); missing > 0 {
		body += strings.Repeat("\n", missing)
	}
	return header + "\n" + body + "\n" + footer
}

func (m Model) editorInputView() string {
	prompt := sanitizeDisplay(m.editor.input.Prompt)
	value := sanitizeDisplay(m.editor.input.Value())
	if prompt == m.editor.input.Prompt && value == m.editor.input.Value() {
		return m.editor.input.View()
	}
	if value == "" {
		value = sanitizeDisplay(m.editor.input.Placeholder)
	}
	return prompt + truncate(value, max(1, m.editor.input.Width()-lipgloss.Width(prompt)))
}

func (m Model) editorHelpLine() string {
	if m.state == StateEditorSearch {
		return truncate("Type to filter • Enter/Esc browse • Ctrl+C quit", max(8, m.width-2))
	}
	keys := []string{"↑↓/j k move", "PgUp/PgDn page", "Home/End ends", "Space/D mark", "A mark visible"}
	keys = append(keys, "←/→ cue text")
	if m.editorDirty() {
		keys = append(keys, "N clear marks", "S/Ctrl+S save")
	}
	if strings.TrimSpace(m.editor.input.Value()) != "" {
		keys = append(keys, "C clear filter")
	} else {
		keys = append(keys, "/ or Ctrl+F filter")
	}
	keys = append(keys, "R reload")
	if m.editor.undoID != "" {
		keys = append(keys, "U undo")
	}
	keys = append(keys, "Esc back", "Q quit")
	return strings.Join(boundedDisplayLines(strings.Join(keys, " • "), max(8, m.width-2), 2), "\n")
}

func (m Model) renderEditorBody(width, height int) string {
	p := m.palette()
	if height <= 0 {
		return ""
	}
	if m.state == StateEditorLoading || m.editor.loading {
		return renderEditorMessage(lipgloss.NewStyle().Foreground(p.dim), "Loading cues…", width, height)
	}
	if m.editor.err != nil && len(m.editor.document.Cues) == 0 {
		return renderEditorMessage(lipgloss.NewStyle().Foreground(p.bad), "Unable to load this subtitle file. Press R to retry or Esc to go back.", width, height)
	}
	if len(m.editor.document.Cues) == 0 {
		return renderEditorMessage(lipgloss.NewStyle().Foreground(p.dim), "This subtitle file has no cues.", width, height)
	}
	if len(m.editor.visible) == 0 {
		message := "No cues match this filter. Press C to clear it."
		if strings.TrimSpace(m.editor.input.Value()) == "" {
			message = "No visible cues. Press R to reload the file."
		}
		return renderEditorMessage(lipgloss.NewStyle().Foreground(p.dim), message, width, height)
	}

	top := min(max(0, m.editor.top), len(m.editor.visible)-1)
	focus := min(max(0, m.editor.focus), len(m.editor.visible)-1)
	if top > focus {
		top = focus
	}
	// Each predecessor needs its minimum card plus one separating row. Clamp
	// an obsolete top value defensively so the focused cue always appears.
	maxPredecessors := max(0, (height-editorMinimumCardHeight)/(editorMinimumCardHeight+1))
	if focus-top > maxPredecessors {
		top = focus - maxPredecessors
	}

	filterActive := strings.TrimSpace(m.editor.input.Value()) != ""
	parts := make([]string, 0, max(1, height/(editorMinimumCardHeight+1)))
	remaining := height
	for visibleIndex := top; visibleIndex < len(m.editor.visible) && remaining >= editorMinimumCardHeight; visibleIndex++ {
		if len(parts) > 0 {
			remaining-- // blank separator between cards
			if remaining < editorMinimumCardHeight {
				break
			}
		}

		sourceIndex := m.editor.visible[visibleIndex]
		if sourceIndex < 0 || sourceIndex >= len(m.editor.document.Cues) {
			continue
		}
		// Reserve enough rows for every cue between this card and the focus,
		// including separators, so a tall predecessor cannot hide the focus.
		cardLimit := remaining
		if visibleIndex < focus {
			distance := focus - visibleIndex
			reserved := distance*editorMinimumCardHeight + distance
			cardLimit = max(editorMinimumCardHeight, remaining-reserved)
		}
		cue := m.editor.document.Cues[sourceIndex]
		lineOffset := 0
		if visibleIndex == focus {
			lineOffset = m.editor.lineOffset
		}
		card := m.renderEditorCueCard(cue, sourceIndex, visibleIndex == focus,
			m.editor.marks[cue.ID], filterActive, width, cardLimit, lineOffset)
		cardHeight := lipgloss.Height(card)
		if cardHeight > remaining {
			break
		}
		parts = append(parts, card)
		remaining -= cardHeight
	}
	if len(parts) == 0 {
		return lipgloss.NewStyle().Foreground(p.dim).Render("Focused cue does not fit; enlarge the terminal.")
	}
	return strings.Join(parts, "\n")
}

func renderEditorMessage(style lipgloss.Style, message string, width, height int) string {
	lines := boundedDisplayLines(message, max(1, width), max(1, height))
	return style.Render(strings.Join(lines, "\n"))
}

func (m Model) renderEditorCueCard(cue Cue, sourceIndex int, focused, marked, matched bool, width, height, lineOffset int) string {
	p := m.palette()
	width = max(12, width)
	height = max(editorMinimumCardHeight, height)
	contentWidth := max(1, width-4) // border and one-cell horizontal padding
	textLines := max(1, height-3)   // two borders plus the cue heading
	text := cue.Text
	if text == "" {
		text = "(no visible text)"
	}
	window := displayLineWindow(text, contentWidth, lineOffset, textLines)

	mark := "[ ]"
	if marked {
		mark = "[DELETE]"
	}
	continuation := ""
	if window.before {
		continuation += "←"
	}
	if window.after {
		continuation += "→"
	}
	if continuation != "" {
		continuation += " "
	}
	heading := fmt.Sprintf("%s%s  #%d", continuation, mark, sourceIndex+1)
	if cue.Timestamp != "" {
		heading += "  " + sanitizeDisplay(cue.Timestamp)
	}
	heading = truncate(sanitizeDisplay(heading), contentWidth)

	border := lipgloss.NormalBorder()
	borderColor := p.dim
	style := lipgloss.NewStyle().Width(width).Border(border).BorderForeground(borderColor).Padding(0, 1)
	if matched {
		style = style.Foreground(p.match).BorderForeground(p.match)
	}
	if marked {
		style = style.BorderForeground(p.warn)
		heading = lipgloss.NewStyle().Bold(true).Foreground(p.warn).Render(heading)
	}
	if focused {
		style = style.Border(lipgloss.ThickBorder()).BorderForeground(p.accent)
		if !marked {
			heading = lipgloss.NewStyle().Bold(true).Foreground(p.accent).Render(heading)
		}
	}
	return style.Render(heading + "\n" + strings.Join(window.lines, "\n"))
}

type displayWindow struct {
	lines         []string
	before, after bool
	offset        int
}

// displayLineWindow sanitizes and wraps only enough grapheme clusters to reach
// the requested line window. It does not build preceding or following wrapped
// text. If offset is beyond EOF it retains a small rolling tail and clamps to
// the final reachable window, so an unbounded sequence of Right keys still
// produces useful output.
func displayLineWindow(value string, width, offset, maxLines int) displayWindow {
	if width <= 0 || maxLines <= 0 {
		return displayWindow{}
	}
	offset = max(0, offset)
	window := make([]string, 0, maxLines)
	tail := make([]string, 0, maxLines)
	var line strings.Builder
	lineWidth := 0
	invisibleBytes := 0
	totalLines := 0
	after := false
	stopped := false

	yieldLine := func() bool {
		text := line.String()
		line.Reset()
		lineWidth = 0
		invisibleBytes = 0
		if len(tail) == maxLines {
			copy(tail, tail[1:])
			tail[len(tail)-1] = text
		} else {
			tail = append(tail, text)
		}
		if totalLines >= offset && len(window) < maxLines {
			window = append(window, text)
		} else if totalLines >= offset+maxLines {
			after = true
			totalLines++
			return true
		}
		totalLines++
		return false
	}

	for byteOffset := 0; byteOffset < len(value); {
		cluster, clusterWidth := ansi.FirstGraphemeCluster(value[byteOffset:], ansi.GraphemeWidth)
		if cluster == "" {
			break
		}
		byteOffset += len(cluster)
		// Cue text is allowed to contain semantic line breaks. Handle them as
		// layout here instead of sending them through the general sanitizer;
		// the fixed line window still prevents row injection.
		if cluster == "\n" || cluster == "\r" || cluster == "\r\n" {
			if yieldLine() {
				stopped = true
				break
			}
			continue
		}
		safe := cluster
		// A syntactically single grapheme can contain an adversarial number of
		// zero-width code points. Replacing an implausibly large cluster preserves
		// terminal and frame bounds without splitting ordinary combining text or
		// emoji sequences.
		if len(cluster) > 256 {
			safe, clusterWidth = "�", 1
		} else {
			safe = sanitizeDisplay(cluster)
		}
		if safe != cluster {
			clusterWidth = ansi.StringWidth(safe)
		}
		if clusterWidth > width {
			safe, clusterWidth = "�", 1
		}
		// A run of individually valid zero-width graphemes could otherwise
		// produce an arbitrarily large string on a single terminal row. Preserve
		// a generous prefix, then render subsequent invisible clusters visibly;
		// this also lets the fixed-height window stop scanning adversarial input.
		if clusterWidth == 0 {
			invisibleLimit := max(64, width*8)
			if invisibleBytes+len(safe) > invisibleLimit {
				safe, clusterWidth = "�", 1
				invisibleBytes = invisibleLimit
			} else {
				invisibleBytes += len(safe)
			}
		}
		if lineWidth > 0 && lineWidth+clusterWidth > width {
			if yieldLine() {
				stopped = true
				break
			}
		}
		line.WriteString(safe)
		lineWidth += clusterWidth
	}
	if !stopped && (line.Len() > 0 || totalLines == 0) {
		_ = yieldLine()
	}

	effectiveOffset := offset
	if len(window) == 0 && totalLines > 0 && offset >= totalLines {
		window = append(window, tail...)
		effectiveOffset = max(0, totalLines-len(window))
	}
	return displayWindow{
		lines: window, before: effectiveOffset > 0,
		after:  after || effectiveOffset+len(window) < totalLines,
		offset: effectiveOffset,
	}
}

// boundedDisplayLines is used for trusted chrome that may wrap to a small
// number of rows. Unlike editor cue text it has no scrolling affordance, so a
// continuation is represented by an ellipsis.
func boundedDisplayLines(value string, width, maxLines int) []string {
	window := displayLineWindow(value, width, 0, maxLines)
	if window.after && len(window.lines) > 0 {
		last := len(window.lines) - 1
		if width == 1 {
			window.lines[last] = "…"
		} else {
			window.lines[last] = ansi.Truncate(window.lines[last], width-1, "") + "…"
		}
	}
	return window.lines
}

func (m Model) renderTooSmall() string {
	p := m.palette()
	title := lipgloss.NewStyle().Bold(true).Foreground(p.warn).Render("subedit needs more room")
	text := fmt.Sprintf("\n%s\n\nResize the terminal to at least %d×%d.\nCurrent size: %d×%d\n\nDestructive actions are disabled.", title, minWidth, minHeight, m.width, m.height)
	return lipgloss.Place(max(1, m.width), max(1, m.height), lipgloss.Center, lipgloss.Center, text)
}

func (m Model) renderRecovery() string {
	p := m.palette()
	header := lipgloss.NewStyle().Bold(true).Foreground(p.warn).Render("Recovery required")
	if len(m.recoveries) == 0 {
		body := "Inspecting recovery data…"
		if m.err != nil {
			body = lipgloss.NewStyle().Foreground(p.bad).Render(sanitizeDisplay(m.err.Error()))
		}
		return m.frame(header + "\n\n" + body + "\n\nQ quit")
	}
	r := m.recoveries[0]
	when := "unknown time"
	if !r.CreatedAt.IsZero() {
		when = r.CreatedAt.Local().Format("2006-01-02 15:04:05")
	}
	detail := r.Summary
	if detail == "" {
		detail = fmt.Sprintf("%d files may need restoration.", r.Files)
	}
	body := fmt.Sprintf("%s\n\nTransaction: %s\nCreated: %s\n%s\n\n%d recovery transaction(s) remain.\n\nR restore  •  D discard (confirmation required)  •  Q leave intact and quit", header, sanitizeDisplay(r.ID), when, sanitizeDisplay(detail), len(m.recoveries))
	return m.frame(body)
}

func (m Model) renderDiscovery() string {
	p := m.palette()
	title := lipgloss.NewStyle().Bold(true).Foreground(p.brand).Render("subedit")
	percent := fraction(m.discProg.Completed, m.discProg.Total)
	line := fmt.Sprintf("Discovering subtitles… %d/%d", m.discProg.Completed, m.discProg.Total)
	if m.discProg.CurrentPath != "" {
		line += "\n" + truncate(sanitizeDisplay(m.discProg.CurrentPath), max(10, m.width-8))
	}
	body := title + "\n\n" + line + "\n" + m.progress.ViewAs(percent)
	if m.err != nil {
		body += "\n\n" + lipgloss.NewStyle().Foreground(p.bad).Render(sanitizeDisplay(m.err.Error()))
	}
	body += "\n\nQ quit"
	return m.frame(body)
}

func (m Model) renderWorkspace() string {
	p := m.palette()
	var b strings.Builder
	title := lipgloss.NewStyle().Bold(true).Foreground(p.brand).Render("subedit")
	root := m.opts.Root
	if root == "" {
		root = "."
	}
	focusLabel := "results"
	if m.state == StateSearch {
		focusLabel = "search"
	} else if m.state == StateSelection {
		focusLabel = "selection"
	}
	b.WriteString(title)
	b.WriteString("  ")
	b.WriteString(lipgloss.NewStyle().Foreground(p.dim).Render(truncate(sanitizeDisplay(root), max(8, m.width-28))))
	b.WriteString("  ")
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(p.accent).Render("[" + focusLabel + "]"))
	b.WriteByte('\n')
	b.WriteString(m.inputView())
	b.WriteByte('\n')
	b.WriteString(m.statusLine())
	b.WriteByte('\n')
	b.WriteString(m.viewport.View())
	b.WriteByte('\n')
	b.WriteString(m.helpLine())
	if m.help {
		b.WriteString("\n")
		b.WriteString("Search: printable text edits • Enter/Esc results • / search • Tab selection\n")
		if m.pendingRecovery != "" {
			b.WriteString("Recovery: R restore pending delete • D discard recovery with confirmation • Q quit\n")
			b.WriteString("New deletes and ordinary undo are blocked until recovery is resolved.")
		} else {
			b.WriteString("Results: ↑↓/j k move • PgUp/PgDn page • Enter edit focused file • P delete matches in focused file • I delete matches in all files • R rescan\n")
			b.WriteString("Selection: Enter edit focused file • Space toggle • A all • O delete selected-file matches • U undo • Q quit")
		}
	}
	return b.String()
}

func (m Model) statusLine() string {
	p := m.palette()
	if m.err != nil && m.status != "" {
		return lipgloss.NewStyle().Foreground(p.bad).Render(truncate(sanitizeDisplay(m.status), max(10, m.width-2)))
	}
	if m.normalizedEmptyQuery() {
		valid := 0
		invalid := 0
		for _, f := range m.discovery.Files {
			if f.Valid {
				valid++
			} else {
				invalid++
			}
		}
		return fmt.Sprintf("%d subtitle files • %d valid • %d skipped", len(m.discovery.Files), valid, max(invalid, m.discovery.Skipped))
	}
	if m.searching || m.displayedRevision != m.queryRevision {
		return "Searching… destructive actions disabled until results are current"
	}
	return fmt.Sprintf("%d matching cues in %d of %d files • %d skipped", m.result.MatchingCues, m.result.MatchingFiles, m.result.TotalFiles, m.result.Skipped)
}

func (m Model) helpLine() string {
	if m.pendingRecovery != "" {
		return "Recovery unresolved: R restore pending delete  •  D discard recovery (confirmation required)  •  Q quit"
	}
	if m.retainedUndo != "" {
		return "Recovery unresolved: U retry undo  •  D discard recovery (confirmation required)  •  Q quit"
	}
	if m.state == StateSearch {
		return "Enter results  •  Tab select  •  ? help  •  Ctrl+C quit"
	}
	if m.state == StateSelection {
		return fmt.Sprintf("Enter edit focused  •  Space toggle  •  A all  •  O delete selected (%d)  •  Tab results  •  ? help", m.selectedCount())
	}
	undo := ""
	if m.undoAvailable {
		undo = "  •  U undo"
	}
	return "↑↓ move  •  Enter edit focused  •  / search  •  Tab select  •  I delete all-file matches  •  P delete focused-file matches" + undo + "  •  ? help"
}

func (m *Model) rebuildCards() {
	files := m.currentFiles()
	if len(files) == 0 {
		empty := "No subtitle files found."
		if !m.normalizedEmptyQuery() {
			empty = "No matching cues."
		}
		m.viewport.SetContent("\n" + empty)
		return
	}
	var cards []string
	for index, file := range files {
		cards = append(cards, m.renderCard(index, file))
	}
	m.viewport.SetContent(strings.Join(cards, "\n"))
	// Cards render to exactly cardLineHeight lines including the separator.
	line := m.focus * cardLineHeight
	if line < m.viewport.YOffset() {
		m.viewport.SetYOffset(line)
	} else if line+cardLineHeight > m.viewport.YOffset()+m.viewport.Height() {
		m.viewport.SetYOffset(max(0, line+cardLineHeight-m.viewport.Height()))
	}
}

func (m Model) renderCard(index int, file File) string {
	p := m.palette()
	selectedMode := m.state == StateSelection
	gutter := ""
	if selectedMode {
		if m.selected[file.ID] {
			gutter = lipgloss.NewStyle().Bold(true).Foreground(p.accent).Render("[x]") + " "
		} else {
			gutter = "[ ] "
		}
	}
	innerWidth := max(18, m.width-6-lipgloss.Width(gutter))
	rows := make([]string, 0, 6)
	for i := 0; i < 5; i++ {
		row := ""
		if i < len(file.Preview) {
			cue := file.Preview[i]
			stamp := cue.Timestamp
			if stamp != "" {
				stamp += "  "
			}
			row = truncate(sanitizeDisplay(stamp)+singleLine(cue.Text), innerWidth)
			if !m.normalizedEmptyQuery() {
				row = lipgloss.NewStyle().Bold(true).Foreground(p.match).Render(row)
			}
		}
		rows = append(rows, row)
	}
	name := sanitizeDisplay(file.Path)
	if !file.Valid {
		name += "  [skipped: " + sanitizeDisplay(file.Error) + "]"
	} else if !m.normalizedEmptyQuery() {
		name += fmt.Sprintf("  (%d matches)", file.MatchCount)
	}
	rows = append(rows, lipgloss.NewStyle().Bold(true).Render(truncate(name, innerWidth)))
	border := lipgloss.RoundedBorder()
	borderColor := p.dim
	if index == m.focus {
		border = lipgloss.ThickBorder()
		borderColor = p.accent
	}
	card := lipgloss.NewStyle().
		Width(innerWidth).
		Border(border).
		BorderForeground(borderColor).
		Padding(0, 1).
		Render(strings.Join(rows, "\n"))
	if gutter != "" {
		card = gutter + strings.ReplaceAll(card, "\n", "\n"+strings.Repeat(" ", lipgloss.Width(gutter)))
	}
	return card
}

func (m Model) renderConfirmation() string {
	p := m.palette()
	title := lipgloss.NewStyle().Bold(true).Foreground(p.warn).Render("Confirm destructive action")
	if m.confirm.kind == confirmUndo || m.confirm.kind == confirmEditorUndo || m.confirm.kind == confirmRecoveryRestore {
		title = lipgloss.NewStyle().Bold(true).Foreground(p.accent).Render("Confirm restoration")
	}
	actions := "Y / Enter confirm  •  N / Esc cancel"
	if isEditorDirtyConfirmation(m.confirm.kind) {
		title = lipgloss.NewStyle().Bold(true).Foreground(p.warn).Render("Unsaved deletion marks")
		actions = "S save deletions  •  D discard marks  •  C / Esc cancel"
	}
	body := title + "\n\n" + sanitizeDisplay(m.confirm.question) + "\n\n" + actions
	box := lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).BorderForeground(p.warn).Padding(1, 2).MaxWidth(max(20, m.width-8)).Render(body)
	return lipgloss.Place(max(1, m.width), max(1, m.height), lipgloss.Center, lipgloss.Center, box)
}

func (m Model) renderMutation() string {
	p := m.palette()
	title := "Deleting matching cues safely…"
	completed, total, path := m.mutProg.Completed, m.mutProg.Total, m.mutProg.CurrentPath
	if m.op == opUndo {
		title = "Restoring latest operation safely…"
	} else if m.op == opRecovery {
		title = "Resolving recovery transaction safely…"
		completed, total, path = m.recoveryProg.Completed, m.recoveryProg.Total, m.recoveryProg.CurrentPath
	}
	if m.cancelRequested {
		title = "Operation cancelled by user — finishing current file safely…"
	}
	body := lipgloss.NewStyle().Bold(true).Foreground(p.accent).Render(title) + "\n\n"
	body += fmt.Sprintf("%d/%d files\n%s\n%s", completed, total, truncate(sanitizeDisplay(path), max(12, m.width-8)), m.progress.ViewAs(fraction(completed, total)))
	if m.op != opRecovery {
		body += fmt.Sprintf("\n\n%d succeeded • %d skipped • %d failed", m.mutProg.Succeeded, m.mutProg.Skipped, m.mutProg.Failed)
		body += "\n\nCtrl+C requests cancellation after the current atomic file."
	} else if m.recoveryAction == RecoveryRestore {
		body += "\n\nCtrl+C requests cancellation after the current atomic file."
	}
	return m.frame(body)
}

func (m Model) renderSummary() string {
	p := m.palette()
	title := "Operation complete"
	if m.summary != nil && m.summary.Cancelled {
		title = "Operation cancelled by user"
	}
	body := lipgloss.NewStyle().Bold(true).Foreground(p.good).Render(title)
	if m.summary != nil {
		s := m.summary
		body += fmt.Sprintf("\n\n%d succeeded • %d skipped/conflicted • %d failed • %d not attempted", s.Succeeded, s.Skipped, s.Failed, s.NotAttempted)
		if s.UndoAvailable && normalizedRecoveryKind(*s) != RecoveryGateApply {
			body += "\n\nUndo is available with U from the results screen."
		}
		if s.RecoveryID != "" {
			if normalizedRecoveryKind(*s) == RecoveryGateApply {
				body += "\n\nA pending delete recovery set is retained. New deletes and ordinary undo are blocked until you retry recovery/restore or explicitly discard it."
				if s.UndoAvailable {
					body += " The previous undo point remains recorded and will be available if recovery preserves it."
				}
			} else {
				body += "\n\nA partial undo recovery set is retained. New deletes are blocked until you retry undo or explicitly discard it."
			}
		}
		if len(s.Warnings) > 0 {
			warnings := append([]string(nil), s.Warnings...)
			sort.Strings(warnings)
			for i := range warnings {
				warnings[i] = sanitizeDisplay(warnings[i])
			}
			body += "\n\nWarnings:\n• " + strings.Join(warnings, "\n• ")
		}
	}
	if m.status != "" {
		body += "\n\n" + lipgloss.NewStyle().Foreground(p.bad).Render(sanitizeDisplay(m.status))
	}
	if m.discovering || m.searching || m.refreshBehindSummary {
		body += "\n\nRefreshing workspace…"
	} else if m.pendingRecovery != "" {
		body += "\n\nR restore pending delete • D discard recovery • Q quit"
	} else if m.retainedUndo != "" {
		body += "\n\nU retry undo • D discard recovery • Q quit"
	} else if m.summary != nil && m.summary.UndoAvailable {
		body += "\n\nU undo now • any other key return • Q quit"
	} else {
		body += "\n\nPress any key to return • Q quit"
	}
	return m.frame(body)
}

func (m Model) frame(content string) string {
	return lipgloss.NewStyle().Padding(1, 2).MaxWidth(max(1, m.width-4)).Render(content)
}

func fraction(completed, total int) float64 {
	if total <= 0 {
		return 0
	}
	return min(1, max(0, float64(completed)/float64(total)))
}

func singleLine(value string) string {
	return sanitizeDisplay(value)
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(value, width, "…")
}
