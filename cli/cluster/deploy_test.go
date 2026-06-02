package cluster

// Tests for the deployment-v2 status strip (#144). The strip is read-
// only: SetDeploymentStatus + SetClusterRole feed it, Draw renders it.
// We render against a tcell SimulationScreen and assert (a) the panel
// shows version / sync for an owner/admin caller, and (b) a non-admin
// caller gets the muted "owner/admin only" line instead of the panel.

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// renderTopology draws the View against a fresh SimulationScreen of the
// given size and returns the flattened rows. Shared shape with
// chrome_contract_test.go's flattenSim.
func renderTopology(t *testing.T, v *View, w, h int) []string {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(w, h)
	sim.Clear()

	screen := ui.NewScreenFromTcell(sim)
	v.Draw(screen, ui.Rect{X: 0, Y: 0, Width: w, Height: h})
	sim.Show()
	return flattenSim(sim)
}

func sampleStatus(env string) client.DeploymentStatus {
	return client.DeploymentStatus{
		Env:           env,
		Version:       "v1.2.3",
		EngineVersion: "e9.9",
		Gate:          "ok",
		Argocd: client.ArgoStatus{
			SyncStatus:   "Synced",
			HealthStatus: "Healthy",
		},
		Rollouts: []client.RolloutStatus{
			{Name: "bff", Kind: "canary", CanaryWeight: 25, CurrentStep: 2, LatestAnalysisResult: "Successful"},
		},
		GateResult: client.GateResult{
			Result: "pass",
			Legs:   []client.GateLeg{{Name: "smoke", Passed: true}},
		},
	}
}

// TestDeployStrip_AdminRendersPanel asserts an admin caller with status
// data sees the deployment panel (header + version + sync) without
// panicking.
func TestDeployStrip_AdminRendersPanel(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(client.RoleAdmin)
	v.SetDeploymentStatus("staging", sampleStatus("staging"))
	v.SetDeploymentStatus("prod", sampleStatus("prod"))

	rows := renderTopology(t, v, 100, 30)
	joined := strings.Join(rows, "\n")

	if !strings.Contains(joined, "DEPLOYMENTS") {
		t.Errorf("expected DEPLOYMENTS header in panel; got:\n%s", joined)
	}
	if !strings.Contains(joined, "v1.2.3") {
		t.Errorf("expected deployed version v1.2.3 in panel; got:\n%s", joined)
	}
	if !strings.Contains(joined, "Synced") {
		t.Errorf("expected Argo sync status 'Synced' in panel; got:\n%s", joined)
	}
	if !strings.Contains(joined, "STAGING") || !strings.Contains(joined, "PROD") {
		t.Errorf("expected both STAGING and PROD env rows; got:\n%s", joined)
	}
	if strings.Contains(joined, "owner/admin only") {
		t.Errorf("admin caller should see the panel, not the muted gate line; got:\n%s", joined)
	}
}

// TestDeployStrip_OwnerRendersPanel mirrors the admin case for the owner
// role -- both are allowed.
func TestDeployStrip_OwnerRendersPanel(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(client.RoleOwner)
	v.SetDeploymentStatus("staging", sampleStatus("staging"))

	rows := renderTopology(t, v, 100, 30)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "DEPLOYMENTS") || !strings.Contains(joined, "v1.2.3") {
		t.Errorf("owner caller should see the deployment panel; got:\n%s", joined)
	}
}

// TestDeployStrip_NonAdminMutedLine asserts a reader/writer caller sees
// the single muted "owner/admin only" line and never the panel body,
// even if status data happens to be present.
func TestDeployStrip_NonAdminMutedLine(t *testing.T) {
	for _, role := range []client.Role{client.RoleReader, client.RoleWriter, ""} {
		v := NewView(ui.DefaultTheme())
		v.SetClusterRole(role)
		// Even with data set, a non-admin must not see it.
		v.SetDeploymentStatus("staging", sampleStatus("staging"))

		rows := renderTopology(t, v, 100, 30)
		joined := strings.Join(rows, "\n")

		if !strings.Contains(joined, "Deployments: owner/admin only") {
			t.Errorf("role %q: expected muted 'owner/admin only' line; got:\n%s", role, joined)
		}
		if strings.Contains(joined, "v1.2.3") {
			t.Errorf("role %q: non-admin caller must NOT see deployment data; got:\n%s", role, joined)
		}
	}
}

// TestDeployStrip_AdminNoDataYet asserts an admin with no fetched status
// yet renders the panel header + per-env "no data yet" without panic.
func TestDeployStrip_AdminNoDataYet(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(client.RoleAdmin)

	rows := renderTopology(t, v, 100, 30)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "DEPLOYMENTS") {
		t.Errorf("expected DEPLOYMENTS header even with no data; got:\n%s", joined)
	}
	if !strings.Contains(joined, "no data yet") {
		t.Errorf("expected per-env 'no data yet' placeholder; got:\n%s", joined)
	}
}
