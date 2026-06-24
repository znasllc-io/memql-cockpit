package dsledit

// Tests for the owner-gated durable Promote action (znasllc-io/memql-cockpit#232):
// the owner-only hint chip + Ctrl+P keybinding, the type-to-confirm
// blast-radius gate, the no-cluster gate, and the result-overlay
// formatting. Mirrors validate_inject_test.go -- the actual
// DurablePromoteBundle wire call needs a live engine (owned by the SDK),
// so here we cover the cockpit-side gating + flow + formatting against
// constructed results and nil clients.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	authoringsdk "github.com/znasllc-io/memql/sdk/go/authoring"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// newOwnerAuthoring builds an authoring sub-view whose cluster role is
// owner, with a bundle already created + opened so Promote has something
// to act on. Returns the view and the collected status messages.
func newOwnerAuthoring(t *testing.T) (*authoring, *[]string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var status []string
	a := newAuthoring(ui.DefaultTheme(), nilSense, nilAuthoring, ownerRole, func(m string) { status = append(status, m) }, func() {})
	if err := createBundle("b"); err != nil {
		t.Fatalf("createBundle: %v", err)
	}
	if _, err := createBundleFile("b", "q"); err != nil {
		t.Fatalf("createBundleFile: %v", err)
	}
	if err := writeBundleFile("b", "q.memql", "query q {}"); err != nil {
		t.Fatalf("writeBundleFile: %v", err)
	}
	a.mu.Lock()
	a.loadBundleFilesLocked("b")
	a.mu.Unlock()
	return a, &status
}

// TestPromoteHintOwnerOnly: the Ctrl+P:Promote chip appears for an owner
// and is absent for a non-owner, per the context-aware action-hint rule.
func TestPromoteHintOwnerOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	owner := newAuthoring(ui.DefaultTheme(), nilSense, nilAuthoring, ownerRole, func(string) {}, func() {})
	if !strings.Contains(owner.hintsForEditor(), "Ctrl+P:Promote") {
		t.Errorf("owner editor hint missing Promote chip: %q", owner.hintsForEditor())
	}

	nonOwner := newAuthoring(ui.DefaultTheme(), nilSense, nilAuthoring, nonOwnerRole, func(string) {}, func() {})
	if strings.Contains(nonOwner.hintsForEditor(), "Promote") {
		t.Errorf("non-owner editor hint must not advertise Promote: %q", nonOwner.hintsForEditor())
	}

	// The owner hint must still be contract-valid Key:Label chips.
	assertChips(t, "authoring-editor (owner)", owner.hintsForEditor())
}

// TestPromoteKeyGatedForNonOwner: Ctrl+P is a no-op for a non-owner --
// it consumes the key (so it can't fall through) but opens no prompt.
func TestPromoteKeyGatedForNonOwner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAuthoring(ui.DefaultTheme(), nilSense, nilAuthoring, nonOwnerRole, func(string) {}, func() {})
	if err := createBundle("b"); err != nil {
		t.Fatalf("createBundle: %v", err)
	}
	a.mu.Lock()
	a.loadBundleFilesLocked("b")
	a.mu.Unlock()

	if !a.HandleEvent(tcell.NewEventKey(tcell.KeyCtrlP, 0, tcell.ModCtrl)) {
		t.Fatal("Ctrl+P should be consumed even when gated")
	}
	if a.prompt != promptNone {
		t.Fatalf("non-owner Ctrl+P opened a prompt %v (must be gated)", a.prompt)
	}
}

// TestPromoteConfirmFlow: an owner pressing Ctrl+P opens the
// type-to-confirm prompt; a wrong word re-arms with an error; the
// matching word clears the prompt and (with a nil client) hits the
// no-cluster gate -- proving the confirm path reaches runPromote.
func TestPromoteConfirmFlow(t *testing.T) {
	a, status := newOwnerAuthoring(t)

	// Ctrl+P opens the confirm prompt with the gathered bundle.
	if !a.HandleEvent(tcell.NewEventKey(tcell.KeyCtrlP, 0, tcell.ModCtrl)) {
		t.Fatal("Ctrl+P not consumed for owner")
	}
	if a.prompt != promptPromote {
		t.Fatalf("Ctrl+P should open the promote confirm prompt, got %v", a.prompt)
	}
	if a.promotePendingBundle != "b" {
		t.Fatalf("pending bundle = %q, want b", a.promotePendingBundle)
	}

	// Wrong confirm word: re-arms with an error, stays in the prompt.
	typeStr(a, "nope")
	a.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if a.prompt != promptPromote {
		t.Fatalf("wrong confirm word should keep the prompt open, got %v", a.prompt)
	}
	if a.promptErr == "" {
		t.Error("wrong confirm word should set a prompt error")
	}

	// Correct word: clears the prompt and runs promote (nil client ->
	// no-cluster gate message, which proves runPromote was reached).
	typeStr(a, promoteConfirmWord)
	a.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if a.prompt != promptNone {
		t.Fatalf("correct confirm word should close the prompt, got %v", a.prompt)
	}
	// runPromote is launched in a goroutine; drive it synchronously here
	// to assert the gate without racing.
	a.runPromote("b", "query q {}")
	joined := strings.Join(*status, " | ")
	if !strings.Contains(joined, "promote needs a connected cluster") {
		t.Errorf("promote gate message missing: %q", joined)
	}
}

// TestPromoteEscCancels: Esc out of the confirm prompt clears the
// pending bundle and never promotes.
func TestPromoteEscCancels(t *testing.T) {
	a, _ := newOwnerAuthoring(t)
	a.HandleEvent(tcell.NewEventKey(tcell.KeyCtrlP, 0, tcell.ModCtrl))
	if a.prompt != promptPromote {
		t.Fatalf("expected promote prompt, got %v", a.prompt)
	}
	a.HandleEvent(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))
	if a.prompt != promptNone {
		t.Fatalf("Esc should cancel the promote prompt, got %v", a.prompt)
	}
	if a.promotePendingBundle != "" || a.promotePendingSources != "" {
		t.Error("Esc should clear the pending promote bundle/sources")
	}
}

// TestShowPromoteResult covers the result-overlay formatting for both
// the durable-OK and rejected paths, including the cluster-wide vs
// session-scoped wording.
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
	if !strings.Contains(a.resultTitle, "cluster-wide") {
		t.Errorf("promote OK title should say cluster-wide: %q", a.resultTitle)
	}
	joined := ""
	for _, l := range a.resultLines {
		joined += l.text + "\n"
	}
	if !strings.Contains(joined, "every node") || !strings.Contains(joined, "q1 (query)") {
		t.Errorf("promote OK lines missing durable/cluster-wide detail: %q", joined)
	}

	// Rejected path.
	a.showPromoteResultLocked(&authoringsdk.PromoteResult{
		OK:    false,
		Error: "validation failed",
		Diagnostics: []authoringsdk.Diagnostic{
			{Name: "bad", Kind: "query", Error: "nope"},
		},
	})
	if !strings.Contains(a.resultTitle, "REJECTED") {
		t.Errorf("promote rejected title = %q", a.resultTitle)
	}
}

// TestPromoteGatedNoClient: with an owner role but a nil authoring
// client, runPromote hits the no-cluster gate and renders no overlay.
func TestPromoteGatedNoClient(t *testing.T) {
	a, status := newOwnerAuthoring(t)
	a.runPromote("b", "query q {}")
	joined := strings.Join(*status, " | ")
	if !strings.Contains(joined, "promote needs a connected cluster") {
		t.Errorf("promote gate message missing: %q", joined)
	}
	if a.resultShown {
		t.Error("no overlay should show when promote is gated")
	}
}

// TestPromoteConfirmOverlayRenders renders the type-to-confirm overlay
// and asserts the blast-radius copy lands on screen (persist +
// cluster-wide), distinguishing it from Inject's session scope.
func TestPromoteConfirmOverlayRenders(t *testing.T) {
	a, _ := newOwnerAuthoring(t)
	a.HandleEvent(tcell.NewEventKey(tcell.KeyCtrlP, 0, tcell.ModCtrl))

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
	for _, want := range []string{"PROMOTE", "PERSISTED", "EVERY", "confirm"} {
		if !strings.Contains(joined, want) {
			t.Errorf("promote confirm overlay missing %q", want)
		}
	}
}
