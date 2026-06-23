package cli

import (
	"testing"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// TestNodesFromRows_MultipleTypes guards the regression where the topology
// only rendered the connected BFF node: parseClusterNodes received an
// opaque *client.Result but the parser only handled []any/map and returned
// nil, so the full fleet was dropped and the code fell back to a single
// synthesized BFF. nodesFromRows now consumes the unwrapped rows; this
// pins that a multi-type result yields every node type.
func TestNodesFromRows_MultipleTypes(t *testing.T) {
	rows := []client.Row{
		{"id": "bff-1", "nodeType": "bff", "health": "healthy", "createdAt": "2026-06-22T10:00:00Z"},
		{"id": "cognition-1", "nodeType": "cognition", "health": "healthy", "createdAt": "2026-06-22T10:00:01Z"},
		{"id": "agent-1", "nodeType": "agent", "health": "healthy", "createdAt": "2026-06-22T10:00:02Z"},
		// Nested-payload shape (raw []any result keeps payload nested).
		{"id": "planner-1", "payload": map[string]any{"nodeType": "planner", "health": "healthy"}, "createdAt": "2026-06-22T10:00:03Z"},
	}
	nodes := nodesFromRows(rows)
	if len(nodes) != 4 {
		t.Fatalf("got %d nodes, want 4: %+v", len(nodes), nodes)
	}
	gotTypes := map[string]bool{}
	for _, n := range nodes {
		gotTypes[n.Type] = true
	}
	for _, want := range []string{"bff", "cognition", "agent", "planner"} {
		if !gotTypes[want] {
			t.Errorf("missing node type %q in %v", want, gotTypes)
		}
	}
}

// TestNodesFromRows_DedupeByIdNewestWins pins the time-series collapse:
// a node that re-registers / changes health produces multiple rows for
// one id; only the newest createdAt should survive.
func TestNodesFromRows_DedupeByIdNewestWins(t *testing.T) {
	rows := []client.Row{
		{"id": "bff-1", "nodeType": "bff", "health": "unhealthy", "version": "v1", "createdAt": "2026-06-22T10:00:00Z"},
		{"id": "bff-1", "nodeType": "bff", "health": "healthy", "version": "v2", "createdAt": "2026-06-22T10:05:00Z"},
	}
	nodes := nodesFromRows(rows)
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1 (deduped): %+v", len(nodes), nodes)
	}
	if nodes[0].Version != "v2" {
		t.Errorf("kept version %q, want v2 (newest createdAt should win)", nodes[0].Version)
	}
}

// TestNodesFromRows_Empty returns nil for an empty/absent result so the
// caller's last-resort single-node fallback can engage.
func TestNodesFromRows_Empty(t *testing.T) {
	if got := nodesFromRows(nil); got != nil {
		t.Errorf("nodesFromRows(nil) = %v, want nil", got)
	}
	if got := nodesFromRows([]client.Row{}); got != nil {
		t.Errorf("nodesFromRows(empty) = %v, want nil", got)
	}
}
