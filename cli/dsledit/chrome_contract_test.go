package dsledit

// Chrome-contract + behavior tests for the Editor tab's read-only pack
// browser (memql-cockpit#228). Mirrors cli/cluster/chrome_contract_test.go
// and cli/concepts/chrome_contract_test.go: render the view against a
// tcell SimulationScreen and assert the pane titles + the bottom hint
// band follow the Panel chrome contract in cli/CLAUDE.md.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/sdk/go/pack"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

const (
	viewWidth  = 100
	viewHeight = 24
)

func flattenSim(sim tcell.SimulationScreen) []string {
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

// assertChips splits a hint string on the canonical two-space chip
// separator and checks each chip is a `Key:Label` with no space after
// the colon.
func assertChips(t *testing.T, where, hint string) {
	t.Helper()
	hint = strings.TrimSpace(hint)
	if hint == "" {
		t.Fatalf("%s: hint band empty -- every pane must advertise at least one action", where)
	}
	for _, chip := range strings.Split(hint, "  ") {
		chip = strings.TrimSpace(chip)
		if chip == "" {
			continue
		}
		if !strings.Contains(chip, ":") {
			t.Errorf("%s: chip %q missing colon (contract: Key:Label)", where, chip)
			continue
		}
		idx := strings.Index(chip, ":")
		if idx+1 < len(chip) && chip[idx+1] == ' ' {
			t.Errorf("%s: chip %q has a space after the colon", where, chip)
		}
	}
}

func seededView() *View {
	v := NewView(ui.DefaultTheme())
	v.SetDomains([]pack.Domain{
		{Name: "cognition", Origin: "embedded", FileCount: 9},
		{Name: "copresent", Origin: "pack:copresent", FileCount: 4},
	})
	return v
}

func render(t *testing.T) (*View, []string) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(viewWidth, viewHeight)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)

	v := seededView()
	bounds := ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight}
	v.Draw(screen, bounds)
	sim.Show()
	return v, flattenSim(sim)
}

// TestEditorPaneTitles asserts the three pane titles render on the top row.
func TestEditorPaneTitles(t *testing.T) {
	_, rows := render(t)
	top := rows[0]
	for _, want := range []string{"DOMAINS", "FILES", "SOURCE"} {
		if !strings.Contains(top, want) {
			t.Errorf("top row missing pane title %q; got %q", want, top)
		}
	}
}

// TestEditorChromeBand asserts the bottom row carries valid action-hint
// chips (the domains pane is populated, so it shows the full hint set).
func TestEditorChromeBand(t *testing.T) {
	_, rows := render(t)
	bottom := rows[viewHeight-1]
	if !strings.Contains(bottom, "Enter:Open") {
		t.Errorf("bottom band missing the domains Enter:Open hint; got %q", bottom)
	}
	if !strings.Contains(bottom, "Tab:Cycle") {
		t.Errorf("bottom band missing Tab:Cycle; got %q", bottom)
	}
}

// TestEditorHintFormat checks every pane's hint helper emits contract-
// valid Key:Label chips.
func TestEditorHintFormat(t *testing.T) {
	v := seededView()
	v.files = []pack.File{{Path: "queries.memql", Size: 10}}
	v.viewer.Lines = []ui.ViewerLine{{Text: "query foo {}"}}
	assertChips(t, "domains", v.hintsForDomains())
	assertChips(t, "files", v.hintsForFiles())
	assertChips(t, "source", v.hintsForSource())
}

// TestEditorFocusCycle asserts Tab cycles focus Domains -> Files ->
// Source -> Domains.
func TestEditorFocusCycle(t *testing.T) {
	v := seededView()
	if v.Focus != FocusDomains {
		t.Fatalf("initial focus = %v, want FocusDomains", v.Focus)
	}
	tab := tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone)
	want := []FocusPane{FocusFiles, FocusSource, FocusDomains}
	for i, w := range want {
		if !v.HandleEvent(tab) {
			t.Fatalf("Tab #%d not consumed", i)
		}
		if v.Focus != w {
			t.Fatalf("after Tab #%d focus = %v, want %v", i, v.Focus, w)
		}
	}
}

// TestEditorGated renders the gated placeholder when no cluster is
// connected.
func TestEditorGated(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(viewWidth, viewHeight)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)

	v := NewView(ui.DefaultTheme())
	v.GatedMessage = "No cluster selected."
	v.Draw(screen, ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight})
	sim.Show()

	joined := strings.Join(flattenSim(sim), "")
	if !strings.Contains(joined, "No cluster selected.") {
		t.Errorf("gated placeholder not rendered")
	}
}

// TestPlainLines covers the raw-source splitter: trailing newline
// dropped, empty source yields no lines.
func TestPlainLines(t *testing.T) {
	if got := plainLines(""); got != nil {
		t.Errorf("empty source -> %v, want nil", got)
	}
	got := plainLines("a\nb\n")
	if len(got) != 2 || got[0].Text != "a" || got[1].Text != "b" {
		t.Fatalf("plainLines(\"a\\nb\\n\") = %+v, want [a b]", got)
	}
}
