package cluster

// Deployments section for the Clusters tab (memql-cockpit#207). This is
// the integrative deliverable of the Deployment & Topology Overhaul
// (epic znasllc-io/memql#1871): a section *within* the right pane of the
// Clusters tab -- toggled alongside the live Topology -- that renders the
// v1:cluster:deployment history newest-first, lets the operator select a
// deployment to see its own topology (nodes filtered by deploymentId,
// with orphans highlighted), and exposes the deploy-control lifecycle
// (cut-version / deploy / rollback) through the SDK DeployControlClient
// wrappers landed in memql#1886.
//
// Distinct from the deployment-v2 status strip (deploy.go / #144) and the
// deployment-v2 control modal (deploy_controls.go / #145): those render
// the per-env Argo/Rollouts overlay state and fire DeployStaging /
// Promote / Rollback / RolloutAction. THIS section is concept-driven --
// it reads the deployment concept rows and fires the newer
// CutVersion / Deploy / RollbackDeployment wrappers.
//
// Role gating (the locked matrix, epic #1871):
//   - view              -- any connected role
//   - cut-version/deploy -- developer, admin, owner
//   - rollback          -- owner only (re-promotes a historical digest)
//
// Thread-safety: all state below lives on the topology View under v.mu,
// reusing the same lock that serializes Draw against the per-cluster
// subscriber goroutine. Mutators (SetDeployments / loadSelectedNodes)
// take the write lock; Draw reads under RLock. Async SDK calls fire in a
// goroutine that re-acquires the lock to store results, mirroring the
// deploy_controls.go modal.

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// expectedNodesPerType is the count the topology truthfulness checks
// render "live vs expected" against. The cluster runs a 2-replica
// cross-node mesh per node type (the multi-node default, epic #1871 /
// the cluster-aware test directive), so a type showing fewer than this
// is flagged. Exposed as a const so the renderer and tests agree.
const expectedNodesPerType = 2

// DeploymentInfo is one v1:cluster:deployment row, projected for display.
// Mirrors the concept fields the Deployments section renders; populated
// from QueryDeploymentsForCluster by the app layer's parse helper.
type DeploymentInfo struct {
	ID                   string // deploymentId (also the row id / Argo rollout hash)
	Version              string // memQL semver being deployed, e.g. "2026.6.21"
	ImageDigest          string // resolved container image digest sha256:...
	Provider             string // "docker-local" | "azure"
	Environment          string // "development" | "staging" | "production"
	Region               string // cloud region, e.g. "us-central1" | "local"
	Status               string // pending|in_progress|succeeded|failed|superseded|rolled_back
	TriggeredBy          string // v1:identity:user.id of the operator who initiated
	CreatedAt            string // deploy-start time (RFC3339)
	UpdatedAt            string // most recent status transition (RFC3339)
	PreviousDeploymentId string // deploymentId this one supersedes (optional)
	Notes                string // free-form operator notes (optional)
}

// deploymentReachedSucceeded reports whether the deployment ever reached a
// terminal-good state, which is the precondition for rollback (re-promote
// that image digest). Superseded/rolled_back deployments also had a
// succeeded image at some point, so they are valid rollback *targets*.
func (d DeploymentInfo) reachedSucceeded() bool {
	switch strings.ToLower(strings.TrimSpace(d.Status)) {
	case "succeeded", "superseded", "rolled_back":
		return true
	}
	return false
}

// isPending reports whether the deployment is cut-but-not-yet-shipped,
// i.e. the precondition for the Deploy action.
func (d DeploymentInfo) isPending() bool {
	return strings.EqualFold(strings.TrimSpace(d.Status), "pending")
}

// SetDeployments stores the deployment history (expected newest-first)
// and reconciles the selection + loaded-nodes cache. Called from the
// app's poll loop (refreshDeployments). Clamps the cursor and, when the
// selected deploymentId is still present, preserves it; otherwise resets
// to the newest row and drops the stale node/orphan cache.
func (v *View) SetDeployments(deps []DeploymentInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()

	prevID := ""
	if v.deploySelected >= 0 && v.deploySelected < len(v.deployments) {
		prevID = v.deployments[v.deploySelected].ID
	}
	v.deployments = deps

	// Re-find the previously selected deployment by id so a refresh that
	// reorders/extends the list doesn't yank the cursor off the row the
	// operator was looking at.
	v.deploySelected = 0
	for i, d := range deps {
		if d.ID != "" && d.ID == prevID {
			v.deploySelected = i
			break
		}
	}
	if v.deploySelected >= len(deps) {
		v.deploySelected = 0
	}
	// Drop the node/orphan cache if it no longer matches the selection.
	if v.deployLoadedFor != "" {
		if v.deploySelected >= len(deps) || deps[v.deploySelected].ID != v.deployLoadedFor {
			v.deployNodes = nil
			v.deployOrphans = nil
			v.deployLoadedFor = ""
		}
	}
}

// DeploymentsActive reports whether the right pane is currently showing
// the Deployments section (vs the live topology). Takes the read lock.
func (v *View) DeploymentsActive() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.showDeployments
}

// SelectedDeploymentID returns the deploymentId under the cursor, or "".
// Used by the app's node-load hook so it can fetch the right rows.
func (v *View) SelectedDeploymentID() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.deploySelected < 0 || v.deploySelected >= len(v.deployments) {
		return ""
	}
	return v.deployments[v.deploySelected].ID
}

// SetDeploymentNodes stores the node/orphan split for a deployment,
// loaded via QueryNodesForDeployment + QueryNodesNotInDeployment. Keyed
// by deploymentId so a stale async result for a since-changed selection
// is dropped rather than rendered against the wrong row.
func (v *View) SetDeploymentNodes(deploymentID string, nodes, orphans []NodeInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deployNodes = nodes
	v.deployOrphans = orphans
	v.deployLoadedFor = deploymentID
}

// canCutDeployLocked reports whether the caller's role admits cut-version
// / deploy. developer, admin, owner. Caller MUST hold v.mu.
func (v *View) canCutDeployLocked() bool {
	switch v.clusterRole {
	case client.RoleOwner, client.RoleAdmin, client.RoleDeveloper:
		return true
	}
	return false
}

// canRollbackLocked reports whether the caller may roll back -- owner
// only, per the locked matrix. Caller MUST hold v.mu.
func (v *View) canRollbackLocked() bool {
	return v.clusterRole == client.RoleOwner
}

// CanCutDeploy / CanRollback are the locking accessors used by the app's
// poll loop + tests.
func (v *View) CanCutDeploy() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.canCutDeployLocked()
}

func (v *View) CanRollback() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.canRollbackLocked()
}

// toggleDeploymentsLocked flips the Deployments section on/off. Entering
// requests a fresh history load via OnDeploymentsShown so the section
// opens populated rather than blank. Caller MUST hold v.mu (write).
func (v *View) toggleDeploymentsLocked() {
	v.showDeployments = !v.showDeployments
	if v.showDeployments {
		// Leaving any pan offset behind is fine; the section has its own
		// scroll model (cursor-based) and ignores panX/panY.
		if cb := v.OnDeploymentsShown; cb != nil {
			// Fire outside the lock to avoid re-entrant SetDeployments
			// deadlocking on v.mu (the callback queries off-thread and
			// calls back via SetDeployments).
			go cb()
		}
	}
}

// requestDeploymentNodesLocked asks the app layer to load the selected
// deployment's node/orphan split. Caller MUST hold v.mu (write); the
// callback runs off-thread and stores results via SetDeploymentNodes.
func (v *View) requestDeploymentNodesLocked() {
	id := ""
	if v.deploySelected >= 0 && v.deploySelected < len(v.deployments) {
		id = v.deployments[v.deploySelected].ID
	}
	if id == "" || v.OnSelectDeployment == nil {
		return
	}
	cb := v.OnSelectDeployment
	go cb(id)
}

// drawDeployments renders the Deployments section into the right pane.
// Caller (Draw) holds v.mu.RLock. Layout, top-to-bottom:
//
//	[name]      cluster name (drawn by Draw before this)
//	HISTORY     section header
//	<list>      deployment rows, newest-first, cursor-highlighted
//	─────
//	DETAIL      selected deployment fields + its topology (nodes/orphans)
//	[status]    bottom hint bar (role-gated controls)
func (v *View) drawDeployments(screen *ui.Screen, bounds ui.Rect) {
	x := bounds.X + 2
	maxW := bounds.Width - 4
	if maxW <= 0 {
		return
	}
	headerStyle := tcell.StyleDefault.Foreground(v.Theme.Info).Background(v.Theme.BG).Bold(true)
	muted := v.Theme.SubtleStyle()

	y := bounds.Y + 2
	screen.DrawText(x, y, maxW, "DEPLOYMENT HISTORY", headerStyle)
	y++

	if len(v.deployments) == 0 {
		screen.DrawText(x, y, maxW, "No deployments recorded for this cluster yet.", muted)
	}

	// History list. Reserve the lower third of the pane for the detail
	// block + status bar; the list scrolls within what's left. The list
	// is small (latest + a handful of previous) so a simple cursor-window
	// is enough -- no scrollbar chrome.
	detailH := 9
	listBottom := bounds.Y + bounds.Height - 1 - detailH
	if listBottom < y+1 {
		listBottom = y + 1
	}
	rows := listBottom - y
	start := 0
	if rows > 0 && v.deploySelected >= rows {
		start = v.deploySelected - rows + 1
	}
	for i := start; i < len(v.deployments) && y < listBottom; i++ {
		d := v.deployments[i]
		sel := i == v.deploySelected
		v.drawDeploymentRow(screen, x, y, maxW, d, sel)
		y++
	}

	// Detail block for the selected deployment.
	detailTop := bounds.Y + bounds.Height - 1 - detailH
	if detailTop > bounds.Y+2 {
		for i := 0; i < maxW; i++ {
			screen.SetCell(x+i, detailTop, '-', muted)
		}
		v.drawDeploymentDetail(screen, x, detailTop+1, maxW, bounds)
	}
}

// drawDeploymentRow paints one history row: status token + version + env
// + provider + relative summary. The selected row gets the selection
// background; the status token keeps its semantic color.
func (v *View) drawDeploymentRow(screen *ui.Screen, x, y, maxW int, d DeploymentInfo, sel bool) {
	rowStyle := v.Theme.BaseStyle()
	if sel {
		rowStyle = v.Theme.SelectionStyle()
		screen.FillRect(x-2, y, maxW+4, 1, rowStyle)
	}
	// Status token, ASCII-only and color-coded (no edge glyphs -- this is
	// interior text, but keep it ASCII so width is unambiguous).
	token, tokenColor := deployStatusToken(d.Status, v.Theme)
	cx := drawColoredToken(screen, x, y, x+maxW, fmt.Sprintf("%-11s", token), rowStyle.Foreground(tokenColor))
	version := d.Version
	if version == "" {
		version = "(no version)"
	}
	rest := fmt.Sprintf(" %s  %s  %s", version, shortEnv(d.Environment), d.Provider)
	screen.DrawText(cx, y, x+maxW-cx, rest, rowStyle)
}

// drawDeploymentDetail renders the selected deployment's fields plus its
// own topology (nodes filtered by deploymentId, orphans highlighted).
func (v *View) drawDeploymentDetail(screen *ui.Screen, x, y, maxW int, bounds ui.Rect) {
	muted := v.Theme.SubtleStyle()
	label := tcell.StyleDefault.Foreground(v.Theme.FG).Background(v.Theme.BG).Bold(true)
	bottom := bounds.Y + bounds.Height - 2 // last row before the status bar

	if v.deploySelected < 0 || v.deploySelected >= len(v.deployments) {
		screen.DrawText(x, y, maxW, "Select a deployment to see its topology.", muted)
		return
	}
	d := v.deployments[v.deploySelected]

	// Line 1: id + status + version.
	token, tokenColor := deployStatusToken(d.Status, v.Theme)
	screen.DrawText(x, y, maxW, "DEPLOYMENT "+shortID(d.ID), label)
	drawColoredToken(screen, x+len("DEPLOYMENT ")+len(shortID(d.ID))+2, y, x+maxW, token, v.colorStyle(tokenColor))
	y++
	if y > bottom {
		return
	}
	// Line 2: provenance.
	line := fmt.Sprintf("ver=%s  env=%s  provider=%s  by=%s",
		orDash(d.Version), orDash(shortEnv(d.Environment)), orDash(d.Provider), orDash(shortID(d.TriggeredBy)))
	screen.DrawText(x, y, maxW, line, muted)
	y++
	if y > bottom {
		return
	}

	// Topology for this deployment: count vs expected + orphan flag.
	if v.deployLoadedFor != d.ID {
		screen.DrawText(x, y, maxW, "loading topology...", muted)
		return
	}
	count := len(v.deployNodes)
	summary := fmt.Sprintf("Nodes: %d", count)
	if orphans := len(v.deployOrphans); orphans > 0 {
		summary += fmt.Sprintf("   Orphans (other/no deployment): %d", orphans)
	}
	screen.DrawText(x, y, maxW, summary, label)
	y++

	// Per-node lines: type health version -- the node belongs to THIS
	// deployment, so deploymentId is implied; we surface health+version
	// which is what staleness checks care about.
	for _, n := range v.deployNodes {
		if y > bottom {
			break
		}
		health := healthLabel(n.Health)
		nline := fmt.Sprintf("  %-10s %-10s %s", nodeTypeShort(n.Type), health, orDash(n.Version))
		screen.DrawText(x, y, maxW, nline, v.colorStyle(nodeHealthColor(n.Health)))
		y++
	}
	// Orphans rendered in the warning color so they stand out as
	// "running but not part of this deployment".
	for _, n := range v.deployOrphans {
		if y > bottom {
			break
		}
		oline := fmt.Sprintf("  %-10s %-10s dep=%s [orphan]", nodeTypeShort(n.Type), healthLabel(n.Health), orDash(shortID(n.DeploymentId)))
		screen.DrawText(x, y, maxW, oline, v.warningStyle())
		y++
	}
}

// hintsForDeployments builds the bottom hint bar for the Deployments
// section -- context+role aware so only usable controls surface.
// Caller MUST hold v.mu.
func (v *View) hintsForDeployments() string {
	chips := []ui.HintChip{
		{Key: "Up/Dn", Label: "Move"},
		{Key: "Enter", Label: "Topology"},
	}
	canCut := v.canCutDeployLocked()
	var sel DeploymentInfo
	haveSel := v.deploySelected >= 0 && v.deploySelected < len(v.deployments)
	if haveSel {
		sel = v.deployments[v.deploySelected]
	}
	chips = append(chips,
		ui.HintChip{Key: "C", Label: "Cut", Disabled: !canCut},
		ui.HintChip{Key: "G", Label: "Deploy", Disabled: !canCut || !haveSel || !sel.isPending()},
		ui.HintChip{Key: "B", Label: "Rollback", Disabled: !v.canRollbackLocked() || !haveSel || !sel.reachedSucceeded()},
		ui.HintChip{Key: "P", Label: "Topology view"},
	)
	return ui.HintBar{Chips: chips}.String()
}

// handleDeploymentsKeyLocked routes keys while the Deployments section is
// active. Caller MUST hold v.mu (write).
func (v *View) handleDeploymentsKeyLocked(key *tcell.EventKey) bool {
	switch key.Key() {
	case tcell.KeyUp:
		if v.deploySelected > 0 {
			v.deploySelected--
			v.deployNodes, v.deployOrphans, v.deployLoadedFor = nil, nil, ""
			v.requestRedrawLocked()
		}
		return true
	case tcell.KeyDown:
		if v.deploySelected < len(v.deployments)-1 {
			v.deploySelected++
			v.deployNodes, v.deployOrphans, v.deployLoadedFor = nil, nil, ""
			v.requestRedrawLocked()
		}
		return true
	case tcell.KeyEnter:
		v.requestDeploymentNodesLocked()
		v.requestRedrawLocked()
		return true
	case tcell.KeyEscape:
		v.showDeployments = false
		v.requestRedrawLocked()
		return true
	case tcell.KeyRune:
		switch key.Rune() {
		case 'p', 'P':
			v.showDeployments = false
			v.requestRedrawLocked()
			return true
		case 'c', 'C':
			if v.canCutDeployLocked() {
				v.openCutModalLocked()
				v.requestRedrawLocked()
			}
			return true
		case 'g', 'G':
			if v.canCutDeployLocked() && v.selectedIsPendingLocked() {
				v.openDeployModalConceptLocked()
				v.requestRedrawLocked()
			}
			return true
		case 'b', 'B':
			if v.canRollbackLocked() && v.selectedReachedSucceededLocked() {
				v.openRollbackModalLocked()
				v.requestRedrawLocked()
			}
			return true
		}
	}
	return false
}

// selectedIsPendingLocked / selectedReachedSucceededLocked are small
// guards used by the key handler + hint bar. Caller holds v.mu.
func (v *View) selectedIsPendingLocked() bool {
	if v.deploySelected < 0 || v.deploySelected >= len(v.deployments) {
		return false
	}
	return v.deployments[v.deploySelected].isPending()
}

func (v *View) selectedReachedSucceededLocked() bool {
	if v.deploySelected < 0 || v.deploySelected >= len(v.deployments) {
		return false
	}
	return v.deployments[v.deploySelected].reachedSucceeded()
}

// colorStyle is a small helper wrapping a foreground color over the base
// background, mirroring successStyle/warningStyle/errorStyle.
func (v *View) colorStyle(c tcell.Color) tcell.Style {
	return tcell.StyleDefault.Foreground(c).Background(v.Theme.BG)
}

// deployStatusToken maps a deployment status to a display token + color.
func deployStatusToken(status string, theme ui.Theme) (string, tcell.Color) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded":
		return "succeeded", theme.Success
	case "in_progress":
		return "in-progress", theme.Warning
	case "pending":
		return "pending", theme.Info
	case "failed":
		return "failed", theme.Error
	case "superseded":
		return "superseded", theme.Subtle
	case "rolled_back":
		return "rolled-back", theme.Subtle
	case "":
		return "unknown", theme.Subtle
	default:
		return status, theme.Subtle
	}
}

// shortEnv abbreviates the verbose environment enum for compact rows.
func shortEnv(env string) string {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "production":
		return "prod"
	case "development":
		return "dev"
	case "":
		return "-"
	default:
		return env
	}
}

// shortID trims a long id (deploymentId / userId) to a readable prefix.
func shortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// orDash renders "-" for an empty string so detail rows never show a
// dangling label with nothing after it.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
