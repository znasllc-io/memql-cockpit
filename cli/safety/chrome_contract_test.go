package safety

// Regression tests for the panel chrome contract documented in
// cli/CLAUDE.md ("Panel chrome contract"). Mirrors the structure of
// cli/concepts/chrome_contract_test.go: drive Draw against a
// tcell.NewSimulationScreen, then assert on the rendered cells.
//
// Pins down four load-bearing rules for the Safety tab:
//
//  1. Pane titles are bare names with no embedded ids or counters
//     glued on with parentheses -- "DECISIONS" + "DETAIL", no
//     "DECISIONS (3)".
//  2. The DECISIONS counter renders right-aligned in the title row,
//     using FormatFiltered (N/M, optionally with "filtered from K").
//  3. Action hints render in the bottom chrome band using the
//     `Key:Label  Key:Label` grammar via ui.HintBar / DrawBottom.
//     Hints land in the last row, NOT in a strip below the title.
//  4. Pressing `:` swaps the hint row for the search prompt
//     ":search _" -- search is invoked by `:`, never `/`.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

const (
	viewWidth  = 140
	viewHeight = 30
)

// makeView builds a Safety view with three classification rows
// installed directly under the view's lock -- mirroring what a
// successful Refresh() would do. Bypassing the SDK keeps the test
// hermetic; the row plumbing is exercised by Refresh's call to
// recomputeMatchesLocked, so writing those slices by hand is
// faithful to the production path.
func makeView(t *testing.T) (*View, *ui.Screen, tcell.SimulationScreen, ui.Rect) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("simulation screen init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(viewWidth, viewHeight)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)

	v := NewView(ui.DefaultTheme())
	v.Mu.Lock()
	v.rows = []map[string]any{
		{
			"id": "row-alpha", "createdAt": "2026-05-26T12:14:53Z",
			"surface": "workbench", "action": "exec",
			"decision": "deny", "source": "rule", "tier": "high", "mode": "shadow",
			"reason": "rm -rf /", "ruleId": "shell.destructive",
		},
		{
			"id": "row-beta", "createdAt": "2026-05-26T12:13:01Z",
			"surface": "computer_use_headless", "action": "fs_read",
			"decision": "allow", "source": "cache", "tier": "low", "mode": "enforce",
			"reason": "read config",
		},
		{
			"id": "row-gamma", "createdAt": "2026-05-26T12:12:00Z",
			"surface": "workbench", "action": "http_fetch",
			"decision": "ask", "source": "model", "tier": "medium", "mode": "shadow",
			"reason": "credential_access pattern in URL", "ruleId": "model.classify_v1",
		},
	}
	v.recomputeMatchesLocked()
	v.Mu.Unlock()

	return v, screen, sim, ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight}
}

// drawAndSnapshot paints the view and returns each terminal row as a
// string. Empty cells render as spaces so column positions are stable.
func drawAndSnapshot(v *View, screen *ui.Screen, sim tcell.SimulationScreen, bounds ui.Rect) []string {
	sim.Clear()
	v.Draw(screen, bounds)
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

func TestChrome_PaneTitles(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	rows := drawAndSnapshot(v, screen, sim, bounds)

	title := rows[0]
	if !strings.Contains(title, "DECISIONS") {
		t.Errorf("title row missing DECISIONS: %q", title)
	}
	if !strings.Contains(title, "DETAIL") {
		t.Errorf("title row missing DETAIL: %q", title)
	}
	// Counter rule: no parentheses around the row count.
	if strings.Contains(title, "(3)") || strings.Contains(title, "(3 ") {
		t.Errorf("title row should not wrap counter in parens: %q", title)
	}
	// FormatCursor / FormatFiltered emits "1/3" -- not "1/3 of 3".
	if !strings.Contains(title, "1/3") {
		t.Errorf("title row should render 1/3 counter, got: %q", title)
	}
}

func TestChrome_ActionHintsInBottomRow(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	rows := drawAndSnapshot(v, screen, sim, bounds)

	// Chrome contract: action hints live in the last row of the pane,
	// never as a strip below the title. We check the bottom row for
	// the hint chips and the row below the title for absence of them.
	last := rows[len(rows)-1]
	for _, chip := range []string{":Search", "R:Refresh", "Tab:Cycle"} {
		if !strings.Contains(last, chip) {
			t.Errorf("bottom chrome row missing %q chip: %q", chip, last)
		}
	}

	// Row 2 (directly below the title) must not carry the hint chips.
	below := rows[1]
	if strings.Contains(below, ":Search") || strings.Contains(below, "Tab:Cycle") {
		t.Errorf("hint chips leaked into row below the title: %q", below)
	}
}

func TestChrome_FilterChipsAboveHintRow(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	rows := drawAndSnapshot(v, screen, sim, bounds)

	// Filter chip row sits above the bottom hint row, rendered by
	// drawAggregateAndFilters. Find it by string match on the
	// bracketed cycle-key prefix unique to this row.
	hit := -1
	for i, r := range rows {
		if strings.Contains(r, "[D]decision:") {
			hit = i
			break
		}
	}
	if hit < 0 {
		t.Fatalf("filter chip row not rendered")
	}
	if hit >= len(rows)-1 {
		t.Errorf("filter chip row should be above the bottom hint row, found at %d (last index %d)", hit, len(rows)-1)
	}
	for _, want := range []string{"[D]decision:*", "[S]source:*", "[T]tier:*", "[U]surface:*", "[M]mode:*"} {
		if !strings.Contains(rows[hit], want) {
			t.Errorf("filter row missing %q: %q", want, rows[hit])
		}
	}
}

func TestChrome_AggregateStripRendersTotals(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	rows := drawAndSnapshot(v, screen, sim, bounds)

	// One row carries "totals N  allow X  ask Y  deny Z".
	found := false
	for _, r := range rows {
		if strings.Contains(r, "totals 3") &&
			strings.Contains(r, "allow 1") &&
			strings.Contains(r, "ask 1") &&
			strings.Contains(r, "deny 1") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("aggregate strip should report totals + decision counts; full snapshot:\n%s",
			strings.Join(rows, "\n"))
	}
}

func TestChrome_ColonOpensSearchPrompt(t *testing.T) {
	v, screen, sim, bounds := makeView(t)

	// Press ':' on the decisions pane.
	consumed := v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, ':', tcell.ModNone))
	if !consumed {
		t.Fatal(": should be consumed by the Safety view (search trigger)")
	}

	rows := drawAndSnapshot(v, screen, sim, bounds)
	last := rows[len(rows)-1]

	// Bottom row should now read ":search _" rather than the action
	// hint chips. The view paints a literal underscore cursor.
	if !strings.Contains(last, ":search") {
		t.Errorf("bottom row should show search prompt after `:`: %q", last)
	}
	// Action-hint chips must have stepped aside while search is active.
	if strings.Contains(last, "D:Decision") {
		t.Errorf("search prompt and action hints both rendered in bottom row: %q", last)
	}
}

func TestChrome_SlashIsNoOp(t *testing.T) {
	// `/` is reserved by the chrome contract -- not bound today.
	// Pressing it must NOT open the search prompt and must NOT be
	// consumed; the App-level handler is then free to repurpose it.
	v, screen, sim, bounds := makeView(t)

	consumed := v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, '/', tcell.ModNone))
	if consumed {
		t.Fatal(`/ should NOT be consumed by the Safety view (reserved key)`)
	}
	rows := drawAndSnapshot(v, screen, sim, bounds)
	last := rows[len(rows)-1]
	if strings.Contains(last, ":search") {
		t.Errorf("/ should not open the search prompt: %q", last)
	}
}

func TestChrome_DecisionCycleFiltersList(t *testing.T) {
	// Pressing D cycles the decision filter and the row count
	// reflected in the title shrinks. The chip row also updates.
	v, screen, sim, bounds := makeView(t)

	// First D press: decision = "allow" (decisionCycle[1]).
	if !v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 'D', tcell.ModNone)) {
		t.Fatal("D should be consumed by the Safety view")
	}
	rows := drawAndSnapshot(v, screen, sim, bounds)
	title := rows[0]
	if !strings.Contains(title, "1/1 filtered from 3") {
		t.Errorf("after D press: title should show filtered-from counter, got: %q", title)
	}
	// Filter chip strip must reflect the new value.
	found := false
	for _, r := range rows {
		if strings.Contains(r, "[D]decision:allow") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("filter chip row should reflect [D]decision:allow")
	}
}

func TestChrome_HintBarKeyLabelGrammar(t *testing.T) {
	// Spot-check that no chip uses the wrong-format `Key: Label`
	// (space after colon) or `Press X to Y` prose. The HintBar
	// helper enforces this -- this test is a regression guard
	// against someone hand-rolling a chip with the wrong shape.
	v, screen, sim, bounds := makeView(t)
	rows := drawAndSnapshot(v, screen, sim, bounds)
	last := rows[len(rows)-1]

	if strings.Contains(last, "Press ") {
		t.Errorf("bottom row should use Key:Label grammar, not 'Press X' prose: %q", last)
	}
	if strings.Contains(last, "R: Refresh") || strings.Contains(last, "Tab: Cycle") {
		t.Errorf("Key:Label grammar disallows space after the colon: %q", last)
	}
}
