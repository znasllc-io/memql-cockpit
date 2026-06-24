package dsledit

// C4 (#232) tests: the owner-gated durable-activation (Promote) action in
// authoring mode -- the role gate, the blast-radius confirm flow, the result
// formatting, the no-cluster gate, and the owner-only hint. The actual
// DurablePromoteBundle wire call (+ its server-side owner gate) needs a live
// engine, so the SDK owns it; here we cover the cockpit-side flow against
// constructed results.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	authoringsdk "github.com/znasllc-io/memql/sdk/go/authoring"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

func keyCtrlP() *tcell.EventKey { return tcell.NewEventKey(tcell.KeyCtrlP, 0, tcell.ModNone) }
func keyRuneEv(r rune) *tcell.EventKey {
	return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone)
}

// openableBundle creates a one-file bundle and marks it open on the
// authoring view so gatherForActionLocked succeeds.
func openableBundle(t *testing.T, a *authoring) {
	t.Helper()
	if err := createBundle("b"); err != nil {
		t.Fatalf("createBundle: %v", err)
	}
	if _, err := createBundleFile("b", "a"); err != nil {
		t.Fatalf("createBundleFile: %v", err)
	}
	if err := writeBundleFile("b", "a.memql", "query a {}"); err != nil {
		t.Fatalf("write: %v", err)
	}
	a.openBundle = "b"
}

func TestShowPromoteResult(t *testing.T) {
	a, _ := newTestAuthoring(t)
	// Success path.
	a.showPromoteResultLocked(&authoringsdk.PromoteResult{
		OK:       true,
		Promoted: []authoringsdk.Construct{{Name: "q1", Kind: "query"}},
	})
	if !a.resultShown || !strings.Contains(a.resultTitle, "OK") {
		t.Fatalf("promote OK: shown=%v title=%q", a.resultShown, a.resultTitle)
	}
	joined := ""
	for _, l := range a.resultLines {
		joined += l.text + "\n"
	}
	if !strings.Contains(joined, "survives restarts") || !strings.Contains(joined, "q1 (query)") {
		t.Errorf("promote OK lines missing detail: %q", joined)
	}
	// Rejected path.
	a.showPromoteResultLocked(&authoringsdk.PromoteResult{
		OK:          false,
		Error:       "not allowed",
		Diagnostics: []authoringsdk.Diagnostic{{Name: "bad", Kind: "query", Error: "nope"}},
	})
	if !strings.Contains(a.resultTitle, "REJECTED") {
		t.Errorf("promote rejected title = %q", a.resultTitle)
	}
}

func TestPromoteGatedNoClient(t *testing.T) {
	a, status := newTestAuthoring(t)
	a.runPromote("b", "src") // nil authoring client
	if !strings.Contains(strings.Join(*status, " | "), "promote needs a connected cluster") {
		t.Errorf("promote gate message missing: %q", *status)
	}
	if a.resultShown {
		t.Error("no overlay should show when promote is gated on no client")
	}
}

func TestPromoteOwnerGate(t *testing.T) {
	a, status := newTestAuthoring(t)
	openableBundle(t, a)

	// Non-owner: Ctrl+P is refused with a message, no confirm overlay.
	a.canPromote = false
	a.HandleEvent(keyCtrlP())
	if a.promoteConfirm {
		t.Fatal("non-owner Ctrl+P must NOT open the promote confirm")
	}
	if !strings.Contains(strings.Join(*status, " | "), "requires the owner role") {
		t.Errorf("owner-gate message missing: %q", *status)
	}

	// Owner: Ctrl+P opens the blast-radius confirm.
	a.canPromote = true
	a.HandleEvent(keyCtrlP())
	if !a.promoteConfirm || !a.resultShown {
		t.Fatalf("owner Ctrl+P should open the confirm; confirm=%v shown=%v", a.promoteConfirm, a.resultShown)
	}
	if !strings.Contains(a.resultTitle, "durable activation") {
		t.Errorf("confirm title = %q", a.resultTitle)
	}
}

func TestPromoteConfirmCancel(t *testing.T) {
	a, _ := newTestAuthoring(t)
	openableBundle(t, a)
	a.canPromote = true

	a.HandleEvent(keyCtrlP()) // open confirm
	if !a.promoteConfirm {
		t.Fatal("confirm should be open")
	}
	// 'N' cancels: no fire, overlay cleared.
	a.HandleEvent(keyRuneEv('n'))
	if a.promoteConfirm || a.resultShown {
		t.Errorf("N should cancel the confirm; confirm=%v shown=%v", a.promoteConfirm, a.resultShown)
	}

	// Re-open, Esc also cancels.
	a.HandleEvent(keyCtrlP())
	a.HandleEvent(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	if a.promoteConfirm {
		t.Error("Esc should cancel the confirm")
	}
}

func TestPromoteConfirmFiresClearsOverlay(t *testing.T) {
	a, _ := newTestAuthoring(t)
	openableBundle(t, a)
	a.canPromote = true

	a.HandleEvent(keyCtrlP())     // open confirm
	a.HandleEvent(keyRuneEv('Y')) // fire: clears the confirm synchronously, then spawns runPromote
	// 'Y' clears the confirm + overlay synchronously before the action runs;
	// the action's own outcome (no-client gate, result overlay) is covered by
	// TestPromoteGatedNoClient / TestShowPromoteResult.
	if a.promoteConfirm || a.resultShown {
		t.Errorf("Y should clear the confirm before firing; confirm=%v shown=%v", a.promoteConfirm, a.resultShown)
	}
}

func TestPromoteHintOwnerOnly(t *testing.T) {
	a, _ := newTestAuthoring(t)
	a.focus = authorEditor

	a.canPromote = false
	if strings.Contains(a.hintsForEditor(), "Ctrl+P") {
		t.Error("non-owner editor hint must NOT advertise Ctrl+P:Promote")
	}
	a.canPromote = true
	if !strings.Contains(a.hintsForEditor(), "Ctrl+P:Promote") {
		t.Error("owner editor hint should advertise Ctrl+P:Promote")
	}
}

func TestPromoteConfirmOverlayRenders(t *testing.T) {
	a, _ := newTestAuthoring(t)
	openableBundle(t, a)
	a.canPromote = true
	a.HandleEvent(keyCtrlP())

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
	if !strings.Contains(joined, "durable activation") {
		t.Error("confirm overlay should render the durable-activation title")
	}
	if !strings.Contains(joined, "Y:Promote") {
		t.Error("confirm overlay should show the Y:Promote / N/Esc:Cancel hint")
	}
}
