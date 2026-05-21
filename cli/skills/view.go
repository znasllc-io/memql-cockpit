// Package skills renders the Skills tab: a read-only operator
// surface that lists the v1:agents:skill catalog the connected
// cluster has loaded. The user picks a skill from the left pane
// and sees its bundled domains / tools / live sources, tier,
// category, and lineage (predefined vs minted by the planner) in
// the detail pane on the right.
//
// Creation / editing is out of scope: the catalog is seeded from
// dsl/agents/skills/*.memql and re-seeded on every startup;
// runtime mints land via the Planner Agent's mintSkill flow
// (memql#159, Phase 3). This view is the operator's window into
// what shipped + what the planner added.
//
// Data plane: queryActiveSkillsFull returns every row with the
// full bundle composition. The query lives in
// memql/dsl/agents/queries.memql and reaches the cockpit through
// the generated SDK method on QueryClient.
//
// Conventions: follows the standard list+detail layout +
// keybinding chrome contract laid out in cli/CLAUDE.md.
package skills

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
// active. Tab cycles between them.
type FocusPane int

const (
	FocusList   FocusPane = 0
	FocusDetail FocusPane = 1
)

const focusPaneCount = 2

// View is the Skills tab. Mirrors agents/view.go's locking +
// refresh-loop conventions.
type View struct {
	ui.BaseView

	skills []map[string]any
	Focus  FocusPane

	fetching    bool
	QueryClient func() *client.QueryClient

	skillList  ui.ListPane
	detailPane ui.DetailPane
}

// NewView creates a fresh Skills tab focused on the list pane.
func NewView(theme ui.Theme) *View {
	v := &View{Focus: FocusList}
	v.Theme = theme
	v.skillList.RowsPerItem = 2
	v.skillList.Render = v.renderSkillRow
	return v
}

// StartRefreshLoop polls queryActiveSkillsFull on the given
// interval until stop closes.
func (v *View) StartRefreshLoop(stop <-chan struct{}, interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	v.BaseView.StartRefreshLoop(ctx, interval, v.Refresh)
}

// Refresh re-pulls the skill catalog. Best-effort: query failures
// surface to the status bar but leave the existing list in place.
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
	v.fetching = true
	v.Mu.Unlock()
	defer func() {
		v.Mu.Lock()
		v.fetching = false
		v.Mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	res, err := qc.QueryActiveSkillsFull(ctx, client.QueryActiveSkillsFullArgs{})
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("skills: queryActiveSkillsFull failed: %v", err))
		}
		return
	}
	rows := res.Rows()

	// Sort: predefined first (catalog-anchored), then category, tier,
	// name. Stable so re-renders don't shuffle rows under the cursor.
	sort.SliceStable(rows, func(i, j int) bool {
		pi := boolFrom(rows[i], "predefined")
		pj := boolFrom(rows[j], "predefined")
		if pi != pj {
			return pi
		}
		ci := getString(rows[i], "category")
		cj := getString(rows[j], "category")
		if ci != cj {
			return ci < cj
		}
		ti := getString(rows[i], "tier")
		tj := getString(rows[j], "tier")
		if ti != tj {
			return ti < tj
		}
		ni := strings.ToLower(getString(rows[i], "name"))
		nj := strings.ToLower(getString(rows[j], "name"))
		return ni < nj
	})

	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.skills = rows
	v.skillList.Count = len(v.skills)
	if v.skillList.Selected >= len(v.skills) {
		v.skillList.Selected = 0
		v.skillList.ScrollY = 0
		v.detailPane.ScrollY = 0
	}
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// Draw paints the tab. Holds the read lock so concurrent refreshes
// can't tear the underlying slices mid-paint.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.Mu.RLock()
	defer v.Mu.RUnlock()

	if v.GatedMessage != "" {
		v.drawGated(screen, bounds, v.GatedMessage)
		return
	}

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.35, MinSize: 28},
		{Flex: 0.65, MinSize: 32},
	})
	listBounds := panes[0]
	detailBounds := panes[1]

	divStyle := v.Theme.SubtleStyle()
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		screen.SetCell(listBounds.X+listBounds.Width-1, y, '│', divStyle)
	}
	listBounds.Width--

	v.drawList(screen, listBounds)
	v.drawDetail(screen, detailBounds)
}

func (v *View) drawGated(screen *ui.Screen, bounds ui.Rect, msg string) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	ui.PaneTitle{Title: "SKILLS"}.Draw(screen, bounds, v.Theme)
	drawCentered(screen, v.Theme, bounds, msg)
}

func paneChromeBounds(bounds ui.Rect) ui.Rect {
	const chromeH = 1
	listTop := bounds.Y + 2
	listH := bounds.Height - 2 - chromeH
	if listH < 1 {
		listH = 1
	}
	return ui.Rect{X: bounds.X, Y: listTop, Width: bounds.Width, Height: listH}
}

func (v *View) drawList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	ui.PaneTitle{
		Title:   "SKILLS",
		Counter: ui.FormatCursor(v.skillList.Selected, len(v.skills)),
		Focused: v.Focus == FocusList,
	}.Draw(screen, bounds, v.Theme)

	if len(v.skills) == 0 {
		drawCentered(screen, v.Theme, bounds,
			"No skills loaded yet. The catalog re-seeds on every cluster startup; check that memql#157 + #158 have landed.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForListEmpty())
		return
	}

	v.skillList.Count = len(v.skills)
	v.skillList.Focused = v.Focus == FocusList
	v.skillList.Draw(screen, paneChromeBounds(bounds), v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForList())
}

// renderSkillRow paints a 2-row skill entry. Primary: marker + name +
// "[tier X]". Subtitle: category · tag-summary.
func (v *View) renderSkillRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.skills) {
		return
	}
	s := v.skills[idx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	name := getString(s, "name")
	if name == "" {
		name = getString(s, "id")
	}
	// Active row marker is strictly single-width ASCII per the layout-
	// edge glyph rule. '*' marks predefined catalog rows.
	marker := " "
	if boolFrom(s, "predefined") {
		marker = "*"
	}
	tier := getString(s, "tier")
	headline := fmt.Sprintf("%s %s [tier %s]", marker, name, tier)
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, headline, primary)

	category := getString(s, "category")
	subStr := category
	if subStr == "" {
		subStr = "(no category)"
	}
	if tags := stringSliceFrom(s, "tags"); len(tags) > 0 {
		subStr = fmt.Sprintf("%s · %s", subStr, strings.Join(tags, ", "))
	}
	screen.DrawText(bounds.X+3, bounds.Y+1, bounds.Width-4, subStr, sub)
}

func (v *View) drawDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	ui.PaneTitle{
		Title:   "SKILL DETAIL",
		Focused: v.Focus == FocusDetail,
	}.Draw(screen, bounds, v.Theme)

	if len(v.skills) == 0 || v.skillList.Selected < 0 || v.skillList.Selected >= len(v.skills) {
		drawCentered(screen, v.Theme, bounds,
			"Select a skill from the list to see its bundle composition, lineage, and tier.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetailEmpty())
		return
	}

	s := v.skills[v.skillList.Selected]
	innerW := bounds.Width - 3
	if innerW < 16 {
		innerW = 16
	}
	v.detailPane.Lines = v.buildDetailLines(s, innerW)
	v.detailPane.Focused = v.Focus == FocusDetail

	inner := paneChromeBounds(bounds)
	inner.X += 1
	inner.Width -= 1
	if inner.Width < 1 {
		inner.Width = 1
	}
	v.detailPane.Draw(screen, inner, v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetail())
}

// buildDetailLines flattens the skill record into the detail pane's
// section-based layout. Mirrors agents/view.go's buildDetailLines.
func (v *View) buildDetailLines(s map[string]any, innerWidth int) []ui.DetailLine {
	var lines []ui.DetailLine
	plain := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LinePlain, Text: s} }
	section := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LineSection, Text: s} }
	kv := func(k, val string) ui.DetailLine { return ui.DetailLine{Kind: ui.LineKV, Key: k, Value: val} }

	lines = append(lines, section("identity"))
	lines = append(lines, kv("id", getString(s, "id")))
	lines = append(lines, kv("slug", getString(s, "slug")))
	lines = append(lines, kv("name", getString(s, "name")))
	lines = append(lines, kv("category", getString(s, "category")))
	lines = append(lines, kv("tier", getString(s, "tier")))
	lines = append(lines, kv("active", boolLabel(boolFrom(s, "active"))))
	lines = append(lines, kv("predefined", boolLabel(boolFrom(s, "predefined"))))

	if desc := getString(s, "description"); desc != "" {
		lines = append(lines, plain(""))
		lines = append(lines, plain(desc))
	}

	lines = append(lines, section("tags"))
	addList(&lines, stringSliceFrom(s, "tags"), "(no tags)")

	lines = append(lines, section("knowledge domains"))
	addList(&lines, stringSliceFrom(s, "domainIds"), "(no domains bundled)")

	lines = append(lines, section("tools / integrations"))
	addList(&lines, stringSliceFrom(s, "toolSlugs"), "(no tools bundled)")

	lines = append(lines, section("live knowledge sources"))
	addList(&lines, stringSliceFrom(s, "liveSourceIds"), "(no live sources bundled)")

	lines = append(lines, section("lineage"))
	lines = append(lines, kv("created", shortenTimestamp(getString(s, "createdAt"))))
	lines = append(lines, kv("created by", getString(s, "createdBy")))

	return lines
}

func addList(lines *[]ui.DetailLine, items []string, empty string) {
	if len(items) == 0 {
		if empty != "" {
			*lines = append(*lines, ui.DetailLine{Kind: ui.LinePlain, Text: "  " + empty})
		}
		return
	}
	for _, item := range items {
		*lines = append(*lines, ui.DetailLine{Kind: ui.LinePlain, Text: "  - " + item})
	}
}

// ---------------------------------------------------------------------------
// Input
// ---------------------------------------------------------------------------

// HandleEvent processes a key event. Returns true if consumed.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	if keyEv.Key() == tcell.KeyTab {
		v.Mu.Lock()
		v.Focus = (v.Focus + 1) % FocusPane(focusPaneCount)
		v.Mu.Unlock()
		return true
	}

	if keyEv.Key() == tcell.KeyRune && (keyEv.Rune() == 'r' || keyEv.Rune() == 'R') {
		go func() {
			v.Refresh()
			v.Redraw()
		}()
		return true
	}

	switch v.Focus {
	case FocusList:
		return v.handleListKey(keyEv)
	case FocusDetail:
		return v.handleDetailKey(keyEv)
	}
	return false
}

func (v *View) handleListKey(ev *tcell.EventKey) bool {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.skillList.Focused = true
	prev := v.skillList.Selected
	if v.skillList.HandleEvent(ev) {
		if v.skillList.Selected != prev {
			v.detailPane.ScrollY = 0
		}
		return true
	}
	if ev.Key() == tcell.KeyEnter {
		v.Focus = FocusDetail
		return true
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
		v.Focus = FocusList
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Hints
// ---------------------------------------------------------------------------

func hintsForList() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Enter", Label: "Open"},
		{Key: "R", Label: "Refresh"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

func hintsForListEmpty() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "R", Label: "Refresh"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

func hintsForDetail() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Scroll"},
		{Key: "PgUp/PgDn", Label: "Page"},
		{Key: "Home", Label: "Top"},
		{Key: "Esc", Label: "Back"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

func hintsForDetailEmpty() string {
	bar := ui.HintBar{Chips: []ui.HintChip{{Key: "Tab", Label: "Cycle"}}}
	return bar.String()
}

// ---------------------------------------------------------------------------
// Helpers (kept local; mirror the agents view's tolerant readers)
// ---------------------------------------------------------------------------

func getString(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return v
	}
	return ""
}

func boolFrom(row map[string]any, key string) bool {
	if v, ok := row[key].(bool); ok {
		return v
	}
	return false
}

func boolLabel(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func stringSliceFrom(row map[string]any, key string) []string {
	switch x := row[key].(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func shortenTimestamp(ts string) string {
	if len(ts) >= 19 {
		return ts[:19]
	}
	return ts
}

func drawCentered(screen *ui.Screen, theme ui.Theme, bounds ui.Rect, msg string) {
	innerW := bounds.Width - 2
	if innerW < 1 {
		innerW = 1
	}
	var lines []string
	for _, paragraph := range strings.Split(msg, "\n") {
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, ui.WrapText(paragraph, innerW)...)
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
		lineX := bounds.X + (bounds.Width-len(line))/2
		if lineX < bounds.X+1 {
			lineX = bounds.X + 1
		}
		screen.DrawText(lineX, y, bounds.Width-1, line, theme.SubtleStyle())
	}
}
