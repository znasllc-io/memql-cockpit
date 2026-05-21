// Package concepts renders the Concepts tab: the unified browser
// that replaced the Explorer + Agents tabs. Three panes -- concept
// picker (left), row list with search (middle), generic detail
// renderer (right). Backed by ListConcepts + ExecuteQuery; no
// per-concept renderer (Hybrid C: walk the payload + metadata
// generically so new concepts work the day they're declared).
//
// This file is the canonical reference for the TUI composable-widget
// refactor (epic #81). Every other view migration follows the same
// shape: embed ui.BaseView, swap hand-rolled list/detail loops for
// ui.ListPane / ui.DetailPane, swap inline hint composition for
// ui.HintBar.
package concepts

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql/sdk/go/sense"
)

// FocusPane identifies which pane has keyboard focus.
type FocusPane int

const (
	FocusConcepts FocusPane = 0
	FocusRows     FocusPane = 1
	FocusDetail   FocusPane = 2
)

// View renders the Concepts tab.
//
// Thread-safety. The view is mutated from two distinct goroutine
// classes: the tcell event-loop goroutine (key handlers,
// HandleEvent) and background fetcher goroutines (SetConcepts +
// refreshRowsFromCurrent from a `go ...` spawn in app.go). Draw
// runs from the event-loop goroutine and takes a read lock for the
// whole frame; mutators take a write lock. The mutex lives in the
// embedded ui.BaseView -- see cli/ui/baseview.go for the rationale.
type View struct {
	ui.BaseView // Mu / Theme / GatedMessage / OnStatus / OnRedraw

	// Concept registry (left pane). Source slice owned by the view;
	// conceptList consumes it via index callbacks.
	Concepts []client.Concept

	// Loaded rows for the selected concept (middle pane). rowMatches
	// is the filtered index slice driven by rowFilter; rowList renders
	// against matches, not Rows directly.
	Rows       []map[string]any
	rowFilter  string
	rowMatches []int

	// Detail (right pane) -- pseudo-MemQL render of the highlighted
	// row, syntax-highlighted via Sense and laid out by the reusable
	// ui.Viewer widget (line numbers, hard-wrap, block tinting).
	detailLines []ui.ViewerLine

	// detailCacheRowId is the id of the row whose tokens currently
	// drive detailLines. We re-tokenize only when the highlighted row
	// changes, not on every redraw -- Sense calls are gRPC round-trips
	// and the cockpit redraws aggressively on focus/scroll changes.
	detailCacheRowId string

	// Version overlay -- when versionsOpen is true the detail pane
	// shows the time-series of versions for the selected row instead
	// of the current snapshot.
	versionsOpen bool
	versionRows  []map[string]any

	Focus    FocusPane
	searchOn bool

	// Plumbing: how to talk to the active cluster's QueryClient.
	// Set by the app layer; reset to nil when no cluster is connected
	// (which also flips GatedMessage on for the placeholder).
	QueryClient func() *client.QueryClient

	// SenseClient returns a sense client bound to the active cluster's
	// dispatcher, or nil when no cluster is connected. The viewer
	// degrades gracefully when this is nil -- no spans, but the
	// content still wraps + numbers + tints as expected.
	SenseClient func() *sense.Client

	// Widgets (composable from cli/ui/). These hold Selected / ScrollY
	// state internally so the view no longer carries
	// conceptSelected / rowSelected / detailScroll fields. Render
	// callbacks close over v.Concepts / v.Rows / v.rowMatches /
	// v.detailLines.
	conceptList ui.ListPane
	rowList     ui.ListPane
	versionList ui.ListPane
	viewer      ui.Viewer
}

// NewView creates an empty Concepts view. Renderer callbacks for
// each widget close over the view's data slices, so the widgets
// stay configuration-free at the use site (only Count and Focused
// are toggled per render).
//
// All three list panes use RowsPerItem=2 to match the planner's
// plan-list visual: primary line carries the most identifying
// string, dimmed subtitle line carries metadata (description / id +
// createdAt / etc.).
func NewView(theme ui.Theme) *View {
	v := &View{Focus: FocusConcepts}
	v.Theme = theme
	v.conceptList.RowsPerItem = 2
	v.conceptList.Render = v.renderConceptRow
	v.rowList.RowsPerItem = 2
	v.rowList.Render = v.renderRowItem
	v.versionList.RowsPerItem = 2
	v.versionList.Render = v.renderVersionRow
	v.conceptList.EmptyMessage = "No concepts loaded."
	v.rowList.EmptyMessage = "No rows."
	v.versionList.EmptyMessage = "No version history for this row."
	v.viewer.EmptyMessage = "Select a row to see its rendered detail."
	return v
}

// SetConcepts replaces the concept registry. Sorting is `domain:entity`
// alphabetical so cognition/agents/etc. stay grouped. Safe to call
// from a background goroutine -- the write lock covers the slice
// swap AND the chained refreshRowsFromCurrentLocked call so Draw
// can't observe a torn intermediate.
func (v *View) SetConcepts(concepts []client.Concept) {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.Concepts = make([]client.Concept, 0, len(concepts))
	for _, c := range concepts {
		if c.Id == "" {
			continue
		}
		v.Concepts = append(v.Concepts, c)
	}
	sort.Slice(v.Concepts, func(i, j int) bool {
		return v.Concepts[i].Id < v.Concepts[j].Id
	})
	v.conceptList.Count = len(v.Concepts)
	if v.conceptList.Selected >= len(v.Concepts) {
		v.conceptList.Selected = 0
	}
	v.refreshRowsFromCurrentLocked()
}

// Draw renders the Concepts tab. Holds the read lock for the whole
// frame so concurrent mutators (SetConcepts from a background
// fetch) can't tear v.Rows / v.rowMatches mid-render.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.Mu.RLock()
	defer v.Mu.RUnlock()
	if v.GatedMessage != "" {
		v.drawGated(screen, bounds)
		return
	}

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.25, MinSize: 24},
		{Flex: 0.30, MinSize: 28},
		{Flex: 0.45, MinSize: 32},
	})
	conceptBounds := panes[0]
	rowBounds := panes[1]
	detailBounds := panes[2]

	// Single-cell box-drawing dividers (safe at pane edges per the
	// Layout-edge glyph rule in cli/CLAUDE.md).
	divStyle := v.Theme.SubtleStyle()
	for _, x := range []int{conceptBounds.X + conceptBounds.Width - 1, rowBounds.X + rowBounds.Width - 1} {
		for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
			screen.SetCell(x, y, '│', divStyle)
		}
	}
	conceptBounds.Width--
	rowBounds.Width--

	v.drawConceptList(screen, conceptBounds)
	v.drawRowList(screen, rowBounds)
	v.drawDetail(screen, detailBounds)
}

func (v *View) drawGated(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	ui.PaneTitle{Title: "CONCEPTS"}.Draw(screen, bounds, v.Theme)
	midY := bounds.Y + bounds.Height/2
	lineX := bounds.X + (bounds.Width-len(v.GatedMessage))/2
	if lineX < bounds.X+1 {
		lineX = bounds.X + 1
	}
	screen.DrawText(lineX, midY, bounds.Width-1, v.GatedMessage, v.Theme.SubtleStyle())
}

// paneChromeBounds returns the inner Rect for a pane's scrollable
// content area: skip the title row at the top and the chrome row
// at the bottom. Used by all three panes so chrome layout stays
// consistent across the tab.
func paneChromeBounds(bounds ui.Rect) ui.Rect {
	const chromeH = 1
	listTop := bounds.Y + 2
	listHeight := bounds.Height - 2 - chromeH
	if listHeight < 1 {
		listHeight = 1
	}
	return ui.Rect{X: bounds.X, Y: listTop, Width: bounds.Width, Height: listHeight}
}

func (v *View) drawConceptList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	ui.PaneTitle{
		Title:   "CONCEPTS",
		Counter: ui.FormatCursor(v.conceptList.Selected, len(v.Concepts)),
		Focused: v.Focus == FocusConcepts,
	}.Draw(screen, bounds, v.Theme)

	v.conceptList.Count = len(v.Concepts)
	v.conceptList.Focused = v.Focus == FocusConcepts
	v.conceptList.Draw(screen, paneChromeBounds(bounds), v.Theme)

	v.drawBottomHints(screen, bounds, hintsForConcepts(v))
}

func (v *View) drawRowList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	// Counter formats: empty when no concept selected, filtered/cursor
	// when there are rows. The concept id no longer appears in the
	// title -- the left pane already shows which concept is selected
	// (chrome contract: no embedded ids in titles).
	ui.PaneTitle{
		Title:   "ROWS",
		Counter: ui.FormatFiltered(v.rowList.Selected, len(v.rowMatches), len(v.Rows)),
		Focused: v.Focus == FocusRows,
	}.Draw(screen, bounds, v.Theme)

	v.rowList.Count = len(v.rowMatches)
	v.rowList.Focused = v.Focus == FocusRows && !v.searchOn
	v.rowList.Draw(screen, paneChromeBounds(bounds), v.Theme)

	v.drawBottomHints(screen, bounds, hintsForRows(v))
}

func (v *View) drawDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	title := "DETAIL"
	counter := ui.FormatLine(v.viewer.ScrollY, len(v.detailLines))
	if v.versionsOpen {
		title = "VERSIONS"
		counter = ui.FormatCursor(v.versionList.Selected, len(v.versionRows))
	}
	ui.PaneTitle{
		Title:   title,
		Counter: counter,
		Focused: v.Focus == FocusDetail,
	}.Draw(screen, bounds, v.Theme)

	inner := paneChromeBounds(bounds)
	// Reserve a two-cell left gutter so detail content doesn't crowd
	// the pane divider on the left.
	inner.X += 1
	inner.Width -= 1
	if inner.Width < 1 {
		inner.Width = 1
	}

	if v.versionsOpen {
		v.versionList.Count = len(v.versionRows)
		v.versionList.Focused = v.Focus == FocusDetail
		v.versionList.Draw(screen, inner, v.Theme)
	} else {
		v.viewer.Lines = v.detailLines
		v.viewer.Focused = v.Focus == FocusDetail
		v.viewer.Draw(screen, inner, v.Theme)
	}

	v.drawBottomHints(screen, bounds, hintsForDetail(v))
}

// renderConceptRow paints one 2-row concept entry. Primary line is
// the concept id (`v1:cognition:agent`); subtitle is the concept's
// description (falling back to type when description is empty).
// ListPane has already filled both rows with the selection
// background when `sel` is true.
func (v *View) renderConceptRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.Concepts) {
		return
	}
	c := v.Concepts[idx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	screen.DrawText(bounds.X+2, bounds.Y, bounds.Width-3, c.Id, primary)

	subStr := c.Description
	if subStr == "" {
		subStr = c.Type
	}
	if subStr != "" {
		screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-3, subStr, sub)
	}
}

// renderRowItem paints one 2-row entry in the middle pane. idx is
// into rowMatches, which dereferences back to v.Rows. Primary line
// is the display label (name/displayName/slug/title/id fallback);
// subtitle is the row id + shortened createdAt.
func (v *View) renderRowItem(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.rowMatches) {
		return
	}
	rowIdx := v.rowMatches[idx]
	if rowIdx < 0 || rowIdx >= len(v.Rows) {
		return
	}
	row := v.Rows[rowIdx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	screen.DrawText(bounds.X+2, bounds.Y, bounds.Width-3, rowDisplayLabel(row), primary)

	rowId := getString(row, "id")
	created := shortenTimestamp(getString(row, "createdAt"))
	var subStr string
	switch {
	case rowId != "" && created != "":
		subStr = rowId + "  ·  " + created
	case rowId != "":
		subStr = rowId
	case created != "":
		subStr = created
	}
	if subStr != "" {
		screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-3, subStr, sub)
	}
}

// renderVersionRow paints one 2-row version-history entry. Primary
// line is the timestamp + author; subtitle is the provenance kind.
func (v *View) renderVersionRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.versionRows) {
		return
	}
	ver := v.versionRows[idx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	when := shortenTimestamp(getString(ver, "createdAt"))
	who := getString(ver, "createdBy")
	primaryStr := fmt.Sprintf("%s  by %s", when, who)
	screen.DrawText(bounds.X, bounds.Y, bounds.Width, primaryStr, primary)

	if prov := getString(getMap(ver, "provenance"), "kind"); prov != "" {
		screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-2, prov, sub)
	}
}

// drawBottomHints renders the per-pane chrome band anchored to the
// last row of bounds. Implements the panel chrome contract documented
// in cli/CLAUDE.md "Panel chrome contract": action hints live at the
// bottom in `Key:Label` format, separated by two spaces. Search
// input rides the same band -- when active for the rows pane the
// hint is replaced with `:search <query>_` in accent style.
func (v *View) drawBottomHints(screen *ui.Screen, bounds ui.Rect, hint paneHint) {
	if hint.text == "" {
		return
	}
	style := v.Theme.SubtleStyle()
	if hint.accent {
		style = v.Theme.AccentStyle()
	}
	ui.DrawBottom(screen, bounds, style, 1, hint.text)
}

// paneHint is what drawBottomHints renders. accent=true is reserved
// for the active-search prompt so it visually stands out from the
// regular subtle action-hint strip. Kept as a struct (not just a
// HintBar) because the search-active prompt is a literal string
// rendered in accent style -- not a chip set.
type paneHint struct {
	text   string
	accent bool
}

func hintsForConcepts(v *View) paneHint {
	if len(v.Concepts) == 0 {
		return paneHint{text: "Tab:Cycle"}
	}
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Enter", Label: "Open"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return paneHint{text: bar.String()}
}

func hintsForRows(v *View) paneHint {
	if v.searchOn {
		// Active search input. Vim-style `:search` prefix so the
		// prompt visually echoes the key the user just pressed --
		// no second mental model for "search is on" vs. "press : to
		// search".
		return paneHint{text: ":search " + v.rowFilter + "_", accent: true}
	}
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Enter", Label: "Detail"},
		{Key: "", Label: "Search"},
		{Key: "V", Label: "Versions"},
		{Key: "Tab", Label: "Cycle"},
		{Key: "Esc", Label: "ClearSearch", Disabled: v.rowFilter == ""},
	}}
	return paneHint{text: bar.String()}
}

func hintsForDetail(v *View) paneHint {
	if v.versionsOpen {
		bar := ui.HintBar{Chips: []ui.HintChip{
			{Key: "↑/↓", Label: "Move"},
			{Key: "Esc", Label: "CloseVersions"},
			{Key: "Tab", Label: "Cycle"},
		}}
		return paneHint{text: bar.String()}
	}
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Scroll"},
		{Key: "PgUp/PgDn", Label: "Page"},
		{Key: "Esc", Label: "Back"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return paneHint{text: bar.String()}
}

// HandleEvent processes keyboard input for the Concepts tab. Takes
// the write lock for the duration so a concurrent Draw + a concurrent
// background SetConcepts both observe a stable, fully-applied state
// transition.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	v.Mu.Lock()
	defer v.Mu.Unlock()

	if v.searchOn {
		return v.handleSearchKeyLocked(keyEv)
	}

	if keyEv.Key() == tcell.KeyTab {
		v.Focus = (v.Focus + 1) % 3
		return true
	}

	// `:` -> jump to search per the chrome contract. `/` is
	// reserved and intentionally a no-op here.
	if keyEv.Key() == tcell.KeyRune && keyEv.Rune() == ':' {
		v.Focus = FocusRows
		v.searchOn = true
		return true
	}
	// `v` toggles version history when a row is selected.
	if keyEv.Key() == tcell.KeyRune && (keyEv.Rune() == 'v' || keyEv.Rune() == 'V') {
		if v.versionsOpen {
			v.versionsOpen = false
		} else {
			// openVersions takes its own lock; drop ours before calling
			// to avoid a self-deadlock.
			v.Mu.Unlock()
			v.openVersions()
			v.Mu.Lock()
		}
		return true
	}

	switch v.Focus {
	case FocusConcepts:
		return v.handleConceptListKeyLocked(keyEv)
	case FocusRows:
		return v.handleRowListKeyLocked(keyEv)
	case FocusDetail:
		return v.handleDetailKeyLocked(keyEv)
	}
	return false
}

func (v *View) handleConceptListKeyLocked(ev *tcell.EventKey) bool {
	// Delegate nav keys to ListPane. ListPane.Focused is set during
	// Draw, but HandleEvent gates on it -- set it here too so the
	// keys consume even if Draw hasn't happened since the focus flip.
	v.conceptList.Focused = true
	prev := v.conceptList.Selected
	if v.conceptList.HandleEvent(ev) {
		if v.conceptList.Selected != prev {
			v.refreshRowsFromCurrentLocked()
		}
		return true
	}
	if ev.Key() == tcell.KeyEnter {
		v.refreshRowsFromCurrentLocked()
		v.Focus = FocusRows
		return true
	}
	return false
}

func (v *View) handleRowListKeyLocked(ev *tcell.EventKey) bool {
	v.rowList.Focused = true
	prev := v.rowList.Selected
	if v.rowList.HandleEvent(ev) {
		if v.rowList.Selected != prev {
			v.refreshDetailFromCurrentLocked()
		}
		return true
	}
	switch ev.Key() {
	case tcell.KeyEnter:
		v.refreshDetailFromCurrentLocked()
		v.Focus = FocusDetail
		return true
	case tcell.KeyEsc:
		// Esc clears an active filter so the user can get back to the
		// full row list without re-typing through the search box.
		// The `Esc:ClearSearch` chip is gated on rowFilter != "".
		if v.rowFilter != "" {
			v.rowFilter = ""
			v.recomputeRowMatchesLocked()
			return true
		}
		return false
	}
	return false
}

func (v *View) handleDetailKeyLocked(ev *tcell.EventKey) bool {
	if v.versionsOpen {
		v.versionList.Focused = true
		if v.versionList.HandleEvent(ev) {
			return true
		}
		if ev.Key() == tcell.KeyEsc {
			v.versionsOpen = false
			return true
		}
		return false
	}
	v.viewer.Focused = true
	if v.viewer.HandleEvent(ev) {
		return true
	}
	if ev.Key() == tcell.KeyEsc {
		v.Focus = FocusRows
		return true
	}
	return false
}

func (v *View) handleSearchKeyLocked(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyEsc, tcell.KeyEnter:
		v.searchOn = false
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(v.rowFilter) > 0 {
			v.rowFilter = v.rowFilter[:len(v.rowFilter)-1]
			v.recomputeRowMatchesLocked()
		}
		return true
	case tcell.KeyRune:
		v.rowFilter += string(ev.Rune())
		v.recomputeRowMatchesLocked()
		return true
	}
	return false
}

// refreshRowsFromCurrent is the exported (lock-taking) wrapper for
// callers that aren't already holding the write lock.
func (v *View) refreshRowsFromCurrent() {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.refreshRowsFromCurrentLocked()
}

// refreshRowsFromCurrentLocked reloads rows for the currently-
// selected concept via the active QueryClient. Caller MUST hold
// v.Mu (write). Synchronous network call inside the lock is
// intentional -- see the original comment in the pre-migration
// view; the async-with-spinner variant is a separate cleanup, not a
// hot-path bug.
func (v *View) refreshRowsFromCurrentLocked() {
	v.Rows = nil
	v.detailLines = nil
	v.detailCacheRowId = ""
	v.rowList.Selected = 0
	v.rowList.ScrollY = 0
	v.viewer.ScrollY = 0
	v.versionsOpen = false
	if v.QueryClient == nil {
		v.rowMatches = nil
		v.rowList.Count = 0
		return
	}
	if v.conceptList.Selected < 0 || v.conceptList.Selected >= len(v.Concepts) {
		v.rowMatches = nil
		v.rowList.Count = 0
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		v.rowMatches = nil
		v.rowList.Count = 0
		return
	}
	conceptId := v.Concepts[v.conceptList.Selected].Id
	if conceptId == "" {
		v.rowMatches = nil
		v.rowList.Count = 0
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	// Admin-surface escape hatch: the Concepts tab is a concept-
	// agnostic row browser, so there's no compile-time named primitive
	// to call. The SDK's BrowseConcept method exists precisely for
	// this case; see sdk/go/CLAUDE.md for the rule.
	res, err := qc.BrowseConcept(ctx, conceptId)
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("rows load failed: %v", err))
		}
		v.recomputeRowMatchesLocked()
		return
	}
	// RawNodes (NOT Rows) preserves the full nested wire shape --
	// `payload`, `metadata`, `provenance`, and the `type` / `schema`
	// / `createdBy` intrinsics survive. Rows() flattens the bundle
	// and drops all of those, which is right for typed named
	// primitives but wrong for an inspector tool. The detail
	// viewer + row search filter + display label all expect the
	// nested form. See memql/sdk/go/client/support.go.
	v.Rows = res.RawNodes()
	v.recomputeRowMatchesLocked()
	v.refreshDetailFromCurrentLocked()
}

func (v *View) refreshDetailFromCurrent() {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.refreshDetailFromCurrentLocked()
}

func (v *View) refreshDetailFromCurrentLocked() {
	v.viewer.ScrollY = 0
	matches := v.rowMatches
	if v.rowList.Selected < 0 || v.rowList.Selected >= len(matches) {
		v.detailLines = nil
		v.detailCacheRowId = ""
		return
	}
	idx := matches[v.rowList.Selected]
	if idx < 0 || idx >= len(v.Rows) {
		// Defensive: matches was computed against an older v.Rows.
		v.detailLines = nil
		v.detailCacheRowId = ""
		return
	}
	row := v.Rows[idx]
	rowId := getString(row, "id")

	// Skip re-tokenization when the highlighted row hasn't changed.
	// Draw fires often (focus changes, scrolls in OTHER panes, timer
	// wakeups); each pass through here would otherwise issue a fresh
	// Sense round-trip.
	if rowId != "" && rowId == v.detailCacheRowId && len(v.detailLines) > 0 {
		return
	}

	tokens := v.tokenizeRowLocked(row)
	v.detailLines = rowToViewerLines(row, tokens, v.Theme)
	v.detailCacheRowId = rowId
}

// tokenizeRowLocked calls Sense.Tokenize on the row's serialized form
// and returns the token slice. Returns nil (no spans) when no Sense
// client is wired or the call fails -- the viewer renders the raw
// text without highlighting, which is degraded but not broken.
//
// Caller MUST hold v.Mu. The Sense call goes out under the lock,
// matching the existing refreshRowsFromCurrentLocked pattern; the
// timeout caps the worst-case stall at 2s.
func (v *View) tokenizeRowLocked(row map[string]any) []sense.Token {
	if v.SenseClient == nil {
		return nil
	}
	sc := v.SenseClient()
	if sc == nil {
		return nil
	}
	source := serializeRow(row).text
	if source == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tokens, err := sc.Tokenize(ctx, source)
	if err != nil {
		// Status callback is optional; surface a non-fatal hint so the
		// user knows highlighting is unavailable. Don't return early --
		// the viewer copes with empty tokens.
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("syntax highlighting unavailable: %v", err))
		}
		return nil
	}
	return tokens
}

// openVersions fetches the time-series history for the selected row
// (concept + id) and swaps the detail pane into the version-list view.
func (v *View) openVersions() {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	if v.QueryClient == nil {
		return
	}
	matches := v.rowMatches
	if v.rowList.Selected < 0 || v.rowList.Selected >= len(matches) {
		return
	}
	idx := matches[v.rowList.Selected]
	if idx < 0 || idx >= len(v.Rows) {
		return
	}
	row := v.Rows[idx]
	rowId := getString(row, "id")
	conceptId := getString(row, "concept")
	if conceptId == "" && v.conceptList.Selected >= 0 && v.conceptList.Selected < len(v.Concepts) {
		conceptId = v.Concepts[v.conceptList.Selected].Id
	}
	if rowId == "" || conceptId == "" {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	// Admin-surface escape hatch (see refreshRows for context).
	res, err := qc.GetRowByConceptAndId(ctx, conceptId, rowId)
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("version history failed: %v", err))
		}
		return
	}
	// RawNodes preserves createdBy + provenance, which the version
	// row renderer (renderVersionRow) reads directly. Rows() would
	// flatten them away.
	v.versionRows = res.RawNodes()
	sort.Slice(v.versionRows, func(i, j int) bool {
		return getString(v.versionRows[i], "createdAt") > getString(v.versionRows[j], "createdAt")
	})
	v.versionList.Selected = 0
	v.versionList.ScrollY = 0
	v.versionList.Count = len(v.versionRows)
	v.versionsOpen = true
}

func (v *View) recomputeRowMatches() {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.recomputeRowMatchesLocked()
}

func (v *View) recomputeRowMatchesLocked() {
	filter := strings.TrimSpace(strings.ToLower(v.rowFilter))
	v.rowMatches = v.rowMatches[:0]
	for idx, row := range v.Rows {
		if filter == "" || rowMatchesFilter(row, filter) {
			v.rowMatches = append(v.rowMatches, idx)
		}
	}
	if v.rowList.Selected >= len(v.rowMatches) {
		v.rowList.Selected = 0
	}
	v.rowList.Count = len(v.rowMatches)
	v.refreshDetailFromCurrentLocked()
}

// --- pure helpers --------------------------------------------------

func rowMatchesFilter(row map[string]any, filter string) bool {
	if id := getString(row, "id"); strings.Contains(strings.ToLower(id), filter) {
		return true
	}
	payload := getMap(row, "payload")
	for _, key := range []string{"name", "displayName", "slug", "id", "title"} {
		if v := getString(payload, key); strings.Contains(strings.ToLower(v), filter) {
			return true
		}
	}
	return false
}

func rowDisplayLabel(row map[string]any) string {
	payload := getMap(row, "payload")
	for _, key := range []string{"name", "displayName", "slug", "title"} {
		if v := getString(payload, key); v != "" {
			return v
		}
	}
	return getString(row, "id")
}

func shortenTimestamp(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Format("2006-01-02 15:04:05Z")
	}
	return ts
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func getMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	out, _ := v.(map[string]any)
	return out
}

// Row → MemQL declaration rendering lives in render.go (the new
// pseudo-MemQL serializer that feeds the Viewer widget). The old
// LinePlain-based renderer here is retired -- the Viewer-driven path
// in render.go is the only one now.
