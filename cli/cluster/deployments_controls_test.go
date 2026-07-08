package cluster

// #292: the Deployments C/G/B controls must fire through the injected
// DeployRunner (the blessed deployEngineCluster automation path), NOT the
// retired DeployControlService gRPC that a bff-role node never serves.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

func TestDeployControls_RouteToDeployRunner(t *testing.T) {
	cases := []struct {
		name   string
		fire   func(v *View)
		action string
	}{
		{"deploy", func(v *View) { v.fireDeployLocked() }, "deploy"},
		{"cut", func(v *View) { v.fireCutLocked() }, "cut"},
		{"rollback", func(v *View) { v.fireRollbackConceptLocked() }, "rollback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := NewView(ui.DefaultTheme())
			v.SetClusterEnvironment("staging") // a cluster IS one env

			var mu sync.Mutex
			var gotEnv, gotAction, gotTarget string
			called := make(chan struct{}, 1)
			v.DeployRunner = func(env, action, targetID string) (string, bool) {
				mu.Lock()
				gotEnv, gotAction, gotTarget = env, action, targetID
				mu.Unlock()
				called <- struct{}{}
				return "SUCCESS: routed " + action, true
			}

			v.mu.Lock()
			v.dctrl = &deployConceptState{targetID: "dep-xyz"}
			tc.fire(v)
			v.mu.Unlock()

			select {
			case <-called:
			case <-time.After(2 * time.Second):
				t.Fatal("DeployRunner was not invoked (control still on the dead gRPC path?)")
			}

			mu.Lock()
			ge, ga, gt := gotEnv, gotAction, gotTarget
			mu.Unlock()
			if ga != tc.action {
				t.Errorf("routed action = %q, want %q", ga, tc.action)
			}
			if ge != "staging" {
				t.Errorf("routed env = %q, want staging (cluster env)", ge)
			}
			if gt != "dep-xyz" {
				t.Errorf("routed targetID = %q, want dep-xyz", gt)
			}

			// The runner's result line lands on the modal (the setter runs
			// just after the runner returns; poll under a deadline).
			deadline := time.Now().Add(2 * time.Second)
			for {
				v.mu.RLock()
				res, ok := v.dctrl.result, v.dctrl.resultOK
				v.mu.RUnlock()
				if res == "SUCCESS: routed "+tc.action && ok {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("runner result not stored on modal; got %q ok=%v", res, ok)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func TestDeployControls_NilRunnerSurfacesError(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.DeployRunner = nil

	v.mu.Lock()
	v.dctrl = &deployConceptState{targetID: "x"}
	v.fireDeployLocked() // synchronous early-return when the runner is nil
	res, ok := v.dctrl.result, v.dctrl.resultOK
	v.mu.Unlock()

	if ok || !strings.Contains(res, "not wired") {
		t.Errorf("a nil runner should surface a not-wired error; got %q ok=%v", res, ok)
	}
}
