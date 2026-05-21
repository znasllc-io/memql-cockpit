// Package agents renders the Agents tab: a read-only operator
// surface that lists the AI agent templates registered in the
// connected cluster. The user picks an agent from the left pane and
// sees its identity, capabilities, knowledge surface, and recent
// plan attribution in the detail pane on the right.
//
// Creation / editing of agents is intentionally out of scope -- the
// cockpit's job here is observability. Mutations live in CoPresent
// (CreateAgentModal etc.) and in the platform's seed automations.
//
// Data plane:
//   - queryAllAgents -- the catalog of v1:agents:agent rows.
//   - queryListKnowledgeDomains -- knowledgeDomain.id -> displayName
//     lookup so capabilities.domains[] reads as human-friendly names
//     instead of opaque ids.
//   - queryAllPlans -- every plan; we filter client-side to
//     whichever ones name this agent in payload.ownerAgentId for
//     the "recent tasks" section.
//
// All three queries live in memql/dsl/{agents,knowledge,planner}/queries.memql
// and are loaded by every BFF / node binary as part of the core DSL tree.
//
// Migrated to the cli/ui widget layer (epic #81).
package agents

import (
	"context"
	"encoding/json"
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
	FocusList   FocusPane = 0 // left pane: agent list
	FocusDetail FocusPane = 1 // right pane: agent detail
)

const focusPaneCount = 2

// maxRecentPlans bounds the "recent plans" section of the detail
// pane.
const maxRecentPlans = 10

// View is the Agents tab. Locking mirrors the Planner / Concepts
// views: write lock on data mutations, read lock on Draw.
type View struct {
	ui.BaseView // Mu / Theme / GatedMessage / OnStatus / OnRedraw

	// Agent catalog. Selection / scroll lives in the widgets below.
	agents []map[string]any

	// Plans across the partition (filtered per-agent at render time).
	plans []map[string]any

	// knowledgeDomain id -> displayName for resolving
	// capabilities.domains[]. Empty for agents whose domain ids
	// don't match any row.
	domainNames map[string]string

	Focus FocusPane

	// fetching coalesces overlapping refresh ticks.
	fetching bool

	// Plumbing.
	QueryClient func() *client.QueryClient

	// Widgets.
	agentList  ui.ListPane
	detailPane ui.DetailPane
}

// NewView creates a fresh Agents view focused on the list pane.
func NewView(theme ui.Theme) *View {
	v := &View{Focus: FocusList}
	v.Theme = theme
	v.agentList.RowsPerItem = 2
	v.agentList.Render = v.renderAgentRow
	return v
}

// StartRefreshLoop polls the underlying queries on the given
// interval until the stop channel closes. Wraps BaseView's
// context-based helper so the legacy stop-channel API stays stable
// for app.go.
func (v *View) StartRefreshLoop(stop <-chan struct{}, interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	v.BaseView.StartRefreshLoop(ctx, interval, v.Refresh)
}

// Refresh re-pulls the agent catalog, knowledge-domain lookup, and
// partition-wide plan list. Safe from any goroutine.
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

	agentRes, err := qc.QueryAllAgents(ctx, client.QueryAllAgentsArgs{})
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("agents: queryAllAgents failed: %v", err))
		}
		return
	}
	agentRows := agentRes.Rows()
	sort.SliceStable(agentRows, func(i, j int) bool {
		ni := strings.ToLower(getString(agentRows[i], "name"))
		nj := strings.ToLower(getString(agentRows[j], "name"))
		if ni == nj {
			return getString(agentRows[i], "id") < getString(agentRows[j], "id")
		}
		return ni < nj
	})

	// Knowledge domains are best-effort: an older cluster might not
	// have the knowledge DSL loaded. Failure becomes an empty lookup
	// table, not a hard fail.
	domainNames := map[string]string{}
	if domRes, err := qc.QueryListKnowledgeDomains(ctx, client.QueryListKnowledgeDomainsArgs{}); err == nil {
		for _, row := range domRes.Rows() {
			id := getString(row, "id")
			if id == "" {
				continue
			}
			name := getString(row, "name")
			if name == "" {
				name = id
			}
			domainNames[id] = name
		}
	}

	// Plans likewise best-effort.
	var planRows []map[string]any
	if planRes, err := qc.QueryAllPlans(ctx, client.QueryAllPlansArgs{}); err == nil {
		planRows = planRes.Rows()
		sort.SliceStable(planRows, func(i, j int) bool {
			return getString(planRows[i], "createdAt") > getString(planRows[j], "createdAt")
		})
	}

	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.agents = agentRows
	v.domainNames = domainNames
	v.plans = planRows
	v.agentList.Count = len(v.agents)
	if v.agentList.Selected >= len(v.agents) {
		v.agentList.Selected = 0
		v.agentList.ScrollY = 0
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

	// Two columns: list (35%) + detail (65%).
	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.35, MinSize: 28},
		{Flex: 0.65, MinSize: 32},
	})
	listBounds := panes[0]
	detailBounds := panes[1]

	// Single box-drawing divider (safe at layout edge -- EAW=Na).
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
	title := " AGENTS "
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))
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

// paneChromeBounds returns the inner Rect for a pane's scrollable
// content area: skip title + chrome row.
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

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusList {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	title := " AGENTS "
	if n := len(v.agents); n > 0 {
		title = fmt.Sprintf(" AGENTS (%d/%d) ", v.agentList.Selected+1, n)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if len(v.agents) == 0 {
		drawCentered(screen, v.Theme, bounds,
			"No agents found in this partition. Agents are created from CoPresent (CreateAgentModal) or seeded by platform automations.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForListEmpty())
		return
	}

	v.agentList.Count = len(v.agents)
	v.agentList.Focused = v.Focus == FocusList
	v.agentList.Draw(screen, paneChromeBounds(bounds), v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForList())
}

// renderAgentRow paints a 2-row agent entry. Primary line: marker
// + name + optional [SYS] tag. Subtitle: role · roleSlug · tool count.
func (v *View) renderAgentRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.agents) {
		return
	}
	a := v.agents[idx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	name := getString(a, "name")
	if name == "" {
		name = "(unnamed)"
	}
	// Active row marker is a strictly single-width ASCII glyph per
	// the layout-edge glyph rule. '*' for active, blank otherwise.
	marker := " "
	if boolFrom(a, "active") {
		marker = "*"
	}
	// [SYS] tag flags platform-infrastructure agents.
	primaryStr := fmt.Sprintf("%s %s", marker, name)
	if getString(a, "kind") == "system" {
		primaryStr = fmt.Sprintf("%s %s  [SYS]", marker, name)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, primaryStr, primary)

	role := getString(a, "role")
	roleSlug := getString(a, "roleSlug")
	var subStr string
	switch {
	case role != "" && roleSlug != "":
		subStr = fmt.Sprintf("%s · %s", role, roleSlug)
	case roleSlug != "":
		subStr = roleSlug
	case role != "":
		subStr = role
	default:
		subStr = "(no role)"
	}
	caps := mapFrom(a, "capabilities")
	if tools := stringSliceFrom(caps, "tools"); len(tools) > 0 {
		subStr = fmt.Sprintf("%s · %d tools", subStr, len(tools))
	}
	screen.DrawText(bounds.X+3, bounds.Y+1, bounds.Width-4, subStr, sub)
}

func (v *View) drawDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusDetail {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " AGENT DETAIL ", titleStyle)

	if len(v.agents) == 0 || v.agentList.Selected < 0 || v.agentList.Selected >= len(v.agents) {
		drawCentered(screen, v.Theme, bounds,
			"Select an agent from the list to see its capabilities, knowledge surface, and recent plans.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetailEmpty())
		return
	}

	a := v.agents[v.agentList.Selected]
	innerW := bounds.Width - 3
	if innerW < 16 {
		innerW = 16
	}
	v.detailPane.Lines = v.buildDetailLines(a, innerW)
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

// buildDetailLines flattens the agent record into the right pane's
// display block.
func (v *View) buildDetailLines(a map[string]any, innerWidth int) []ui.DetailLine {
	if innerWidth < 16 {
		innerWidth = 16
	}
	var lines []ui.DetailLine

	plain := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LinePlain, Text: s} }
	header := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LineHeader, Text: s} }

	addKV := func(label, value string) {
		if value == "" {
			value = "—"
		}
		line := fmt.Sprintf("  %-16s %s", label+":", value)
		if len(line) <= innerWidth {
			lines = append(lines, plain(line))
			return
		}
		wrapped := ui.WrapText(value, innerWidth-20)
		if len(wrapped) == 0 {
			lines = append(lines, plain(line))
			return
		}
		lines = append(lines, plain(fmt.Sprintf("  %-16s %s", label+":", wrapped[0])))
		for _, cont := range wrapped[1:] {
			lines = append(lines, plain(strings.Repeat(" ", 20)+cont))
		}
	}
	addSection := func(name string) {
		if len(lines) > 0 {
			lines = append(lines, plain(""))
		}
		lines = append(lines, header("─ "+name+" ─"))
	}
	addList := func(values []string, empty string) {
		if len(values) == 0 {
			lines = append(lines, plain("  "+empty))
			return
		}
		for _, val := range values {
			for _, w := range ui.WrapText("- "+val, innerWidth-2) {
				lines = append(lines, plain("  "+w))
			}
		}
	}
	addBlock := func(body string) {
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		for _, raw := range strings.Split(body, "\n") {
			if raw == "" {
				lines = append(lines, plain(""))
				continue
			}
			for _, w := range ui.WrapText(raw, innerWidth-2) {
				lines = append(lines, plain("  "+w))
			}
		}
	}

	// Identity.
	addSection("identity")
	addKV("name", getString(a, "name"))
	addKV("id", getString(a, "id"))
	kindVal := getString(a, "kind")
	if kindVal == "" {
		kindVal = "user (default)"
	}
	addKV("kind", kindVal)
	addKV("role", getString(a, "role"))
	addKV("role slug", getString(a, "roleSlug"))
	addKV("active", boolLabel(boolFrom(a, "active")))
	addKV("owner user", getString(a, "ownerUserId"))
	addKV("created", shortenTimestamp(getString(a, "createdAt")))
	if desc := getString(a, "description"); desc != "" {
		lines = append(lines, plain(""))
		addBlock(desc)
	}

	// Capabilities + tools.
	addSection("capabilities")
	caps := mapFrom(a, "capabilities")
	addKV("vision", boolLabel(boolFrom(caps, "vision")))
	addKV("voice-to-voice", boolLabel(boolFrom(caps, "voiceToVoice")))
	addKV("avatar", boolLabel(boolFrom(caps, "avatar")))
	addKV("lip-sync", boolLabel(boolFrom(caps, "lipSync")))
	addKV("claw (coding)", boolLabel(boolFrom(caps, "claw")))
	lines = append(lines, plain(""))
	lines = append(lines, plain("  tools / integrations:"))
	addList(stringSliceFrom(caps, "tools"), "(none)")
	if kw := stringSliceFrom(caps, "keywords"); len(kw) > 0 {
		lines = append(lines, plain(""))
		lines = append(lines, plain("  keywords:"))
		addList(kw, "")
	}

	// Knowledge domains.
	addSection("knowledge domains")
	domains := stringSliceFrom(caps, "domains")
	if len(domains) == 0 {
		lines = append(lines, plain("  (no domains attached)"))
	} else {
		labels := make([]string, 0, len(domains))
		for _, id := range domains {
			if name, ok := v.domainNames[id]; ok && name != "" && name != id {
				labels = append(labels, fmt.Sprintf("%s  (%s)", name, id))
			} else {
				labels = append(labels, id)
			}
		}
		addList(labels, "")
	}

	// Live knowledge.
	addSection("live knowledge")
	live := stringSliceFrom(caps, "liveKnowledge")
	if len(live) == 0 {
		live = stringSliceFrom(caps, "liveSources")
	}
	if len(live) == 0 {
		lines = append(lines, plain("  (no live knowledge sources)"))
	} else {
		addList(live, "")
	}

	// Provider config.
	addSection("provider")
	provider := mapFrom(a, "providerConfig")
	llm := mapFrom(provider, "llm")
	addKV("llm provider", stringFrom(llm, "provider"))
	addKV("llm model", stringFrom(llm, "model"))
	addKV("llm policy", stringFrom(llm, "policyName"))
	voice := mapFrom(provider, "voice")
	if vid := stringFrom(voice, "voiceId"); vid != "" {
		addKV("voice", vid)
	}

	// Recent plans owned by this agent.
	addSection(fmt.Sprintf("recent plans (top %d)", maxRecentPlans))
	agentID := getString(a, "id")
	matched := v.plansForAgent(agentID, maxRecentPlans)
	if len(matched) == 0 {
		lines = append(lines, plain("  (no plans recorded for this agent)"))
	} else {
		for _, p := range matched {
			goal := getString(p, "goal")
			if goal == "" {
				goal = getString(p, "kind")
			}
			if goal == "" {
				goal = "(no goal)"
			}
			status := getString(p, "status")
			when := shortenTimestamp(getString(p, "createdAt"))
			headerLine := fmt.Sprintf("- [%s] %s", status, when)
			for _, w := range ui.WrapText(headerLine, innerWidth-2) {
				lines = append(lines, plain("  "+w))
			}
			for _, w := range ui.WrapText(goal, innerWidth-4) {
				lines = append(lines, plain("    "+w))
			}
		}
	}

	// Raw row as a fallback when an operator wants the full picture.
	skip := map[string]bool{
		"id": true, "createdAt": true, "createdBy": true,
		"concept": true, "type": true, "schema": true,
		"name": true, "role": true, "roleSlug": true, "kind": true,
		"description": true, "ownerUserId": true, "active": true,
		"capabilities": true, "providerConfig": true,
	}
	extras := map[string]any{}
	for k, val := range a {
		if !skip[k] {
			extras[k] = val
		}
	}
	if raw := prettyJSON(extras); raw != "" && raw != "{}" {
		addSection("other fields")
		for _, rawLine := range strings.Split(raw, "\n") {
			lines = append(lines, plain("  "+rawLine))
		}
	}

	return lines
}

// plansForAgent returns up to `limit` plans whose ownerAgentId
// matches agentID. Caller must hold the read lock.
func (v *View) plansForAgent(agentID string, limit int) []map[string]any {
	if agentID == "" || len(v.plans) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, limit)
	for _, p := range v.plans {
		if getString(p, "ownerAgentId") != agentID {
			continue
		}
		out = append(out, p)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Hints
// ---------------------------------------------------------------------------

func hintsForList() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Enter", Label: "Detail"},
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
		{Key: "Esc", Label: "List"},
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

	// Tab cycles focus regardless of pane.
	if keyEv.Key() == tcell.KeyTab {
		v.Mu.Lock()
		v.Focus = (v.Focus + 1) % FocusPane(focusPaneCount)
		v.Mu.Unlock()
		return true
	}

	// R triggers a manual refresh.
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
	v.agentList.Focused = true
	prev := v.agentList.Selected
	if v.agentList.HandleEvent(ev) {
		if v.agentList.Selected != prev {
			// Reset detail scroll so a fresh agent starts at the top.
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
// Helpers
// ---------------------------------------------------------------------------

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

func mapFrom(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func stringFrom(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolFrom(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

func stringSliceFrom(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolLabel(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func shortenTimestamp(ts string) string {
	if ts == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		if len(ts) > 16 {
			return ts[:16]
		}
		return ts
	}
	return t.Format("2006-01-02 15:04")
}

func prettyJSON(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			return ""
		}
	case []any:
		if len(x) == 0 {
			return ""
		}
	case string:
		if x == "" {
			return ""
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
