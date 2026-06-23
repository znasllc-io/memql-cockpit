// Package cluster provides the Cluster tab for memQL Cockpit.
// It renders a visual topology diagram of cluster nodes using tcell
// box-drawing characters for crisp, scalable rendering.
package cluster

import (
	"fmt"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// NodeInfo describes a cluster node for rendering. The Health field is the
// proto-defined enum from component/node/node.proto -- the source of truth
// for node states across backend, concept schema, and CLI. Display labels
// and colors are derived from the enum value via healthLabel() /
// nodeHealthColor().
//
// `Type` is the free-form nodeType string from v1:cluster:node (e.g. "bff",
// "voice", "cognition", "agent", "planner"). There is no hard-coded
// whitelist here: whatever types the server reports get rendered, grouped
// by the order established via SetNodeTypes (seeded from
// v1:cluster:nodeType) with unknown types appended at the end.
type NodeInfo struct {
	ID      string
	Name    string
	Type    string
	Address string
	Version string
	Health  nodev1.NodeHealthStatus
	Labels  map[string]string

	// DeploymentId is the stable id of the v1:cluster:deployment that
	// created this node (FK to v1:cluster:deployment.deploymentId). Empty
	// for nodes registered before deployment tracking, or hand-started
	// nodes. Used by the Deployments section to filter a deployment's
	// topology and to flag orphans (a node whose deploymentId is not the
	// expected/current one). Per memql-cockpit#207.
	DeploymentId string

	// ParentId is the node id this node discovered the mesh through (the
	// peer it registered from). Drives the parent/child edges the
	// topology renders so the diagram reflects the real discovery graph
	// rather than only the static service-relationship map. Empty for the
	// seed node. Per memql-cockpit#207.
	ParentId string
}

// NodeTypeInfo is a row from the v1:cluster:nodeType concept. The CLI
// fetches these once on connect (via queryClusterNodeTypes) and uses
// Name + seed order to drive the topology display. Description is kept
// around for future tooltip / detail-pane work.
type NodeTypeInfo struct {
	Name        string
	Description string
}

// View renders the Cluster topology using tcell box-drawing characters.
//
// Thread-safety: cluster-node + nodeType events arrive on the
// per-cluster runSubscriber goroutine (see pool.go) and mutate
// this view via ApplyNodeUpdate / SetNodes / SetNodeTypes /
// SetDisconnected. The tcell event loop concurrently calls Draw +
// HandleEvent. Without locking, Draw can see torn state (e.g. a
// half-populated Nodes slice or an ApplyNodeUpdate mid-write),
// which surfaces visually as ghosting once the per-frame paint
// uses Show()-style diff emission. mu serializes both writers
// (Lock) and Draw (RLock).
type View struct {
	mu        sync.RWMutex
	Theme     ui.Theme
	Nodes     []NodeInfo
	NodeTypes []NodeTypeInfo // seed order from v1:cluster:nodeType, drives row layout
	Edges     [][2]int       // pairs of node indices that are connected

	// OnInitialLoad is called once at startup and on reconnect to pull the
	// initial topology snapshot via queryClusterNodes(). After that, live updates
	// arrive via the gRPC subscription in wireCluster and are applied
	// through ApplyNodeUpdate -- no manual refresh.
	OnInitialLoad func() []NodeInfo

	// panX, panY shift the diagram within the drawing pane. Positive values
	// move the diagram left/up (revealing content to the right/below).
	// Driven by the W/A/S/D keys in HandleEvent.
	panX int
	panY int

	// disconnected is true when the CLI's gRPC stream to the cluster is
	// down (detected via Dispatcher.Done). While disconnected, every box
	// renders red/offline regardless of the last-known Health, because
	// we have no live signal -- the cached state is stale.
	disconnected bool

	// ClusterName is the display name of the cluster whose topology is
	// being rendered. Prefixed onto the inner section header ("Local
	// CLUSTER TOPOLOGY") so the user always knows which cluster the
	// diagram represents. Empty renders as just "CLUSTER TOPOLOGY".
	ClusterName string

	// Arch is the architecture-model navigator, layered on top of the
	// live topology. Toggled with 'X' (or 'x'); when active, this
	// pane renders the embedded architecture model's drill-down
	// instead of the live grid. The navigator owns its own keys
	// (arrows, Enter, Backspace, Esc); HandleEvent routes accordingly.
	Arch *ArchView

	// clusterRole is the connected caller's cluster-wide role, mirrored
	// from refreshMyAccess via SetClusterRole. Drives the role gating on
	// the Deployments section's cut/deploy/rollback controls (view = any
	// role; cut/deploy = developer/admin/owner; rollback = owner only).
	clusterRole client.Role

	// DeployClient returns a DeployControlClient bound to the currently
	// active cluster's connection, or nil when none is connected. Wired
	// in app.go's wireCluster() using the same closure pattern the other
	// views use for their QueryClient. The poll loop calls this each tick
	// so a cluster switch transparently retargets.
	DeployClient func() *client.DeployControlClient

	// OnRedraw, when set, requests a repaint of the topology pane.
	// Wired to App.postRedraw in app.go so an async deploy action's
	// result line lands on screen the moment the SDK call returns,
	// without waiting for the next poll tick. nil is a no-op (tests
	// that drive the modal state machine directly don't wire it).
	OnRedraw func()

	// --- Deployments section (memql-cockpit#207 / #221) ---
	//
	// The right pane is a persistent vertical split: the live topology
	// grid on top, the concept-driven Deployments section (history +
	// per-deployment topology + cut/deploy/rollback controls) anchored
	// to the bottom. Both are always visible -- there is no toggle. All
	// fields below are guarded by v.mu.
	deployments     []DeploymentInfo // v1:cluster:deployment history, newest-first
	deploySelected  int              // cursor into deployments
	deployNodes     []NodeInfo       // nodes for the selected deployment (QueryNodesForDeployment)
	deployOrphans   []NodeInfo       // nodes NOT in the selected deployment (QueryNodesNotInDeployment)
	deployLoadedFor string           // deploymentId the node/orphan cache was loaded for

	// dctrl holds the transient cut/deploy/rollback modal state for the
	// Deployments section. nil when closed. Distinct from ctrl (the
	// deployment-v2 overlay modal). Guarded by v.mu.
	dctrl *deployConceptState

	// deployConceptActor overrides the deployment-concept action executor.
	// Production leaves it nil and the modal resolves the real client
	// from DeployClient() per-fire; tests inject a fake.
	deployConceptActor deployConceptActions

	// OnDeploymentsShown is called (off-thread) so the app can fetch the
	// deployment history via QueryDeploymentsForCluster and push it back
	// through SetDeployments. Fired when a cluster's topology is wired up
	// (the Deployments section is always present, so the history is
	// loaded eagerly rather than on a toggle).
	OnDeploymentsShown func()

	// OnSelectDeployment is called (off-thread) with the selected
	// deploymentId when the operator drills into a deployment, so the app
	// can load its nodes (QueryNodesForDeployment) + orphans
	// (QueryNodesNotInDeployment) and push them back via SetDeploymentNodes.
	OnSelectDeployment func(deploymentID string)

	// OnDeploymentsChanged is called after a cut/deploy/rollback action
	// succeeds, so the app refreshes the history right away rather than on
	// the next poll tick. nil is a no-op.
	OnDeploymentsChanged func()
}

// SetDisconnected toggles the "stream lost" rendering override. Does not
// mutate node data; when the stream recovers the existing Health values
// resume display.
func (v *View) SetDisconnected(disconnected bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.disconnected = disconnected
}

// Pan step in cells per keypress. Horizontal gets a larger step because
// terminal cells are roughly half as wide as they are tall.
const (
	panStepX = 3
	panStepY = 2
)

// NewView creates an empty topology view. The ArchView is wired
// eagerly so the embedded model is decoded once and the toggle key
// produces an instant response (no first-touch latency).
func NewView(theme ui.Theme) *View {
	return &View{
		Theme: theme,
		Arch:  NewArchView(theme),
	}
}

// SetNodes updates the cluster topology data.
func (v *View) SetNodes(nodes []NodeInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Nodes = nodes
	v.buildEdgesLocked()
}

// SetNodeTypes stores the ordered nodeType list (from
// queryClusterNodeTypes). This is what drives the row order in the
// topology diagram -- the first entry is drawn on the top row, the
// rest in sequence. When empty, drawTopology falls back to the order
// in which types appear on registered nodes.
func (v *View) SetNodeTypes(types []NodeTypeInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.NodeTypes = types
}

// SetClusterRole mirrors the connected caller's cluster-wide role into
// the view. Wired from refreshMyAccess alongside the Settings update.
// Drives the role gating on the Deployments section's cut/deploy/rollback
// controls.
func (v *View) SetClusterRole(r client.Role) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.clusterRole = r
}

// ApplyNodeUpdate merges a single-node change into the topology model. If
// the node already exists (matched by ID, or by Type for single-node-per-
// type deployments) its record is updated in place; otherwise it is
// appended. Nodes that transitioned to OFFLINE or STOPPED remain in the
// view (colored red) rather than disappearing, so the user sees that the
// node went away rather than silently losing it from the diagram.
func (v *View) ApplyNodeUpdate(incoming NodeInfo) {
	if incoming.Type == "" && incoming.ID == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	idx := v.findNodeIndexLocked(incoming)
	if idx < 0 {
		v.Nodes = append(v.Nodes, incoming)
		v.buildEdgesLocked()
		return
	}
	// Preserve any fields the subscription payload didn't carry.
	existing := &v.Nodes[idx]
	if incoming.ID != "" {
		existing.ID = incoming.ID
	}
	if incoming.Name != "" {
		existing.Name = incoming.Name
	}
	if incoming.Type != "" {
		existing.Type = incoming.Type
	}
	if incoming.Address != "" {
		existing.Address = incoming.Address
	}
	if incoming.Version != "" {
		existing.Version = incoming.Version
	}
	if incoming.Health != nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED {
		existing.Health = incoming.Health
	}
	if incoming.Labels != nil {
		existing.Labels = incoming.Labels
	}
	if incoming.DeploymentId != "" {
		existing.DeploymentId = incoming.DeploymentId
	}
	if incoming.ParentId != "" {
		existing.ParentId = incoming.ParentId
	}
	v.buildEdgesLocked()
}

// findNodeIndexLocked is the lock-internal flavor of findNodeIndex;
// caller MUST hold v.mu (read or write). Renamed (vs. just adding a
// lock at the entry) because the function is called from
// ApplyNodeUpdate which already holds the write lock; re-entering a
// non-recursive RWMutex would deadlock.
func (v *View) findNodeIndexLocked(target NodeInfo) int {
	if target.ID != "" {
		for i, n := range v.Nodes {
			if n.ID == target.ID {
				return i
			}
		}
	}
	// Fall back to matching by type (initial load + spawn events only knew
	// type, so the first "real" event should update that row).
	if target.Type != "" {
		for i, n := range v.Nodes {
			if n.ID == "" && n.Type == target.Type {
				return i
			}
		}
	}
	return -1
}

// Draw renders the topology view. Holds the read lock for the full
// frame so a concurrent runSubscriber goroutine can't tear v.Nodes /
// v.NodeTypes / v.Edges mid-render. The ArchView overlay also reads
// state inside the lock when it's active.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	// Title. Just the cluster name in Info (lighter blue) -- the outer
	// " TOPOLOGY " pane title already says what this pane is, so a
	// "CLUSTER TOPOLOGY" subtitle was redundant noise.
	//
	// Cluster name is rendered VERBATIM -- no auto-title-casing. The
	// user's chosen capitalization in clusters.yaml (e.g. "Znasllc",
	// "local") is the source of truth.
	if v.ClusterName != "" {
		nameStyle := tcell.StyleDefault.Foreground(v.Theme.Info).Background(v.Theme.BG).Bold(true)
		screen.DrawText(bounds.X+2, bounds.Y, bounds.Width-3, v.ClusterName, nameStyle)
	}

	// Architecture navigator: when toggled, owns the whole drawing
	// area below the cluster-name title; the status bar below still
	// reflects the live topology (since that's the system-state truth
	// the navigator is layered over).
	if v.Arch.Active() {
		v.Arch.Draw(screen, bounds)
		return
	}

	// --- Persistent vertical split (memql-cockpit#221) ---
	//
	// The right pane is split into a live TOPOLOGY region on top and the
	// per-cluster DEPLOYMENTS section anchored to the bottom, both always
	// visible. Rect math, top -> bottom:
	//
	//   bounds.Y                       cluster-name title (drawn above)
	//   [topoRegion]                   topology grid + 1-row tally
	//   dividerY                       1-row full-width `─` divider
	//   [deployRegion]                 deployments list + detail block
	//   hintY (last row)               deployments hint bar
	//
	// deployRegion height is clamped to 40% of the pane (min 8, max 14)
	// so a tall terminal doesn't drown the topology and a short one still
	// shows a usable history list. The hint bar takes the very last row.
	statusStyle := tcell.StyleDefault.Foreground(v.Theme.Subtle).Background(v.Theme.BG)

	hintY := bounds.Y + bounds.Height - 1

	deployH := bounds.Height * 2 / 5 // ~40% of the pane
	if deployH < 8 {
		deployH = 8
	}
	if deployH > 14 {
		deployH = 14
	}
	// Never let the deployments band + divider + title swallow the whole
	// pane: reserve 1 row each for the title, divider and hint bar, plus a
	// 4-row floor for the topology region on a short pane.
	const reservedAboveBand = 1 + 1 + 1 + 4 // title + divider + hint + topology floor
	maxDeployH := bounds.Height - reservedAboveBand
	if maxDeployH < 0 {
		maxDeployH = 0
	}
	if deployH > maxDeployH {
		deployH = maxDeployH
	}

	// deployRegion spans the rows above the hint bar; the hint bar lives
	// on hintY (the pane's last row).
	deployRegion := ui.Rect{
		X:      bounds.X,
		Y:      hintY - deployH,
		Width:  bounds.Width,
		Height: deployH,
	}
	dividerY := deployRegion.Y - 1

	// topoRegion runs from the title down to (but not including) the
	// divider. Its Y stays at bounds.Y so drawTopology's internal
	// title-gap offset (area.Y = region.Y+3) lines up under the cluster
	// name; its Height clips the grid above the divider so it can't bleed
	// into the deployments band -- the overlap bug fix.
	topoRegion := ui.Rect{
		X:      bounds.X,
		Y:      bounds.Y,
		Width:  bounds.Width,
		Height: dividerY - bounds.Y,
	}

	// TODO(#221): graphical topology preview of the selected deployment --
	// for now the selected deployment's node composition is shown
	// textually in the deployments detail block below; the top region
	// always renders the live whole-cluster grid.
	if len(v.Nodes) > 0 && topoRegion.Height > 0 {
		v.drawTopology(screen, topoRegion)
	}

	// One-line topology tally at the bottom of the topology region. Same
	// content the old status bar carried; now it labels the grid above the
	// divider instead of the whole pane.
	tallyY := dividerY - 1
	if tallyY > topoRegion.Y {
		statusText := ""
		if len(v.Nodes) == 0 {
			statusText = " No nodes. Waiting for cluster data..."
		} else if v.disconnected {
			statusText = fmt.Sprintf(" Nodes: %d  Offline:%d", len(v.Nodes), len(v.Nodes))
		} else {
			healthy, degraded, offline := v.healthCounts()
			statusText = fmt.Sprintf(" Nodes: %d", len(v.Nodes))
			if healthy > 0 {
				statusText += fmt.Sprintf("  Online:%d", healthy)
			}
			if degraded > 0 {
				statusText += fmt.Sprintf("  Degraded:%d", degraded)
			}
			if offline > 0 {
				statusText += fmt.Sprintf("  Offline:%d", offline)
			}
			// Count vs expected (memql-cockpit#207): flag any node type
			// running fewer healthy replicas than expectedNodesPerType so
			// an understaffed tier is visible at a glance.
			if short := v.shortTypesLocked(); len(short) > 0 {
				statusText += "  short: " + strings.Join(short, ",")
			}
		}
		screen.FillRect(bounds.X, tallyY, bounds.Width, 1, statusStyle)
		screen.DrawText(bounds.X, tallyY, bounds.Width/2, statusText, statusStyle)

		// Right-align the pan/reset/arch hints on the same row.
		hints := "WASD:Pan  R:Reset  X:Architecture"
		if v.disconnected {
			hints += "  stale"
		}
		drawRightHints(screen, bounds, tallyY, hints, statusStyle)
	}

	// Full-width divider between the topology and deployments regions.
	if dividerY > bounds.Y && dividerY < hintY {
		for i := 0; i < bounds.Width; i++ {
			screen.SetCell(bounds.X+i, dividerY, '─', statusStyle)
		}
	}

	// DEPLOYMENTS region (Surface B): the per-cluster history list + the
	// selected deployment's detail block, rendered into its own sub-rect
	// so its internal bottom-anchored detail math stays scoped to the
	// band.
	if deployRegion.Height > 0 {
		v.drawDeployments(screen, deployRegion)
	}

	// Deployments hint bar on the pane's last row.
	screen.FillRect(bounds.X, hintY, bounds.Width, 1, statusStyle)
	left := fmt.Sprintf(" Deployments: %d", len(v.deployments))
	screen.DrawText(bounds.X, hintY, bounds.Width/2, left, statusStyle)
	drawRightHints(screen, bounds, hintY, v.hintsForDeployments(), statusStyle)

	// Cut/deploy/rollback concept modal overlays everything else in the
	// pane when open.
	if v.dctrl != nil {
		v.drawDeployConceptModal(screen, bounds)
	}
}

// drawTopology renders nodes as boxes with connection lines.
//
// Layout is computed in an unbounded "world" coordinate space (positions
// ignore the pane size), then every cell is shifted by (-panX, -panY) and
// clipped to the drawing area. That keeps the pan logic trivial and
// guarantees we never bleed into the clusters pane on the left.
func (v *View) drawTopology(screen *ui.Screen, bounds ui.Rect) {
	const boxW = 16
	// boxH is 5 so each box carries three content rows: type, health, and
	// a version + short-deploymentId line (the topology-truthfulness data
	// from memql-cockpit#207). Border rows top + bottom make 5.
	const boxH = 5

	// Group by type for hierarchical layout. Row order comes from the
	// seeded nodeType list (v.NodeTypes, fetched via
	// queryClusterNodeTypes); any type present on a node but missing
	// from that list gets appended at the end so we still render
	// unexpected nodes rather than silently dropping them.
	groups := map[string][]int{}
	for i, n := range v.Nodes {
		groups[n.Type] = append(groups[n.Type], i)
	}
	typeOrder := v.buildTypeOrder(groups)

	type nodePos struct {
		x, y int // top-left of box (world coords)
		cx   int // center x for connections
	}
	positions := make([]nodePos, len(v.Nodes))

	// Drawing area (below title, above status). Anything outside this
	// rectangle is clipped by plotCell/plotText below.
	area := ui.Rect{
		X:      bounds.X + 2,
		Y:      bounds.Y + 3,
		Width:  bounds.Width - 4,
		Height: bounds.Height - 5,
	}

	// Lay out every row starting from the same origin regardless of how
	// much of it fits -- pan determines what's visible, not the layout.
	rowY := area.Y

	for _, nodeType := range typeOrder {
		indices := groups[nodeType]
		if len(indices) == 0 {
			continue
		}

		totalW := len(indices)*boxW + (len(indices)-1)*3
		// Center within the pane when it fits; otherwise left-align so
		// the first nodes are visible at pan=0.
		startX := area.X
		if totalW < area.Width {
			startX = area.X + (area.Width-totalW)/2
		}

		for j, idx := range indices {
			x := startX + j*(boxW+3)
			positions[idx] = nodePos{x: x, y: rowY, cx: x + boxW/2}
		}

		rowY += boxH + 2 // Gap between rows.
	}

	// Draw connection lines first.
	for _, edge := range v.Edges {
		p1 := positions[edge[0]]
		p2 := positions[edge[1]]

		lineX := p1.cx
		lineStyle := tcell.StyleDefault.Foreground(v.Theme.Subtle).Background(v.Theme.BG)

		// Vertical line from bottom of parent to top of child.
		fromY := p1.y + boxH
		toY := p2.y

		for y := fromY; y < toY; y++ {
			ch := '│'
			if y == fromY {
				ch = '┬'
			}
			v.plotCell(screen, lineX, y, area, ch, lineStyle)
		}

		// If child is offset, draw horizontal + corner.
		if p2.cx != lineX {
			cornerY := toY - 1
			if cornerY <= fromY {
				cornerY = fromY + 1
			}

			minCX := lineX
			maxCX := p2.cx
			if minCX > maxCX {
				minCX, maxCX = maxCX, minCX
			}
			for x := minCX; x <= maxCX; x++ {
				v.plotCell(screen, x, cornerY, area, '─', lineStyle)
			}

			if p2.cx > lineX {
				v.plotCell(screen, lineX, cornerY, area, '├', lineStyle)
				v.plotCell(screen, p2.cx, cornerY, area, '┐', lineStyle)
			} else {
				v.plotCell(screen, lineX, cornerY, area, '┤', lineStyle)
				v.plotCell(screen, p2.cx, cornerY, area, '┌', lineStyle)
			}

			for y := cornerY + 1; y < toY; y++ {
				v.plotCell(screen, p2.cx, y, area, '│', lineStyle)
			}
			v.plotCell(screen, p2.cx, toY, area, '▼', lineStyle)
		} else if toY > fromY {
			v.plotCell(screen, lineX, toY-1, area, '▼', lineStyle)
		}
	}

	// Draw node boxes. Orphan/stale nodes (stopped, or carrying a
	// non-current deploymentId) render with the warning border + an
	// [orphan] tag so a half-finished rollout or leftover node is obvious.
	current := v.currentDeploymentIdLocked()
	for i, node := range v.Nodes {
		pos := positions[i]
		v.drawNodeBox(screen, pos.x, pos.y, boxW, boxH, area, node, v.isOrphanLocked(node, current))
	}
}

// drawRightHints renders a hint string right-aligned within bounds on
// row y, clamped so it never bleeds left of bounds.X. On the Clusters
// tab the topology pane sits to the right of a `│` divider; an
// over-long, naively right-aligned hint would start at a negative
// offset and paint over the divider + the management pane (breaking the
// panel-chrome contract, memql-cockpit#207). Clamping the start to
// bounds.X and the width to the pane keeps long hints clipping on the
// right inside the pane instead.
func drawRightHints(screen *ui.Screen, bounds ui.Rect, y int, hints string, style tcell.Style) {
	x := bounds.X + bounds.Width - len(hints) - 1
	width := len(hints)
	if x < bounds.X {
		x = bounds.X
		width = bounds.Width
	}
	screen.DrawText(x, y, width, hints, style)
}

// plotCell writes a cell in world coordinates, applying the pan offset
// and clipping to the drawing area. All topology drawing must go through
// this (or plotText) so nothing escapes the pane.
func (v *View) plotCell(screen *ui.Screen, worldX, worldY int, area ui.Rect, ch rune, style tcell.Style) {
	sx := worldX - v.panX
	sy := worldY - v.panY
	if sx < area.X || sx >= area.X+area.Width {
		return
	}
	if sy < area.Y || sy >= area.Y+area.Height {
		return
	}
	screen.SetCell(sx, sy, ch, style)
}

// plotText writes a string in world coordinates with clipping.
func (v *View) plotText(screen *ui.Screen, worldX, worldY int, area ui.Rect, text string, style tcell.Style) {
	for _, ch := range text {
		v.plotCell(screen, worldX, worldY, area, ch, style)
		worldX++
	}
}

// drawNodeBox renders a single node as a bordered box.
// Layout: line 1 = TYPE, line 2 = name, line 3 = status
// Coordinates are in world space -- plotCell/plotText apply pan + clip.
// When the view is disconnected, every box is forced to the OFFLINE
// visual treatment regardless of the node's last-known Health.
func (v *View) drawNodeBox(screen *ui.Screen, x, y, w, h int, area ui.Rect, node NodeInfo, orphan bool) {
	effectiveHealth := node.Health
	if v.disconnected {
		effectiveHealth = nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE
	}
	healthColor := nodeHealthColor(effectiveHealth)
	// An orphan/stale node overrides the health-derived border with the
	// warning color so it reads as "running but not part of the current
	// deployment" regardless of its own health.
	if orphan && !v.disconnected {
		healthColor = v.Theme.Warning
	}
	borderStyle := tcell.StyleDefault.Foreground(healthColor).Background(v.Theme.BG)
	typeStyle := tcell.StyleDefault.Foreground(v.Theme.FG).Background(v.Theme.BG).Bold(true)

	// Border.
	v.plotCell(screen, x, y, area, '┌', borderStyle)
	v.plotCell(screen, x+w-1, y, area, '┐', borderStyle)
	v.plotCell(screen, x, y+h-1, area, '└', borderStyle)
	v.plotCell(screen, x+w-1, y+h-1, area, '┘', borderStyle)
	for i := x + 1; i < x+w-1; i++ {
		v.plotCell(screen, i, y, area, '─', borderStyle)
		v.plotCell(screen, i, y+h-1, area, '─', borderStyle)
	}
	for i := y + 1; i < y+h-1; i++ {
		v.plotCell(screen, x, i, area, '│', borderStyle)
		v.plotCell(screen, x+w-1, i, area, '│', borderStyle)
	}

	// Line 1: node type (bold).
	typeLabel := nodeTypeShort(node.Type)
	v.plotText(screen, x+1, y+1, area, clipText(typeLabel, w-2), typeStyle)

	// Line 2: health status (colored).
	status := healthLabel(effectiveHealth)
	v.plotText(screen, x+1, y+2, area, clipText(status, w-2), tcell.StyleDefault.Foreground(healthColor).Background(v.Theme.BG))

	// Line 3: topology-truthfulness data (memql-cockpit#207). Orphans show
	// an [orphan] tag in the warning color; otherwise show the running
	// version + a short deploymentId so version/deployment drift is
	// visible per-node at a glance.
	subStyle := tcell.StyleDefault.Foreground(v.Theme.Subtle).Background(v.Theme.BG)
	var line3 string
	if orphan {
		line3 = "[orphan]"
		subStyle = tcell.StyleDefault.Foreground(v.Theme.Warning).Background(v.Theme.BG)
	} else {
		ver := strings.TrimSpace(node.Version)
		dep := shortID(node.DeploymentId)
		switch {
		case ver != "" && dep != "":
			line3 = ver + " " + dep
		case ver != "":
			line3 = ver
		case dep != "":
			line3 = dep
		}
	}
	if line3 != "" {
		v.plotText(screen, x+1, y+3, area, clipText(line3, w-2), subStyle)
	}
}

// HandleEvent processes keyboard input. The topology view is driven by
// gRPC event subscriptions -- there is no manual refresh key. WASD pans
// the diagram within the pane so the user can inspect off-screen nodes.
//
// Takes the write lock for the duration -- pan keys mutate v.panX /
// v.panY which Draw reads under the read lock.
func (v *View) HandleEvent(ev tcell.Event) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}
	// Architecture navigator: when active, it claims all keys --
	// including the 'X' toggle (so the user can press X again to
	// leave). When inactive, the live topology's WASD pan + 'X'
	// toggle apply.
	if v.Arch.Active() {
		return v.Arch.HandleEvent(ev)
	}
	// Cut/deploy/rollback concept modal takes precedence whenever it's
	// open. handleDeployConceptKeyLocked assumes v.mu held.
	if v.dctrl != nil {
		return v.handleDeployConceptKeyLocked(keyEv)
	}
	// Persistent split (memql-cockpit#221): the Deployments section is
	// always present, so its navigation + control keys are live without a
	// toggle. Arrow keys + Enter drive the deployment cursor / node load;
	// they're routed before the KeyRune guard so they reach the handler.
	switch keyEv.Key() {
	case tcell.KeyUp, tcell.KeyDown, tcell.KeyEnter:
		return v.handleDeploymentsKeyLocked(keyEv)
	}
	if keyEv.Key() != tcell.KeyRune {
		return false
	}
	switch keyEv.Rune() {
	case 'w', 'W':
		v.panY -= panStepY
		return true
	case 's', 'S':
		v.panY += panStepY
		return true
	case 'a', 'A':
		v.panX -= panStepX
		return true
	case 'd', 'D':
		v.panX += panStepX
		return true
	case 'r', 'R':
		v.panX, v.panY = 0, 0
		return true
	case 'x', 'X':
		v.Arch.Toggle()
		return true
	case 'c', 'C', 'g', 'G', 'b', 'B':
		// Deployments cut/deploy/rollback controls (role-gated inside the
		// handler). 'b'/'B' (rollback) doesn't collide with a pan key.
		return v.handleDeploymentsKeyLocked(keyEv)
	}
	return false
}

// clipText trims a string to fit within the given max width in cells.
func clipText(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	if len(s) <= maxW {
		return s
	}
	return s[:maxW]
}

// buildEdges creates connections between nodes.
// BFF connects to all other nodes in the mesh topology.
// edgeRelations is the static service-relationship map that drives
// topology connections. Each entry is (parent type) -> (child type).
// An edge appears in the diagram only when BOTH endpoints are
// actually present in v.Nodes -- so missing services (e.g. polyphon
// overlay not running) drop their edges silently. The list captures
// the live request flow through the local cluster, not every
// possible communication path.
var edgeRelations = [][2]string{
	// Entry point fronts the public-facing services.
	{"lb", "bff"},
	{"lb", "identity"},
	// BFF dispatches to workers + persistence layer.
	{"bff", "cognition"},
	{"bff", "agent"},
	{"bff", "planner"},
	{"bff", "voice"},
	{"bff", "database"},
	// Voice node uses the polyphon overlay services when they're up.
	{"voice", "livekit"},
	{"voice", "redis"},
}

// buildEdgesLocked rebuilds v.Edges from v.Nodes. Caller MUST hold
// v.mu (write). Renamed to *Locked to make the contract obvious;
// every caller in this file is inside a mutator that already holds
// the lock.
func (v *View) buildEdgesLocked() {
	v.Edges = nil
	// First index per type (multiple BFFs in the cluster: edges
	// fan out from the first one; the others render side-by-side
	// in the same row but don't double-up the line drawing).
	firstByType := make(map[string]int, len(v.Nodes))
	for i, n := range v.Nodes {
		if _, exists := firstByType[n.Type]; !exists {
			firstByType[n.Type] = i
		}
	}

	seen := make(map[[2]int]bool)
	for _, rel := range edgeRelations {
		parentIdx, hasParent := firstByType[rel[0]]
		childIdx, hasChild := firstByType[rel[1]]
		if !hasParent || !hasChild {
			continue // drop the edge if one side is missing
		}
		e := [2]int{parentIdx, childIdx}
		if !seen[e] {
			seen[e] = true
			v.Edges = append(v.Edges, e)
		}
	}

	// Real discovery edges from parentId (memql-cockpit#207): a node that
	// discovered the mesh through a peer carries that peer's id in
	// ParentId. Render those actual parent/child links on top of the
	// static service-relationship map so the diagram reflects the live
	// discovery graph. Indexed by node id; self-edges and dangling
	// parents (peer not in the current node set) are skipped.
	idToIdx := make(map[string]int, len(v.Nodes))
	for i, n := range v.Nodes {
		if n.ID != "" {
			idToIdx[n.ID] = i
		}
	}
	for childIdx, n := range v.Nodes {
		if n.ParentId == "" {
			continue
		}
		parentIdx, ok := idToIdx[n.ParentId]
		if !ok || parentIdx == childIdx {
			continue
		}
		e := [2]int{parentIdx, childIdx}
		if !seen[e] {
			seen[e] = true
			v.Edges = append(v.Edges, e)
		}
	}
}

// currentDeploymentIdLocked returns the deploymentId treated as the live
// "current" deployment for orphan detection. It prefers the newest
// succeeded / in-progress deployment from the history (newest-first), and
// falls back to the most common non-empty deploymentId across the live
// nodes. Returns "" when unknown -- in which case nothing is flagged as an
// orphan, so a cluster with no deployment metadata never shows false
// positives. Caller MUST hold v.mu.
func (v *View) currentDeploymentIdLocked() string {
	for _, d := range v.deployments {
		switch strings.ToLower(strings.TrimSpace(d.Status)) {
		case "succeeded", "in_progress":
			if d.ID != "" {
				return d.ID
			}
		}
	}
	counts := map[string]int{}
	best, bestN := "", 0
	for _, n := range v.Nodes {
		if n.DeploymentId == "" {
			continue
		}
		counts[n.DeploymentId]++
		if counts[n.DeploymentId] > bestN {
			best, bestN = n.DeploymentId, counts[n.DeploymentId]
		}
	}
	return best
}

// isOrphanLocked reports whether a node is orphaned/stale: it has stopped,
// or it carries a deploymentId that isn't the current one (when current is
// known). These are the nodes the topology highlights so a half-finished
// rollout or a leftover node from a superseded deployment is obvious.
// Caller MUST hold v.mu.
func (v *View) isOrphanLocked(n NodeInfo, current string) bool {
	if n.Health == nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED {
		return true
	}
	if current != "" && n.DeploymentId != "" && n.DeploymentId != current {
		return true
	}
	return false
}

// shortTypesLocked returns, in topology row order, the node types whose
// healthy-node count is below expectedNodesPerType, formatted as
// "type(got/exp)". Drives the "count vs expected" truthfulness summary in
// the status bar. Caller MUST hold v.mu.
func (v *View) shortTypesLocked() []string {
	healthyByType := map[string]int{}
	groups := map[string][]int{}
	for i, n := range v.Nodes {
		groups[n.Type] = append(groups[n.Type], i)
		if n.Health == nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY {
			healthyByType[n.Type]++
		}
	}
	var short []string
	for _, t := range v.buildTypeOrder(groups) {
		got := healthyByType[t]
		if got < expectedNodesPerType {
			short = append(short, fmt.Sprintf("%s(%d/%d)", nodeTypeShort(t), got, expectedNodesPerType))
		}
	}
	return short
}

func (v *View) healthCounts() (healthy, degraded, offline int) {
	for _, n := range v.Nodes {
		switch n.Health {
		case nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY:
			healthy++
		case nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED,
			nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING,
			nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING:
			degraded++
		case nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE,
			nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED:
			offline++
		default:
			degraded++
		}
	}
	return
}

// preferredTypeOrder is the tier ordering the topology renders
// top-to-bottom when types are present. Captures the request flow
// through a local cluster: LB at the top, services it fronts on the
// next row, workers BFF dispatches to, the data layer, then any
// external clients. Types not in this list fall through to the
// gRPC-seed and first-seen heuristics so unrecognized services
// still appear at the bottom rather than being dropped.
var preferredTypeOrder = []string{
	"lb",
	"bff",
	"identity",
	"cognition",
	"agent",
	"planner",
	"voice",
	"database",
	"redis",
	"livekit",
	"voice-agent",
}

// buildTypeOrder returns the row order for drawTopology. The static
// preferredTypeOrder list comes first so well-known services land in
// the expected tiers; NodeTypes (seeded from v1:cluster:nodeType)
// follows so unknown-to-the-cockpit registered types still render
// in seed order; v.Nodes fills the rest in first-seen insertion
// order so a brand-new docker service shows up without a Go change.
func (v *View) buildTypeOrder(groups map[string][]int) []string {
	seen := make(map[string]bool, len(preferredTypeOrder)+len(v.NodeTypes)+len(groups))
	order := make([]string, 0, len(preferredTypeOrder)+len(v.NodeTypes)+len(groups))

	for _, name := range preferredTypeOrder {
		if _, present := groups[name]; !present {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		order = append(order, name)
	}
	for _, t := range v.NodeTypes {
		name := strings.TrimSpace(t.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		order = append(order, name)
	}
	// Preserve insertion order from v.Nodes so unknown types render
	// deterministically (map iteration alone would flap between draws).
	for _, n := range v.Nodes {
		name := strings.TrimSpace(n.Type)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		order = append(order, name)
	}
	return order
}

// nodeTypeShort returns the uppercase rendering of the nodeType used
// on the first line of each topology box. Accepts any string so new
// node types (anything the server writes to v1:cluster:node.nodeType)
// show up without a Go change.
func nodeTypeShort(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return "NODE"
	}
	return strings.ToUpper(t)
}

// healthLabel formats the proto-defined NodeHealthStatus enum for display.
// The proto enum is the source of truth; keep this function as the only
// place that maps enum -> visible string.
func healthLabel(health nodev1.NodeHealthStatus) string {
	switch health {
	case nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING:
		return "connecting"
	case nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY:
		return "online"
	case nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED:
		return "degraded"
	case nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING:
		return "draining"
	case nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE:
		return "offline"
	case nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED:
		return "stopped"
	default:
		return "unknown"
	}
}

// nodeHealthColor picks a border color for the node box based on the enum.
func nodeHealthColor(health nodev1.NodeHealthStatus) tcell.Color {
	switch health {
	case nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY:
		return tcell.NewRGBColor(80, 200, 100) // green
	case nodev1.NodeHealthStatus_NODE_HEALTH_CONNECTING:
		return tcell.NewRGBColor(180, 180, 60) // dim yellow
	case nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED,
		nodev1.NodeHealthStatus_NODE_HEALTH_DRAINING:
		return tcell.NewRGBColor(220, 180, 60) // amber
	case nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE,
		nodev1.NodeHealthStatus_NODE_HEALTH_STOPPED:
		return tcell.NewRGBColor(220, 60, 60) // red
	default:
		return tcell.NewRGBColor(100, 100, 120) // gray
	}
}
