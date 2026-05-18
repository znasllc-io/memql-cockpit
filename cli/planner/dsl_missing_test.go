package planner

// Regression tests for the "function not found" gating path. The bug
// they pin down: the Planner tab polls queryAllPlans + queryTasksForPlan
// every 3 seconds, and when the cluster's BFF doesn't carry the
// Planner DSL (the copresent tree), every poll raised a notification
// like:
//
//   planner: queryAllPlans failed: query error: MemQL engine failed
//   to execute query. Details: function "queryAllPlans" not found
//
// Pre-fix that warning surfaced on every tick. Post-fix the first
// failure latches a dslMissing flag, suppresses the notification,
// stops the polling RTT, and renders an explanatory gated screen.

import (
	"errors"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// TestIsPlannerDSLMissing_Classification covers the matcher: it must
// fire only for the specific "function <name> not found" shape and
// only for the three function names the Planner tab calls. Anything
// else (a partition-ACL reject, a network timeout, a parser failure
// that happens to mention the word "function") must NOT be
// classified as DSL-missing -- those need their normal warning path.
func TestIsPlannerDSLMissing_Classification(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		expect bool
	}{
		{
			name:   "queryAllPlans missing (the reproducer)",
			err:    errors.New(`query error: MemQL engine failed to execute query. Details: function "queryAllPlans" not found`),
			expect: true,
		},
		{
			name:   "queryTasksForPlan missing",
			err:    errors.New(`function "queryTasksForPlan" not found`),
			expect: true,
		},
		{
			name:   "nil error",
			err:    nil,
			expect: false,
		},
		{
			name:   "unrelated function-not-found",
			err:    errors.New(`function "queryRandomThing" not found`),
			expect: false,
		},
		{
			name:   "permission denial -- distinct failure path",
			err:    errors.New("permission denied: caller lacks reader role on partition default"),
			expect: false,
		},
		{
			name:   "network timeout -- transient, must not latch",
			err:    errors.New("context deadline exceeded"),
			expect: false,
		},
		{
			name:   "parser error mentioning the word function but not a missing-function",
			err:    errors.New(`unexpected token "function" at line 12`),
			expect: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPlannerDSLMissing(tc.err); got != tc.expect {
				t.Errorf("isPlannerDSLMissing(%v) = %v, want %v", tc.err, got, tc.expect)
			}
		})
	}
}

// TestMarkDSLMissing_Latches checks the state mutation.
func TestMarkDSLMissing_Latches(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.markDSLMissing()
	if !v.dslMissing {
		t.Errorf("markDSLMissing did not set the flag")
	}
}

// TestDraw_DSLMissing renders the view in the latched state and
// asserts the user sees an explanation that names the missing
// functions. The exact wording is allowed to drift -- what matters
// is that (a) it mentions at least one of the three function names
// so an operator can grep the cluster's logs / DSL tree, and (b)
// it tells the user the R key retries the check.
func TestDraw_DSLMissing(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.dslMissing = true

	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	const w, h = 80, 20
	sim.SetSize(w, h)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)
	v.Draw(screen, ui.Rect{X: 0, Y: 0, Width: w, Height: h})
	sim.Show()

	rendered := flattenSim(sim)
	if !strings.Contains(rendered, "queryAllPlans") {
		t.Errorf("gated screen should name queryAllPlans so operators can grep for it; rendered:\n%s", rendered)
	}
	if !strings.Contains(strings.ToLower(rendered), "press r") {
		t.Errorf("gated screen should advertise the R retry key; rendered:\n%s", rendered)
	}
	if !strings.Contains(strings.ToLower(rendered), "planner") {
		t.Errorf("gated screen should mention the Planner tab; rendered:\n%s", rendered)
	}
}

// TestDraw_GatedMessagePrecedence ensures the outer
// GatedMessage (cluster-not-connected, set by app.go's
// updateTabGating) wins over the inner dslMissing latch. The
// connection state has to be fixed first; only when the cluster
// is connected does it make sense to surface a DSL capability gap.
func TestDraw_GatedMessagePrecedence(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.dslMissing = true
	v.GatedMessage = "Selected cluster \"local\" is connecting. Available again once it reaches a connected state."

	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	const w, h = 80, 20
	sim.SetSize(w, h)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)
	v.Draw(screen, ui.Rect{X: 0, Y: 0, Width: w, Height: h})
	sim.Show()

	rendered := flattenSim(sim)
	if !strings.Contains(rendered, "connecting") {
		t.Errorf("outer GatedMessage (connection state) must win; rendered:\n%s", rendered)
	}
	if strings.Contains(rendered, "Planner DSL not loaded") {
		t.Errorf("dslMissing message leaked through even though GatedMessage was set; rendered:\n%s", rendered)
	}
}

// flattenSim joins the simulation screen's cells into one string per
// row, then concatenates with newlines. Tests assert via
// strings.Contains so wrapping / centering changes don't break the
// check.
func flattenSim(sim tcell.SimulationScreen) string {
	cells, w, h := sim.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(c.Runes[0])
		}
		b.WriteByte('\n')
	}
	return b.String()
}
