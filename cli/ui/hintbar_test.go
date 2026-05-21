package ui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestHintBar_String pins the chip-composition grammar described in
// the Panel chrome contract. The contract is the source of truth;
// these cases are the regression net underneath it.
func TestHintBar_String(t *testing.T) {
	cases := []struct {
		name string
		bar  HintBar
		want string
	}{
		{
			name: "empty bar",
			bar:  HintBar{},
			want: "",
		},
		{
			name: "single chip",
			bar:  HintBar{Chips: []HintChip{{Key: "Enter", Label: "Open"}}},
			want: "Enter:Open",
		},
		{
			name: "multiple chips default separator (two spaces)",
			bar: HintBar{Chips: []HintChip{
				{Key: "↑/↓", Label: "Move"},
				{Key: "Tab", Label: "Cycle"},
				{Key: "Enter", Label: "Open"},
			}},
			want: "↑/↓:Move  Tab:Cycle  Enter:Open",
		},
		{
			name: "disabled chip is OMITTED, not dimmed",
			bar: HintBar{Chips: []HintChip{
				{Key: "A", Label: "Add"},
				{Key: "R", Label: "Retry", Disabled: true},
				{Key: "D", Label: "Del"},
			}},
			want: "A:Add  D:Del",
		},
		{
			name: "all chips disabled -> empty",
			bar: HintBar{Chips: []HintChip{
				{Key: "A", Label: "Add", Disabled: true},
				{Key: "D", Label: "Del", Disabled: true},
			}},
			want: "",
		},
		{
			name: "empty key renders as :Label (e.g. colon-search hint)",
			bar: HintBar{Chips: []HintChip{
				{Key: ":", Label: "search", Disabled: true}, // colon search trigger gone while typing
				{Key: "", Label: "Search"},
			}},
			want: ":Search",
		},
		{
			name: "custom separator",
			bar: HintBar{
				Chips: []HintChip{
					{Key: "A", Label: "Add"},
					{Key: "E", Label: "Edit"},
				},
				Separator: " | ",
			},
			want: "A:Add | E:Edit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.bar.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHintBar_Draw_RendersIntoBottomRow verifies the widget actually
// lands at the bottom of the given bounds via DrawBottom. Catches
// regressions where Draw silently drops content or paints into the
// wrong row -- the chrome contract is specific about anchoring.
func TestHintBar_Draw_RendersIntoBottomRow(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim screen init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(40, 10)

	screen := NewScreenFromTcell(sim)
	bar := HintBar{Chips: []HintChip{
		{Key: "Tab", Label: "Cycle"},
		{Key: "Q", Label: "Quit"},
	}}
	bounds := Rect{X: 0, Y: 0, Width: 40, Height: 10}
	bar.Draw(screen, bounds, DefaultTheme())
	sim.Sync()

	// The bar renders into the LAST row of bounds (y = 9), padded one
	// cell from the left (x = 1). Read row 9 and confirm the chip
	// string is present.
	got := rowText(sim, 0, 9, 40)
	want := "Tab:Cycle  Q:Quit"
	if !strings.Contains(got, want) {
		t.Errorf("bottom row = %q, missing %q", got, want)
	}

	// Sanity: row 8 (above the bar) should be blank. Confirms Draw
	// didn't bleed upward.
	above := strings.TrimSpace(rowText(sim, 0, 8, 40))
	if above != "" {
		t.Errorf("row above bar should be blank, got %q", above)
	}
}

// TestHintBar_Draw_EmptyBarIsNoOp confirms an empty bar paints
// nothing -- otherwise an "all chips disabled" state would leave a
// stale band on screen.
func TestHintBar_Draw_EmptyBarIsNoOp(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim screen init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(40, 5)

	screen := NewScreenFromTcell(sim)
	bar := HintBar{Chips: []HintChip{
		{Key: "A", Label: "Add", Disabled: true},
	}}
	bar.Draw(screen, Rect{X: 0, Y: 0, Width: 40, Height: 5}, DefaultTheme())
	sim.Sync()

	for y := 0; y < 5; y++ {
		if got := strings.TrimSpace(rowText(sim, 0, y, 40)); got != "" {
			t.Errorf("row %d should be blank for no-op bar, got %q", y, got)
		}
	}
}

// rowText extracts the printable characters of a row from the
// simulation screen. Returns the row with trailing spaces preserved
// so callers can detect padding.
func rowText(sim tcell.SimulationScreen, x, y, w int) string {
	var b strings.Builder
	for col := x; col < x+w; col++ {
		ch, _, _, _ := sim.GetContent(col, y)
		b.WriteRune(ch)
	}
	return b.String()
}
