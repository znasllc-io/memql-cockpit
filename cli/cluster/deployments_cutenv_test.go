package cluster

// Tests for the de-env-ified cut modal (memql-cockpit#236, closes #226): a
// cluster IS one environment, so the cut flow resolves the env from the
// cluster record instead of offering a {staging,production,development}
// picker. The modal opens straight on the bump picker.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

func TestCut_OpensOnBumpStage_NoEnvPicker(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetClusterRole(client.RoleDeveloper)
	v.SetClusterEnvironment("staging")

	v.HandleEvent(keyRune('C')) // open cut

	v.mu.RLock()
	m := v.dctrl
	v.mu.RUnlock()
	if m == nil {
		t.Fatal("pressing C should open the cut modal")
	}
	if m.stage != dcStageCutBump {
		t.Errorf("cut modal should open on the bump picker (no env stage); got stage %d", m.stage)
	}
	if m.env != "staging" {
		t.Errorf("cut modal env should be resolved to the cluster env; got %q", m.env)
	}

	// The rendered modal must show the resolved env and NOT an env picker.
	joined := strings.Join(renderTopology(t, v, 100, 30), "\n")
	if !strings.Contains(joined, "STAGING") {
		t.Errorf("cut modal should display the resolved env STAGING; got:\n%s", joined)
	}
	if strings.Contains(joined, "pick environment") {
		t.Errorf("cut modal must not offer an environment picker; got:\n%s", joined)
	}
}

func TestCutEnvResolution(t *testing.T) {
	// 1. Explicit cluster env wins.
	v := NewView(ui.DefaultTheme())
	v.SetClusterEnvironment("production")
	v.SetDeployments([]DeploymentInfo{{ID: "d", Environment: "staging", Status: "succeeded"}})
	if got := v.cutEnvLocked(); got != "production" {
		t.Errorf("cluster env should win; got %q", got)
	}

	// 2. No cluster env -> newest deployment's environment.
	v2 := NewView(ui.DefaultTheme())
	v2.SetDeployments([]DeploymentInfo{
		{ID: "new", Environment: "staging", Status: "succeeded"},
		{ID: "old", Environment: "development", Status: "superseded"},
	})
	if got := v2.cutEnvLocked(); got != "staging" {
		t.Errorf("should fall back to newest deployment env; got %q", got)
	}

	// 3. Nothing known -> development default.
	v3 := NewView(ui.DefaultTheme())
	if got := v3.cutEnvLocked(); got != "development" {
		t.Errorf("should default to development; got %q", got)
	}
}
