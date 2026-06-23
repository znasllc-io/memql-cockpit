package cluster

// Tests for the deployment topology preview (memql-cockpit#225): the pure
// computeEdges edge model, the parameterized renderer's preview path, and
// the live -> preview -> fullscreen drill-down state machine driven through
// HandleEvent.

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

func keyUp() *tcell.EventKey { return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone) }
func keyF() *tcell.EventKey  { return keyRune('f') }

// recvWithin blocks for a node-load request id (OnSelectDeployment fires
// off-thread via `go cb(id)`), failing if none arrives in time.
func recvWithin(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(time.Second):
		t.Fatal("expected a deployment node-load request, got none")
		return ""
	}
}

// hasEdge reports whether the (a,b) pair is present (order-sensitive, the
// way computeEdges emits parent->child).
func hasEdge(edges [][2]int, a, b int) bool {
	for _, e := range edges {
		if e[0] == a && e[1] == b {
			return true
		}
	}
	return false
}

func TestComputeEdges_StaticRelationsDropMissingEndpoints(t *testing.T) {
	// lb, bff, voice present; identity / livekit / redis / database absent.
	nodes := []NodeInfo{
		{ID: "lb-1", Type: "lb"},
		{ID: "bff-1", Type: "bff"},
		{ID: "voice-1", Type: "voice"},
	}
	edges := computeEdges(nodes)

	if !hasEdge(edges, 0, 1) {
		t.Errorf("expected lb(0)->bff(1) edge; got %v", edges)
	}
	if !hasEdge(edges, 1, 2) {
		t.Errorf("expected bff(1)->voice(2) edge; got %v", edges)
	}
	// lb->identity, bff->database, voice->livekit/redis all dropped (one
	// endpoint missing), so only the two satisfiable static edges remain.
	if len(edges) != 2 {
		t.Errorf("expected exactly 2 edges, got %d: %v", len(edges), edges)
	}
}

func TestComputeEdges_DiscoveryEdgesFromParentId(t *testing.T) {
	// Two same-type nodes: the static map has no agent->agent relation, so
	// the only edge must come from the ParentId discovery link.
	nodes := []NodeInfo{
		{ID: "agent-A", Type: "agent"},
		{ID: "agent-B", Type: "agent", ParentId: "agent-A"},
	}
	edges := computeEdges(nodes)
	if !hasEdge(edges, 0, 1) {
		t.Errorf("expected discovery edge agent-A(0)->agent-B(1); got %v", edges)
	}
	if len(edges) != 1 {
		t.Errorf("expected exactly 1 discovery edge, got %d: %v", len(edges), edges)
	}
}

func TestComputeEdges_DanglingAndSelfParentSkipped(t *testing.T) {
	nodes := []NodeInfo{
		{ID: "bff-1", Type: "bff", ParentId: "bff-1"},     // self-parent
		{ID: "voice-1", Type: "voice", ParentId: "ghost"}, // dangling parent
	}
	edges := computeEdges(nodes)
	// Only the static bff->voice edge should appear; neither parentId
	// produces a (valid, non-self) discovery edge.
	if !hasEdge(edges, 0, 1) {
		t.Errorf("expected static bff(0)->voice(1) edge; got %v", edges)
	}
	if len(edges) != 1 {
		t.Errorf("expected exactly 1 edge, got %d: %v", len(edges), edges)
	}
}

func TestComputeEdges_Dedupe(t *testing.T) {
	// Static bff->voice AND a parentId voice<-bff would both yield (0,1);
	// the result must not double-count.
	nodes := []NodeInfo{
		{ID: "bff-1", Type: "bff"},
		{ID: "voice-1", Type: "voice", ParentId: "bff-1"},
	}
	edges := computeEdges(nodes)
	if len(edges) != 1 || !hasEdge(edges, 0, 1) {
		t.Errorf("expected a single deduped (0,1) edge, got %v", edges)
	}
}

func TestComputeEdges_BuildEdgesLockedMatches(t *testing.T) {
	// The view wrapper must produce exactly what the pure helper does.
	nodes := []NodeInfo{
		{ID: "lb-1", Type: "lb"},
		{ID: "bff-1", Type: "bff", ParentId: "lb-1"},
	}
	v := NewView(ui.DefaultTheme())
	v.SetNodes(nodes)
	want := computeEdges(nodes)
	if len(v.Edges) != len(want) {
		t.Fatalf("buildEdgesLocked produced %v, computeEdges %v", v.Edges, want)
	}
	for i := range want {
		if v.Edges[i] != want[i] {
			t.Fatalf("edge %d mismatch: view %v vs pure %v", i, v.Edges[i], want[i])
		}
	}
}

// --- preview state machine ---

func newPreviewView() (*View, chan string) {
	v := NewView(ui.DefaultTheme())
	sel := make(chan string, 8)
	v.OnSelectDeployment = func(id string) { sel <- id }
	v.SetDeployments([]DeploymentInfo{
		{ID: "dep-newest", Version: "2026.6.2", Status: "succeeded"},
		{ID: "dep-older", Version: "2026.6.1", Status: "superseded"},
	})
	return v, sel
}

func TestPreview_EnterEscalatesLiveToPreviewToFullscreen(t *testing.T) {
	v, sel := newPreviewView()

	if v.previewActive || v.previewFullscreen {
		t.Fatal("preview should start inactive")
	}

	// Enter #1: live -> preview (+ requests node load).
	v.HandleEvent(keyEnter())
	if !v.previewActive || v.previewFullscreen {
		t.Fatalf("after first Enter: want preview-only; got active=%v full=%v", v.previewActive, v.previewFullscreen)
	}
	if id := recvWithin(t, sel); id != "dep-newest" {
		t.Errorf("OnSelectDeployment got %q, want dep-newest", id)
	}

	// Enter #2: preview -> fullscreen.
	v.HandleEvent(keyEnter())
	if !v.previewActive || !v.previewFullscreen {
		t.Fatalf("after second Enter: want fullscreen; got active=%v full=%v", v.previewActive, v.previewFullscreen)
	}
}

func TestPreview_EscCollapsesFullscreenThenPreview(t *testing.T) {
	v, _ := newPreviewView()
	v.HandleEvent(keyEnter()) // preview
	v.HandleEvent(keyEnter()) // fullscreen

	// Esc #1: fullscreen -> preview.
	if !v.HandleEvent(keyEsc()) {
		t.Error("Esc should be consumed while fullscreen")
	}
	if !v.previewActive || v.previewFullscreen {
		t.Fatalf("after first Esc: want preview-only; got active=%v full=%v", v.previewActive, v.previewFullscreen)
	}

	// Esc #2: preview -> live.
	if !v.HandleEvent(keyEsc()) {
		t.Error("Esc should be consumed while previewing")
	}
	if v.previewActive || v.previewFullscreen {
		t.Fatalf("after second Esc: want live; got active=%v full=%v", v.previewActive, v.previewFullscreen)
	}

	// Esc #3: nothing to collapse -> not consumed (free for higher handlers).
	if v.HandleEvent(keyEsc()) {
		t.Error("Esc with no preview open should not be consumed")
	}
}

func TestPreview_FTogglesFullscreen(t *testing.T) {
	v, _ := newPreviewView()
	// F from live jumps straight into fullscreen preview.
	v.HandleEvent(keyF())
	if !v.previewActive || !v.previewFullscreen {
		t.Fatalf("F from live: want fullscreen preview; got active=%v full=%v", v.previewActive, v.previewFullscreen)
	}
	// F again collapses the fullscreen but keeps preview active.
	v.HandleEvent(keyF())
	if !v.previewActive || v.previewFullscreen {
		t.Fatalf("F again: want preview-only; got active=%v full=%v", v.previewActive, v.previewFullscreen)
	}
}

func TestPreview_ArrowReloadsWhilePreviewing(t *testing.T) {
	v, sel := newPreviewView()
	v.HandleEvent(keyEnter()) // preview newest, drains one load request
	recvWithin(t, sel)

	// Down to the older deployment: cache cleared + reload requested for
	// the new selection so the preview tracks the cursor.
	v.HandleEvent(keyDown())
	if id := recvWithin(t, sel); id != "dep-older" {
		t.Errorf("arrow reload requested %q, want dep-older", id)
	}
	if v.deployLoadedFor != "" {
		t.Error("moving the cursor should clear the stale node cache")
	}

	// Back up; preview stays active throughout.
	v.HandleEvent(keyUp())
	if !v.previewActive {
		t.Error("preview should remain active across arrow navigation")
	}
}

func TestPreview_RendersDeploymentCompositionGraphically(t *testing.T) {
	v, _ := newPreviewView()
	// Load a composition for the selected deployment: one in-deployment
	// bff node + one orphan voice node.
	v.SetDeploymentNodes("dep-newest",
		[]NodeInfo{{ID: "bff-1", Type: "bff", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY, Version: "2026.6.2"}},
		[]NodeInfo{{ID: "voice-9", Type: "voice", DeploymentId: "dep-older", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY}},
	)
	v.SetDeploymentNodeSpecs("dep-newest", []NodeSpecInfo{{NodeType: "bff", Version: "2026.6.2", Replicas: 2}})
	v.HandleEvent(keyEnter()) // enter preview

	rows := renderTopology(t, v, 80, 24)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "preview: deployment dep-newest") {
		t.Errorf("expected preview header in render; got:\n%s", joined)
	}
	// The spec summary (desired per-tier version/replicas) should surface.
	if !strings.Contains(joined, "x2") {
		t.Errorf("expected deploymentNodeSpec replicas (x2) in render; got:\n%s", joined)
	}
}
