package dsledit

// #2163 tests: the owner-gated durable-DEMOTE (retire) action in authoring
// mode -- the role gate, the blast-radius confirm flow, the result
// formatting, the no-cluster gate, and the owner-only hint. Mirrors the
// promote tests (author_promote_test.go); reuses newTestAuthoring /
// openableBundle / keyRuneEv. The DurableDemoteBundle wire call needs a live
// engine, so the SDK owns it; here we cover the cockpit-side flow.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	authoringsdk "github.com/znasllc-io/memql/sdk/go/authoring"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

func keyCtrlD() *tcell.EventKey { return tcell.NewEventKey(tcell.KeyCtrlD, 0, tcell.ModNone) }

func TestShowDemoteResult(t *testing.T) {
	a, _ := newTestAuthoring(t)
	a.showDemoteResultLocked(&authoringsdk.DemoteResult{
		OK:      true,
		Demoted: []authoringsdk.Construct{{Name: "q1", Kind: "query"}},
	})
	if !a.resultShown || !strings.Contains(a.resultTitle, "OK") {
		t.Fatalf("demote OK: shown=%v title=%q", a.resultShown, a.resultTitle)
	}
	joined := ""
	for _, l := range a.resultLines {
		joined += l.text + "\n"
	}
	if !strings.Contains(joined, "cluster-wide") || !strings.Contains(joined, "q1 (query)") {
		t.Errorf("demote OK lines missing detail: %q", joined)
	}
	a.showDemoteResultLocked(&authoringsdk.DemoteResult{
		OK:    false,
		Error: "not allowed",
	})
	if !strings.Contains(a.resultTitle, "REJECTED") {
		t.Errorf("demote rejected title = %q", a.resultTitle)
	}
}

func TestDemoteGatedNoClient(t *testing.T) {
	a, status := newTestAuthoring(t)
	a.runDemote("b", "src") // nil authoring client
	if !strings.Contains(strings.Join(*status, " | "), "demote needs a connected cluster") {
		t.Errorf("demote gate message missing: %q", *status)
	}
	if a.resultShown {
		t.Error("no overlay should show when demote is gated on no client")
	}
}

func TestDemoteOwnerGate(t *testing.T) {
	a, status := newTestAuthoring(t)
	openableBundle(t, a)

	a.canPromote = false
	a.HandleEvent(keyCtrlD())
	if a.demoteConfirm {
		t.Fatal("non-owner Ctrl+D must NOT open the demote confirm")
	}
	if !strings.Contains(strings.Join(*status, " | "), "requires the owner role") {
		t.Errorf("owner-gate message missing: %q", *status)
	}

	a.canPromote = true
	a.HandleEvent(keyCtrlD())
	if !a.demoteConfirm || !a.resultShown {
		t.Fatalf("owner Ctrl+D should open the confirm; confirm=%v shown=%v", a.demoteConfirm, a.resultShown)
	}
	if !strings.Contains(a.resultTitle, "durable retire") {
		t.Errorf("confirm title = %q", a.resultTitle)
	}
}

func TestDemoteConfirmCancel(t *testing.T) {
	a, _ := newTestAuthoring(t)
	openableBundle(t, a)
	a.canPromote = true

	a.HandleEvent(keyCtrlD())
	if !a.demoteConfirm {
		t.Fatal("confirm should be open")
	}
	a.HandleEvent(keyRuneEv('n'))
	if a.demoteConfirm || a.resultShown {
		t.Errorf("N should cancel the confirm; confirm=%v shown=%v", a.demoteConfirm, a.resultShown)
	}

	a.HandleEvent(keyCtrlD())
	a.HandleEvent(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	if a.demoteConfirm {
		t.Error("Esc should cancel the confirm")
	}
}

func TestDemoteConfirmFiresClearsOverlay(t *testing.T) {
	a, _ := newTestAuthoring(t)
	openableBundle(t, a)
	a.canPromote = true

	a.HandleEvent(keyCtrlD())
	a.HandleEvent(keyRuneEv('Y')) // fire: clears confirm synchronously, then spawns runDemote
	if a.demoteConfirm || a.resultShown {
		t.Errorf("Y should clear the confirm before firing; confirm=%v shown=%v", a.demoteConfirm, a.resultShown)
	}
}

func TestDemoteHintOwnerOnly(t *testing.T) {
	a, _ := newTestAuthoring(t)
	a.focus = authorEditor

	a.canPromote = false
	if strings.Contains(a.hintsForEditor(), "Ctrl+D") {
		t.Error("non-owner editor hint must NOT advertise Ctrl+D:Retire")
	}
	a.canPromote = true
	if !strings.Contains(a.hintsForEditor(), "Ctrl+D:Retire") {
		t.Error("owner editor hint should advertise Ctrl+D:Retire")
	}
}

func TestDemoteConfirmOverlayRenders(t *testing.T) {
	a, _ := newTestAuthoring(t)
	openableBundle(t, a)
	a.canPromote = true
	a.HandleEvent(keyCtrlD())

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
	if !strings.Contains(joined, "durable retire") {
		t.Error("confirm overlay should render the durable-retire title")
	}
	if !strings.Contains(joined, "Y:Retire") {
		t.Error("confirm overlay should show the Y:Retire / N/Esc:Cancel hint")
	}
}
