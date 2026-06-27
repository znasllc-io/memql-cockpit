package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fakeRuntime is a Runtime double so the command core (gate + audit +
// version pin + exit codes) is tested without the real engine.
type fakeRuntime struct {
	called bool
	result RunResult
	err    error
}

func (f *fakeRuntime) Resolve(name string) error { return f.err }
func (f *fakeRuntime) Run(_ context.Context, _ RunRequest) (RunResult, error) {
	f.called = true
	return f.result, f.err
}

// lastAudit parses the final "AUDIT {...}" line out of the mirror buffer.
func lastAudit(t *testing.T, buf *bytes.Buffer) AuditRecord {
	t.Helper()
	var rec AuditRecord
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "AUDIT "))
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit record found in: %q", buf.String())
	}
	return rec
}

func TestRunInvocation_DeniedBeforeRuntime(t *testing.T) {
	var buf bytes.Buffer
	rt := &fakeRuntime{}
	inv := invocation{command: "run", automation: "deployEngineCluster", role: RoleReader, gate: GateForward, input: map[string]any{}}

	code := runInvocation(inv, "0.9.0", rt, &Auditor{Mirror: &buf})

	if code != 1 {
		t.Errorf("exit = %d want 1", code)
	}
	if rt.called {
		t.Error("runtime should NOT be invoked when the gate denies")
	}
	rec := lastAudit(t, &buf)
	if rec.Decision != DecisionDenied {
		t.Errorf("decision = %q want denied", rec.Decision)
	}
	if rec.BundleVersion == "" || rec.CockpitVersion != "0.9.0" {
		t.Errorf("version pin missing in audit: %+v", rec)
	}
}

func TestRunInvocation_AllowedExecuted(t *testing.T) {
	var buf bytes.Buffer
	rt := &fakeRuntime{result: RunResult{Resolved: true, Executed: true, Status: "completed", ExecutionID: "exec-1"}}
	inv := invocation{command: "run", automation: "pruneStaleClusterNodes", role: RoleDeveloper, gate: GateForward, input: map[string]any{}}

	code := runInvocation(inv, "0.9.0", rt, &Auditor{Mirror: &buf})

	if code != 0 {
		t.Errorf("exit = %d want 0", code)
	}
	if !rt.called {
		t.Error("runtime should be invoked when the gate allows")
	}
	rec := lastAudit(t, &buf)
	if rec.Decision != DecisionAllowed || rec.Status != "completed" || rec.ExecutionID != "exec-1" {
		t.Errorf("audit mismatch: %+v", rec)
	}
}

func TestRunInvocation_DeployNotFoundIsBlocked(t *testing.T) {
	var buf bytes.Buffer
	rt := &fakeRuntime{err: fmt.Errorf("%w: %q", ErrAutomationNotFound, "deployEngineCluster")}
	inv := invocation{command: "deploy", automation: "deployEngineCluster", env: "development", role: RoleOwner, gate: GateForward, input: map[string]any{}}

	code := runInvocation(inv, "0.9.0", rt, &Auditor{Mirror: &buf})

	// Exit 3 == "blocked until I10", distinct from a hard error.
	if code != 3 {
		t.Errorf("exit = %d want 3 (blocked)", code)
	}
	rec := lastAudit(t, &buf)
	if rec.Decision != DecisionAllowed {
		t.Errorf("gate should still allow before the blocked runtime: %+v", rec)
	}
}

func TestRunInvocation_RunNotFoundIsError(t *testing.T) {
	var buf bytes.Buffer
	rt := &fakeRuntime{err: fmt.Errorf("%w: %q", ErrAutomationNotFound, "nope")}
	inv := invocation{command: "run", automation: "nope", role: RoleOwner, gate: GateForward, input: map[string]any{}}

	code := runInvocation(inv, "0.9.0", rt, &Auditor{Mirror: &buf})

	// run (not deploy) of a missing automation is a plain error, not exit 3.
	if code != 1 {
		t.Errorf("exit = %d want 1", code)
	}
}

func TestRunInvocation_DryRunResolved(t *testing.T) {
	var buf bytes.Buffer
	rt := &fakeRuntime{result: RunResult{Resolved: true}}
	inv := invocation{command: "run", automation: "pruneStaleClusterNodes", role: RoleAdmin, gate: GateForward, dryRun: true, input: map[string]any{}}

	if code := runInvocation(inv, "0.9.0", rt, &Auditor{Mirror: &buf}); code != 0 {
		t.Errorf("exit = %d want 0", code)
	}
	if rec := lastAudit(t, &buf); !rec.DryRun {
		t.Errorf("audit should record dryRun: %+v", rec)
	}
}
