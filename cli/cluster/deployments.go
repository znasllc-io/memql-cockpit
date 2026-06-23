package cluster

// Deployments section for the Clusters tab (memql-cockpit#207 / #221).
// This is the integrative deliverable of the Deployment & Topology
// Overhaul (epic znasllc-io/memql#1871): the bottom band of the right
// pane's persistent split (#221) -- always visible below the live
// topology grid -- that renders the v1:cluster:deployment history
// newest-first, lets the operator select a deployment to see its own
// topology (nodes filtered by deploymentId, with orphans highlighted),
// and exposes the deploy-control lifecycle (cut-version / deploy /
// rollback) through the SDK DeployControlClient wrappers landed in
// memql#1886.
//
// This concept-driven section is the SINGLE deployment surface: the
// earlier deployment-v2 status strip + control modal (deploy.go /
// deploy_controls.go / deploy_grpcstatus.go) were removed in #221.
//
// Role gating (the locked matrix, epic #1871):
//   - view              -- any connected role
//   - cut-version/deploy -- developer, admin, owner
//   - rollback          -- owner only (re-promotes a historical digest)
//
// Thread-safety: all state below lives on the topology View under v.mu,
// reusing the same lock that serializes Draw against the per-cluster
// subscriber goroutine. Mutators (SetDeployments / SetDeploymentNodes)
// take the write lock; Draw reads under RLock. Async SDK calls fire in a
// goroutine that re-acquires the lock to store results.

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
// from DeploymentsForCluster by the app layer's parse helper.
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
	// Drop the node/orphan + spec caches if they no longer match the
	// selection. A preview already on screen for the previous selection is
	// stale once the row underneath it changed, so reloads are requested
	// lazily by the key handler / app poll loop.
	if v.deployLoadedFor != "" {
		if v.deploySelected >= len(deps) || deps[v.deploySelected].ID != v.deployLoadedFor {
			v.clearPreviewCacheLocked()
		}
	}
	// If the selection became invalid (history shrank to empty), there is
	// nothing to preview -- fall back to the live grid.
	if v.deploySelected >= len(deps) {
		v.previewActive = false
		v.previewFullscreen = false
	}
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
// loaded via NodesForDeployment + NodesNotInDeployment. Keyed
// by deploymentId so a stale async result for a since-changed selection
// is dropped rather than rendered against the wrong row.
func (v *View) SetDeploymentNodes(deploymentID string, nodes, orphans []NodeInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deployNodes = nodes
	v.deployOrphans = orphans
	v.deployLoadedFor = deploymentID
}

// NodeSpecInfo is one v1:cluster:deploymentNodeSpec row, projected for
// display: the desired per-node-type spec (version / replicas / image
// digest) for a deployment. Loaded via NodeSpecsForDeployment -- the
// Epic 2 deploy-pack query (memql#2094) -- and surfaced in the
// composition view so the operator sees the INTENT (what each tier should
// run) next to the live node reality.
type NodeSpecInfo struct {
	NodeType    string
	Version     string
	Replicas    int
	ImageDigest string
}

// SetDeploymentNodeSpecs stores the per-node-type spec set for a
// deployment. Keyed by deploymentId like SetDeploymentNodes so a stale
// async result is dropped. Wired from the app's loadDeploymentNodes
// alongside the node/orphan load.
func (v *View) SetDeploymentNodeSpecs(deploymentID string, specs []NodeSpecInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deploySpecs = specs
	v.deploySpecsLoadedFor = deploymentID
}

// previewReadyLocked reports whether a deployment preview can be rendered:
// preview mode is on, a deployment is selected, and its node set has
// finished loading for the current selection. Caller MUST hold v.mu.
func (v *View) previewReadyLocked() bool {
	if !v.previewActive {
		return false
	}
	if v.deploySelected < 0 || v.deploySelected >= len(v.deployments) {
		return false
	}
	sel := v.deployments[v.deploySelected].ID
	return sel != "" && v.deployLoadedFor == sel
}

// previewNodesLocked returns the node set the preview renders -- the
// deployment's own nodes followed by its orphans -- plus an orphan
// predicate keyed by index (orphans are the tail). The combined set feeds
// computeEdges so the preview gets the same hierarchical layout + lines as
// the live grid. Caller MUST hold v.mu.
func (v *View) previewNodesLocked() ([]NodeInfo, func(i int) bool) {
	n := len(v.deployNodes)
	nodes := make([]NodeInfo, 0, n+len(v.deployOrphans))
	nodes = append(nodes, v.deployNodes...)
	nodes = append(nodes, v.deployOrphans...)
	return nodes, func(i int) bool { return i >= n }
}

// clearPreviewCacheLocked drops the per-deployment node + spec caches and
// resets the loaded-for keys, used when the selection moves. Caller MUST
// hold v.mu.
func (v *View) clearPreviewCacheLocked() {
	v.deployNodes, v.deployOrphans, v.deployLoadedFor = nil, nil, ""
	v.deploySpecs, v.deploySpecsLoadedFor = nil, ""
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

// specSummaryLocked renders the loaded deploymentNodeSpec set as a compact
// "type ver xN" comma list, ordered as the specs arrived. Caller holds
// v.mu. Empty when no specs are loaded.
func (v *View) specSummaryLocked() string {
	parts := make([]string, 0, len(v.deploySpecs))
	for _, s := range v.deploySpecs {
		seg := nodeTypeShort(s.NodeType)
		if s.Version != "" {
			seg += " " + s.Version
		}
		if s.Replicas > 0 {
			seg += fmt.Sprintf(" x%d", s.Replicas)
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, ", ")
}

// drawDeploymentPreview renders the selected deployment's composition
// graphically into region (memql-cockpit#225): a "preview: deployment
// <id>" header, optionally the desired per-tier spec (Epic 2
// deploymentNodeSpec), then the deployment's nodes drawn with the shared
// topology renderer -- its own nodes laid out normally and its orphans
// (nodes-not-in-deployment) flagged. Used by both the split top region and
// the fullscreen drill-down. Caller holds v.mu; assumes v.previewActive.
func (v *View) drawDeploymentPreview(screen *ui.Screen, region ui.Rect) {
	hdrStyle := tcell.StyleDefault.Foreground(v.Theme.Warning).Background(v.Theme.BG).Bold(true)
	muted := v.Theme.SubtleStyle()

	if v.deploySelected < 0 || v.deploySelected >= len(v.deployments) {
		screen.DrawText(region.X+2, region.Y+1, region.Width-3, "preview: no deployment selected", hdrStyle)
		return
	}
	d := v.deployments[v.deploySelected]
	hdr := "preview: deployment " + shortID(d.ID)
	if tok, _ := deployStatusToken(d.Status, v.Theme); tok != "" {
		hdr += " (" + tok + ")"
	}
	screen.DrawText(region.X+2, region.Y+1, region.Width-3, clipText(hdr, region.Width-3), hdrStyle)

	// Desired per-tier spec (Epic 2 deploymentNodeSpec, memql#2094), when
	// loaded for this deployment -- the intent next to the live composition.
	if v.deploySpecsLoadedFor == d.ID && len(v.deploySpecs) > 0 {
		spec := "spec: " + v.specSummaryLocked()
		screen.DrawText(region.X+2, region.Y+2, region.Width-3, clipText(spec, region.Width-3), muted)
	}

	if !v.previewReadyLocked() {
		screen.DrawText(region.X+2, region.Y+3, region.Width-4, "loading deployment topology...", muted)
		return
	}
	nodes, isOrphan := v.previewNodesLocked()
	if len(nodes) == 0 {
		screen.DrawText(region.X+2, region.Y+3, region.Width-4, "This deployment has no recorded nodes.", muted)
		return
	}
	v.drawTopology(screen, region, nodes, computeEdges(nodes), isOrphan)
}

// drawDeployments renders the Deployments section into the bottom band
// of the persistent split (memql-cockpit#221). `bounds` is the
// deployments sub-region Draw computed -- NOT the whole pane -- so the
// list + detail block bottom-anchor cleanly to the band floor and the
// region's own internal math never bleeds into the topology grid above.
// Caller (Draw) holds v.mu.RLock. The deployments hint bar lives on the
// pane's last row, drawn by Draw (just below this region). Layout,
// top-to-bottom within the band:
//
//	HISTORY     section header
//	<list>      deployment rows, newest-first, cursor-highlighted
//	─────
//	DETAIL      selected deployment fields + its topology (nodes/orphans)
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

	// History list + detail block share the band. The detail block prefers
	// detailMax rows (id/provenance/spec + per-node composition) but is
	// shrunk toward detailMin so the list always keeps a few rows -- a
	// "deployment list" that only ever shows one entry isn't a list
	// (memql-cockpit#235). The list scrolls within what remains via a simple
	// cursor-window (no scrollbar chrome).
	const (
		detailMax = 9
		detailMin = 6
		wantList  = 3 // rows we try to keep for the history list
	)
	detailH := detailMax
	avail := bounds.Height - 3 // rows below the header, for list + detail
	if avail-detailH < wantList {
		detailH = avail - wantList
	}
	if detailH < detailMin {
		detailH = detailMin
	}
	if detailH > avail {
		detailH = avail
	}
	listBottom := bounds.Y + bounds.Height - 1 - detailH
	if listBottom < y+1 {
		listBottom = y + 1
	}
	rows := listBottom - y
	start := 0
	if rows > 0 && v.deploySelected >= rows {
		start = v.deploySelected - rows + 1
	}
	// The current (live) deployment is resolved once and flagged per row so
	// the newest-first list shows which entry is actually running (green).
	currentID := v.currentDeploymentIdLocked()
	for i := start; i < len(v.deployments) && y < listBottom; i++ {
		d := v.deployments[i]
		sel := i == v.deploySelected
		isCurrent := d.ID != "" && d.ID == currentID
		v.drawDeploymentRow(screen, x, y, maxW, d, sel, isCurrent)
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

// drawDeploymentRow paints one history row: a current-deployment marker +
// status token + version + env + provider summary. The selected row gets
// the selection background; the status token keeps its semantic color. The
// CURRENT (live) deployment is flagged with a green "●" marker and its
// version/meta rendered in green so the operator can pick out -- at a
// glance, in a newest-first list -- which historical row is the one
// actually running now (memql-cockpit#235).
func (v *View) drawDeploymentRow(screen *ui.Screen, x, y, maxW int, d DeploymentInfo, sel, isCurrent bool) {
	rowStyle := v.Theme.BaseStyle()
	if sel {
		rowStyle = v.Theme.SelectionStyle()
		screen.FillRect(x-2, y, maxW+4, 1, rowStyle)
	}
	// Two-column current marker, always reserved so current + non-current
	// rows stay column-aligned. Green dot == the live deployment.
	marker := "  "
	markerStyle := rowStyle
	if isCurrent {
		marker = "● " // ● + space
		markerStyle = rowStyle.Foreground(v.Theme.Success)
	}
	screen.DrawText(x, y, 2, marker, markerStyle)
	tx := x + 2

	// Status token, ASCII-only and color-coded (no edge glyphs -- this is
	// interior text, but keep it ASCII so width is unambiguous).
	token, tokenColor := deployStatusToken(d.Status, v.Theme)
	cx := drawColoredToken(screen, tx, y, x+maxW, fmt.Sprintf("%-11s", token), rowStyle.Foreground(tokenColor))
	version := d.Version
	if version == "" {
		version = "(no version)"
	}
	rest := fmt.Sprintf(" %s  %s  %s", version, shortEnv(d.Environment), d.Provider)
	restStyle := rowStyle
	if isCurrent {
		restStyle = rowStyle.Foreground(v.Theme.Success)
	}
	screen.DrawText(cx, y, x+maxW-cx, rest, restStyle)
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

	// Line 1: id + status + (current) marker.
	token, tokenColor := deployStatusToken(d.Status, v.Theme)
	screen.DrawText(x, y, maxW, "DEPLOYMENT "+shortID(d.ID), label)
	cx := drawColoredToken(screen, x+len("DEPLOYMENT ")+len(shortID(d.ID))+2, y, x+maxW, token, v.colorStyle(tokenColor))
	if d.ID != "" && d.ID == v.currentDeploymentIdLocked() {
		drawColoredToken(screen, cx+1, y, x+maxW, "(current)", v.colorStyle(v.Theme.Success))
	}
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
	if y > bottom {
		return
	}

	// Desired per-tier spec (Epic 2 deploymentNodeSpec, memql#2094): the
	// intent -- what version + how many replicas each node type should run --
	// alongside the live node reality below.
	if v.deploySpecsLoadedFor == d.ID && len(v.deploySpecs) > 0 {
		screen.DrawText(x, y, maxW, clipText("spec: "+v.specSummaryLocked(), maxW), muted)
		y++
		if y > bottom {
			return
		}
	}

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
		{Key: "Enter", Label: "Preview"},
		{Key: "F", Label: "Fullscreen"},
	}
	// While a preview is on screen, surface the way back to the live grid.
	if v.previewActive {
		chips = append(chips, ui.HintChip{Key: "Esc", Label: "Live"})
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
			v.clearPreviewCacheLocked()
			// While previewing, arrow keys switch which deployment is
			// previewed: reload the newly selected row's composition so the
			// graphical preview tracks the cursor.
			if v.previewActive {
				v.requestDeploymentNodesLocked()
			}
			v.requestRedrawLocked()
		}
		return true
	case tcell.KeyDown:
		if v.deploySelected < len(v.deployments)-1 {
			v.deploySelected++
			v.clearPreviewCacheLocked()
			if v.previewActive {
				v.requestDeploymentNodesLocked()
			}
			v.requestRedrawLocked()
		}
		return true
	case tcell.KeyEnter:
		// Enter is the drill-down escalator: live -> preview (graphical
		// composition in the top region) -> fullscreen drill-down. Esc
		// steps back out.
		if !v.previewActive {
			v.previewActive = true
			v.requestDeploymentNodesLocked()
		} else if !v.previewFullscreen {
			v.previewFullscreen = true
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyEscape:
		// Step back out one level: fullscreen -> preview -> live. Only
		// consume the key when there's a preview level to collapse, so Esc
		// is otherwise free for higher-level handlers.
		if v.previewFullscreen {
			v.previewFullscreen = false
			v.requestRedrawLocked()
			return true
		}
		if v.previewActive {
			v.previewActive = false
			v.requestRedrawLocked()
			return true
		}
		return false
	case tcell.KeyRune:
		switch key.Rune() {
		case 'f', 'F':
			// Jump straight to (or out of) the fullscreen drill-down,
			// activating preview + loading the composition on the way in.
			if v.previewFullscreen {
				v.previewFullscreen = false
			} else {
				if !v.previewActive {
					v.previewActive = true
					v.requestDeploymentNodesLocked()
				}
				v.previewFullscreen = true
			}
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
// background, mirroring warningStyle (deploy_shared.go).
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
