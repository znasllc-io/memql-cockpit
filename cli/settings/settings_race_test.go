package settings

import (
	"sync"
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestSettingsView_NoRaceUnderConcurrentMutate is the race test for
// Settings.View. SetMyAccess / ClearMyAccess are called from the
// refreshMyAccess goroutine (cli/app.go's per-cluster My Access
// fetcher); Draw concurrently reads access + accessCluster. Without
// the sync.RWMutex on View, this would surface as DATA RACE under
// the -race detector + corrupted MY ACCESS pane rendering in
// practice.
func TestSettingsView_NoRaceUnderConcurrentMutate(t *testing.T) {
	v := NewView(ui.DefaultTheme(), "0.1.0")

	const iters = 1000
	var wg sync.WaitGroup

	// Writer: SetMyAccess churn -- the typical refreshMyAccess flow
	// is a query result lands -> SetMyAccess. Multiple clusters
	// switching in quick succession would fire this repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			v.SetMyAccess("local", &memqlv1.MyAccessResult{
				// Bare struct -- the test cares about the assignment
				// being atomic, not the contents.
			})
		}
	}()

	// Writer 2: ClearMyAccess -- fires when the active cluster goes
	// away (disconnect / delete). Most aggressive mutation since it
	// nils out access entirely.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			v.ClearMyAccess()
		}
	}()

	// Reader: walks the access fields under the read lock --
	// equivalent to what Draw does internally via drawMyAccess.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters*2; i++ {
			v.mu.RLock()
			_ = v.access
			_ = v.accessCluster
			v.mu.RUnlock()
		}
	}()

	wg.Wait()
}
