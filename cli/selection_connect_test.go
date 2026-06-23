package cli

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/cli/config"
)

func closedSnapshot(e *connEntry) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// startedLifecycleNames returns the pool entries whose lifecycle has
// been launched. Same-package test helper for the single-connection
// invariant.
func startedLifecycleNames(a *App) []string {
	a.poolMu.RLock()
	defer a.poolMu.RUnlock()
	var out []string
	for name, entry := range a.pool {
		if lifecycleStartedSnapshot(entry) {
			out = append(out, name)
		}
	}
	return out
}

// TestSelectionDrivesConnectAndTeardown is the regression test for
// epic #239 / CM2 #241: selection DRIVES the single live connection.
// At boot only the selected cluster ("alpha") dials. Pressing Enter on
// a non-selected cluster ("beta") must:
//   - tear down alpha's live connection back to registered-idle, and
//   - bring up beta's full lifecycle,
//
// so exactly one entry runs a lifecycle at any time (the selected one).
func TestSelectionDrivesConnectAndTeardown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

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
	t.Cleanup(app.Quit)

	app.connect()

	// Boot invariant (CM1): alpha is the single live connection.
	if started := startedLifecycleNames(app); len(started) != 1 || started[0] != "alpha" {
		t.Fatalf("boot: expected only alpha's lifecycle started, got %v", started)
	}
	oldAlpha := app.pool["alpha"]
	if oldAlpha == nil {
		t.Fatal("alpha entry missing after connect")
	}

	// User presses Enter on beta -> selection drives the switch.
	app.setSelected("beta")

	// Selection moved + persisted.
	if got := app.selectedName(); got != "beta" {
		t.Fatalf("selected cluster = %q, want beta", got)
	}
	if clusters, err := config.LoadClusters(); err != nil {
		t.Fatalf("reload clusters: %v", err)
	} else if clusters.SelectedCluster != "beta" {
		t.Fatalf("persisted selection = %q, want beta", clusters.SelectedCluster)
	}

	// beta now carries the single live connection; the lifecycle is
	// launched synchronously inside openEntry, so this is deterministic.
	if started := startedLifecycleNames(app); len(started) != 1 || started[0] != "beta" {
		t.Fatalf("after switch: expected only beta's lifecycle started, got %v", started)
	}

	// alpha is demoted back to a registered-idle entry (listed, not
	// dialed) -- a fresh entry with no running lifecycle.
	newAlpha := app.pool["alpha"]
	if newAlpha == nil {
		t.Fatal("alpha row dropped from the pool after demotion; it must stay listed")
	}
	if newAlpha == oldAlpha {
		t.Fatal("alpha entry was not replaced on demotion")
	}
	if lifecycleStartedSnapshot(newAlpha) {
		t.Fatal("demoted alpha still runs a lifecycle; only the selected cluster should dial")
	}
	if state, _, _ := newAlpha.stateSnapshot(); state != stateIdle {
		t.Fatalf("demoted alpha state = %v, want idle", state)
	}

	// alpha's OLD connection is torn down (Close runs off the UI
	// thread, so allow a brief settle) -- proves the lifecycle stopped,
	// no orphaned stream.
	deadline := time.Now().Add(2 * time.Second)
	for !closedSnapshot(oldAlpha) {
		if time.Now().After(deadline) {
			t.Fatal("alpha's previous connection was never closed on switch (goroutine leak)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSetSelectedSameClusterIsNoop verifies that re-selecting the
// already-selected cluster does not tear down and re-dial its
// connection (no churn on a redundant Enter).
func TestSetSelectedSameClusterIsNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := config.SaveClusters(&config.ClustersFile{
		SelectedCluster: "alpha",
		Clusters: []config.ClusterConfig{
			{Name: "alpha", Endpoint: "127.0.0.1:1", PAT: "mql_pat_alpha"},
		},
	}); err != nil {
		t.Fatalf("seed clusters: %v", err)
	}

	app := NewApp(AppConfig{Version: "test"})
	t.Cleanup(app.Quit)
	app.connect()

	before := app.pool["alpha"]
	app.setSelected("alpha")
	after := app.pool["alpha"]

	if before != after {
		t.Fatal("re-selecting the active cluster replaced its entry; Enter on the working cluster must be a no-op")
	}
	if closedSnapshot(before) {
		t.Fatal("re-selecting the active cluster closed its live connection")
	}
}
