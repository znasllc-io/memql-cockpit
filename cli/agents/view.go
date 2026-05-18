// Package agents renders the Agents tab: a read-only operator
// surface that lists the AI agent templates registered in the
// connected cluster's active partition. The user picks an agent
// from the left pane and sees its identity, capabilities, knowledge
// surface, and recent plan attribution in the detail pane on the
// right.
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
//   - queryAllPlans -- partition-wide plans; we filter client-side
//     to whichever ones name this agent in payload.ownerAgentId for
//     the "recent tasks" section.
//
// All three queries live in memql/dsl/{agents,knowledge,planner}/queries.memql
// and are loaded by every BFF / node binary as part of the core DSL tree.
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/component/memql"

	"github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
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
// pane. Plans are an unbounded set; rendering them all would push
// other sections off the visible viewport on busy partitions.
const maxRecentPlans = 10

// View is the Agents tab. Locking mirrors the Planner / Concepts
// views: write lock on data mutations, read lock on Draw.
type View struct {
	mu    sync.RWMutex
	Theme ui.Theme

	// Agent catalog + selection.
	agents   []map[string]any
	selected int
	scrollY  int

	// Plans across the partition (filtered per-agent at render time).
	// Refreshed on the same tick as agents so the detail pane sees
	// reasonably-fresh attribution without a per-agent query.
	plans []map[string]any

	// knowledgeDomain id -> displayName for resolving
	// capabilities.domains[]. Empty for agents whose domain ids
	// don't match any row (the renderer falls back to the raw id).
	domainNames map[string]string

	// Vertical scroll offset for the detail pane. Reset to 0 when
	// the selected agent changes.
	detailScrollY int

	Focus FocusPane

	// fetching coalesces overlapping refresh ticks (slow cluster,
	// fast tick).
	fetching bool

	// Plumbing.
	QueryClient func() *client.QueryClient

	// GatedMessage, when non-empty, replaces the layout with a
	// centered "not available" notice (cluster not connected, etc).
	GatedMessage string

	// OnStatus surfaces transient errors to the notification bar.
	OnStatus func(msg string)

	// OnRedraw is posted by the background refresher when new data
	// has landed so the event loop redraws without a key press.
	OnRedraw func()
}

// NewView creates a fresh Agents view focused on the list pane.
func NewView(theme ui.Theme) *View {
	return &View{
		Theme: theme,
		Focus: FocusList,
	}
}

// StartRefreshLoop polls the underlying queries on the given interval
// until the stop channel closes. Safe to call once at app-init after
// the cluster wiring is in place. Mirrors planner.View.StartRefreshLoop.
func (v *View) StartRefreshLoop(stop <-chan struct{}, interval time.Duration) {
	go func() {
		// Immediate refresh on launch so the first paint after the
		// user opens the tab isn't blank.
		v.Refresh()
		if v.OnRedraw != nil {
			v.OnRedraw()
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				v.Refresh()
				if v.OnRedraw != nil {
					v.OnRedraw()
				}
			}
		}
	}()
}

// Refresh re-pulls the agent catalog, knowledge-domain lookup, and
// partition-wide plan list. Safe from any goroutine. Soft-fails per
// query: a missing knowledge tree on the cluster shouldn't prevent
// the agent list from rendering.
func (v *View) Refresh() {
	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}

	v.mu.Lock()
	if v.fetching {
		v.mu.Unlock()
		return
	}
	v.fetching = true
	v.mu.Unlock()
	defer func() {
		v.mu.Lock()
		v.fetching = false
		v.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	agentRes, err := qc.Execute(ctx, "queryAllAgents({})")
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("agents: queryAllAgents failed: %v", err))
		}
		return
	}
	agentRows := memql.MaterializeRows(agentRes)
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
	// table, not a hard fail. queryListKnowledgeDomains projects
	// through shape knowledgeDomainFull, which flattens fields to the
	// row top level -- read row.name, NOT row.payload.name.
	domainNames := map[string]string{}
	if domRes, err := qc.Execute(ctx, "queryListKnowledgeDomains({})"); err == nil {
		for _, row := range memql.MaterializeRows(domRes) {
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

	// Plans likewise best-effort: filtering client-side by
	// ownerAgentId means a missing planner DSL just blanks the
	// "recent tasks" section.
	var planRows []map[string]any
	if planRes, err := qc.Execute(ctx, "queryAllPlans({})"); err == nil {
		planRows = memql.MaterializeRows(planRes)
		sort.SliceStable(planRows, func(i, j int) bool {
			return getString(planRows[i], "createdAt") > getString(planRows[j], "createdAt")
		})
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.agents = agentRows
	v.domainNames = domainNames
	v.plans = planRows
	if v.selected >= len(v.agents) {
		v.selected = 0
		v.scrollY = 0
		v.detailScrollY = 0
	}
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// Draw paints the tab. Holds the read lock so concurrent refreshes
// can't tear the underlying slices mid-paint.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.GatedMessage != "" {
		v.drawGated(screen, bounds, v.GatedMessage)
		return
	}

	// Two columns: list (35%) + detail (65%). The list pane wants
	// enough width for a name + role tag on one line at typical
	// terminal widths; the detail pane needs the rest for the
	// section blocks (capabilities, knowledge, plans).
	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.35, MinSize: 28},
		{Flex: 0.65, MinSize: 32},
	})
	listBounds := panes[0]
	detailBounds := panes[1]

	// Vertical divider between the two columns. Single box-drawing
	// char per row keeps the layout-edge glyph rule (cli/CLAUDE.md
	// "Layout-edge glyph rule") safe -- '│' is EAW=Na.
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

func (v *View) drawList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusList {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	title := " AGENTS "
	if n := len(v.agents); n > 0 {
		title = fmt.Sprintf(" AGENTS (%d/%d) ", v.selected+1, n)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if len(v.agents) == 0 {
		drawCentered(screen, v.Theme, bounds,
			"No agents found in this partition. Agents are created from CoPresent (CreateAgentModal) or seeded by platform automations.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, "R:Refresh  Tab:Cycle")
		return
	}

	const chromeH = 1
	listTop := bounds.Y + 2
	listH := bounds.Height - 2 - chromeH
	if listH < 1 {
		listH = 1
	}

	// Two rows per agent: primary (name + active/deleted marker) +
	// subtitle (role/roleSlug + tool count). Same shape the planner
	// list uses for plans.
	rowsPerAgent := 2
	maxVisible := listH / rowsPerAgent
	if maxVisible < 1 {
		maxVisible = 1
	}
	v.clampListScroll(maxVisible)

	for i := 0; i < maxVisible && v.scrollY+i < len(v.agents); i++ {
		idx := v.scrollY + i
		a := v.agents[idx]
		y := listTop + i*rowsPerAgent

		style := v.Theme.BaseStyle()
		if idx == v.selected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)
		screen.FillRect(bounds.X, y+1, bounds.Width, 1, style)

		name := getString(a, "name")
		if name == "" {
			name = "(unnamed)"
		}
		// Active row marker is a strictly single-width ASCII glyph
		// per the layout-edge glyph rule. '*' for active selected,
		// blank otherwise; deleted agents (queryAllAgents already
		// filters them out) would render with '-' for distinction
		// but we shouldn't see any.
		marker := " "
		if boolFrom(a, "active") {
			marker = "*"
		}
		// [SYS] tag flags platform-infrastructure agents (kind=="system",
		// e.g. MemQL Planner / MemQL Trainer). Their seeds set autoJoin
		// false and they never join cognition spaces, but the cockpit
		// surfaces them here so operators can confirm they exist + see
		// their config. The tag keeps them visually separable from
		// user-level agents (kind=="user" or empty -- the latter is
		// the legacy default for rows pre-dating the field).
		primary := fmt.Sprintf("%s %s", marker, name)
		if getString(a, "kind") == "system" {
			primary = fmt.Sprintf("%s %s  [SYS]", marker, name)
		}
		screen.DrawText(bounds.X+1, y, bounds.Width-2, primary, style)

		role := getString(a, "role")
		roleSlug := getString(a, "roleSlug")
		var sub string
		switch {
		case role != "" && roleSlug != "":
			sub = fmt.Sprintf("%s · %s", role, roleSlug)
		case roleSlug != "":
			sub = roleSlug
		case role != "":
			sub = role
		default:
			sub = "(no role)"
		}
		caps := mapFrom(a, "capabilities")
		if tools := stringSliceFrom(caps, "tools"); len(tools) > 0 {
			sub = fmt.Sprintf("%s · %d tools", sub, len(tools))
		}
		screen.DrawText(bounds.X+3, y+1, bounds.Width-4, sub, dimify(style, v.Theme.Subtle))
	}

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1,
		"↑/↓:Move  Enter:Detail  R:Refresh  Tab:Cycle")
}

func (v *View) drawDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusDetail {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " AGENT DETAIL ", titleStyle)

	if len(v.agents) == 0 || v.selected < 0 || v.selected >= len(v.agents) {
		drawCentered(screen, v.Theme, bounds,
			"Select an agent from the list to see its capabilities, knowledge surface, and recent plans.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, "Tab:Cycle")
		return
	}

	a := v.agents[v.selected]
	lines := v.buildDetailLines(a, bounds.Width-3)

	const chromeH = 1
	bodyTop := bounds.Y + 2
	bodyH := bounds.Height - 2 - chromeH
	if bodyH < 1 {
		bodyH = 1
	}

	maxScroll := len(lines) - bodyH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if v.detailScrollY > maxScroll {
		v.detailScrollY = maxScroll
	}
	if v.detailScrollY < 0 {
		v.detailScrollY = 0
	}

	for i := 0; i < bodyH && v.detailScrollY+i < len(lines); i++ {
		line := lines[v.detailScrollY+i]
		style := v.Theme.BaseStyle()
		if strings.HasPrefix(line, "─") {
			style = v.Theme.SubtleStyle().Bold(true)
		}
		screen.DrawText(bounds.X+2, bodyTop+i, bounds.Width-3, line, style)
	}

	hint := "↑/↓:Scroll  Esc:List  Tab:Cycle"
	if maxScroll == 0 {
		hint = "Esc:List  Tab:Cycle"
	}
	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hint)
}

// buildDetailLines flattens the agent record into the right pane's
// display block: identity, role, capabilities, provider, knowledge
// (domains + live), and recent plan attribution. Pure function for
// easy unit-testing.
func (v *View) buildDetailLines(a map[string]any, innerWidth int) []string {
	if innerWidth < 16 {
		innerWidth = 16
	}
	var lines []string

	addKV := func(label, value string) {
		if value == "" {
			value = "—"
		}
		line := fmt.Sprintf("  %-16s %s", label+":", value)
		if len(line) <= innerWidth {
			lines = append(lines, line)
			return
		}
		wrapped := ui.WrapText(value, innerWidth-20)
		if len(wrapped) == 0 {
			lines = append(lines, line)
			return
		}
		lines = append(lines, fmt.Sprintf("  %-16s %s", label+":", wrapped[0]))
		for _, cont := range wrapped[1:] {
			lines = append(lines, strings.Repeat(" ", 20)+cont)
		}
	}
	addSection := func(name string) {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "─ "+name+" ─")
	}
	addList := func(values []string, empty string) {
		if len(values) == 0 {
			lines = append(lines, "  "+empty)
			return
		}
		for _, val := range values {
			for _, w := range ui.WrapText("- "+val, innerWidth-2) {
				lines = append(lines, "  "+w)
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
				lines = append(lines, "")
				continue
			}
			for _, w := range ui.WrapText(raw, innerWidth-2) {
				lines = append(lines, "  "+w)
			}
		}
	}

	// Identity. Shape-wrapped queries (queryAllAgents → agentFull)
	// flatten the projected fields to row top level -- read row.name,
	// NOT row.payload.name. Intrinsics (id, createdAt, createdBy)
	// likewise sit at the top level via the row.* aliases in the
	// shape file.
	addSection("identity")
	addKV("name", getString(a, "name"))
	addKV("id", getString(a, "id"))
	// kind = "system" | "user". Surfaced near the top of identity so
	// it's the first signal an operator reads about an agent. Empty
	// for legacy rows pre-dating the field; render those as user since
	// the schema default is "user".
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
		lines = append(lines, "")
		addBlock(desc)
	}

	// Capabilities + tools. capabilities.tools[] is the SI-callable
	// surface the agent has access to; capabilities.{vision, claw,
	// voiceToVoice, ...} are coarser modality flags. Both are useful
	// at a glance.
	addSection("capabilities")
	caps := mapFrom(a, "capabilities")
	addKV("vision", boolLabel(boolFrom(caps, "vision")))
	addKV("voice-to-voice", boolLabel(boolFrom(caps, "voiceToVoice")))
	addKV("avatar", boolLabel(boolFrom(caps, "avatar")))
	addKV("lip-sync", boolLabel(boolFrom(caps, "lipSync")))
	addKV("claw (coding)", boolLabel(boolFrom(caps, "claw")))
	lines = append(lines, "")
	lines = append(lines, "  tools / integrations:")
	addList(stringSliceFrom(caps, "tools"), "(none)")
	if kw := stringSliceFrom(caps, "keywords"); len(kw) > 0 {
		lines = append(lines, "")
		lines = append(lines, "  keywords:")
		addList(kw, "")
	}

	// Knowledge surface. capabilities.domains[] is a list of
	// knowledgeDomain ids that the cognition scoring engine uses for
	// relevance matching. We resolve them against the
	// queryListKnowledgeDomains lookup so the user reads a name
	// rather than a raw id; if a domain is missing from the lookup
	// (cluster without knowledge DSL, or a stale row), we fall back
	// to the raw id verbatim.
	addSection("knowledge domains")
	domains := stringSliceFrom(caps, "domains")
	if len(domains) == 0 {
		lines = append(lines, "  (no domains attached)")
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

	// "Live knowledge" today is layered onto the agent through its
	// role (agentRole.lockedLiveKnowledgeIds). We don't have that
	// row in hand here, but we can surface anything stamped onto the
	// agent itself under capabilities.liveKnowledge / liveSources
	// when present so the section is at least non-empty when the
	// data exists. Empty otherwise.
	addSection("live knowledge")
	live := stringSliceFrom(caps, "liveKnowledge")
	if len(live) == 0 {
		live = stringSliceFrom(caps, "liveSources")
	}
	if len(live) == 0 {
		lines = append(lines, "  (no live knowledge sources)")
	} else {
		addList(live, "")
	}

	// Provider config: the actual LLM the agent runs on. The router
	// resolves provider > model > policyName in that order.
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

	// Recent plans owned by this agent -- the closest stand-in for
	// "tasks the agent has worked on" we can render without a
	// per-agent server query. Plans carry status + goal + createdAt,
	// which is enough for the user to recognize what the agent has
	// been busy with.
	addSection(fmt.Sprintf("recent plans (top %d)", maxRecentPlans))
	agentID := getString(a, "id")
	matched := v.plansForAgent(agentID, maxRecentPlans)
	if len(matched) == 0 {
		lines = append(lines, "  (no plans recorded for this agent)")
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
			header := fmt.Sprintf("- [%s] %s", status, when)
			for _, w := range ui.WrapText(header, innerWidth-2) {
				lines = append(lines, "  "+w)
			}
			for _, w := range ui.WrapText(goal, innerWidth-4) {
				lines = append(lines, "    "+w)
			}
		}
	}

	// Raw row as a fallback when an operator wants the full picture.
	// agentFull projects fields flat at the row top level, so the
	// raw dump IS the row map itself (skipping intrinsics already
	// shown in the identity section keeps the block focused).
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
		for _, raw := range strings.Split(raw, "\n") {
			lines = append(lines, "  "+raw)
		}
	}

	return lines
}

// plansForAgent returns up to `limit` plans whose ownerAgentId
// matches agentID, in createdAt-descending order (the underlying
// v.plans slice is already sorted that way). planFull is a
// flat-projecting shape, so ownerAgentId is at row.ownerAgentId.
// Caller must hold the read lock.
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
// Input handling
// ---------------------------------------------------------------------------

// HandleEvent processes a key event. Returns true if consumed.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// Tab cycles focus regardless of which pane is active.
	if keyEv.Key() == tcell.KeyTab {
		v.mu.Lock()
		v.Focus = (v.Focus + 1) % FocusPane(focusPaneCount)
		v.mu.Unlock()
		return true
	}

	// R triggers a manual refresh from any focus. Useful when the
	// background tick is too slow for an operator who just edited
	// the agent in CoPresent.
	if keyEv.Key() == tcell.KeyRune && (keyEv.Rune() == 'r' || keyEv.Rune() == 'R') {
		go func() {
			v.Refresh()
			if v.OnRedraw != nil {
				v.OnRedraw()
			}
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
	switch ev.Key() {
	case tcell.KeyUp:
		v.mu.Lock()
		if v.selected > 0 {
			v.selected--
			v.detailScrollY = 0
		}
		v.mu.Unlock()
		return true
	case tcell.KeyDown:
		v.mu.Lock()
		if v.selected < len(v.agents)-1 {
			v.selected++
			v.detailScrollY = 0
		}
		v.mu.Unlock()
		return true
	case tcell.KeyEnter:
		v.mu.Lock()
		v.Focus = FocusDetail
		v.mu.Unlock()
		return true
	}
	return false
}

func (v *View) handleDetailKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		v.mu.Lock()
		if v.detailScrollY > 0 {
			v.detailScrollY--
		}
		v.mu.Unlock()
		return true
	case tcell.KeyDown:
		v.mu.Lock()
		v.detailScrollY++
		v.mu.Unlock()
		return true
	case tcell.KeyPgUp:
		v.mu.Lock()
		v.detailScrollY -= 10
		if v.detailScrollY < 0 {
			v.detailScrollY = 0
		}
		v.mu.Unlock()
		return true
	case tcell.KeyPgDn:
		v.mu.Lock()
		v.detailScrollY += 10
		v.mu.Unlock()
		return true
	case tcell.KeyHome:
		v.mu.Lock()
		v.detailScrollY = 0
		v.mu.Unlock()
		return true
	case tcell.KeyEsc:
		v.mu.Lock()
		v.Focus = FocusList
		v.mu.Unlock()
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (v *View) clampListScroll(maxVisible int) {
	if maxVisible < 1 {
		maxVisible = 1
	}
	if v.selected < v.scrollY {
		v.scrollY = v.selected
	}
	if v.selected >= v.scrollY+maxVisible {
		v.scrollY = v.selected - maxVisible + 1
	}
	if v.scrollY < 0 {
		v.scrollY = 0
	}
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

func dimify(style tcell.Style, dim tcell.Color) tcell.Style {
	return style.Foreground(dim)
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
