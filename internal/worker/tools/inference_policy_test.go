package tools_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

func policyFrom(t *testing.T, body string) *tools.Policy {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return p
}

// DEFAULT OWNER. A machine that says nothing has not consented to
// serving anybody else's prompt, and reading absence as permissive
// would hand a stranger somebody's GPU on an upgrade.
func TestInferenceServeDefaultsToOwner(t *testing.T) {
	if got := tools.DefaultPolicy().InferenceServe(); got != tools.ServeOwner {
		t.Fatalf("DefaultPolicy = %q, want %q", got, tools.ServeOwner)
	}
	if got := policyFrom(t, "shell:\n  allow: []\n").InferenceServe(); got != tools.ServeOwner {
		t.Fatalf("absent inference.serve = %q, want %q", got, tools.ServeOwner)
	}
}

func TestInferenceServeReadsTheGrant(t *testing.T) {
	if got := policyFrom(t, "inference:\n  serve: cluster\n").InferenceServe(); got != tools.ServeCluster {
		t.Fatalf("inference.serve: cluster -> %q, want %q", got, tools.ServeCluster)
	}
}

// An unrecognised value is OWNER: not an error, and not the grant. A
// typo must not widen a permission, and it must not stop a worker
// starting either -- a machine that refused to boot over a misspelled
// sharing preference is a machine nobody can reach to fix it.
func TestInferenceServeUnknownValueIsOwner(t *testing.T) {
	for _, body := range []string{
		"inference:\n  serve: everyone\n",
		"inference:\n  serve: Cluster\n", // case matters; the engine's spelling is lower
		"inference:\n  serve: \"\"\n",
		"inference:\n  serve: true\n",
	} {
		if got := policyFrom(t, body).InferenceServe(); got != tools.ServeOwner {
			t.Fatalf("%q -> %q, want %q", body, got, tools.ServeOwner)
		}
	}
}

// A SIGHUP that REMOVED the key returns this machine to owner. The
// field replaces rather than merges, so a consent cannot become
// unrevokable without a restart -- the wrong direction for the one
// setting that hands a stranger this machine's GPU.
func TestInferenceServeIsRevokableByReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("inference:\n  serve: cluster\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.InferenceServe() != tools.ServeCluster {
		t.Fatal("setup: want cluster before the reload")
	}
	if err := os.WriteFile(path, []byte("shell:\n  allow: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := p.InferenceServe(); got != tools.ServeOwner {
		t.Fatalf("after removing the key: %q, want %q", got, tools.ServeOwner)
	}
}

// -----------------------------------------------------------------------------
// The wire
// -----------------------------------------------------------------------------

// The key rides the EXISTING capability_descriptor_json field, which
// the engine's ParseCapabilityDescriptor tolerates unknown keys in.
func TestCapabilityDescriptorCarriesInferenceServe(t *testing.T) {
	for _, serve := range []string{tools.ServeOwner, tools.ServeCluster} {
		raw, err := tools.CapabilityDescriptorJSONFor(serve, nil)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
		if got["inferenceServe"] != serve {
			t.Fatalf("inferenceServe = %v, want %q", got["inferenceServe"], serve)
		}
	}
}

// THE VERSION MUST NOT MOVE. The engine admits exactly 1 and refuses
// anything else BY VALUE ("unsupported schemaVersion %d"), so bumping
// it for an additive field fails every registration on the fleet at
// once -- and the failure is a handshake refusal, not a missing field,
// so every machine goes offline rather than losing one capability.
//
// This is the same rule Displays shipped under, written down where the
// next additive field will be added.
func TestCapabilityDescriptorSchemaVersionStaysOne(t *testing.T) {
	raw, err := tools.CapabilityDescriptorJSONFor(tools.ServeCluster, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got["schemaVersion"] != float64(1) {
		t.Fatalf("schemaVersion = %v, want 1 -- see the engine's component/worker/capability_descriptor.go, "+
			"which refuses any other value and would fail every registration on the fleet",
			got["schemaVersion"])
	}
}

// A bare string from any caller is normalised the same way the policy
// normalises one. Anything that is not the grant is the default.
func TestCapabilityDescriptorNormalisesAnUnknownConsent(t *testing.T) {
	raw, err := tools.CapabilityDescriptorJSONFor("everyone", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got["inferenceServe"] != tools.ServeOwner {
		t.Fatalf("inferenceServe = %v, want %q", got["inferenceServe"], tools.ServeOwner)
	}
}

// The no-argument form still exists and still reports the closed
// default, so a caller that has no policy in hand cannot accidentally
// advertise a grant.
func TestCapabilityDescriptorJSONDefaultsClosed(t *testing.T) {
	raw, err := tools.CapabilityDescriptorJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got["inferenceServe"] != tools.ServeOwner {
		t.Fatalf("inferenceServe = %v, want %q", got["inferenceServe"], tools.ServeOwner)
	}
}
