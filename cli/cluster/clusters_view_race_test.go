package cluster

import (
	"fmt"
	"sync"
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/config"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// TestClustersView_NoRaceUnderConcurrentMutate exercises every
// background-driven mutator the pool lifecycle hits (SetClusters,
// SetConnected, SetRowStatus) against a UI-reader simulation. With
// the sync.RWMutex in place, this passes cleanly under -race.
// Without the lock, the per-row Status field would race the reader
// and the race detector would print DATA RACE diagnostics.
//
// Mirrors topology_race_test.go's shape so any future contributor
// who adds a view-level mutator has a template to copy.
func TestClustersView_NoRaceUnderConcurrentMutate(t *testing.T) {
	v := NewClustersView(ui.DefaultTheme())
	v.SetClusters([]ClusterStatus{
		{Config: config.ClusterConfig{Name: "local"}, Status: "connecting"},
		{Config: config.ClusterConfig{Name: "staging"}, Status: "unknown"},
	})

	const iters = 500
	var wg sync.WaitGroup

	// Writer 1: SetClusters churn -- replaces the whole slice.
	// Simulates a config-file reload via refreshClusterList.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			rest := []ClusterStatus{
				{Config: config.ClusterConfig{Name: "local"}, Status: "connected"},
			}
			if i%3 == 0 {
				rest = append(rest,
					ClusterStatus{Config: config.ClusterConfig{Name: "staging"}, Status: "connecting"},
				)
			}
			v.SetClusters(rest)
		}
	}()

	// Writer 2: SetConnected -- the pool lifecycle's "I just dialed
	// successfully" hook. Mutates ActiveCluster + per-row Status +
	// NodeId / NodeVer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			v.SetConnected("local", i%2 == 0, fmt.Sprintf("node-%d", i), "1.0")
		}
	}()

	// Writer 3: SetRowStatus -- the pool lifecycle's "transition to
	// stateBackoff / stateFailed" hook. Mutates per-row Status
	// without touching NodeId.
	wg.Add(1)
	go func() {
		defer wg.Done()
		states := []string{"connecting", "unreachable", "connected", "unknown"}
		for i := 0; i < iters; i++ {
			v.SetRowStatus("local", states[i%len(states)])
		}
	}()

	// Reader: walks the Clusters slice under the read lock --
	// equivalent to what Draw does internally.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters*2; i++ {
			v.mu.RLock()
			for _, c := range v.Clusters {
				_ = c.Config.Name
				_ = c.Status
				_ = c.NodeId
				_ = c.NodeVer
			}
			_ = v.SelectedCluster
			_ = v.ActiveCluster
			v.mu.RUnlock()
		}
	}()

	// FormOpen: takes its own lock; exercises the read-locked
	// accessor path that has both ClustersView's mu and the nested
	// PartitionsView's mu in the chain.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = v.FormOpen()
		}
	}()

	wg.Wait()
}
