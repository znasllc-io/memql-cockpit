package tools

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/worker/consent"
)

// fakeGate is a ConsentGate stub the dispatcher tests plug in so we
// don't have to spin up a Manager + socket just to assert the
// failure shape.
type fakeGate struct {
	decision consent.Decision
	calls    int
}

func (g *fakeGate) Allows(tool, action string) consent.Decision {
	g.calls++
	return g.decision
}

// quietLogger returns a slog logger that drops everything below
// Error so the dispatcher's normal INFO chatter doesn't drown the
// test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestDispatcher_ConsentGateDenies(t *testing.T) {
	gate := &fakeGate{decision: consent.Decision{
		Allowed: false,
		Reason:  "computer-use consent has not been granted -- run `memql-cockpit worker consent grant --window=<duration>`",
	}}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), gate)

	// Use workerHost/fs_read -- the simplest tool action; we want
	// to confirm the gate runs BEFORE the handler so this never
	// touches the filesystem.
	dispatch := &memqlv1.ToolDispatch{
		Tool:     "workerHost",
		Action:   "fs_read",
		ArgsJson: []byte(`{"fs_read":{"path":"/etc/hosts"}}`),
		CallId:   "test-1",
	}
	success, failure := d.Dispatch(context.Background(), dispatch)
	if success != nil {
		t.Fatalf("expected nil success when gate denies; got %+v", success)
	}
	if failure == nil {
		t.Fatal("expected non-nil failure when gate denies")
	}
	if failure.GetErrorCode() != "consent_required" {
		t.Errorf("expected error_code=consent_required; got %q", failure.GetErrorCode())
	}
	if !strings.Contains(failure.GetErrorMessage(), "consent has not been granted") {
		t.Errorf("error message should carry the gate's reason; got %q", failure.GetErrorMessage())
	}
	if gate.calls != 1 {
		t.Errorf("gate should be consulted exactly once; got %d", gate.calls)
	}
}

func TestDispatcher_ConsentGateAllowsPassesThrough(t *testing.T) {
	gate := &fakeGate{decision: consent.Decision{Allowed: true, Reason: "consent granted"}}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), gate)

	// Use a known-bad action so we exit with a typed failure
	// instead of touching the filesystem -- we just want to assert
	// the gate didn't short-circuit, the dispatcher proceeded to
	// the per-action handler.
	dispatch := &memqlv1.ToolDispatch{
		Tool:     "workerHost",
		Action:   "bogus_action",
		ArgsJson: []byte(`{}`),
		CallId:   "test-2",
	}
	success, failure := d.Dispatch(context.Background(), dispatch)
	if success != nil {
		t.Fatalf("expected nil success for unknown action; got %+v", success)
	}
	if failure == nil {
		t.Fatal("expected non-nil failure from unknown_action path")
	}
	if failure.GetErrorCode() == "consent_required" {
		t.Errorf("gate said Allowed=true but dispatcher still returned consent_required; got %q", failure.GetErrorCode())
	}
	if failure.GetErrorCode() != "unknown_action" {
		t.Errorf("expected unknown_action after the gate admits; got %q", failure.GetErrorCode())
	}
	if gate.calls != 1 {
		t.Errorf("gate should be consulted exactly once even on admit; got %d", gate.calls)
	}
}

func TestDispatcher_NilGateBehavesPre64(t *testing.T) {
	// Pre-#64 behavior: when no gate is wired, the dispatcher runs
	// every call without a consent check. Used by tests that
	// predate the gate; we want to keep that path working.
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)

	dispatch := &memqlv1.ToolDispatch{
		Tool:     "workerHost",
		Action:   "bogus_action",
		ArgsJson: []byte(`{}`),
		CallId:   "test-3",
	}
	_, failure := d.Dispatch(context.Background(), dispatch)
	if failure == nil || failure.GetErrorCode() != "unknown_action" {
		t.Errorf("expected unknown_action path to fire when gate is nil; got %+v", failure)
	}
}

// TestDispatcher_ConsentGate_EndToEndWithManager wires the real
// consent.Manager (no fake) and confirms the full grant -> allow,
// revoke -> deny cycle through the dispatcher.
func TestDispatcher_ConsentGate_EndToEndWithManager(t *testing.T) {
	mgr := consent.NewManager()
	d := NewDispatcher(quietLogger(), DefaultPolicy(), mgr)

	dispatch := &memqlv1.ToolDispatch{
		Tool:     "workerHost",
		Action:   "bogus_action",
		ArgsJson: []byte(`{}`),
		CallId:   "e2e",
	}

	// Pre-grant: deny.
	_, failure := d.Dispatch(context.Background(), dispatch)
	if failure == nil || failure.GetErrorCode() != "consent_required" {
		t.Fatalf("pre-grant should deny with consent_required; got %+v", failure)
	}

	// Grant: admit (unknown_action surfaces from the inner dispatch).
	if _, err := mgr.Grant(getTestWindow(t), false); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	_, failure = d.Dispatch(context.Background(), dispatch)
	if failure == nil || failure.GetErrorCode() == "consent_required" {
		t.Fatalf("post-grant should NOT deny with consent_required; got %+v", failure)
	}

	// Revoke: deny again.
	mgr.Revoke()
	_, failure = d.Dispatch(context.Background(), dispatch)
	if failure == nil || failure.GetErrorCode() != "consent_required" {
		t.Errorf("post-revoke should deny with consent_required; got %+v", failure)
	}
}

func getTestWindow(t *testing.T) time.Duration {
	t.Helper()
	return time.Hour
}
