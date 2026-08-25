package tools

import "testing"

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
