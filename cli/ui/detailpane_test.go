package ui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func makeDetailSim(t *testing.T, w, h int) (*Screen, tcell.SimulationScreen) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	sim.SetSize(w, h)
	return NewScreenFromTcell(sim), sim
}

func detailRow(sim tcell.SimulationScreen, y, w int) string {
	var b strings.Builder
	for x := 0; x < w; x++ {
		ch, _, _, _ := sim.GetContent(x, y)
		b.WriteRune(ch)
	}
	return b.String()
}

// TestDetailPane_EmptyShowsMessage: zero lines + EmptyMessage paints
// the message; no panic.
func TestDetailPane_EmptyShowsMessage(t *testing.T) {
	screen, sim := makeDetailSim(t, 30, 10)
	defer sim.Fini()

	d := DetailPane{EmptyMessage: "(no detail)"}
	d.Draw(screen, Rect{X: 0, Y: 0, Width: 30, Height: 10}, DefaultTheme())
	sim.Sync()

	found := false
	for y := 0; y < 10; y++ {
		if strings.Contains(detailRow(sim, y, 30), "(no detail)") {
			found = true
			break
		}
	}
	if !found {
		t.Error("EmptyMessage not rendered")
	}
}

// TestDetailPane_RendersAllLineKinds confirms each DetailLineKind
// produces visible content (basic smoke; style assertions are too
// brittle for a unit test, but presence/absence is solid).
func TestDetailPane_RendersAllLineKinds(t *testing.T) {
	screen, sim := makeDetailSim(t, 40, 10)
	defer sim.Fini()

	d := DetailPane{
		Lines: []DetailLine{
			{Kind: LineHeader, Text: "PROVIDER"},
			{Kind: LineSection, Text: "─ identity ─"},
			{Kind: LinePlain, Text: "anthropic"},
			{Kind: LineDim, Text: "----"},
			{Kind: LineKV, Key: "Endpoint", Value: "localhost:9000"},
		},
	}
	d.Draw(screen, Rect{X: 0, Y: 0, Width: 40, Height: 10}, DefaultTheme())
	sim.Sync()

	wants := []string{"PROVIDER", "─ identity ─", "anthropic", "----", "Endpoint:", "localhost:9000"}
	for _, want := range wants {
		found := false
		for y := 0; y < 10; y++ {
			if strings.Contains(detailRow(sim, y, 40), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q somewhere in pane", want)
		}
	}
}

// TestDetailPane_LineSection_RendersSubtleBold pins the visual
// contract for LineSection -- the kind introduced in cockpit#93 so
// the planner / agents section markers don't pop in the accent
// color the way LineHeader does. Asserts the row's style matches
// theme.SubtleStyle().Bold(true) and is distinct from LineHeader's
// accent-bold.
func TestDetailPane_LineSection_RendersSubtleBold(t *testing.T) {
	screen, sim := makeDetailSim(t, 40, 5)
	defer sim.Fini()
	theme := DefaultTheme()

	d := DetailPane{Lines: []DetailLine{
		{Kind: LineSection, Text: "─ identity ─"},
		{Kind: LineHeader, Text: "─ accent ─"},
	}}
	d.Draw(screen, Rect{X: 0, Y: 0, Width: 40, Height: 5}, theme)
	sim.Sync()

	// Row 0 = LineSection. First non-space cell should carry the
	// subtle + bold style.
	sectionStyle := theme.SubtleStyle().Bold(true)
	headerStyle := theme.AccentStyle().Bold(true)

	gotRow0 := cellStyleAt(t, sim, 0, 0, 40)
	if gotRow0 != sectionStyle {
		t.Errorf("LineSection row style = %v, want SubtleStyle().Bold(true) %v", gotRow0, sectionStyle)
	}
	// Row 1 = LineHeader. Must differ from LineSection so the two
	// kinds are visually distinguishable.
	gotRow1 := cellStyleAt(t, sim, 1, 0, 40)
	if gotRow1 != headerStyle {
		t.Errorf("LineHeader row style = %v, want AccentStyle().Bold(true) %v", gotRow1, headerStyle)
	}
	if gotRow0 == gotRow1 {
		t.Errorf("LineSection and LineHeader rendered with the same style (%v); they must visually differ", gotRow0)
	}
}

// cellStyleAt returns the style of the first non-space cell in row
// y, scanning from startX up to totalW. Helper for style
// assertions; the cell's text content isn't inspected.
func cellStyleAt(t *testing.T, sim tcell.SimulationScreen, y, startX, totalW int) tcell.Style {
	t.Helper()
	for x := startX; x < totalW; x++ {
		ch, _, style, _ := sim.GetContent(x, y)
		if ch != ' ' && ch != 0 {
			return style
		}
	}
	t.Fatalf("row %d has no non-space cells", y)
	return tcell.Style{}
}

// TestDetailPane_WrapsLongLines: a single Plain line wider than the
// pane gets wrapped onto multiple rows via WrapText.
func TestDetailPane_WrapsLongLines(t *testing.T) {
	screen, sim := makeDetailSim(t, 20, 10)
	defer sim.Fini()

	// 50-char message in a 20-col pane (19 inner after scrollbar) -> >= 3 rows.
	long := "alpha beta gamma delta epsilon zeta eta theta"
	d := DetailPane{Lines: []DetailLine{{Kind: LinePlain, Text: long}}}
	d.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 10}, DefaultTheme())
	sim.Sync()

	rowsWithContent := 0
	for y := 0; y < 10; y++ {
		if strings.TrimSpace(detailRow(sim, y, 20)) != "" {
			rowsWithContent++
		}
	}
	if rowsWithContent < 2 {
		t.Errorf("expected wrap onto >= 2 rows, got %d", rowsWithContent)
	}
}

// TestDetailPane_KVRendersOnSingleRow: KV doesn't wrap; both
// segments land on the same row.
func TestDetailPane_KVRendersOnSingleRow(t *testing.T) {
	screen, sim := makeDetailSim(t, 40, 5)
	defer sim.Fini()

	d := DetailPane{Lines: []DetailLine{
		{Kind: LineKV, Key: "Endpoint", Value: "localhost:9000"},
	}}
	d.Draw(screen, Rect{X: 0, Y: 0, Width: 40, Height: 5}, DefaultTheme())
	sim.Sync()

	row0 := detailRow(sim, 0, 40)
	if !strings.Contains(row0, "Endpoint:") || !strings.Contains(row0, "localhost:9000") {
		t.Errorf("KV segments not on row 0: %q", row0)
	}
	if strings.TrimSpace(detailRow(sim, 1, 40)) != "" {
		t.Errorf("KV bled to row 1 (should be single-row)")
	}
}

// TestDetailPane_HandleEvent_NotFocusedReturnsFalse: cardinal rule.
func TestDetailPane_HandleEvent_NotFocusedReturnsFalse(t *testing.T) {
	d := DetailPane{Lines: []DetailLine{{Kind: LinePlain, Text: "x"}}}
	if d.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)) {
		t.Error("unfocused DetailPane consumed Down")
	}
}

// TestDetailPane_HandleEvent_ScrollKeys covers the focused-mode
// bindings: arrows, vim keys, PgUp/PgDn, Home/End.
func TestDetailPane_HandleEvent_ScrollKeys(t *testing.T) {
	cases := []struct {
		name       string
		key        tcell.Key
		ru         rune
		startScrl  int
		pageCache  int
		wantScrl   int
		wantNoClmp bool // true if want is the raw value BEFORE Draw clamping
	}{
		{name: "Down increments", key: tcell.KeyDown, startScrl: 0, wantScrl: 1, wantNoClmp: true},
		{name: "Up decrements", key: tcell.KeyUp, startScrl: 5, wantScrl: 4},
		{name: "Up clamps to 0", key: tcell.KeyUp, startScrl: 0, wantScrl: 0},
		{name: "PgUp jumps by viewport cache", key: tcell.KeyPgUp, startScrl: 30, pageCache: 10, wantScrl: 20},
		{name: "PgUp clamps to 0", key: tcell.KeyPgUp, startScrl: 5, pageCache: 10, wantScrl: 0},
		{name: "PgDn jumps by viewport cache", key: tcell.KeyPgDn, startScrl: 0, pageCache: 10, wantScrl: 10, wantNoClmp: true},
		{name: "Home jumps to 0", key: tcell.KeyHome, startScrl: 99, wantScrl: 0},
		{name: "k (vim up)", key: tcell.KeyRune, ru: 'k', startScrl: 5, wantScrl: 4},
		{name: "j (vim down)", key: tcell.KeyRune, ru: 'j', startScrl: 0, wantScrl: 1, wantNoClmp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DetailPane{
				Lines:             []DetailLine{{Kind: LinePlain, Text: "x"}},
				Focused:           true,
				ScrollY:           tc.startScrl,
				viewportRowsCache: tc.pageCache,
			}
			var ev tcell.Event
			if tc.key == tcell.KeyRune {
				ev = tcell.NewEventKey(tcell.KeyRune, tc.ru, tcell.ModNone)
			} else {
				ev = tcell.NewEventKey(tc.key, 0, tcell.ModNone)
			}
			if !d.HandleEvent(ev) {
				t.Errorf("HandleEvent returned false; want true (consumed)")
			}
			if d.ScrollY != tc.wantScrl {
				t.Errorf("ScrollY = %d, want %d", d.ScrollY, tc.wantScrl)
			}
		})
	}
}

// TestDetailPane_HandleEvent_UnknownKeysReturnFalse: parent should
// still see keys we don't handle.
func TestDetailPane_HandleEvent_UnknownKeysReturnFalse(t *testing.T) {
	d := DetailPane{Lines: []DetailLine{{Kind: LinePlain, Text: "x"}}, Focused: true}
	cases := []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModNone),
		tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone),
		tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone),
		tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone),
	}
	for _, ev := range cases {
		if d.HandleEvent(ev) {
			t.Errorf("unexpected consumption of %v", ev)
		}
	}
}

// TestDetailPane_Draw_ClampsScrollOnOverflow: setting ScrollY past
// the last row should snap back to maxScroll on Draw.
func TestDetailPane_Draw_ClampsScrollOnOverflow(t *testing.T) {
	screen, sim := makeDetailSim(t, 20, 5)
	defer sim.Fini()

	lines := make([]DetailLine, 10)
	for i := range lines {
		lines[i] = DetailLine{Kind: LinePlain, Text: "row"}
	}
	d := DetailPane{Lines: lines, ScrollY: 999}
	d.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 5}, DefaultTheme())
	// 10 rows, viewport 5 -> maxScroll = 5.
	if d.ScrollY != 5 {
		t.Errorf("ScrollY = %d, want 5 (clamped)", d.ScrollY)
	}
}

// TestDetailPane_ScrollbarVisibleOnlyWhenOverflow: same chrome
// discipline as ListPane.
func TestDetailPane_ScrollbarVisibleOnlyWhenOverflow(t *testing.T) {
	cases := []struct {
		name          string
		nLines        int
		boundsH       int
		wantScrollbar bool
	}{
		{"fits", 3, 10, false},
		{"overflows", 30, 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			screen, sim := makeDetailSim(t, 20, tc.boundsH)
			defer sim.Fini()
			lines := make([]DetailLine, tc.nLines)
			for i := range lines {
				lines[i] = DetailLine{Kind: LinePlain, Text: "r"}
			}
			d := DetailPane{Lines: lines}
			d.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: tc.boundsH}, DefaultTheme())
			sim.Sync()

			has := false
			for y := 0; y < tc.boundsH; y++ {
				ch, _, _, _ := sim.GetContent(19, y)
				if ch == '│' || ch == '■' {
					has = true
					break
				}
			}
			if has != tc.wantScrollbar {
				t.Errorf("scrollbar = %v, want %v", has, tc.wantScrollbar)
			}
		})
	}
}

// TestDetailPane_HandleEvent_EndScrollsToBottomAfterDraw uses a real
// Draw cycle to verify End → max scroll, since End uses Draw's
// clamping to find the bottom.
func TestDetailPane_HandleEvent_EndScrollsToBottomAfterDraw(t *testing.T) {
	screen, sim := makeDetailSim(t, 20, 5)
	defer sim.Fini()
	lines := make([]DetailLine, 12)
	for i := range lines {
		lines[i] = DetailLine{Kind: LinePlain, Text: "r"}
	}
	d := DetailPane{Lines: lines, Focused: true}
	d.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 5}, DefaultTheme())
	d.HandleEvent(tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone))
	d.Draw(screen, Rect{X: 0, Y: 0, Width: 20, Height: 5}, DefaultTheme())
	if d.ScrollY != 7 {
		t.Errorf("after End: ScrollY = %d, want 7 (12 rows - 5 viewport)", d.ScrollY)
	}
}
