package cluster

// Render test for the persistent vertical split (memql-cockpit#221): the
// right pane shows the live TOPOLOGY grid on top and the per-cluster
// DEPLOYMENTS section anchored to the bottom, both always visible -- no
// toggle. The deleted deployment-v2 "Surface A" (the always-on
// STAGING/PROD "no data yet" strip + D:Deploy modal) must NOT appear.
//
// We drive a tcell SimulationScreen the same way chrome_contract_test.go
// does, scrape the cells, and assert both regions render and Surface A is
// gone.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/sdk/go/client"
)

func TestDeploymentsSplit_BothRegionsRenderAndSurfaceAGone(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.ClusterName = "local"
	v.SetClusterRole(client.RoleOwner)

	// A couple of nodes for the topology region (top).
	v.SetNodeTypes([]NodeTypeInfo{{Name: "bff"}, {Name: "cognition"}})
	v.SetNodes([]NodeInfo{
		{ID: "bff-local", Type: "bff", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY, Version: "2026.6.21"},
		{ID: "cog-local", Type: "cognition", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY, Version: "2026.6.21"},
	})

	// A couple of deployment rows for the deployments region (bottom).
	v.SetDeployments([]DeploymentInfo{
		{ID: "dep-aaaaaaaaaaaa", Version: "2026.6.21", Environment: "staging", Provider: "azure", Status: "succeeded"},
		{ID: "dep-bbbbbbbbbbbb", Version: "2026.6.20", Environment: "staging", Provider: "azure", Status: "superseded"},
	})

	rows := renderTopology(t, v, 110, 36)
	joined := strings.Join(rows, "\n")

	// (a) The screen contains the DEPLOYMENTS section header and a node
	// type label from the topology grid.
	if !strings.Contains(joined, "DEPLOYMENT") {
		t.Errorf("expected a DEPLOYMENT header in the split render; got:\n%s", joined)
	}
	if !strings.Contains(joined, "BFF") {
		t.Errorf("expected the BFF node type label from the topology grid; got:\n%s", joined)
	}

	// (b) The deleted Surface A strings must NOT appear anywhere.
	for _, banned := range []string{"STAGING", "PROD", "no data yet"} {
		if strings.Contains(joined, banned) {
			t.Errorf("Surface A string %q must not appear in the split render; got:\n%s", banned, joined)
		}
	}

	// (c) Both a node box and a deployment row are present, with the
	// topology region ABOVE the deployments region. Locate the row index
	// of a node box (its '┌' top-left border) and of the "succeeded"
	// deployment-status token, and assert the node box renders higher.
	nodeBoxRow, depRow := -1, -1
	for i, row := range rows {
		if nodeBoxRow < 0 && strings.Contains(row, "┌") {
			nodeBoxRow = i
		}
		if depRow < 0 && strings.Contains(row, "succeeded") {
			depRow = i
		}
	}
	if nodeBoxRow < 0 {
		t.Errorf("expected a node box (┌ border) in the topology region; got:\n%s", joined)
	}
	if depRow < 0 {
		t.Errorf("expected a 'succeeded' deployment row in the deployments region; got:\n%s", joined)
	}
	if nodeBoxRow >= 0 && depRow >= 0 && nodeBoxRow >= depRow {
		t.Errorf("topology region (node box row %d) should render ABOVE the deployments region (deployment row %d)", nodeBoxRow, depRow)
	}
}
