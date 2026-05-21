package concepts

// Regression tests for the panel chrome contract documented in
// cli/CLAUDE.md ("Panel chrome contract"). The contract has three
// load-bearing rules; this file pins down all three with assertions
// that fail loudly if a future refactor regresses any of them:
//
//  1. Search renders in the bottom chrome band, NEVER as a strip
//     below the pane title.
//  2. Search is invoked by `:` (colon), NOT `/`. `/` is reserved and
//     must be a no-op for the Concepts view today.
//  3. Action hints follow the `Key:Label` grammar with two-space
//     separators -- no `Key: Label`, no `Press X to Y` prose, no
//     trailing count-footer rows.
//
// We drive the production Draw + HandleEvent paths against a
// tcell.NewSimulationScreen so the assertions are about what the
// user actually sees, not what the code "intends to render".

import (
	"regexp"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// boundsForView is the rect a `concepts.View` paints into during the
// tests. Wide + tall enough that none of the action-hint strips have
// to wrap; the contract assertions look at specific rows, so a stable
// geometry matters.
const (
	viewWidth  = 120
	viewHeight = 30
)

// makeView seeds a `concepts.View` with enough state that Draw paints
// non-empty content in all three panes. The test fixture deliberately
// bypasses the real QueryClient (which would need a live gRPC stream)
// and instead writes directly to Rows / rowMatches under the package
// lock -- mirroring what refreshRowsFromCurrentLocked would do after a
// successful Execute.
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
	v.SetConcepts([]*memqlv1.ConceptInfo{
		{Id: "v1:platform:policyTrace"},
		{Id: "v1:cognition:agent"},
		{Id: "v1:hr:milestone"},
	})

	v.Mu.Lock()
	v.Rows = []map[string]any{
		{"id": "row-alpha", "payload": map[string]any{"name": "alpha"}},
		{"id": "row-beta", "payload": map[string]any{"name": "beta"}},
		{"id": "row-gamma", "payload": map[string]any{"name": "gamma"}},
	}
	v.rowMatches = []int{0, 1, 2}
	v.rowList.Selected = 0
	v.rowList.Count = len(v.rowMatches)
	v.refreshDetailFromCurrentLocked()
	v.Mu.Unlock()

	return v, screen, sim, ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight}
}

// drawAndSnapshot renders the view and returns the rendered cells as
// a `[]string` keyed by row index. Tests assert on individual rows
// (top row = title, bottom row = chrome band).
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

// TestPanelChrome_SearchLivesInBottomBand asserts contract rule #1:
// the search prompt + active query render in the LAST row of the
// pane, never on the row directly under the title. Pre-refactor the
// concepts view painted "/search: " on row Y+1 of the Rows pane, which
// is exactly what the rule forbids.
func TestPanelChrome_SearchLivesInBottomBand(t *testing.T) {
	v, screen, sim, bounds := makeView(t)

	// Default state -- no search active. Row directly under the title
	// must be content (or blank), never the literal string "search".
	rows := drawAndSnapshot(v, screen, sim, bounds)
	titleAdjacent := rows[bounds.Y+1]
	if strings.Contains(strings.ToLower(titleAdjacent), "search") {
		t.Errorf("row below title leaks search affordance:\n  row=%q\nsearch must live in the bottom chrome band, not below the title", titleAdjacent)
	}

	// Activate search. Bottom chrome row of the Rows pane (which lives
	// in the middle column) must contain the `:search` prompt; the
	// row-under-title must still NOT contain it.
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, ':', tcell.ModNone))
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 'a', tcell.ModNone))
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 'l', tcell.ModNone))

	rows = drawAndSnapshot(v, screen, sim, bounds)

	chromeRow := rows[bounds.Y+bounds.Height-1]
	if !strings.Contains(chromeRow, ":search ") {
		t.Errorf("bottom chrome row missing active search prompt:\n  chrome=%q", chromeRow)
	}
	if !strings.Contains(chromeRow, "al") {
		t.Errorf("bottom chrome row missing typed query 'al':\n  chrome=%q", chromeRow)
	}

	// Re-assert rule #1 with search active: still no search strip
	// under the title.
	titleAdjacent = rows[bounds.Y+1]
	if strings.Contains(strings.ToLower(titleAdjacent), "search") {
		t.Errorf("row below title still leaks search affordance while search is active:\n  row=%q", titleAdjacent)
	}
}

// TestPanelChrome_ColonTriggersSearch_SlashDoesNot asserts contract
// rule #2: the search invocation is `:`, not `/`. Pressing `/` must
// leave searchOn false; pressing `:` must flip it true.
func TestPanelChrome_ColonTriggersSearch_SlashDoesNot(t *testing.T) {
	v, _, _, _ := makeView(t)

	// `/` is reserved; today it must NOT trigger search.
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, '/', tcell.ModNone))
	v.Mu.RLock()
	wasOn := v.searchOn
	v.Mu.RUnlock()
	if wasOn {
		t.Fatalf("pressing '/' activated search; the contract requires ':' as the trigger and reserves '/'")
	}

	// `:` is the canonical trigger.
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, ':', tcell.ModNone))
	v.Mu.RLock()
	wasOn = v.searchOn
	v.Mu.RUnlock()
	if !wasOn {
		t.Fatalf("pressing ':' did not activate search; the chrome contract advertises ':Search' as the bottom-band hint")
	}
}

// hintChip matches the canonical `Key:Label` chip. Key is a possibly-
// empty whitespace-free run, Label is one or more whitespace-free
// runs that may contain `/` or `↑/↓`. The Key part is allowed to be
// empty so that `:Search` -- where the trigger key IS `:` -- is
// accepted by the same grammar (the colon plays both roles: the
// key and the separator).
//
// Examples that MATCH:
//
//	A:Add
//	Enter:Select
//	↑/↓:Move
//	:Search                (key IS `:`)
//	PgUp/PgDn:Page
//
// Anti-examples (must NOT match):
//
//	Enter: Save            (space after colon)
//	Press X to Y           (prose, no colon)
//	A: add                 (space after colon)
var hintChip = regexp.MustCompile(`^\S*:\S+$`)

// TestPanelChrome_HintFormat asserts contract rule #3: every chip in
// the chrome band parses as `Key:Label` with two-space separators.
// Walks the action hint strings produced by the per-pane builders.
func TestPanelChrome_HintFormat(t *testing.T) {
	v, _, _, _ := makeView(t)

	v.Mu.RLock()
	defer v.Mu.RUnlock()

	cases := []struct {
		name string
		hint paneHint
	}{
		{"concepts pane", hintsForConcepts(v)},
		{"rows pane (default)", hintsForRows(v)},
		{"detail pane (snapshot)", hintsForDetail(v)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.hint.text
			if text == "" {
				t.Fatalf("empty hint text for %s", tc.name)
			}
			chips := strings.Split(text, "  ")
			for _, chip := range chips {
				if chip == "" {
					t.Errorf("empty chip in hint %q -- likely a triple-space separator", text)
					continue
				}
				if !strings.Contains(chip, ":") {
					t.Errorf("chip %q missing colon separator (contract: Key:Label)", chip)
					continue
				}
				// Anti-pattern: `Key: Label` (space after colon).
				idx := strings.Index(chip, ":")
				if idx >= 0 && idx+1 < len(chip) && chip[idx+1] == ' ' {
					t.Errorf("chip %q has space after colon (contract: `Key:Label` no space)", chip)
				}
				if !hintChip.MatchString(chip) {
					t.Errorf("chip %q does not match canonical Key:Label shape", chip)
				}
			}
			// Single-space separators between chips are a bug: they
			// would visually merge into adjacent labels. The contract
			// is two spaces, so reject anything that looks like a
			// single-space-joined neighbor that wasn't already split.
			if strings.Contains(text, " : ") {
				t.Errorf("hint %q contains ' : ' -- chip separator must be exactly two spaces", text)
			}
		})
	}
}

// TestPanelChrome_ActiveSearchHintIsAccent asserts the cosmetic-but-
// important rule that the bottom band swaps to accent style while the
// user is typing into the search box. Without the swap, the prompt
// reads identical to the regular hint strip and there's no signal
// that input is being captured.
func TestPanelChrome_ActiveSearchHintIsAccent(t *testing.T) {
	v, _, _, _ := makeView(t)
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, ':', tcell.ModNone))

	v.Mu.RLock()
	defer v.Mu.RUnlock()
	hint := hintsForRows(v)
	if !hint.accent {
		t.Fatalf("active search prompt must render in accent style; got subtle")
	}
	if !strings.HasPrefix(hint.text, ":search ") {
		t.Fatalf("active search prompt must start with ':search '; got %q", hint.text)
	}
	if !strings.HasSuffix(hint.text, "_") {
		t.Fatalf("active search prompt must end with cursor '_'; got %q", hint.text)
	}
}

// TestPanelChrome_EscClearsFilter asserts the `Esc:Clear search` chip
// is wired up -- pressing Esc on the Rows pane with a filter active
// must reset rowFilter so the user can get back to the full list
// without re-entering the search box.
func TestPanelChrome_EscClearsFilter(t *testing.T) {
	v, _, _, _ := makeView(t)

	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, ':', tcell.ModNone))
	v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 'a', tcell.ModNone))
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone)) // exit search input
	v.Mu.RLock()
	if v.rowFilter != "a" {
		v.Mu.RUnlock()
		t.Fatalf("expected filter 'a' after typing into search; got %q", v.rowFilter)
	}
	v.Mu.RUnlock()

	// Esc on Rows pane with filter set must clear it. (`Esc:ClearSearch`.)
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	v.Mu.RLock()
	defer v.Mu.RUnlock()
	if v.rowFilter != "" {
		t.Fatalf("Esc on Rows pane with active filter must clear it; rowFilter still %q", v.rowFilter)
	}
}

// TestPanelChrome_NoCountFooterRow asserts the title-bar carries the
// pane's count/position, and that we do NOT also paint a `<n>/<m>`
// strip in the LAST row. Before the refactor every pane stacked a
// subtle count footer at bounds.Height-1, which collided with the
// chrome band and pushed action hints up a row. The contract is:
// count goes in the title, last row is the chrome band only.
func TestPanelChrome_NoCountFooterRow(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	rows := drawAndSnapshot(v, screen, sim, bounds)

	titleRow := rows[bounds.Y]
	if !strings.Contains(titleRow, "1/3") && !strings.Contains(titleRow, "1/") {
		t.Errorf("title row should carry the count (e.g. '1/3'); got %q", titleRow)
	}

	// Footer scan: a literal `n/m` count somewhere on the last row
	// would mean we still paint the duplicate count footer. The
	// chrome band only contains `Key:Label` chips, which never look
	// like `digit/digit`.
	chromeRow := rows[bounds.Y+bounds.Height-1]
	dupCount := regexp.MustCompile(` \d+/\d+ `)
	if dupCount.MatchString(chromeRow) {
		t.Errorf("chrome row appears to still contain a count footer: %q", chromeRow)
	}
}
