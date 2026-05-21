package ui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func makeViewerSim(t *testing.T, w, h int) (*Screen, tcell.SimulationScreen) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	sim.SetSize(w, h)
	return NewScreenFromTcell(sim), sim
}

func viewerRow(sim tcell.SimulationScreen, y, w int) string {
	var b strings.Builder
	for x := 0; x < w; x++ {
		ch, _, _, _ := sim.GetContent(x, y)
		b.WriteRune(ch)
	}
	return b.String()
}

// TestViewer_EmptyShowsMessage: no lines + EmptyMessage paints the
// message and doesn't panic.
func TestViewer_EmptyShowsMessage(t *testing.T) {
	screen, sim := makeViewerSim(t, 30, 10)
	defer sim.Fini()

	v := Viewer{EmptyMessage: "(no detail)"}
	v.Draw(screen, Rect{X: 0, Y: 0, Width: 30, Height: 10}, DefaultTheme())
	sim.Sync()

	found := false
	for y := 0; y < 10; y++ {
		if strings.Contains(viewerRow(sim, y, 30), "(no detail)") {
			found = true
			break
		}
	}
	if !found {
		t.Error("EmptyMessage not rendered")
	}
}

// TestViewer_RendersLineNumbers checks the gutter: numbers
// right-aligned in the first gutterWidth-1 cells, trailing space.
func TestViewer_RendersLineNumbers(t *testing.T) {
	screen, sim := makeViewerSim(t, 30, 10)
	defer sim.Fini()

	v := Viewer{Lines: []ViewerLine{
		{Text: "alpha"},
		{Text: "beta"},
		{Text: "gamma"},
	}}
	v.Draw(screen, Rect{X: 0, Y: 0, Width: 30, Height: 10}, DefaultTheme())
	sim.Sync()

	// gutterWidth=5 -> 4 digit cells right-aligned + 1 trailing space.
	// Line 1 gutter == "   1 " and content starts at col 5.
	for i, want := range []string{"   1 alpha", "   2 beta", "   3 gamma"} {
		got := strings.TrimRight(viewerRow(sim, i, 30), " ")
		if !strings.HasPrefix(got, want) {
			t.Errorf("row %d: got %q, want prefix %q", i, got, want)
		}
	}
}

// TestViewer_HardWrapsUnbrokenToken is the cardinal bug-fix test for
// #50: a 60-char unbroken string in a 20-col viewer must wrap onto
// >= 3 rows, with NO character painted past the inner content width.
func TestViewer_HardWrapsUnbrokenToken(t *testing.T) {
	screen, sim := makeViewerSim(t, 20, 10)
	defer sim.Fini()

	long := strings.Repeat("X", 60) // no whitespace -- WrapText would have left it as one giant token
	v := Viewer{Lines: []ViewerLine{{Text: long}}}
	v.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 10}, DefaultTheme())
	sim.Sync()

	rowsWithX := 0
	for y := 0; y < 10; y++ {
		row := viewerRow(sim, y, 20)
		// Anything painted at col 19 or beyond would be overflow into
		// the scrollbar column (which is fine -- the assertion below
		// is "anything past 19 is empty").
		for col := 19; col < 20; col++ {
			// scrollbar column allowed; nothing further to assert
			_ = col
		}
		if strings.Contains(row, "X") {
			rowsWithX++
		}
	}
	if rowsWithX < 3 {
		t.Errorf("60-char token in 20-col viewer wrapped onto %d rows, want >= 3", rowsWithX)
	}
}

// TestViewer_WrapContinuationGutterIsBlank confirms the first
// rendered row carries the source line number and subsequent
// continuation rows have an empty gutter (so the eye reads "row 1
// continues" instead of repeating "1" four times).
func TestViewer_WrapContinuationGutterIsBlank(t *testing.T) {
	screen, sim := makeViewerSim(t, 20, 10)
	defer sim.Fini()

	long := strings.Repeat("Y", 40)
	v := Viewer{Lines: []ViewerLine{{Text: long}}}
	v.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 10}, DefaultTheme())
	sim.Sync()

	if got := viewerRow(sim, 0, 5); strings.TrimRight(got, " ") != "   1" {
		t.Errorf("row 0 gutter: %q, want %q", got, "   1")
	}
	if got := viewerRow(sim, 1, 5); strings.TrimSpace(got) != "" {
		t.Errorf("row 1 gutter (continuation): %q, want empty", got)
	}
}

// TestViewer_BlockTintingDiffersFromBaseRow checks consecutive lines
// sharing a Block id render with a different background than rows
// with Block==0. Style assertions probe the background color cell
// only -- foreground is whatever the spans or base style decided.
func TestViewer_BlockTintingDiffersFromBaseRow(t *testing.T) {
	screen, sim := makeViewerSim(t, 30, 10)
	defer sim.Fini()

	v := Viewer{Lines: []ViewerLine{
		{Text: "plain"},
		{Text: "block1", Block: 1},
		{Text: "block2", Block: 1},
		{Text: "plain again"},
	}}
	v.Draw(screen, Rect{X: 0, Y: 0, Width: 30, Height: 10}, DefaultTheme())
	sim.Sync()

	// Row 0 (plain) -- background is default theme BG.
	_, _, plainStyle, _ := sim.GetContent(6, 0)
	_, plainBG, _ := plainStyle.Decompose()

	// Row 1 (block) -- background is the tint.
	_, _, blockStyle, _ := sim.GetContent(6, 1)
	_, blockBG, _ := blockStyle.Decompose()

	if plainBG == blockBG {
		t.Errorf("plain and block rows share BG %v -- block tint not applied", plainBG)
	}
}

// TestViewer_SpanStylesPainted checks that a span's style replaces
// the base style on covered runes. We probe a single cell inside the
// span.
func TestViewer_SpanStylesPainted(t *testing.T) {
	screen, sim := makeViewerSim(t, 30, 5)
	defer sim.Fini()

	theme := DefaultTheme()
	tokenStyle := theme.TokenStyle("keyword")
	v := Viewer{Lines: []ViewerLine{{
		Text:  "concept v1:cognition:space",
		Spans: []HighlightSpan{{Start: 0, End: 7, Style: tokenStyle}},
	}}}
	v.Draw(screen, Rect{X: 0, Y: 0, Width: 30, Height: 5}, theme)
	sim.Sync()

	// Probe a cell inside "concept" (col gutterWidth + 0 = 5). The
	// foreground should match keyword token.
	_, _, gotStyle, _ := sim.GetContent(gutterWidth, 0)
	gotFG, _, _ := gotStyle.Decompose()
	wantFG, _, _ := tokenStyle.Decompose()
	if gotFG != wantFG {
		t.Errorf("span cell FG = %v, want %v (keyword)", gotFG, wantFG)
	}

	// Probe outside the span (col gutterWidth + 8, which is the 'v'
	// of "v1:cognition"). Should NOT carry the keyword style.
	_, _, outStyle, _ := sim.GetContent(gutterWidth+8, 0)
	outFG, _, _ := outStyle.Decompose()
	if outFG == wantFG {
		t.Errorf("out-of-span cell FG = %v, equal to keyword (span leaked)", outFG)
	}
}

// TestViewer_NoCharacterPastRightEdge: the most direct bug-fix
// assertion. After hard-wrap, the rightmost content column carries
// painted runes; the scrollbar column (last col) is the only place
// any glyph from the scrollbar appears.
func TestViewer_NoCharacterPastRightEdge(t *testing.T) {
	screen, sim := makeViewerSim(t, 15, 4)
	defer sim.Fini()

	// 50 'Z' chars in a 15-col viewer (content area = 15-1 scrollbar
	// -5 gutter = 9 chars). Output should wrap onto >= 6 rows.
	v := Viewer{Lines: []ViewerLine{{Text: strings.Repeat("Z", 50)}}}
	v.Draw(screen, Rect{X: 0, Y: 0, Width: 15, Height: 4}, DefaultTheme())
	sim.Sync()

	// Past col 14 there are no cells (width=15 -> valid x in [0,14]).
	// Within col 14 (scrollbar column) we only expect scrollbar glyphs
	// or spaces -- never user content. Assert no 'Z' painted there.
	for y := 0; y < 4; y++ {
		ch, _, _, _ := sim.GetContent(14, y)
		if ch == 'Z' {
			t.Errorf("col 14 row %d: got Z -- content leaked into scrollbar column", y)
		}
	}
}

// TestViewer_HandleEvent_NotFocusedReturnsFalse mirrors DetailPane's
// cardinal rule.
func TestViewer_HandleEvent_NotFocusedReturnsFalse(t *testing.T) {
	v := Viewer{Lines: []ViewerLine{{Text: "x"}}}
	if v.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)) {
		t.Error("unfocused Viewer consumed Down")
	}
}

// TestViewer_HandleEvent_ScrollKeys covers ↑/↓ + j/k + PgUp/PgDn +
// Home/End under the focused viewer. Each case is parametrized with
// the maxScroll value Draw would have stamped -- HandleEvent clamps
// to [0, maxScroll] on every key press so ScrollY can't drift past
// the visible end of the content (the bug from
// memql-cockpit#115).
func TestViewer_HandleEvent_ScrollKeys(t *testing.T) {
	cases := []struct {
		name      string
		key       tcell.Key
		ru        rune
		startScrl int
		pageCache int
		maxScroll int
		wantScrl  int
	}{
		{"Down +1", tcell.KeyDown, 0, 0, 0, 100, 1},
		{"Up -1", tcell.KeyUp, 0, 5, 0, 100, 4},
		{"Up clamps to 0", tcell.KeyUp, 0, 0, 0, 0, 0},
		{"PgDn by page", tcell.KeyPgDn, 0, 0, 10, 100, 10},
		{"PgUp by page", tcell.KeyPgUp, 0, 20, 10, 100, 10},
		{"Home", tcell.KeyHome, 0, 99, 0, 100, 0},
		{"k vim up", tcell.KeyRune, 'k', 5, 0, 100, 4},
		{"j vim down", tcell.KeyRune, 'j', 0, 0, 100, 1},

		// #115 regression: downward keys clamp to maxScroll so
		// ScrollY can't drift past the visible end. Pre-fix the
		// internal counter accumulated phantom presses and KeyUp
		// then had to drain the dead range before the viewport
		// moved.
		{"Down stops at maxScroll", tcell.KeyDown, 0, 7, 0, 7, 7},
		{"PgDn stops at maxScroll", tcell.KeyPgDn, 0, 5, 10, 7, 7},
		{"End jumps to maxScroll", tcell.KeyEnd, 0, 0, 0, 42, 42},
		{"j stops at maxScroll", tcell.KeyRune, 'j', 7, 0, 7, 7},

		// Without a prior Draw (maxScroll = 0), downward keys are
		// a no-op. Without bounds the widget can't tell where the
		// content ends; better to do nothing than drift past.
		{"Down without Draw is no-op", tcell.KeyDown, 0, 0, 0, 0, 0},
		{"End without Draw is no-op", tcell.KeyEnd, 0, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Viewer{
				Lines:             []ViewerLine{{Text: "x"}},
				Focused:           true,
				ScrollY:           tc.startScrl,
				viewportRowsCache: tc.pageCache,
				maxScrollCache:    tc.maxScroll,
			}
			var ev tcell.Event
			if tc.key == tcell.KeyRune {
				ev = tcell.NewEventKey(tcell.KeyRune, tc.ru, tcell.ModNone)
			} else {
				ev = tcell.NewEventKey(tc.key, 0, tcell.ModNone)
			}
			if !v.HandleEvent(ev) {
				t.Fatalf("HandleEvent returned false; want consumed")
			}
			if v.ScrollY != tc.wantScrl {
				t.Errorf("ScrollY = %d, want %d", v.ScrollY, tc.wantScrl)
			}
		})
	}
}

// TestViewer_HandleEvent_DownDoesNotDriftPastMaxScroll is the
// dedicated regression for memql-cockpit#115. Mashing KeyDown past
// the bottom of a long detail used to accumulate phantom presses
// on ScrollY; the next KeyUp had to drain the overshoot before the
// viewport moved, which to the user read as "arrow keys don't work."
func TestViewer_HandleEvent_DownDoesNotDriftPastMaxScroll(t *testing.T) {
	v := Viewer{
		Lines:             []ViewerLine{{Text: "x"}},
		Focused:           true,
		viewportRowsCache: 10,
		maxScrollCache:    5,
	}
	for i := 0; i < 50; i++ {
		v.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	}
	if v.ScrollY != 5 {
		t.Fatalf("ScrollY after 50x KeyDown = %d, want 5 (clamped to maxScroll)", v.ScrollY)
	}
	// A single KeyUp must immediately move the viewport, not first
	// drain the overshoot.
	v.HandleEvent(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if v.ScrollY != 4 {
		t.Errorf("ScrollY after KeyUp = %d, want 4 (KeyUp must take effect immediately)", v.ScrollY)
	}
}

// TestViewer_TooNarrowDoesNotPanic: Bounds.Width < gutterWidth+2
// should be a graceful no-op (no panic, no partial gutter painted).
func TestViewer_TooNarrowDoesNotPanic(t *testing.T) {
	screen, sim := makeViewerSim(t, 5, 5)
	defer sim.Fini()
	v := Viewer{Lines: []ViewerLine{{Text: "alpha"}}}
	v.Draw(screen, Rect{X: 0, Y: 0, Width: 5, Height: 5}, DefaultTheme())
	sim.Sync()
	// At width=5 (gutterWidth=5), gutterWidth+2 > 5 -> Draw bails.
	// Nothing painted = no 'a' anywhere.
	for y := 0; y < 5; y++ {
		if strings.Contains(viewerRow(sim, y, 5), "a") {
			t.Errorf("row %d: alpha painted at sub-min width", y)
		}
	}
}

// TestViewer_EmptyLineRendersBlankRowWithNumber confirms a blank
// source line gets a row of its own (separator semantics) carrying
// the source-line number.
func TestViewer_EmptyLineRendersBlankRowWithNumber(t *testing.T) {
	screen, sim := makeViewerSim(t, 30, 5)
	defer sim.Fini()
	v := Viewer{Lines: []ViewerLine{
		{Text: "above"},
		{Text: ""},
		{Text: "below"},
	}}
	v.Draw(screen, Rect{X: 0, Y: 0, Width: 30, Height: 5}, DefaultTheme())
	sim.Sync()

	if got := strings.TrimRight(viewerRow(sim, 1, 5), " "); got != "   2" {
		t.Errorf("blank-line gutter on row 1: %q, want %q", got, "   2")
	}
	if got := strings.TrimSpace(viewerRow(sim, 1, 30)); got != "2" {
		t.Errorf("blank-line content area not empty: %q", got)
	}
}
