package dsledit

// B3 (#229) tests: Sense coloring of the SOURCE pane, the read-only
// hover cursor navigation, and the hover overlay render.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/sdk/go/sense"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// tok builds a single-line Sense token spanning [startCol,endCol)
// (1-indexed columns) on a 1-indexed line.
func tok(typ string, line, startCol, endCol int) sense.Token {
	return sense.Token{
		Type: typ,
		Range: sense.Range{
			Start: sense.Position{Line: line, Column: startCol},
			End:   sense.Position{Line: line, Column: endCol},
		},
	}
}

// TestColoredLinesMapsTokens checks token ranges land on the right
// viewer line as 0-based rune spans.
func TestColoredLinesMapsTokens(t *testing.T) {
	src := "query foo\nshape bar"
	tokens := []sense.Token{
		tok("keyword", 1, 1, 6),     // "query" on line 1, cols 1..5
		tok("identifier", 2, 7, 10), // "bar" on line 2, cols 7..9
	}
	lines := coloredLines(src, tokens, ui.DefaultTheme())
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	if len(lines[0].Spans) != 1 || lines[0].Spans[0].Start != 0 || lines[0].Spans[0].End != 5 {
		t.Fatalf("line 1 spans = %+v, want one span [0,5)", lines[0].Spans)
	}
	if len(lines[1].Spans) != 1 || lines[1].Spans[0].Start != 6 || lines[1].Spans[0].End != 9 {
		t.Fatalf("line 2 spans = %+v, want one span [6,9)", lines[1].Spans)
	}
}

// TestColoredLinesNoTokensIsPlain verifies coloring degrades to plain
// text when Sense returns nothing.
func TestColoredLinesNoTokensIsPlain(t *testing.T) {
	lines := coloredLines("a\nb\n", nil, ui.DefaultTheme())
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (trailing newline dropped), got %d", len(lines))
	}
	for i, ln := range lines {
		if ln.Spans != nil {
			t.Errorf("line %d should have no spans, got %+v", i, ln.Spans)
		}
	}
}

// TestSourceCursorNavigation drives the SOURCE pane's hover cursor with
// Up/Down/Left/Right and checks clamping.
func TestSourceCursorNavigation(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.Focus = FocusSource
	v.srcText = "a\nbbbb\ncc\nd\ne"
	v.viewer.Lines = coloredLines(v.srcText, nil, v.Theme)

	down := tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	up := tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	right := tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone)

	if !v.HandleEvent(down) || v.cursorLine != 1 {
		t.Fatalf("after Down cursorLine = %d, want 1", v.cursorLine)
	}
	// Down past the end clamps at the last line (index 4).
	for i := 0; i < 10; i++ {
		v.HandleEvent(down)
	}
	if v.cursorLine != 4 {
		t.Fatalf("cursorLine after many Downs = %d, want 4 (clamped)", v.cursorLine)
	}
	v.HandleEvent(up)
	if v.cursorLine != 3 {
		t.Fatalf("cursorLine after Up = %d, want 3", v.cursorLine)
	}
	// On line "d" (index 3, length 1) Right clamps cursorCol to 1.
	for i := 0; i < 5; i++ {
		v.HandleEvent(right)
	}
	if v.cursorCol != 1 {
		t.Fatalf("cursorCol after many Rights on a 1-rune line = %d, want 1", v.cursorCol)
	}
}

// TestSourceCursorHidesHoverOnMove verifies moving the cursor dismisses
// an open hover.
func TestSourceCursorHidesHoverOnMove(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.Focus = FocusSource
	v.srcText = "a\nb"
	v.viewer.Lines = coloredLines(v.srcText, nil, v.Theme)
	v.hoverShown = true
	v.hoverText = "info"
	v.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if v.hoverShown {
		t.Fatal("moving the cursor should dismiss the hover overlay")
	}
}

// TestHoverOverlayRenders draws the hover overlay and asserts the
// content lands on screen.
func TestHoverOverlayRenders(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(viewWidth, viewHeight)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)

	v := seededView()
	v.Focus = FocusSource
	v.srcText = "query foo"
	v.viewer.Lines = coloredLines(v.srcText, nil, v.Theme)
	v.hoverShown = true
	v.hoverText = "**query** keyword"

	v.Draw(screen, ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight})
	sim.Show()
	joined := strings.Join(flattenSim(sim), "")
	if !strings.Contains(joined, "query keyword") {
		t.Errorf("hover overlay content not rendered (markdown should be stripped)")
	}
}

// TestHoverLinesStripsMarkdown checks the markdown stripper + wrap.
func TestHoverLinesStripsMarkdown(t *testing.T) {
	got := hoverLines("**bold** `code`\n### head", 80)
	joined := strings.Join(got, "\n")
	for _, m := range []string{"**", "`", "###"} {
		if strings.Contains(joined, m) {
			t.Errorf("hoverLines left markdown %q in %q", m, joined)
		}
	}
	if !strings.Contains(joined, "bold") || !strings.Contains(joined, "head") {
		t.Errorf("hoverLines dropped content: %q", joined)
	}
}
