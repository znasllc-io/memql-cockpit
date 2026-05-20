// Package concepts renders the Concepts tab: the unified browser
// that replaced the Explorer + Agents tabs. Three panes -- concept
// picker (left), row list with search (middle), generic detail
// renderer (right). Backed by ListConcepts + ExecuteQuery; no
// per-concept renderer (Hybrid C: walk the payload + metadata
// generically so new concepts work the day they're declared).
package concepts

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
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
// classes:
//   - the tcell event-loop goroutine (key handlers, HandleEvent)
//   - background fetcher goroutines (refreshConcepts wires
//     QueryClient + SetConcepts from a `go ...` spawn in app.go)
//
// Draw runs from the event-loop goroutine. Without locking, a
// background fetcher that calls SetConcepts (which clears Rows +
// rebuilds rowMatches) while Draw is mid-render through rowMatches
// produces a stale-index crash: idx came from a valid matches
// slice, then Rows was emptied before the v.Rows[idx] read. The
// mu RWMutex eliminates that window -- mutators take Lock(),
// Draw takes RLock().
type View struct {
	mu    sync.RWMutex
	Theme ui.Theme

	// Concept registry (left pane).
	Concepts        []*memqlv1.ConceptInfo
	conceptSelected int
	conceptScrollY  int

	// Loaded rows for the selected concept (middle pane).
	Rows        []map[string]any
	rowFilter   string // text from the search box
	rowMatches  []int  // indices into Rows that match rowFilter (cached)
	rowSelected int
	rowScrollY  int

	// Detail (right pane).
	detailLines  []string
	detailScroll int

	// Version overlay -- when versionsOpen is true the detail pane
	// shows the time-series of versions for the selected row instead
	// of the current snapshot.
	versionsOpen    bool
	versionRows     []map[string]any
	versionSelected int
	versionScrollY  int

	Focus    FocusPane
	searchOn bool

	// Plumbing: how to talk to the active cluster's QueryClient.
	// Set by the app layer; reset to nil when no cluster is connected
	// (which also flips GatedMessage on for the placeholder).
	QueryClient func() *client.QueryClient

	// GatedMessage, when non-empty, replaces the layout with a
	// centered "not available" message. Set by the app layer when
	// the selected cluster has no live connection.
	GatedMessage string

	// onStatus is set by the app layer so the view can push
	// transient errors to the notification bar without importing
	// the app package.
	OnStatus func(msg string)
}

// NewView creates an empty Concepts view.
func NewView(theme ui.Theme) *View {
	return &View{
		Theme: theme,
		Focus: FocusConcepts,
	}
}

// SetConcepts replaces the concept registry. Sorting is `domain:entity`
// alphabetical so cognition/agents/etc. stay grouped.
//
// Safe to call from a background goroutine -- holds the write lock
// while replacing the concept list AND while refreshRowsFromCurrent
// runs (which clears + re-fills Rows). Draw is blocked the whole time
// so it never observes a torn intermediate state.
func (v *View) SetConcepts(concepts []*memqlv1.ConceptInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Concepts = make([]*memqlv1.ConceptInfo, 0, len(concepts))
	for _, c := range concepts {
		if c == nil {
			continue
		}
		v.Concepts = append(v.Concepts, c)
	}
	sort.Slice(v.Concepts, func(i, j int) bool {
		return v.Concepts[i].GetId() < v.Concepts[j].GetId()
	})
	if v.conceptSelected >= len(v.Concepts) {
		v.conceptSelected = 0
	}
	v.refreshRowsFromCurrentLocked()
}

// Draw renders the Concepts tab. Holds the read lock for the whole
// frame so concurrent mutators (SetConcepts from a background fetch)
// can't tear v.Rows / v.rowMatches mid-render.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.mu.RLock()
	defer v.mu.RUnlock()
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

	// Dividers (single-cell box-drawing chars; safe at pane edges).
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
	title := " CONCEPTS "
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))
	midY := bounds.Y + bounds.Height/2
	lineX := bounds.X + (bounds.Width-len(v.GatedMessage))/2
	if lineX < bounds.X+1 {
		lineX = bounds.X + 1
	}
	screen.DrawText(lineX, midY, bounds.Width-1, v.GatedMessage, v.Theme.SubtleStyle())
}

func (v *View) drawConceptList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusConcepts {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	title := " CONCEPTS "
	if c := len(v.Concepts); c > 0 {
		title = fmt.Sprintf(" CONCEPTS (%d/%d) ", v.conceptSelected+1, c)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if len(v.Concepts) == 0 {
		v.drawCentered(screen, bounds, "No concepts loaded.")
		v.drawBottomHints(screen, bounds, hintsForConcepts(v))
		return
	}

	// Reserve one bottom row for the action-hint chrome band.
	const chromeH = 1
	listTop := bounds.Y + 2
	listHeight := bounds.Height - 2 - chromeH
	if listHeight < 1 {
		listHeight = 1
	}

	v.clampConceptScroll(listHeight)

	for i := 0; i < listHeight && v.conceptScrollY+i < len(v.Concepts); i++ {
		idx := v.conceptScrollY + i
		c := v.Concepts[idx]
		y := listTop + i

		style := v.Theme.BaseStyle()
		if idx == v.conceptSelected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)
		label := c.GetId()
		screen.DrawText(bounds.X+2, y, bounds.Width-3, label, style)
	}

	v.drawBottomHints(screen, bounds, hintsForConcepts(v))
}

func (v *View) drawRowList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusRows {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	conceptId := ""
	if v.conceptSelected >= 0 && v.conceptSelected < len(v.Concepts) {
		conceptId = v.Concepts[v.conceptSelected].GetId()
	}
	matches := v.rowMatches
	title := " ROWS "
	if conceptId != "" {
		switch {
		case len(matches) < len(v.Rows):
			title = fmt.Sprintf(" ROWS: %s (%d/%d filtered from %d) ", conceptId, v.rowSelected+1, len(matches), len(v.Rows))
		case len(matches) > 0:
			title = fmt.Sprintf(" ROWS: %s (%d/%d) ", conceptId, v.rowSelected+1, len(matches))
		default:
			title = " ROWS: " + conceptId + " "
		}
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	// Reserve one bottom row for the chrome band (action hints or
	// active search input). The list grows up from above the chrome.
	const chromeH = 1
	if len(v.Rows) == 0 {
		v.drawCentered(screen, ui.Rect{X: bounds.X, Y: bounds.Y + 2, Width: bounds.Width, Height: bounds.Height - 2 - chromeH}, "No rows.")
		v.drawBottomHints(screen, bounds, hintsForRows(v))
		return
	}

	listTop := bounds.Y + 2
	listHeight := bounds.Height - 2 - chromeH
	if listHeight < 1 {
		listHeight = 1
	}

	v.clampRowScroll(listHeight)

	for i := 0; i < listHeight && v.rowScrollY+i < len(matches); i++ {
		idx := matches[v.rowScrollY+i]
		row := v.Rows[idx]
		y := listTop + i

		style := v.Theme.BaseStyle()
		if v.rowScrollY+i == v.rowSelected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)
		label := rowDisplayLabel(row)
		screen.DrawText(bounds.X+2, y, bounds.Width-3, label, style)
	}

	v.drawBottomHints(screen, bounds, hintsForRows(v))
}

func (v *View) drawDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusDetail {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	title := " DETAIL "
	if v.versionsOpen {
		title = fmt.Sprintf(" VERSIONS (%d/%d) ", v.versionSelected+1, len(v.versionRows))
	} else if n := len(v.detailLines); n > 0 {
		title = fmt.Sprintf(" DETAIL (line %d/%d) ", v.detailScroll+1, n)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if v.versionsOpen {
		v.drawVersionsList(screen, bounds)
		return
	}

	if len(v.detailLines) == 0 {
		v.drawCentered(screen, bounds, "Select a row to see its rendered detail.")
		v.drawBottomHints(screen, bounds, hintsForDetail(v))
		return
	}

	// Reserve one bottom row for the action-hint chrome band.
	const chromeH = 1
	contentTop := bounds.Y + 2
	contentH := bounds.Height - 2 - chromeH
	if contentH < 1 {
		contentH = 1
	}
	v.clampDetailScroll(contentH)
	for i := 0; i < contentH && v.detailScroll+i < len(v.detailLines); i++ {
		screen.DrawText(bounds.X+2, contentTop+i, bounds.Width-3, v.detailLines[v.detailScroll+i], v.Theme.BaseStyle())
	}
	v.drawBottomHints(screen, bounds, hintsForDetail(v))
}

func (v *View) drawVersionsList(screen *ui.Screen, bounds ui.Rect) {
	if len(v.versionRows) == 0 {
		v.drawCentered(screen, bounds, "No version history for this row.")
		v.drawBottomHints(screen, bounds, hintsForDetail(v))
		return
	}
	const chromeH = 1
	contentTop := bounds.Y + 2
	contentH := bounds.Height - 2 - chromeH
	if contentH < 1 {
		contentH = 1
	}
	for i := 0; i < contentH && v.versionScrollY+i < len(v.versionRows); i++ {
		idx := v.versionScrollY + i
		ver := v.versionRows[idx]
		y := contentTop + i
		style := v.Theme.BaseStyle()
		if idx == v.versionSelected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)
		when := getString(ver, "createdAt")
		who := getString(ver, "createdBy")
		prov := getString(getMap(ver, "provenance"), "kind")
		label := fmt.Sprintf("%s  by %s  (%s)", shortenTimestamp(when), who, prov)
		screen.DrawText(bounds.X+2, y, bounds.Width-3, label, style)
	}
	v.drawBottomHints(screen, bounds, hintsForDetail(v))
}

func (v *View) drawCentered(screen *ui.Screen, bounds ui.Rect, msg string) {
	midY := bounds.Y + bounds.Height/2
	lineX := bounds.X + (bounds.Width-len(msg))/2
	if lineX < bounds.X+1 {
		lineX = bounds.X + 1
	}
	screen.DrawText(lineX, midY, bounds.Width-1, msg, v.Theme.SubtleStyle())
}

// drawBottomHints renders the per-pane chrome band anchored to the
// last row of bounds. Implements the panel chrome contract documented
// in cli/CLAUDE.md "Panel chrome contract": action hints live at the
// bottom in `Key:Action` format, separated by two spaces. Search input
// rides the same band -- when active for the rows pane the hint is
// replaced with `:search <query>_` in accent style. Title-bar
// counters do the work of the old per-pane count footers.
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
// regular subtle action-hint strip.
type paneHint struct {
	text   string
	accent bool
}

func hintsForConcepts(v *View) paneHint {
	if len(v.Concepts) == 0 {
		return paneHint{text: "Tab:Cycle"}
	}
	return paneHint{text: "↑/↓:Move  Enter:Open  Tab:Cycle"}
}

func hintsForRows(v *View) paneHint {
	if v.searchOn {
		// Active search input. Vim-style `:search` prefix so the
		// prompt visually echoes the key the user just pressed --
		// no second mental model for "search is on" vs. "press : to
		// search".
		return paneHint{text: ":search " + v.rowFilter + "_", accent: true}
	}
	parts := []string{"↑/↓:Move", "Enter:Detail", ":Search", "v:Versions", "Tab:Cycle"}
	if v.rowFilter != "" {
		parts = append(parts, "Esc:ClearSearch")
	}
	return paneHint{text: strings.Join(parts, "  ")}
}

func hintsForDetail(v *View) paneHint {
	if v.versionsOpen {
		return paneHint{text: "↑/↓:Move  Esc:CloseVersions  Tab:Cycle"}
	}
	return paneHint{text: "↑/↓:Scroll  PgUp/PgDn:Page  Esc:Back  Tab:Cycle"}
}

// HandleEvent processes keyboard input for the Concepts tab. Takes
// the write lock for the duration so a concurrent Draw + a concurrent
// background SetConcepts both observe a stable, fully-applied state
// transition. The inner handlers use the *Locked variants since the
// lock is already held.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	// When the search box is active, keys go to the search box first.
	if v.searchOn {
		return v.handleSearchKeyLocked(keyEv)
	}

	if keyEv.Key() == tcell.KeyTab {
		v.Focus = (v.Focus + 1) % 3
		return true
	}

	// `:` anywhere -> jump to search. Mirrors the panel chrome
	// contract: the bottom-band hint advertises `:Search`, and the
	// active prompt renders as `:search <query>_` in the same band.
	// `/` is intentionally NOT a trigger -- see cli/CLAUDE.md.
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
			// to avoid a self-deadlock. State coherence is preserved
			// because openVersions reads the same fields we just set.
			v.mu.Unlock()
			v.openVersions()
			v.mu.Lock()
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
	switch ev.Key() {
	case tcell.KeyUp:
		if v.conceptSelected > 0 {
			v.conceptSelected--
			v.refreshRowsFromCurrentLocked()
		}
		return true
	case tcell.KeyDown:
		if v.conceptSelected < len(v.Concepts)-1 {
			v.conceptSelected++
			v.refreshRowsFromCurrentLocked()
		}
		return true
	case tcell.KeyEnter:
		v.refreshRowsFromCurrentLocked()
		v.Focus = FocusRows
		return true
	}
	return false
}

func (v *View) handleRowListKeyLocked(ev *tcell.EventKey) bool {
	matches := v.rowMatches
	switch ev.Key() {
	case tcell.KeyUp:
		if v.rowSelected > 0 {
			v.rowSelected--
			v.refreshDetailFromCurrentLocked()
		}
		return true
	case tcell.KeyDown:
		if v.rowSelected < len(matches)-1 {
			v.rowSelected++
			v.refreshDetailFromCurrentLocked()
		}
		return true
	case tcell.KeyEnter:
		v.refreshDetailFromCurrentLocked()
		v.Focus = FocusDetail
		return true
	case tcell.KeyEsc:
		// Esc clears an active filter so the user can get back to the
		// full row list without re-typing through the search box. The
		// `Esc:Clear search` chip in the bottom hint band advertises
		// this affordance; the chip only shows when rowFilter != "".
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
	switch ev.Key() {
	case tcell.KeyUp:
		if v.detailScroll > 0 {
			v.detailScroll--
		}
		return true
	case tcell.KeyDown:
		v.detailScroll++
		return true
	case tcell.KeyPgUp:
		v.detailScroll -= 10
		if v.detailScroll < 0 {
			v.detailScroll = 0
		}
		return true
	case tcell.KeyPgDn:
		v.detailScroll += 10
		return true
	case tcell.KeyHome:
		v.detailScroll = 0
		return true
	case tcell.KeyEsc:
		if v.versionsOpen {
			v.versionsOpen = false
			return true
		}
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
// callers that aren't already holding the write lock. Most event-loop
// handlers go through here; SetConcepts calls the *Locked variant
// directly since it already holds the lock.
func (v *View) refreshRowsFromCurrent() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refreshRowsFromCurrentLocked()
}

// refreshRowsFromCurrentLocked reloads rows for the currently-
// selected concept via the active QueryClient. Caller MUST hold
// v.mu (write). Runs the network call synchronously while holding
// the lock -- Draw blocks for the duration. For local clusters this
// is sub-100ms; remote slow clusters will manifest as a brief UI
// stall, which is correct behavior (the data is what's stalling, not
// the UI) and is preferable to either a torn read or a spinner-state
// flash. The async-with-spinner version is a future cleanup, not a
// hot path bug.
func (v *View) refreshRowsFromCurrentLocked() {
	v.Rows = nil
	v.detailLines = nil
	v.rowSelected = 0
	v.rowScrollY = 0
	v.versionsOpen = false
	if v.QueryClient == nil {
		v.rowMatches = nil
		return
	}
	if v.conceptSelected < 0 || v.conceptSelected >= len(v.Concepts) {
		v.rowMatches = nil
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		v.rowMatches = nil
		return
	}
	conceptId := v.Concepts[v.conceptSelected].GetId()
	if conceptId == "" {
		v.rowMatches = nil
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
	v.Rows = res.Rows()
	v.recomputeRowMatchesLocked()
	v.refreshDetailFromCurrentLocked()
}

func (v *View) refreshDetailFromCurrent() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refreshDetailFromCurrentLocked()
}

func (v *View) refreshDetailFromCurrentLocked() {
	v.detailScroll = 0
	matches := v.rowMatches
	if v.rowSelected < 0 || v.rowSelected >= len(matches) {
		v.detailLines = nil
		return
	}
	idx := matches[v.rowSelected]
	if idx < 0 || idx >= len(v.Rows) {
		// Defensive: matches was computed against an older v.Rows.
		// Shouldn't happen under the lock but cheap to guard.
		v.detailLines = nil
		return
	}
	v.detailLines = renderRowDetail(v.Rows[idx])
}

// openVersions fetches the time-series history for the selected row
// (concept + id) and swaps the detail pane into the version-list view.
func (v *View) openVersions() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.QueryClient == nil {
		return
	}
	matches := v.rowMatches
	if v.rowSelected < 0 || v.rowSelected >= len(matches) {
		return
	}
	idx := matches[v.rowSelected]
	if idx < 0 || idx >= len(v.Rows) {
		return
	}
	row := v.Rows[idx]
	rowId := getString(row, "id")
	conceptId := getString(row, "concept")
	if conceptId == "" && v.conceptSelected >= 0 && v.conceptSelected < len(v.Concepts) {
		conceptId = v.Concepts[v.conceptSelected].GetId()
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
	// Admin-surface escape hatch (see refreshRows for context). The
	// version-history pane needs the time-series tail of one row,
	// which is intrinsically concept-agnostic.
	res, err := qc.GetRowByConceptAndId(ctx, conceptId, rowId)
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("version history failed: %v", err))
		}
		return
	}
	v.versionRows = res.Rows()
	sort.Slice(v.versionRows, func(i, j int) bool {
		return getString(v.versionRows[i], "createdAt") > getString(v.versionRows[j], "createdAt")
	})
	v.versionSelected = 0
	v.versionScrollY = 0
	v.versionsOpen = true
}

func (v *View) recomputeRowMatches() {
	v.mu.Lock()
	defer v.mu.Unlock()
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
	if v.rowSelected >= len(v.rowMatches) {
		v.rowSelected = 0
	}
	v.refreshDetailFromCurrentLocked()
}

func (v *View) clampConceptScroll(visibleRows int) {
	if v.conceptSelected < v.conceptScrollY {
		v.conceptScrollY = v.conceptSelected
	}
	if v.conceptSelected >= v.conceptScrollY+visibleRows {
		v.conceptScrollY = v.conceptSelected - visibleRows + 1
	}
	if v.conceptScrollY < 0 {
		v.conceptScrollY = 0
	}
}

func (v *View) clampRowScroll(visibleRows int) {
	if v.rowSelected < v.rowScrollY {
		v.rowScrollY = v.rowSelected
	}
	if v.rowSelected >= v.rowScrollY+visibleRows {
		v.rowScrollY = v.rowSelected - visibleRows + 1
	}
	if v.rowScrollY < 0 {
		v.rowScrollY = 0
	}
}

func (v *View) clampDetailScroll(visibleRows int) {
	maxScroll := len(v.detailLines) - visibleRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	if v.detailScroll > maxScroll {
		v.detailScroll = maxScroll
	}
	if v.detailScroll < 0 {
		v.detailScroll = 0
	}
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

// --- generic renderer (Hybrid C) -----------------------------------

// renderRowDetail walks the row map and emits a human-readable
// declaration-style block. No concept-specific knowledge: every field
// renders the same way, so a brand-new concept is browsable the day
// it's declared. Sections: intrinsics (id, concept, createdBy,
// createdAt), payload (with nested object/array indentation),
// provenance (the engine intrinsic).
func renderRowDetail(row map[string]any) []string {
	if row == nil {
		return nil
	}
	var lines []string

	lines = append(lines, "INTRINSICS")
	for _, key := range []string{"id", "concept", "type", "createdBy", "createdAt"} {
		if v := getString(row, key); v != "" {
			lines = append(lines, fmt.Sprintf("  %-12s %s", key, v))
		}
	}

	if payload := getMap(row, "payload"); payload != nil {
		lines = append(lines, "")
		lines = append(lines, "PAYLOAD")
		lines = append(lines, renderObject(payload, 1)...)
	}

	if meta := getMap(row, "metadata"); meta != nil {
		if prov := getMap(meta, "provenance"); prov != nil {
			lines = append(lines, "")
			lines = append(lines, "PROVENANCE")
			lines = append(lines, renderObject(prov, 1)...)
		}
	}
	if prov := getMap(row, "provenance"); prov != nil {
		lines = append(lines, "")
		lines = append(lines, "PROVENANCE")
		lines = append(lines, renderObject(prov, 1)...)
	}

	return lines
}

func renderObject(obj map[string]any, depth int) []string {
	if obj == nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	indent := strings.Repeat("  ", depth)
	var lines []string
	for _, k := range keys {
		v := obj[k]
		switch val := v.(type) {
		case map[string]any:
			lines = append(lines, fmt.Sprintf("%s%s:", indent, k))
			lines = append(lines, renderObject(val, depth+1)...)
		case []any:
			lines = append(lines, fmt.Sprintf("%s%s: (%d items)", indent, k, len(val)))
			lines = append(lines, renderArray(val, depth+1)...)
		default:
			lines = append(lines, fmt.Sprintf("%s%-20s %s", indent, k+":", renderScalar(val)))
		}
	}
	return lines
}

func renderArray(arr []any, depth int) []string {
	indent := strings.Repeat("  ", depth)
	var lines []string
	limit := 10
	for i, v := range arr {
		if i >= limit {
			lines = append(lines, fmt.Sprintf("%s... (%d more)", indent, len(arr)-limit))
			break
		}
		switch val := v.(type) {
		case map[string]any:
			lines = append(lines, fmt.Sprintf("%s[%d]:", indent, i))
			lines = append(lines, renderObject(val, depth+1)...)
		default:
			lines = append(lines, fmt.Sprintf("%s[%d] %s", indent, i, renderScalar(val)))
		}
	}
	return lines
}

func renderScalar(v any) string {
	if v == nil {
		return "null"
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}
