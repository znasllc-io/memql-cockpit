package editor

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// TestCursorScreenAfterDraw verifies the editor records the absolute
// screen cell of its text cursor during Draw, so a host can anchor
// completion/hover popups there (memql-cockpit#259).
func TestCursorScreenAfterDraw(t *testing.T) {
	// Before any Draw: not on screen.
	e := NewEditor(NewBuffer("hello world", "f.memql", false), ui.DefaultTheme())
	if _, _, ok := e.CursorScreen(); ok {
		t.Fatal("CursorScreen should report ok=false before the first Draw")
	}

	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(40, 10)
	screen := ui.NewScreenFromTcell(sim)

	e.CursorLine = 0
	e.CursorCol = 3
	e.Draw(screen, ui.Rect{X: 0, Y: 0, Width: 40, Height: 10})

	x, y, ok := e.CursorScreen()
	if !ok {
		t.Fatal("cursor should be on screen after Draw")
	}
	// textAreaX = bounds.X + gutterWidth (5); cursorX = textAreaX + col.
	if x != gutterWidth+3 || y != 0 {
		t.Fatalf("cursor screen = (%d,%d), want (%d,0)", x, y, gutterWidth+3)
	}
}
