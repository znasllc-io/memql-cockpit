package cluster

import (
	"sync"
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// TestTopology_NoRaceUnderConcurrentMutate exercises the race the
// screenshot exposed: background subscribers churning state via
// ApplyNodeUpdate / SetNodes / SetDisconnected while the UI thread
// reads state concurrently. With go test -race, an unlocked View
// would print "DATA RACE" diagnostics. With the sync.RWMutex in
// place this passes cleanly.
//
// We don't call Draw because Draw needs a live *ui.Screen (and ui.Screen
// is a concrete tcell wrapper that requires a TTY). Instead we
// stand in for the UI-thread reader with a tight loop over the
// public read-side surface (Nodes / NodeTypes / Edges field access
// via a snapshot helper). The lock contract is identical -- Draw's
// RLock is the same RLock the snapshot helper takes.
func TestTopology_NoRaceUnderConcurrentMutate(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetNodeTypes([]NodeTypeInfo{
		{Name: "bff"}, {Name: "cognition"}, {Name: "agent"},
	})
	v.SetNodes([]NodeInfo{
		{ID: "bff-local", Type: "bff", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY},
		{ID: "cog-local", Type: "cognition", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY},
	})

	const writers = 4
	const readers = 4
	const opsPerWriter = 500
	const opsPerReader = 1000

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < opsPerWriter; i++ {
				switch i % 4 {
				case 0:
					v.SetNodes([]NodeInfo{
						{ID: "bff-local", Type: "bff", Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY},
					})
				case 1:
					v.ApplyNodeUpdate(NodeInfo{
						ID:     "voice-local",
						Type:   "voice",
						Health: nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
					})
				case 2:
					v.SetDisconnected(wid%2 == 0)
				case 3:
					v.SetNodeTypes([]NodeTypeInfo{
						{Name: "bff"}, {Name: "voice"}, {Name: "cognition"},
					})
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerReader; i++ {
				snapshotForRace(v)
			}
		}()
	}

	wg.Wait()
}

// snapshotForRace performs the same read-side work Draw would: walk
// every field that holds racy state under the read lock. The race
// detector flags any concurrent unlocked write here, same as it
// would if we'd called Draw. Lives next to the test (vs. shipping
// as a real production method) because the production API doesn't
// need a snapshot -- Draw IS the consumer -- but the test needs
// one to exercise the lock without a screen.
func snapshotForRace(v *View) (nodes int, types int, edges int, disc bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	nodes = len(v.Nodes)
	types = len(v.NodeTypes)
	edges = len(v.Edges)
	disc = v.disconnected
	// Touch a few node fields to make sure the per-element reads
	// also fall under the lock.
	for _, n := range v.Nodes {
		_ = n.ID
		_ = n.Type
		_ = n.Health
	}
	for _, e := range v.Edges {
		_ = e
	}
	for _, nt := range v.NodeTypes {
		_ = nt.Name
	}
	return
}
