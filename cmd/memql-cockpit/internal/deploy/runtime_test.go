package deploy

import (
	"context"
	"errors"
	"testing"
)

// TestEmbeddedRuntime_ResolvesExistingAutomation proves the embedding end to
// end: the cockpit constructs an in-process memQL engine + automation loader
// from the memql module and resolves a real existing engine automation by
// name out of the embedded DSL tree -- no database, no remote cluster. This
// is the smoke proof that `run <automation>` rides the embedded runtime.
func TestEmbeddedRuntime_ResolvesExistingAutomation(t *testing.T) {
	rt := NewEmbeddedRuntime(nil)
	if err := rt.Resolve("pruneStaleClusterNodes"); err != nil {
		t.Fatalf("Resolve(pruneStaleClusterNodes): %v", err)
	}
}

func TestEmbeddedRuntime_UnknownAutomationNotFound(t *testing.T) {
	rt := NewEmbeddedRuntime(nil)
	err := rt.Resolve("definitelyNotAnAutomation_zzz")
	if err == nil {
		t.Fatal("expected an error for an unknown automation")
	}
	if !errors.Is(err, ErrAutomationNotFound) {
		t.Errorf("error = %v, want wrapped ErrAutomationNotFound", err)
	}
}

// TestEmbeddedRuntime_RunInvokesExecutor proves the run reaches the embedded
// executor. With a bare (DB-less) engine a DB-backed automation surfaces an
// honest "memory engine not initialized" step error rather than a fake
// success; the run itself does not error at the cockpit boundary, and the
// result records that the executor was invoked. A fully-initialized engine
// for durable deployment-record mutations lands with I13 (memql#2220).
func TestEmbeddedRuntime_RunInvokesExecutor(t *testing.T) {
	rt := NewEmbeddedRuntime(nil)
	res, err := rt.Run(context.Background(), RunRequest{
		Automation: "pruneStaleClusterNodes",
		Owner:      "test",
		Input:      map[string]any{},
	})
	if err != nil {
		t.Fatalf("Run returned a boundary error: %v", err)
	}
	if !res.Resolved {
		t.Error("expected Resolved=true")
	}
	if !res.Executed {
		t.Error("expected Executed=true (executor invoked)")
	}
}

// TestEmbeddedRuntime_DryRunResolvesNoExecute confirms a dry run stops at
// resolution with no execution side effects.
func TestEmbeddedRuntime_DryRunResolvesNoExecute(t *testing.T) {
	rt := NewEmbeddedRuntime(nil)
	res, err := rt.Run(context.Background(), RunRequest{
		Automation: "pruneStaleClusterNodes",
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	if !res.Resolved || res.Executed {
		t.Errorf("dry run should resolve without executing: %+v", res)
	}
}
