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

// deployEnvs is the fixed set of environments the deployment-v2 status
// strip renders, top-to-bottom. #144 is read-only across both; a future
// story (#145) may add a selector / per-env controls.
var deployEnvs = []string{"staging", "prod"}

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

	// deployStatus is the last-known deployment-v2 status keyed by env
	// ("staging" / "prod"). Populated by SetDeploymentStatus from the
	// per-env poll (see app.go refreshDeployStatus); read under RLock
	// in Draw. A missing key renders as "no data yet" for that env.
	deployStatus map[string]client.DeploymentStatus

	// clusterRole is the connected caller's cluster-wide role, mirrored
	// from refreshMyAccess via SetClusterRole. The deployment strip is
	// only rendered for owner/admin -- the GetDeploymentStatus RPC is
	// owner/admin-gated server-side, so a non-admin caller would just
	// get PermissionDenied. Lower roles see a single muted line instead.
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

	// OnActionComplete, when set, is called after a deploy-control
	// action succeeds. Wired to App.refreshDeployStatus in app.go so
	// the Argo/Rollouts overlay reflects the new state right away
	// rather than on the next 10s poll tick. nil is a no-op.
	OnActionComplete func()

	// deployActor is the action executor the deploy-control modal fires
	// against. In production it is left nil and the modal resolves it
	// lazily from DeployClient() (so a cluster switch retargets); tests
	// inject a fake to drive an action to SUCCESS / ERROR without a wire.
	deployActor deployActions

	// ctrl holds the transient deploy-control modal state. nil when the
	// modal is closed. Guarded by v.mu like every other view field.
	ctrl *deployControlState
}

// SetDeployActor overrides the deploy-control action executor. Used by
// tests to inject a fake DeployControlClient; production leaves it nil
// and the modal resolves the real client from DeployClient() per-fire.
func (v *View) SetDeployActor(a deployActions) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deployActor = a
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

// SetDeploymentStatus stores the latest deployment-v2 status for env
// ("staging" / "prod"). Called from the per-env poll goroutine (see
// app.go refreshDeployStatus). Guarded by the same mu that serializes
// Draw, so a render can never see a torn map.
func (v *View) SetDeploymentStatus(env string, st client.DeploymentStatus) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.deployStatus == nil {
		v.deployStatus = make(map[string]client.DeploymentStatus, len(deployEnvs))
	}
	v.deployStatus[env] = st
}

// SetClusterRole mirrors the connected caller's cluster-wide role into
// the view. Wired from refreshMyAccess alongside the Settings update.
// Drives whether the deployment strip renders (owner/admin) or shows
// the "owner/admin only" muted line (everyone else).
func (v *View) SetClusterRole(r client.Role) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.clusterRole = r
}

// canViewDeploymentsLocked reports whether the caller's role admits the
// deployment-v2 surface. Caller MUST hold v.mu (read or write).
func (v *View) canViewDeploymentsLocked() bool {
	return v.clusterRole == client.RoleOwner || v.clusterRole == client.RoleAdmin
}

// CanViewDeployments reports whether the connected caller's role admits
// the deployment-v2 surface (owner/admin). Used by the app's poll loop
// to avoid issuing the owner/admin-gated GetDeploymentStatus RPC for a
// caller that would just get PermissionDenied. Takes the read lock.
func (v *View) CanViewDeployments() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.canViewDeploymentsLocked()
}

// DeployEnvs returns the fixed set of environments the deployment-v2
// status strip renders. Exposed so the app's poll loop iterates the
// same list the renderer does.
func DeployEnvs() []string {
	out := make([]string, len(deployEnvs))
	copy(out, deployEnvs)
	return out
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

	if len(v.Nodes) > 0 {
		v.drawTopology(screen, bounds)
	}

	// Deployment-v2 status strip. Painted in a fixed band anchored
	// just above the status bar so it never overlaps the topology's
	// own status row. Stays inside the RLock frame.
	v.drawDeployStrip(screen, bounds)

	// Status bar at bottom.
	statusY := bounds.Y + bounds.Height - 1
	statusStyle := tcell.StyleDefault.Foreground(v.Theme.Subtle).Background(v.Theme.BG)
	screen.FillRect(bounds.X, statusY, bounds.Width, 1, statusStyle)

	// Left side of the status row. When there are no nodes yet, the
	// "Nodes:N Online:..." tally has nothing to say, so the
	// empty-state hint ("No nodes...") lives here instead -- on the
	// same row as the WASD/R:Reset hints on the right, which is what
	// makes the two halves visually balance.
	statusText := ""
	if len(v.Nodes) == 0 {
		statusText = " No nodes. Waiting for cluster data..."
	} else if v.disconnected {
		// Force the offline tally to match the override rendering.
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
	}
	screen.DrawText(bounds.X, statusY, bounds.Width/2, statusText, statusStyle)

	// Right-align: pan/reset hints. The "live" indicator was redundant
	// (a healthy stream is the implicit default); only the "stale"
	// warning surfaces, and only when we've actually lost the stream.
	hints := "WASD:Pan  R:Reset View  X:Architecture"
	// Deploy-control menu key is owner/admin-only; only advertise it
	// when the caller can actually use it (chrome contract: hints that
	// lie rot trust).
	if v.canViewDeploymentsLocked() {
		hints += "  D:Deploy"
	}
	if v.disconnected {
		hints += "  stale"
	}
	screen.DrawText(bounds.X+bounds.Width-len(hints)-1, statusY, len(hints), hints, statusStyle)

	// Deploy-control modal overlays everything else in the pane when open.
	if v.ctrl != nil {
		v.drawDeployModal(screen, bounds)
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
	const boxH = 4

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

	// Draw node boxes.
	for i, node := range v.Nodes {
		pos := positions[i]
		v.drawNodeBox(screen, pos.x, pos.y, boxW, boxH, area, node)
	}
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
func (v *View) drawNodeBox(screen *ui.Screen, x, y, w, h int, area ui.Rect, node NodeInfo) {
	effectiveHealth := node.Health
	if v.disconnected {
		effectiveHealth = nodev1.NodeHealthStatus_NODE_HEALTH_OFFLINE
	}
	healthColor := nodeHealthColor(effectiveHealth)
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
	// Deploy-control modal takes key precedence whenever it's open
	// (mirrors the workers grant/approval modal). handleDeployKey
	// assumes v.mu is already held (we took the write lock above).
	if v.ctrl != nil {
		return v.handleDeployKeyLocked(keyEv)
	}
	if keyEv.Key() != tcell.KeyRune {
		return false
	}
	// 'D' opens the deploy-control menu -- owner/admin only. Non-admins
	// get a no-op (no controls, no hint). Checked before the pan keys so
	// it isn't shadowed.
	if r := keyEv.Rune(); r == 'D' {
		if v.canViewDeploymentsLocked() {
			v.openDeployModalLocked()
			return true
		}
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

	for _, rel := range edgeRelations {
		parentIdx, hasParent := firstByType[rel[0]]
		childIdx, hasChild := firstByType[rel[1]]
		if !hasParent || !hasChild {
			continue // drop the edge if one side is missing
		}
		v.Edges = append(v.Edges, [2]int{parentIdx, childIdx})
	}
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
