package cluster

// Tests for the always-on Deployments section + cut/deploy/rollback
// modal (memql-cockpit#207 / #221). They drive the section + modal state
// machine through HandleEvent against a fake deployConceptActions (no
// live gRPC), asserting: the section is always present (no toggle) so its
// navigation + control keys are live for any role; the history list +
// per-deployment topology render; the role matrix gates the controls
// (cut/deploy = developer+admin+owner, rollback = owner-only); and each
// action fires its SDK wrapper with the right args once the confirm gate
// passes.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// --- shared test helpers (relocated from the removed deploy_test.go /
// deploy_controls_test.go when the deployment-v2 "Surface A" was deleted
// in memql-cockpit#221) ---

// keyRune / keyEnter / keyEsc / keyDown synthesize tcell key events.
func keyRune(r rune) *tcell.EventKey { return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone) }
func keyEnter() *tcell.EventKey      { return tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone) }
func keyEsc() *tcell.EventKey        { return tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone) }
func keyDown() *tcell.EventKey       { return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone) }

// typeStr feeds each rune of s through HandleEvent.
func typeStr(v *View, s string) {
	for _, r := range s {
		v.HandleEvent(keyRune(r))
	}
}

// renderTopology draws the View against a fresh SimulationScreen of the
// given size and returns the flattened rows. Shares flattenSim with
// chrome_contract_test.go.
func renderTopology(t *testing.T, v *View, w, h int) []string {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(w, h)
	sim.Clear()

	screen := ui.NewScreenFromTcell(sim)
	v.Draw(screen, ui.Rect{X: 0, Y: 0, Width: w, Height: h})
	sim.Show()
	return flattenSim(sim)
}

// fakeConceptActor is a stub deployConceptActions recording call args.
type fakeConceptActor struct {
	result client.ActionResult
	err    error
	sugg   client.NextVersionSuggestion

	lastCutEnv, lastCutBump, lastCutVersion string
	lastDeployID, lastRollbackID            string
	lastSuggestEnv                          string
	cuts, deploys, rollbacks                int

	// runner-path recording (#292): C/G/B now fire through the injected
	// View.DeployRunner (the deployEngineCluster automation path), not the
	// gRPC actor methods above -- only SuggestNextVersion still uses the
	// actor.
	runCalls                     int
	runEnv, runAction, runTarget string
}

func (f *fakeConceptActor) CutVersion(_ context.Context, env, bump, version string) (client.ActionResult, error) {
	f.cuts++
	f.lastCutEnv, f.lastCutBump, f.lastCutVersion = env, bump, version
	return f.result, f.err
}
func (f *fakeConceptActor) Deploy(_ context.Context, id string) (client.ActionResult, error) {
	f.deploys++
	f.lastDeployID = id
	return f.result, f.err
}
func (f *fakeConceptActor) RollbackDeployment(_ context.Context, id string) (client.ActionResult, error) {
	f.rollbacks++
	f.lastRollbackID = id
	return f.result, f.err
}
func (f *fakeConceptActor) SuggestNextVersion(_ context.Context, env string) (client.NextVersionSuggestion, error) {
	f.lastSuggestEnv = env
	return f.sugg, nil
}

// newConceptView returns a view at role with the fake actor wired and a
// redraw latch (buffered so async fires never block).
func newConceptView(t *testing.T, role client.Role, fa *fakeConceptActor) (*View, chan struct{}) {
	t.Helper()
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(role)
	v.SetDeployConceptActor(fa) // still used by SuggestNextVersion (cut preview)
	v.DeployRunner = func(env, action, targetID string) (string, bool) {
		fa.runCalls++
		fa.runEnv, fa.runAction, fa.runTarget = env, action, targetID
		return "OK: routed " + action, true
	}
	redrawCh := make(chan struct{}, 32)
	v.OnRedraw = func() {
		select {
		case redrawCh <- struct{}{}:
		default:
		}
	}
	return v, redrawCh
}

// waitForConceptResult spins until the concept modal reaches a terminal
// (non-running) result, draining redraw signals. Returns the line.
func waitForConceptResult(t *testing.T, v *View, redrawCh chan struct{}) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		v.mu.RLock()
		m := v.dctrl
		var done bool
		var line string
		if m != nil && m.stage == dcStageResult && !m.running {
			done, line = true, m.result
		}
		v.mu.RUnlock()
		if done {
			return line
		}
		select {
		case <-redrawCh:
		case <-deadline:
			t.Fatal("concept action did not reach a terminal result")
			return ""
		}
	}
}

// TestDeployments_AlwaysPresentNavForAnyRole asserts the always-on
// Deployments section's navigation keys (Up/Down/Enter) are live for any
// role without a toggle, and that the now-removed 'P'/'Esc' toggle keys
// are inert (no toggle exists in the persistent split).
func TestDeployments_AlwaysPresentNavForAnyRole(t *testing.T) {
	for _, role := range []client.Role{client.RoleReader, client.RoleWriter, client.RoleDeveloper, client.RoleAdmin, client.RoleOwner} {
		v := NewView(ui.DefaultTheme())
		v.SetClusterRole(role)
		v.SetDeployments([]DeploymentInfo{
			{ID: "dep-1", Version: "1.0.0", Status: "succeeded"},
			{ID: "dep-2", Version: "0.9.0", Status: "superseded"},
		})
		// Down moves the deployment cursor without any toggle.
		if !v.HandleEvent(keyDown()) {
			t.Fatalf("role %q: Down should move the deployment cursor", role)
		}
		if got := v.SelectedDeploymentID(); got != "dep-2" {
			t.Errorf("role %q: Down should select dep-2, got %q", role, got)
		}
		// 'P' is no longer a toggle key -- it's inert here.
		if v.HandleEvent(keyRune('P')) {
			t.Errorf("role %q: 'P' should be inert (no toggle in the persistent split)", role)
		}
		// Esc is likewise inert (nothing to close).
		if v.HandleEvent(keyEsc()) {
			t.Errorf("role %q: Esc should be inert with no modal open", role)
		}
	}
}

func TestDeployments_HistoryAndTopologyRender(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(client.RoleDeveloper)
	v.SetDeployments([]DeploymentInfo{
		{ID: "dep-aaaaaaaaaaaa", Version: "2026.6.21", Environment: "staging", Provider: "azure", Status: "succeeded"},
		{ID: "dep-bbbbbbbbbbbb", Version: "2026.6.20", Environment: "staging", Provider: "azure", Status: "superseded"},
	})
	// Load the selected deployment's topology.
	v.SetDeploymentNodes("dep-aaaaaaaaaaaa",
		[]NodeInfo{{Type: "bff", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY, Version: "2026.6.21"}},
		[]NodeInfo{{Type: "agent", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY, DeploymentId: "dep-old"}},
	)

	joined := strings.Join(renderTopology(t, v, 110, 34), "\n")
	for _, want := range []string{"DEPLOYMENT HISTORY", "succeeded", "2026.6.21", "Nodes: 1", "Orphans", "[orphan]"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in the Deployments render; got:\n%s", want, joined)
		}
	}
}

func TestDeployments_SelectFiresOnSelect(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(client.RoleReader)
	got := make(chan string, 1)
	v.OnSelectDeployment = func(id string) { got <- id }
	v.SetDeployments([]DeploymentInfo{{ID: "dep-xyz", Version: "1.0.0", Status: "succeeded"}})
	v.HandleEvent(keyEnter()) // drill into the selected deployment
	select {
	case id := <-got:
		if id != "dep-xyz" {
			t.Errorf("OnSelectDeployment got %q, want dep-xyz", id)
		}
	case <-time.After(time.Second):
		t.Fatal("Enter should fire OnSelectDeployment")
	}
}

func TestDeployments_CutRoleGate(t *testing.T) {
	// Reader/Writer cannot open cut; developer/admin/owner can.
	cannot := []client.Role{client.RoleReader, client.RoleWriter}
	can := []client.Role{client.RoleDeveloper, client.RoleAdmin, client.RoleOwner}
	for _, role := range cannot {
		v, _ := newConceptView(t, role, &fakeConceptActor{})
		v.HandleEvent(keyRune('C'))
		if modalOpen(v) {
			t.Errorf("role %q must not open the cut modal", role)
		}
	}
	for _, role := range can {
		v, _ := newConceptView(t, role, &fakeConceptActor{})
		v.HandleEvent(keyRune('C'))
		if !modalOpen(v) {
			t.Errorf("role %q should open the cut modal", role)
		}
	}
}

func TestDeployments_CutRoutesToRunner(t *testing.T) {
	// #292: cut fires through the DeployRunner (automation path), NOT the
	// retired DeployControlService gRPC. The cluster env is carried through.
	fa := &fakeConceptActor{
		sugg: client.NextVersionSuggestion{CurrentVersion: "2026.6.20", NextPatch: "2026.6.21"},
	}
	v, redrawCh := newConceptView(t, client.RoleDeveloper, fa)
	v.SetClusterEnvironment("production")
	v.HandleEvent(keyRune('C')) // open cut -> bump picker (env already resolved)
	v.HandleEvent(keyEnter())   // select patch -> confirm
	v.HandleEvent(keyEnter())   // confirm -> fire

	line := waitForConceptResult(t, v, redrawCh)
	if !strings.Contains(line, "routed cut") {
		t.Errorf("cut should route to the deploy runner; got %q", line)
	}
	if fa.runCalls != 1 || fa.runAction != "cut" || fa.runEnv != "production" {
		t.Errorf("cut routed wrong: calls=%d action=%q env=%q", fa.runCalls, fa.runAction, fa.runEnv)
	}
	if fa.cuts != 0 {
		t.Errorf("cut must NOT dial the retired DeployControlService (cuts=%d)", fa.cuts)
	}
}

func TestDeployments_DeployRoutesToRunner(t *testing.T) {
	// #292: G fires the env-level deployEngineCluster automation via the
	// runner, carrying the cluster env + the selected deployment id.
	fa := &fakeConceptActor{}
	v, redrawCh := newConceptView(t, client.RoleDeveloper, fa)
	v.SetClusterEnvironment("staging")
	v.SetDeployments([]DeploymentInfo{{ID: "dep-pending", Version: "9.9.9", Status: "pending"}})
	v.HandleEvent(keyRune('G')) // deploy the selected pending deployment
	if !modalOpen(v) {
		t.Fatal("G should open the deploy-confirm for a pending deployment")
	}
	v.HandleEvent(keyEnter()) // confirm -> fire
	line := waitForConceptResult(t, v, redrawCh)
	if !strings.Contains(line, "routed deploy") {
		t.Errorf("deploy should route to the runner; got %q", line)
	}
	if fa.runCalls != 1 || fa.runAction != "deploy" || fa.runEnv != "staging" || fa.runTarget != "dep-pending" {
		t.Errorf("deploy routed wrong: calls=%d action=%q env=%q target=%q",
			fa.runCalls, fa.runAction, fa.runEnv, fa.runTarget)
	}
	if fa.deploys != 0 {
		t.Errorf("deploy must NOT dial the retired DeployControlService (deploys=%d)", fa.deploys)
	}
}

func TestDeployments_DeployBlockedWhenNotPending(t *testing.T) {
	fa := &fakeConceptActor{}
	v, _ := newConceptView(t, client.RoleDeveloper, fa)
	v.SetDeployments([]DeploymentInfo{{ID: "dep-done", Version: "1.0.0", Status: "succeeded"}})
	v.HandleEvent(keyRune('G'))
	if modalOpen(v) {
		t.Error("G must not open deploy-confirm for a non-pending deployment")
	}
}

func TestDeployments_RollbackOwnerOnly(t *testing.T) {
	// developer/admin cannot rollback; owner can.
	for _, role := range []client.Role{client.RoleDeveloper, client.RoleAdmin} {
		v, _ := newConceptView(t, role, &fakeConceptActor{})
		v.SetDeployments([]DeploymentInfo{{ID: "dep-s", Version: "1.0.0", Status: "succeeded"}})
		v.HandleEvent(keyRune('B'))
		if modalOpen(v) {
			t.Errorf("role %q must not open rollback", role)
		}
	}
	fa := &fakeConceptActor{}
	v, redrawCh := newConceptView(t, client.RoleOwner, fa)
	v.SetClusterEnvironment("staging")
	v.SetDeployments([]DeploymentInfo{{ID: "dep-succeeded", Version: "3.0.0", Status: "succeeded"}})
	v.HandleEvent(keyRune('B'))
	if !modalOpen(v) {
		t.Fatal("owner B should open rollback confirm for a succeeded deployment")
	}
	// Type-to-confirm gate: a wrong phrase must not fire the runner.
	typeStr(v, "nope")
	v.HandleEvent(keyEnter())
	if fa.runCalls != 0 {
		t.Fatalf("wrong confirm phrase must not fire rollback (runCalls=%d)", fa.runCalls)
	}
	for range "nope" {
		v.HandleEvent(tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModNone))
	}
	typeStr(v, "rollback")
	v.HandleEvent(keyEnter())
	line := waitForConceptResult(t, v, redrawCh)
	if !strings.Contains(line, "routed rollback") {
		t.Errorf("rollback should route to the runner; got %q", line)
	}
	if fa.runCalls != 1 || fa.runAction != "rollback" || fa.runTarget != "dep-succeeded" {
		t.Errorf("rollback routed wrong: calls=%d action=%q target=%q", fa.runCalls, fa.runAction, fa.runTarget)
	}
	if fa.rollbacks != 0 {
		t.Errorf("rollback must NOT dial the retired DeployControlService (rollbacks=%d)", fa.rollbacks)
	}
}

func TestDeployments_HintsRoleGated(t *testing.T) {
	owner := NewView(ui.DefaultTheme())
	owner.SetClusterRole(client.RoleOwner)
	owner.SetDeployments([]DeploymentInfo{{ID: "d", Version: "1", Status: "succeeded"}})
	joined := strings.Join(renderTopology(t, owner, 120, 30), "\n")
	if !strings.Contains(joined, "Rollback") {
		t.Errorf("owner should see the Rollback hint; got:\n%s", joined)
	}

	reader := NewView(ui.DefaultTheme())
	reader.SetClusterRole(client.RoleReader)
	reader.SetDeployments([]DeploymentInfo{{ID: "d", Version: "1", Status: "succeeded"}})
	// The reader still sees the section + hint bar, but Cut/Rollback chips
	// are disabled (the HintBar renders disabled chips dimmed; the action
	// keys are no-ops, which the role-gate tests above already assert).
	if reader.CanCutDeploy() || reader.CanRollback() {
		t.Error("reader must not have cut/rollback capability")
	}
}

// modalOpen reports whether the concept modal is open (read under lock).
func modalOpen(v *View) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dctrl != nil
}
