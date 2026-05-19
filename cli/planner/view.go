// Package planner renders the Planner tab: a read-only operator
// surface for observing v1:planner:plan + v1:planner:task rows in the
// connected cluster.
//
// Goal submission lives in the Chat tab now -- the user talks to the
// assistant, which decides whether to escalate to the planner via its
// tools. This tab is for watching what the planner is doing, not for
// driving it. The one mutation that remains is mutationStartPlan: when
// a plan sits in status="queued" awaiting user confirmation, pressing R
// on the Plans pane flips it to running.
//
// Data plane: queryAllPlans + queryTasksForPlan to read (live in
// memql-bff-copresent/dsl/copresent/queries.memql), mutationStartPlan
// to advance a queued plan. The Planner Agent
// (memql/integrations/planner/agent_loop.go) drives the actual
// decomposition + dispatch from the event bus on planner-tagged nodes.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/component/memql"

	"github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
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
	mu    sync.RWMutex
	Theme ui.Theme

	// Plans + currently-selected.
	plans         []map[string]any
	planSelected  int
	planScrollY   int

	// Tasks for the selected plan + currently-selected.
	tasks         []map[string]any
	taskSelected  int
	taskScrollY   int

	// Vertical scroll offset for the Task Detail pane (the third
	// pane on the right). Reset to 0 whenever taskSelected changes
	// so a fresh row starts at the top of the detail view.
	taskDetailScrollY int

	// Focus + transient flags.
	Focus     FocusPane
	fetching bool

	// dslMissing latches when the cluster's BFF returns
	// "function not found" for a Planner query. Once set, the
	// refresher stops issuing the failing call -- no point making
	// a fresh RTT every 3s -- and Draw renders an explanatory
	// gated screen instead of the empty layout. Cleared by
	// pressing R (manual refresh) so a re-deploy of the BFF that
	// loads the Planner DSL picks back up without restarting
	// the cockpit. See cli/planner/view.go isPlannerDSLMissing.
	dslMissing bool

	// Plumbing.
	//
	// QueryClient returns a client bound to the active cluster's
	// dispatcher, or nil.
	QueryClient func() *client.QueryClient

	// GatedMessage, when non-empty, replaces the whole layout with a
	// "not available" message (no cluster connected, etc.).
	GatedMessage string

	// OnStatus surfaces a transient string to the notification bar.
	OnStatus func(msg string)

	// OnRedraw is posted by the background refresher when new data
	// has landed so the event loop redraws even if no key was pressed.
	OnRedraw func()
}

// NewView creates a fresh Planner view focused on the plan list.
func NewView(theme ui.Theme) *View {
	return &View{
		Theme: theme,
		Focus: FocusPlans,
	}
}

// StartRefreshLoop runs a background ticker that re-pulls plans (and
// the selected plan's tasks) every interval. The caller passes a stop
// channel; closing it cancels the loop. Safe to call once at app-init
// after the cluster wiring is in place.
func (v *View) StartRefreshLoop(stop <-chan struct{}, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				v.RefreshPlans()
				v.RefreshTasksForSelected()
				if v.OnRedraw != nil {
					v.OnRedraw()
				}
			}
		}
	}()
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

	v.mu.Lock()
	if v.fetching {
		v.mu.Unlock()
		return
	}
	if v.dslMissing {
		// Cluster's BFF doesn't have the Planner DSL loaded. Don't
		// burn an RTT every refresh tick re-confirming. The user can
		// press R to retry (handlePlanListKey clears the latch).
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

	res, err := qc.Execute(ctx, "queryAllPlans({})")
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

	rows := memql.MaterializeRows(res)
	sort.Slice(rows, func(i, j int) bool {
		return getString(rows[i], "createdAt") > getString(rows[j], "createdAt")
	})

	v.mu.Lock()
	defer v.mu.Unlock()
	v.plans = rows
	if v.planSelected >= len(v.plans) {
		v.planSelected = 0
		v.planScrollY = 0
	}
}

// RefreshTasksForSelected re-pulls tasks for whichever plan is
// currently highlighted. No-op if no plan is selected.
func (v *View) RefreshTasksForSelected() {
	v.mu.RLock()
	planID := ""
	if v.planSelected >= 0 && v.planSelected < len(v.plans) {
		planID = getString(v.plans[v.planSelected], "id")
	}
	v.mu.RUnlock()
	if planID == "" {
		v.mu.Lock()
		v.tasks = nil
		v.taskSelected = 0
		v.taskScrollY = 0
		v.mu.Unlock()
		return
	}

	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}

	v.mu.RLock()
	missing := v.dslMissing
	v.mu.RUnlock()
	if missing {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	res, err := qc.Execute(ctx, fmt.Sprintf(`queryTasksForPlan({"planId": %q})`, planID))
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

	rows := memql.MaterializeRows(res)
	sort.SliceStable(rows, func(i, j int) bool {
		return getInt(rows[i], "seq") < getInt(rows[j], "seq")
	})

	v.mu.Lock()
	defer v.mu.Unlock()
	v.tasks = rows
	if v.taskSelected >= len(v.tasks) {
		v.taskSelected = 0
		v.taskScrollY = 0
	}
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// Draw renders the Planner tab. Holds the read lock so concurrent
// background refreshes can't tear the slices mid-paint.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.mu.RLock()
	defer v.mu.RUnlock()

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
	//   PLANS        (left, flex -- plans in the active partition)
	//   TASKS        (center, flex -- tasks under the highlighted plan)
	//   TASK DETAIL  (right, flex -- input / output / metadata for
	//                the highlighted task)
	//
	// Goal submission moved to the Chat tab -- the user talks to the
	// assistant, which decides when to escalate to the planner. The
	// planner tab is observation-only now (R:Run still flips queued
	// plans to running).
	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.28, MinSize: 32},
		{Flex: 0.36, MinSize: 28},
		{Flex: 0.36, MinSize: 28},
	})
	plansBounds := panes[0]
	tasksBounds := panes[1]
	detailBounds := panes[2]

	// Vertical dividers between each column. Single box-drawing char
	// per row -- safe at the layout edge (East-Asian-width Narrow).
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
// GatedMessage (cluster not connected, set by app.go's
// updateTabGating) and the planner-specific dslMissing latch (BFF
// doesn't have the Planner queries loaded). Message text drives the
// rendering; the layout is identical.
func (v *View) drawGated(screen *ui.Screen, bounds ui.Rect, msg string) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	title := " PLANNER "
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))
	// Wrap long messages -- the dslMissing explanation is long enough
	// that it would overflow a narrow Planner tab width.
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
		title = fmt.Sprintf(" PLANS (%d/%d) ", v.planSelected+1, n)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if len(v.plans) == 0 {
		drawCentered(screen, v.Theme, bounds, "No plans yet -- submit a goal above.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1,
			"Tab:Cycle")
		return
	}

	// Reserve one bottom row for the action-hint chrome band per the
	// panel chrome contract.
	const chromeH = 1
	listTop := bounds.Y + 2
	listH := bounds.Height - 2 - chromeH
	if listH < 1 {
		listH = 1
	}

	v.clampPlanScroll(listH)
	rowsPerPlan := 2
	maxVisible := listH / rowsPerPlan
	for i := 0; i < maxVisible && v.planScrollY+i < len(v.plans); i++ {
		idx := v.planScrollY + i
		p := v.plans[idx]
		y := listTop + i*rowsPerPlan

		style := v.Theme.BaseStyle()
		if idx == v.planSelected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)
		screen.FillRect(bounds.X, y+1, bounds.Width, 1, style)

		goal := getString(p, "goal")
		if goal == "" {
			goal = "(no goal)"
		}
		screen.DrawText(bounds.X+2, y, bounds.Width-3, goal, style)

		status := getString(p, "status")
		when := shortenTimestamp(getString(p, "createdAt"))
		sub := fmt.Sprintf("%s  %s", statusLabel(status), when)
		// Active-status timer: when a plan is in a non-terminal,
		// non-idle state, show how long it's been there so the user
		// can spot stuck plans at a glance. Terminal statuses
		// (succeeded / failed / cancelled) and `queued` (idle,
		// waiting for the user's Run click) get no timer -- a timer
		// on those is misleading. The clock is computed relative to
		// the row's createdAt because there isn't a separate
		// status-changed timestamp on the projection today; close
		// enough for at-a-glance UX, and a refresh upgrade is
		// straightforward once Plan.startedAt / phaseStartedAt land.
		if elapsed := activeStatusElapsed(status, getString(p, "createdAt")); elapsed != "" {
			sub = fmt.Sprintf("%s  %s  +%s", statusLabel(status), when, elapsed)
		}
		// Token strip rides on the subtitle so the plan list stays
		// 2 rows tall. Format: " · 1.2k / 5k tok" or " · 482 tok"
		// when there's no budget. Skipped entirely when the plan
		// hasn't spent any tokens yet (queued, idle).
		if tok := planTokenSummary(p); tok != "" {
			sub += "  ·  " + tok
		}
		screen.DrawText(bounds.X+2, y+1, bounds.Width-3, sub, dimify(style, v.Theme.Subtle))
	}

	hint := "↑/↓:Move  Enter:Tasks  Tab:Cycle"
	if v.planSelected >= 0 && v.planSelected < len(v.plans) &&
		getString(v.plans[v.planSelected], "status") == "queued" {
		hint = "↑/↓:Move  Enter:Tasks  R:Run  Tab:Cycle"
	}
	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hint)
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
		title = fmt.Sprintf(" TASKS (%d/%d) ", v.taskSelected+1, n)
	}
	if v.planSelected >= 0 && v.planSelected < len(v.plans) {
		if tok := planTokenSummary(v.plans[v.planSelected]); tok != "" {
			title = strings.TrimRight(title, " ") + "  ·  " + tok + " "
		}
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if len(v.tasks) == 0 {
		msg := "No tasks for this plan."
		if v.planSelected >= 0 && v.planSelected < len(v.plans) {
			switch getString(v.plans[v.planSelected], "status") {
			case "planning":
				msg = "Planning in progress -- the planner agent is decomposing the goal and emitting tasks. This usually takes 10-30 seconds. Tasks appear here as they are committed; the plan transitions to 'queued' once planning is done."
			case "needsAgent":
				msg = "Plan is parked: no good-fit agent in this space. Create or extend an agent that matches the goal, then resume the plan."
			case "awaitingFeedback":
				msg = "Plan is awaiting your feedback. Respond on the plan card to resume."
			case "succeeded":
				msg = "Plan completed with no tasks emitted."
			case "failed":
				if errMsg := getString(v.plans[v.planSelected], "errorMessage"); errMsg != "" {
					msg = "Plan failed: " + truncate(errMsg, 240)
				} else {
					msg = "Plan failed."
				}
			case "cancelled":
				msg = "Plan was cancelled."
			}
		}
		drawCentered(screen, v.Theme, bounds, msg)
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, "Tab:Cycle")
		return
	}

	const chromeH = 1
	listTop := bounds.Y + 2
	listH := bounds.Height - 2 - chromeH
	if listH < 1 {
		listH = 1
	}
	v.clampTaskScroll(listH)

	// Each task row is 2 visual rows: a primary line (seq + label)
	// and a subtitle (status + phase + elapsed-since-created). Same
	// pattern the Plans pane uses; gives the user enough at-a-glance
	// signal that the Task Detail pane doesn't have to carry every
	// bit of context.
	rowsPerTask := 2
	maxVisible := listH / rowsPerTask
	for i := 0; i < maxVisible && v.taskScrollY+i < len(v.tasks); i++ {
		idx := v.taskScrollY + i
		t := v.tasks[idx]
		y := listTop + i*rowsPerTask

		style := v.Theme.BaseStyle()
		if idx == v.taskSelected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)
		screen.FillRect(bounds.X, y+1, bounds.Width, 1, style)

		// Primary line: "[seq] <label>". Label prefers the
		// logicalStepId (the planner-assigned stable id like
		// "simulate-superman-vs-spiderman") because the `kind`
		// field is often the generic "chat" and isn't useful for
		// telling tasks apart. Fall back to kind when no
		// logicalStepId, fall back to "(unnamed)" otherwise.
		label := getString(t, "logicalStepId")
		if label == "" {
			label = getString(t, "kind")
		}
		if label == "" {
			label = "(unnamed)"
		}
		primary := fmt.Sprintf("[%d] %s", getInt(t, "seq"), label)
		screen.DrawText(bounds.X+2, y, bounds.Width-3, primary, style)

		// Subtitle: status + phase + active-status timer + tokens.
		// task.metrics.tokensSpent rolls up the task's LLM + tool
		// costs; 0 for queued / fresh-running tasks where no calls
		// have completed yet.
		status := getString(t, "status")
		sub := statusLabel(status)
		if phase := getString(t, "phase"); phase != "" {
			sub += "  phase:" + phase
		}
		if elapsed := activeStatusElapsed(status, getString(t, "createdAt")); elapsed != "" {
			sub += "  +" + elapsed
		}
		if tok := taskTokensSpent(t); tok > 0 {
			sub += "  ·  " + formatTokens(tok) + " tok"
		}
		screen.DrawText(bounds.X+2, y+1, bounds.Width-3, sub, dimify(style, v.Theme.Subtle))
	}

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1,
		"↑/↓:Move  Enter:Detail  Esc:Plans  Tab:Cycle")
}

// drawTaskDetail renders the third pane: rich per-task detail for
// whichever task is highlighted in the center Tasks pane. Sections,
// top to bottom: identity (id, agent, kind, phase, status, attempts),
// timing (createdAt, startedAt, completedAt, elapsed), input
// payload, output payload, error message, parking state. Each section
// is omitted when empty, so a freshly-queued task with no output yet
// renders only identity + timing + input.
func (v *View) drawTaskDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusTaskDetail {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	title := " TASK DETAIL "
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	// Empty-state precedence: no plan selected -> no task -> nothing
	// to show. Each handled with a centered hint that wraps to pane
	// width via drawCentered.
	if len(v.plans) == 0 || v.planSelected < 0 || v.planSelected >= len(v.plans) {
		drawCentered(screen, v.Theme, bounds, "Select a plan to see its tasks.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, "Tab:Cycle")
		return
	}
	if len(v.tasks) == 0 || v.taskSelected < 0 || v.taskSelected >= len(v.tasks) {
		drawCentered(screen, v.Theme, bounds, "No task selected. Pick one in the Tasks pane to see its input, output, and metadata here.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, "Tab:Cycle")
		return
	}

	task := v.tasks[v.taskSelected]
	plan := v.plans[v.planSelected]

	// Build the rendered block as a []string of lines, then paint with
	// scroll. Easier to manage than direct draw + a tight bounds
	// calculation, and lets the same code path drive PageUp/Down later.
	lines := v.buildTaskDetailLines(task, plan, bounds.Width-3)

	const chromeH = 1
	bodyTop := bounds.Y + 2
	bodyH := bounds.Height - 2 - chromeH
	if bodyH < 1 {
		bodyH = 1
	}
	// Clamp scroll so we never paint past the end.
	maxScroll := len(lines) - bodyH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if v.taskDetailScrollY > maxScroll {
		v.taskDetailScrollY = maxScroll
	}
	if v.taskDetailScrollY < 0 {
		v.taskDetailScrollY = 0
	}

	for i := 0; i < bodyH && v.taskDetailScrollY+i < len(lines); i++ {
		line := lines[v.taskDetailScrollY+i]
		style := v.Theme.BaseStyle()
		// Section headers (lines that start with "─" or end with ":")
		// render in subtle/bold to give the eye anchors as it scrolls.
		if strings.HasPrefix(line, "─") {
			style = v.Theme.SubtleStyle().Bold(true)
		}
		screen.DrawText(bounds.X+2, bodyTop+i, bounds.Width-3, line, style)
	}

	hint := "↑/↓:Scroll  Esc:Tasks  Tab:Cycle"
	if maxScroll == 0 {
		hint = "Esc:Tasks  Tab:Cycle"
	}
	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hint)
}

// buildTaskDetailLines flattens the task + plan context into a list
// of pre-wrapped display lines for the detail pane. Pure function so
// it's easy to unit-test against fixture rows. innerWidth bounds each
// JSON/string field's wrap so the detail pane never overflows.
func (v *View) buildTaskDetailLines(task, plan map[string]any, innerWidth int) []string {
	if innerWidth < 16 {
		innerWidth = 16
	}
	var lines []string

	addKV := func(label, value string) {
		if value == "" {
			value = "—"
		}
		// Two-space indent under the section header keeps the eye on
		// the key column.
		line := fmt.Sprintf("  %-13s %s", label+":", value)
		// If the value pushes us past innerWidth, wrap the continuation
		// lines flush under the value column.
		if len(line) <= innerWidth {
			lines = append(lines, line)
			return
		}
		wrapped := ui.WrapText(value, innerWidth-17)
		lines = append(lines, fmt.Sprintf("  %-13s %s", label+":", wrapped[0]))
		for _, cont := range wrapped[1:] {
			lines = append(lines, strings.Repeat(" ", 17)+cont)
		}
	}
	addSection := func(name string) {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "─ "+name+" ─")
	}
	addBlock := func(name string, body string) {
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		addSection(name)
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

	// Timing section: createdAt + startedAt + completedAt; elapsed is
	// computed for transient statuses, omitted for terminal.
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

	// Tokens section: per-task LLM + tool spend, with the parent
	// Plan's total + budget alongside so the operator sees the
	// task's share of the plan-level cap. Always rendered (even if
	// 0) so the section is a stable reference point as tasks
	// transition queued -> running -> succeeded and the counts
	// climb. Model breakdown (modelBreakdown[]) is collapsed into
	// one line per (tag, tokens) entry when present.
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
			lines = append(lines, "")
			lines = append(lines, "  by model:")
			for _, item := range breakdown {
				row, _ := item.(map[string]any)
				if row == nil {
					continue
				}
				tag := getString(row, "tag")
				if tag == "" {
					tag = "(unknown)"
				}
				lines = append(lines, fmt.Sprintf("    - %s: %s tok", tag, formatTokens(getInt(row, "tokens"))))
			}
		}
	}

	// Input / output / tool args+result / error / metrics blocks --
	// each rendered as JSON when non-empty. addBlock skips empties so
	// the pane only shows sections that have content. The full
	// metrics JSON still renders at the bottom; the tokens section
	// above is the surface-level summary.
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

// prettyJSON serializes v with two-space indentation. Used by the
// task detail pane to render input/output/error payloads. Returns
// "" for nil / empty-map / empty-slice so the caller's addBlock
// skips the section entirely. Marshal errors return "" too; the
// pane is a developer surface, not a serializer correctness gate.
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
// Input handling
// ---------------------------------------------------------------------------

// HandleEvent processes a key event. Returns true if consumed.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// Tab cycles focus regardless of which pane is active. Must come
	// before the goal-input typing branch so Tab doesn't get eaten as
	// a literal character in the goal field.
	if keyEv.Key() == tcell.KeyTab {
		v.mu.Lock()
		v.Focus = (v.Focus + 1) % FocusPane(focusPaneCount)
		v.mu.Unlock()
		return true
	}

	// R means Run: when a plan in status="queued" is highlighted in
	// the Plans pane, R calls mutationStartPlan to flip it to
	// running. R is a no-op on any non-queued row -- the bottom hint
	// strip only advertises R:Run when the highlighted plan is
	// actually runnable. (The dslMissing latch can still be cleared
	// via re-entering the tab; we deliberately drop the implicit-
	// refresh behavior here so R has one unambiguous meaning per the
	// cockpit panel-chrome contract.)
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

// handleTaskDetailKey scrolls the detail pane's content (Up/Down,
// PgUp/PgDn, Home/End) and bounces focus back to the Tasks pane on
// Esc. The pane's content is a pre-wrapped []string built by
// buildTaskDetailLines; this handler only mutates taskDetailScrollY,
// the Draw path clamps it on the next paint.
func (v *View) handleTaskDetailKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		v.mu.Lock()
		if v.taskDetailScrollY > 0 {
			v.taskDetailScrollY--
		}
		v.mu.Unlock()
		return true
	case tcell.KeyDown:
		v.mu.Lock()
		v.taskDetailScrollY++
		v.mu.Unlock()
		return true
	case tcell.KeyPgUp:
		v.mu.Lock()
		v.taskDetailScrollY -= 10
		if v.taskDetailScrollY < 0 {
			v.taskDetailScrollY = 0
		}
		v.mu.Unlock()
		return true
	case tcell.KeyPgDn:
		v.mu.Lock()
		v.taskDetailScrollY += 10
		v.mu.Unlock()
		return true
	case tcell.KeyHome:
		v.mu.Lock()
		v.taskDetailScrollY = 0
		v.mu.Unlock()
		return true
	case tcell.KeyEsc:
		v.mu.Lock()
		v.Focus = FocusTasks
		v.mu.Unlock()
		return true
	}
	return false
}

func (v *View) handlePlanListKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		v.mu.Lock()
		if v.planSelected > 0 {
			v.planSelected--
		}
		v.mu.Unlock()
		go func() {
			v.RefreshTasksForSelected()
			if v.OnRedraw != nil {
				v.OnRedraw()
			}
		}()
		return true
	case tcell.KeyDown:
		v.mu.Lock()
		if v.planSelected < len(v.plans)-1 {
			v.planSelected++
		}
		v.mu.Unlock()
		go func() {
			v.RefreshTasksForSelected()
			if v.OnRedraw != nil {
				v.OnRedraw()
			}
		}()
		return true
	case tcell.KeyEnter:
		v.mu.Lock()
		v.Focus = FocusTasks
		v.mu.Unlock()
		return true
	}
	return false
}

// highlightedPlanIsQueued reports whether the currently-highlighted
// plan in the FocusPlans pane is in status="queued" (i.e. planning
// complete, ready for the user to click Run). Used by the R-key
// dispatcher to decide Run-vs-Refresh.
func (v *View) highlightedPlanIsQueued() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.planSelected < 0 || v.planSelected >= len(v.plans) {
		return false
	}
	return getString(v.plans[v.planSelected], "status") == "queued"
}

// runSelectedPlan calls mutationStartPlan on the highlighted plan
// when its status is "queued" (planning complete, awaiting user).
// No-op for any other status -- the prompt strip in the chrome band
// only advertises R:Run when the highlighted plan is actually queued.
func (v *View) runSelectedPlan() {
	v.mu.RLock()
	if v.planSelected < 0 || v.planSelected >= len(v.plans) {
		v.mu.RUnlock()
		return
	}
	plan := v.plans[v.planSelected]
	v.mu.RUnlock()

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
	call := fmt.Sprintf("mutationStartPlan({planId:%q})", planID)
	if _, err := qc.Execute(ctx, call); err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("planner: run failed: %v", err))
		}
		return
	}
	v.RefreshPlans()
	if v.OnRedraw != nil {
		v.OnRedraw()
	}
}

func (v *View) handleTaskListKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		v.mu.Lock()
		if v.taskSelected > 0 {
			v.taskSelected--
			// Reset detail scroll so a fresh row starts at the top
			// of the detail pane instead of the previous row's
			// scroll position.
			v.taskDetailScrollY = 0
		}
		v.mu.Unlock()
		return true
	case tcell.KeyDown:
		v.mu.Lock()
		if v.taskSelected < len(v.tasks)-1 {
			v.taskSelected++
			v.taskDetailScrollY = 0
		}
		v.mu.Unlock()
		return true
	case tcell.KeyEnter:
		// Enter promotes focus to the detail pane so the user can
		// scroll through long input/output blocks without
		// hijacking the task list's Up/Down.
		v.mu.Lock()
		v.Focus = FocusTaskDetail
		v.mu.Unlock()
		return true
	case tcell.KeyEsc:
		v.mu.Lock()
		v.Focus = FocusPlans
		v.mu.Unlock()
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (v *View) clampPlanScroll(visibleRows int) {
	rowsPerPlan := 2
	maxVisible := visibleRows / rowsPerPlan
	if maxVisible < 1 {
		maxVisible = 1
	}
	if v.planSelected < v.planScrollY {
		v.planScrollY = v.planSelected
	}
	if v.planSelected >= v.planScrollY+maxVisible {
		v.planScrollY = v.planSelected - maxVisible + 1
	}
	if v.planScrollY < 0 {
		v.planScrollY = 0
	}
}

func (v *View) clampTaskScroll(visibleRows int) {
	if v.taskSelected < v.taskScrollY {
		v.taskScrollY = v.taskSelected
	}
	if v.taskSelected >= v.taskScrollY+visibleRows {
		v.taskScrollY = v.taskSelected - visibleRows + 1
	}
	if v.taskScrollY < 0 {
		v.taskScrollY = 0
	}
}

func drawCentered(screen *ui.Screen, theme ui.Theme, bounds ui.Rect, msg string) {
	// Width-aware: explicit `\n` becomes a hard break, and any
	// individual line longer than the pane's inner width is word-
	// wrapped via ui.WrapText. The resulting block is centered
	// vertically and horizontally within bounds.
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

// (extractRows / rowsFromArray moved to component/memql.MaterializeRows
// in memql -- one canonical helper for every consumer of a query
// response, in-process or over the wire.)

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
		// Permit numeric strings from the wire encoder.
		var n int
		_, err := fmt.Sscanf(v, "%d", &n)
		if err == nil {
			return n
		}
	}
	return 0
}

// taskTokensSpent extracts task.metrics.tokensSpent. Tasks track
// their own LLM + tool token costs under the metrics rollup; the
// parent Plan's tokenSpent is the sum of these plus the planner's
// own decomposition / replanning calls. Returns 0 when metrics is
// missing or hasn't been populated yet (e.g., a fresh queued task).
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
// "120" form so it fits on a row alongside status + phase + elapsed
// without pushing the elapsed timer off the edge. Anything under 1000
// renders verbatim; 1000+ rolls up to a one-decimal "k" with the
// trailing ".0" stripped so 4000 reads as "4k" not "4.0k".
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
// for display in the Tasks pane title, or just "<spent> tok" when no
// budget is set. Empty when the plan has no token activity yet
// (queued state, no llm calls).
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
		// Fall back to "2026-05-17T12:34:56..." -> first 16 chars
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

// activeStatusElapsed returns a compact "Ns / Nm Ss / Nh Mm" string
// describing how long the plan has been in the given status, but
// ONLY for statuses where a timer carries useful signal. Terminal
// statuses (succeeded / failed / cancelled) and `queued` (waiting
// on a user click, intentionally idle) return "" so the row chrome
// stays clean. Anything else (planning / routing / running /
// paused / awaitingFeedback / needsAgent) is "active" -- the user
// wants to see how long it's been sitting there.
//
// Timestamp source today is row.createdAt, which approximates the
// time the plan entered the current status: close enough at a
// glance, and tightens up once the planner stamps
// statusChangedAt / phaseStartedAt on every transition (follow-up
// work mentioned in docs/planner/observability.md).
func activeStatusElapsed(status, createdAt string) string {
	switch status {
	case "planning", "routing", "running", "paused", "awaitingFeedback", "needsAgent":
		// fall through; these get a timer
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

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
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
// registered" vs. anything else. The engine surfaces missing
// functions as `function "<name>" not found` (see
// component/memql/engine.go's resolver). Matching on that exact
// shape avoids tripping on unrelated errors that happen to contain
// the word "function" -- a permission denial, a parser failure, or
// a partition-ACL reject all use different verbs.
//
// The check is intentionally loose on the function name -- we want
// the same code path to fire for queryAllPlans, queryTasksForPlan,
// and mutationCreatePlan since all three live in the same DSL tree
// (memql-bff-copresent/dsl/copresent/{queries,mutations}.memql).
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
// pending error message so the gated screen has a clean slate to
// render against. Notifications are deliberately NOT fired -- the
// "function not found" condition is a deploy-time capability gap,
// not a transient failure, and surfacing it on every 3s refresh
// tick was exactly the bug that prompted this fix.
func (v *View) markDSLMissing() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.dslMissing = true
}

func prettyJSONLines(v any, maxWidth int) []string {
	if v == nil {
		return []string{"  (none)"}
	}
	b, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		return []string{fmt.Sprintf("  %v", v)}
	}
	raw := strings.Split(string(b), "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		line = "  " + line
		if maxWidth > 0 && len(line) > maxWidth {
			line = line[:maxWidth-1] + "…"
		}
		out = append(out, line)
	}
	return out
}
