package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultPolicy_ShellRlimitsNonZero asserts the post-Wave-6
// hardening: the default ShellPolicy ships non-zero rlimits so a
// runaway shell exec can't exhaust the user session's CPU / memory
// ceiling. Earlier defaults left these at zero (inherit parent),
// which was the runaway-blast-radius footgun.
func TestDefaultPolicy_ShellRlimitsNonZero(t *testing.T) {
	p := DefaultPolicy()
	if got := p.shell.MaxCPUSeconds; got <= 0 {
		t.Errorf("MaxCPUSeconds = %d, want > 0 (default cap)", got)
	}
	if got := p.shell.MaxMemoryMB; got <= 0 {
		t.Errorf("MaxMemoryMB = %d, want > 0 (default cap)", got)
	}
	if got := p.shell.MaxOpenFiles; got <= 0 {
		t.Errorf("MaxOpenFiles = %d, want > 0 (default cap)", got)
	}
}

// TestDefaultPolicy_AllowDenyShape sanity-checks the curated lists
// haven't drifted -- a regression here is a sign someone deleted
// the curated allow / deny by mistake (the lists are intentionally
// conservative).
func TestDefaultPolicy_AllowDenyShape(t *testing.T) {
	p := DefaultPolicy()
	if len(p.shell.Allow) == 0 {
		t.Fatalf("ShellPolicy.Allow must not be empty -- default allow-list missing")
	}
	// The deny list is hard-coded to a curated set; spot-check a few
	// known entries so an accidental wipe of the slice is caught.
	mustDeny := []string{"rm", "dd", "sudo", "kill"}
	denySet := make(map[string]bool, len(p.shell.Deny))
	for _, d := range p.shell.Deny {
		denySet[d] = true
	}
	for _, want := range mustDeny {
		if !denySet[want] {
			t.Errorf("default deny list missing %q", want)
		}
	}
}

// TestAppsAllow_DefaultsToDeny. An app session does exactly what
// workerHost.exec does -- edits files and runs commands on somebody's own
// computer -- so it gets this file's posture: nothing runs until the
// machine's owner names it. Every machine upgrading into the feature has
// an empty list, and empty must not read as "all".
func TestAppsAllow_DefaultsToDeny(t *testing.T) {
	if got := DefaultPolicy().AppsAllow(); len(got) != 0 {
		t.Errorf("default apps.allow = %v, want empty (default-deny)", got)
	}
	var nilPolicy *Policy
	if got := nilPolicy.AppsAllow(); got != nil {
		t.Errorf("nil policy apps.allow = %v, want nil", got)
	}
}

// TestAppsAllow_LoadsFromPolicyYAML pins the shape the engine's runbook
// documents, so an operator following it gets what it promises.
func TestAppsAllow_LoadsFromPolicyYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	const doc = `apps:
  allow:
    - claude-code
    - codex
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	got := p.AppsAllow()
	if len(got) != 2 || got[0] != "claude-code" || got[1] != "codex" {
		t.Fatalf("apps.allow = %v, want [claude-code codex]", got)
	}

	// The returned slice must be a copy: the worker calls this on every
	// beat and a shared slice would race a SIGHUP reload mid-beat.
	got[0] = "mutated"
	if again := p.AppsAllow(); again[0] != "claude-code" {
		t.Errorf("AppsAllow handed out its backing array: %v", again)
	}
}

// TestAppsAllow_ReloadPicksUpANewApp: SIGHUP adds an app without a worker
// restart, matching the cadence signing into the app itself gets.
func TestAppsAllow_ReloadPicksUpANewApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("apps:\n  allow:\n    - claude-code\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if len(p.AppsAllow()) != 1 {
		t.Fatalf("apps.allow = %v", p.AppsAllow())
	}
	if err := os.WriteFile(path, []byte("apps:\n  allow:\n    - claude-code\n    - codex\n"), 0o600); err != nil {
		t.Fatalf("rewrite policy: %v", err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := p.AppsAllow(); len(got) != 2 {
		t.Errorf("after reload apps.allow = %v, want two entries", got)
	}
}
