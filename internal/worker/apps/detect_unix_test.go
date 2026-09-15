//go:build linux || darwin

package apps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDetector_VersionDoesNotWaitOnAChildHoldingItsOutput: an app behind a
// version-manager shim can leave a child holding stdout open after the
// probe has answered. The probe returns with what it printed once its own
// process is done, instead of waiting on the pipe -- the session's
// fingerprint asks for this version in front of the app's start.
func TestDetector_VersionDoesNotWaitOnAChildHoldingItsOutput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 5 &\necho '2.1.270 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Detector{LookPath: func(string) (string, error) { return bin, nil }}
	start := time.Now()
	got := d.Version(context.Background(), IDClaudeCode)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the probe waited %v on a child holding its output", elapsed)
	}
	if got != "2.1.270 (Claude Code)" {
		t.Errorf("version = %q, want what the probe printed", got)
	}
}
