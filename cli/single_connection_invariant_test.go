package cli

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/cluster"
	"github.com/znasllc-io/memql-cockpit/cli/config"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// simFrame renders the simulation screen's contents into one string
// (row-major, single rune per cell). Used to compare frames for the
// stable-frame (no-flicker) assertions.
func simFrame(sim tcell.SimulationScreen) string {
	cells, w, h := sim.GetContents()
	out := make([]rune, 0, w*h)
	for i := 0; i < w*h; i++ {
		if len(cells[i].Runes) == 0 {
			out = append(out, ' ')
			continue
		}
		out = append(out, cells[i].Runes[0])
	}
	return string(out)
}

// TestSingleConnectionInvariantLaunchAndSwitch is the CM4 #243
// regression test that locks the epic #239 invariant end to end: at
// most ONE pool entry runs a live-connection lifecycle at any time --
// at launch it's the restored selection, and a cluster switch MOVES
// that single connection rather than adding a second. It drives the
// flow against a real tcell SimulationScreen so the connect + switch
// draw paths are exercised (and asserted not to panic).
func TestSingleConnectionInvariantLaunchAndSwitch(t *testing.T) {
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

	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(100, 30)

	app := NewApp(AppConfig{Version: "test"})
	app.screen = ui.NewScreenFromTcell(sim)
	t.Cleanup(app.Quit)

	app.connect()

	// Launch invariant: exactly one live lifecycle, and it's alpha.
	if started := startedLifecycleNames(app); len(started) != 1 || started[0] != "alpha" {
		t.Fatalf("launch: want only alpha's lifecycle started, got %v", started)
	}
	app.refreshClusterList()
	app.draw() // must not panic
	if frame := simFrame(sim); len(frame) == 0 {
		t.Fatal("launch frame rendered empty")
	}

	// Switch to beta: the single live connection must MOVE.
	app.setSelected("beta")

	if started := startedLifecycleNames(app); len(started) != 1 || started[0] != "beta" {
		t.Fatalf("after switch: want only beta's lifecycle started, got %v", started)
	}
	if app.selectedName() != "beta" {
		t.Fatalf("selected = %q, want beta", app.selectedName())
	}
	app.refreshClusterList()
	app.draw() // must not panic after the switch
}

// TestStableFrameNoFlicker asserts the cluster list renders identically
// across two consecutive draws with no intervening state change -- the
// rendering path is pure, so a steady state produces a stable frame and
// not a flicker. Covers the new "available" (probe-up) row status too.
func TestStableFrameNoFlicker(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(80, 24)

	screen := ui.NewScreenFromTcell(sim)
	v := cluster.NewClustersView(ui.DefaultTheme())
	v.SetClusters([]cluster.ClusterStatus{
		{Config: config.ClusterConfig{Name: "local"}, Status: "connected"},
		{Config: config.ClusterConfig{Name: "staging"}, Status: "available"},
		{Config: config.ClusterConfig{Name: "prod"}, Status: "unreachable"},
	})
	v.OnEntryState = func(name string) (string, int, string, bool) {
		switch name {
		case "local":
			return "connected", 0, "", true
		case "staging":
			return "idle", 0, "", true
		case "prod":
			return "idle", 0, "", true
		}
		return "", 0, "", false
	}
	v.SelectedCluster = "local"

	bounds := ui.Rect{X: 0, Y: 0, Width: 80, Height: 24}

	sim.Clear()
	v.Draw(screen, bounds)
	sim.Show()
	frame1 := simFrame(sim)

	sim.Clear()
	v.Draw(screen, bounds)
	sim.Show()
	frame2 := simFrame(sim)

	if frame1 != frame2 {
		t.Fatal("two consecutive draws of an unchanged cluster view differ -- the render path is not stable (flicker)")
	}
}

// TestProbeSteadyStateNoRedraw asserts the liveness probe's change-gate:
// re-recording the same reachability result reports "no change", so the
// probe loop emits zero redraws in steady state (no flicker from the
// periodic sweep).
func TestProbeSteadyStateNoRedraw(t *testing.T) {
	e := &connEntry{}
	if !e.setProbe(probeUnreachable) {
		t.Fatal("first probe result should register as a change")
	}
	if e.setProbe(probeUnreachable) {
		t.Fatal("re-recording the same probe result must NOT report a change (would cause a steady-state redraw)")
	}
	if !e.setProbe(probeReachable) {
		t.Fatal("a real transition (down -> up) must report a change")
	}
}
