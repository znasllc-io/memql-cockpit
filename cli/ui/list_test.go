package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// makeListSim spins up a SimulationScreen of the given size and
// returns a ui.Screen wrapper plus the inner sim screen for content
// assertions. Tests are responsible for calling sim.Fini.
func makeListSim(t *testing.T, w, h int) (*Screen, tcell.SimulationScreen) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	sim.SetSize(w, h)
	return NewScreenFromTcell(sim), sim
}

// stringRow extracts the printable chars from a row of the
// simulation screen.
func stringRow(sim tcell.SimulationScreen, y, w int) string {
	var b strings.Builder
	for x := 0; x < w; x++ {
		ch, _, _, _ := sim.GetContent(x, y)
		b.WriteRune(ch)
	}
	return b.String()
}

// TestListPane_EmptyShowsMessage confirms EmptyMessage renders when
// Count == 0 and nothing else paints.
func TestListPane_EmptyShowsMessage(t *testing.T) {
	screen, sim := makeListSim(t, 30, 10)
	defer sim.Fini()

	l := ListPane{Count: 0, EmptyMessage: "(no rows)"}
	l.Draw(screen, Rect{X: 0, Y: 0, Width: 30, Height: 10}, DefaultTheme())
	sim.Sync()

	// EmptyMessage lands somewhere in the bounds.
	found := false
	for y := 0; y < 10; y++ {
		if strings.Contains(stringRow(sim, y, 30), "(no rows)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("EmptyMessage not rendered")
	}
}

// TestListPane_RendersAllVisibleItems_1Row checks the basic 1-row
// path: every visible item gets a renderer call with the right idx,
// and the row's y matches the iteration index.
func TestListPane_RendersAllVisibleItems_1Row(t *testing.T) {
	screen, sim := makeListSim(t, 30, 10)
	defer sim.Fini()

	items := []string{"alpha", "beta", "gamma"}
	l := ListPane{
		Count: len(items),
		Render: func(s *Screen, b Rect, idx int, sel bool, theme Theme) {
			s.DrawText(b.X+1, b.Y, b.Width-1, items[idx], theme.BaseStyle())
		},
	}
	l.Draw(screen, Rect{X: 0, Y: 0, Width: 30, Height: 10}, DefaultTheme())
	sim.Sync()

	for i, want := range items {
		if !strings.Contains(stringRow(sim, i, 30), want) {
			t.Errorf("row %d: missing %q in %q", i, want, stringRow(sim, i, 30))
		}
	}
}

// TestListPane_RendersAllVisibleItems_2Row verifies 2-rows-per-item
// math: each item gets a 2-row Rect, the renderer can paint both
// rows, and items are spaced correctly.
func TestListPane_RendersAllVisibleItems_2Row(t *testing.T) {
	screen, sim := makeListSim(t, 40, 10)
	defer sim.Fini()

	items := []struct{ a, b string }{
		{"plan-1", "queued"},
		{"plan-2", "running"},
		{"plan-3", "done"},
	}
	l := ListPane{
		Count:       len(items),
		RowsPerItem: 2,
		Render: func(s *Screen, bnd Rect, idx int, sel bool, theme Theme) {
			s.DrawText(bnd.X+1, bnd.Y, bnd.Width-1, items[idx].a, theme.BaseStyle())
			s.DrawText(bnd.X+3, bnd.Y+1, bnd.Width-3, items[idx].b, theme.SubtleStyle())
		},
	}
	l.Draw(screen, Rect{X: 0, Y: 0, Width: 40, Height: 10}, DefaultTheme())
	sim.Sync()

	// Item 0 occupies rows 0 (primary) + 1 (subtitle).
	// Item 1 occupies rows 2 + 3. Item 2 occupies rows 4 + 5.
	cases := []struct {
		y    int
		want string
	}{
		{0, "plan-1"}, {1, "queued"},
		{2, "plan-2"}, {3, "running"},
		{4, "plan-3"}, {5, "done"},
	}
	for _, c := range cases {
		if !strings.Contains(stringRow(sim, c.y, 40), c.want) {
			t.Errorf("row %d: missing %q in %q", c.y, c.want, stringRow(sim, c.y, 40))
		}
	}
}

// TestListPane_SelectedClampedToBounds confirms an out-of-range
// Selected gets pulled into [0, Count) on Draw so callers can pre-
// set Selected without worrying about Count.
func TestListPane_SelectedClampedToBounds(t *testing.T) {
	screen, sim := makeListSim(t, 20, 5)
	defer sim.Fini()
	l := ListPane{
		Count:    3,
		Selected: 99, // way out of range
		Render:   func(*Screen, Rect, int, bool, Theme) {},
	}
	l.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 5}, DefaultTheme())
	if l.Selected != 2 {
		t.Errorf("Selected = %d, want 2 (clamped to Count-1)", l.Selected)
	}

	l.Selected = -5
	l.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 5}, DefaultTheme())
	if l.Selected != 0 {
		t.Errorf("Selected = %d, want 0 (clamped to 0)", l.Selected)
	}
}

// TestListPane_EnsureVisible verifies ScrollTo wiring: jumping
// Selected far past the viewport adjusts ScrollY so it's visible.
func TestListPane_EnsureVisible_ScrollsDownWhenSelectionPastViewport(t *testing.T) {
	l := ListPane{Count: 50, Selected: 30, ScrollY: 0}
	l.EnsureVisible(10) // viewport = 10 items
	// After EnsureVisible, item 30 must be inside [ScrollY, ScrollY+10).
	if l.Selected < l.ScrollY || l.Selected >= l.ScrollY+10 {
		t.Errorf("Selected=%d not in viewport [%d, %d)", l.Selected, l.ScrollY, l.ScrollY+10)
	}
}

func TestListPane_EnsureVisible_NoOpWhenAlreadyVisible(t *testing.T) {
	l := ListPane{Count: 50, Selected: 5, ScrollY: 0}
	l.EnsureVisible(10)
	if l.ScrollY != 0 {
		t.Errorf("ScrollY = %d, want 0 (no-op)", l.ScrollY)
	}
}

// TestListPane_HandleEvent_NotFocusedReturnsFalse pins the cardinal
// rule: a non-focused list never consumes a key.
func TestListPane_HandleEvent_NotFocusedReturnsFalse(t *testing.T) {
	l := ListPane{Count: 5, Focused: false}
	ev := tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	if l.HandleEvent(ev) {
		t.Errorf("unfocused ListPane consumed Up; should return false")
	}
}

// TestListPane_HandleEvent_FocusedArrowKeysMove confirms arrow keys
// shift Selected and consume the event.
func TestListPane_HandleEvent_FocusedArrowKeysMove(t *testing.T) {
	cases := []struct {
		name      string
		key       tcell.Key
		startSel  int
		wantSel   int
		startVPC  int
		count     int
		ru        rune
	}{
		{name: "Up", key: tcell.KeyUp, startSel: 3, wantSel: 2, count: 10},
		{name: "Down", key: tcell.KeyDown, startSel: 3, wantSel: 4, count: 10},
		{name: "Up clamps at 0", key: tcell.KeyUp, startSel: 0, wantSel: 0, count: 10},
		{name: "Down clamps at Count-1", key: tcell.KeyDown, startSel: 9, wantSel: 9, count: 10},
		{name: "PgUp jumps by viewport cache", key: tcell.KeyPgUp, startSel: 8, wantSel: 3, startVPC: 5, count: 10},
		{name: "PgDn jumps by viewport cache", key: tcell.KeyPgDn, startSel: 0, wantSel: 5, startVPC: 5, count: 10},
		{name: "Home jumps to 0", key: tcell.KeyHome, startSel: 7, wantSel: 0, count: 10},
		{name: "End jumps to Count-1", key: tcell.KeyEnd, startSel: 2, wantSel: 9, count: 10},
		{name: "k (vim up)", key: tcell.KeyRune, ru: 'k', startSel: 5, wantSel: 4, count: 10},
		{name: "j (vim down)", key: tcell.KeyRune, ru: 'j', startSel: 5, wantSel: 6, count: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := ListPane{
				Count:              tc.count,
				Selected:           tc.startSel,
				Focused:            true,
				viewportItemsCache: tc.startVPC,
			}
			var ev tcell.Event
			if tc.key == tcell.KeyRune {
				ev = tcell.NewEventKey(tcell.KeyRune, tc.ru, tcell.ModNone)
			} else {
				ev = tcell.NewEventKey(tc.key, 0, tcell.ModNone)
			}
			if !l.HandleEvent(ev) {
				t.Errorf("HandleEvent returned false; want true (consumed)")
			}
			if l.Selected != tc.wantSel {
				t.Errorf("Selected = %d, want %d", l.Selected, tc.wantSel)
			}
		})
	}
}

// TestListPane_HandleEvent_UnknownKeysReturnFalse confirms keys
// outside the binding set don't get silently swallowed -- the
// parent can still route them.
func TestListPane_HandleEvent_UnknownKeysReturnFalse(t *testing.T) {
	l := ListPane{Count: 5, Focused: true}
	cases := []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModNone),
		tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone),
		tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone),
		tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone),
	}
	for _, ev := range cases {
		if l.HandleEvent(ev) {
			t.Errorf("unexpected consumption of %v", ev)
		}
	}
}

// TestListPane_HandleEvent_EmptyListReturnsFalse: when Count is 0,
// navigation is meaningless; events flow through to the parent.
func TestListPane_HandleEvent_EmptyListReturnsFalse(t *testing.T) {
	l := ListPane{Count: 0, Focused: true}
	if l.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)) {
		t.Errorf("empty list consumed Down; should return false so parent can route")
	}
}

// TestListPane_ScrollbarVisibleOnlyWhenOverflow pins the chrome:
// no scrollbar when everything fits; one column reserved when it
// doesn't.
func TestListPane_ScrollbarVisibleOnlyWhenOverflow(t *testing.T) {
	cases := []struct {
		name        string
		count       int
		boundsH     int
		wantScrollbar bool
	}{
		{"fits exactly", 5, 5, false},
		{"under-fills", 3, 10, false},
		{"overflows", 20, 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			screen, sim := makeListSim(t, 20, tc.boundsH)
			defer sim.Fini()
			l := ListPane{
				Count: tc.count,
				Render: func(s *Screen, b Rect, idx int, sel bool, theme Theme) {
					s.DrawText(b.X+1, b.Y, b.Width-1, fmt.Sprintf("item-%d", idx), theme.BaseStyle())
				},
			}
			l.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: tc.boundsH}, DefaultTheme())
			sim.Sync()

			// Scrollbar paints exactly one '■' thumb into column W-1.
			// No track glyphs -- the rest of the column is background
			// per memql-cockpit#111.
			hasScrollbar := false
			thumbCount := 0
			trackGlyphs := 0
			for y := 0; y < tc.boundsH; y++ {
				ch, _, _, _ := sim.GetContent(19, y)
				if ch == '■' {
					hasScrollbar = true
					thumbCount++
				}
				if ch == '│' {
					trackGlyphs++
				}
			}
			if hasScrollbar != tc.wantScrollbar {
				t.Errorf("scrollbar visible = %v, want %v", hasScrollbar, tc.wantScrollbar)
			}
			if tc.wantScrollbar && thumbCount != 1 {
				t.Errorf("scrollbar drew %d thumb cells, want exactly 1", thumbCount)
			}
			if trackGlyphs != 0 {
				t.Errorf("scrollbar column has %d track '│' glyphs; want 0 (no track lines)", trackGlyphs)
			}
		})
	}
}

// TestListPane_HandleEvent_DownPastViewportScrolls verifies that
// arrowing past the visible window auto-scrolls via Draw's
// EnsureVisible -- the user shouldn't be able to lose their cursor.
func TestListPane_HandleEvent_DownPastViewportScrolls(t *testing.T) {
	screen, sim := makeListSim(t, 20, 5)
	defer sim.Fini()
	l := ListPane{
		Count:   20,
		Focused: true,
		Render: func(s *Screen, b Rect, idx int, sel bool, theme Theme) {
			s.DrawText(b.X, b.Y, b.Width, fmt.Sprintf("r%d", idx), theme.BaseStyle())
		},
	}
	// Draw once to populate viewportItemsCache (5 items fit).
	l.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 5}, DefaultTheme())
	// Press Down 8 times so Selected goes past the initial viewport.
	for i := 0; i < 8; i++ {
		l.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	}
	l.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 5}, DefaultTheme())

	if l.Selected != 8 {
		t.Fatalf("Selected = %d, want 8", l.Selected)
	}
	// Item 8 must now be inside the rendered window [ScrollY, ScrollY+5).
	if l.Selected < l.ScrollY || l.Selected >= l.ScrollY+5 {
		t.Errorf("Selected %d not in viewport [%d, %d)", l.Selected, l.ScrollY, l.ScrollY+5)
	}
}

// TestListPane_ScrollbarThumbReachesBottomAtLastSelection is the
// regression net for memql-cockpit#111. Pre-fix the scrollbar
// passed (ScrollY, Count) -- item-units for both, but the widget
// internally compared Count against bounds.Height (row-units).
// For a 2-rows-per-item list with Count=77 and viewportItems=15,
// the thumb maxed out at ScrollY=47 even though the actual scroll
// range went to ScrollY=62, then sat pinned at the bottom for the
// remaining 15 items. Switched to passing (Selected, Count-1) so
// the thumb tracks the user's cursor with units that line up
// end-to-end.
func TestListPane_ScrollbarThumbReachesBottomAtLastSelection(t *testing.T) {
	// 77-item list, 2 rows per item, 30 rows of bounds -> 15 items
	// visible. Without the fix the thumb saturated well before the
	// last item.
	const W = 30
	const H = 30
	screen, sim := makeListSim(t, W, H)
	defer sim.Fini()

	l := ListPane{
		Count:       77,
		RowsPerItem: 2,
		Selected:    76, // last item
		Render: func(s *Screen, b Rect, idx int, sel bool, theme Theme) {
			s.DrawText(b.X+1, b.Y, b.Width-1, fmt.Sprintf("item-%d", idx), theme.BaseStyle())
		},
	}
	l.Draw(screen, Rect{X: 0, Y: 0, Width: W, Height: H}, DefaultTheme())
	sim.Sync()

	// Thumb at the LAST row of the scrollbar column.
	scrollbarCol := W - 1
	lastRow := H - 1
	ch, _, _, _ := sim.GetContent(scrollbarCol, lastRow)
	if ch != '■' {
		t.Errorf("thumb not at bottom of scrollbar when Selected=Count-1 (col %d, row %d): got %q",
			scrollbarCol, lastRow, string(ch))

		// Diagnostic: find where the thumb actually landed.
		for y := 0; y < H; y++ {
			c, _, _, _ := sim.GetContent(scrollbarCol, y)
			if c == '■' {
				t.Logf("thumb landed at row %d (last row is %d)", y, lastRow)
				break
			}
		}
	}

	// Sanity: thumb at the TOP when Selected=0.
	l.Selected = 0
	sim.Clear()
	l.Draw(screen, Rect{X: 0, Y: 0, Width: W, Height: H}, DefaultTheme())
	sim.Sync()
	ch, _, _, _ = sim.GetContent(scrollbarCol, 0)
	if ch != '■' {
		t.Errorf("thumb not at top of scrollbar when Selected=0: got %q at (col %d, row 0)",
			string(ch), scrollbarCol)
	}
}

// TestListPane_RendererSeesSelectedFlag confirms the `selected`
// argument is true for exactly one row (the highlighted one) and
// false for the others.
func TestListPane_RendererSeesSelectedFlag(t *testing.T) {
	screen, sim := makeListSim(t, 20, 5)
	defer sim.Fini()
	selFlags := make(map[int]bool)
	l := ListPane{
		Count:    4,
		Selected: 2,
		Render: func(s *Screen, b Rect, idx int, sel bool, theme Theme) {
			selFlags[idx] = sel
		},
	}
	l.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 5}, DefaultTheme())
	for i, got := range selFlags {
		want := i == 2
		if got != want {
			t.Errorf("idx %d selected = %v, want %v", i, got, want)
		}
	}
}
