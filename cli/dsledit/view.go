// Package dsledit is the cockpit's "Editor" tab: a read-only browser
// over a connected node's DSL packs (the embedded + plugin-registered
// .memql / .tmpl tree), backed by the memql pack-browser SDK
// (memql/sdk/go/pack). It mirrors the Concepts tab in shape -- a
// multi-pane View embedding ui.BaseView, wired to the active cluster's
// dispatcher by the app layer.
//
// B2 (memql-cockpit#228) is the scaffold + read-only browser: a
// domain tree (left), the files in the selected domain (middle), and
// the selected file's raw source (right). B3 (#229) swaps the raw
// source pane for a MemQL Sense-colored render; C2/C3 add the bundle
// authoring surface. Per the cockpit SDK-only rule, every wire call
// goes through the SDK pack.Client -- no raw DSL, no memqlv1 imports.
package dsledit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/sdk/go/pack"

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

// View is the Editor tab. It holds the browsed pack tree and the
// currently-loaded file source, plus the three list/viewer widgets.
type View struct {
	ui.BaseView

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

	// PackClient returns a pack client bound to the active cluster's
	// dispatcher, or nil when no cluster is connected. Set by the app
	// layer; the gated placeholder shows when it (or its result) is
	// nil. Mirrors the Concepts tab's QueryClient closure.
	PackClient func() *pack.Client
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
	v.viewer.Draw(screen, paneChromeBounds(bounds), v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, v.hintsForSource())
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
		return v.viewer.HandleEvent(ev)
	}
	return false
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
		v.viewer.ScrollY = 0
		v.Focus = FocusFiles
		v.Mu.Unlock()
		v.Redraw()
	}()
}

// loadSource fetches a single file's source and moves focus to the
// Source pane. Renders the raw source as plain viewer lines -- Sense
// coloring is B3 (#229).
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
		v.Mu.Lock()
		if src == nil || !src.Found {
			v.viewer.Lines = []ui.ViewerLine{{Text: fmt.Sprintf("(file not found: %s/%s)", domain, path)}}
		} else {
			v.viewer.Lines = plainLines(src.Source)
		}
		v.viewer.ScrollY = 0
		v.loadedFile = path
		v.Focus = FocusSource
		v.Mu.Unlock()
		v.Redraw()
	}()
}

func (v *View) packClient() *pack.Client {
	if v.PackClient == nil {
		return nil
	}
	return v.PackClient()
}

func (v *View) status(msg string) {
	if v.OnStatus != nil {
		v.OnStatus(msg)
	}
}

// plainLines splits source into viewer lines with no highlight spans
// (B3 adds Sense coloring). A trailing newline is dropped so the
// viewer doesn't show a spurious empty last line.
func plainLines(source string) []ui.ViewerLine {
	source = strings.TrimSuffix(source, "\n")
	if source == "" {
		return nil
	}
	raw := strings.Split(source, "\n")
	out := make([]ui.ViewerLine, len(raw))
	for i, ln := range raw {
		out[i] = ui.ViewerLine{Text: ln}
	}
	return out
}

func (v *View) hintsForDomains() string {
	if len(v.domains) == 0 {
		return "Tab:Cycle"
	}
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
	return ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Scroll"},
		{Key: "Tab", Label: "Cycle"},
	}}.String()
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
