package planner

// Regression tests pinning the planner view to the panel chrome
// contract documented in cli/CLAUDE.md. Mirrors
// cli/concepts/chrome_contract_test.go + cli/cluster/chrome_contract_test.go
// so the same rule set is enforced everywhere a new interactive
// pane is added.
//
// Specifically guards:
//   1. Action hints render in the LAST row of the pane, as
//      `Key:Label  Key:Label` chips with two-space separators.
//   2. No `n/m` count footer at the bottom row -- counts must ride
//      the title bar instead.
//   3. Hint vocabulary follows the contract (Tab:Cycle, not the
//      earlier Tab:Focus; CamelCase compound labels).

import (
	"regexp"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

const (
	plannerW = 100
	plannerH = 30
)

// hintChip matches the canonical `Key:Label` chip. Key is a possibly-
// empty whitespace-free run; Label is one whitespace-free run that
// may contain `/` for combo keys like `↑/↓`. Empty-key form `:Search`
// is permitted (the colon plays both roles); the planner view itself
// doesn't use that form, but keeping the grammar identical across
// chrome_contract_test.go files means future planner hints that DO
// adopt it pass without divergence.
var hintChip = regexp.MustCompile(`^\S*:\S+$`)

// drawFn is the per-pane draw function under test. The chrome
// contract is a per-pane assertion (each interactive pane has its
// own title bar + chrome band), so each test isolates a single pane
// rather than driving the full v.Draw -- which paints multiple
// adjacent panes that would smear together in a single-row slice
// of the simulation buffer.
type drawFn func(screen *ui.Screen, bounds ui.Rect)

// renderPaneAndSnapshot runs one pane's draw function against a
// fresh SimulationScreen and returns the rendered rows. Caller
// supplies the bounds the pane should paint into; the simulation
// screen sizes itself to match so there's no unused area to leak
// content from previous tests.
func renderPaneAndSnapshot(t *testing.T, draw drawFn, bounds ui.Rect) []string {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("simulation screen init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(bounds.Width, bounds.Y+bounds.Height)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)
	draw(screen, bounds)
	sim.Show()
	cells, w, h := sim.GetContents()
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(c.Runes[0])
		}
		rows[y] = b.String()
	}
	return rows
}

// makeView seeds a planner.View with enough state that the plan-list
// pane has rows to render. Tasks slice stays empty so the empty-state
// branch of drawTaskList exercises too. We bypass QueryClient
// entirely; the chrome contract is purely a draw-side concern.
func makeView() *View {
	v := NewView(ui.DefaultTheme())
	v.plans = []map[string]any{
		{"id": "plan-1", "goal": "do the thing", "status": "running", "createdAt": "2026-05-17T12:00:00Z"},
		{"id": "plan-2", "goal": "do the other thing", "status": "succeeded", "createdAt": "2026-05-17T11:30:00Z"},
	}
	return v
}

// assertChipFormat trims leading/trailing whitespace, splits on the
// canonical two-space separator, and runs the contract checks on
// each chip. Empty hint rows fail loudly -- every interactive pane
// must advertise at least one action.
func assertChipFormat(t *testing.T, where, row string) {
	t.Helper()
	row = strings.TrimSpace(row)
	if row == "" {
		t.Fatalf("%s: chrome band row was empty; every interactive pane must surface at least one Key:Label hint", where)
	}
	for _, chip := range strings.Split(row, "  ") {
		chip = strings.TrimSpace(chip)
		if chip == "" {
			continue
		}
		if !strings.Contains(chip, ":") {
			t.Errorf("%s: chip %q missing colon separator (contract: Key:Label)", where, chip)
			continue
		}
		idx := strings.Index(chip, ":")
		if idx+1 < len(chip) && chip[idx+1] == ' ' {
			t.Errorf("%s: chip %q has space after colon (contract: Key:Label with no space)", where, chip)
		}
		if !hintChip.MatchString(chip) {
			t.Errorf("%s: chip %q does not match canonical Key:Label shape", where, chip)
		}
	}
}

// TestPanelChrome_PlanList_ChromeBand asserts the plans pane renders
// its action hints in the LAST row of the bounds, as canonical
// `Key:Label` chips. Pre-migration this row carried a `n/m` count
// footer that violated the contract; the regression guard would
// fail again if a future refactor reintroduces it.
func TestPanelChrome_PlanList_ChromeBand(t *testing.T) {
	v := makeView()
	v.Focus = FocusPlans
	// Same shape drawPlanList sees in production -- a narrow column
	// under the goal pane. Wide enough that hints don't wrap.
	bounds := ui.Rect{X: 0, Y: 0, Width: 50, Height: plannerH}
	rows := renderPaneAndSnapshot(t, v.drawPlanList, bounds)

	// Title row carries the count.
	if !strings.Contains(rows[bounds.Y], "1/2") {
		t.Errorf("plans title row should carry `(sel/total)`; got %q", rows[bounds.Y])
	}

	// Last row is the chrome band with action hints.
	chrome := rows[bounds.Y+bounds.Height-1]
	assertChipFormat(t, "PlanList chrome band", chrome)
	if !strings.Contains(chrome, "Tab:Cycle") {
		t.Errorf("PlanList chrome must advertise Tab:Cycle (the contract vocabulary); got %q", chrome)
	}
	if strings.Contains(chrome, "Tab:Focus") {
		t.Errorf("PlanList chrome still uses Tab:Focus (pre-contract vocabulary); got %q", chrome)
	}

	// Same guard as the concepts contract test: a literal `digit/digit`
	// surrounded by spaces in the last row means the duplicate count
	// footer crept back in.
	dupCount := regexp.MustCompile(` \d+/\d+ `)
	if dupCount.MatchString(chrome) {
		t.Errorf("PlanList chrome row still carries a count footer: %q", chrome)
	}
}

// TestPanelChrome_TaskList_ChromeBand mirrors the plans assertion on
// the tasks pane. Both list panes share the same contract, and both
// previously violated it with a count-footer row.
func TestPanelChrome_TaskList_ChromeBand(t *testing.T) {
	v := makeView()
	v.Focus = FocusTasks
	v.tasks = []map[string]any{
		{"seq": 1, "kind": "llmAnalyze", "status": "succeeded"},
		{"seq": 2, "kind": "runCommand", "status": "running"},
		{"seq": 3, "kind": "persistResult", "status": "queued"},
	}
	bounds := ui.Rect{X: 0, Y: 0, Width: 60, Height: plannerH}
	rows := renderPaneAndSnapshot(t, v.drawTaskList, bounds)

	if !strings.Contains(rows[bounds.Y], "1/3") {
		t.Errorf("tasks title row should carry `(sel/total)`; got %q", rows[bounds.Y])
	}

	chrome := rows[bounds.Y+bounds.Height-1]
	assertChipFormat(t, "TaskList chrome band", chrome)
	if !strings.Contains(chrome, "Tab:Cycle") {
		t.Errorf("TaskList chrome must advertise Tab:Cycle; got %q", chrome)
	}
	dupCount := regexp.MustCompile(` \d+/\d+ `)
	if dupCount.MatchString(chrome) {
		t.Errorf("TaskList chrome row still carries a count footer: %q", chrome)
	}
}

// TestPanelChrome_GoalPane_Hints checks the goal-input pane's hint
// row -- it never had a count footer, but it WAS using the
// pre-contract `Tab:Focus` label that the chrome contract retired.
func TestPanelChrome_GoalPane_Hints(t *testing.T) {
	v := makeView()
	v.Focus = FocusGoal
	bounds := ui.Rect{X: 0, Y: 0, Width: 60, Height: 12}
	rows := renderPaneAndSnapshot(t, v.drawGoalPane, bounds)

	chrome := rows[bounds.Y+bounds.Height-1]
	assertChipFormat(t, "GoalPane chrome band", chrome)
	if !strings.Contains(chrome, "Enter:Submit") {
		t.Errorf("GoalPane chrome must advertise Enter:Submit; got %q", chrome)
	}
	if !strings.Contains(chrome, "Tab:Cycle") {
		t.Errorf("GoalPane chrome must advertise Tab:Cycle; got %q", chrome)
	}
	if strings.Contains(chrome, "Tab:Focus") {
		t.Errorf("GoalPane chrome still uses Tab:Focus (pre-contract vocabulary); got %q", chrome)
	}
}

// TestPanelChrome_EmptyStates_StillCarryHints asserts that the
// empty-data branches of drawPlanList / drawTaskList ALSO render the
// chrome band, not just the populated path. The pre-migration code
// painted a hint for the empty plans state but not the empty tasks
// state -- inconsistent UX that the contract calls out:
// "Every interactive pane must surface at least one Key:Label hint".
func TestPanelChrome_EmptyStates_StillCarryHints(t *testing.T) {
	t.Run("empty plans", func(t *testing.T) {
		v := NewView(ui.DefaultTheme())
		v.Focus = FocusPlans
		bounds := ui.Rect{X: 0, Y: 0, Width: 50, Height: 12}
		rows := renderPaneAndSnapshot(t, v.drawPlanList, bounds)
		assertChipFormat(t, "empty PlanList chrome", rows[bounds.Y+bounds.Height-1])
	})
	t.Run("empty tasks", func(t *testing.T) {
		v := makeView() // has plans, no tasks
		v.Focus = FocusTasks
		bounds := ui.Rect{X: 0, Y: 0, Width: 60, Height: 12}
		rows := renderPaneAndSnapshot(t, v.drawTaskList, bounds)
		assertChipFormat(t, "empty TaskList chrome", rows[bounds.Y+bounds.Height-1])
	})
}
