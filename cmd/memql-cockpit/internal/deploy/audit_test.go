package deploy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditor_EmitMirrorAndFile(t *testing.T) {
	var buf bytes.Buffer
	logPath := filepath.Join(t.TempDir(), "audit.log")
	a := &Auditor{LogPath: logPath, Mirror: &buf}

	rec := AuditRecord{
		Command:        "deploy",
		Automation:     "deployEngineCluster",
		Env:            "staging",
		Role:           RoleDeveloper,
		Gate:           GateForward,
		Decision:       DecisionAllowed,
		CockpitVersion: "0.9.0",
		BundleVersion:  "sha256:abc",
	}
	if err := a.Emit(rec); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Mirror got an "AUDIT " prefixed JSON line.
	if !strings.HasPrefix(buf.String(), "AUDIT ") {
		t.Errorf("mirror missing AUDIT prefix: %q", buf.String())
	}

	// File got valid JSONL with the fields + a stamped time.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var got AuditRecord
	if err := json.Unmarshal(bytes.TrimSpace(data), &got); err != nil {
		t.Fatalf("unmarshal audit line: %v (%s)", err, data)
	}
	if got.Command != "deploy" || got.Decision != DecisionAllowed || got.Gate != GateForward {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Time.IsZero() {
		t.Error("Emit did not stamp a time")
	}
}

func TestAuditor_EmitAppends(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	a := &Auditor{LogPath: logPath}
	for i := 0; i < 3; i++ {
		if err := a.Emit(AuditRecord{Command: "run", Decision: DecisionAllowed}); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}
	data, _ := os.ReadFile(logPath)
	if n := strings.Count(strings.TrimSpace(string(data)), "\n"); n != 2 { // 3 lines = 2 newlines between
		t.Errorf("expected 3 appended lines, got %d separators in %q", n, data)
	}
}
