// Package planner renders the Planner tab: a thin operator surface
// for interacting with the Planner Agent that lives in the connected
// cluster.
//
// The user does not chat with the Planner. They submit a GOAL (the
// agent's prompt) and the Planner Agent decomposes it into Plans and
// Tasks. The right pane shows the live state of the selected Plan's
// Tasks (kind, status, output, errorMessage) refreshed on a timer.
//
// Data plane: mutationCreatePlan to submit, queryAllPlans + queryTasksForPlan
// to read. Both live in memql-bff-copresent/dsl/copresent/{mutations,queries}.memql.
// The Planner Agent (memql/integrations/planner/agent_loop.go) picks the
// Plan up from the event bus on planner-tagged nodes.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/client"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// FocusPane identifies which of the three keyboard-focus regions is
// active. Tab cycles through them.
type FocusPane int

const (
	FocusGoal  FocusPane = 0 // the goal input + submit chrome
	FocusPlans FocusPane = 1 // the plan list
	FocusTasks FocusPane = 2 // the task list under the selected plan
)

// defaultSpaceID is the fallback Plan.spaceId when the user hasn't
// overridden it via the Space input. Plans require a spaceId; cockpit
// is a developer tool and doesn't manage cognition spaces directly,
// so we pin all Planner-tab submissions to a sentinel space the user
// can search for ("plans authored from cockpit").
const defaultSpaceID = "cockpit-default"

// defaultPlanKind is the kind we stamp on Plans created from the
// Planner tab. Any kind other than adHocAction / scopeElevation /
// agentInvocation triggers the Planner Agent's HandlePlanCreated
// path (see memql/integrations/planner/agent_loop.go).
const defaultPlanKind = "userGoal"

// View is the Planner tab. Mutated from both the event-loop goroutine
// (HandleEvent) and a background refresher (refreshLoop). Locking
// mirrors the Concepts tab pattern: write lock on mutators, read lock
// on Draw.
type View struct {
	mu    sync.RWMutex
	Theme ui.Theme

	// Goal-input state.
	goalText  string
	spaceText string

	// Plans + currently-selected.
	plans         []map[string]any
	planSelected  int
	planScrollY   int

	// Tasks for the selected plan + currently-selected.
	tasks         []map[string]any
	taskSelected  int
	taskScrollY   int

	// Focus + transient flags.
	Focus     FocusPane
	submitErr string
	fetching  bool

	// Plumbing.
	//
	// QueryClient returns a client bound to the active cluster's
	// dispatcher, or nil. UserID is the caller's v1:identity:user.id
	// used as Plan.requestedBy.
	QueryClient func() *client.QueryClient
	UserID      func() string

	// GatedMessage, when non-empty, replaces the whole layout with a
	// "not available" message (no cluster connected, etc.).
	GatedMessage string

	// OnStatus surfaces a transient string to the notification bar.
	OnStatus func(msg string)

	// OnRedraw is posted by the background refresher when new data
	// has landed so the event loop redraws even if no key was pressed.
	OnRedraw func()
}

// NewView creates a fresh Planner view focused on the goal input.
func NewView(theme ui.Theme) *View {
	return &View{
		Theme:     theme,
		Focus:     FocusGoal,
		spaceText: defaultSpaceID,
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
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("planner: queryAllPlans failed: %v", err))
		}
		return
	}

	rows := extractRows(res)
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

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	res, err := qc.Execute(ctx, fmt.Sprintf(`queryTasksForPlan({"planId": %q})`, planID))
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("planner: queryTasksForPlan failed: %v", err))
		}
		return
	}

	rows := extractRows(res)
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

// submitGoal creates a v1:planner:plan via mutationCreatePlan, then
// kicks an immediate refresh so the user sees their submission. The
// Planner Agent (planner-tagged nodes only) picks the row up from the
// event bus and starts decomposing.
func (v *View) submitGoal() {
	v.mu.RLock()
	goal := strings.TrimSpace(v.goalText)
	space := strings.TrimSpace(v.spaceText)
	v.mu.RUnlock()

	if goal == "" {
		v.mu.Lock()
		v.submitErr = "goal is empty"
		v.mu.Unlock()
		return
	}
	if space == "" {
		space = defaultSpaceID
	}

	if v.QueryClient == nil {
		v.mu.Lock()
		v.submitErr = "no cluster connection"
		v.mu.Unlock()
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		v.mu.Lock()
		v.submitErr = "no cluster connection"
		v.mu.Unlock()
		return
	}

	userID := "cockpit"
	if v.UserID != nil {
		if id := v.UserID(); id != "" {
			userID = id
		}
	}

	args := map[string]any{
		"spaceId":       space,
		"kind":          defaultPlanKind,
		"goal":          goal,
		"requestedBy":   userID,
		"triggerSource": "user.explicit",
		"authorizedBy":  userID,
		"input": map[string]any{
			"prompt": goal,
		},
	}
	argBytes, _ := json.Marshal(args)
	call := fmt.Sprintf("mutationCreatePlan(%s)", string(argBytes))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := qc.Execute(ctx, call); err != nil {
		v.mu.Lock()
		v.submitErr = err.Error()
		v.mu.Unlock()
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("planner: submit failed: %v", err))
		}
		return
	}

	v.mu.Lock()
	v.goalText = ""
	v.submitErr = ""
	v.mu.Unlock()

	v.RefreshPlans()
	v.RefreshTasksForSelected()
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
		v.drawGated(screen, bounds)
		return
	}

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.40, MinSize: 32},
		{Flex: 0.60, MinSize: 40},
	})
	leftBounds := panes[0]
	rightBounds := panes[1]

	// Vertical divider (single-cell box-drawing char, safe at the edge).
	divStyle := v.Theme.SubtleStyle()
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		screen.SetCell(leftBounds.X+leftBounds.Width-1, y, '│', divStyle)
	}
	leftBounds.Width--

	// Left column: goal input (top) + plan list (bottom).
	leftRows := ui.FlexRow(leftBounds, []ui.FlexItem{
		{Fixed: 9},
		{Flex: 1.0, MinSize: 6},
	})
	v.drawGoalPane(screen, leftRows[0])
	v.drawPlanList(screen, leftRows[1])

	// Right column: plan detail (top) + task list (middle) + task detail (bottom).
	rightRows := ui.FlexRow(rightBounds, []ui.FlexItem{
		{Fixed: 8},
		{Flex: 0.55, MinSize: 6},
		{Flex: 0.45, MinSize: 6},
	})
	v.drawPlanDetail(screen, rightRows[0])
	v.drawTaskList(screen, rightRows[1])
	v.drawTaskDetail(screen, rightRows[2])
}

func (v *View) drawGated(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	title := " PLANNER "
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))
	midY := bounds.Y + bounds.Height/2
	lineX := bounds.X + (bounds.Width-len(v.GatedMessage))/2
	if lineX < bounds.X+1 {
		lineX = bounds.X + 1
	}
	screen.DrawText(lineX, midY, bounds.Width-1, v.GatedMessage, v.Theme.SubtleStyle())
}

func (v *View) drawGoalPane(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusGoal {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " NEW GOAL ", titleStyle)

	// Goal entry line.
	goalY := bounds.Y + 2
	prefix := "goal> "
	display := prefix + v.goalText
	if v.Focus == FocusGoal {
		display += "_"
	}
	goalStyle := v.Theme.BaseStyle()
	if v.Focus == FocusGoal {
		goalStyle = v.Theme.AccentStyle()
	}
	screen.DrawText(bounds.X+1, goalY, bounds.Width-2, display, goalStyle)

	// Space line.
	spaceY := bounds.Y + 4
	screen.DrawText(bounds.X+1, spaceY, bounds.Width-2,
		fmt.Sprintf("space: %s", v.spaceText), v.Theme.SubtleStyle())

	// Status / error line.
	statusY := bounds.Y + 5
	if v.submitErr != "" {
		screen.DrawText(bounds.X+1, statusY, bounds.Width-2,
			fmt.Sprintf("error: %s", v.submitErr), v.Theme.ErrorStyle())
	}

	// Action hints, anchored to the bottom row per the panel chrome
	// contract (cli/CLAUDE.md "Panel chrome contract"). Same
	// `Key:Label` grammar (two-space chip separator, CamelCase
	// labels, no space after colon) that Clusters / Concepts use.
	// `Tab:Cycle` rather than the earlier `Tab:Focus` to match the
	// contract's vocabulary.
	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1,
		"Enter:Submit  Esc:Clear  Tab:Cycle")
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
			"R:Refresh  Tab:Cycle")
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
		screen.DrawText(bounds.X+2, y+1, bounds.Width-3, sub, dimify(style, v.Theme.Subtle))
	}

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1,
		"↑/↓:Move  Enter:Tasks  R:Refresh  Tab:Cycle")
}

func (v *View) drawPlanDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	title := " PLAN DETAIL "
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))

	if v.planSelected < 0 || v.planSelected >= len(v.plans) {
		drawCentered(screen, v.Theme, bounds, "Select a plan to see its detail.")
		return
	}
	p := v.plans[v.planSelected]

	lines := []string{
		fmt.Sprintf("id:      %s", getString(p, "id")),
		fmt.Sprintf("goal:    %s", getString(p, "goal")),
		fmt.Sprintf("status:  %s", statusLabel(getString(p, "status"))),
		fmt.Sprintf("kind:    %s   space: %s", getString(p, "kind"), getString(p, "spaceId")),
		fmt.Sprintf("owner:   %s   by: %s",
			orDash(getString(p, "ownerAgentId")), getString(p, "requestedBy")),
		fmt.Sprintf("created: %s   started: %s   done: %s",
			shortenTimestamp(getString(p, "createdAt")),
			shortenTimestamp(getString(p, "startedAt")),
			shortenTimestamp(getString(p, "completedAt"))),
	}
	if errMsg := getString(p, "errorMessage"); errMsg != "" {
		lines = append(lines, fmt.Sprintf("error:   %s", errMsg))
	}

	for i, line := range lines {
		if i+1 >= bounds.Height-1 {
			break
		}
		style := v.Theme.BaseStyle()
		if strings.HasPrefix(line, "error:") {
			style = v.Theme.ErrorStyle()
		}
		screen.DrawText(bounds.X+1, bounds.Y+1+i, bounds.Width-2, line, style)
	}
}

func (v *View) drawTaskList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusTasks {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	// Position count rides the title bar per the chrome contract.
	title := " TASKS "
	if n := len(v.tasks); n > 0 {
		title = fmt.Sprintf(" TASKS (%d/%d) ", v.taskSelected+1, n)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if len(v.tasks) == 0 {
		drawCentered(screen, v.Theme, bounds, "No tasks for this plan.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1,
			"R:Refresh  Tab:Cycle")
		return
	}

	const chromeH = 1
	listTop := bounds.Y + 2
	listH := bounds.Height - 2 - chromeH
	if listH < 1 {
		listH = 1
	}
	v.clampTaskScroll(listH)

	for i := 0; i < listH && v.taskScrollY+i < len(v.tasks); i++ {
		idx := v.taskScrollY + i
		t := v.tasks[idx]
		y := listTop + i

		style := v.Theme.BaseStyle()
		if idx == v.taskSelected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)

		label := fmt.Sprintf("[%d] %-18s %s",
			getInt(t, "seq"),
			truncate(getString(t, "kind"), 18),
			statusLabel(getString(t, "status")))
		screen.DrawText(bounds.X+2, y, bounds.Width-3, label, style)
	}

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1,
		"↑/↓:Move  Esc:Plans  R:Refresh  Tab:Cycle")
}

func (v *View) drawTaskDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	title := " SELECTED TASK "
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))

	if v.taskSelected < 0 || v.taskSelected >= len(v.tasks) {
		drawCentered(screen, v.Theme, bounds, "Select a task to see its result.")
		return
	}
	t := v.tasks[v.taskSelected]

	contentTop := bounds.Y + 1
	contentH := bounds.Height - 1
	if contentH < 1 {
		return
	}

	lines := []string{
		fmt.Sprintf("kind:    %s", getString(t, "kind")),
		fmt.Sprintf("status:  %s", statusLabel(getString(t, "status"))),
		fmt.Sprintf("started: %s   done: %s",
			shortenTimestamp(getString(t, "startedAt")),
			shortenTimestamp(getString(t, "completedAt"))),
	}
	if errMsg := getString(t, "errorMessage"); errMsg != "" {
		lines = append(lines, fmt.Sprintf("error:   %s", errMsg))
	}
	lines = append(lines, "")
	lines = append(lines, "input:")
	lines = append(lines, prettyJSONLines(t["input"], bounds.Width-3)...)
	if out, ok := t["output"]; ok && out != nil {
		lines = append(lines, "")
		lines = append(lines, "output:")
		lines = append(lines, prettyJSONLines(out, bounds.Width-3)...)
	}

	for i, line := range lines {
		if i >= contentH {
			break
		}
		style := v.Theme.BaseStyle()
		if strings.HasPrefix(line, "error:") {
			style = v.Theme.ErrorStyle()
		}
		screen.DrawText(bounds.X+1, contentTop+i, bounds.Width-2, line, style)
	}
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
		v.Focus = (v.Focus + 1) % 3
		v.mu.Unlock()
		return true
	}

	// R refreshes plans + tasks, except while typing in the goal box
	// (where r should mean a literal 'r').
	if v.Focus != FocusGoal && keyEv.Key() == tcell.KeyRune &&
		(keyEv.Rune() == 'r' || keyEv.Rune() == 'R') {
		go func() {
			v.RefreshPlans()
			v.RefreshTasksForSelected()
			if v.OnRedraw != nil {
				v.OnRedraw()
			}
		}()
		return true
	}

	switch v.Focus {
	case FocusGoal:
		return v.handleGoalKey(keyEv)
	case FocusPlans:
		return v.handlePlanListKey(keyEv)
	case FocusTasks:
		return v.handleTaskListKey(keyEv)
	}
	return false
}

func (v *View) handleGoalKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyEnter:
		go v.submitGoal()
		return true
	case tcell.KeyEsc:
		v.mu.Lock()
		v.goalText = ""
		v.submitErr = ""
		v.mu.Unlock()
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		v.mu.Lock()
		if len(v.goalText) > 0 {
			// Trim a full rune from the end so multi-byte chars don't
			// leave dangling bytes in the string.
			r := []rune(v.goalText)
			v.goalText = string(r[:len(r)-1])
		}
		v.mu.Unlock()
		return true
	case tcell.KeyRune:
		v.mu.Lock()
		v.goalText += string(ev.Rune())
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

func (v *View) handleTaskListKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		v.mu.Lock()
		if v.taskSelected > 0 {
			v.taskSelected--
		}
		v.mu.Unlock()
		return true
	case tcell.KeyDown:
		v.mu.Lock()
		if v.taskSelected < len(v.tasks)-1 {
			v.taskSelected++
		}
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
	midY := bounds.Y + bounds.Height/2
	lineX := bounds.X + (bounds.Width-len(msg))/2
	if lineX < bounds.X+1 {
		lineX = bounds.X + 1
	}
	screen.DrawText(lineX, midY, bounds.Width-1, msg, theme.SubtleStyle())
}

func dimify(style tcell.Style, dim tcell.Color) tcell.Style {
	return style.Foreground(dim)
}

func extractRows(res any) []map[string]any {
	if res == nil {
		return nil
	}
	// Server returns shapes that come back as either a top-level array
	// or a wrapper object with the array under "rows" / "results" --
	// we accept both since both shapes appear across queries.
	switch v := res.(type) {
	case []any:
		return rowsFromArray(v)
	case map[string]any:
		for _, key := range []string{"rows", "results", "data", "output"} {
			if arr, ok := v[key].([]any); ok {
				return rowsFromArray(arr)
			}
		}
	}
	return nil
}

func rowsFromArray(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
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
