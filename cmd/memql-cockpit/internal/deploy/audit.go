package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Decision is the recorded outcome of the role-gate check.
type Decision string

const (
	DecisionAllowed Decision = "allowed"
	DecisionDenied  Decision = "denied"
)

// AuditRecord is one immutable audit-trail entry for a deploy / run
// invocation. Every cockpit-driven deployment action emits one (handoff
// "Make → cockpit ... enforces the role specs and an audit trail").
type AuditRecord struct {
	Time           time.Time `json:"time"`
	Command        string    `json:"command"`          // "deploy" | "run"
	Automation     string    `json:"automation"`       // resolved automation name
	Env            string    `json:"env,omitempty"`    // deploy --env
	Ref            string    `json:"ref,omitempty"`    // deploy --ref
	Actor          string    `json:"actor,omitempty"`  // caller identity
	Role           Role      `json:"role"`             // caller role
	Gate           Gate      `json:"gate"`             // role gate evaluated
	Decision       Decision  `json:"decision"`         // allowed | denied
	Reason         string    `json:"reason,omitempty"` // denial / failure reason
	CockpitVersion string    `json:"cockpitVersion"`   // reproducibility pin
	BundleVersion  string    `json:"bundleVersion"`    // reproducibility pin
	DryRun         bool      `json:"dryRun,omitempty"` //
	Status         string    `json:"status,omitempty"` // automation execution status
	ExecutionID    string    `json:"executionId,omitempty"`
}

// Auditor writes audit records. The default writes JSONL to both an audit
// log file and an optional mirror writer (stderr) so the trail survives the
// process and is visible in CI logs. A nil sink writer disables the mirror.
type Auditor struct {
	// LogPath is the JSONL file the record is appended to. Empty disables
	// the file sink (used in tests).
	LogPath string
	// Mirror, when non-nil, also receives the JSON line (e.g. os.Stderr).
	Mirror io.Writer
}

// DefaultAuditor returns an Auditor that appends to
// ~/.memql/cockpit/audit.log and mirrors to stderr.
func DefaultAuditor() *Auditor {
	a := &Auditor{Mirror: os.Stderr}
	if home, err := os.UserHomeDir(); err == nil {
		a.LogPath = filepath.Join(home, ".memql", "cockpit", "audit.log")
	}
	return a
}

// Emit serializes rec as a single JSON line to the configured sinks. A
// file-sink failure is reported but never blocks the operation: the mirror
// (stderr) keeps the trail visible even when the log file is unwritable.
func (a *Auditor) Emit(rec AuditRecord) error {
	if rec.Time.IsZero() {
		rec.Time = time.Now().UTC()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}
	line = append(line, '\n')

	if a.Mirror != nil {
		_, _ = a.Mirror.Write(append([]byte("AUDIT "), line...))
	}

	if a.LogPath == "" {
		return nil
	}
	if mkErr := os.MkdirAll(filepath.Dir(a.LogPath), 0o755); mkErr != nil {
		return fmt.Errorf("create audit dir: %w", mkErr)
	}
	f, openErr := os.OpenFile(a.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if openErr != nil {
		return fmt.Errorf("open audit log: %w", openErr)
	}
	defer f.Close()
	if _, wErr := f.Write(line); wErr != nil {
		return fmt.Errorf("append audit log: %w", wErr)
	}
	return nil
}
