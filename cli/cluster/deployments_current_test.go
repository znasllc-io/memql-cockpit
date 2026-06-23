package cluster

// Tests for the deployment-list polish (memql-cockpit#235): newest-first
// ordering, the current (live) deployment flagged with a green "●" marker,
// and the rollback role gate (owner-only, matching the backend
// RollbackDeployment authorizeOwner gate -- developer/admin may NOT roll
// back).

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// rowWith returns the first rendered row containing sub, or "".
func rowWith(rows []string, sub string) string {
	for _, r := range rows {
		if strings.Contains(r, sub) {
			return r
		}
	}
	return ""
}

func TestDeploymentList_CurrentDeploymentMarkedGreen(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	// Newest-first: the newest succeeded deployment is the current/live one.
	v.SetDeployments([]DeploymentInfo{
		{ID: "dep-new", Version: "9.9.9", Status: "succeeded"},
		{ID: "dep-old", Version: "1.1.1", Status: "superseded"},
	})
	rows := renderTopology(t, v, 120, 30)

	curRow := rowWith(rows, "9.9.9")
	if curRow == "" {
		t.Fatalf("current deployment row (9.9.9) not rendered:\n%s", strings.Join(rows, "\n"))
	}
	if !strings.Contains(curRow, "●") {
		t.Errorf("current deployment row should carry the green ● marker; got %q", curRow)
	}

	oldRow := rowWith(rows, "1.1.1")
	if oldRow == "" {
		t.Fatalf("historical deployment row (1.1.1) not rendered")
	}
	if strings.Contains(oldRow, "●") {
		t.Errorf("non-current deployment row should NOT carry the ● marker; got %q", oldRow)
	}
}

func TestDeploymentList_NewestFirstOrdering(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetDeployments([]DeploymentInfo{
		{ID: "dep-new", Version: "9.9.9", Status: "succeeded"},
		{ID: "dep-old", Version: "1.1.1", Status: "superseded"},
	})
	rows := renderTopology(t, v, 120, 30)

	var newIdx, oldIdx = -1, -1
	for i, r := range rows {
		if newIdx < 0 && strings.Contains(r, "9.9.9") {
			newIdx = i
		}
		if oldIdx < 0 && strings.Contains(r, "1.1.1") {
			oldIdx = i
		}
	}
	if newIdx < 0 || oldIdx < 0 {
		t.Fatalf("expected both rows rendered; newIdx=%d oldIdx=%d", newIdx, oldIdx)
	}
	if newIdx > oldIdx {
		t.Errorf("newest deployment should render above older (newest-first); newIdx=%d oldIdx=%d", newIdx, oldIdx)
	}
}

func TestDeploymentList_InProgressIsCurrent(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	// No succeeded row yet, but an in-progress one is the live target.
	v.SetDeployments([]DeploymentInfo{
		{ID: "dep-ship", Version: "5.0.0", Status: "in_progress"},
		{ID: "dep-prev", Version: "4.0.0", Status: "superseded"},
	})
	rows := renderTopology(t, v, 120, 30)
	if cur := rowWith(rows, "5.0.0"); !strings.Contains(cur, "●") {
		t.Errorf("in-progress deployment should be flagged current; got %q", cur)
	}
}

func TestRollback_OwnerOnly_NotDeveloperOrAdmin(t *testing.T) {
	// The backend RollbackDeployment is owner-only (deploycontrol
	// authorizeOwner / auth.IsOwner, memql#1876 -- "not even admin may roll
	// back"). The UI gate MUST match: gating it looser would let a
	// developer trigger a guaranteed PermissionDenied. cut/deploy stay
	// developer-or-above.
	cases := []struct {
		role            client.Role
		wantCut, wantRb bool
	}{
		{client.RoleOwner, true, true},
		{client.RoleAdmin, true, false},
		{client.RoleDeveloper, true, false},
		{client.RoleReader, false, false},
	}
	for _, tc := range cases {
		v := NewView(ui.DefaultTheme())
		v.SetClusterRole(tc.role)
		if got := v.CanCutDeploy(); got != tc.wantCut {
			t.Errorf("role %v: CanCutDeploy=%v, want %v", tc.role, got, tc.wantCut)
		}
		if got := v.CanRollback(); got != tc.wantRb {
			t.Errorf("role %v: CanRollback=%v, want %v", tc.role, got, tc.wantRb)
		}
	}
}

func TestRollback_HintReflectsPermission(t *testing.T) {
	// Owner sees an enabled Rollback chip; a developer sees it disabled
	// (rendered dimmed by the HintBar) -- the action key is also a no-op.
	owner := NewView(ui.DefaultTheme())
	owner.SetClusterRole(client.RoleOwner)
	owner.SetDeployments([]DeploymentInfo{{ID: "d", Version: "1", Status: "succeeded"}})
	if !owner.canRollbackLocked() {
		t.Fatal("owner should be permitted to roll back")
	}

	dev := NewView(ui.DefaultTheme())
	dev.SetClusterRole(client.RoleDeveloper)
	dev.SetDeployments([]DeploymentInfo{{ID: "d", Version: "1", Status: "succeeded"}})
	// Pressing rollback as a developer must not open the modal.
	dev.HandleEvent(keyRune('b'))
	if modalOpen(dev) {
		t.Error("developer pressing rollback should not open the rollback modal")
	}
}
