package dsledit

// Authoring mode for the Editor tab (memql-cockpit#230). A second
// surface alongside the read-only pack browser: the user creates a
// local *bundle* (a directory of .memql files under ~/.memql/bundles)
// and edits it with full MemQL Sense IntelliSense -- syntax coloring,
// inline diagnostics, completion, and hover -- reusing cli/editor. The
// bundle stays on disk until it's validated / injected into a session
// (C3 #231).
//
// Three panes: BUNDLES (local bundle dirs) -> FILES (a bundle's .memql
// files) -> EDITOR (the editable buffer). Per the cockpit SDK-only
// rule every language call goes through the SDK sense.Client.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/sdk/go/sense"

	"github.com/znasllc-io/memql-cockpit/cli/editor"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

type authorFocus int

const (
	authorBundles authorFocus = iota
	authorFiles
	authorEditor
)

const authorFocusCount = 3

type promptKind int

const (
	promptNone promptKind = iota
	promptNewBundle
	promptNewFile
)

// authoring is the Editor tab's authoring-mode sub-view. Its own mutex
// guards all state, since the Sense refresh / completion / hover calls
// run on background goroutines and mutate the editor + popups while the
// UI goroutine draws.
type authoring struct {
	mu    sync.Mutex
	theme ui.Theme

	bundles    []string
	files      []string
	openBundle string
	openFile   string

	focus authorFocus

	bundleList ui.ListPane
	fileList   ui.ListPane

	editor     *editor.Editor
	completion *editor.CompletionPopup
	hover      *editor.HoverTooltip

	prompt      promptKind
	promptInput string
	promptErr   string

	senseClient func() *sense.Client
	onStatus    func(string)
	onRedraw    func()

	debounce *ui.Debouncer
}

func newAuthoring(theme ui.Theme, senseClient func() *sense.Client, onStatus func(string), onRedraw func()) *authoring {
	a := &authoring{
		theme:       theme,
		focus:       authorBundles,
		completion:  editor.NewCompletionPopup(theme),
		hover:       editor.NewHoverTooltip(theme),
		senseClient: senseClient,
		onStatus:    onStatus,
		onRedraw:    onRedraw,
	}
	a.bundleList.Render = a.renderBundleRow
	a.fileList.Render = a.renderFileRow
	a.bundleList.EmptyMessage = "No bundles. Press N to create one."
	a.fileList.EmptyMessage = "No files. Select a bundle, press N to add a file."
	a.debounce = ui.NewDebouncer(350*time.Millisecond, a.senseRefresh)
	a.refreshBundles()
	return a
}

func (a *authoring) status(msg string) {
	if a.onStatus != nil {
		a.onStatus(msg)
	}
}

func (a *authoring) redraw() {
	if a.onRedraw != nil {
		a.onRedraw()
	}
}

// refreshBundles reloads the local bundle list.
func (a *authoring) refreshBundles() {
	bundles, err := listBundles()
	if err != nil {
		a.status(fmt.Sprintf("list bundles: %v", err))
		return
	}
	a.mu.Lock()
	a.bundles = bundles
	a.mu.Unlock()
}

// --- rendering -------------------------------------------------------

func (a *authoring) Draw(screen *ui.Screen, bounds ui.Rect) {
	a.mu.Lock()
	defer a.mu.Unlock()

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.22, MinSize: 18},
		{Flex: 0.24, MinSize: 18},
		{Flex: 0.54, MinSize: 36},
	})
	bundleBounds, fileBounds, editorBounds := panes[0], panes[1], panes[2]

	divStyle := a.theme.SubtleStyle()
	for _, x := range []int{bundleBounds.X + bundleBounds.Width - 1, fileBounds.X + fileBounds.Width - 1} {
		for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
			screen.SetCell(x, y, '│', divStyle)
		}
	}
	bundleBounds.Width--
	fileBounds.Width--

	a.drawBundleList(screen, bundleBounds)
	a.drawFileList(screen, fileBounds)
	a.drawEditor(screen, editorBounds)
}

func (a *authoring) drawBundleList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, a.theme.BaseStyle())
	ui.PaneTitle{
		Title:   "BUNDLES",
		Counter: ui.FormatCursor(a.bundleList.Selected, len(a.bundles)),
		Focused: a.focus == authorBundles,
	}.Draw(screen, bounds, a.theme)
	a.bundleList.Count = len(a.bundles)
	a.bundleList.Focused = a.focus == authorBundles
	a.bundleList.Draw(screen, paneChromeBounds(bounds), a.theme)

	if a.prompt == promptNewBundle {
		a.drawPrompt(screen, bounds, "New bundle name")
	} else {
		ui.DrawBottom(screen, bounds, a.theme.SubtleStyle(), 1, a.hintsForBundles())
	}
}

func (a *authoring) drawFileList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, a.theme.BaseStyle())
	ui.PaneTitle{
		Title:   "FILES",
		Counter: ui.FormatCursor(a.fileList.Selected, len(a.files)),
		Focused: a.focus == authorFiles,
	}.Draw(screen, bounds, a.theme)
	a.fileList.Count = len(a.files)
	a.fileList.Focused = a.focus == authorFiles
	a.fileList.Draw(screen, paneChromeBounds(bounds), a.theme)

	if a.prompt == promptNewFile {
		a.drawPrompt(screen, bounds, "New file name")
	} else {
		ui.DrawBottom(screen, bounds, a.theme.SubtleStyle(), 1, a.hintsForFiles())
	}
}

func (a *authoring) drawEditor(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, a.theme.BaseStyle())
	title := "EDITOR"
	counter := ""
	if a.editor != nil {
		counter = a.openFile
		if a.editor.IsDirty() {
			counter += " *"
		}
	}
	ui.PaneTitle{
		Title:   title,
		Counter: counter,
		Focused: a.focus == authorEditor,
	}.Draw(screen, bounds, a.theme)

	chrome := paneChromeBounds(bounds)
	if a.editor == nil {
		screen.DrawText(bounds.X+1, chrome.Y, chrome.Width-2, "Open a file to edit it (Enter on a file).", a.theme.SubtleStyle())
	} else {
		a.editor.Draw(screen, chrome)
		a.completion.Draw(screen, chrome)
		a.hover.Draw(screen, chrome)
	}
	ui.DrawBottom(screen, bounds, a.theme.SubtleStyle(), 1, a.hintsForEditor())
}

func (a *authoring) drawPrompt(screen *ui.Screen, bounds ui.Rect, label string) {
	y := bounds.Y + bounds.Height - 1
	accent := a.theme.AccentStyle()
	prefix := ":" + label + " "
	screen.DrawText(bounds.X+1, y, bounds.Width-2, prefix, accent)
	fieldX := bounds.X + 1 + len(prefix)
	fieldW := bounds.Width - 2 - len(prefix)
	if fieldW > 2 {
		caret := tcell.StyleDefault.Background(a.theme.FG)
		field := tcell.StyleDefault.Foreground(a.theme.FG)
		ui.DrawInputValue(screen, fieldX, y, fieldW, a.promptInput, true, field, caret)
	}
	if a.promptErr != "" {
		errStyle := tcell.StyleDefault.Foreground(a.theme.Error)
		screen.DrawText(bounds.X+1, y-1, bounds.Width-2, a.promptErr, errStyle)
	}
}

func (a *authoring) renderBundleRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(a.bundles) {
		return
	}
	style := theme.SubtleStyle()
	if sel {
		style = theme.AccentStyle()
	}
	marker := "  "
	if a.bundles[idx] == a.openBundle {
		marker = "* "
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, marker+a.bundles[idx], style)
}

func (a *authoring) renderFileRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(a.files) {
		return
	}
	style := theme.SubtleStyle()
	if sel {
		style = theme.AccentStyle()
	}
	marker := "  "
	if a.files[idx] == a.openFile {
		marker = "* "
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, marker+a.files[idx], style)
}

// --- hints -----------------------------------------------------------

func (a *authoring) hintsForBundles() string {
	// Ctrl+B (back to the read-only browser) is the tab-level toggle --
	// documented in cli/CLAUDE.md, not crammed into this narrow pane.
	chips := []ui.HintChip{{Key: "N", Label: "New"}}
	if len(a.bundles) > 0 {
		chips = append(chips, ui.HintChip{Key: "↑/↓", Label: "Move"}, ui.HintChip{Key: "Enter", Label: "Open"})
	}
	chips = append(chips, ui.HintChip{Key: "Tab", Label: "Cycle"})
	return ui.HintBar{Chips: chips}.String()
}

func (a *authoring) hintsForFiles() string {
	chips := []ui.HintChip{}
	if a.openBundle != "" {
		chips = append(chips, ui.HintChip{Key: "N", Label: "New"})
	}
	if len(a.files) > 0 {
		chips = append(chips, ui.HintChip{Key: "↑/↓", Label: "Move"}, ui.HintChip{Key: "Enter", Label: "Edit"})
	}
	chips = append(chips, ui.HintChip{Key: "Tab", Label: "Cycle"})
	return ui.HintBar{Chips: chips}.String()
}

func (a *authoring) hintsForEditor() string {
	if a.editor == nil {
		return "Tab:Cycle"
	}
	return ui.HintBar{Chips: []ui.HintChip{
		{Key: "Ctrl+S", Label: "Save"},
		{Key: "Ctrl+Space", Label: "Complete"},
		{Key: "Ctrl+K", Label: "Hover"},
		{Key: "Tab", Label: "Cycle"},
	}}.String()
}

// --- events ----------------------------------------------------------

// HandleEvent routes a key for authoring mode. Returns true if
// consumed. The mode toggle (Ctrl+B) is handled by the parent View.
func (a *authoring) HandleEvent(key *tcell.EventKey) bool {
	a.mu.Lock()

	// Prompt input takes priority.
	if a.prompt != promptNone {
		consumed := a.handlePromptKeyLocked(key)
		a.mu.Unlock()
		return consumed
	}

	if key.Key() == tcell.KeyTab {
		a.focus = (a.focus + 1) % authorFocusCount
		a.mu.Unlock()
		return true
	}

	switch a.focus {
	case authorBundles:
		consumed := a.handleBundlesKeyLocked(key)
		a.mu.Unlock()
		return consumed
	case authorFiles:
		consumed := a.handleFilesKeyLocked(key)
		a.mu.Unlock()
		return consumed
	case authorEditor:
		// editor key handling may spawn gRPC + needs to release the
		// lock for the debounced refresh; handle inline.
		return a.handleEditorKey(key)
	}
	a.mu.Unlock()
	return false
}

func (a *authoring) handleBundlesKeyLocked(key *tcell.EventKey) bool {
	if key.Key() == tcell.KeyRune && (key.Rune() == 'n' || key.Rune() == 'N') {
		a.prompt = promptNewBundle
		a.promptInput = ""
		a.promptErr = ""
		return true
	}
	if key.Key() == tcell.KeyEnter {
		if a.bundleList.Selected >= 0 && a.bundleList.Selected < len(a.bundles) {
			bundle := a.bundles[a.bundleList.Selected]
			a.loadBundleFilesLocked(bundle)
			a.focus = authorFiles
		}
		return true
	}
	return a.bundleList.HandleEvent(key)
}

func (a *authoring) handleFilesKeyLocked(key *tcell.EventKey) bool {
	if key.Key() == tcell.KeyRune && (key.Rune() == 'n' || key.Rune() == 'N') {
		if a.openBundle == "" {
			a.status("select a bundle first")
			return true
		}
		a.prompt = promptNewFile
		a.promptInput = ""
		a.promptErr = ""
		return true
	}
	if key.Key() == tcell.KeyEnter {
		if a.fileList.Selected >= 0 && a.fileList.Selected < len(a.files) {
			a.openFileLocked(a.files[a.fileList.Selected])
			a.focus = authorEditor
		}
		return true
	}
	return a.fileList.HandleEvent(key)
}

// handleEditorKey handles the EDITOR pane. Called WITHOUT the lock held
// at exit boundaries that spawn goroutines; it acquires/releases a.mu
// around state mutation itself.
func (a *authoring) handleEditorKey(key *tcell.EventKey) bool {
	// (lock currently held by HandleEvent)
	defer a.mu.Unlock()

	if a.editor == nil {
		return false
	}

	// Completion popup intercepts navigation/accept while visible.
	if a.completion.Visible {
		if consumed, item := a.completion.HandleEvent(key); consumed {
			if item != nil {
				a.acceptCompletionLocked(item)
			}
			return true
		}
	}

	switch {
	case key.Key() == tcell.KeyCtrlS:
		a.saveLocked()
		return true
	case key.Key() == tcell.KeyCtrlSpace:
		line, col, src := a.editor.CursorLine, a.editor.CursorCol, a.editor.Buffer.Source()
		go a.requestCompletion(src, line, col)
		return true
	case key.Key() == tcell.KeyCtrlK:
		line, col, src := a.editor.CursorLine, a.editor.CursorCol, a.editor.Buffer.Source()
		go a.requestHover(src, line, col)
		return true
	case key.Key() == tcell.KeyEsc:
		if a.hover.Visible {
			a.hover.Hide()
			return true
		}
		return false
	}

	// Delegate editing to the editor; on a content change, re-run Sense.
	before := a.editor.Buffer.Source()
	consumed := a.editor.HandleEvent(key)
	if consumed && a.editor.Buffer.Source() != before {
		a.hover.Hide()
		a.debounce.Trigger()
	}
	return consumed
}

func (a *authoring) handlePromptKeyLocked(key *tcell.EventKey) bool {
	switch key.Key() {
	case tcell.KeyEsc:
		a.prompt = promptNone
		a.promptInput = ""
		a.promptErr = ""
		return true
	case tcell.KeyEnter:
		a.commitPromptLocked()
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if n := len(a.promptInput); n > 0 {
			a.promptInput = a.promptInput[:n-1]
		}
		return true
	case tcell.KeyRune:
		a.promptInput += string(key.Rune())
		return true
	}
	return true
}

func (a *authoring) commitPromptLocked() {
	kind := a.prompt
	name := a.promptInput
	switch kind {
	case promptNewBundle:
		if err := createBundle(name); err != nil {
			a.promptErr = err.Error()
			return
		}
		a.prompt = promptNone
		a.promptInput = ""
		// refreshBundles takes the lock -- do it after releasing via a
		// deferred goroutine-free inline reload.
		bundles, err := listBundles()
		if err == nil {
			a.bundles = bundles
		}
		a.selectBundleLocked(name)
	case promptNewFile:
		created, err := createBundleFile(a.openBundle, name)
		if err != nil {
			a.promptErr = err.Error()
			return
		}
		a.prompt = promptNone
		a.promptInput = ""
		a.loadBundleFilesLocked(a.openBundle)
		a.selectFileLocked(created)
		a.openFileLocked(created)
		a.focus = authorEditor
	}
}

func (a *authoring) selectBundleLocked(name string) {
	for i, b := range a.bundles {
		if b == name {
			a.bundleList.Selected = i
			return
		}
	}
}

func (a *authoring) selectFileLocked(name string) {
	for i, f := range a.files {
		if f == name {
			a.fileList.Selected = i
			return
		}
	}
}

func (a *authoring) loadBundleFilesLocked(bundle string) {
	files, err := listBundleFiles(bundle)
	if err != nil {
		a.status(fmt.Sprintf("list files: %v", err))
		return
	}
	a.openBundle = bundle
	a.files = files
	a.fileList.Selected = 0
	a.fileList.ScrollY = 0
}

func (a *authoring) openFileLocked(name string) {
	src, err := readBundleFile(a.openBundle, name)
	if err != nil {
		a.status(fmt.Sprintf("read %s: %v", name, err))
		return
	}
	buf := editor.NewBuffer(src, name, false)
	a.editor = editor.NewEditor(buf, a.theme)
	a.openFile = name
	a.completion.Hide()
	a.hover.Hide()
	// Kick an initial Sense pass so coloring + diagnostics appear
	// without the user having to type first.
	a.debounce.Trigger()
}

func (a *authoring) saveLocked() {
	if a.editor == nil || a.openBundle == "" || a.openFile == "" {
		return
	}
	if err := writeBundleFile(a.openBundle, a.openFile, a.editor.Buffer.Source()); err != nil {
		a.status(fmt.Sprintf("save %s: %v", a.openFile, err))
		return
	}
	a.editor.ClearDirty()
	a.status(fmt.Sprintf("saved %s/%s", a.openBundle, a.openFile))
}

func (a *authoring) acceptCompletionLocked(item *sense.CompletionItem) {
	if a.editor == nil {
		return
	}
	text := item.InsertText
	if text == "" {
		text = item.Label
	}
	for _, r := range text {
		a.editor.Buffer.InsertChar(a.editor.CursorLine, a.editor.CursorCol, r)
		a.editor.CursorCol++
	}
	a.completion.Hide()
	a.debounce.Trigger()
}

// --- Sense (background) ---------------------------------------------

// senseRefresh re-tokenizes + re-diagnoses the current buffer and feeds
// the result into the editor. Fired by the debouncer off the UI
// goroutine.
func (a *authoring) senseRefresh() {
	sc := a.senseClientFn()
	a.mu.Lock()
	if a.editor == nil || sc == nil {
		a.mu.Unlock()
		return
	}
	src := a.editor.Buffer.Source()
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tokens, terr := sc.Tokenize(ctx, src)
	diags, derr := sc.Diagnose(ctx, src)

	a.mu.Lock()
	if a.editor != nil {
		if terr == nil {
			a.editor.SetTokens(tokens)
		}
		if derr == nil {
			a.editor.SetDiagnostics(diags)
		}
	}
	a.mu.Unlock()
	if terr != nil {
		a.status(fmt.Sprintf("tokenize: %v", terr))
	}
	if derr != nil {
		a.status(fmt.Sprintf("diagnose: %v", derr))
	}
	a.redraw()
}

func (a *authoring) requestCompletion(src string, line, col int) {
	sc := a.senseClientFn()
	if sc == nil {
		a.status("completion unavailable -- no Sense client")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	items, err := sc.Complete(ctx, src, sense.Position{Line: line + 1, Column: col + 1})
	if err != nil {
		a.status(fmt.Sprintf("complete: %v", err))
		return
	}
	a.mu.Lock()
	if len(items) > 0 {
		a.completion.Show(items, 0, 0)
	} else {
		a.status("no completions here")
	}
	a.mu.Unlock()
	a.redraw()
}

func (a *authoring) requestHover(src string, line, col int) {
	sc := a.senseClientFn()
	if sc == nil {
		a.status("hover unavailable -- no Sense client")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := sc.Hover(ctx, src, sense.Position{Line: line + 1, Column: col + 1})
	if err != nil {
		a.status(fmt.Sprintf("hover: %v", err))
		return
	}
	a.mu.Lock()
	if h != nil && h.Contents != "" {
		a.hover.Show(h.Contents, 0, 1)
	} else {
		a.status("no hover info here")
	}
	a.mu.Unlock()
	a.redraw()
}

func (a *authoring) senseClientFn() *sense.Client {
	if a.senseClient == nil {
		return nil
	}
	return a.senseClient()
}
