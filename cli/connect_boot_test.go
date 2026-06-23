package cli

import (
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/config"
)

// lifecycleStartedSnapshot reads e.lifecycleStarted under the entry
// lock. Same-package test helper for the boot-connection invariant.
func lifecycleStartedSnapshot(e *connEntry) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lifecycleStarted
}

// TestConnectBootOpensOnlySelected is the regression test for epic
// #239 / CM1 #240: at boot the cockpit must open a live connection
// (a running lifecycle) for the SELECTED cluster only. Every other
// configured cluster is registered into the pool (so its row renders)
// but is NOT dialed -- no lifecycle goroutine, no stream, no
// subscriber, no token refresher. Before this change connect() looped
// every config and called openEntry on each, so N clusters meant N
// concurrent dials at launch (the flicker / dial-storm bug).
func TestConnectBootOpensOnlySelected(t *testing.T) {
	// Isolate ~/.memql to a temp dir (config.ConfigDir uses $HOME).
	t.Setenv("HOME", t.TempDir())

	// Two fully-configured user clusters (endpoint + PAT => NeedsAuth
	// is false and the PAT is the credential, so each WOULD dial under
	// the old all-cluster boot). "alpha" is the sticky selection.
	if err := config.SaveClusters(&config.ClustersFile{
		SelectedCluster: "alpha",
		Clusters: []config.ClusterConfig{
			{Name: "alpha", Endpoint: "127.0.0.1:1", PAT: "mql_pat_alpha"},
			{Name: "beta", Endpoint: "127.0.0.1:1", PAT: "mql_pat_beta"},
		},
	}); err != nil {
		t.Fatalf("seed clusters: %v", err)
	}

	app := NewApp(AppConfig{Version: "test"})
	// Close any started lifecycle goroutine when the test ends so the
	// detached dial doesn't outlive the test.
	t.Cleanup(app.Quit)

	app.connect()

	// lifecycleStarted is set synchronously inside openEntry, before
	// the lifecycle goroutine is launched -- so the invariant is
	// deterministic the moment connect() returns, no polling needed.
	app.poolMu.RLock()
	defer app.poolMu.RUnlock()

	var started []string
	for name, entry := range app.pool {
		if lifecycleStartedSnapshot(entry) {
			started = append(started, name)
		}
	}
	if len(started) != 1 || started[0] != "alpha" {
		t.Fatalf("expected exactly one started lifecycle (alpha), got %v", started)
	}

	// The non-selected, fully-configured cluster must still be listed
	// in the pool (so its row renders) but sit in stateIdle -- never
	// dialed. beta has no lifecycle goroutine, so this is stable.
	beta, ok := app.pool["beta"]
	if !ok || beta == nil {
		t.Fatal("beta is not registered in the pool; its row would not render")
	}
	if state, _, _ := beta.stateSnapshot(); state != stateIdle {
		t.Fatalf("beta should be registered idle (listed, not dialed); got %v", state)
	}
	if lifecycleStartedSnapshot(beta) {
		t.Fatal("beta started a lifecycle at boot; only the selected cluster should dial")
	}

	// The selected cluster's entry is the one carrying the live
	// connection lifecycle.
	alpha, ok := app.pool["alpha"]
	if !ok || alpha == nil {
		t.Fatal("alpha (selected) missing from the pool")
	}
	if !lifecycleStartedSnapshot(alpha) {
		t.Fatal("alpha (selected) did not start its lifecycle at boot")
	}
}
