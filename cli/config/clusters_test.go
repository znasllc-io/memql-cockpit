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

func TestStoredTokenExpiry(t *testing.T) {
	t.Run("zero time is expired", func(t *testing.T) {
		token := &StoredToken{}
		if !token.IsExpired() {
			t.Error("zero expiry should be expired")
		}
	})
}
