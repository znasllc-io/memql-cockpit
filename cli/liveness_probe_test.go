package cli

import (
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/config"
)

// rowStatus returns the rendered status string for a cluster row.
func rowStatus(a *App, name string) string {
	a.clustersView.Mu.RLock()
	defer a.clustersView.Mu.RUnlock()
	for _, c := range a.clustersView.Clusters {
		if c.Config.Name == name {
			return c.Status
		}
	}
	return ""
}

// TestLivenessProbeMarksUnreachable is the down-path regression test
// for epic #239 / CM3 #242: a non-selected, registered-idle cluster
// gets a liveness probe, and an unreachable endpoint drives its row to
// "unreachable" without ever opening a full connection (no lifecycle).
func TestLivenessProbeMarksUnreachable(t *testing.T) {
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

	// beta is registered-idle and eligible for probing.
	beta := app.pool["beta"]
	if beta == nil || !beta.probeReachableEligible() {
		t.Fatal("beta should be a registered-idle, probe-eligible entry")
	}

	// probeSweep blocks until the bounded worker pool drains, so the
	// result is observable as soon as it returns.
	app.probeSweep()

	if got := beta.probeStatusSnapshot(); got != probeUnreachable {
		t.Fatalf("beta probe = %v, want probeUnreachable", got)
	}
	// beta never started a lifecycle -- the probe is NOT a full connection.
	if lifecycleStartedSnapshot(beta) {
		t.Fatal("liveness probe started a full lifecycle; it must be connect-and-close only")
	}
	if got := rowStatus(app, "beta"); got != "unreachable" {
		t.Fatalf("beta row status = %q, want unreachable", got)
	}
}

// TestLivenessProbeSkipsSelected verifies the selected cluster (which
// holds a real connection) is never liveness-probed.
func TestLivenessProbeSkipsSelected(t *testing.T) {
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

	app.probeSweep()

	alpha := app.pool["alpha"]
	if alpha == nil {
		t.Fatal("alpha missing")
	}
	if got := alpha.probeStatusSnapshot(); got != probeNone {
		t.Fatalf("selected cluster alpha was probed (probe=%v); it must be skipped", got)
	}
}

// TestLivenessProbeSkipsNonIdle verifies a cluster that isn't
// registered-idle (here: needs-auth, no credentials) is not probed --
// there's nothing to probe.
func TestLivenessProbeSkipsNonIdle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := config.SaveClusters(&config.ClustersFile{
		SelectedCluster: "alpha",
		Clusters: []config.ClusterConfig{
			{Name: "alpha", Endpoint: "127.0.0.1:1", PAT: "mql_pat_alpha"},
			// gamma: endpoint but no PAT / issuer -> needs-auth, not idle.
			{Name: "gamma", Endpoint: "127.0.0.1:1"},
		},
	}); err != nil {
		t.Fatalf("seed clusters: %v", err)
	}

	app := NewApp(AppConfig{Version: "test"})
	t.Cleanup(app.Quit)
	app.connect()

	gamma := app.pool["gamma"]
	if gamma == nil {
		t.Fatal("gamma missing")
	}
	if gamma.probeReachableEligible() {
		t.Fatal("a needs-auth cluster must not be probe-eligible")
	}

	app.probeSweep()

	if got := gamma.probeStatusSnapshot(); got != probeNone {
		t.Fatalf("needs-auth cluster gamma was probed (probe=%v); it must be skipped", got)
	}
}

// TestSyncProbeRowStatusMapping locks the probe-result -> row-status
// string mapping (reusing the existing available / unreachable icons).
func TestSyncProbeRowStatusMapping(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveClusters(&config.ClustersFile{
		Clusters: []config.ClusterConfig{{Name: "beta", Endpoint: "127.0.0.1:1", PAT: "p"}},
	}); err != nil {
		t.Fatalf("seed clusters: %v", err)
	}
	app := NewApp(AppConfig{Version: "test"})
	app.refreshClusterList()

	cases := []struct {
		probe probeStatus
		want  string
	}{
		{probeReachable, "available"},
		{probeUnreachable, "unreachable"},
		{probeNone, "unknown"},
	}
	for _, tc := range cases {
		app.syncProbeRowStatus("beta", tc.probe)
		if got := rowStatus(app, "beta"); got != tc.want {
			t.Fatalf("probe %v -> row status %q, want %q", tc.probe, got, tc.want)
		}
	}
}
