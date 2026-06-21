package cluster

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/config"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// runWithTimeout fails the test if fn doesn't return within d -- used to
// catch the re-entrant view-mutex deadlock (#201) instead of hanging the
// whole test binary.
func runWithTimeout(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s deadlocked (HandleEvent invoked a callback that re-enters the view lock) -- #201", what)
	}
}

// TestDeleteConfirm_NoReentrantDeadlock drives the delete confirmation
// through HandleEvent (which holds v.Mu) with an OnDelete that re-enters
// the v.Mu-locking setters -- exactly what App.deleteCluster does via
// SetConnected + refreshClusterList. Before the fix this self-deadlocks.
func TestDeleteConfirm_NoReentrantDeadlock(t *testing.T) {
	v := NewClustersView(ui.DefaultTheme())
	v.SetClusters([]ClusterStatus{
		{Config: config.ClusterConfig{Name: "local"}},
		{Config: config.ClusterConfig{Name: "staging"}},
	})
	v.Selected = 1
	v.confirmDelete = true
	v.OnDelete = func(name string) {
		// Re-enter the view the same way deleteCluster does.
		v.SetConnected(name, false, "", "")
		v.SetClusters([]ClusterStatus{{Config: config.ClusterConfig{Name: "local"}}})
	}

	runWithTimeout(t, 2*time.Second, "delete confirm", func() {
		v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))
	})
}

// TestAddForm_NoReentrantDeadlock drives the Add form Enter through
// HandleEvent with an OnAdd that re-enters the view (like OnAdd ->
// refreshClusterList -> SetClusters).
func TestAddForm_NoReentrantDeadlock(t *testing.T) {
	v := NewClustersView(ui.DefaultTheme())
	v.SetClusters([]ClusterStatus{{Config: config.ClusterConfig{Name: "local"}}})
	v.showAddForm = true
	v.addForm = addFormState{}
	v.OnAdd = func(config.ClusterConfig) {
		v.SetClusters([]ClusterStatus{{Config: config.ClusterConfig{Name: "local"}}})
	}

	runWithTimeout(t, 2*time.Second, "add form save", func() {
		for _, r := range "example.copresent.ai" {
			v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		}
		v.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	})
}
