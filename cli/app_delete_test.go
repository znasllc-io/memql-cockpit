package cli

import (
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/cli/config"
)

// blockingStore is a CredentialStore whose Delete blocks until the test
// releases it. It stands in for the OS keyring, whose Delete can block
// indefinitely on macOS (an invisible Keychain permission prompt behind
// the full-screen TUI) -- the exact failure that froze the cockpit in
// memql-cockpit#191.
type blockingStore struct {
	release   chan struct{} // closed by the test to unblock Delete
	mu        sync.Mutex
	deleted   []string
	deletedCh chan string
}

func newBlockingStore() *blockingStore {
	return &blockingStore{
		release:   make(chan struct{}),
		deletedCh: make(chan string, 4),
	}
}

func (s *blockingStore) Get(string) (*config.StoredToken, error) { return nil, nil }
func (s *blockingStore) Put(string, *config.StoredToken) error   { return nil }
func (s *blockingStore) Name() string                            { return "blocking-test" }

func (s *blockingStore) Delete(cluster string) error {
	<-s.release // block until the test allows teardown to proceed
	s.mu.Lock()
	s.deleted = append(s.deleted, cluster)
	s.mu.Unlock()
	s.deletedCh <- cluster
	return nil
}

// TestDeleteClusterDoesNotBlockOnCredentialStore is the regression test
// for memql-cockpit#191: deleting a cluster runs on the tcell event-loop
// goroutine, so it must NOT block on the (potentially indefinitely
// blocking) keyring token delete. The fast, local work -- removing the
// cluster from the registry + the UI -- must complete on this frame; the
// blocking teardown is forked to a background goroutine.
func TestDeleteClusterDoesNotBlockOnCredentialStore(t *testing.T) {
	// Isolate ~/.memql to a temp dir (config.ConfigDir uses $HOME).
	t.Setenv("HOME", t.TempDir())

	// Inject the blocking store the forked teardown will call into.
	store := newBlockingStore()
	config.SetActiveStore(store)
	t.Cleanup(func() { config.SetActiveStore(nil) })

	// Seed two user clusters; "beta" is the one we delete.
	if err := config.SaveClusters(&config.ClustersFile{
		Clusters: []config.ClusterConfig{
			{Name: "alpha", Endpoint: "alpha.example:50051"},
			{Name: "beta", Endpoint: "beta.example:50051"},
		},
	}); err != nil {
		t.Fatalf("seed clusters: %v", err)
	}

	app := NewApp(AppConfig{Version: "test"})

	// Pretend the user was viewing "beta" so the delete also exercises
	// the viewed-pointer rehoming. (We deliberately leave a.selected
	// unset so deleteCluster doesn't try to openEntry/dial "alpha".)
	app.viewed = "beta"
	for i, c := range app.clustersView.Clusters {
		if c.Config.Name == "beta" {
			app.clustersView.Selected = i
		}
	}

	// The delete must return promptly even though the store's Delete is
	// currently blocked. If teardown ran inline this would hang.
	done := make(chan struct{})
	start := time.Now()
	go func() {
		app.deleteCluster("beta")
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("deleteCluster took %v; it must not block on the credential store", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deleteCluster blocked on the credential store delete (regression of #191)")
	}

	// The row must be gone from the registry immediately, before teardown
	// is allowed to finish.
	clusters, err := config.LoadClusters()
	if err != nil {
		t.Fatalf("reload clusters: %v", err)
	}
	if _, ok := clusters.Get("beta"); ok {
		t.Fatal("beta still present in clusters.yaml after delete")
	}
	if _, ok := clusters.Get("alpha"); !ok {
		t.Fatal("alpha was incorrectly removed")
	}
	if app.viewed != "alpha" {
		t.Fatalf("viewed pointer not rehomed: got %q, want \"alpha\"", app.viewed)
	}

	// Now allow the forked teardown to proceed and confirm the token
	// delete still happens (best-effort, just off the event loop).
	close(store.release)
	select {
	case got := <-store.deletedCh:
		if got != "beta" {
			t.Fatalf("token delete fired for %q, want \"beta\"", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forked teardown never deleted the token")
	}
}
