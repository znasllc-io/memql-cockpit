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
	called  bool
	lastReq RunRequest
	result  RunResult
	err     error
}

func (f *fakeRuntime) Resolve(name string) error { return f.err }
func (f *fakeRuntime) Run(_ context.Context, req RunRequest) (RunResult, error) {
	f.called = true
	f.lastReq = req
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

// TestRunInvocation_DeployResolvesAndInvokes is the I15 acceptance: now that
// deployEngineCluster (I10) is in the bundle, `deploy --env` resolves + invokes
// it through the embedded runtime under the deploy.requested topic — no more
// "blocked until I10" exit-3 stub.
func TestRunInvocation_DeployResolvesAndInvokes(t *testing.T) {
	var buf bytes.Buffer
	rt := &fakeRuntime{result: RunResult{Resolved: true, Executed: true, Status: "completed", ExecutionID: "exec-9"}}
	inv := invocation{
		command:    "deploy",
		automation: deployAutomation,
		env:        "development",
		role:       RoleDeveloper,
		gate:       GateForward,
		topic:      deployRequestedTopic,
		input:      map[string]any{"environment": "development"},
	}

	code := runInvocation(inv, "0.9.0", rt, &Auditor{Mirror: &buf})

	if code != 0 {
		t.Errorf("exit = %d want 0", code)
	}
	if !rt.called {
		t.Fatal("deploy must invoke the embedded runtime")
	}
	if rt.lastReq.Automation != deployAutomation {
		t.Errorf("runtime got automation %q want %q", rt.lastReq.Automation, deployAutomation)
	}
	if rt.lastReq.Topic != deployRequestedTopic {
		t.Errorf("runtime got topic %q want %q", rt.lastReq.Topic, deployRequestedTopic)
	}
	if rt.lastReq.Input["environment"] != "development" {
		t.Errorf("runtime input missing environment payload: %+v", rt.lastReq.Input)
	}
	if rec := lastAudit(t, &buf); rec.Decision != DecisionAllowed || rec.Status != "completed" {
		t.Errorf("audit mismatch: %+v", rec)
	}
}

// TestRunInvocation_DeployNoEngineDBReportsHonestly: a real (non-dry-run)
// deploy against the DB-less embedded engine surfaces an honest
// "memory engine not initialized" step error. The command reports it as an
// owner-gated blocked-on-DB outcome — NOT a crash and NOT the old exit-3 stub —
// and records the step error in the audit trail.
func TestRunInvocation_DeployNoEngineDBReportsHonestly(t *testing.T) {
	var buf bytes.Buffer
	rt := &fakeRuntime{result: RunResult{
		Resolved:  true,
		Executed:  true,
		ExecError: `function "deploymentForwardAllowed" execution failed: memory engine not initialized`,
	}}
	inv := invocation{command: "deploy", automation: deployAutomation, env: "development", role: RoleDeveloper, gate: GateForward, input: map[string]any{}}

	code := runInvocation(inv, "0.9.0", rt, &Auditor{Mirror: &buf})

	if code != 0 {
		t.Errorf("exit = %d want 0 (honest blocked-on-DB, not a hard error/stub)", code)
	}
	rec := lastAudit(t, &buf)
	if !strings.Contains(rec.Reason, "memory engine not initialized") {
		t.Errorf("audit should record the no-DB step error, got reason %q", rec.Reason)
	}
}

func TestApplyDeployDefaults_BuildsDeployRequestedPayload(t *testing.T) {
	inv := invocation{command: "deploy", env: "staging", ref: "v0.12.0", actor: "alice", input: map[string]any{}}
	applyDeployDefaults(&inv)

	if inv.input["environment"] != "staging" {
		t.Errorf("environment = %v want staging", inv.input["environment"])
	}
	if inv.input["ref"] != "v0.12.0" || inv.input["version"] != "v0.12.0" {
		t.Errorf("ref/version not set from --ref: %+v", inv.input)
	}
	if inv.input["triggeredBy"] != "alice" {
		t.Errorf("triggeredBy = %v want alice", inv.input["triggeredBy"])
	}
	if _, ok := inv.input["deploymentId"].(string); !ok {
		t.Errorf("deploymentId missing/!string: %+v", inv.input["deploymentId"])
	}
	nt, ok := inv.input["engineNodeTypes"].([]string)
	if !ok || len(nt) == 0 {
		t.Fatalf("engineNodeTypes missing: %+v", inv.input["engineNodeTypes"])
	}
}

// applyDeployDefaults must not clobber operator-supplied --input values.
func TestApplyDeployDefaults_RespectsOperatorOverrides(t *testing.T) {
	inv := invocation{command: "deploy", env: "development", actor: "bob", input: map[string]any{
		"deploymentId":    "dep-pinned",
		"engineNodeTypes": []string{"cognition"},
	}}
	applyDeployDefaults(&inv)

	if inv.input["deploymentId"] != "dep-pinned" {
		t.Errorf("deploymentId override clobbered: %v", inv.input["deploymentId"])
	}
	if nt, _ := inv.input["engineNodeTypes"].([]string); len(nt) != 1 || nt[0] != "cognition" {
		t.Errorf("engineNodeTypes override clobbered: %v", inv.input["engineNodeTypes"])
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
