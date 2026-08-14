package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

const (
	minWidth       = 52
	minHeight      = 16
	cardLineHeight = 9
)

// editorSession is loaded on demand. visible and top contain indices into the
// source-ordered document rather than copies of Cue, which keeps filtering and
// rendering bounded to one open file and lets marks survive filter changes.
type editorSession struct {
	active     bool
	document   EditorDocument
	visible    []int
	marks      map[string]bool
	focus      int // index into visible
	anchor     int // preferred source index when a filter changes
	top        int // index into visible
	lineOffset int // wrapped text-line offset within the focused cue
	input      textinput.Model

	loading     bool
	searching   bool
	err         error
	returnState State
	stale       bool // editor snapshot is not currently safe to mutate
	outerStale  bool // workspace cards need one full rescan on editor exit
	undoID      string

	openRevision     uint64
	searchRevision   uint64
	mutationRevision uint64
	loadCancel       context.CancelFunc
	searchCancel     context.CancelFunc
	loadMode         editorLoadMode
	preserveMarks    bool
	afterSave        editorAfterAction
}

type editorLoadMode uint8

const (
	editorLoadOpen editorLoadMode = iota
	editorLoadRefresh
)

type editorAfterAction uint8

const (
	editorAfterStay editorAfterAction = iota
	editorAfterExit
	editorAfterQuit
	editorAfterReload
)

type confirmationKind uint8

const (
	confirmNone confirmationKind = iota
	confirmDelete
	confirmUndo
	confirmRecoveryRestore
	confirmRecoveryDiscard
	confirmEditorSave
	confirmEditorUndo
	confirmEditorDirtyExit
	confirmEditorDirtyQuit
	confirmEditorDirtyReload
)

type confirmation struct {
	kind     confirmationKind
	question string
	back     State
	request  MutationRequest
	recovery RecoveryRequest
}

type operationKind uint8

const (
	opNone operationKind = iota
	opDelete
	opUndo
	opRecovery
	opEditorSave
	opEditorUndo
)

// Model is subedit's root Bubble Tea model. Copying it as Bubble Tea does is
// safe; maps and cancellation functions are only mutated from Update.
type Model struct {
	backend       Backend
	editorBackend EditorBackend
	opts          Options

	state       State
	width       int
	height      int
	input       textinput.Model
	viewport    viewport.Model
	progress    progress.Model
	help        bool
	dark        bool
	status      string
	err         error
	discovery   Discovery
	discovering bool
	discDone    bool
	discProg    DiscoveryEvent

	result            SearchResult
	searching         bool
	queryRevision     uint64
	displayedRevision uint64
	searchCancel      context.CancelFunc
	discoveryCancel   context.CancelFunc

	focus    int
	selected map[string]bool

	confirm              confirmation
	op                   operationKind
	recoveryAction       RecoveryAction
	mutProg              MutationProgress
	summary              *MutationSummary
	mutationCancel       context.CancelFunc
	cancelRequested      bool
	refreshBehindSummary bool

	undoAvailable   bool
	retainedUndo    string
	pendingRecovery string
	userQuit        bool

	recoveryBackend RecoveryBackend
	recoveries      []RecoveryItem
	recoveryLoading bool
	recoveryProg    RecoveryProgress
	activeRecovery  RecoveryRequest

	editor                editorSession
	summaryBack           State
	returnAfterRescan     State
	returnPathAfterRescan string
}

// New constructs a model. The returned model starts with the search input
// focused as soon as discovery (and any required crash recovery) completes.
func New(backend Backend, opts Options) Model {
	if opts.Width <= 0 {
		opts.Width = 80
	}
	if opts.Height <= 0 {
		opts.Height = 24
	}
	input := textinput.New()
	input.Prompt = "Search: "
	input.Placeholder = ""
	input.SetValue(opts.InitialQuery)
	input.SetWidth(max(8, opts.Width-12))
	input.Focus()
	editorInput := textinput.New()
	editorInput.Prompt = "Filter: "
	editorInput.Placeholder = ""
	editorInput.SetWidth(max(8, opts.Width-12))
	editorInput.Blur()

	vp := viewport.New(
		viewport.WithWidth(max(1, opts.Width-2)),
		viewport.WithHeight(max(1, opts.Height-6)),
	)
	vp.MouseWheelEnabled = false
	vp.SoftWrap = false

	p := progress.New(progress.WithWidth(max(12, min(60, opts.Width-8))))
	m := Model{
		backend:       backend,
		opts:          opts,
		state:         StateDiscovery,
		width:         opts.Width,
		height:        opts.Height,
		dark:          true,
		input:         input,
		viewport:      vp,
		progress:      p,
		queryRevision: 1,
		selected:      make(map[string]bool),
		undoAvailable: opts.InitialUndoAvailable || opts.InitialRetainedUndoID != "",
		retainedUndo:  opts.InitialRetainedUndoID,
		editor: editorSession{
			input:  editorInput,
			marks:  make(map[string]bool),
			anchor: -1,
		},
	}
	if eb, ok := backend.(EditorBackend); ok {
		m.editorBackend = eb
	}
	if rb, ok := backend.(RecoveryBackend); ok {
		m.recoveryBackend = rb
		m.state = StateRecovery
	} else {
		m.discovering = true
	}
	m.applyTheme()
	return m
}

func (m *Model) applyTheme() {
	p := m.palette()
	styles := textinput.DefaultStyles(m.dark)
	styles.Focused.Prompt = styles.Focused.Prompt.Bold(true).Foreground(p.accent)
	styles.Focused.Placeholder = styles.Focused.Placeholder.Foreground(p.dim)
	styles.Focused.Suggestion = styles.Focused.Suggestion.Foreground(p.dim)
	styles.Blurred.Prompt = styles.Blurred.Prompt.Foreground(p.dim)
	styles.Blurred.Text = styles.Blurred.Text.Foreground(p.dim)
	styles.Blurred.Placeholder = styles.Blurred.Placeholder.Foreground(p.dim)
	styles.Blurred.Suggestion = styles.Blurred.Suggestion.Foreground(p.dim)
	styles.Cursor.Color = p.accent
	m.input.SetStyles(styles)
	m.editor.input.SetStyles(styles)

	m.progress.FullColor = p.accent
	m.progress.EmptyColor = p.dim
	m.progress.PercentageStyle = m.progress.PercentageStyle.Foreground(p.dim)
}

// State reports the current root state and is useful to embedders and tests.
func (m Model) State() State { return m.state }

// Query returns the exact current search input.
func (m Model) Query() string { return m.input.Value() }

// UserQuitRequested reports whether the user explicitly chose a normal quit
// path. The command layer can use this on Program.Run's returned final model to
// distinguish clean exit (eligible for session-backup cleanup) from terminal
// failure, signal interruption, or panic (recovery data must be preserved).
func (m Model) UserQuitRequested() bool { return m.userQuit }

func (m Model) quit() (tea.Model, tea.Cmd) {
	m.userQuit = true
	return m, tea.Quit
}

// Init checks for incomplete recovery transactions before normal discovery.
func (m Model) Init() tea.Cmd {
	colorCmd := tea.Cmd(func() tea.Msg { return tea.RequestBackgroundColor() })
	if m.backend == nil {
		return func() tea.Msg { return fatalMsg{fmt.Errorf("subedit TUI: nil backend")} }
	}
	if m.recoveryBackend != nil {
		return tea.Batch(colorCmd, listRecoveriesCmd(m.recoveryBackend))
	}
	return tea.Batch(colorCmd, startDiscoveryCmd(m.backend, context.Background()))
}

// Update advances the root state machine.
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.applyTheme()
		return m, nil
	case fatalMsg:
		m.err = msg.err
		m.status = msg.err.Error()
		return m, nil
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case recoveriesMsg:
		if msg.err != nil {
			m.err = msg.err
			m.status = "Could not inspect recovery data: " + msg.err.Error()
			return m, nil
		}
		m.recoveries = msg.items
		if len(m.recoveries) == 0 {
			m.state = StateDiscovery
			m.discovering = true
			return m, startDiscoveryCmd(m.backend, context.Background())
		}
		m.state = StateRecovery
		return m, nil
	case discoveryStreamMsg:
		return m.updateDiscovery(msg)
	case searchStreamMsg:
		return m.updateSearch(msg)
	case editorOpenStreamMsg:
		return m.updateEditorOpen(msg)
	case editorSearchStreamMsg:
		return m.updateEditorSearch(msg)
	case mutationStreamMsg:
		return m.updateMutation(msg)
	case recoveryStreamMsg:
		return m.updateRecovery(msg)
	case tea.KeyPressMsg:
		return m.updateKey(msg)
	default:
		if m.state == StateSearch {
			before := m.input.Value()
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(message)
			return m.afterInputChange(before, cmd)
		}
		if m.state == StateEditorSearch {
			before := m.editor.input.Value()
			var cmd tea.Cmd
			m.editor.input, cmd = m.editor.input.Update(message)
			return m.afterEditorInputChange(before, cmd)
		}
	}
	return m, nil
}

func (m *Model) resize(width, height int) {
	m.width, m.height = width, height
	m.editor.lineOffset = 0
	m.input.SetWidth(max(8, width-12))
	m.editor.input.SetWidth(max(8, width-12))
	m.viewport.SetWidth(max(1, width-2))
	m.viewport.SetHeight(max(1, height-m.chromeHeight()))
	m.progress.SetWidth(max(12, min(60, width-8)))
	m.rebuildCards()
}

func (m Model) chromeHeight() int {
	height := 6
	if m.help {
		height += 3
	}
	return height
}

func (m Model) tooSmall() bool { return m.width > 0 && (m.width < minWidth || m.height < minHeight) }

func (m Model) normalizedEmptyQuery() bool { return strings.TrimSpace(m.input.Value()) == "" }

func (m Model) currentFiles() []File {
	if m.normalizedEmptyQuery() {
		return m.discovery.Files
	}
	return m.result.Files
}

func (m Model) actionsReady() bool {
	return !m.tooSmall() && m.discDone && !m.discovering && !m.searching &&
		!m.normalizedEmptyQuery() && m.displayedRevision == m.queryRevision &&
		len(m.result.Files) > 0
}

func (m *Model) clearSelection() {
	m.selected = make(map[string]bool)
}

func (m *Model) clampFocus() {
	files := m.currentFiles()
	if len(files) == 0 {
		m.focus = 0
		return
	}
	m.focus = max(0, min(m.focus, len(files)-1))
}

func (m *Model) restoreOuterFocus(path string) {
	if path == "" {
		m.clampFocus()
		return
	}
	for index, file := range m.currentFiles() {
		if file.Path == path {
			m.focus = index
			return
		}
	}
	m.clampFocus()
}

func (m Model) focusedFile() (File, bool) {
	files := m.currentFiles()
	if m.focus < 0 || m.focus >= len(files) {
		return File{}, false
	}
	return files[m.focus], true
}

func (m Model) editorVisibleCues() []Cue {
	cues := make([]Cue, 0, len(m.editor.visible))
	for _, sourceIndex := range m.editor.visible {
		if sourceIndex >= 0 && sourceIndex < len(m.editor.document.Cues) {
			cues = append(cues, m.editor.document.Cues[sourceIndex])
		}
	}
	return cues
}

func (m Model) editorFocusedCue() (Cue, bool) {
	if m.editor.focus < 0 || m.editor.focus >= len(m.editor.visible) {
		return Cue{}, false
	}
	sourceIndex := m.editor.visible[m.editor.focus]
	if sourceIndex < 0 || sourceIndex >= len(m.editor.document.Cues) {
		return Cue{}, false
	}
	return m.editor.document.Cues[sourceIndex], true
}

func (m Model) editorDirty() bool { return m.editorMarkedCount() > 0 }

func (m Model) editorMarkedCount() int {
	n := 0
	for _, cue := range m.editor.document.Cues {
		if m.editor.marks[cue.ID] {
			n++
		}
	}
	return n
}

func (m Model) editorVisibleMarkedCount() int {
	n := 0
	for _, sourceIndex := range m.editor.visible {
		if sourceIndex >= 0 && sourceIndex < len(m.editor.document.Cues) && m.editor.marks[m.editor.document.Cues[sourceIndex].ID] {
			n++
		}
	}
	return n
}

func (m Model) editorHiddenCount() int {
	return max(0, len(m.editor.document.Cues)-len(m.editor.visible))
}

func (m Model) editorHiddenMarkedCount() int {
	return max(0, m.editorMarkedCount()-m.editorVisibleMarkedCount())
}

func (m Model) updateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := strings.ToLower(msg.String())
	if m.state == StateMutation {
		cancellable := m.op != opRecovery || m.recoveryAction == RecoveryRestore
		if key == "ctrl+c" && cancellable && !m.cancelRequested {
			m.cancelRequested = true
			m.status = "Cancellation requested — finishing the current file safely…"
			if m.mutationCancel != nil {
				m.mutationCancel()
			}
		}
		return m, nil
	}
	if m.state == StateRecovery && m.recoveryLoading {
		return m, nil
	}
	if m.tooSmall() {
		if key == "q" || key == "ctrl+c" {
			if m.editor.active && m.editorDirty() {
				m.status = "Resize the terminal to review, save, or discard marked cues before quitting."
				return m, nil
			}
			return m.quit()
		}
		return m, nil
	}

	switch m.state {
	case StateRecovery:
		return m.updateRecoveryKey(key)
	case StateConfirmation:
		return m.updateConfirmationKey(key)
	case StateSummary:
		if key == "q" || key == "ctrl+c" {
			if m.summaryBack == StateEditor {
				if m.editor.loading || m.editor.searching {
					return m, nil
				}
				if m.editorDirty() {
					return m.openEditorDirtyConfirmation(confirmEditorDirtyQuit)
				}
			}
			return m.quit()
		}
		if key == "?" {
			m.help = !m.help
			m.resize(m.width, m.height)
			return m, nil
		}
		// While the post-mutation rescan is still in flight, keep the summary
		// visible so stale cards cannot be acted upon.
		if m.discovering || m.searching || m.refreshBehindSummary || m.editor.loading || m.editor.searching {
			return m, nil
		}
		if (key == "r" || key == "u") && m.pendingRecovery != "" && m.recoveryBackend != nil {
			m.confirm = confirmation{
				kind:     confirmRecoveryRestore,
				question: "Restore files from the retained pending delete recovery set?",
				back:     StateSummary,
				recovery: RecoveryRequest{ID: m.pendingRecovery, Action: RecoveryRestore},
			}
			m.state = StateConfirmation
			return m, nil
		}
		if key == "u" && m.summaryBack == StateEditor {
			if !m.editorScopedUndoReady() {
				m.status = "No exact scoped editor undo is available."
				return m, nil
			}
			m.confirm = confirmation{
				kind: confirmEditorUndo, back: StateSummary,
				question: "Undo the latest saved change for this exact subtitle file?",
			}
			m.state = StateConfirmation
			return m, nil
		}
		if key == "u" && m.undoAvailable {
			m.confirm = confirmation{kind: confirmUndo, question: "Restore the latest successful operation from its recovery backup?", back: StateSummary}
			m.state = StateConfirmation
			return m, nil
		}
		if key == "d" && m.pendingRecovery != "" && m.recoveryBackend != nil {
			m.confirm = confirmation{
				kind:     confirmRecoveryDiscard,
				question: "Permanently discard the pending delete recovery set and accept the current files? This may retire the previous undo and cannot be undone.",
				back:     StateSummary,
				recovery: RecoveryRequest{ID: m.pendingRecovery, Action: RecoveryDiscard},
			}
			m.state = StateConfirmation
			return m, nil
		}
		if key == "d" && m.retainedUndo != "" && m.recoveryBackend != nil {
			m.confirm = confirmation{
				kind:     confirmRecoveryDiscard,
				question: "Permanently discard the retained partial undo recovery set? This cannot be undone.",
				back:     StateSummary,
				recovery: RecoveryRequest{ID: m.retainedUndo, Action: RecoveryDiscard},
			}
			m.state = StateConfirmation
			return m, nil
		}
		if m.summaryBack == StateEditor {
			after := m.editor.afterSave
			m.editor.afterSave = editorAfterStay
			m.summaryBack = 0
			m.summary = nil
			m.err = nil
			if m.editor.stale || m.editor.err != nil {
				m.state = StateEditor
				if m.status == "" {
					m.status = "The file could not be refreshed; press R to load a fresh snapshot before editing."
				}
				return m, nil
			}
			m.status = ""
			if m.editorDirty() {
				m.state = StateEditor
				return m, nil
			}
			switch after {
			case editorAfterExit:
				return m.exitEditor(true)
			case editorAfterQuit:
				if m.recoveryGated() {
					m.state = StateEditor
					m.status = "Recovery remains unresolved; leave it intact or resolve it before quitting."
					return m, nil
				}
				return m.quit()
			default:
				m.state = StateEditor
				return m, nil
			}
		}
		m.summary = nil
		m.err = nil
		m.status = ""
		if m.normalizedEmptyQuery() {
			m.state = StateSearch
			m.input.Focus()
		} else {
			m.state = StateResults
			m.input.Blur()
		}
		return m, nil
	case StateDiscovery:
		if key == "q" || key == "ctrl+c" {
			return m.quit()
		}
		return m, nil
	case StateSearch:
		return m.updateSearchKey(msg, key)
	case StateEditorLoading, StateEditor, StateEditorSearch:
		return m.updateEditorKey(msg, key)
	default:
		return m.updateResultsKey(key)
	}
}

func (m Model) updateSearchKey(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "ctrl+c":
		return m.quit()
	case "enter", "esc":
		m.input.Blur()
		m.state = StateResults
		return m, nil
	case "tab":
		if m.actionsReady() {
			m.input.Blur()
			m.state = StateSelection
			m.rebuildCards()
		}
		return m, nil
	}
	before := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m.afterInputChange(before, cmd)
}

func (m Model) afterInputChange(before string, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.input.Value() == before {
		return m, inputCmd
	}
	m.queryRevision++
	m.displayedRevision = 0
	m.searching = false
	m.result = SearchResult{}
	m.err = nil
	m.status = ""
	m.focus = 0
	m.clearSelection()
	if m.searchCancel != nil {
		m.searchCancel()
	}
	if m.normalizedEmptyQuery() || !m.discDone {
		m.rebuildCards()
		return m, inputCmd
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.searchCancel = cancel
	m.searching = true
	req := SearchRequest{Query: m.input.Value(), Revision: m.queryRevision}
	return m, tea.Batch(inputCmd, startSearchCmd(m.backend, ctx, req))
}

func (m Model) updateResultsKey(key string) (tea.Model, tea.Cmd) {
	files := m.currentFiles()
	switch key {
	case "q", "ctrl+c":
		return m.quit()
	case "enter":
		return m.openEditor()
	case "/":
		m.clearSelection()
		m.state = StateSearch
		return m, m.input.Focus()
	case "?":
		m.help = !m.help
		m.resize(m.width, m.height)
	case "tab":
		if m.actionsReady() {
			if m.state == StateSelection {
				m.state = StateResults
			} else {
				m.state = StateSelection
			}
			m.rebuildCards()
		}
	case "up", "k":
		if len(files) > 0 {
			m.focus = max(0, m.focus-1)
			m.rebuildCards()
		}
	case "down", "j":
		if len(files) > 0 {
			m.focus = min(len(files)-1, m.focus+1)
			m.rebuildCards()
		}
	case "pgup":
		m.focus = max(0, m.focus-max(1, m.viewport.Height()/cardLineHeight))
		m.rebuildCards()
	case "pgdown":
		m.focus = min(max(0, len(files)-1), m.focus+max(1, m.viewport.Height()/cardLineHeight))
		m.rebuildCards()
	case " ", "space":
		if m.state == StateSelection && m.actionsReady() {
			if f, ok := m.focusedFile(); ok {
				m.selected[f.ID] = !m.selected[f.ID]
				m.rebuildCards()
			}
		}
	case "a":
		if m.state == StateSelection && m.actionsReady() {
			for _, f := range m.result.Files {
				m.selected[f.ID] = true
			}
			m.rebuildCards()
		}
	case "i":
		if m.actionsReady() && !m.recoveryGated() {
			return m.openDeleteConfirmation(DeleteAll)
		}
	case "p":
		if m.actionsReady() && !m.recoveryGated() {
			return m.openDeleteConfirmation(DeleteFocused)
		}
	case "o":
		if m.state == StateSelection && m.actionsReady() && !m.recoveryGated() && m.selectedCount() > 0 {
			return m.openDeleteConfirmation(DeleteSelected)
		}
	case "u":
		if m.pendingRecovery != "" && m.recoveryBackend != nil {
			m.confirm = confirmation{
				kind:     confirmRecoveryRestore,
				question: "Restore files from the retained pending delete recovery set?",
				back:     m.state,
				recovery: RecoveryRequest{ID: m.pendingRecovery, Action: RecoveryRestore},
			}
			m.state = StateConfirmation
			break
		}
		if m.undoAvailable {
			m.confirm = confirmation{kind: confirmUndo, question: "Restore the latest successful operation from its recovery backup?", back: m.state}
			m.state = StateConfirmation
		}
	case "d":
		if m.pendingRecovery != "" && m.recoveryBackend != nil {
			m.confirm = confirmation{
				kind:     confirmRecoveryDiscard,
				question: "Permanently discard the pending delete recovery set and accept the current files? This may retire the previous undo and cannot be undone.",
				back:     m.state,
				recovery: RecoveryRequest{ID: m.pendingRecovery, Action: RecoveryDiscard},
			}
			m.state = StateConfirmation
			break
		}
		if m.retainedUndo != "" && m.recoveryBackend != nil {
			m.confirm = confirmation{
				kind:     confirmRecoveryDiscard,
				question: "Permanently discard the retained partial undo recovery set? This cannot be undone.",
				back:     m.state,
				recovery: RecoveryRequest{ID: m.retainedUndo, Action: RecoveryDiscard},
			}
			m.state = StateConfirmation
		}
	case "r":
		if m.pendingRecovery != "" && m.recoveryBackend != nil {
			m.confirm = confirmation{
				kind:     confirmRecoveryRestore,
				question: "Restore files from the retained pending delete recovery set?",
				back:     m.state,
				recovery: RecoveryRequest{ID: m.pendingRecovery, Action: RecoveryRestore},
			}
			m.state = StateConfirmation
			break
		}
		return m.startRescan(false)
	}
	return m, nil
}

func (m Model) editorOpenReady() bool {
	if m.tooSmall() || !m.discDone || m.discovering || m.searching {
		return false
	}
	if !m.normalizedEmptyQuery() && m.displayedRevision != m.queryRevision {
		return false
	}
	_, ok := m.focusedFile()
	return ok
}

func (m Model) openEditor() (tea.Model, tea.Cmd) {
	if m.editorBackend == nil {
		m.status = "Single-file editing is unavailable with this backend."
		return m, nil
	}
	if !m.editorOpenReady() {
		m.status = "Wait for current results before opening a subtitle file."
		return m, nil
	}
	file, ok := m.focusedFile()
	if !ok || !file.Valid || file.ID == "" || file.Path == "" {
		m.status = "This subtitle file is invalid and cannot be opened."
		return m, nil
	}
	if m.editor.loadCancel != nil {
		m.editor.loadCancel()
	}
	if m.editor.searchCancel != nil {
		m.editor.searchCancel()
	}
	editorInput := m.editor.input
	editorInput.SetValue(m.input.Value())
	editorInput.Blur()
	revision := m.editor.openRevision + 1
	m.editor = editorSession{
		active:           true,
		document:         EditorDocument{FileID: file.ID, Path: file.Path},
		marks:            make(map[string]bool),
		anchor:           -1,
		input:            editorInput,
		loading:          true,
		returnState:      m.state,
		openRevision:     revision,
		searchRevision:   m.editor.searchRevision,
		mutationRevision: m.editor.mutationRevision,
		loadMode:         editorLoadOpen,
	}
	m.input.Blur()
	m.state = StateEditorLoading
	m.status = ""
	m.err = nil
	ctx, cancel := context.WithCancel(context.Background())
	m.editor.loadCancel = cancel
	req := EditorOpenRequest{FileID: file.ID, Path: file.Path, Revision: revision}
	return m, startEditorOpenCmd(m.editorBackend, ctx, req)
}

func (m Model) updateEditorKey(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	if m.state == StateEditorLoading {
		switch key {
		case "esc":
			return m.exitEditor(false)
		case "q", "ctrl+c":
			return m.quit()
		}
		return m, nil
	}
	if m.state == StateEditorSearch {
		switch key {
		case "ctrl+c":
			if m.editorDirty() {
				return m.openEditorDirtyConfirmation(confirmEditorDirtyQuit)
			}
			return m.quit()
		case "enter", "esc":
			m.editor.input.Blur()
			m.state = StateEditor
			return m, nil
		}
		before := m.editor.input.Value()
		var cmd tea.Cmd
		m.editor.input, cmd = m.editor.input.Update(msg)
		return m.afterEditorInputChange(before, cmd)
	}

	switch key {
	case "q", "ctrl+c":
		if m.editorDirty() {
			return m.openEditorDirtyConfirmation(confirmEditorDirtyQuit)
		}
		return m.quit()
	case "esc":
		if m.editorDirty() {
			return m.openEditorDirtyConfirmation(confirmEditorDirtyExit)
		}
		return m.exitEditor(false)
	case "/", "ctrl+f":
		m.state = StateEditorSearch
		return m, m.editor.input.Focus()
	case "up", "k":
		m.moveEditorFocus(-1)
	case "down", "j":
		m.moveEditorFocus(1)
	case "pgup":
		m.moveEditorFocus(-m.editorPageSize())
	case "pgdown":
		m.moveEditorFocus(m.editorPageSize())
	case "home":
		m.setEditorFocus(0)
	case "end":
		m.setEditorFocus(len(m.editor.visible) - 1)
	case "left":
		if window, ok := m.editorFocusedLineWindow(); ok {
			m.editor.lineOffset = max(0, window.offset-1)
		}
	case "right":
		if window, ok := m.editorFocusedLineWindow(); ok {
			m.editor.lineOffset = window.offset
			if window.after {
				m.editor.lineOffset++
			}
		}
	case " ", "space", "d":
		if !m.editorActionsReady() {
			return m.editorBlocked()
		}
		if cue, ok := m.editorFocusedCue(); ok {
			m.editor.marks[cue.ID] = !m.editor.marks[cue.ID]
			if !m.editor.marks[cue.ID] {
				delete(m.editor.marks, cue.ID)
			}
		}
	case "a":
		if !m.editorActionsReady() {
			return m.editorBlocked()
		}
		for _, sourceIndex := range m.editor.visible {
			if sourceIndex >= 0 && sourceIndex < len(m.editor.document.Cues) {
				m.editor.marks[m.editor.document.Cues[sourceIndex].ID] = true
			}
		}
	case "n":
		m.editor.marks = make(map[string]bool)
	case "s", "ctrl+s":
		return m.openEditorSaveConfirmation(editorAfterStay)
	case "c":
		if m.editor.input.Value() == "" {
			return m, nil
		}
		m.captureEditorAnchor()
		before := m.editor.input.Value()
		m.editor.input.SetValue("")
		return m.afterEditorInputChange(before, nil)
	case "r":
		if m.editorDirty() {
			return m.openEditorDirtyConfirmation(confirmEditorDirtyReload)
		}
		return m.refreshEditor(false, editorAfterStay)
	case "u":
		if m.pendingRecovery != "" || (m.retainedUndo != "" && m.retainedUndo != m.editor.undoID) {
			return m.editorBlocked()
		}
		if m.editorDirty() {
			m.status = "Save or clear marked cues before undoing this file."
			return m, nil
		}
		if !m.editorScopedUndoReady() {
			m.status = "No scoped editor undo is available."
			return m, nil
		}
		m.confirm = confirmation{
			kind: confirmEditorUndo, back: StateEditor,
			question: "Undo the latest saved change for this exact subtitle file?",
		}
		m.state = StateConfirmation
	}
	return m, nil
}

func (m Model) editorActionsReady() bool {
	return !m.tooSmall() && !m.editor.loading && !m.editor.searching && m.editor.err == nil &&
		m.editor.document.SnapshotID != "" && !m.editor.stale && !m.recoveryGated()
}

func (m Model) editorBlocked() (tea.Model, tea.Cmd) {
	if m.recoveryGated() {
		m.status = "Resolve the retained recovery set before marking, saving, or undoing cues."
	} else if m.editor.stale {
		m.status = "Reload this file before changing cues."
	} else {
		m.status = "Wait for the editor snapshot and filter to finish loading."
	}
	return m, nil
}

func (m *Model) captureEditorAnchor() {
	if m.editor.focus >= 0 && m.editor.focus < len(m.editor.visible) {
		m.editor.anchor = m.editor.visible[m.editor.focus]
	}
}

func (m *Model) moveEditorFocus(delta int) {
	if len(m.editor.visible) == 0 {
		m.editor.focus, m.editor.top, m.editor.anchor = 0, 0, -1
		return
	}
	m.editor.focus = max(0, min(len(m.editor.visible)-1, m.editor.focus+delta))
	m.editor.anchor = m.editor.visible[m.editor.focus]
	m.editor.lineOffset = 0
	page := m.editorPageSize()
	if m.editor.focus < m.editor.top {
		m.editor.top = m.editor.focus
	} else if m.editor.focus >= m.editor.top+page {
		m.editor.top = max(0, m.editor.focus-page+1)
	}
}

func (m *Model) setEditorFocus(index int) {
	if len(m.editor.visible) == 0 {
		m.editor.focus, m.editor.top, m.editor.anchor, m.editor.lineOffset = 0, 0, -1, 0
		return
	}
	m.editor.focus = max(0, min(len(m.editor.visible)-1, index))
	m.editor.anchor = m.editor.visible[m.editor.focus]
	m.editor.lineOffset = 0
	page := m.editorPageSize()
	m.editor.top = max(0, min(m.editor.focus, len(m.editor.visible)-page))
}

func (m Model) editorPageSize() int {
	return max(1, (m.height-8)/(editorMinimumCardHeight+1))
}

func (m Model) editorFocusedLineWindow() (displayWindow, bool) {
	cue, ok := m.editorFocusedCue()
	if !ok {
		return displayWindow{}, false
	}
	text := cue.Text
	if text == "" {
		text = "(no visible text)"
	}
	// A one-line probe finds the true final wrapped offset without eagerly
	// allocating the cue. The renderer may show a taller window, but an offset
	// can never accumulate beyond EOF, so one Left always moves immediately.
	contentWidth := max(1, max(12, m.width-2)-4)
	return displayLineWindow(text, contentWidth, m.editor.lineOffset, 1), true
}

func (m Model) openEditorDirtyConfirmation(kind confirmationKind) (tea.Model, tea.Cmd) {
	action := "leave the editor"
	if kind == confirmEditorDirtyQuit {
		action = "quit subedit"
	} else if kind == confirmEditorDirtyReload {
		action = "reload this file"
	}
	question := fmt.Sprintf("%d cues are marked for deletion in %s. Save them, discard the marks, or cancel before you %s?", m.editorMarkedCount(), m.editor.document.Path, action)
	question += m.editorSaveSafetyDisclosure()
	m.confirm = confirmation{
		kind: kind, back: StateEditor,
		question: question,
	}
	m.state = StateConfirmation
	return m, nil
}

func (m Model) openEditorSaveConfirmation(after editorAfterAction) (tea.Model, tea.Cmd) {
	if !m.editorActionsReady() {
		return m.editorBlocked()
	}
	request := m.buildEditorMutationRequest()
	if len(request.Targets) != 1 || len(request.Targets[0].CueIDs) == 0 {
		m.status = "No cues are marked for deletion."
		return m, nil
	}
	m.editor.afterSave = after
	question := fmt.Sprintf("Delete %d marked cues from %s?", len(request.Targets[0].CueIDs), m.editor.document.Path)
	question += m.editorSaveSafetyDisclosure()
	m.confirm = confirmation{
		kind: confirmEditorSave, back: StateEditor, request: request,
		question: question,
	}
	m.state = StateConfirmation
	return m, nil
}

func (m Model) editorSaveSafetyDisclosure() string {
	hidden := m.editorHiddenMarkedCount()
	remaining := len(m.editor.document.Cues) - m.editorMarkedCount()
	disclosure := ""
	if hidden > 0 {
		disclosure += fmt.Sprintf(" %d marked cues are hidden by the current filter.", hidden)
	}
	if remaining == 0 {
		disclosure += " This will leave zero cues in the file."
	}
	if m.editor.undoID != "" || m.undoAvailable {
		disclosure += " This successful save will replace the current one-level undo point."
	} else {
		disclosure += " A successful save will become the one-level undo point."
	}
	return disclosure
}

func (m Model) buildEditorMutationRequest() MutationRequest {
	ids := make([]string, 0, m.editorMarkedCount())
	for _, cue := range m.editor.document.Cues {
		if m.editor.marks[cue.ID] {
			ids = append(ids, cue.ID)
		}
	}
	return MutationRequest{
		Scope: DeleteEditor, Source: MutationSourceEditor,
		SnapshotID: m.editor.document.SnapshotID,
		Revision:   m.editor.mutationRevision + 1,
		Targets: []MutationTarget{{
			FileID: m.editor.document.FileID, Path: m.editor.document.Path, CueIDs: ids,
		}},
	}
}

func (m Model) afterEditorInputChange(before string, inputCmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.editor.input.Value() == before {
		return m, inputCmd
	}
	m.captureEditorAnchor()
	if m.editor.searchCancel != nil {
		m.editor.searchCancel()
	}
	m.editor.searchRevision++
	m.editor.searching = true
	m.editor.err = nil
	m.status = ""
	if m.editorBackend == nil || m.editor.document.SnapshotID == "" {
		m.editor.searching = false
		m.editor.err = errorsNew("editor snapshot is unavailable")
		return m, inputCmd
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.editor.searchCancel = cancel
	req := EditorSearchRequest{
		SnapshotID: m.editor.document.SnapshotID,
		Query:      m.editor.input.Value(),
		Revision:   m.editor.searchRevision,
	}
	return m, tea.Batch(inputCmd, startEditorSearchCmd(m.editorBackend, ctx, req))
}

// errorsNew keeps editor validation errors local without exposing backend
// details; it is a variable-free wrapper so tests can compare user-facing text.
func errorsNew(message string) error { return fmt.Errorf("%s", message) }

func (m Model) updateEditorOpen(msg editorOpenStreamMsg) (tea.Model, tea.Cmd) {
	next := tea.Cmd(nil)
	if msg.ok && !msg.event.Done {
		next = readEditorOpenCmd(msg.ch, msg.revision, msg.mode, msg.fileID, msg.path)
	}
	if !m.editor.active || msg.revision != m.editor.openRevision || msg.mode != m.editor.loadMode {
		return m, next
	}
	if !msg.ok {
		m.editor.loading = false
		m.editor.stale = true
		m.editor.err = ErrClosedStream
		m.status = ErrClosedStream.Error()
		if m.state == StateEditorLoading {
			m.state = StateEditor
		}
		return m, nil
	}
	if msg.event.Revision != msg.revision {
		return m, next
	}
	if msg.event.Err != nil {
		m.editor.err = msg.event.Err
		m.status = msg.event.Err.Error()
		if msg.event.Done {
			m.editor.loading = false
			m.editor.stale = true
			if m.state == StateEditorLoading {
				m.state = StateEditor
			}
		}
		return m, next
	}
	if !msg.event.Done {
		return m, next
	}
	document, err := validateEditorDocument(msg.event.Document, msg.fileID, msg.path)
	if err != nil {
		m.editor.loading = false
		m.editor.stale = true
		m.editor.err = err
		m.status = err.Error()
		if m.state == StateEditorLoading {
			m.state = StateEditor
		}
		return m, nil
	}

	marks := make(map[string]bool)
	if m.editor.preserveMarks {
		for _, cue := range document.Cues {
			if m.editor.marks[cue.ID] {
				marks[cue.ID] = true
			}
		}
	}
	m.editor.document = document
	m.editor.marks = marks
	m.editor.loading = true // remains loading until this snapshot is filtered
	m.editor.searching = false
	m.editor.stale = false
	m.editor.err = nil
	m.editor.lineOffset = 0
	m.status = ""
	if msg.mode == editorLoadRefresh {
		m.editor.outerStale = true
	}
	if m.editor.searchCancel != nil {
		m.editor.searchCancel()
	}
	m.editor.searchRevision++
	ctx, cancel := context.WithCancel(context.Background())
	m.editor.searchCancel = cancel
	m.editor.searching = true
	req := EditorSearchRequest{
		SnapshotID: document.SnapshotID,
		Query:      m.editor.input.Value(),
		Revision:   m.editor.searchRevision,
	}
	return m, startEditorSearchCmd(m.editorBackend, ctx, req)
}

func validateEditorDocument(document *EditorDocument, expectedFileID, expectedPath string) (EditorDocument, error) {
	if document == nil {
		return EditorDocument{}, errorsNew("editor backend returned no document")
	}
	if document.FileID == "" || document.Path == "" || document.SnapshotID == "" {
		return EditorDocument{}, errorsNew("editor backend returned an incomplete snapshot")
	}
	if expectedFileID != "" && document.FileID != expectedFileID {
		return EditorDocument{}, errorsNew("editor backend returned a different file")
	}
	if expectedPath != "" && document.Path != expectedPath {
		return EditorDocument{}, errorsNew("editor backend returned a different path")
	}
	copyDocument := *document
	copyDocument.Cues = append([]Cue(nil), document.Cues...)
	seen := make(map[string]struct{}, len(copyDocument.Cues))
	for _, cue := range copyDocument.Cues {
		if cue.ID == "" {
			return EditorDocument{}, errorsNew("editor snapshot contains a cue without an identity")
		}
		if _, duplicate := seen[cue.ID]; duplicate {
			return EditorDocument{}, errorsNew("editor snapshot contains duplicate cue identities")
		}
		seen[cue.ID] = struct{}{}
	}
	return copyDocument, nil
}

func (m Model) updateEditorSearch(msg editorSearchStreamMsg) (tea.Model, tea.Cmd) {
	next := tea.Cmd(nil)
	if msg.ok && !msg.event.Done {
		next = readEditorSearchCmd(msg.ch, msg.revision, msg.snapshotID, msg.query)
	}
	if !m.editor.active || msg.revision != m.editor.searchRevision || msg.snapshotID != m.editor.document.SnapshotID || msg.query != m.editor.input.Value() {
		return m, next
	}
	if !msg.ok {
		m.editor.searching = false
		m.editor.loading = false
		m.editor.err = ErrClosedStream
		m.status = ErrClosedStream.Error()
		if m.state == StateEditorLoading {
			m.state = StateEditor
		}
		return m, nil
	}
	if msg.event.Revision != msg.revision || msg.event.SnapshotID != msg.snapshotID || msg.event.Query != msg.query {
		return m, next
	}
	if msg.event.Err != nil {
		m.editor.err = msg.event.Err
		m.status = msg.event.Err.Error()
		if msg.event.Done {
			m.editor.searching = false
			m.editor.loading = false
			if m.state == StateEditorLoading {
				m.state = StateEditor
			}
		}
		return m, next
	}
	visible, err := editorVisibleIndices(m.editor.document.Cues, msg.event.CueIDs)
	if err != nil {
		m.editor.err = err
		m.status = err.Error()
		m.editor.searching = !msg.event.Done
		if msg.event.Done {
			m.editor.loading = false
			if m.state == StateEditorLoading {
				m.state = StateEditor
			}
		}
		return m, next
	}
	m.editor.visible = visible
	m.remapEditorFocus()
	m.editor.err = nil
	m.status = ""
	m.editor.searching = !msg.event.Done
	if msg.event.Done {
		m.editor.loading = false
		if m.state == StateEditorLoading {
			m.state = StateEditor
		}
	}
	return m, next
}

func editorVisibleIndices(cues []Cue, ids []string) ([]int, error) {
	indices := make(map[string]int, len(cues))
	for index, cue := range cues {
		indices[cue.ID] = index
	}
	visible := make([]int, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		index, ok := indices[id]
		if !ok {
			return nil, errorsNew("editor filter returned an unknown cue identity")
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		visible = append(visible, index)
	}
	sort.Ints(visible)
	return visible, nil
}

func (m *Model) remapEditorFocus() {
	m.editor.lineOffset = 0
	if len(m.editor.visible) == 0 {
		m.editor.focus, m.editor.top = 0, 0
		return
	}
	anchor := m.editor.anchor
	if anchor < 0 {
		m.editor.focus, m.editor.top = 0, 0
		m.editor.anchor = m.editor.visible[0]
		return
	}
	index := sort.SearchInts(m.editor.visible, anchor)
	if index >= len(m.editor.visible) {
		index = len(m.editor.visible) - 1
	}
	m.editor.focus = index
	m.editor.anchor = m.editor.visible[index]
	page := m.editorPageSize()
	m.editor.top = max(0, min(m.editor.top, len(m.editor.visible)-1))
	if index < m.editor.top {
		m.editor.top = index
	} else if index >= m.editor.top+page {
		m.editor.top = max(0, index-page+1)
	}
}

func (m Model) refreshEditor(preserveMarks bool, after editorAfterAction) (tea.Model, tea.Cmd) {
	if m.editorBackend == nil || m.editor.document.Path == "" {
		m.editor.err = errorsNew("editor refresh is unavailable")
		m.status = m.editor.err.Error()
		return m, nil
	}
	m.captureEditorAnchor()
	if m.editor.loadCancel != nil {
		m.editor.loadCancel()
	}
	if m.editor.searchCancel != nil {
		m.editor.searchCancel()
	}
	m.editor.openRevision++
	m.editor.loadMode = editorLoadRefresh
	m.editor.loading = true
	m.editor.searching = false
	m.editor.stale = true
	m.editor.outerStale = true
	m.editor.preserveMarks = preserveMarks
	m.editor.afterSave = after
	m.editor.err = nil
	m.editor.lineOffset = 0
	if m.state != StateSummary {
		m.state = StateEditorLoading
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.editor.loadCancel = cancel
	req := EditorRefreshRequest{Path: m.editor.document.Path, Revision: m.editor.openRevision}
	return m, startEditorRefreshCmd(m.editorBackend, ctx, req)
}

func (m Model) exitEditor(force bool) (tea.Model, tea.Cmd) {
	if m.editorDirty() && !force {
		return m.openEditorDirtyConfirmation(confirmEditorDirtyExit)
	}
	if m.editor.loadCancel != nil {
		m.editor.loadCancel()
	}
	if m.editor.searchCancel != nil {
		m.editor.searchCancel()
	}
	returnState := m.editor.returnState
	if returnState != StateResults && returnState != StateSelection {
		returnState = StateResults
	}
	outerStale := m.editor.outerStale
	returnPath := m.editor.document.Path
	editorInput := m.editor.input
	editorInput.SetValue("")
	editorInput.Blur()
	m.editor = editorSession{
		input: editorInput, marks: make(map[string]bool), anchor: -1,
		openRevision: m.editor.openRevision, searchRevision: m.editor.searchRevision,
		mutationRevision: m.editor.mutationRevision,
	}
	m.summaryBack = 0
	m.summary = nil
	m.err = nil
	m.status = ""
	if outerStale {
		m.returnAfterRescan = returnState
		m.returnPathAfterRescan = returnPath
		return m.startRescan(false)
	}
	m.state = returnState
	m.input.Blur()
	m.rebuildCards()
	return m, nil
}

func (m Model) recoveryGated() bool {
	return m.retainedUndo != "" || m.pendingRecovery != ""
}

// editorScopedUndoReady permits an ordinary exact editor undo and a retry of
// that same transaction after a partial undo. A pending apply, or a retained
// transaction with any other identity, must never fall through to broad
// workspace undo based on stale UI state.
func (m Model) editorScopedUndoReady() bool {
	if m.editorBackend == nil || m.editor.undoID == "" || m.pendingRecovery != "" || m.editorDirty() {
		return false
	}
	return m.retainedUndo == "" || m.retainedUndo == m.editor.undoID
}

func (m Model) selectedCount() int {
	n := 0
	for _, selected := range m.selected {
		if selected {
			n++
		}
	}
	return n
}

func (m Model) openDeleteConfirmation(scope DeleteScope) (tea.Model, tea.Cmd) {
	request := m.buildMutationRequest(scope)
	if len(request.Targets) == 0 {
		return m, nil
	}
	cues := 0
	for _, target := range request.Targets {
		cues += len(target.CueIDs)
	}
	m.confirm = confirmation{
		kind:     confirmDelete,
		question: fmt.Sprintf("Delete %d matching cues from %d files?", cues, len(request.Targets)),
		back:     m.state,
		request:  request,
	}
	m.state = StateConfirmation
	return m, nil
}

func (m Model) buildMutationRequest(scope DeleteScope) MutationRequest {
	files := m.result.Files
	if scope == DeleteFocused {
		if f, ok := m.focusedFile(); ok {
			files = []File{f}
		} else {
			files = nil
		}
	}
	targets := make([]MutationTarget, 0, len(files))
	for _, f := range files {
		if scope == DeleteSelected && !m.selected[f.ID] {
			continue
		}
		if len(f.MatchIDs) == 0 {
			continue
		}
		targets = append(targets, MutationTarget{FileID: f.ID, Path: f.Path, CueIDs: append([]string(nil), f.MatchIDs...)})
	}
	return MutationRequest{Scope: scope, Query: m.input.Value(), Revision: m.queryRevision, Targets: targets}
}

func (m Model) updateConfirmationKey(key string) (tea.Model, tea.Cmd) {
	if isEditorDirtyConfirmation(m.confirm.kind) {
		return m.updateEditorDirtyConfirmationKey(key)
	}
	if (m.confirm.kind == confirmEditorSave || m.confirm.kind == confirmEditorUndo) &&
		(key == "q" || key == "ctrl+c") && m.editorDirty() {
		return m.openEditorDirtyConfirmation(confirmEditorDirtyQuit)
	}
	switch key {
	case "q", "ctrl+c":
		return m.quit()
	case "n", "esc":
		m.state = m.confirm.back
		m.confirm = confirmation{}
		return m, nil
	case "y", "enter":
		if m.tooSmall() {
			return m, nil
		}
		confirm := m.confirm
		m.confirm = confirmation{}
		m.state = StateMutation
		m.cancelRequested = false
		m.mutProg = MutationProgress{}
		ctx, cancel := context.WithCancel(context.Background())
		m.mutationCancel = cancel
		switch confirm.kind {
		case confirmDelete:
			if confirm.request.Revision != m.queryRevision || !m.actionsReady() {
				cancel()
				m.state = confirm.back
				m.status = "Results changed; review the refreshed matches before deleting."
				return m, nil
			}
			m.op = opDelete
			return m, startMutationCmd(m.backend, ctx, confirm.request, 0)
		case confirmUndo:
			m.op = opUndo
			return m, startUndoCmd(m.backend, ctx, 0)
		case confirmEditorSave:
			if confirm.request.Source != MutationSourceEditor || confirm.request.Scope != DeleteEditor ||
				confirm.request.SnapshotID != m.editor.document.SnapshotID ||
				confirm.request.Revision != m.editor.mutationRevision+1 || !m.editorActionsReady() {
				cancel()
				m.state = StateEditor
				m.status = "Editor snapshot changed; review the cues before saving."
				return m, nil
			}
			m.op = opEditorSave
			m.editor.mutationRevision = confirm.request.Revision
			m.editor.stale = true
			return m, startMutationCmd(m.backend, ctx, confirm.request, confirm.request.Revision)
		case confirmEditorUndo:
			if m.editorBackend == nil || m.editorDirty() || !m.editorScopedUndoReady() {
				cancel()
				m.state = confirm.back
				m.status = "Scoped editor undo is no longer available."
				return m, nil
			}
			m.op = opEditorUndo
			m.editor.mutationRevision++
			revision := m.editor.mutationRevision
			m.editor.stale = true
			req := EditorUndoRequest{Path: m.editor.document.Path, UndoID: m.editor.undoID, Revision: revision}
			return m, startEditorUndoCmd(m.editorBackend, ctx, req)
		case confirmRecoveryRestore, confirmRecoveryDiscard:
			m.op = opRecovery
			m.recoveryAction = confirm.recovery.Action
			m.activeRecovery = confirm.recovery
			m.recoveryLoading = true
			return m, startRecoveryCmd(m.recoveryBackend, ctx, confirm.recovery)
		}
	}
	return m, nil
}

func isEditorDirtyConfirmation(kind confirmationKind) bool {
	return kind == confirmEditorDirtyExit || kind == confirmEditorDirtyQuit || kind == confirmEditorDirtyReload
}

func (m Model) updateEditorDirtyConfirmationKey(key string) (tea.Model, tea.Cmd) {
	kind := m.confirm.kind
	after := editorAfterExit
	if kind == confirmEditorDirtyQuit {
		after = editorAfterQuit
	} else if kind == confirmEditorDirtyReload {
		after = editorAfterReload
	}
	switch key {
	case "c", "n", "esc", "q", "ctrl+c":
		m.confirm = confirmation{}
		m.state = StateEditor
		return m, nil
	case "s":
		m.confirm = confirmation{}
		m.state = StateEditor
		// The three-way prompt chooses the disposition; the detailed save
		// confirmation still discloses hidden marks, zero-cue outcomes, and undo
		// replacement before the destructive request is authorized.
		return m.openEditorSaveConfirmation(after)
	case "d":
		m.confirm = confirmation{}
		m.editor.marks = make(map[string]bool)
		m.editor.afterSave = editorAfterStay
		switch after {
		case editorAfterQuit:
			return m.quit()
		case editorAfterReload:
			m.state = StateEditor
			return m.refreshEditor(false, editorAfterStay)
		default:
			m.state = StateEditor
			return m.exitEditor(true)
		}
	}
	return m, nil
}

func (m Model) updateRecoveryKey(key string) (tea.Model, tea.Cmd) {
	if len(m.recoveries) == 0 {
		if key == "q" || key == "ctrl+c" {
			return m.quit()
		}
		return m, nil
	}
	current := m.recoveries[0]
	switch key {
	case "q", "ctrl+c":
		return m.quit()
	case "r":
		m.confirm = confirmation{
			kind:     confirmRecoveryRestore,
			question: fmt.Sprintf("Restore %d files from recovery transaction %s?", current.Files, current.ID),
			back:     StateRecovery,
			recovery: RecoveryRequest{ID: current.ID, Action: RecoveryRestore},
		}
		m.state = StateConfirmation
	case "d":
		m.confirm = confirmation{
			kind:     confirmRecoveryDiscard,
			question: fmt.Sprintf("Permanently discard recovery transaction %s?", current.ID),
			back:     StateRecovery,
			recovery: RecoveryRequest{ID: current.ID, Action: RecoveryDiscard},
		}
		m.state = StateConfirmation
	}
	return m, nil
}

func (m Model) startRescan(behindSummary bool) (tea.Model, tea.Cmd) {
	if m.discoveryCancel != nil {
		m.discoveryCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.discoveryCancel = cancel
	m.discovering = true
	m.discDone = false
	m.searching = false
	m.refreshBehindSummary = behindSummary
	if !behindSummary {
		m.state = StateDiscovery
	}
	return m, startDiscoveryCmd(m.backend, ctx)
}

func (m Model) updateDiscovery(msg discoveryStreamMsg) (tea.Model, tea.Cmd) {
	if !msg.ok {
		m.discovering = false
		if !m.discDone {
			m.err = ErrClosedStream
			m.status = ErrClosedStream.Error()
		}
		m.refreshBehindSummary = false
		return m, nil
	}
	m.discProg = msg.event
	if msg.event.Err != nil {
		m.err = msg.event.Err
		m.status = msg.event.Err.Error()
	}
	if msg.event.Discovery != nil {
		m.discovery = *msg.event.Discovery
	}
	if !msg.event.Done {
		return m, readDiscoveryCmd(msg.ch)
	}
	m.discovering = false
	m.discDone = msg.event.Discovery != nil && msg.event.Err == nil
	if !m.discDone {
		m.refreshBehindSummary = false
		return m, nil
	}
	m.err = nil
	m.status = ""
	if !m.refreshBehindSummary {
		m.focus = 0
	}
	m.clearSelection()
	if m.normalizedEmptyQuery() {
		m.result = SearchResult{}
		m.displayedRevision = m.queryRevision
		m.rebuildCards()
		if !m.refreshBehindSummary {
			if m.returnAfterRescan == StateResults || m.returnAfterRescan == StateSelection {
				m.restoreOuterFocus(m.returnPathAfterRescan)
				m.state = m.returnAfterRescan
				m.returnAfterRescan = 0
				m.returnPathAfterRescan = ""
				m.input.Blur()
				m.rebuildCards()
				return m, nil
			}
			m.state = StateSearch
			return m, m.input.Focus()
		}
		m.refreshBehindSummary = false
		return m, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.searchCancel = cancel
	m.searching = true
	req := SearchRequest{Query: m.input.Value(), Revision: m.queryRevision}
	if !m.refreshBehindSummary && m.returnAfterRescan == 0 {
		m.state = StateSearch
	}
	return m, startSearchCmd(m.backend, ctx, req)
}

func (m Model) updateSearch(msg searchStreamMsg) (tea.Model, tea.Cmd) {
	if !msg.ok {
		if msg.revision == m.queryRevision && m.searching {
			m.searching = false
			m.err = ErrClosedStream
			m.status = ErrClosedStream.Error()
			m.refreshBehindSummary = false
			if m.returnAfterRescan != 0 {
				m.returnAfterRescan = 0
				m.returnPathAfterRescan = ""
				m.state = StateSearch
				return m, m.input.Focus()
			}
		}
		return m, nil
	}
	// Always drain stale streams so a backend sender cannot be stranded.
	next := tea.Cmd(nil)
	if !msg.event.Done {
		next = readSearchCmd(msg.ch, msg.revision)
	}
	if msg.revision != m.queryRevision {
		return m, next
	}
	if msg.event.Err != nil {
		m.err = msg.event.Err
		m.status = msg.event.Err.Error()
		m.searching = !msg.event.Done
		if msg.event.Done {
			m.refreshBehindSummary = false
			if m.returnAfterRescan != 0 {
				m.returnAfterRescan = 0
				m.returnPathAfterRescan = ""
				m.state = StateSearch
				return m, m.input.Focus()
			}
		}
		return m, next
	}
	if msg.event.Result.Revision != m.queryRevision {
		return m, next
	}
	m.result = msg.event.Result
	m.displayedRevision = msg.event.Result.Revision
	m.searching = !msg.event.Done
	m.err = nil
	m.status = ""
	m.clampFocus()
	m.rebuildCards()
	if msg.event.Done && m.refreshBehindSummary {
		// Keep the summary visible, but make its embedded workspace snapshot
		// current before allowing the user to leave it.
		m.refreshBehindSummary = false
	}
	if msg.event.Done && !m.refreshBehindSummary && (m.returnAfterRescan == StateResults || m.returnAfterRescan == StateSelection) {
		m.restoreOuterFocus(m.returnPathAfterRescan)
		m.state = m.returnAfterRescan
		m.returnAfterRescan = 0
		m.returnPathAfterRescan = ""
		m.input.Blur()
		m.rebuildCards()
	}
	return m, next
}

func (m Model) updateMutation(msg mutationStreamMsg) (tea.Model, tea.Cmd) {
	editorOperation := m.op == opEditorSave || m.op == opEditorUndo
	if editorOperation && msg.revision != m.editor.mutationRevision {
		if msg.ok && !msg.event.Done {
			return m, readMutationCmd(msg.ch, msg.revision)
		}
		return m, nil
	}
	if !msg.ok {
		if m.state == StateMutation {
			summary := &MutationSummary{Operation: m.operationName(), Failed: 1, UndoAvailable: m.undoAvailable}
			if editorOperation {
				return m.finishEditorMutation(summary, ErrClosedStream, m.op)
			}
			m.finishMutation(summary, ErrClosedStream)
		}
		return m, nil
	}
	if msg.event.Progress != nil {
		progress := *msg.event.Progress
		// Completed/Total are absolute; outcome counts are per-event deltas.
		m.mutProg.Completed = progress.Completed
		m.mutProg.Total = progress.Total
		m.mutProg.CurrentPath = progress.CurrentPath
		m.mutProg.Succeeded += progress.Succeeded
		m.mutProg.Skipped += progress.Skipped
		m.mutProg.Failed += progress.Failed
	}
	if msg.event.Err != nil {
		m.err = msg.event.Err
	}
	if !msg.event.Done {
		return m, readMutationCmd(msg.ch, msg.revision)
	}
	summary := msg.event.Summary
	if summary == nil {
		summary = &MutationSummary{Operation: m.operationName(), Failed: 1, UndoAvailable: m.undoAvailable}
	}
	op := m.op
	if editorOperation {
		return m.finishEditorMutation(summary, msg.event.Err, op)
	}
	m.finishMutation(summary, msg.event.Err)
	if op != opRecovery {
		if m.summaryBack == StateEditor {
			m.editor.marks = make(map[string]bool)
			m.editor.stale = true
			m.editor.outerStale = true
			return m.refreshEditor(false, editorAfterStay)
		}
		updated, cmd := m.startRescan(true)
		return updated, cmd
	}
	return m, nil
}

func (m Model) finishEditorMutation(summary *MutationSummary, opErr error, op operationKind) (tea.Model, tea.Cmd) {
	priorUndoID := m.editor.undoID
	m.finishMutation(summary, opErr)
	accepted := summary.Succeeded > 0 || (summary.RecoveryID != "" && normalizedRecoveryKind(*summary) == RecoveryGateApply)
	if op == opEditorSave {
		// A consumed or possibly-consumed snapshot can never safely rebase old
		// staged cue identities. The exact refresh always starts a fresh review,
		// including after failed or conflicted saves.
		m.editor.marks = make(map[string]bool)
		if summary.RecoveryID != "" && normalizedRecoveryKind(*summary) == RecoveryGateApply {
			// The authoritative UndoID during a pending apply may describe the
			// previous global undo point, not this editor path. Never present it as
			// a scoped editor undo until a completed save explicitly creates one.
			m.editor.undoID = ""
		} else if accepted {
			m.editor.undoID = summary.UndoID
		} else {
			m.editor.undoID = priorUndoID
		}
	} else {
		if opErr == nil || summary.Succeeded > 0 || summary.UndoID != "" || summary.RecoveryID != "" {
			m.editor.undoID = summary.UndoID
		} else {
			m.editor.undoID = priorUndoID
		}
	}
	m.editor.stale = true
	m.editor.outerStale = true
	m.summaryBack = StateEditor
	after := m.editor.afterSave
	if op == opEditorUndo {
		after = editorAfterStay
	}
	return m.refreshEditor(false, after)
}

func (m *Model) finishMutation(summary *MutationSummary, opErr error) {
	copySummary := *summary
	if copySummary.Operation == "" {
		copySummary.Operation = m.operationName()
	}
	m.summary = &copySummary
	recoveryKind := normalizedRecoveryKind(copySummary)
	if copySummary.RecoveryID != "" && recoveryKind == RecoveryGateApply {
		// An explicit pending apply identity is authoritative even when no file
		// reached a clean success result. It is distinct from the current undo
		// point and must be resolved through RecoveryBackend.
		copySummary.RecoveryKind = RecoveryGateApply
		m.pendingRecovery = copySummary.RecoveryID
		m.undoAvailable = copySummary.UndoAvailable
		m.retainedUndo = ""
		m.summary = &copySummary
	} else if m.op == opUndo || m.op == opEditorUndo {
		if opErr == nil || copySummary.Succeeded > 0 || copySummary.UndoAvailable || copySummary.RecoveryID != "" {
			if copySummary.RecoveryID != "" {
				copySummary.RecoveryKind = RecoveryGateUndo
			}
			m.undoAvailable = copySummary.UndoAvailable
			m.retainedUndo = copySummary.RecoveryID
			m.pendingRecovery = ""
			m.summary = &copySummary
		} else {
			// A failed undo attempt is not evidence that its durable backup was
			// consumed. Preserve the gate until the backend reports a durable
			// outcome or the user explicitly discards that exact recovery ID.
			copySummary.UndoAvailable = m.undoAvailable
			m.applyKnownRecoveryGate(&copySummary)
			m.summary = &copySummary
		}
	} else if copySummary.Succeeded > 0 {
		// A successful newer operation supersedes the previous undo point.
		m.undoAvailable = copySummary.UndoAvailable
		m.retainedUndo = copySummary.RecoveryID
		m.pendingRecovery = ""
		if copySummary.RecoveryID != "" {
			copySummary.RecoveryKind = RecoveryGateUndo
		}
		m.summary = &copySummary
	} else {
		// Failed/no-op delete attempts do not destroy the previous undo point.
		copySummary.UndoAvailable = m.undoAvailable
		m.applyKnownRecoveryGate(&copySummary)
		m.summary = &copySummary
	}
	if opErr != nil {
		m.status = opErr.Error()
	}
	m.state = StateSummary
	if m.mutationCancel != nil {
		m.mutationCancel()
		m.mutationCancel = nil
	}
	m.clearSelection()
}

func normalizedRecoveryKind(summary MutationSummary) RecoveryGateKind {
	if summary.RecoveryID == "" {
		return ""
	}
	switch summary.RecoveryKind {
	case "", RecoveryGateUndo:
		return RecoveryGateUndo
	case RecoveryGateApply:
		return RecoveryGateApply
	default:
		// An exact but unknown recovery kind must fail closed through the
		// identity-based recovery API, never through broad one-level undo.
		return RecoveryGateApply
	}
}

func (m Model) applyKnownRecoveryGate(summary *MutationSummary) {
	if m.pendingRecovery != "" {
		summary.RecoveryID = m.pendingRecovery
		summary.RecoveryKind = RecoveryGateApply
		return
	}
	if m.retainedUndo != "" {
		summary.RecoveryID = m.retainedUndo
		summary.RecoveryKind = RecoveryGateUndo
	}
}

func (m Model) operationName() string {
	switch m.op {
	case opUndo, opEditorUndo:
		return "undo"
	case opRecovery:
		return "recovery"
	default:
		return "delete"
	}
}

func (m Model) updateRecovery(msg recoveryStreamMsg) (tea.Model, tea.Cmd) {
	request := msg.request
	if request.ID == "" {
		// Only tests and embedders constructing an internal message can omit the
		// request. Production stream commands always carry it end-to-end.
		request = m.activeRecovery
	}
	if m.activeRecovery.ID != "" && request != m.activeRecovery {
		// Drain a stale stream without letting it resolve a different recovery
		// operation that has since become active.
		if !msg.ok || msg.event.Done {
			return m, nil
		}
		return m, readRecoveryCmd(msg.ch, request)
	}
	priorRetainedUndo := m.retainedUndo
	priorPendingRecovery := m.pendingRecovery
	if !msg.ok {
		m.recoveryLoading = false
		m.activeRecovery = RecoveryRequest{}
		m.routeRecoveryFailure(request, priorRetainedUndo, priorPendingRecovery, nil)
		m.status = ErrClosedStream.Error()
		if request.ID != "" && request.ID == priorPendingRecovery && m.recoveryIndex(request.ID) < 0 && request.Action == RecoveryRestore {
			return m.postRecoveryRefresh(true)
		}
		if m.summaryBack == StateEditor && request.ID != "" && (request.ID == priorPendingRecovery || request.ID == priorRetainedUndo) {
			return m.postRecoveryRefresh(false)
		}
		return m, nil
	}
	if msg.event.Progress != nil {
		m.recoveryProg = *msg.event.Progress
	}
	if msg.event.Err != nil {
		m.err = msg.event.Err
		m.status = msg.event.Err.Error()
	}
	if !msg.event.Done {
		return m, readRecoveryCmd(msg.ch, request)
	}
	m.recoveryLoading = false
	if msg.event.Summary != nil && msg.event.Summary.Undo != nil {
		m.applyUndoSnapshot(*msg.event.Summary.Undo)
	}
	m.activeRecovery = RecoveryRequest{}
	if m.mutationCancel != nil {
		m.mutationCancel()
		m.mutationCancel = nil
	}
	listedIndex := m.recoveryIndex(request.ID)
	pendingRequest := request.ID != "" && request.ID == priorPendingRecovery && listedIndex < 0
	undoRequest := request.ID != "" && request.ID == priorRetainedUndo && listedIndex < 0
	if msg.event.Err != nil {
		m.routeRecoveryFailure(request, priorRetainedUndo, priorPendingRecovery, msg.event.Summary)
		if pendingRequest && request.Action == RecoveryRestore {
			return m.postRecoveryRefresh(true)
		}
		if m.summaryBack == StateEditor && (pendingRequest || undoRequest) {
			return m.postRecoveryRefresh(false)
		}
		return m, nil
	}
	m.err = nil
	m.status = ""
	if msg.event.Summary != nil && msg.event.Summary.Retained {
		m.recoveryLoading = false
		if pendingRequest || undoRequest {
			if pendingRequest {
				m.pendingRecovery = priorPendingRecovery
			} else if msg.event.Summary.Undo == nil {
				m.retainedUndo = priorRetainedUndo
				m.undoAvailable = true
			}
			m.setRecoveryOutcomeSummary(request, msg.event.Summary)
			m.state = StateSummary
			m.status = fmt.Sprintf("Recovery remains unresolved: %d restored, %d conflicted/skipped, %d failed.", msg.event.Summary.Succeeded, msg.event.Summary.Skipped, msg.event.Summary.Failed)
			if pendingRequest && request.Action == RecoveryRestore {
				return m.postRecoveryRefresh(true)
			}
			if m.summaryBack == StateEditor {
				return m.postRecoveryRefresh(false)
			}
			return m, nil
		}
		m.state = StateRecovery
		m.status = fmt.Sprintf("Recovery remains unresolved: %d restored, %d conflicted/skipped, %d failed.", msg.event.Summary.Succeeded, msg.event.Summary.Skipped, msg.event.Summary.Failed)
		return m, nil
	}
	if listedIndex >= 0 {
		// Startup recovery items are resolved by exact identity. In particular,
		// a separate retained current undo must not be mistaken for this item.
		m.recoveries = append(m.recoveries[:listedIndex], m.recoveries[listedIndex+1:]...)
		if len(m.recoveries) > 0 {
			m.state = StateRecovery
			return m, nil
		}
		m.state = StateDiscovery
		m.discovering = true
		return m, startDiscoveryCmd(m.backend, context.Background())
	}
	if pendingRequest {
		m.pendingRecovery = ""
		m.setRecoveryOutcomeSummary(request, msg.event.Summary)
		m.state = StateSummary
		if request.Action == RecoveryRestore {
			return m.postRecoveryRefresh(true)
		}
		if m.summaryBack == StateEditor {
			return m.postRecoveryRefresh(false)
		}
		return m, nil
	}

	// Resolving the retained current undo happens after startup and therefore
	// has no entry in m.recoveries. Without an authoritative snapshot, only an
	// exact request-ID match is allowed to clear the gate.
	if undoRequest {
		if msg.event.Summary == nil || msg.event.Summary.Undo == nil {
			m.retainedUndo = ""
			m.undoAvailable = false
		}
		if m.summaryBack == StateEditor && m.editor.undoID == request.ID {
			// This exact retained transaction was successfully restored or
			// discarded. UndoSnapshot deliberately does not expose an unrelated
			// global transaction ID, so the editor must stop advertising the
			// retired local ID rather than guessing new provenance.
			m.editor.undoID = ""
		}
		m.setRecoveryOutcomeSummary(request, msg.event.Summary)
		m.state = StateSummary
		if m.summaryBack == StateEditor {
			return m.postRecoveryRefresh(false)
		}
		return m, nil
	}

	// An unmatched completion must not silently consume either safety gate.
	m.state = StateRecovery
	m.status = "Recovery completion did not match the requested recovery set; no item was dismissed."
	return m, nil
}

func (m Model) postRecoveryRefresh(rootRescan bool) (tea.Model, tea.Cmd) {
	if m.summaryBack == StateEditor {
		m.editor.marks = make(map[string]bool)
		m.editor.stale = true
		m.editor.outerStale = true
		m.state = StateSummary
		return m.refreshEditor(false, editorAfterStay)
	}
	if rootRescan {
		return m.startRescan(true)
	}
	return m, nil
}

func (m *Model) applyUndoSnapshot(snapshot UndoSnapshot) {
	m.retainedUndo = snapshot.RetainedUndoID
	m.undoAvailable = snapshot.Available || snapshot.RetainedUndoID != ""
}

func (m *Model) routeRecoveryFailure(request RecoveryRequest, priorRetainedUndo, priorPendingRecovery string, summary *RecoverySummary) {
	if request.ID != "" && request.ID == priorPendingRecovery && m.recoveryIndex(request.ID) < 0 {
		m.pendingRecovery = priorPendingRecovery
		m.setRecoveryOutcomeSummary(request, summary)
		m.markRecoverySummaryFailedIfEmpty()
		m.state = StateSummary
		return
	}
	if request.ID != "" && request.ID == priorRetainedUndo && m.recoveryIndex(request.ID) < 0 {
		// Retained-undo recovery is invoked from results/summary, not the
		// startup recovery list. An error is not proof that the exact recovery
		// set was safely retired, so fail closed even if an intermediate state
		// snapshot no longer calls it the current undo.
		m.retainedUndo = priorRetainedUndo
		m.undoAvailable = true
		m.setRecoveryOutcomeSummary(request, summary)
		m.markRecoverySummaryFailedIfEmpty()
		m.state = StateSummary
		return
	}
	m.state = StateRecovery
}

func (m *Model) markRecoverySummaryFailedIfEmpty() {
	if m.summary != nil && m.summary.Succeeded == 0 && m.summary.Skipped == 0 && m.summary.Failed == 0 {
		m.summary.Failed = 1
	}
}

func (m *Model) setRecoveryOutcomeSummary(request RecoveryRequest, recovery *RecoverySummary) {
	operation := "restore recovery"
	if request.Action == RecoveryDiscard {
		operation = "discard recovery"
	}
	summary := &MutationSummary{Operation: operation, UndoAvailable: m.undoAvailable}
	if recovery != nil {
		summary.Succeeded = recovery.Succeeded
		summary.Skipped = recovery.Skipped
		summary.Failed = recovery.Failed
	} else {
		summary.Failed = 1
	}
	m.applyKnownRecoveryGate(summary)
	m.summary = summary
}

func (m Model) recoveryIndex(id string) int {
	if id == "" {
		return -1
	}
	for index := range m.recoveries {
		if m.recoveries[index].ID == id {
			return index
		}
	}
	return -1
}

// Stream plumbing. Each command receives exactly one event, preserving order
// without blocking Bubble Tea's update loop.
type fatalMsg struct{ err error }
type recoveriesMsg struct {
	items []RecoveryItem
	err   error
}
type discoveryStreamMsg struct {
	event DiscoveryEvent
	ch    <-chan DiscoveryEvent
	ok    bool
}
type searchStreamMsg struct {
	event    SearchEvent
	ch       <-chan SearchEvent
	revision uint64
	ok       bool
}
type editorOpenStreamMsg struct {
	event    EditorOpenEvent
	ch       <-chan EditorOpenEvent
	revision uint64
	mode     editorLoadMode
	fileID   string
	path     string
	ok       bool
}
type editorSearchStreamMsg struct {
	event      EditorSearchEvent
	ch         <-chan EditorSearchEvent
	revision   uint64
	snapshotID string
	query      string
	ok         bool
}
type mutationStreamMsg struct {
	event    MutationEvent
	ch       <-chan MutationEvent
	revision uint64
	ok       bool
}
type recoveryStreamMsg struct {
	event   RecoveryEvent
	ch      <-chan RecoveryEvent
	request RecoveryRequest
	ok      bool
}

func listRecoveriesCmd(backend RecoveryBackend) tea.Cmd {
	return func() tea.Msg {
		items, err := backend.ListRecoveries(context.Background())
		return recoveriesMsg{items: items, err: err}
	}
}

func startDiscoveryCmd(backend Backend, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		ch := backend.Discover(ctx)
		event, ok := <-ch
		return discoveryStreamMsg{event: event, ch: ch, ok: ok}
	}
}

func readDiscoveryCmd(ch <-chan DiscoveryEvent) tea.Cmd {
	return func() tea.Msg { event, ok := <-ch; return discoveryStreamMsg{event: event, ch: ch, ok: ok} }
}

func startSearchCmd(backend Backend, ctx context.Context, request SearchRequest) tea.Cmd {
	return func() tea.Msg {
		ch := backend.Search(ctx, request)
		event, ok := <-ch
		return searchStreamMsg{event: event, ch: ch, revision: request.Revision, ok: ok}
	}
}

func readSearchCmd(ch <-chan SearchEvent, revision uint64) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-ch
		return searchStreamMsg{event: event, ch: ch, revision: revision, ok: ok}
	}
}

func startEditorOpenCmd(backend EditorBackend, ctx context.Context, request EditorOpenRequest) tea.Cmd {
	return func() tea.Msg {
		ch := backend.OpenEditor(ctx, request)
		event, ok := <-ch
		return editorOpenStreamMsg{
			event: event, ch: ch, revision: request.Revision, mode: editorLoadOpen,
			fileID: request.FileID, path: request.Path, ok: ok,
		}
	}
}

func startEditorRefreshCmd(backend EditorBackend, ctx context.Context, request EditorRefreshRequest) tea.Cmd {
	return func() tea.Msg {
		ch := backend.RefreshEditor(ctx, request)
		event, ok := <-ch
		return editorOpenStreamMsg{
			event: event, ch: ch, revision: request.Revision, mode: editorLoadRefresh,
			path: request.Path, ok: ok,
		}
	}
}

func readEditorOpenCmd(ch <-chan EditorOpenEvent, revision uint64, mode editorLoadMode, fileID, path string) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-ch
		return editorOpenStreamMsg{event: event, ch: ch, revision: revision, mode: mode, fileID: fileID, path: path, ok: ok}
	}
}

func startEditorSearchCmd(backend EditorBackend, ctx context.Context, request EditorSearchRequest) tea.Cmd {
	return func() tea.Msg {
		ch := backend.SearchEditor(ctx, request)
		event, ok := <-ch
		return editorSearchStreamMsg{
			event: event, ch: ch, revision: request.Revision,
			snapshotID: request.SnapshotID, query: request.Query, ok: ok,
		}
	}
}

func readEditorSearchCmd(ch <-chan EditorSearchEvent, revision uint64, snapshotID, query string) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-ch
		return editorSearchStreamMsg{
			event: event, ch: ch, revision: revision,
			snapshotID: snapshotID, query: query, ok: ok,
		}
	}
}

func startMutationCmd(backend Backend, ctx context.Context, request MutationRequest, revision uint64) tea.Cmd {
	return func() tea.Msg {
		ch := backend.Mutate(ctx, request)
		event, ok := <-ch
		return mutationStreamMsg{event: event, ch: ch, revision: revision, ok: ok}
	}
}

func startUndoCmd(backend Backend, ctx context.Context, revision uint64) tea.Cmd {
	return func() tea.Msg {
		ch := backend.Undo(ctx)
		event, ok := <-ch
		return mutationStreamMsg{event: event, ch: ch, revision: revision, ok: ok}
	}
}

func startEditorUndoCmd(backend EditorBackend, ctx context.Context, request EditorUndoRequest) tea.Cmd {
	return func() tea.Msg {
		ch := backend.UndoEditor(ctx, request)
		event, ok := <-ch
		return mutationStreamMsg{event: event, ch: ch, revision: request.Revision, ok: ok}
	}
}

func readMutationCmd(ch <-chan MutationEvent, revision uint64) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-ch
		return mutationStreamMsg{event: event, ch: ch, revision: revision, ok: ok}
	}
}

func startRecoveryCmd(backend RecoveryBackend, ctx context.Context, request RecoveryRequest) tea.Cmd {
	return func() tea.Msg {
		ch := backend.ResolveRecovery(ctx, request)
		event, ok := <-ch
		return recoveryStreamMsg{event: event, ch: ch, request: request, ok: ok}
	}
}

func readRecoveryCmd(ch <-chan RecoveryEvent, request RecoveryRequest) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-ch
		return recoveryStreamMsg{event: event, ch: ch, request: request, ok: ok}
	}
}
