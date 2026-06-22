package cluster

// Tests for the Deployments section + cut/deploy/rollback modal
// (memql-cockpit#207). They drive the section + modal state machine
// through HandleEvent against a fake deployConceptActions (no live
// gRPC), asserting: 'P' toggles the section for any role; the history
// list + per-deployment topology render; the role matrix gates the
// controls (cut/deploy = developer+admin+owner, rollback = owner-only);
// and each action fires its SDK wrapper with the right args once the
// confirm gate passes.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// fakeConceptActor is a stub deployConceptActions recording call args.
type fakeConceptActor struct {
	result client.ActionResult
	err    error
	sugg   client.NextVersionSuggestion

	lastCutEnv, lastCutBump, lastCutVersion string
	lastDeployID, lastRollbackID            string
	lastSuggestEnv                          string
	cuts, deploys, rollbacks                int
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
	v.SetDeployConceptActor(fa)
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

func TestDeployments_PToggleAnyRole(t *testing.T) {
	for _, role := range []client.Role{client.RoleReader, client.RoleWriter, client.RoleDeveloper, client.RoleAdmin, client.RoleOwner} {
		v := NewView(ui.DefaultTheme())
		v.SetClusterRole(role)
		if !v.HandleEvent(keyRune('P')) {
			t.Fatalf("role %q: 'P' should toggle the Deployments section", role)
		}
		if !v.DeploymentsActive() {
			t.Errorf("role %q: section should be active after 'P'", role)
		}
		// Esc leaves the section.
		v.HandleEvent(keyEsc())
		if v.DeploymentsActive() {
			t.Errorf("role %q: Esc should close the section", role)
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
	v.HandleEvent(keyRune('P'))
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
	v.HandleEvent(keyRune('P'))
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
		v.HandleEvent(keyRune('P'))
		v.HandleEvent(keyRune('C'))
		if modalOpen(v) {
			t.Errorf("role %q must not open the cut modal", role)
		}
	}
	for _, role := range can {
		v, _ := newConceptView(t, role, &fakeConceptActor{})
		v.HandleEvent(keyRune('P'))
		v.HandleEvent(keyRune('C'))
		if !modalOpen(v) {
			t.Errorf("role %q should open the cut modal", role)
		}
	}
}

func TestDeployments_CutFiresWithEnvAndBump(t *testing.T) {
	fa := &fakeConceptActor{
		result: client.ActionResult{OK: true, Message: "cut", AuditEventID: "evt-cut"},
		sugg:   client.NextVersionSuggestion{CurrentVersion: "2026.6.20", NextPatch: "2026.6.21"},
	}
	v, redrawCh := newConceptView(t, client.RoleDeveloper, fa)
	v.HandleEvent(keyRune('P'))
	v.HandleEvent(keyRune('C')) // open cut -> env picker
	// env picker: cutEnvs[0]=staging. Down once -> production, Enter.
	v.HandleEvent(keyDown())
	v.HandleEvent(keyEnter()) // env=production -> bump picker (+ fires SuggestNextVersion)
	// bump picker: cutBumps[0]=patch. Enter selects patch -> confirm.
	v.HandleEvent(keyEnter())
	// confirm stage: Enter fires CutVersion.
	v.HandleEvent(keyEnter())

	line := waitForConceptResult(t, v, redrawCh)
	if !strings.HasPrefix(line, "SUCCESS:") || !strings.Contains(line, "evt-cut") {
		t.Errorf("expected cut SUCCESS w/ audit id, got %q", line)
	}
	if fa.cuts != 1 || fa.lastCutEnv != "production" || fa.lastCutBump != "patch" {
		t.Errorf("CutVersion args wrong: cuts=%d env=%q bump=%q", fa.cuts, fa.lastCutEnv, fa.lastCutBump)
	}
	// Note: SuggestNextVersion is fired best-effort in a detached
	// goroutine when the bump picker opens (it only populates a preview
	// hint), so we don't assert its call synchronously -- doing so races
	// the cut fire. Its wiring is covered by compiling against the
	// deployConceptActions interface.
}

func TestDeployments_DeployPendingFires(t *testing.T) {
	fa := &fakeConceptActor{result: client.ActionResult{OK: true, Message: "shipping"}}
	v, redrawCh := newConceptView(t, client.RoleDeveloper, fa)
	v.SetDeployments([]DeploymentInfo{{ID: "dep-pending", Version: "9.9.9", Status: "pending"}})
	v.HandleEvent(keyRune('P'))
	v.HandleEvent(keyRune('G')) // deploy the selected pending deployment
	if !modalOpen(v) {
		t.Fatal("G should open the deploy-confirm for a pending deployment")
	}
	v.HandleEvent(keyEnter()) // confirm -> fire Deploy
	line := waitForConceptResult(t, v, redrawCh)
	if !strings.HasPrefix(line, "SUCCESS:") {
		t.Errorf("expected deploy SUCCESS, got %q", line)
	}
	if fa.deploys != 1 || fa.lastDeployID != "dep-pending" {
		t.Errorf("Deploy args wrong: deploys=%d id=%q", fa.deploys, fa.lastDeployID)
	}
}

func TestDeployments_DeployBlockedWhenNotPending(t *testing.T) {
	fa := &fakeConceptActor{}
	v, _ := newConceptView(t, client.RoleDeveloper, fa)
	v.SetDeployments([]DeploymentInfo{{ID: "dep-done", Version: "1.0.0", Status: "succeeded"}})
	v.HandleEvent(keyRune('P'))
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
		v.HandleEvent(keyRune('P'))
		v.HandleEvent(keyRune('B'))
		if modalOpen(v) {
			t.Errorf("role %q must not open rollback", role)
		}
	}
	fa := &fakeConceptActor{result: client.ActionResult{OK: true, Message: "rolled back", AuditEventID: "evt-rb"}}
	v, redrawCh := newConceptView(t, client.RoleOwner, fa)
	v.SetDeployments([]DeploymentInfo{{ID: "dep-succeeded", Version: "3.0.0", Status: "succeeded"}})
	v.HandleEvent(keyRune('P'))
	v.HandleEvent(keyRune('B'))
	if !modalOpen(v) {
		t.Fatal("owner B should open rollback confirm for a succeeded deployment")
	}
	// Type-to-confirm gate: a wrong phrase must not fire.
	typeStr(v, "nope")
	v.HandleEvent(keyEnter())
	if fa.rollbacks != 0 {
		t.Fatalf("wrong confirm phrase must not fire rollback (rollbacks=%d)", fa.rollbacks)
	}
	for range "nope" {
		v.HandleEvent(tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModNone))
	}
	typeStr(v, "rollback")
	v.HandleEvent(keyEnter())
	line := waitForConceptResult(t, v, redrawCh)
	if !strings.Contains(line, "SUCCESS") || !strings.Contains(line, "evt-rb") {
		t.Errorf("expected rollback SUCCESS w/ audit id, got %q", line)
	}
	if fa.rollbacks != 1 || fa.lastRollbackID != "dep-succeeded" {
		t.Errorf("RollbackDeployment args wrong: rollbacks=%d id=%q", fa.rollbacks, fa.lastRollbackID)
	}
}

func TestDeployments_RollbackPermissionDeniedMapped(t *testing.T) {
	fa := &fakeConceptActor{err: status.Error(codes.PermissionDenied, "nope")}
	v, redrawCh := newConceptView(t, client.RoleOwner, fa)
	v.SetDeployments([]DeploymentInfo{{ID: "dep-x", Version: "1.0.0", Status: "succeeded"}})
	v.HandleEvent(keyRune('P'))
	v.HandleEvent(keyRune('B'))
	typeStr(v, "rollback")
	v.HandleEvent(keyEnter())
	line := waitForConceptResult(t, v, redrawCh)
	if line != "ERROR: requires owner/admin" {
		t.Errorf("expected owner/admin mapping, got %q", line)
	}
}

func TestDeployments_HintsRoleGated(t *testing.T) {
	owner := NewView(ui.DefaultTheme())
	owner.SetClusterRole(client.RoleOwner)
	owner.SetDeployments([]DeploymentInfo{{ID: "d", Version: "1", Status: "succeeded"}})
	owner.HandleEvent(keyRune('P'))
	joined := strings.Join(renderTopology(t, owner, 120, 30), "\n")
	if !strings.Contains(joined, "Rollback") {
		t.Errorf("owner should see the Rollback hint; got:\n%s", joined)
	}

	reader := NewView(ui.DefaultTheme())
	reader.SetClusterRole(client.RoleReader)
	reader.SetDeployments([]DeploymentInfo{{ID: "d", Version: "1", Status: "succeeded"}})
	reader.HandleEvent(keyRune('P'))
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
