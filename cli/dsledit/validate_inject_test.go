package dsledit

// C3 (#231) tests: bundle source concatenation, the Validate/Inject
// result formatting + overlay, and the no-cluster gate. The actual
// ValidateBundle / SessionDefineBundle wire calls need a live engine,
// so the SDK owns those; here we cover the cockpit-side formatting +
// flow against constructed results.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	authoringsdk "github.com/znasllc-io/memql/sdk/go/authoring"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

func TestBundleSourcesConcatenates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := createBundle("b"); err != nil {
		t.Fatalf("createBundle: %v", err)
	}
	if _, err := createBundleFile("b", "a"); err != nil {
		t.Fatalf("createBundleFile a: %v", err)
	}
	if _, err := createBundleFile("b", "z"); err != nil {
		t.Fatalf("createBundleFile z: %v", err)
	}
	if err := writeBundleFile("b", "a.memql", "query a {}"); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := writeBundleFile("b", "z.memql", "query z {}"); err != nil {
		t.Fatalf("write z: %v", err)
	}
	got, err := bundleSources("b")
	if err != nil {
		t.Fatalf("bundleSources: %v", err)
	}
	// Files are listed sorted (a before z), blank-line separated.
	if got != "query a {}\n\nquery z {}" {
		t.Fatalf("bundleSources = %q", got)
	}
}

func TestDiagnosticLine(t *testing.T) {
	ok := diagnosticLine(authoringsdk.Diagnostic{Name: "foo", Kind: "query", OK: true})
	if ok.sev != sevOK || !strings.HasPrefix(ok.text, "[ok] foo (query)") {
		t.Errorf("ok diagnostic = %+v", ok)
	}
	skip := diagnosticLine(authoringsdk.Diagnostic{Name: "bar", Kind: "automation", Skipped: true})
	if skip.sev != sevWarn || !strings.Contains(skip.text, "skipped") {
		t.Errorf("skipped diagnostic = %+v", skip)
	}
	fail := diagnosticLine(authoringsdk.Diagnostic{Name: "baz", Kind: "spec", Error: "line 3: bad"})
	if fail.sev != sevErr || !strings.Contains(fail.text, "line 3: bad") {
		t.Errorf("failed diagnostic = %+v", fail)
	}
}

func newTestAuthoring(t *testing.T) (*authoring, *[]string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var status []string
	a := newAuthoring(ui.DefaultTheme(), nilSense, nilAuthoring, nonOwnerRole, func(m string) { status = append(status, m) }, func() {})
	return a, &status
}

func TestShowValidateResult(t *testing.T) {
	a, _ := newTestAuthoring(t)
	a.showValidateResultLocked(&authoringsdk.ValidateResult{
		OK: false,
		Diagnostics: []authoringsdk.Diagnostic{
			{Name: "ok1", Kind: "query", OK: true},
			{Name: "bad1", Kind: "spec", Error: "boom"},
		},
	})
	if !a.resultShown {
		t.Fatal("resultShown should be true after a validate result")
	}
	if !strings.Contains(a.resultTitle, "FAILED") {
		t.Errorf("title = %q, want it to mention FAILED", a.resultTitle)
	}
	if len(a.resultLines) != 2 {
		t.Fatalf("want 2 result lines, got %d", len(a.resultLines))
	}
}

func TestShowInjectResult(t *testing.T) {
	a, _ := newTestAuthoring(t)
	// Success path.
	a.showInjectResultLocked(&authoringsdk.SessionDefineResult{
		OK:      true,
		Defined: []authoringsdk.Construct{{Name: "q1", Kind: "query"}},
	})
	if !a.resultShown || !strings.Contains(a.resultTitle, "OK") {
		t.Fatalf("inject OK: shown=%v title=%q", a.resultShown, a.resultTitle)
	}
	joined := ""
	for _, l := range a.resultLines {
		joined += l.text + "\n"
	}
	if !strings.Contains(joined, "never shadows core") || !strings.Contains(joined, "q1 (query)") {
		t.Errorf("inject OK lines missing detail: %q", joined)
	}

	// Rejected path.
	a.showInjectResultLocked(&authoringsdk.SessionDefineResult{
		OK:    false,
		Error: "validation failed",
		Diagnostics: []authoringsdk.Diagnostic{
			{Name: "bad", Kind: "query", Error: "nope"},
		},
	})
	if !strings.Contains(a.resultTitle, "REJECTED") {
		t.Errorf("inject rejected title = %q", a.resultTitle)
	}
}

func TestValidateInjectGatedNoClient(t *testing.T) {
	a, status := newTestAuthoring(t)
	a.runValidate("b", "src") // nil authoring client
	a.runInject("b", "src")
	joined := strings.Join(*status, " | ")
	if !strings.Contains(joined, "validate needs a connected cluster") {
		t.Errorf("validate gate message missing: %q", joined)
	}
	if !strings.Contains(joined, "inject needs a connected cluster") {
		t.Errorf("inject gate message missing: %q", joined)
	}
	if a.resultShown {
		t.Error("no overlay should show when the action is gated")
	}
}

func TestResultsOverlayRendersAndDismiss(t *testing.T) {
	a, _ := newTestAuthoring(t)
	a.resultShown = true
	a.resultTitle = "VALIDATE -- Gate-1: OK"
	a.resultLines = []resultEntry{{"[ok] myQuery (query)", sevOK}}

	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(viewWidth, viewHeight)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)
	a.Draw(screen, ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight})
	sim.Show()

	joined := strings.Join(flattenSim(sim), "")
	if !strings.Contains(joined, "Gate-1: OK") || !strings.Contains(joined, "myQuery") {
		t.Errorf("results overlay not rendered: title/line missing")
	}

	// Esc dismisses the overlay.
	a.HandleEvent(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	if a.resultShown {
		t.Error("Esc should dismiss the results overlay")
	}
}
