package cluster

// Tests for the deploy-control modal (#145). They drive the modal
// state machine through HandleEvent against a fake deployActions (no
// live gRPC), asserting: owner/admin opens the modal and a stubbed
// action lands a SUCCESS / ERROR line carrying the AuditEventId; a
// non-admin caller's 'D' key is a no-op (modal never opens); and the
// type-to-confirm gate blocks a promote/abort until the phrase matches.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// fakeActor is a stub deployActions. Each method returns the canned
// result / error and records the args it was called with.
type fakeActor struct {
	result client.ActionResult
	err    error

	lastStagingVersion string
	lastPromoteVersion string
	lastRollbackEnv    string
	lastRollbackSha    string
	lastRolloutEnv     string
	lastRolloutName    string
	lastRolloutAction  string
	calls              int
}

func (f *fakeActor) DeployStaging(_ context.Context, version string) (client.ActionResult, error) {
	f.calls++
	f.lastStagingVersion = version
	return f.result, f.err
}
func (f *fakeActor) Promote(_ context.Context, version string) (client.ActionResult, error) {
	f.calls++
	f.lastPromoteVersion = version
	return f.result, f.err
}
func (f *fakeActor) Rollback(_ context.Context, env, sha string) (client.ActionResult, error) {
	f.calls++
	f.lastRollbackEnv = env
	f.lastRollbackSha = sha
	return f.result, f.err
}
func (f *fakeActor) RolloutAction(_ context.Context, env, rollout, action string) (client.ActionResult, error) {
	f.calls++
	f.lastRolloutEnv = env
	f.lastRolloutName = rollout
	f.lastRolloutAction = action
	return f.result, f.err
}

// keyRune / keyNamed are small helpers to synthesize tcell events.
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

// newDoneView returns an owner view with the fake actor wired and a
// synchronous OnActionComplete latch so tests can confirm the post-
// action refresh hook fires. drainRedraw blocks until the async fire's
// result is stored (the goroutine signals via the redrawCh).
func newOwnerView(t *testing.T, fa *fakeActor) (*View, chan struct{}) {
	t.Helper()
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(client.RoleOwner)
	v.SetDeployActor(fa)
	redrawCh := make(chan struct{}, 16)
	v.OnRedraw = func() {
		select {
		case redrawCh <- struct{}{}:
		default:
		}
	}
	return v, redrawCh
}

// waitForResult spins HandleEvent-free until the modal reaches a
// terminal (non-running) result, draining redraw signals. Returns the
// result line.
func waitForResult(t *testing.T, v *View, redrawCh chan struct{}) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		v.mu.RLock()
		m := v.ctrl
		var done bool
		var line string
		if m != nil && m.stage == deployStageResult && !m.running {
			done = true
			line = m.result
		}
		v.mu.RUnlock()
		if done {
			return line
		}
		select {
		case <-redrawCh:
			// repaint signalled -- re-check.
		case <-deadline:
			t.Fatal("action did not reach a terminal result")
			return ""
		}
	}
}

func TestDeployModal_NonAdminCannotOpen(t *testing.T) {
	for _, role := range []client.Role{client.RoleReader, client.RoleWriter, ""} {
		v := NewView(ui.DefaultTheme())
		v.SetClusterRole(role)
		consumed := v.HandleEvent(keyRune('D'))
		if consumed {
			t.Errorf("role %q: 'D' should be a no-op for non-admin (not consumed)", role)
		}
		v.mu.RLock()
		open := v.ctrl != nil
		v.mu.RUnlock()
		if open {
			t.Errorf("role %q: deploy modal must not open for non-admin", role)
		}
	}
}

func TestDeployModal_OwnerOpensAndDeployStagingSuccess(t *testing.T) {
	fa := &fakeActor{result: client.ActionResult{OK: true, Message: "queued", AuditEventID: "evt-123"}}
	v, redrawCh := newOwnerView(t, fa)

	if !v.HandleEvent(keyRune('D')) {
		t.Fatal("owner 'D' should open the modal")
	}
	v.mu.RLock()
	open := v.ctrl != nil && v.ctrl.stage == deployStageMenu
	v.mu.RUnlock()
	if !open {
		t.Fatal("modal should be open on the menu stage")
	}

	// Menu item 0 is "Deploy to staging". Enter selects it.
	v.HandleEvent(keyEnter())
	typeStr(v, "v9.9.9")
	v.HandleEvent(keyEnter()) // fire DeployStaging

	line := waitForResult(t, v, redrawCh)
	if !strings.HasPrefix(line, "SUCCESS:") {
		t.Errorf("expected SUCCESS line, got %q", line)
	}
	if !strings.Contains(line, "evt-123") {
		t.Errorf("expected AuditEventId in result, got %q", line)
	}
	if fa.lastStagingVersion != "v9.9.9" {
		t.Errorf("DeployStaging got version %q, want v9.9.9", fa.lastStagingVersion)
	}
}

func TestDeployModal_DeployStagingErrorSurfaced(t *testing.T) {
	fa := &fakeActor{err: status.Error(codes.Internal, "boom")}
	v, redrawCh := newOwnerView(t, fa)

	v.HandleEvent(keyRune('D'))
	v.HandleEvent(keyEnter()) // pick deploy staging
	typeStr(v, "v1")
	v.HandleEvent(keyEnter())

	line := waitForResult(t, v, redrawCh)
	if !strings.HasPrefix(line, "ERROR:") {
		t.Errorf("expected ERROR line, got %q", line)
	}
}

func TestDeployModal_PermissionDeniedMapped(t *testing.T) {
	fa := &fakeActor{err: status.Error(codes.PermissionDenied, "nope")}
	v, redrawCh := newOwnerView(t, fa)

	v.HandleEvent(keyRune('D'))
	v.HandleEvent(keyEnter())
	typeStr(v, "v1")
	v.HandleEvent(keyEnter())

	line := waitForResult(t, v, redrawCh)
	if line != "ERROR: requires owner/admin" {
		t.Errorf("expected owner/admin mapping, got %q", line)
	}
}

func TestDeployModal_PromoteTypeToConfirmGate(t *testing.T) {
	fa := &fakeActor{result: client.ActionResult{OK: true, Message: "promoted", AuditEventID: "evt-9"}}
	v, redrawCh := newOwnerView(t, fa)

	v.HandleEvent(keyRune('D'))
	v.HandleEvent(keyDown()) // menu idx 1 = Promote
	v.HandleEvent(keyEnter())
	typeStr(v, "v2.0.0")
	v.HandleEvent(keyEnter()) // advance to confirm

	v.mu.RLock()
	atConfirm := v.ctrl != nil && v.ctrl.stage == deployStagePromoteConfirm
	v.mu.RUnlock()
	if !atConfirm {
		t.Fatal("expected promote-confirm stage")
	}

	// Wrong confirmation phrase -> no fire, still on confirm.
	typeStr(v, "wrong")
	v.HandleEvent(keyEnter())
	v.mu.RLock()
	stillConfirm := v.ctrl != nil && v.ctrl.stage == deployStagePromoteConfirm && fa.calls == 0
	v.mu.RUnlock()
	if !stillConfirm {
		t.Fatalf("wrong confirm phrase must not fire promote (calls=%d)", fa.calls)
	}

	// Clear the wrong text and type the matching version.
	for range "wrong" {
		v.HandleEvent(tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModNone))
	}
	typeStr(v, "v2.0.0")
	v.HandleEvent(keyEnter()) // fires Promote

	line := waitForResult(t, v, redrawCh)
	if !strings.Contains(line, "SUCCESS") || !strings.Contains(line, "evt-9") {
		t.Errorf("expected promote SUCCESS with audit id, got %q", line)
	}
	if fa.lastPromoteVersion != "v2.0.0" {
		t.Errorf("Promote got version %q, want v2.0.0", fa.lastPromoteVersion)
	}
}

func TestDeployModal_EscClosesFromMenu(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(client.RoleAdmin)
	v.HandleEvent(keyRune('D'))
	v.HandleEvent(keyEsc())
	v.mu.RLock()
	open := v.ctrl != nil
	v.mu.RUnlock()
	if open {
		t.Error("Esc on the menu should close the modal")
	}
}

// TestDeployModal_RendersOverTopology asserts the modal paints its
// title + a body line over the topology pane without panicking.
func TestDeployModal_RendersOverTopology(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(client.RoleOwner)
	v.HandleEvent(keyRune('D'))

	rows := renderTopology(t, v, 100, 30)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "DEPLOY CONTROLS") {
		t.Errorf("expected modal title; got:\n%s", joined)
	}
	if !strings.Contains(joined, "Deploy to staging") {
		t.Errorf("expected menu item in modal body; got:\n%s", joined)
	}
}

// TestDeployModal_HintShownForAdminOnly asserts the D:Deploy hint is
// present for owner/admin and absent for non-admins.
func TestDeployModal_HintShownForAdminOnly(t *testing.T) {
	admin := NewView(ui.DefaultTheme())
	admin.SetClusterRole(client.RoleAdmin)
	if !strings.Contains(strings.Join(renderTopology(t, admin, 100, 30), "\n"), "D:Deploy") {
		t.Error("expected D:Deploy hint for admin")
	}
	reader := NewView(ui.DefaultTheme())
	reader.SetClusterRole(client.RoleReader)
	if strings.Contains(strings.Join(renderTopology(t, reader, 100, 30), "\n"), "D:Deploy") {
		t.Error("D:Deploy hint must be hidden for non-admin")
	}
}
