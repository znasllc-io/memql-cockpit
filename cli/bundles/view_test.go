package bundles

// Regression tests for the Bundles tab. They pin the panel chrome
// contract (cli/CLAUDE.md) the same way cli/concepts/chrome_contract_test.go
// does -- driving the real Draw + HandleEvent paths against a
// tcell.NewSimulationScreen -- plus the two behaviors unique to this
// view: the detail pane renders a selected bundle's authored `.memql`
// source, and `X` exports that source to files.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

const (
	viewWidth  = 120
	viewHeight = 30
)

// makeView seeds a View with two bundles and the first bundle's authored
// constructs already cached (bypassing the live QueryClient).
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
	v.Mu.Lock()
	v.bundles = []map[string]any{
		{"id": "v1:authoring:bundle:aaa", "title": "Draft a refund reply", "status": "dryRunPassed", "version": float64(1), "sourcePlanId": "plan-123", "createdAt": "2026-06-08T10:00:00Z"},
		{"id": "v1:authoring:bundle:bbb", "title": "Weekly digest", "status": "validated", "version": float64(2), "sourcePlanId": "plan-456", "createdAt": "2026-06-07T10:00:00Z"},
	}
	v.constructs["v1:authoring:bundle:aaa"] = []map[string]any{
		{"id": "c1", "kind": "automation", "name": "draftRefundReply", "source": "automation draftRefundReply {\n  // headline\n}"},
		{"id": "c2", "kind": "spec", "name": "isRefundEscalation", "source": "spec isRefundEscalation {\n  payload.kind == \"refund\"\n}"},
	}
	v.bundleList.Count = len(v.bundles)
	v.bundleList.Selected = 0
	v.Mu.Unlock()

	return v, screen, sim, ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight}
}

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

// TestChrome_TitlesAndHints: left pane titled BUNDLES, right pane titled
// AUTHORED MQL, and the bottom chrome row carries `Key:Label` hints with
// no prose / no count-footer.
func TestChrome_TitlesAndHints(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	rows := drawAndSnapshot(v, screen, sim, bounds)

	title := rows[bounds.Y]
	if !strings.Contains(title, "BUNDLES") {
		t.Errorf("left pane title row missing BUNDLES: %q", title)
	}
	if !strings.Contains(title, "AUTHORED MQL") {
		t.Errorf("right pane title row missing AUTHORED MQL: %q", title)
	}

	chrome := rows[bounds.Y+bounds.Height-1]
	for _, want := range []string{"Move", "Export", "Refresh"} {
		if !strings.Contains(chrome, want) {
			t.Errorf("bottom chrome row missing %q hint: %q", want, chrome)
		}
	}
	if strings.Contains(strings.ToLower(chrome), "press ") {
		t.Errorf("hint band must use Key:Label grammar, not prose: %q", chrome)
	}
}

// TestDetail_RendersAuthoredSource: the detail pane shows the selected
// bundle's construct source (the headline "see the MQL" feature).
func TestDetail_RendersAuthoredSource(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	rows := drawAndSnapshot(v, screen, sim, bounds)
	joined := strings.Join(rows, "\n")

	for _, want := range []string{"draftRefundReply", "isRefundEscalation", "payload.kind"} {
		if !strings.Contains(joined, want) {
			t.Errorf("detail pane should render authored source containing %q", want)
		}
	}
}

// TestDetail_LoadingWhenConstructsAbsent: selecting a bundle whose
// constructs aren't cached yet shows a loading affordance, not a blank.
func TestDetail_LoadingWhenConstructsAbsent(t *testing.T) {
	v, screen, sim, bounds := makeView(t)
	v.Mu.Lock()
	v.bundleList.Selected = 1 // "bbb" has no cached constructs
	v.Mu.Unlock()
	rows := drawAndSnapshot(v, screen, sim, bounds)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "loading") {
		t.Errorf("detail for an unloaded bundle should show a loading affordance; got:\n%s", joined)
	}
}

// TestExport_WritesConstructFiles: X-export writes one .memql per
// construct plus a MANIFEST under ~/.memql/bundles/<bundleId>/.
func TestExport_WritesConstructFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	v, _, _, _ := makeView(t)
	v.exportSelected()

	dir := filepath.Join(home, ".memql", "bundles", "v1_authoring_bundle_aaa")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("export dir not created: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"automation__draftRefundReply.memql", "spec__isRefundEscalation.memql", "MANIFEST.txt"} {
		if !got[want] {
			t.Errorf("export missing file %q (got %v)", want, got)
		}
	}
	// The exported .memql must carry the real authored source.
	b, err := os.ReadFile(filepath.Join(dir, "automation__draftRefundReply.memql"))
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	if !strings.Contains(string(b), "automation draftRefundReply") {
		t.Errorf("exported file lost the authored source: %q", string(b))
	}
}

// TestSanitize keeps path-segment-safe ids and replaces the rest.
func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"v1:authoring:bundle:aaa": "v1_authoring_bundle_aaa",
		"draftRefundReply":        "draftRefundReply",
		"a/b\\c":                  "a_b_c",
		"":                        "_",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
