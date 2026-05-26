// Package safety renders the Command Safety tab: a read-only
// operator surface for observing v1:safety:classification rows
// emitted by the memQL command-classifier (memql#234 / #259).
//
// Why this tab exists: the classifier rollout (memql#235) needs
// operators to see what shadow mode would have done so they can
// flip MEMQL_COMMAND_CLASSIFIER_MODE per surface with FP/FN data
// instead of by gut feel. memql-cockpit#134 is the cockpit side
// of that story.
//
// Data plane: queryAllSafetyClassifications (memql/dsl/safety/
// queries.memql). Layered, surface-specific queries
// (querySafetyClassificationsRecent / *ByDecision / *BySurface)
// land on the memQL side when the row count justifies them; the
// view filters in memory until then so first-paint isn't blocked
// on a memql release.
//
// Layout follows the cli/CLAUDE.md panel chrome contract:
//
//   LEFT (DECISIONS)                            RIGHT (DETAIL)
//   title + filtered counter                    title + line counter
//   row list, 2-row items                       generic field list
//   aggregate strip (totals + breakdowns)       (scrolls independently)
//   filter chip strip
//   action hints
//
// Both panes embed cli/ui widgets (ListPane / DetailPane) so
// scroll, scrollbar, page keys, and key-routing behave the same as
// every other tab.
package safety

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// FocusPane identifies which of the two keyboard-focus regions is
// active. Tab cycles through them in order.
type FocusPane int

const (
	FocusDecisions FocusPane = 0
	FocusDetail    FocusPane = 1
)

const focusPaneCount = 2

// reservedChromeRows is how many rows the LEFT pane reserves below
// the row list for the aggregate / filter / hint chrome. Two rows
// of aggregate (totals + source/tier/mode breakdown), one row of
// filter chips, one row of hints = 4. A blank gap row above the
// aggregate matches the chrome contract's "lives ABOVE the chrome
// with a one-row gap" rule for pinned detail blocks.
const reservedChromeRows = 5

// refreshTimeout caps each queryAllSafetyClassifications round-trip.
// Mirrors the planner view's 6s budget so a slow cluster doesn't
// freeze the refresh loop.
const refreshTimeout = 6 * time.Second

// View is the Safety tab. Mutated from both the event-loop
// goroutine (HandleEvent) and a background refresher
// (StartRefreshLoop). Locking mirrors the planner / concepts
// tabs: write-lock on mutators, read-lock on Draw.
type View struct {
	ui.BaseView // Mu / Theme / GatedMessage / OnStatus / OnRedraw

	rows        []map[string]any // newest-first
	rowMatches  []int            // indices into rows that pass the filter
	filter      Filter
	searchInput string
	searchOn    bool

	Focus    FocusPane
	fetching bool

	// dslMissing latches when the cluster's BFF rejects the safety
	// query as "function not found" -- same pattern as the planner
	// view's gate. Cleared by R:Refresh so a fresh deploy that
	// loads the safety DSL picks back up without restarting.
	dslMissing bool

	// Plumbing.
	QueryClient func() *client.QueryClient

	decisionList ui.ListPane
	detailPane   ui.DetailPane
}

// NewView creates a fresh Safety view focused on the decisions list.
func NewView(theme ui.Theme) *View {
	v := &View{Focus: FocusDecisions}
	v.Theme = theme
	v.decisionList.RowsPerItem = 2
	v.decisionList.Render = v.renderDecisionRow
	v.decisionList.EmptyMessage = "No safety classifications yet for this cluster."
	return v
}

// StartRefreshLoop runs a background ticker that re-pulls
// classifications every interval. The caller passes a stop channel;
// closing it cancels the loop.
func (v *View) StartRefreshLoop(stop <-chan struct{}, interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	v.BaseView.StartRefreshLoop(ctx, interval, v.Refresh)
}

// Refresh re-pulls the full classification list. Safe from any
// goroutine. Sorts rows newest-first by createdAt.
func (v *View) Refresh() {
	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}

	v.Mu.Lock()
	if v.fetching {
		v.Mu.Unlock()
		return
	}
	if v.dslMissing {
		v.Mu.Unlock()
		return
	}
	v.fetching = true
	v.Mu.Unlock()

	defer func() {
		v.Mu.Lock()
		v.fetching = false
		v.Mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()

	res, err := qc.QueryAllSafetyClassifications(ctx, client.QueryAllSafetyClassificationsArgs{})
	if err != nil {
		if isSafetyDSLMissing(err) {
			v.markDSLMissing()
			return
		}
		v.Notify(fmt.Sprintf("safety: queryAllSafetyClassifications failed: %v", err))
		return
	}

	rows := res.Rows()
	sort.Slice(rows, func(i, j int) bool {
		return getString(rows[i], "createdAt") > getString(rows[j], "createdAt")
	})

	v.Mu.Lock()
	v.rows = rows
	v.recomputeMatchesLocked()
	v.Mu.Unlock()
}

// recomputeMatchesLocked walks v.rows under v.filter and refreshes
// v.rowMatches + the list widget counters. Caller holds v.Mu.
func (v *View) recomputeMatchesLocked() {
	v.rowMatches = v.rowMatches[:0]
	for i, r := range v.rows {
		if v.filter.Match(r) {
			v.rowMatches = append(v.rowMatches, i)
		}
	}
	v.decisionList.Count = len(v.rowMatches)
	if v.decisionList.Selected >= v.decisionList.Count {
		v.decisionList.Selected = 0
		v.decisionList.ScrollY = 0
	}
	if v.decisionList.Selected < 0 {
		v.decisionList.Selected = 0
	}
}

// selectedRowLocked returns the highlighted row (after filtering),
// or nil when nothing is selected. Caller holds v.Mu (read OK).
func (v *View) selectedRowLocked() map[string]any {
	if v.decisionList.Selected < 0 || v.decisionList.Selected >= len(v.rowMatches) {
		return nil
	}
	idx := v.rowMatches[v.decisionList.Selected]
	if idx < 0 || idx >= len(v.rows) {
		return nil
	}
	return v.rows[idx]
}

// markDSLMissing latches the dslMissing flag. The gated screen
// then explains how to unblock without retrying the failing call
// every refresh tick.
func (v *View) markDSLMissing() {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.dslMissing = true
}

// isSafetyDSLMissing classifies an Execute error as "the BFF's
// engine doesn't have the safety queries registered" vs. anything
// else. Same shape as the planner view's classifier.
func isSafetyDSLMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") &&
		strings.Contains(msg, `function "queryAllSafetyClassifications"`)
}

// ---------------------------------------------------------------------------
// Draw
// ---------------------------------------------------------------------------

// Draw renders the Safety tab. Holds the read lock so a concurrent
// Refresh can't tear the row slice mid-paint.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.Mu.RLock()
	defer v.Mu.RUnlock()

	if v.GatedMessage != "" {
		v.drawGated(screen, bounds, v.GatedMessage)
		return
	}
	if v.dslMissing {
		v.drawGated(screen, bounds,
			"Safety DSL not loaded on this cluster. The Command "+
				"Safety tab requires queryAllSafetyClassifications "+
				"to be registered with the BFF engine (the safety "+
				"namespace provides it). Press R to retry once the "+
				"BFF has been redeployed.")
		return
	}

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.58, MinSize: 48},
		{Flex: 0.42, MinSize: 32},
	})
	leftBounds := panes[0]
	rightBounds := panes[1]

	// Single box-drawing divider between the two panes. │ is East
	// Asian Width Narrow, safe at the edge per the layout-edge rule.
	divStyle := v.Theme.SubtleStyle()
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		screen.SetCell(leftBounds.X+leftBounds.Width-1, y, '│', divStyle)
	}
	leftBounds.Width--

	v.drawDecisionList(screen, leftBounds)
	v.drawDetail(screen, rightBounds)
}

// drawGated renders the centered "tab unavailable" layout with the
// supplied message. Same shape as the planner version.
func (v *View) drawGated(screen *ui.Screen, bounds ui.Rect, msg string) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	ui.PaneTitle{Title: "COMMAND SAFETY"}.Draw(screen, bounds, v.Theme)
	wrapped := ui.WrapText(msg, bounds.Width-4)
	if len(wrapped) == 0 {
		wrapped = []string{msg}
	}
	startY := bounds.Y + bounds.Height/2 - len(wrapped)/2
	if startY < bounds.Y+1 {
		startY = bounds.Y + 1
	}
	for i, line := range wrapped {
		if startY+i >= bounds.Y+bounds.Height {
			break
		}
		lineX := bounds.X + (bounds.Width-len(line))/2
		if lineX < bounds.X+1 {
			lineX = bounds.X + 1
		}
		screen.DrawText(lineX, startY+i, bounds.Width-1, line, v.Theme.SubtleStyle())
	}
}

func (v *View) drawDecisionList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	ui.PaneTitle{
		Title:   "DECISIONS",
		Counter: ui.FormatFiltered(v.decisionList.Selected, len(v.rowMatches), len(v.rows)),
		Focused: v.Focus == FocusDecisions,
	}.Draw(screen, bounds, v.Theme)

	// Reserve the bottom chrome rows for the aggregate strip + filter
	// chips + action hints. listH is what remains for the scrolling
	// row list.
	const titleH = 2
	listH := bounds.Height - titleH - reservedChromeRows
	if listH < 1 {
		listH = 1
	}
	listBounds := ui.Rect{X: bounds.X, Y: bounds.Y + titleH, Width: bounds.Width, Height: listH}

	v.decisionList.Count = len(v.rowMatches)
	v.decisionList.Focused = v.Focus == FocusDecisions
	v.decisionList.Draw(screen, listBounds, v.Theme)

	// Aggregate + filter chrome sits above the action-hint row.
	v.drawAggregateAndFilters(screen, bounds)
	v.drawBottomHints(screen, bounds)
}

// drawAggregateAndFilters paints the totals / breakdown / filter
// chip block that lives ABOVE the bottom chrome row per the
// chrome contract's "pinned detail block" rule. Two rows for
// aggregates, one row for filter chips.
func (v *View) drawAggregateAndFilters(screen *ui.Screen, bounds ui.Rect) {
	subset := make([]map[string]any, 0, len(v.rowMatches))
	for _, i := range v.rowMatches {
		if i >= 0 && i < len(v.rows) {
			subset = append(subset, v.rows[i])
		}
	}
	agg := Summarise(subset)

	totals := fmt.Sprintf("totals %d  allow %d  ask %d  deny %d",
		agg.Total, agg.Decision["allow"], agg.Decision["ask"], agg.Decision["deny"])
	breakdown := fmt.Sprintf("rule %d  model %d  cache %d  noop %d   shadow %d  enforce %d  off %d",
		agg.Source["rule"], agg.Source["model"], agg.Source["cache"], agg.Source["noop"],
		agg.Mode["shadow"], agg.Mode["enforce"], agg.Mode["off"])
	// Each chip embeds the cycle key in brackets so the filter strip
	// is self-documenting -- the bottom hint band stays terse, and
	// the operator sees "press D to cycle decision" at the same row
	// as the current decision value.
	chips := fmt.Sprintf("[D]decision:%s  [S]source:%s  [T]tier:%s  [U]surface:%s  [M]mode:%s",
		chipValue(v.filter.Decision), chipValue(v.filter.Source),
		chipValue(v.filter.Tier), chipValue(v.filter.Surface), chipValue(v.filter.Mode))

	// Stack from bottom up: totals (top), breakdown (middle), chips
	// (closest to chrome). DrawBottomBlocks paints the LAST block on
	// the bottom row -- we want the chip row right above the hint
	// row, so it goes last. Chips render in accent style so they
	// read at a glance; totals/breakdown stay subtle.
	chipReserved := bounds.Height
	_ = chipReserved
	// Carve out a sub-rect that excludes the action-hint row at the
	// very bottom so the chip rows don't overlap it.
	chromeBounds := ui.Rect{
		X:      bounds.X,
		Y:      bounds.Y,
		Width:  bounds.Width,
		Height: bounds.Height - 1, // leaves the last row for action hints
	}
	ui.DrawBottomBlocks(screen, chromeBounds, 1,
		ui.BottomBlock{Lines: []string{totals}, Style: v.Theme.SubtleStyle()},
		ui.BottomBlock{Lines: []string{breakdown}, Style: v.Theme.SubtleStyle()},
		ui.BottomBlock{Lines: []string{chips}, Style: v.Theme.AccentStyle()},
	)
}

func (v *View) drawBottomHints(screen *ui.Screen, bounds ui.Rect) {
	if v.searchOn {
		ui.DrawBottom(screen, bounds, v.Theme.AccentStyle(), 1, ":search "+v.searchInput+"_")
		return
	}
	// The per-axis cycle keys (D/S/T/U/M) live in the filter chip
	// strip so the bottom hint band stays narrow enough to fit the
	// pane width without wrapping. Esc:ClearFilters only appears
	// once a filter is active per the "hints that lie rot trust"
	// rule.
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: ":", Label: "Search"},
		{Key: "R", Label: "Refresh"},
		{Key: "Esc", Label: "ClearFilters", Disabled: !v.anyFilterActive()},
		{Key: "Tab", Label: "Cycle"},
	}}
	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, bar.String())
}

func (v *View) anyFilterActive() bool {
	return v.filter.Surface != "" || v.filter.Decision != "" ||
		v.filter.Source != "" || v.filter.Tier != "" ||
		v.filter.Mode != "" || v.filter.Search != ""
}

func (v *View) drawDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	row := v.selectedRowLocked()

	totalLines := len(v.detailPane.Lines)
	ui.PaneTitle{
		Title:   "DETAIL",
		Counter: ui.FormatLine(v.detailPane.ScrollY, totalLines),
		Focused: v.Focus == FocusDetail,
	}.Draw(screen, bounds, v.Theme)

	if row == nil {
		drawCentered(screen, v.Theme, bounds,
			"No decision selected. Pick a row in the Decisions pane to see the redacted args, full reason, and rule id.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetailEmpty())
		return
	}

	innerW := bounds.Width - 3
	if innerW < 16 {
		innerW = 16
	}
	v.detailPane.Lines = buildDetailLines(row, innerW)
	v.detailPane.Focused = v.Focus == FocusDetail

	const titleH = 2
	const chromeH = 1
	inner := ui.Rect{
		X:      bounds.X + 1,
		Y:      bounds.Y + titleH,
		Width:  bounds.Width - 2,
		Height: bounds.Height - titleH - chromeH,
	}
	if inner.Height < 1 {
		inner.Height = 1
	}
	v.detailPane.Draw(screen, inner, v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetail())
}

// renderDecisionRow paints a 2-row decision entry. Primary line is
// timestamp + surface + action + decision; subtitle is tier +
// source + reason snippet. Layout mirrors the planner / concepts
// 2-row item style.
func (v *View) renderDecisionRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.rowMatches) {
		return
	}
	row := v.rows[v.rowMatches[idx]]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	ts := shortenTimestamp(getString(row, "createdAt"))
	surface := getString(row, "surface")
	action := getString(row, "action")
	decision := strings.ToUpper(getString(row, "decision"))

	primaryStr := fmt.Sprintf("%-8s  %-22s  %-14s  %s",
		ts, truncate(surface, 22), truncate(action, 14), decision)
	screen.DrawText(bounds.X+2, bounds.Y, bounds.Width-3, primaryStr, primary)

	tier := getString(row, "tier")
	source := getString(row, "source")
	reason := getString(row, "reason")
	subStr := fmt.Sprintf("tier:%-8s  via:%-8s  %s", tier, source, reason)
	screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-3, subStr, sub)
}

// buildDetailLines flattens a classification row into a list of
// DetailLine entries for DetailPane. Pure function so it's easy
// to unit-test against fixture rows.
func buildDetailLines(row map[string]any, innerWidth int) []ui.DetailLine {
	if innerWidth < 16 {
		innerWidth = 16
	}
	var lines []ui.DetailLine

	section := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LineSection, Text: "─ " + s + " ─"} }
	plain := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LinePlain, Text: s} }
	kv := func(k, val string) ui.DetailLine {
		if val == "" {
			val = "—"
		}
		return ui.DetailLine{Kind: ui.LineKV, Key: "  " + k, Value: val}
	}

	lines = append(lines, section("verdict"))
	lines = append(lines, kv("decision", getString(row, "decision")))
	lines = append(lines, kv("tier", getString(row, "tier")))
	lines = append(lines, kv("categories", getString(row, "categories")))
	lines = append(lines, kv("source", getString(row, "source")))
	lines = append(lines, kv("rule id", getString(row, "ruleId")))
	if c := getFloat(row, "confidence"); c > 0 {
		lines = append(lines, kv("confidence", fmt.Sprintf("%.2f", c)))
	}
	if lat := getFloat(row, "latencyMs"); lat > 0 {
		lines = append(lines, kv("latency", fmt.Sprintf("%.1f ms", lat)))
	}
	lines = append(lines, kv("mode", getString(row, "mode")))

	lines = append(lines, plain(""))
	lines = append(lines, section("target"))
	lines = append(lines, kv("surface", getString(row, "surface")))
	lines = append(lines, kv("action", getString(row, "action")))

	lines = append(lines, plain(""))
	lines = append(lines, section("identity"))
	lines = append(lines, kv("row id", getString(row, "id")))
	lines = append(lines, kv("created", shortenTimestamp(getString(row, "createdAt"))))
	lines = append(lines, kv("agent", getString(row, "agentId")))
	lines = append(lines, kv("owner user", getString(row, "ownerUserId")))
	lines = append(lines, kv("plan", getString(row, "planId")))
	lines = append(lines, kv("correlation", getString(row, "correlationId")))

	if reason := getString(row, "reason"); reason != "" {
		lines = append(lines, plain(""))
		lines = append(lines, section("reason"))
		for _, w := range ui.WrapText(reason, innerWidth-2) {
			lines = append(lines, plain("  "+w))
		}
	}

	if args := getString(row, "argsRedacted"); args != "" {
		lines = append(lines, plain(""))
		lines = append(lines, section("argsRedacted"))
		for _, raw := range strings.Split(args, "\n") {
			if raw == "" {
				lines = append(lines, plain(""))
				continue
			}
			for _, w := range ui.WrapText(raw, innerWidth-2) {
				lines = append(lines, plain("  "+w))
			}
		}
	}

	return lines
}

// ---------------------------------------------------------------------------
// Hints
// ---------------------------------------------------------------------------

func hintsForDetail() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Scroll"},
		{Key: "PgUp/PgDn", Label: "Page"},
		{Key: "Esc", Label: "Decisions"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

func hintsForDetailEmpty() string {
	bar := ui.HintBar{Chips: []ui.HintChip{{Key: "Tab", Label: "Cycle"}}}
	return bar.String()
}

// ---------------------------------------------------------------------------
// Input handling
// ---------------------------------------------------------------------------

// HandleEvent processes a key event. Returns true if consumed.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// Search input mode captures everything except Esc/Enter so
	// modifier keys don't leak into filter cycling.
	v.Mu.RLock()
	searchOn := v.searchOn
	v.Mu.RUnlock()
	if searchOn {
		return v.handleSearchKey(keyEv)
	}

	// Tab cycles focus regardless of pane.
	if keyEv.Key() == tcell.KeyTab {
		v.Mu.Lock()
		v.Focus = (v.Focus + 1) % FocusPane(focusPaneCount)
		v.Mu.Unlock()
		return true
	}

	// R refreshes from any pane.
	if keyEv.Key() == tcell.KeyRune && (keyEv.Rune() == 'r' || keyEv.Rune() == 'R') {
		v.Mu.Lock()
		v.dslMissing = false
		v.Mu.Unlock()
		go func() {
			v.Refresh()
			v.Redraw()
		}()
		return true
	}

	switch v.Focus {
	case FocusDecisions:
		return v.handleDecisionsKey(keyEv)
	case FocusDetail:
		return v.handleDetailKey(keyEv)
	}
	return false
}

func (v *View) handleDecisionsKey(ev *tcell.EventKey) bool {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.decisionList.Focused = true

	if v.decisionList.HandleEvent(ev) {
		// Reset detail scroll so a fresh row starts at the top.
		v.detailPane.ScrollY = 0
		return true
	}

	switch ev.Key() {
	case tcell.KeyEnter:
		v.Focus = FocusDetail
		return true
	case tcell.KeyEsc:
		if v.anyFilterActive() {
			v.filter = Filter{}
			v.searchInput = ""
			v.recomputeMatchesLocked()
			return true
		}
		return false
	case tcell.KeyRune:
		switch ev.Rune() {
		case ':':
			v.searchOn = true
			v.searchInput = v.filter.Search
			return true
		case 'd', 'D':
			v.filter.Decision = cycleNext(decisionCycle, v.filter.Decision)
			v.recomputeMatchesLocked()
			return true
		case 's', 'S':
			v.filter.Source = cycleNext(sourceCycle, v.filter.Source)
			v.recomputeMatchesLocked()
			return true
		case 't', 'T':
			v.filter.Tier = cycleNext(tierCycle, v.filter.Tier)
			v.recomputeMatchesLocked()
			return true
		case 'u', 'U':
			v.filter.Surface = cycleSurface(v.rows, v.filter.Surface)
			v.recomputeMatchesLocked()
			return true
		case 'm', 'M':
			v.filter.Mode = cycleNext(modeCycle, v.filter.Mode)
			v.recomputeMatchesLocked()
			return true
		}
	}
	return false
}

func (v *View) handleDetailKey(ev *tcell.EventKey) bool {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.detailPane.Focused = true
	if v.detailPane.HandleEvent(ev) {
		return true
	}
	if ev.Key() == tcell.KeyEsc {
		v.Focus = FocusDecisions
		return true
	}
	return false
}

func (v *View) handleSearchKey(ev *tcell.EventKey) bool {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	switch ev.Key() {
	case tcell.KeyEnter:
		v.filter.Search = v.searchInput
		v.searchOn = false
		v.recomputeMatchesLocked()
		return true
	case tcell.KeyEsc:
		v.searchOn = false
		v.searchInput = v.filter.Search
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if n := len(v.searchInput); n > 0 {
			v.searchInput = v.searchInput[:n-1]
		}
		return true
	case tcell.KeyRune:
		v.searchInput += string(ev.Rune())
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Helpers (timestamp / truncate / centered text)
// ---------------------------------------------------------------------------

func drawCentered(screen *ui.Screen, theme ui.Theme, bounds ui.Rect, msg string) {
	innerW := bounds.Width - 2
	if innerW < 1 {
		innerW = 1
	}
	lines := ui.WrapText(msg, innerW)
	if len(lines) == 0 {
		lines = []string{msg}
	}
	startY := bounds.Y + (bounds.Height-len(lines))/2
	if startY < bounds.Y+1 {
		startY = bounds.Y + 1
	}
	for i, line := range lines {
		y := startY + i
		if y >= bounds.Y+bounds.Height {
			break
		}
		x := bounds.X + (bounds.Width-len(line))/2
		if x < bounds.X+1 {
			x = bounds.X + 1
		}
		screen.DrawText(x, y, bounds.Width-1, line, theme.SubtleStyle())
	}
}

// shortenTimestamp turns "2026-05-26T12:14:53.123Z" into
// "12:14:53". Matches the planner view's surface treatment for
// list rows where vertical real estate is scarce.
func shortenTimestamp(ts string) string {
	if ts == "" {
		return ""
	}
	if i := strings.IndexByte(ts, 'T'); i >= 0 && i+9 <= len(ts) {
		return ts[i+1 : i+9]
	}
	return ts
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return s[:max-1] + "…"
}
