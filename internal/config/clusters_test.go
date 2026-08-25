package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClustersFileGet(t *testing.T) {
	file := &ClustersFile{
		Clusters: []ClusterConfig{
			{Name: "local", Endpoint: "localhost:50051"},
			{Name: "staging", Endpoint: "staging.example.com:50051"},
		},
	}

	t.Run("found", func(t *testing.T) {
		c, ok := file.Get("local")
		if !ok {
			t.Fatal("expected to find 'local'")
		}
		if c.Endpoint != "localhost:50051" {
			t.Errorf("expected localhost:50051, got %s", c.Endpoint)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, ok := file.Get("production")
		if ok {
			t.Error("expected 'production' not to be found")
		}
	})
}

// TestClusterLocalFieldRoundTrip pins the half of the shared-file
// contract the cockpit owns: ~/.memql/clusters.yaml is written by BOTH
// the cockpit and the memQL VS Code extension, and `local:` -- which
// gates the extension's non-local write confirmation -- has to survive
// a cockpit load/save cycle rather than being dropped on the floor
// (znasllc-io/memql#3313).
//
// The other half of the contract is the extension's: it writes through
// the yaml Document API precisely because the cockpit's plain struct
// marshal, asserted below, does NOT preserve keys it does not model.
// That is why the cockpit has to model `local` explicitly instead of
// relying on the round trip to carry it.
func TestClusterLocalFieldRoundTrip(t *testing.T) {
	// A file as the extension writes it: `local: true` plus a key this
	// Go version does not know about.
	const onDisk = `clusters:
    - name: staging
      endpoint: https://cockpit.staging.example.com
      local: true
      future_key: kept-by-the-extension
selected_cluster: staging
`

	var loaded ClustersFile
	if err := yaml.Unmarshal([]byte(onDisk), &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(loaded.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(loaded.Clusters))
	}
	if !loaded.Clusters[0].Local {
		t.Error("local: true did not decode onto ClusterConfig.Local")
	}

	// Save it back the way the cockpit does, then reload: Local has to
	// be there both on the wire and in the decoded struct.
	saved, err := yaml.Marshal(&loaded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(saved), "local: true") {
		t.Errorf("re-marshalled file lost `local: true`:\n%s", saved)
	}

	var reloaded ClustersFile
	if err := yaml.Unmarshal(saved, &reloaded); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if !reloaded.Clusters[0].Local {
		t.Error("Local did not survive marshal -> unmarshal")
	}

	// A cluster the cockpit writes with Local unset stays quiet on the
	// wire -- omitempty, so an untouched registry does not grow a
	// `local: false` on every save.
	quiet, err := yaml.Marshal(&ClustersFile{
		Clusters: []ClusterConfig{{Name: "prod", Endpoint: "https://cockpit.example.com"}},
	})
	if err != nil {
		t.Fatalf("marshal quiet: %v", err)
	}
	if strings.Contains(string(quiet), "local:") {
		t.Errorf("Local=false should be omitted, got:\n%s", quiet)
	}

	// The documented limitation, asserted so it cannot drift silently:
	// a struct round trip drops any key the cockpit does not model. If
	// this ever starts passing through unknown keys, revisit the note
	// above (and the extension's reason for using the Document API).
	if strings.Contains(string(saved), "future_key") {
		t.Error("unknown keys now survive the struct round trip -- update the shared-file note")
	}
}

// TestClusterVersionFieldRoundTrip is TestClusterLocalFieldRoundTrip's twin,
// for the `version:` key the memQL VS Code plugin records per cluster
// (znasllc-io/memql#3994).
//
// The hazard is the same and worth restating, because it is the whole reason
// this test exists rather than the field just being added: the cockpit saves
// by marshalling ClusterConfig over the file, so any key it does not model is
// gone the next time an operator edits a cluster here. The plugin's recorded
// version is written far more often than a human edit -- it refreshes
// opportunistically from four sources -- so "the cockpit quietly deletes it"
// would present as the upgrade banner vanishing for no reason an operator
// could connect to anything they did.
//
// The version is a plain string, which is why this is simpler than `local`:
// there is no false-serialises-as-absent collapse for the two tools to agree
// about. omitempty drops only "", and "" is not a version either tool writes.
func TestClusterVersionFieldRoundTrip(t *testing.T) {
	// A file as the plugin writes it, with the version alongside `local`.
	const onDisk = `clusters:
    - name: staging
      endpoint: https://cockpit.staging.example.com
      local: true
      version: v0.18.0
selected_cluster: staging
`

	var loaded ClustersFile
	if err := yaml.Unmarshal([]byte(onDisk), &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(loaded.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(loaded.Clusters))
	}
	if got := loaded.Clusters[0].Version; got != "v0.18.0" {
		t.Errorf("version did not decode onto ClusterConfig.Version: got %q", got)
	}

	// Save it back the way the cockpit does, then reload.
	saved, err := yaml.Marshal(&loaded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(saved), "version: v0.18.0") {
		t.Errorf("re-marshalled file lost `version: v0.18.0`:\n%s", saved)
	}

	var reloaded ClustersFile
	if err := yaml.Unmarshal(saved, &reloaded); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if got := reloaded.Clusters[0].Version; got != "v0.18.0" {
		t.Errorf("Version did not survive marshal -> unmarshal: got %q", got)
	}

	// Whatever the cluster said, unmangled. The record is a report, not a
	// judgement: a branch name, a commit sha, or the `0.15.0-<epoch>` build
	// stamp every shipped engine reports are all legitimate values, and the
	// plugin's comparator can only refuse to compare a value that reached
	// disk intact. Nothing here may normalise or reject them.
	for _, recorded := range []string{"0.18.0", "0.15.0-1737072000", "feat/some-branch", "9fd53842"} {
		round, err := yaml.Marshal(&ClustersFile{
			Clusters: []ClusterConfig{{Name: "c", Version: recorded}},
		})
		if err != nil {
			t.Fatalf("marshal %q: %v", recorded, err)
		}
		var back ClustersFile
		if err := yaml.Unmarshal(round, &back); err != nil {
			t.Fatalf("unmarshal %q: %v", recorded, err)
		}
		if got := back.Clusters[0].Version; got != recorded {
			t.Errorf("version %q round-tripped as %q", recorded, got)
		}
	}

	// A cluster the cockpit writes with no known version stays quiet on the
	// wire. Every cluster in an operator's file predates this key, so absent
	// is the normal case and a save must not grow `version: ""` on all of
	// them.
	quiet, err := yaml.Marshal(&ClustersFile{
		Clusters: []ClusterConfig{{Name: "prod", Endpoint: "https://cockpit.example.com"}},
	})
	if err != nil {
		t.Fatalf("marshal quiet: %v", err)
	}
	if strings.Contains(string(quiet), "version:") {
		t.Errorf("an unset Version should be omitted, got:\n%s", quiet)
	}
}

func TestStoredTokenExpiry(t *testing.T) {
	t.Run("zero time is expired", func(t *testing.T) {
		token := &StoredToken{}
		if !token.IsExpired() {
			t.Error("zero expiry should be expired")
		}
	})
}
