// Package planner renders the Planner tab: a read-only operator
// surface for observing v1:planner:plan + v1:planner:task rows in the
// connected cluster.
//
// Goal submission lives in the Chat tab now -- the user talks to the
// assistant, which decides whether to escalate to the planner via its
// tools. This tab is for watching what the planner is doing, not for
// driving it. The one mutation that remains is mutationStartPlan:
// when a plan sits in status="queued" awaiting user confirmation,
// pressing R on the Plans pane flips it to running.
//
// Data plane: queryAllPlans + queryTasksForPlan to read (live in
// memql-bff-copresent/dsl/copresent/queries.memql), mutationStartPlan
// to advance a queued plan. The Planner Agent
// (memql/integrations/planner/agent_loop.go) drives the actual
// decomposition + dispatch from the event bus on planner-tagged nodes.
//
// Migrated to the cli/ui widget layer (epic #81): the view embeds
// ui.BaseView, the plan + task lists are ui.ListPane instances with
// RowsPerItem=2, and the task-detail pane is ui.DetailPane.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// FocusPane identifies which of the three keyboard-focus regions is
// active. Tab cycles through them in order.
type FocusPane int

const (
	FocusPlans      FocusPane = 0 // the plan list (left column)
	FocusTasks      FocusPane = 1 // the task list under the selected plan
	FocusTaskDetail FocusPane = 2 // input/output/metadata of the selected task
)

// focusPaneCount caps the modulo used by Tab cycling; bump when
// adding a new focus region.
const focusPaneCount = 3

// View is the Planner tab. Mutated from both the event-loop goroutine
// (HandleEvent) and a background refresher (refreshLoop). Locking
// mirrors the Concepts tab pattern: write lock on mutators, read lock
// on Draw.
type View struct {
	ui.BaseView // Mu / Theme / GatedMessage / OnStatus / OnRedraw

	// Plans + the currently-loaded tasks for the highlighted plan.
	// Selection / scroll state lives in the widgets below.
	plans []map[string]any
	tasks []map[string]any

	// Focus + transient flags.
	Focus    FocusPane
	fetching bool

	// dslMissing latches when the cluster's BFF returns
	// "function not found" for a Planner query. Once set, the
	// refresher stops issuing the failing call -- no point making
	// a fresh RTT every 3s -- and Draw renders an explanatory
	// gated screen instead of the empty layout. Cleared by
	// pressing R (manual refresh) so a re-deploy of the BFF that
	// loads the Planner DSL picks back up without restarting
	// the cockpit. See isPlannerDSLMissing below.
	dslMissing bool

	// Plumbing.
	QueryClient func() *client.QueryClient

	// Widgets (composable from cli/ui/). RowsPerItem=2 on both lists
	// so each plan / task row is a primary + dimmed-subtitle pair.
	planList       ui.ListPane
	taskList       ui.ListPane
	taskDetailPane ui.DetailPane
}

// NewView creates a fresh Planner view focused on the plan list.
func NewView(theme ui.Theme) *View {
	v := &View{Focus: FocusPlans}
	v.Theme = theme
	v.planList.RowsPerItem = 2
	v.planList.Render = v.renderPlanRow
	v.taskList.RowsPerItem = 2
	v.taskList.Render = v.renderTaskRow
	return v
}

// StartRefreshLoop runs a background ticker that re-pulls plans (and
// the selected plan's tasks) every interval. The caller passes a stop
// channel; closing it cancels the loop. Wraps BaseView's context-based
// helper so the legacy stop-channel API stays stable for app.go.
func (v *View) StartRefreshLoop(stop <-chan struct{}, interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	v.BaseView.StartRefreshLoop(ctx, interval, func() {
		v.RefreshPlans()
		v.RefreshTasksForSelected()
	})
}

// RefreshPlans re-pulls the full plan list. Safe from any goroutine.
func (v *View) RefreshPlans() {
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
		// Cluster's BFF doesn't have the Planner DSL loaded. Don't
		// burn an RTT every refresh tick re-confirming. The user can
		// press R to retry (handlePlanListKey clears the latch).
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

	res, err := qc.QueryAllPlans(ctx, client.QueryAllPlansArgs{})
	if err != nil {
		if isPlannerDSLMissing(err) {
			v.markDSLMissing()
			return
		}
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("planner: queryAllPlans failed: %v", err))
		}
		return
	}

	rows := res.Rows()
	sort.Slice(rows, func(i, j int) bool {
		return getString(rows[i], "createdAt") > getString(rows[j], "createdAt")
	})

	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.plans = rows
	v.planList.Count = len(v.plans)
	if v.planList.Selected >= len(v.plans) {
		v.planList.Selected = 0
		v.planList.ScrollY = 0
	}
}

// RefreshTasksForSelected re-pulls tasks for whichever plan is
// currently highlighted. No-op if no plan is selected.
func (v *View) RefreshTasksForSelected() {
	v.Mu.RLock()
	planID := ""
	if v.planList.Selected >= 0 && v.planList.Selected < len(v.plans) {
		planID = getString(v.plans[v.planList.Selected], "id")
	}
	v.Mu.RUnlock()
	if planID == "" {
		v.Mu.Lock()
		v.tasks = nil
		v.taskList.Count = 0
		v.taskList.Selected = 0
		v.taskList.ScrollY = 0
		v.Mu.Unlock()
		return
	}

	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}

	v.Mu.RLock()
	missing := v.dslMissing
	v.Mu.RUnlock()
	if missing {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	res, err := qc.QueryTasksForPlan(ctx, client.QueryTasksForPlanArgs{PlanId: planID})
	if err != nil {
		if isPlannerDSLMissing(err) {
			v.markDSLMissing()
			return
		}
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("planner: queryTasksForPlan failed: %v", err))
		}
		return
	}

	rows := res.Rows()
	sort.SliceStable(rows, func(i, j int) bool {
		return getInt(rows[i], "seq") < getInt(rows[j], "seq")
	})

	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.tasks = rows
	v.taskList.Count = len(v.tasks)
	if v.taskList.Selected >= len(v.tasks) {
		v.taskList.Selected = 0
		v.taskList.ScrollY = 0
	}
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// Draw renders the Planner tab. Holds the read lock so concurrent
// background refreshes can't tear the slices mid-paint.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.Mu.RLock()
	defer v.Mu.RUnlock()

	if v.GatedMessage != "" {
		v.drawGated(screen, bounds, v.GatedMessage)
		return
	}
	// DSL-missing precedence: the outer "cluster not connected" path
	// (GatedMessage) is fixed first, so it wins above. Only when the
	// cluster IS connected and the BFF rejected our query as
	// "function not found" do we surface this gating. R:Refresh
	// clears the latch (see handlePlanListKey).
	if v.dslMissing {
		v.drawGated(screen, bounds,
			"Planner DSL not loaded on this cluster. The Planner tab "+
				"requires queryAllPlans / queryTasksForPlan to be "+
				"registered with the BFF engine (the copresent DSL "+
				"tree provides them). Press R to retry once the BFF "+
				"has been redeployed.")
		return
	}

	// Three panes mirroring the Concepts tab's progressive-disclosure
	// flow (picker -> row list -> detail):
	//
	//   PLANS        (left, flex -- recent plans)
	//   TASKS        (center, flex -- tasks under the highlighted plan)
	//   TASK DETAIL  (right, flex -- input / output / metadata for
	//                the highlighted task)
	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.28, MinSize: 32},
		{Flex: 0.36, MinSize: 28},
		{Flex: 0.36, MinSize: 28},
	})
	plansBounds := panes[0]
	tasksBounds := panes[1]
	detailBounds := panes[2]

	// Single box-drawing dividers (safe at the layout edge -- East
	// Asian Width Narrow).
	divStyle := v.Theme.SubtleStyle()
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		screen.SetCell(plansBounds.X+plansBounds.Width-1, y, '│', divStyle)
		screen.SetCell(tasksBounds.X+tasksBounds.Width-1, y, '│', divStyle)
	}
	plansBounds.Width--
	tasksBounds.Width--

	v.drawPlanList(screen, plansBounds)
	v.drawTaskList(screen, tasksBounds)
	v.drawTaskDetail(screen, detailBounds)
}

// drawGated renders the centered "tab unavailable" layout with the
// supplied message. Used for two distinct cases: the outer
// GatedMessage (cluster not connected) and the planner-specific
// dslMissing latch.
func (v *View) drawGated(screen *ui.Screen, bounds ui.Rect, msg string) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	title := " PLANNER "
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
// content area: skip the title row at the top and the chrome row
// at the bottom.
func paneChromeBounds(bounds ui.Rect) ui.Rect {
	const chromeH = 1
	listTop := bounds.Y + 2
	listH := bounds.Height - 2 - chromeH
	if listH < 1 {
		listH = 1
	}
	return ui.Rect{X: bounds.X, Y: listTop, Width: bounds.Width, Height: listH}
}

func (v *View) drawPlanList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusPlans {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	// Counts ride the title bar per the panel chrome contract --
	// the bottom row is reserved for action hints, never a duplicate
	// `n/m` strip.
	title := " PLANS "
	if n := len(v.plans); n > 0 {
		title = fmt.Sprintf(" PLANS (%d/%d) ", v.planList.Selected+1, n)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if len(v.plans) == 0 {
		drawCentered(screen, v.Theme, bounds, "No plans yet -- submit a goal above.")
	} else {
		v.planList.Count = len(v.plans)
		v.planList.Focused = v.Focus == FocusPlans
		v.planList.Draw(screen, paneChromeBounds(bounds), v.Theme)
	}

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForPlans(v))
}

func (v *View) drawTaskList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusTasks {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	// Position count rides the title bar per the chrome contract.
	// Plan-level token rollup tags onto the title strip so the user
	// sees the running total without leaving the Tasks pane.
	title := " TASKS "
	if n := len(v.tasks); n > 0 {
		title = fmt.Sprintf(" TASKS (%d/%d) ", v.taskList.Selected+1, n)
	}
	if v.planList.Selected >= 0 && v.planList.Selected < len(v.plans) {
		if tok := planTokenSummary(v.plans[v.planList.Selected]); tok != "" {
			title = strings.TrimRight(title, " ") + "  ·  " + tok + " "
		}
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if len(v.tasks) == 0 {
		msg := "No tasks for this plan."
		if v.planList.Selected >= 0 && v.planList.Selected < len(v.plans) {
			switch getString(v.plans[v.planList.Selected], "status") {
			case "planning":
				msg = "Planning in progress -- the planner agent is decomposing the goal and emitting tasks. This usually takes 10-30 seconds. Tasks appear here as they are committed; the plan transitions to 'queued' once planning is done."
			case "needsAgent":
				msg = "Plan is parked: no good-fit agent in this space. Create or extend an agent that matches the goal, then resume the plan."
			case "awaitingFeedback":
				msg = "Plan is awaiting your feedback. Respond on the plan card to resume."
			case "succeeded":
				msg = "Plan completed with no tasks emitted."
			case "failed":
				if errMsg := getString(v.plans[v.planList.Selected], "errorMessage"); errMsg != "" {
					msg = "Plan failed: " + truncate(errMsg, 240)
				} else {
					msg = "Plan failed."
				}
			case "cancelled":
				msg = "Plan was cancelled."
			}
		}
		drawCentered(screen, v.Theme, bounds, msg)
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForTasksEmpty())
		return
	}

	v.taskList.Count = len(v.tasks)
	v.taskList.Focused = v.Focus == FocusTasks
	v.taskList.Draw(screen, paneChromeBounds(bounds), v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForTasks())
}

// renderPlanRow paints a single 2-row plan entry. Primary line is
// the goal; subtitle is status + when + elapsed timer + token rollup.
// Layout mirrors the pre-migration drawPlanList loop exactly.
func (v *View) renderPlanRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.plans) {
		return
	}
	p := v.plans[idx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	goal := getString(p, "goal")
	if goal == "" {
		goal = "(no goal)"
	}
	screen.DrawText(bounds.X+2, bounds.Y, bounds.Width-3, goal, primary)

	status := getString(p, "status")
	when := shortenTimestamp(getString(p, "createdAt"))
	subStr := fmt.Sprintf("%s  %s", statusLabel(status), when)
	if elapsed := activeStatusElapsed(status, getString(p, "createdAt")); elapsed != "" {
		subStr = fmt.Sprintf("%s  %s  +%s", statusLabel(status), when, elapsed)
	}
	if tok := planTokenSummary(p); tok != "" {
		subStr += "  ·  " + tok
	}
	screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-3, subStr, sub)
}

// renderTaskRow paints a single 2-row task entry. Primary line is
// `[seq] <label>` where the label prefers logicalStepId then kind.
// Subtitle is status + phase + elapsed + token rollup.
func (v *View) renderTaskRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.tasks) {
		return
	}
	t := v.tasks[idx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	// Primary line: "[seq] <label>". Label prefers the
	// logicalStepId (the planner-assigned stable id like
	// "simulate-superman-vs-spiderman") because the `kind` field
	// is often the generic "chat" and isn't useful for telling
	// tasks apart.
	label := getString(t, "logicalStepId")
	if label == "" {
		label = getString(t, "kind")
	}
	if label == "" {
		label = "(unnamed)"
	}
	primaryStr := fmt.Sprintf("[%d] %s", getInt(t, "seq"), label)
	screen.DrawText(bounds.X+2, bounds.Y, bounds.Width-3, primaryStr, primary)

	// Subtitle: status + phase + active-status timer + tokens.
	status := getString(t, "status")
	subStr := statusLabel(status)
	if phase := getString(t, "phase"); phase != "" {
		subStr += "  phase:" + phase
	}
	if elapsed := activeStatusElapsed(status, getString(t, "createdAt")); elapsed != "" {
		subStr += "  +" + elapsed
	}
	if tok := taskTokensSpent(t); tok > 0 {
		subStr += "  ·  " + formatTokens(tok) + " tok"
	}
	screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-3, subStr, sub)
}

// drawTaskDetail renders the third pane: rich per-task detail for
// whichever task is highlighted in the center Tasks pane.
func (v *View) drawTaskDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusTaskDetail {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	title := " TASK DETAIL "
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	// Empty-state precedence: no plan selected -> no task -> nothing
	// to show.
	if len(v.plans) == 0 || v.planList.Selected < 0 || v.planList.Selected >= len(v.plans) {
		drawCentered(screen, v.Theme, bounds, "Select a plan to see its tasks.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetailEmpty())
		return
	}
	if len(v.tasks) == 0 || v.taskList.Selected < 0 || v.taskList.Selected >= len(v.tasks) {
		drawCentered(screen, v.Theme, bounds, "No task selected. Pick one in the Tasks pane to see its input, output, and metadata here.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetailEmpty())
		return
	}

	task := v.tasks[v.taskList.Selected]
	plan := v.plans[v.planList.Selected]

	// Build the rendered block as a []DetailLine, then hand to
	// DetailPane which owns wrap + scroll + scrollbar. The pre-
	// migration view rolled its own clamp + paint loop here.
	innerW := bounds.Width - 3
	if innerW < 16 {
		innerW = 16
	}
	v.taskDetailPane.Lines = v.buildTaskDetailLines(task, plan, innerW)
	v.taskDetailPane.Focused = v.Focus == FocusTaskDetail

	inner := paneChromeBounds(bounds)
	inner.X += 1
	inner.Width -= 1
	if inner.Width < 1 {
		inner.Width = 1
	}
	v.taskDetailPane.Draw(screen, inner, v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetail())
}

// buildTaskDetailLines flattens the task + plan context into a list
// of DetailLine entries for DetailPane. Pure function so it's easy
// to unit-test against fixture rows. innerWidth bounds each
// JSON/string field's wrap so the detail pane never overflows.
//
// Section markers ("─ name ─") use LineSection (subtle + bold) so
// they recede into the chrome instead of popping in the accent
// color -- every row sits under a section marker, so accent-bold
// would make the whole pane look busy. Body content uses LinePlain.
func (v *View) buildTaskDetailLines(task, plan map[string]any, innerWidth int) []ui.DetailLine {
	if innerWidth < 16 {
		innerWidth = 16
	}
	var lines []ui.DetailLine

	plain := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LinePlain, Text: s} }
	header := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LineSection, Text: s} }

	addKV := func(label, value string) {
		if value == "" {
			value = "—"
		}
		line := fmt.Sprintf("  %-13s %s", label+":", value)
		if len(line) <= innerWidth {
			lines = append(lines, plain(line))
			return
		}
		wrapped := ui.WrapText(value, innerWidth-17)
		lines = append(lines, plain(fmt.Sprintf("  %-13s %s", label+":", wrapped[0])))
		for _, cont := range wrapped[1:] {
			lines = append(lines, plain(strings.Repeat(" ", 17)+cont))
		}
	}
	addSection := func(name string) {
		if len(lines) > 0 {
			lines = append(lines, plain(""))
		}
		lines = append(lines, header("─ "+name+" ─"))
	}
	addBlock := func(name string, body string) {
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		addSection(name)
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

	// Identity section. Pulls the agent name from the parent plan
	// since the task concept doesn't carry agentId itself.
	addSection("identity")
	addKV("task id", getString(task, "id"))
	addKV("seq", strconv.Itoa(getInt(task, "seq")))
	addKV("kind", getString(task, "kind"))
	addKV("category", getString(task, "category"))
	addKV("phase", getString(task, "phase"))
	addKV("logical id", getString(task, "logicalStepId"))
	addKV("attempts", strconv.Itoa(getInt(task, "attemptNumber")))
	addKV("status", getString(task, "status"))
	addKV("agent (plan.ownerAgentId)", getString(plan, "ownerAgentId"))
	addKV("execution", getString(task, "executionSurface"))
	if backend := getString(task, "executorBackend"); backend != "" {
		addKV("backend", backend)
	}
	if toolName := getString(task, "toolName"); toolName != "" {
		addKV("tool name", toolName)
	}

	addSection("timing")
	addKV("created", shortenTimestamp(getString(task, "createdAt")))
	if started := getString(task, "startedAt"); started != "" {
		addKV("started", shortenTimestamp(started))
	}
	if completed := getString(task, "completedAt"); completed != "" {
		addKV("completed", shortenTimestamp(completed))
	}
	if parked := getString(task, "parkedAt"); parked != "" {
		addKV("parked", shortenTimestamp(parked))
		if mark := getString(task, "parkedAtCheckpoint"); mark != "" {
			addKV("checkpoint", mark)
		}
	}
	if elapsed := activeStatusElapsed(getString(task, "status"), getString(task, "createdAt")); elapsed != "" {
		addKV("elapsed", elapsed)
	}

	// Tokens section: per-task + parent plan totals + optional
	// model breakdown.
	metrics, _ := task["metrics"].(map[string]any)
	addSection("tokens")
	taskSpent := taskTokensSpent(task)
	addKV("task spent", formatTokens(taskSpent)+" tok")
	planSpent := getInt(plan, "tokenSpent")
	planBudget := getInt(plan, "tokenBudget")
	if planBudget > 0 {
		addKV("plan spent", fmt.Sprintf("%s / %s tok", formatTokens(planSpent), formatTokens(planBudget)))
	} else {
		addKV("plan spent", formatTokens(planSpent)+" tok")
	}
	if metrics != nil {
		if llm := getInt(metrics, "llmCallCount"); llm > 0 {
			addKV("LLM calls", strconv.Itoa(llm))
		}
		if tools := getInt(metrics, "toolCallCount"); tools > 0 {
			addKV("tool calls", strconv.Itoa(tools))
		}
		if breakdown, ok := metrics["modelBreakdown"].([]any); ok && len(breakdown) > 0 {
			lines = append(lines, plain(""))
			lines = append(lines, plain("  by model:"))
			for _, item := range breakdown {
				row, _ := item.(map[string]any)
				if row == nil {
					continue
				}
				tag := getString(row, "tag")
				if tag == "" {
					tag = "(unknown)"
				}
				lines = append(lines, plain(fmt.Sprintf("    - %s: %s tok", tag, formatTokens(getInt(row, "tokens")))))
			}
		}
	}

	addBlock("input", prettyJSON(task["input"]))
	addBlock("output", prettyJSON(task["output"]))
	addBlock("tool args", prettyJSON(task["toolArgs"]))
	addBlock("tool result", prettyJSON(task["toolResult"]))
	if errMsg := getString(task, "errorMessage"); errMsg != "" {
		addBlock("error", errMsg)
	}
	addBlock("metrics", prettyJSON(task["metrics"]))

	return lines
}

// prettyJSON serializes v with two-space indentation.
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

// ---------------------------------------------------------------------------
// Hints
// ---------------------------------------------------------------------------

func hintsForPlans(v *View) string {
	if len(v.plans) == 0 {
		bar := ui.HintBar{Chips: []ui.HintChip{{Key: "Tab", Label: "Cycle"}}}
		return bar.String()
	}
	canRun := v.planList.Selected >= 0 && v.planList.Selected < len(v.plans) &&
		getString(v.plans[v.planList.Selected], "status") == "queued"
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Enter", Label: "Tasks"},
		{Key: "R", Label: "Run", Disabled: !canRun},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

func hintsForTasks() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Enter", Label: "Detail"},
		{Key: "Esc", Label: "Plans"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

func hintsForTasksEmpty() string {
	bar := ui.HintBar{Chips: []ui.HintChip{{Key: "Tab", Label: "Cycle"}}}
	return bar.String()
}

func hintsForDetail() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Scroll"},
		{Key: "PgUp/PgDn", Label: "Page"},
		{Key: "Esc", Label: "Tasks"},
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

	// R means Run: when a plan in status="queued" is highlighted in
	// the Plans pane, R calls mutationStartPlan to flip it to
	// running. R is a no-op on any non-queued row -- the bottom hint
	// strip only advertises R:Run when the highlighted plan is
	// actually runnable.
	if v.Focus == FocusPlans && keyEv.Key() == tcell.KeyRune &&
		(keyEv.Rune() == 'r' || keyEv.Rune() == 'R') {
		if v.highlightedPlanIsQueued() {
			go v.runSelectedPlan()
		}
		return true
	}

	switch v.Focus {
	case FocusPlans:
		return v.handlePlanListKey(keyEv)
	case FocusTasks:
		return v.handleTaskListKey(keyEv)
	case FocusTaskDetail:
		return v.handleTaskDetailKey(keyEv)
	}
	return false
}

func (v *View) handlePlanListKey(ev *tcell.EventKey) bool {
	v.Mu.Lock()
	v.planList.Focused = true
	prev := v.planList.Selected
	if v.planList.HandleEvent(ev) {
		moved := v.planList.Selected != prev
		v.Mu.Unlock()
		if moved {
			go func() {
				v.RefreshTasksForSelected()
				v.Redraw()
			}()
		}
		return true
	}
	if ev.Key() == tcell.KeyEnter {
		v.Focus = FocusTasks
		v.Mu.Unlock()
		return true
	}
	v.Mu.Unlock()
	return false
}

func (v *View) handleTaskListKey(ev *tcell.EventKey) bool {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.taskList.Focused = true
	prev := v.taskList.Selected
	if v.taskList.HandleEvent(ev) {
		if v.taskList.Selected != prev {
			// Reset detail scroll so a fresh row starts at the top
			// of the detail pane instead of the previous row's
			// scroll position.
			v.taskDetailPane.ScrollY = 0
		}
		return true
	}
	switch ev.Key() {
	case tcell.KeyEnter:
		// Enter promotes focus to the detail pane so the user can
		// scroll through long input/output blocks without
		// hijacking the task list's Up/Down.
		v.Focus = FocusTaskDetail
		return true
	case tcell.KeyEsc:
		v.Focus = FocusPlans
		return true
	}
	return false
}

func (v *View) handleTaskDetailKey(ev *tcell.EventKey) bool {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.taskDetailPane.Focused = true
	if v.taskDetailPane.HandleEvent(ev) {
		return true
	}
	if ev.Key() == tcell.KeyEsc {
		v.Focus = FocusTasks
		return true
	}
	return false
}

// highlightedPlanIsQueued reports whether the currently-highlighted
// plan in the FocusPlans pane is in status="queued" (i.e. planning
// complete, ready for the user to click Run).
func (v *View) highlightedPlanIsQueued() bool {
	v.Mu.RLock()
	defer v.Mu.RUnlock()
	if v.planList.Selected < 0 || v.planList.Selected >= len(v.plans) {
		return false
	}
	return getString(v.plans[v.planList.Selected], "status") == "queued"
}

// runSelectedPlan calls mutationStartPlan on the highlighted plan
// when its status is "queued" (planning complete, awaiting user).
func (v *View) runSelectedPlan() {
	v.Mu.RLock()
	if v.planList.Selected < 0 || v.planList.Selected >= len(v.plans) {
		v.Mu.RUnlock()
		return
	}
	plan := v.plans[v.planList.Selected]
	v.Mu.RUnlock()

	if getString(plan, "status") != "queued" {
		if v.OnStatus != nil {
			v.OnStatus("planner: Run only applies to plans in 'queued' status")
		}
		return
	}
	planID := getString(plan, "id")
	if planID == "" {
		return
	}
	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := qc.MutationStartPlan(ctx, client.MutationStartPlanArgs{PlanId: planID}); err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("planner: run failed: %v", err))
		}
		return
	}
	v.RefreshPlans()
	v.Redraw()
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

func getInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		var n int
		_, err := fmt.Sscanf(v, "%d", &n)
		if err == nil {
			return n
		}
	}
	return 0
}

// taskTokensSpent extracts task.metrics.tokensSpent.
func taskTokensSpent(task map[string]any) int {
	if task == nil {
		return 0
	}
	metrics, _ := task["metrics"].(map[string]any)
	if metrics == nil {
		return 0
	}
	return getInt(metrics, "tokensSpent")
}

// formatTokens renders a token count in a compact "1.2k" / "23.4k" /
// "120" form.
func formatTokens(n int) string {
	if n < 0 {
		n = 0
	}
	if n < 1000 {
		return strconv.Itoa(n)
	}
	if n < 1_000_000 {
		v := float64(n) / 1000.0
		if v == float64(int(v)) {
			return fmt.Sprintf("%dk", int(v))
		}
		return fmt.Sprintf("%.1fk", v)
	}
	v := float64(n) / 1_000_000.0
	if v == float64(int(v)) {
		return fmt.Sprintf("%dM", int(v))
	}
	return fmt.Sprintf("%.1fM", v)
}

// planTokenSummary returns the plan-level "<spent> / <budget>" pair
// or just "<spent> tok" when no budget is set.
func planTokenSummary(plan map[string]any) string {
	if plan == nil {
		return ""
	}
	spent := getInt(plan, "tokenSpent")
	budget := getInt(plan, "tokenBudget")
	if spent == 0 && budget == 0 {
		return ""
	}
	if budget == 0 {
		return formatTokens(spent) + " tok"
	}
	return fmt.Sprintf("%s / %s tok", formatTokens(spent), formatTokens(budget))
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

func statusLabel(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// activeStatusElapsed returns a compact elapsed string for non-
// terminal, non-idle statuses. Terminal statuses and `queued`
// (idle, waiting on a user click) return "".
func activeStatusElapsed(status, createdAt string) string {
	switch status {
	case "planning", "routing", "running", "paused", "awaitingFeedback", "needsAgent":
		// fall through
	default:
		return ""
	}
	if createdAt == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		return ""
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh %dm", h, m)
	}
}

func truncate(s string, max int) string {
	if max < 1 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// isPlannerDSLMissing classifies an Execute error as "the BFF's
// memQL engine doesn't have the Planner queries / mutations
// registered" vs. anything else. Matches on `function "<name>"
// not found` for the three Planner-tab function names.
func isPlannerDSLMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") &&
		(strings.Contains(msg, `function "queryAllPlans"`) ||
			strings.Contains(msg, `function "queryTasksForPlan"`) ||
			strings.Contains(msg, `function "mutationCreatePlan"`))
}

// markDSLMissing latches the dslMissing flag and clears any
// pending error message so the gated screen has a clean slate.
// Notifications are deliberately NOT fired -- the "function not
// found" condition is a deploy-time capability gap, not a transient
// failure.
func (v *View) markDSLMissing() {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.dslMissing = true
}
