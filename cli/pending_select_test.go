package cli

import "testing"

// TestPendingSelect covers the one-shot auto-select arming used by the
// Add flow: takePendingSelect returns true exactly once for the armed
// cluster and clears the arming.
func TestPendingSelect(t *testing.T) {
	a := &App{}

	if a.takePendingSelect("staging") {
		t.Fatal("takePendingSelect returned true before anything was armed")
	}

	a.setPendingSelect("staging")

	if a.takePendingSelect("other") {
		t.Fatal("takePendingSelect matched the wrong cluster")
	}
	if !a.takePendingSelect("staging") {
		t.Fatal("takePendingSelect should match the armed cluster")
	}
	if a.takePendingSelect("staging") {
		t.Fatal("takePendingSelect should be one-shot (cleared after taking)")
	}
}
