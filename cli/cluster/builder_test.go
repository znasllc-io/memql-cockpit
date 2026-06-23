package cluster

// Tests for the interactive topology-as-canvas phase 1 (memql-cockpit#237):
// the pure composeBuilder model, the builder mode wired through the view's
// HandleEvent/Draw, and the parameterized renderer drawing an explicit node
// set. Also confirms the Epic 2 deploymentNodeSpec data is consumed (the
// builder seeds from it).

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

func keyN() *tcell.EventKey { return keyRune('N') }

// --- pure model ---

func TestComposeBuilder_SeedFromSpecs(t *testing.T) {
	b := newComposeBuilderFromSpecs([]NodeSpecInfo{
		{NodeType: "bff", Version: "2026.6.2", Replicas: 2},
		{NodeType: "voice", Replicas: 0}, // 0 -> defaults to 1
		{NodeType: "", Replicas: 5},      // skipped
	})
	if len(b.rows) != 2 {
		t.Fatalf("expected 2 rows (empty type skipped), got %d", len(b.rows))
	}
	if b.rows[0] != (composeRow{NodeType: "bff", Replicas: 2, Version: "2026.6.2"}) {
		t.Errorf("row0 = %+v", b.rows[0])
	}
	if b.rows[1].Replicas != 1 {
		t.Errorf("zero-replica spec should default to 1, got %d", b.rows[1].Replicas)
	}
}

func TestComposeBuilder_SeedFromTypes(t *testing.T) {
	b := &composeBuilder{}
	b.seedFromTypes([]string{"bff", "voice", ""}, map[string]int{"bff": 3})
	if len(b.rows) != 2 {
		t.Fatalf("expected 2 rows (empty skipped), got %d", len(b.rows))
	}
	if b.rows[0].Replicas != 3 {
		t.Errorf("bff should take live count 3, got %d", b.rows[0].Replicas)
	}
	if b.rows[1].Replicas != 1 {
		t.Errorf("voice with no live count should default to 1, got %d", b.rows[1].Replicas)
	}
}

func TestComposeBuilder_NavigateAndAdjust(t *testing.T) {
	b := newComposeBuilderFromSpecs([]NodeSpecInfo{
		{NodeType: "bff", Replicas: 2},
		{NodeType: "voice", Replicas: 1},
	})
	b.moveUp() // already at 0, clamp
	if b.cursor != 0 {
		t.Fatalf("cursor should clamp at 0, got %d", b.cursor)
	}
	b.moveDown()
	if b.cursor != 1 {
		t.Fatalf("cursor should be 1, got %d", b.cursor)
	}
	b.moveDown() // clamp at last
	if b.cursor != 1 {
		t.Fatalf("cursor should clamp at last (1), got %d", b.cursor)
	}
	b.adjustReplicas(5)
	if b.rows[1].Replicas != 6 {
		t.Errorf("voice replicas should be 6, got %d", b.rows[1].Replicas)
	}
	b.adjustReplicas(-100) // clamp at min
	if b.rows[1].Replicas != composeMinReplicas {
		t.Errorf("replicas should clamp at min %d, got %d", composeMinReplicas, b.rows[1].Replicas)
	}
	b.adjustReplicas(1000) // clamp at max
	if b.rows[1].Replicas != composeMaxReplicas {
		t.Errorf("replicas should clamp at max %d, got %d", composeMaxReplicas, b.rows[1].Replicas)
	}
}

func TestComposeBuilder_AddRemoveAndExport(t *testing.T) {
	b := &composeBuilder{}
	if !b.addType("bff") {
		t.Fatal("addType bff should succeed")
	}
	if b.addType("bff") {
		t.Error("addType bff again should be a no-op (dedupe)")
	}
	b.addType("voice")
	if b.cursor != 1 {
		t.Errorf("cursor should follow the just-added row (1), got %d", b.cursor)
	}
	// nextAddableType skips present types.
	if got := b.nextAddableType([]string{"bff", "voice", "agent"}); got != "agent" {
		t.Errorf("nextAddableType = %q, want agent", got)
	}
	if got := b.nextAddableType([]string{"bff", "voice"}); got != "" {
		t.Errorf("nextAddableType with all present = %q, want empty", got)
	}

	specs := b.toSpecs()
	if len(specs) != 2 || specs[0].NodeType != "bff" || specs[1].NodeType != "voice" {
		t.Fatalf("toSpecs = %+v", specs)
	}

	b.removeSelected() // removes voice (cursor 1)
	if len(b.rows) != 1 || b.rows[0].NodeType != "bff" {
		t.Fatalf("after remove, rows = %+v", b.rows)
	}
	if b.cursor != 0 {
		t.Errorf("cursor should clamp to 0 after remove, got %d", b.cursor)
	}
}

// --- view integration ---

func TestBuilder_ToggleOpensAndCloses(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetNodeTypes([]NodeTypeInfo{{Name: "bff"}, {Name: "voice"}})
	v.SetNodes([]NodeInfo{
		{ID: "bff-1", Type: "bff", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY},
		{ID: "bff-2", Type: "bff", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY},
	})

	v.HandleEvent(keyN()) // open
	if v.builder == nil {
		t.Fatal("N should open the topology builder")
	}
	// Seeded from known types; bff has 2 live nodes -> replicas 2.
	if len(v.builder.rows) != 2 {
		t.Fatalf("builder should seed 2 types, got %d", len(v.builder.rows))
	}
	if v.builder.rows[0].NodeType != "bff" || v.builder.rows[0].Replicas != 2 {
		t.Errorf("bff row should seed replicas=2 from live count, got %+v", v.builder.rows[0])
	}

	joined := strings.Join(renderTopology(t, v, 100, 30), "\n")
	if !strings.Contains(joined, "TOPOLOGY BUILDER") {
		t.Errorf("builder render should show the header; got:\n%s", joined)
	}

	// Edit: +replicas on the selected (bff) row.
	v.HandleEvent(keyRune('+'))
	if v.builder.rows[0].Replicas != 3 {
		t.Errorf("+ should bump bff replicas to 3, got %d", v.builder.rows[0].Replicas)
	}

	v.HandleEvent(keyEsc()) // close
	if v.builder != nil {
		t.Error("Esc should close the builder")
	}

	v.HandleEvent(keyN()) // reopen
	v.HandleEvent(keyN()) // N again closes
	if v.builder != nil {
		t.Error("N should toggle the builder closed")
	}
}

func TestBuilder_SeedsFromDeploymentNodeSpec(t *testing.T) {
	// Confirms the Epic 2 deploymentNodeSpec data is consumed: when the
	// selected deployment's spec set is loaded, the builder seeds from it
	// (version + replicas), not from live node counts.
	v := NewView(ui.DefaultTheme())
	v.SetDeployments([]DeploymentInfo{{ID: "dep-1", Version: "2026.6.2", Status: "succeeded"}})
	v.SetDeploymentNodes("dep-1", []NodeInfo{{ID: "bff-1", Type: "bff"}}, nil)
	v.SetDeploymentNodeSpecs("dep-1", []NodeSpecInfo{
		{NodeType: "bff", Version: "2026.6.2", Replicas: 4},
		{NodeType: "cognition", Version: "2026.6.2", Replicas: 2},
	})

	v.HandleEvent(keyN())
	if v.builder == nil || len(v.builder.rows) != 2 {
		t.Fatalf("builder should seed 2 rows from the deploymentNodeSpec set; got %+v", v.builder)
	}
	if v.builder.rows[0].NodeType != "bff" || v.builder.rows[0].Replicas != 4 || v.builder.rows[0].Version != "2026.6.2" {
		t.Errorf("builder row0 should mirror the spec (bff x4 @2026.6.2); got %+v", v.builder.rows[0])
	}
}

// --- parameterized renderer ---

func TestDrawTopology_RendersExplicitNodeSet(t *testing.T) {
	// The parameterized renderer draws WHATEVER node set it is handed, not
	// v.Nodes. Hand it an explicit set and assert its type labels render,
	// with v.Nodes left empty.
	v := NewView(ui.DefaultTheme())
	nodes := []NodeInfo{
		{ID: "bff-1", Type: "bff", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY},
		{ID: "voice-1", Type: "voice", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY},
	}

	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(100, 30)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)

	v.mu.RLock()
	v.drawTopology(screen, ui.Rect{X: 0, Y: 0, Width: 100, Height: 30}, nodes, computeEdges(nodes), nil)
	v.mu.RUnlock()
	sim.Show()

	joined := strings.Join(flattenSim(sim), "\n")
	if !strings.Contains(joined, "BFF") {
		t.Errorf("explicit node set should render BFF box; got:\n%s", joined)
	}
	if !strings.Contains(joined, "VOICE") {
		t.Errorf("explicit node set should render VOICE box; got:\n%s", joined)
	}
}
