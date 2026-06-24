// Package dsledit is the cockpit's "Editor" tab: a read-only browser
// over a connected node's DSL packs (the embedded + plugin-registered
// .memql / .tmpl tree), backed by the memql pack-browser SDK
// (memql/sdk/go/pack). It mirrors the Concepts tab in shape -- a
// multi-pane View embedding ui.BaseView, wired to the active cluster's
// dispatcher by the app layer.
//
// B2 (memql-cockpit#228) was the scaffold + read-only browser: a
// domain tree (left), the files in the selected domain (middle), and
// the selected file's raw source (right). B3 (#229) colors the SOURCE
// pane with MemQL Sense tokens and adds a line cursor + Sense hover
// (read-only -- editing embedded/pack files is the bundle-authoring
// path, C2/C3). Per the cockpit SDK-only rule, every wire call goes
// through the SDK pack.Client / sense.Client -- no raw DSL, no memqlv1
// import.
package dsledit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	authoringsdk "github.com/znasllc-io/memql/sdk/go/authoring"
	"github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql/sdk/go/pack"
	"github.com/znasllc-io/memql/sdk/go/sense"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// FocusPane identifies which of the three panes currently owns the
// keyboard. Tab cycles Domains -> Files -> Source -> Domains.
type FocusPane int

const (
	FocusDomains FocusPane = iota
	FocusFiles
	FocusSource
)

const focusPaneCount = 3

// editMode selects the Editor tab's surface: the read-only pack
// browser (B2/B3) or the bundle authoring mode (C2 #230). Ctrl+B
// toggles between them.
type editMode int

const (
	modeBrowse editMode = iota
	modeAuthor
)

// View is the Editor tab. It holds the browsed pack tree and the
// currently-loaded file source, plus the three list/viewer widgets.
type View struct {
	ui.BaseView

	// mode toggles the read-only pack browser vs. the authoring mode.
	// author is lazily built on the first switch into authoring.
	mode   editMode
	author *authoring

	// domains is the node's full domain set (left pane). files is the
	// file list for loadedDomain (middle pane). loadedDomain /
	// loadedFile track which selection the middle/right panes reflect,
	// so a redraw doesn't re-fetch.
	domains      []pack.Domain
	files        []pack.File
	loadedDomain string
	loadedFile   string

	Focus FocusPane

	domainList ui.ListPane
	fileList   ui.ListPane
	viewer     ui.Viewer

	// srcText is the raw source backing the SOURCE viewer (kept so
	// Sense hover can be requested against the exact text the user
	// sees). cursorLine / cursorCol are the 0-based hover cursor
	// position within srcText, navigated in the SOURCE pane.
	srcText    string
	cursorLine int
	cursorCol  int

	// hover holds the last Sense hover result for the cursor position;
	// hoverShown gates the overlay.
	hoverShown bool
	hoverText  string

	// PackClient returns a pack client bound to the active cluster's
	// dispatcher, or nil when no cluster is connected. Set by the app
	// layer; the gated placeholder shows when it (or its result) is
	// nil. Mirrors the Concepts tab's QueryClient closure.
	PackClient func() *pack.Client

	// SenseClient returns a Sense client bound to the active cluster's
	// dispatcher, or nil when none is connected. Used to color the
	// SOURCE pane (Tokenize) and answer hover (Hover). The viewer
	// degrades to plain text when this is nil. Mirrors the Concepts
	// tab's SenseClient closure.
	SenseClient func() *sense.Client

	// AuthoringClient returns an authoring client bound to the active
	// cluster's dispatcher, or nil when none is connected. Backs the
	// authoring mode's Validate (Gate-1) + Inject (session-define)
	// actions (C3 #231).
	AuthoringClient func() *authoringsdk.Client

	// clusterRole mirrors the connected caller's cluster-wide role,
	// pushed from the app layer's refreshMyAccess via SetClusterRole
	// (same access record the Settings + Deployments views consume).
	// Read under v.Mu; the authoring sub-view gates its owner-only
	// Promote action (#232) on RoleOwner via the closure passed at
	// construction.
	clusterRole client.Role
}

// SetClusterRole mirrors the connected caller's cluster-wide role into
// the Editor view so the authoring mode's owner-only Promote action
// (#232) can gate on it. Wired from the app's refreshMyAccess alongside
// the Settings + Deployments role updates.
func (v *View) SetClusterRole(r client.Role) {
	v.Mu.Lock()
	v.clusterRole = r
	v.Mu.Unlock()
}

// clusterRoleFn reads the mirrored role under the lock; passed as a
// closure to newAuthoring so the authoring sub-view always sees the
// latest role even though it's built lazily on the first Ctrl+B.
func (v *View) clusterRoleFn() client.Role {
	v.Mu.RLock()
	defer v.Mu.RUnlock()
	return v.clusterRole
}

// NewView builds an empty Editor view.
func NewView(theme ui.Theme) *View {
	v := &View{Focus: FocusDomains}
	v.Theme = theme
	v.domainList.Render = v.renderDomainRow
	v.fileList.Render = v.renderFileRow
	v.domainList.EmptyMessage = "No DSL domains. Connect a cluster."
	v.fileList.EmptyMessage = "No files. Select a domain and press Enter."
	v.viewer.EmptyMessage = "Select a file and press Enter to view its source."
	return v
}

// SetDomains replaces the domain list (called by the app's refresh
// after a successful ListDomains). Resets the middle/right panes so
// stale files/source from a previous cluster don't linger.
func (v *View) SetDomains(domains []pack.Domain) {
	v.Mu.Lock()
	v.domains = domains
	v.files = nil
	v.loadedDomain = ""
	v.loadedFile = ""
	v.viewer.Lines = nil
	v.domainList.Selected = 0
	v.fileList.Selected = 0
	v.Mu.Unlock()
	v.Redraw()
}

// Draw renders the Editor tab: domains | files | source. Holds the
// read lock for the whole frame so a concurrent loader can't tear the
// slices mid-render.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.Mu.RLock()
	defer v.Mu.RUnlock()

	if v.GatedMessage != "" {
		v.drawGated(screen, bounds)
		return
	}

	if v.mode == modeAuthor && v.author != nil {
		v.author.Draw(screen, bounds)
		return
	}

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.25, MinSize: 22},
		{Flex: 0.30, MinSize: 24},
		{Flex: 0.45, MinSize: 32},
	})
	domainBounds, fileBounds, sourceBounds := panes[0], panes[1], panes[2]

	// Single-cell box-drawing dividers between panes (safe at edges
	// per the Layout-edge glyph rule in cli/CLAUDE.md).
	divStyle := v.Theme.SubtleStyle()
	for _, x := range []int{domainBounds.X + domainBounds.Width - 1, fileBounds.X + fileBounds.Width - 1} {
		for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
			screen.SetCell(x, y, '│', divStyle)
		}
	}
	domainBounds.Width--
	fileBounds.Width--

	v.drawDomainList(screen, domainBounds)
	v.drawFileList(screen, fileBounds)
	v.drawSource(screen, sourceBounds)
}

func (v *View) drawGated(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	lines := ui.WrapText(v.GatedMessage, bounds.Width-4)
	y := bounds.Y + bounds.Height/2 - len(lines)/2
	for _, ln := range lines {
		screen.DrawText(bounds.X+2, y, bounds.Width-4, ln, v.Theme.SubtleStyle())
		y++
	}
}

func (v *View) drawDomainList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	ui.PaneTitle{
		Title:   "DOMAINS",
		Counter: ui.FormatCursor(v.domainList.Selected, len(v.domains)),
		Focused: v.Focus == FocusDomains,
	}.Draw(screen, bounds, v.Theme)

	v.domainList.Count = len(v.domains)
	v.domainList.Focused = v.Focus == FocusDomains
	v.domainList.Draw(screen, paneChromeBounds(bounds), v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, v.hintsForDomains())
}

func (v *View) drawFileList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	ui.PaneTitle{
		Title:   "FILES",
		Counter: ui.FormatCursor(v.fileList.Selected, len(v.files)),
		Focused: v.Focus == FocusFiles,
	}.Draw(screen, bounds, v.Theme)

	v.fileList.Count = len(v.files)
	v.fileList.Focused = v.Focus == FocusFiles
	v.fileList.Draw(screen, paneChromeBounds(bounds), v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, v.hintsForFiles())
}

func (v *View) drawSource(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	counter := ""
	if len(v.viewer.Lines) > 0 {
		counter = ui.FormatLine(v.viewer.ScrollY, len(v.viewer.Lines))
	}
	ui.PaneTitle{
		Title:   "SOURCE",
		Counter: counter,
		Focused: v.Focus == FocusSource,
	}.Draw(screen, bounds, v.Theme)

	v.viewer.Focused = v.Focus == FocusSource
	// Show the hover cursor only when the SOURCE pane is focused and has
	// content -- otherwise the viewer stays in plain scroll mode.
	v.viewer.ShowCursor = v.Focus == FocusSource && len(v.viewer.Lines) > 0
	v.viewer.CursorLine = v.cursorLine
	chrome := paneChromeBounds(bounds)
	v.viewer.Draw(screen, chrome, v.Theme)

	if v.hoverShown && v.hoverText != "" {
		v.drawHoverOverlay(screen, chrome)
	}

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, v.hintsForSource())
}

// drawHoverOverlay paints the Sense hover result as a small bordered
// box anchored to the top of the SOURCE content area. Markdown is
// stripped to plain text (the cockpit terminal can't render it) and
// the box is clamped to the pane so it never overflows.
func (v *View) drawHoverOverlay(screen *ui.Screen, bounds ui.Rect) {
	lines := hoverLines(v.hoverText, bounds.Width-4)
	if len(lines) == 0 {
		return
	}
	maxLines := bounds.Height - 2
	if maxLines < 1 {
		return
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	w := 0
	for _, ln := range lines {
		if len([]rune(ln)) > w {
			w = len([]rune(ln))
		}
	}
	boxW := w + 4
	if boxW > bounds.Width {
		boxW = bounds.Width
	}
	boxH := len(lines) + 2
	x := bounds.X
	y := bounds.Y
	bg := tcell.StyleDefault.Background(tcell.NewRGBColor(50, 54, 62)).Foreground(v.Theme.FG)
	screen.FillRect(x, y, boxW, boxH, bg)
	screen.DrawBox(x, y, boxW, boxH, bg.Foreground(v.Theme.Subtle))
	for i, ln := range lines {
		screen.DrawText(x+2, y+1+i, boxW-4, ln, bg)
	}
}

func (v *View) renderDomainRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.domains) {
		return
	}
	d := v.domains[idx]
	style := theme.SubtleStyle()
	if sel {
		style = theme.AccentStyle()
	}
	// "name            origin (N)" -- origin label flush right per the
	// list-pane conventions, name flush left.
	name := d.Name
	meta := fmt.Sprintf("%s (%d)", d.Origin, d.FileCount)
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, name, style)
	if w := bounds.Width - 2 - len(meta); w > len(name)+1 {
		screen.DrawText(bounds.X+1+w, bounds.Y, len(meta), meta, theme.SubtleStyle())
	}
}

func (v *View) renderFileRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.files) {
		return
	}
	style := theme.SubtleStyle()
	if sel {
		style = theme.AccentStyle()
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, v.files[idx].Path, style)
}

// HandleEvent routes keys: Tab cycles focus; Enter loads the selected
// domain's files / the selected file's source; arrow keys navigate the
// focused pane.
func (v *View) HandleEvent(ev tcell.Event) bool {
	key, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// Ctrl+B toggles between the read-only browser and the authoring
	// mode. Handled before any pane dispatch so it works from any focus
	// (incl. the editor, where printable keys are typed).
	if key.Key() == tcell.KeyCtrlB {
		v.toggleMode()
		return true
	}

	if v.modeIsAuthor() {
		if v.author != nil {
			return v.author.HandleEvent(key)
		}
		return false
	}

	v.Mu.Lock()
	if key.Key() == tcell.KeyTab {
		v.Focus = (v.Focus + 1) % focusPaneCount
		v.Mu.Unlock()
		return true
	}
	if key.Key() == tcell.KeyEnter {
		switch v.Focus {
		case FocusDomains:
			if v.domainList.Selected >= 0 && v.domainList.Selected < len(v.domains) {
				domain := v.domains[v.domainList.Selected].Name
				v.Mu.Unlock()
				v.loadFiles(domain)
				return true
			}
		case FocusFiles:
			if v.fileList.Selected >= 0 && v.fileList.Selected < len(v.files) {
				file := v.files[v.fileList.Selected].Path
				domain := v.loadedDomain
				v.Mu.Unlock()
				v.loadSource(domain, file)
				return true
			}
		}
		v.Mu.Unlock()
		return true
	}
	v.Mu.Unlock()

	switch v.Focus {
	case FocusDomains:
		return v.domainList.HandleEvent(ev)
	case FocusFiles:
		return v.fileList.HandleEvent(ev)
	case FocusSource:
		return v.handleSourceKey(key)
	}
	return false
}

// handleSourceKey drives the SOURCE pane's read-only hover cursor:
// Up/Down move the cursor line (the viewer auto-scrolls to keep it
// visible), Left/Right move the column, H requests Sense hover at the
// cursor, Esc dismisses the hover. PgUp/PgDn/Home/End fall through to
// the viewer's scroll bindings.
func (v *View) handleSourceKey(key *tcell.EventKey) bool {
	v.Mu.Lock()
	nLines := len(v.viewer.Lines)
	if nLines == 0 {
		v.Mu.Unlock()
		return v.viewer.HandleEvent(key)
	}
	switch key.Key() {
	case tcell.KeyUp:
		if v.cursorLine > 0 {
			v.cursorLine--
			v.clampCursorColLocked()
		}
		v.hoverShown = false
		v.Mu.Unlock()
		return true
	case tcell.KeyDown:
		if v.cursorLine < nLines-1 {
			v.cursorLine++
			v.clampCursorColLocked()
		}
		v.hoverShown = false
		v.Mu.Unlock()
		return true
	case tcell.KeyLeft:
		if v.cursorCol > 0 {
			v.cursorCol--
		}
		v.hoverShown = false
		v.Mu.Unlock()
		return true
	case tcell.KeyRight:
		v.cursorCol++
		v.clampCursorColLocked()
		v.hoverShown = false
		v.Mu.Unlock()
		return true
	case tcell.KeyEsc:
		if v.hoverShown {
			v.hoverShown = false
			v.Mu.Unlock()
			return true
		}
		v.Mu.Unlock()
		return false
	case tcell.KeyRune:
		if r := key.Rune(); r == 'h' || r == 'H' {
			line, col := v.cursorLine, v.cursorCol
			v.Mu.Unlock()
			v.requestHover(line, col)
			return true
		}
	}
	// Everything else (PgUp/PgDn/Home/End/j/k) is viewport scrolling.
	v.Mu.Unlock()
	return v.viewer.HandleEvent(key)
}

// clampCursorColLocked keeps cursorCol within the current cursor
// line's rune length. Caller holds v.Mu.
func (v *View) clampCursorColLocked() {
	lines := strings.Split(v.srcText, "\n")
	if v.cursorLine < 0 || v.cursorLine >= len(lines) {
		v.cursorCol = 0
		return
	}
	n := len([]rune(lines[v.cursorLine]))
	if v.cursorCol > n {
		v.cursorCol = n
	}
	if v.cursorCol < 0 {
		v.cursorCol = 0
	}
}

// requestHover asks Sense for hover info at the (0-based) line/col and
// shows the result in the overlay. Runs off the UI goroutine.
func (v *View) requestHover(line, col int) {
	sc := v.senseClient()
	v.Mu.RLock()
	src := v.srcText
	v.Mu.RUnlock()
	if sc == nil || src == "" {
		v.status("hover unavailable -- no Sense client for the active cluster")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Sense positions are 1-indexed (line + column).
		h, err := sc.Hover(ctx, src, sense.Position{Line: line + 1, Column: col + 1})
		if err != nil {
			v.status(fmt.Sprintf("hover: %v", err))
			return
		}
		v.Mu.Lock()
		if h == nil || strings.TrimSpace(h.Contents) == "" {
			v.hoverText = "(no hover info here)"
		} else {
			v.hoverText = h.Contents
		}
		v.hoverShown = true
		v.Mu.Unlock()
		v.Redraw()
	}()
}

// loadFiles fetches the file list for domain and moves focus to the
// Files pane. Runs the gRPC call off the UI goroutine.
func (v *View) loadFiles(domain string) {
	client := v.packClient()
	if client == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		files, err := client.ListFiles(ctx, domain)
		if err != nil {
			v.status(fmt.Sprintf("list files for %q: %v", domain, err))
			return
		}
		v.Mu.Lock()
		v.files = files
		v.loadedDomain = domain
		v.fileList.Selected = 0
		v.fileList.ScrollY = 0
		v.loadedFile = ""
		v.viewer.Lines = nil
		v.srcText = ""
		v.resetSourceCursorLocked()
		v.Focus = FocusFiles
		v.Mu.Unlock()
		v.Redraw()
	}()
}

// loadSource fetches a single file's source, colors it with Sense
// tokens (when a Sense client is available), and moves focus to the
// Source pane. Coloring failures fall back to plain text -- the source
// still renders, just uncolored.
func (v *View) loadSource(domain, path string) {
	client := v.packClient()
	if client == nil || domain == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		src, err := client.ReadFile(ctx, domain, path)
		if err != nil {
			v.status(fmt.Sprintf("read %s/%s: %v", domain, path, err))
			return
		}
		if src == nil || !src.Found {
			v.Mu.Lock()
			v.viewer.Lines = []ui.ViewerLine{{Text: fmt.Sprintf("(file not found: %s/%s)", domain, path)}}
			v.srcText = ""
			v.resetSourceCursorLocked()
			v.loadedFile = path
			v.Focus = FocusSource
			v.Mu.Unlock()
			v.Redraw()
			return
		}

		// Color via Sense if available. The tokenize call is best-
		// effort: on error (or no Sense client) we render plain.
		var tokens []sense.Token
		if sc := v.senseClient(); sc != nil {
			if toks, terr := sc.Tokenize(ctx, src.Source); terr != nil {
				v.status(fmt.Sprintf("tokenize %s/%s: %v", domain, path, terr))
			} else {
				tokens = toks
			}
		}

		v.Mu.Lock()
		v.srcText = src.Source
		v.viewer.Lines = coloredLines(src.Source, tokens, v.Theme)
		v.resetSourceCursorLocked()
		v.loadedFile = path
		v.Focus = FocusSource
		v.Mu.Unlock()
		v.Redraw()
	}()
}

// resetSourceCursorLocked rewinds the SOURCE viewport + hover cursor to
// the top. Caller holds v.Mu.
func (v *View) resetSourceCursorLocked() {
	v.viewer.ScrollY = 0
	v.cursorLine = 0
	v.cursorCol = 0
	v.hoverShown = false
	v.hoverText = ""
}

func (v *View) packClient() *pack.Client {
	if v.PackClient == nil {
		return nil
	}
	return v.PackClient()
}

func (v *View) senseClient() *sense.Client {
	if v.SenseClient == nil {
		return nil
	}
	return v.SenseClient()
}

// modeIsAuthor reports whether the Editor tab is in authoring mode.
func (v *View) modeIsAuthor() bool {
	v.Mu.RLock()
	defer v.Mu.RUnlock()
	return v.mode == modeAuthor
}

// toggleMode flips between the read-only browser and authoring mode,
// lazily building the authoring sub-view on the first switch.
func (v *View) toggleMode() {
	v.Mu.Lock()
	if v.mode == modeBrowse {
		if v.author == nil {
			v.author = newAuthoring(v.Theme, v.SenseClient, v.AuthoringClient, v.clusterRoleFn, v.OnStatus, v.Redraw)
		}
		v.mode = modeAuthor
	} else {
		v.mode = modeBrowse
	}
	v.Mu.Unlock()
	v.Redraw()
}

func (v *View) status(msg string) {
	if v.OnStatus != nil {
		v.OnStatus(msg)
	}
}

// coloredLines splits source into viewer lines and overlays Sense
// token highlight spans. With no tokens (Sense unavailable or a
// tokenize failure) it degrades to plain text. A trailing newline is
// dropped so the viewer doesn't show a spurious empty last line.
func coloredLines(source string, tokens []sense.Token, theme ui.Theme) []ui.ViewerLine {
	source = strings.TrimSuffix(source, "\n")
	if source == "" {
		return nil
	}
	raw := strings.Split(source, "\n")
	spansByLine := spansFromTokens(tokens, theme)
	out := make([]ui.ViewerLine, len(raw))
	for i, ln := range raw {
		out[i] = ui.ViewerLine{
			Text:  ln,
			Spans: spansByLine[i+1], // Sense lines are 1-indexed
		}
	}
	return out
}

// plainLines is coloredLines with no tokens -- kept as a named helper
// for the not-found placeholder + tests.
func plainLines(source string) []ui.ViewerLine {
	return coloredLines(source, nil, ui.Theme{})
}

// spansFromTokens converts Sense tokens (1-indexed line/column) into
// per-line viewer HighlightSpans, keyed by 1-indexed line. Mirrors the
// Concepts detail renderer (cli/concepts/render.go) -- duplicated here
// because a tab package must not import another tab package.
func spansFromTokens(tokens []sense.Token, theme ui.Theme) map[int][]ui.HighlightSpan {
	out := map[int][]ui.HighlightSpan{}
	for _, tok := range tokens {
		if tok.Range.Start.Line < 1 {
			continue
		}
		style := theme.TokenStyle(tok.Type)
		if tok.Range.Start.Line == tok.Range.End.Line {
			line := tok.Range.Start.Line
			out[line] = append(out[line], ui.HighlightSpan{
				Start: tok.Range.Start.Column - 1,
				End:   tok.Range.End.Column - 1,
				Style: style,
			})
			continue
		}
		// Multi-line token (rare in MemQL): clamp the first line to
		// end-of-line and drop continuation lines.
		first := tok.Range.Start.Line
		out[first] = append(out[first], ui.HighlightSpan{
			Start: tok.Range.Start.Column - 1,
			End:   1 << 30, // "to end of line" -- styleRunes clamps
			Style: style,
		})
	}
	return out
}

func (v *View) hintsForDomains() string {
	if len(v.domains) == 0 {
		return "Ctrl+B:Author  Tab:Cycle"
	}
	// Ctrl+B:Author is the tab-level toggle; it's advertised in the
	// empty-state hint + cli/CLAUDE.md rather than crammed into this
	// narrow pane's populated hint (which would push Enter:Open off the
	// chrome band).
	return ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Enter", Label: "Open"},
		{Key: "Tab", Label: "Cycle"},
	}}.String()
}

func (v *View) hintsForFiles() string {
	if len(v.files) == 0 {
		return "Tab:Cycle"
	}
	return ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Enter", Label: "View"},
		{Key: "Tab", Label: "Cycle"},
	}}.String()
}

func (v *View) hintsForSource() string {
	if len(v.viewer.Lines) == 0 {
		return "Tab:Cycle"
	}
	if v.hoverShown {
		return ui.HintBar{Chips: []ui.HintChip{
			{Key: "Esc", Label: "CloseHover"},
			{Key: "↑/↓", Label: "Move"},
			{Key: "Tab", Label: "Cycle"},
		}}.String()
	}
	return ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "←/→", Label: "Col"},
		{Key: "H", Label: "Hover"},
		{Key: "Tab", Label: "Cycle"},
	}}.String()
}

// hoverLines strips basic markdown from Sense hover content and wraps
// it to width. Empty result when the content is blank.
func hoverLines(content string, width int) []string {
	if width < 1 {
		width = 1
	}
	for _, m := range []string{"**", "__", "`", "###", "##", "#"} {
		content = strings.ReplaceAll(content, m, "")
	}
	var out []string
	for _, raw := range strings.Split(content, "\n") {
		raw = strings.TrimRight(raw, " ")
		if raw == "" {
			out = append(out, "")
			continue
		}
		out = append(out, ui.WrapText(raw, width)...)
	}
	return out
}

// paneChromeBounds carves the list/viewer region out of a pane's
// bounds: title row + blank row at top, one chrome row at the bottom.
// Matches the Concepts tab's paneChromeBounds so the Editor pane
// chrome lines up with the rest of the cockpit.
func paneChromeBounds(bounds ui.Rect) ui.Rect {
	const chromeH = 1
	listTop := bounds.Y + 2
	listHeight := bounds.Height - 2 - chromeH
	if listHeight < 1 {
		listHeight = 1
	}
	return ui.Rect{X: bounds.X, Y: listTop, Width: bounds.Width, Height: listHeight}
}
