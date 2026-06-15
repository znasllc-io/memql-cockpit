package ui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestPaneTitle_FormatHelpers pins the counter-formatting contract.
// Every pane that adopts PaneTitle reads its counter string from one
// of these helpers, so a regression here would silently change the
// chrome of every pane in the cockpit.
func TestPaneTitle_FormatHelpers(t *testing.T) {
	t.Run("FormatCursor", func(t *testing.T) {
		cases := []struct {
			selected int
			total    int
			want     string
		}{
			{0, 0, ""},    // empty list -> bare title
			{0, 1, "1/1"}, // 0-indexed cursor displays as 1-indexed
			{2, 5, "3/5"},
			{-1, 5, "1/5"}, // clamp under
			{99, 5, "5/5"}, // clamp over
			{0, -3, ""},    // defensive: negative total -> empty
		}
		for _, tc := range cases {
			if got := FormatCursor(tc.selected, tc.total); got != tc.want {
				t.Errorf("FormatCursor(%d, %d) = %q, want %q", tc.selected, tc.total, got, tc.want)
			}
		}
	})

	t.Run("FormatFiltered", func(t *testing.T) {
		cases := []struct {
			selected, matches, total int
			want                     string
		}{
			{0, 0, 0, ""},                       // empty everything
			{0, 5, 5, "1/5"},                    // no filter active
			{2, 3, 12, "3/3 filtered from 12"},  // filter narrows
			{0, 0, 12, "0/0 filtered from 12"},  // filter matched nothing
			{99, 3, 12, "3/3 filtered from 12"}, // selected clamps under matches
		}
		for _, tc := range cases {
			if got := FormatFiltered(tc.selected, tc.matches, tc.total); got != tc.want {
				t.Errorf("FormatFiltered(%d, %d, %d) = %q, want %q",
					tc.selected, tc.matches, tc.total, got, tc.want)
			}
		}
	})

	t.Run("FormatLine", func(t *testing.T) {
		if got := FormatLine(0, 0); got != "" {
			t.Errorf("FormatLine(0, 0) = %q, want empty", got)
		}
		if got := FormatLine(3, 27); got != "line 4/27" {
			t.Errorf("FormatLine(3, 27) = %q, want \"line 4/27\"", got)
		}
	})

	t.Run("FormatCount", func(t *testing.T) {
		if got := FormatCount(0); got != "" {
			t.Errorf("FormatCount(0) = %q, want empty", got)
		}
		if got := FormatCount(5); got != "5" {
			t.Errorf("FormatCount(5) = %q, want \"5\"", got)
		}
	})

	// Contract guard: no helper should ever produce a parenthesized
	// counter. The PaneTitle widget itself doesn't add parens, but a
	// future helper that did would silently break the format across
	// every pane.
	for _, s := range []string{
		FormatCursor(2, 5),
		FormatFiltered(2, 3, 12),
		FormatLine(3, 27),
		FormatCount(5),
	} {
		if strings.ContainsAny(s, "()") {
			t.Errorf("counter helper produced parenthesized output %q -- contract forbids parens in titles", s)
		}
	}
}

// TestPaneTitle_Draw_LeftAndRight asserts the load-bearing layout
// rule: title at bounds.X+1, counter's last cell at bounds.X+Width-2.
// The contract test in cli/concepts/chrome_contract_test.go pins the
// rendered output of real panes; this test pins the widget's own
// guarantees against a synthetic 40-column bounds.
func TestPaneTitle_Draw_LeftAndRight(t *testing.T) {
	sim := newSimScreen(t, 40, 3)
	screen := NewScreenFromTcell(sim)
	bounds := Rect{X: 0, Y: 0, Width: 40, Height: 3}

	pt := PaneTitle{Title: "CONCEPTS", Counter: "1/77"}
	pt.Draw(screen, bounds, DefaultTheme())
	sim.Show()

	row := rowText(sim, 0, 0, 40)

	// Title flush left, one cell of pane gutter.
	if !strings.HasPrefix(row, " CONCEPTS") {
		t.Errorf("title not anchored at bounds.X+1; row=%q", row)
	}

	// Counter flush right, ending at column bounds.Width-2 (col 38),
	// then a single gutter space at col 39.
	if !strings.HasSuffix(strings.TrimRight(row, " "), "1/77") {
		t.Errorf("counter not anchored to the right; row=%q", row)
	}
	if row[len(row)-1] != ' ' {
		t.Errorf("expected right-edge gutter cell to be blank; row=%q", row)
	}
	// Last non-blank cell index must be 38 (Width-2).
	lastNonBlank := strings.LastIndexFunc(row, func(r rune) bool { return r != ' ' })
	if lastNonBlank != bounds.Width-2 {
		t.Errorf("counter's last cell at col %d, want col %d (bounds.X+Width-2)", lastNonBlank, bounds.Width-2)
	}

	// No parentheses anywhere in the row. The chrome contract is
	// explicit -- "no parentheses around the counter".
	if strings.ContainsAny(row, "()") {
		t.Errorf("title row contains parentheses; row=%q", row)
	}
}

// TestPaneTitle_Draw_DropsCounterWhenNarrow asserts the truncation
// rule: if title + 2-cell gap + counter doesn't fit, the counter is
// dropped and the title keeps the row.
func TestPaneTitle_Draw_DropsCounterWhenNarrow(t *testing.T) {
	// Inner width is bounds.Width-2 = 10. Title=8 + gap=2 +
	// counter=5 = 15 > 10. Counter must be dropped.
	sim := newSimScreen(t, 12, 3)
	screen := NewScreenFromTcell(sim)
	bounds := Rect{X: 0, Y: 0, Width: 12, Height: 3}

	pt := PaneTitle{Title: "CONCEPTS", Counter: "12/77"}
	pt.Draw(screen, bounds, DefaultTheme())
	sim.Show()

	row := rowText(sim, 0, 0, 12)
	if !strings.Contains(row, "CONCEPTS") {
		t.Errorf("title dropped when it should have stayed; row=%q", row)
	}
	if strings.Contains(row, "12/77") {
		t.Errorf("counter painted when it should have been dropped; row=%q", row)
	}
}

// TestPaneTitle_Draw_NoCounter asserts the bare-title path: no
// counter at all (e.g. SETTINGS, TOPOLOGY) just renders the title
// with no right-side content.
func TestPaneTitle_Draw_NoCounter(t *testing.T) {
	sim := newSimScreen(t, 40, 3)
	screen := NewScreenFromTcell(sim)
	bounds := Rect{X: 0, Y: 0, Width: 40, Height: 3}

	pt := PaneTitle{Title: "SETTINGS"}
	pt.Draw(screen, bounds, DefaultTheme())
	sim.Show()

	row := rowText(sim, 0, 0, 40)
	if !strings.HasPrefix(row, " SETTINGS") {
		t.Errorf("title not anchored at left; row=%q", row)
	}
	// Everything past the title is blank.
	if strings.TrimSpace(row) != "SETTINGS" {
		t.Errorf("bare-title row has extra content; row=%q", row)
	}
}

// TestPaneTitle_Draw_FocusedSwitchesStyle asserts the focus flip
// produces a different style. We don't pin the exact colors (theme-
// dependent) -- just that focused != unfocused for the title cells.
func TestPaneTitle_Draw_FocusedSwitchesStyle(t *testing.T) {
	theme := DefaultTheme()

	unfocused := drawTitleStyle(t, PaneTitle{Title: "AGENTS", Counter: "1/14"}, theme)
	focused := drawTitleStyle(t, PaneTitle{Title: "AGENTS", Counter: "1/14", Focused: true}, theme)

	if unfocused == focused {
		t.Errorf("focused and unfocused styles must differ; both = %v", focused)
	}
}

// TestPaneTitle_Draw_ZeroBounds is a no-op smoke test: a degenerate
// Rect (Width=0 / Height=0 / nil screen) must not panic.
func TestPaneTitle_Draw_ZeroBounds(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Draw panicked on zero bounds: %v", r)
		}
	}()
	pt := PaneTitle{Title: "X", Counter: "1/1"}
	pt.Draw(nil, Rect{}, DefaultTheme())

	sim := newSimScreen(t, 10, 1)
	pt.Draw(NewScreenFromTcell(sim), Rect{X: 0, Y: 0, Width: 0, Height: 0}, DefaultTheme())
	pt.Draw(NewScreenFromTcell(sim), Rect{X: 0, Y: 0, Width: 10, Height: 0}, DefaultTheme())
	pt.Draw(NewScreenFromTcell(sim), Rect{X: 0, Y: 0, Width: 2, Height: 1}, DefaultTheme()) // innerW=0
}

// TestPaneTitle_Draw_FilteredCounter end-to-ends a longer counter
// string ("3/3 filtered from 12") so the right-anchor logic gets
// tested with realistic input from FormatFiltered.
func TestPaneTitle_Draw_FilteredCounter(t *testing.T) {
	sim := newSimScreen(t, 60, 3)
	screen := NewScreenFromTcell(sim)
	bounds := Rect{X: 0, Y: 0, Width: 60, Height: 3}

	counter := FormatFiltered(2, 3, 12)
	if counter != "3/3 filtered from 12" {
		t.Fatalf("FormatFiltered guard tripped: %q", counter)
	}

	pt := PaneTitle{Title: "ROWS", Counter: counter}
	pt.Draw(screen, bounds, DefaultTheme())
	sim.Show()

	row := rowText(sim, 0, 0, 60)
	if !strings.HasPrefix(row, " ROWS") {
		t.Errorf("title not anchored at left; row=%q", row)
	}
	if !strings.HasSuffix(strings.TrimRight(row, " "), "3/3 filtered from 12") {
		t.Errorf("filtered counter not anchored to right; row=%q", row)
	}
}

// --- helpers --------------------------------------------------------

func newSimScreen(t *testing.T, w, h int) tcell.SimulationScreen {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim screen init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(w, h)
	sim.Clear()
	return sim
}

// drawTitleStyle paints a title and returns the tcell.Style applied
// to the first cell of the title text (column 1). Used to confirm
// the focus flag changes the rendered style without pinning the
// exact theme colors.
func drawTitleStyle(t *testing.T, pt PaneTitle, theme Theme) tcell.Style {
	t.Helper()
	sim := newSimScreen(t, 40, 3)
	pt.Draw(NewScreenFromTcell(sim), Rect{X: 0, Y: 0, Width: 40, Height: 3}, theme)
	sim.Show()
	_, _, style, _ := sim.GetContent(1, 0)
	return style
}
