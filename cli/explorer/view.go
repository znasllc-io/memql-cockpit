package explorer

import (
	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/editor"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// View is the Explorer tab — tree on the left, editor on the right.
type View struct {
	Tree       *Tree
	Editor     *editor.Editor
	Completion *editor.CompletionPopup
	Hover      *editor.HoverTooltip
	Theme      ui.Theme
	FocusTree  bool // true = tree has focus, false = editor has focus

	// GatedMessage, when non-empty, causes Draw to render a centered
	// "not available" message INSTEAD of the tree/editor. The app
	// layer sets this when the user's selected cluster isn't in
	// stateConnected -- there's no dispatcher to run queries against,
	// so there's nothing meaningful for the Explorer to show.
	GatedMessage string

	// Agents caches the materialized v1:agents:agent rows from the
	// connected cluster, keyed by id. The app layer populates this via
	// SetAgents whenever queryAllAgents resolves; OnFileOpen for an
	// agent path renders the cached entry instead of round-tripping
	// to the server again. nil/empty -> no agents listed yet.
	agents map[string]AgentEntry

	// Callbacks for loading file content and Sense operations.
	// These are set by the app layer to call gRPC.
	OnFileOpen       func(path string) (source string, err error)
	OnRequestTokens  func(source string) []editor.SenseToken
	OnRequestDiags   func(source string) []editor.SenseDiagnostic
	OnRequestComplete func(source string, line, col int) []editor.CompletionItem
	OnRequestHover   func(source string, line, col int) string
}

// NewView creates an Explorer view with an empty tree and editor.
func NewView(theme ui.Theme) *View {
	return &View{
		Tree:       NewTree(theme, nil),
		Editor:     editor.NewEditor(nil, theme),
		Completion: editor.NewCompletionPopup(theme),
		Hover:      editor.NewHoverTooltip(theme),
		Theme:      theme,
		FocusTree:  true,
	}
}

// SetFiles populates the explorer tree with file entries.
func (v *View) SetFiles(files map[string][]FileEntry) {
	roots := BuildTree(DefaultCategories(), files)
	v.Tree = NewTree(v.Theme, roots)
	v.Tree.OnSelect = v.handleFileSelect
}

// SetAgents replaces the cached agent rows. The app layer calls this
// after queryAllAgents resolves. RenderAgent uses the cache; nil/empty
// just means "no agents listed yet" -- not an error condition.
func (v *View) SetAgents(agents map[string]AgentEntry) {
	v.agents = agents
}

// AgentByID returns a cached agent row by id, ok=false when missing.
// Exposed so the app layer can render the same entry without keeping
// its own copy.
func (v *View) AgentByID(id string) (AgentEntry, bool) {
	if v.agents == nil {
		return AgentEntry{}, false
	}
	a, ok := v.agents[id]
	return a, ok
}

// Draw renders the Explorer tab.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	if v.GatedMessage != "" {
		// Selected cluster isn't connected -- render a centered
		// message explaining how to fix it. Arrow-key nav / tab
		// switching still work because they don't go through here.
		screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
		title := " EXPLORER "
		screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))
		midY := bounds.Y + bounds.Height/2
		lineX := bounds.X + (bounds.Width-len(v.GatedMessage))/2
		if lineX < bounds.X+1 {
			lineX = bounds.X + 1
		}
		screen.DrawText(lineX, midY, bounds.Width-1, v.GatedMessage, v.Theme.SubtleStyle())
		return
	}
	// Split: left 30% tree, right 70% editor.
	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.3, MinSize: 20},
		{Flex: 0.7, MinSize: 30},
	})

	treeBounds := panes[0]
	editorBounds := panes[1]

	// Draw divider.
	divX := treeBounds.X + treeBounds.Width
	divStyle := v.Theme.SubtleStyle()
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		screen.SetCell(divX-1, y, '│', divStyle)
	}

	// Adjust tree width for divider.
	treeBounds.Width--

	// Tree title (gray when unfocused, accent when focused -- matches the
	// pane-title convention used in Clusters and Agents).
	treeTitleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.FocusTree {
		treeTitleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(treeBounds.X+1, bounds.Y, treeBounds.Width-1, " EXPLORER ", treeTitleStyle)

	// Editor title.
	editorTitleStyle := v.Theme.SubtleStyle().Bold(true)
	if !v.FocusTree {
		editorTitleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(editorBounds.X+1, bounds.Y, editorBounds.Width-1, " EDITOR ", editorTitleStyle)

	// Shift pane content down by one row to make room for the titles.
	treeBounds.Y++
	treeBounds.Height--
	editorBounds.Y++
	editorBounds.Height--

	// Tree.
	v.Tree.AdjustScroll(treeBounds.Height)
	v.Tree.Draw(screen, treeBounds)

	// Editor.
	v.Editor.Draw(screen, editorBounds)

	// Overlays.
	v.Completion.Draw(screen, editorBounds)
	v.Hover.Draw(screen, editorBounds)
}

// HandleEvent routes events to the focused pane.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// Tab key switches focus between tree and editor.
	if keyEv.Key() == tcell.KeyTab {
		v.FocusTree = !v.FocusTree
		v.Completion.Hide()
		v.Hover.Hide()
		return true
	}

	// Escape hides popups.
	if keyEv.Key() == tcell.KeyEscape {
		if v.Completion.Visible {
			v.Completion.Hide()
			return true
		}
		if v.Hover.Visible {
			v.Hover.Hide()
			return true
		}
	}

	// Completion popup takes priority when visible.
	if v.Completion.Visible {
		consumed, item := v.Completion.HandleEvent(keyEv)
		if item != nil {
			v.acceptCompletion(item)
		}
		if consumed {
			return true
		}
	}

	if v.FocusTree {
		return v.Tree.HandleEvent(ev)
	}

	// Editor focus: handle Ctrl+Space for completion.
	if keyEv.Key() == tcell.KeyCtrlSpace {
		v.triggerCompletion()
		return true
	}

	// Editor input.
	if v.Editor.HandleEvent(ev) {
		// After editing, request updated Sense data.
		if v.Editor.IsDirty() {
			v.refreshSense()
			v.Editor.ClearDirty()
		}
		return true
	}

	return false
}

func (v *View) handleFileSelect(node *TreeNode) {
	if v.OnFileOpen == nil {
		return
	}
	source, err := v.OnFileOpen(node.Path)
	if err != nil {
		return
	}
	buf := editor.NewBuffer(source, node.Name, false)
	v.Editor = editor.NewEditor(buf, v.Theme)
	v.FocusTree = false

	// Request initial Sense data.
	v.refreshSense()
}

func (v *View) refreshSense() {
	if v.Editor.Buffer == nil {
		return
	}
	source := v.Editor.Buffer.Source()

	if v.OnRequestTokens != nil {
		tokens := v.OnRequestTokens(source)
		v.Editor.SetTokens(tokens)
	}
	if v.OnRequestDiags != nil {
		diags := v.OnRequestDiags(source)
		v.Editor.SetDiagnostics(diags)
	}
}

func (v *View) triggerCompletion() {
	if v.Editor.Buffer == nil || v.OnRequestComplete == nil {
		return
	}
	source := v.Editor.Buffer.Source()
	items := v.OnRequestComplete(source, v.Editor.CursorLine+1, v.Editor.CursorCol+1)
	if len(items) == 0 {
		return
	}

	// Position the popup at the cursor.
	anchorX := v.Editor.CursorCol + 5 // offset for gutter
	anchorY := v.Editor.CursorLine - v.Editor.ScrollY
	v.Completion.Show(items, anchorX, anchorY)
}

func (v *View) acceptCompletion(item *editor.CompletionItem) {
	if v.Editor.Buffer == nil || item == nil {
		return
	}
	// Insert the completion text at the cursor.
	text := item.InsertText
	if text == "" {
		text = item.Label
	}
	for _, ch := range text {
		v.Editor.Buffer.InsertChar(v.Editor.CursorLine, v.Editor.CursorCol, ch)
		v.Editor.CursorCol++
	}
	v.refreshSense()
}
