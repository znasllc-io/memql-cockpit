package worker

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// Server-side validation contract for Register.capability_descriptor_json
// (memql#1331). Encoded here verbatim so the cockpit can never drift
// into producing a descriptor the server would reject with
// register_failed:
//
//   - raw JSON <= 4096 bytes
//   - schemaVersion == 1
//   - <= 64 actions
//   - every action name matches [A-Za-z0-9._-]{1,64}
//   - no duplicate action names
const (
	registerDescriptorMaxBytes   = 4096
	registerDescriptorSchemaWant = 1
	registerDescriptorMaxActions = 64
)

var registerActionNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// TestBuildRegister_CarriesValidCapabilityDescriptor asserts the
// Register handshake message populates capability_descriptor_json
// (memql-cockpit#166) with JSON that satisfies every rule the memql
// server validates (memql#1331), and that it matches the exact
// descriptor workerComputer.capabilities would return -- one source
// of truth, two surfaces.
func TestBuildRegister_CarriesValidCapabilityDescriptor(t *testing.T) {
	register := buildRegister(Config{
		Name:         "test-worker",
		Capabilities: []string{"HEADLESS"},
		Concurrency:  map[string]uint32{"HEADLESS": 1},
	}, nil)

	raw := register.GetCapabilityDescriptorJson()
	if raw == "" {
		t.Fatal("Register must carry capability_descriptor_json")
	}
	if len(raw) > registerDescriptorMaxBytes {
		t.Fatalf("descriptor is %d bytes; server rejects > %d", len(raw), registerDescriptorMaxBytes)
	}

	var desc tools.CapabilityDescriptor
	if err := json.Unmarshal([]byte(raw), &desc); err != nil {
		t.Fatalf("capability_descriptor_json is not valid descriptor JSON: %v (raw: %s)", err, raw)
	}
	if desc.SchemaVersion != registerDescriptorSchemaWant {
		t.Errorf("schemaVersion = %d; server requires %d", desc.SchemaVersion, registerDescriptorSchemaWant)
	}
	if len(desc.Actions) > registerDescriptorMaxActions {
		t.Errorf("%d actions; server rejects > %d", len(desc.Actions), registerDescriptorMaxActions)
	}
	seen := make(map[string]bool, len(desc.Actions))
	for _, a := range desc.Actions {
		if !registerActionNamePattern.MatchString(a) {
			t.Errorf("action %q does not match the server's name pattern %s", a, registerActionNamePattern)
		}
		if seen[a] {
			t.Errorf("duplicate action %q; server rejects duplicates", a)
		}
		seen[a] = true
	}
	if desc.Platform == "" {
		t.Error("descriptor must carry the platform")
	}
	if desc.DisplayServer == "" {
		t.Error("descriptor must carry the displayServer")
	}

	// The handshake descriptor and the capabilities action must agree
	// byte-for-byte: both serialize tools.ComputeCapabilities().
	want, err := tools.CapabilityDescriptorJSON()
	if err != nil {
		t.Fatalf("CapabilityDescriptorJSON: %v", err)
	}
	if raw != want {
		t.Errorf("handshake descriptor diverges from the capabilities action:\nregister: %s\naction:   %s", raw, want)
	}
}
