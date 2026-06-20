package ui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// rowString reads w cells of row y from the simulation screen.
func rowString(sim tcell.SimulationScreen, y, w int) string {
	var b strings.Builder
	for x := 0; x < w; x++ {
		ch, _, _, _ := sim.GetContent(x, y)
		b.WriteRune(ch)
	}
	return b.String()
}

// TestDrawInputValue_FocusedScrollsToTail is the regression test for
// memql-cockpit#193: a focused field whose value is longer than the box
// must window to the END of the value (so the caret + tail stay visible),
// not render the start and hide everything the user just typed/pasted.
func TestDrawInputValue_FocusedScrollsToTail(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(40, 4)
	screen := NewScreenFromTcell(sim)

	const boxX, boxW, y = 0, 12, 0 // inner = 10 cells
	value := "https://identity.example.com/.well-known/openid-configuration"
	st := tcell.StyleDefault
	DrawInputValue(screen, boxX, y, boxW, value, true, st, st.Background(tcell.ColorWhite))
	sim.Sync()

	row := rowString(sim, y, boxW)
	// inner=10, one cell reserved for the caret -> 9 tail glyphs of the
	// value, then the caret block. The value ends in "configuration".
	if !strings.Contains(row, "guration") {
		t.Fatalf("focused field did not scroll to the tail; row=%q want it to contain the value's end", row)
	}
	if strings.Contains(row, "https") {
		t.Fatalf("focused field rendered the head instead of the tail; row=%q", row)
	}
	// Caret cell: the last INNER cell (boxX+boxW-2; boxX+boxW-1 is the
	// box's right edge) carries the caret background since the value
	// overflows.
	_, _, caretStyle, _ := sim.GetContent(boxX+boxW-2, y)
	if fg, bg, _ := caretStyle.Decompose(); bg != tcell.ColorWhite {
		t.Fatalf("caret block not at right inset cell: fg=%v bg=%v", fg, bg)
	}
}

// TestDrawInputValue_ShortValueShowsCaretAfterText: a value that fits
// renders in full with the caret immediately after the last glyph.
func TestDrawInputValue_ShortValueShowsCaretAfterText(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(40, 4)
	screen := NewScreenFromTcell(sim)

	const boxX, boxW, y = 0, 12, 0 // inner = 10
	st := tcell.StyleDefault
	DrawInputValue(screen, boxX, y, boxW, "abc", true, st, st.Background(tcell.ColorWhite))
	sim.Sync()

	if got := rowString(sim, y, boxW); !strings.HasPrefix(got, " abc") {
		t.Fatalf("short value not rendered from the start: row=%q", got)
	}
	// Caret right after "abc": boxX+1 (inset) + 3 (len) = boxX+4.
	_, _, caretStyle, _ := sim.GetContent(boxX+4, y)
	if _, bg, _ := caretStyle.Decompose(); bg != tcell.ColorWhite {
		t.Fatalf("caret not positioned after the value end")
	}
}

// TestDrawInputValue_UnfocusedShowsHead: a field the user isn't editing
// renders from the start (the readable default) and draws no caret.
func TestDrawInputValue_UnfocusedShowsHead(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(40, 4)
	screen := NewScreenFromTcell(sim)

	const boxX, boxW, y = 0, 12, 0
	st := tcell.StyleDefault
	value := "https://identity.example.com/very/long/path"
	DrawInputValue(screen, boxX, y, boxW, value, false, st, st.Background(tcell.ColorWhite))
	sim.Sync()

	if got := rowString(sim, y, boxW); !strings.Contains(got, "https") {
		t.Fatalf("unfocused field should show the head; row=%q", got)
	}
	// No caret block anywhere in the box.
	for x := boxX; x < boxX+boxW; x++ {
		if _, _, stl, _ := sim.GetContent(x, y); func() bool { _, bg, _ := stl.Decompose(); return bg == tcell.ColorWhite }() {
			t.Fatalf("unfocused field drew a caret at x=%d", x)
		}
	}
}
