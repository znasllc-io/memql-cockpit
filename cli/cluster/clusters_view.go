package cluster

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/config"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// ClusterStatus describes the connection state of a known cluster.
type ClusterStatus struct {
	Config  config.ClusterConfig
	Status  string // "connected", "connecting", "unreachable", "unknown"
	NodeId  string
	NodeVer string
}

// FocusPane identifies which pane has focus in the Clusters tab.
type FocusPane int

const (
	FocusManagement FocusPane = 0
	FocusPartitions FocusPane = 1
	FocusTopology   FocusPane = 2
)

// ClustersView is the top-level Clusters tab: left = management, right = topology.
//
// Connection lifecycle state lives outside this view (in the app's
// pool). ClustersView only renders: per-row status decorated via
// ClusterStatus, "selected" marker via SelectedCluster, retry/backoff
// details pulled via OnEntryState. No global Busy modal; all keys
// remain responsive while dials happen in the background.
type ClustersView struct {
	Theme      ui.Theme
	Focus      FocusPane
	Topology   *View           // node topology diagram (right pane)
	Partitions *PartitionsView // partition manager (bottom-left pane)

	// Cluster management state.
	Clusters     []ClusterStatus
	Selected     int // index of the currently-highlighted row (arrow keys)
	scrollOffset int // first visible row index inside the scrollable viewport

	// SelectedCluster is the name of the cluster the user has chosen
	// as their "working cluster" (via Enter). It's a separate concept
	// from Selected (list row index) and from the row's Status.
	// Marked with a ★ glyph in the list.
	SelectedCluster string

	// ActiveCluster mirrors SelectedCluster historically. Kept for
	// backward compat with code that still reads it (e.g. name bold).
	// Prefer SelectedCluster for new code.
	ActiveCluster string
	Connected     bool

	// Add-form state.
	addForm     addFormState
	showAddForm bool

	// Delete confirmation.
	confirmDelete bool

	// Callbacks — set by the app layer.
	OnAdd       func(c config.ClusterConfig) // Add a new cluster
	OnSave      func(c config.ClusterConfig) // Save edits to an existing cluster
	OnDelete    func(clusterName string)     // Delete a cluster
	OnEnter     func(clusterName string)     // Enter on a row -- user picks their "working cluster"
	OnHighlight func(clusterName string)     // Arrow keys moved highlight -- topology view should follow
	OnCancel    func(clusterName string)     // Esc on a row -- cancel its retry cycle if any
	OnRetry     func(clusterName string)     // R on a row -- manually retry after the 3-attempt cycle failed
	// OnEntryState returns the pool-entry state for rendering row
	// details (retry attempt, next-try countdown). Returns false when
	// no entry exists yet. Called only during Draw, from the UI thread.
	OnEntryState func(clusterName string) (state string, attempt int, nextTryAt string, ok bool)
}

// formFieldCount is the number of editable rows in the add/edit form.
const formFieldCount = 5

// Field indices for the add/edit form. Keep this consistent with the
// `labels` / `placeholders` arrays in drawAddForm.
const (
	formFieldName = iota
	formFieldHost
	formFieldPort
	formFieldIssuer
	formFieldClientId
)

type addFormState struct {
	fields   [formFieldCount]string
	cursor   int
	editMode bool   // true when re-opening the form for an existing cluster
	editName string // original cluster name when editMode is true
	// formError is set by Enter-handling when validation fails. Cleared
	// on the next keystroke so the hint disappears once the user
	// acknowledges it and continues editing.
	formError string
}

// defaultLocalClusterConfig is the seed used when no user override exists
// in clusters.yaml. The user can edit "local" through the Edit form; the
// override is persisted and takes precedence on load.
//
// The preset has NO auth shortcut. Local-dev runs the identity
// service via docker compose and uses the same OAuth flow as any
// other cluster -- the user runs `memql-cockpit authorize
// http://localhost:8081` (or wherever identity is reachable) to
// wire issuer + client_id, then `login local` to mint a token.
var defaultLocalClusterConfig = config.ClusterConfig{
	Name:     "local",
	Endpoint: "localhost:50052",
}

// LocalClusterConfig returns the active local-cluster default. Exposed as
// a function so tests can swap the underlying value.
func LocalClusterConfig() config.ClusterConfig {
	return defaultLocalClusterConfig
}

// splitEndpoint parses a "host:port" string into its two parts, falling
// back gracefully when the input is incomplete. Empty port is allowed
// (validated on save).
func splitEndpoint(endpoint string) (host, port string) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", ""
	}
	// Use LastIndex so IPv6 literals with brackets (future-proofing) work.
	idx := strings.LastIndex(endpoint, ":")
	if idx < 0 {
		return endpoint, ""
	}
	return endpoint[:idx], endpoint[idx+1:]
}

// joinEndpoint combines host + port into the single "host:port" form
// stored on ClusterConfig. The connection layer still receives one
// string; splitting is a UI concern only.
func joinEndpoint(host, port string) string {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" && port == "" {
		return ""
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// formStateFromConfig builds an addFormState pre-populated from an
// existing ClusterConfig. Used when opening the form in edit mode.
func formStateFromConfig(c config.ClusterConfig) addFormState {
	host, port := splitEndpoint(c.Endpoint)
	return addFormState{
		fields: [formFieldCount]string{
			formFieldName:     c.Name,
			formFieldHost:     host,
			formFieldPort:     port,
			formFieldIssuer:   c.Issuer,
			formFieldClientId: c.ClientId,
		},
		cursor:   formFieldHost, // jump straight to the most-edited field
		editMode: true,
		editName: c.Name,
	}
}

// NewClustersView creates the combined Clusters tab.
// The "local" cluster is always present and cannot be deleted.
func NewClustersView(theme ui.Theme) *ClustersView {
	v := &ClustersView{
		Theme:      theme,
		Topology:   NewView(theme),
		Partitions: NewPartitionsView(theme),
		Focus:      FocusManagement,
	}
	// Ensure local cluster is always in the list.
	v.Clusters = []ClusterStatus{{
		Config: LocalClusterConfig(),
		Status: "unknown",
	}}
	return v
}

// SetClusters updates the cluster list, ensuring "local" is always first.
// Preserves "connecting" status for any cluster currently being connected to.
func (v *ClustersView) SetClusters(clusters []ClusterStatus) {
	// Build a map of current statuses to preserve.
	oldStatus := make(map[string]ClusterStatus)
	for _, c := range v.Clusters {
		oldStatus[c.Config.Name] = c
	}

	// Start with local, then add user clusters. If the on-disk config
	// file overrides "local" (see OnSave) that entry will come through
	// the loop below and replace result[0].Config.
	result := []ClusterStatus{{
		Config: LocalClusterConfig(),
		Status: "unknown",
	}}

	// Preserve local's current status.
	if old, ok := oldStatus["local"]; ok {
		result[0].Status = old.Status
		result[0].NodeId = old.NodeId
		result[0].NodeVer = old.NodeVer
	}

	// Add non-local clusters from config file.
	for _, c := range clusters {
		if c.Config.Name == "local" {
			// Persisted override for local -- copy the config (endpoint,
			// auth settings) and any non-unknown status/node info.
			result[0].Config = c.Config
			if c.Status != "unknown" {
				result[0].Status = c.Status
				result[0].NodeId = c.NodeId
				result[0].NodeVer = c.NodeVer
			}
			continue
		}
		// Preserve "connecting" status across refreshes (the pool
		// lifecycle owns the transitions; SetClusters shouldn't
		// rewrite a row that's mid-attempt).
		if old, ok := oldStatus[c.Config.Name]; ok && old.Status == "connecting" {
			c.Status = "connecting"
		}
		result = append(result, c)
	}

	// Sort the non-local tail alphabetically so adding a new
	// cluster doesn't shove rows around arbitrarily. "local"
	// stays pinned at index 0 regardless of sort order.
	if len(result) > 1 {
		sort.Slice(result[1:], func(i, j int) bool {
			return result[1+i].Config.Name < result[1+j].Config.Name
		})
	}

	v.Clusters = result
}

// fireOnHighlight invokes the OnHighlight callback with the currently
// highlighted cluster name. Safe to call even when OnHighlight isn't
// wired.
func (v *ClustersView) fireOnHighlight() {
	if v.OnHighlight == nil || v.Selected < 0 || v.Selected >= len(v.Clusters) {
		return
	}
	v.OnHighlight(v.Clusters[v.Selected].Config.Name)
}

// SetConnected marks a cluster as connected / unreachable in the row
// list. Called by the app layer from a pool entry's lifecycle.
func (v *ClustersView) SetConnected(name string, connected bool, nodeId, nodeVer string) {
	v.ActiveCluster = name
	v.Connected = connected
	for i := range v.Clusters {
		if v.Clusters[i].Config.Name == name {
			if connected {
				v.Clusters[i].Status = "connected"
				v.Clusters[i].NodeId = nodeId
				v.Clusters[i].NodeVer = nodeVer
			} else {
				v.Clusters[i].Status = "unreachable"
			}
		}
	}
}

// SetRowStatus updates a single cluster's row status in the list.
// Used by the pool lifecycle to reflect connecting / backoff / failed
// transitions without touching NodeId / NodeVer.
func (v *ClustersView) SetRowStatus(name, status string) {
	for i := range v.Clusters {
		if v.Clusters[i].Config.Name == name {
			v.Clusters[i].Status = status
		}
	}
}

// Draw renders the Clusters tab.
func (v *ClustersView) Draw(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.30, MinSize: 30},
		{Flex: 0.70, MinSize: 30},
	})

	leftBounds := panes[0]
	rightBounds := panes[1]

	// Divider.
	divX := leftBounds.X + leftBounds.Width
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		screen.SetCell(divX-1, y, '│', v.Theme.SubtleStyle())
	}
	leftBounds.Width--

	// Split the left column vertically when the form isn't open --
	// top half is the cluster manager, bottom half is the partition
	// manager. The form takes the full pane to keep the editor wide
	// enough to type into.
	if v.showAddForm || v.Partitions == nil {
		v.drawManagement(screen, leftBounds)
	} else {
		topRows := leftBounds.Height / 2
		if topRows < 8 {
			topRows = leftBounds.Height
		}
		topBounds := ui.Rect{X: leftBounds.X, Y: leftBounds.Y, Width: leftBounds.Width, Height: topRows}
		botBounds := ui.Rect{X: leftBounds.X, Y: leftBounds.Y + topRows, Width: leftBounds.Width, Height: leftBounds.Height - topRows}
		v.drawManagement(screen, topBounds)
		// Horizontal divider between the two halves.
		screen.DrawHLine(botBounds.X, botBounds.Y, botBounds.Width-1, '─', v.Theme.SubtleStyle())
		partBounds := ui.Rect{X: botBounds.X, Y: botBounds.Y + 1, Width: botBounds.Width, Height: botBounds.Height - 1}
		v.Partitions.Focused = v.Focus == FocusPartitions
		v.Partitions.Draw(screen, partBounds)
	}

	// Topology pane.
	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusTopology {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(rightBounds.X+1, rightBounds.Y, rightBounds.Width-2, " TOPOLOGY ", titleStyle)
	topoBounds := ui.Rect{X: rightBounds.X, Y: rightBounds.Y + 1, Width: rightBounds.Width, Height: rightBounds.Height - 1}
	v.Topology.Draw(screen, topoBounds)
}

// drawManagement renders the cluster management left pane.
func (v *ClustersView) drawManagement(screen *ui.Screen, bounds ui.Rect) {
	y := bounds.Y
	x := bounds.X + 1
	maxW := bounds.Width - 2

	// Pane-level title (focus-aware blue/gray).
	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusManagement {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	// Title reflects the pane's current mode. Entering add/edit
	// mode rewrites the title instead of stacking a second one
	// inside the form -- one status strip per pane, no duplicates.
	title := " CLUSTERS "
	if v.showAddForm {
		if v.addForm.editMode {
			title = " EDIT CLUSTER "
		} else {
			title = " ADD CLUSTER "
		}
	}
	screen.DrawText(x, y, maxW, title, titleStyle)
	y++

	if v.showAddForm {
		v.drawAddForm(screen, x, y, maxW, bounds)
		return
	}
	// Pane title (CLUSTERS) above is the only header -- the previous
	// "CLUSTER MANAGER" subtitle was redundant noise.
	//
	// Layout below the title is a three-band stack, bottom-up:
	//
	//   [chrome]     -- hints or delete-confirmation prompt (chromeH)
	//   [gap]        -- 1 blank row so chrome always has breathing room
	//   [detail]     -- selected row's endpoint/auth/status/retry/node
	//                   (detailMaxH rows; empty slots stay blank)
	//   [list]       -- scrollable cluster list (whatever remains)
	//
	// Fixed height is reserved for gap + detail + chrome so the list
	// can't grow into them when the cluster count balloons. When the
	// list exceeds its viewport we scroll + draw an accent-colored
	// indicator via ui.DrawScrollbar.
	const detailMaxH = 5    // divider + Endpoint + Auth + Status + (retry|Node)
	const chromeGapH = 1    // clear row above the action hints
	chromeH := 1
	if v.confirmDelete && v.Selected >= 0 && v.Selected < len(v.Clusters) && v.Clusters[v.Selected].Config.Name != "local" {
		chromeH = 2
	}
	// Reserve a blank row between the list and the detail block so
	// the two read as distinct zones.
	viewportTop := y + 1
	viewportH := bounds.Y + bounds.Height - viewportTop - detailMaxH - chromeGapH - chromeH
	if viewportH < 1 {
		viewportH = 1
	}

	// "local" is pinned at index 0 as a sticky header when there's at
	// least one other cluster; a subtle ── divider separates it from
	// the sorted rest so the list visually groups "the always-there
	// fallback" apart from "whatever you've added". Scrolling only
	// affects the rest; the pinned row + divider stay put.
	hasPinned := len(v.Clusters) > 0 && v.Clusters[0].Config.Name == "local"
	hasRest := len(v.Clusters) > 1
	headerH := 0
	if hasPinned && hasRest {
		headerH = 2
	}
	scrollH := viewportH - headerH
	if scrollH < 1 {
		scrollH = 1
	}

	// Scroll math is on "rest" (non-pinned) indices when we have a
	// sticky header; otherwise it's on the full list.
	restTotal := len(v.Clusters)
	dataOffset := 0 // index shift from scroll-index to data-index
	if headerH > 0 {
		restTotal = len(v.Clusters) - 1
		dataOffset = 1
	}
	restSelected := v.Selected - dataOffset
	if restSelected < 0 {
		// Selected sits on the pinned row -- leave the scroll where it is
		// but keep it clamped to the valid range.
		v.scrollOffset = ui.ScrollTo(v.scrollOffset, 0, restTotal, scrollH)
	} else {
		v.scrollOffset = ui.ScrollTo(v.scrollOffset, restSelected, restTotal, scrollH)
	}

	drawClusterRow := func(cs ClusterStatus, dataIdx int, rowY int) {
		rowStyle := v.Theme.BaseStyle()
		if dataIdx == v.Selected {
			rowStyle = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, rowY, bounds.Width, 1, rowStyle)

		if dataIdx == v.Selected {
			screen.SetCell(x, rowY, '▸', rowStyle.Foreground(v.Theme.Accent))
		}
		icon, color := clusterStatusIcon(cs.Status, v.Theme)
		screen.SetCell(x+2, rowY, icon, rowStyle.Foreground(color))

		nameStyle := rowStyle
		if cs.Config.Name == v.SelectedCluster {
			nameStyle = rowStyle.Foreground(v.Theme.Accent).Bold(true)
		}
		screen.DrawText(x+4, rowY, maxW-5, cs.Config.Name, nameStyle)

		if cs.Config.Name == v.SelectedCluster {
			markerX := bounds.X + bounds.Width - 2
			screen.SetCell(markerX, rowY, '◆', rowStyle.Foreground(v.Theme.Accent).Bold(true))
		}
	}

	// Sticky header: pinned row + divider stay at the top of the
	// viewport regardless of scroll. Only drawn when there are
	// non-pinned items to separate from.
	if headerH > 0 {
		drawClusterRow(v.Clusters[0], 0, viewportTop)
		screen.DrawHLine(bounds.X+1, viewportTop+1, bounds.Width-2, '─', v.Theme.SubtleStyle())
	}

	// Scrollable rest.
	restStart, restEnd := ui.VisibleRange(v.scrollOffset, restTotal, scrollH)
	for j := restStart; j < restEnd; j++ {
		dataIdx := j + dataOffset
		rowY := viewportTop + headerH + (j - restStart)
		drawClusterRow(v.Clusters[dataIdx], dataIdx, rowY)
	}

	// If no sticky header but only pinned-or-empty, still render the
	// single-cluster case via the scrollable path (restTotal captures
	// that already; the loop above covers it).

	// Accent-colored scrollbar scoped to the scrollable region.
	if headerH > 0 || !hasPinned {
		ui.DrawScrollbar(screen, v.Theme,
			ui.Rect{X: bounds.X, Y: viewportTop + headerH, Width: bounds.Width, Height: scrollH},
			v.scrollOffset, restTotal,
		)
	}

	// Detail block for the selected cluster, pinned to the rows just
	// above the chrome gap. Rows that have nothing to show stay blank
	// -- keeps the list viewport height stable as the user arrows
	// around.
	detailTop := bounds.Y + bounds.Height - chromeH - chromeGapH - detailMaxH
	if v.Selected >= 0 && v.Selected < len(v.Clusters) {
		cs := v.Clusters[v.Selected]
		detailStyle := v.Theme.BaseStyle()
		subtleDetail := v.Theme.SubtleStyle()

		dy := detailTop
		screen.DrawHLine(x, dy, min(maxW, 28), '─', v.Theme.SubtleStyle())
		dy++

		screen.DrawText(x+1, dy, 10, "Endpoint", subtleDetail)
		screen.DrawText(x+12, dy, maxW-13, cs.Config.Endpoint, detailStyle)
		dy++

		authStr := "OIDC"
		if cs.Config.PAT != "" {
			authStr = "PAT"
		}
		screen.DrawText(x+1, dy, 10, "Auth", subtleDetail)
		screen.DrawText(x+12, dy, maxW-13, authStr, detailStyle)
		dy++

		screen.DrawText(x+1, dy, 10, "Status", subtleDetail)
		_, sColor := clusterStatusIcon(cs.Status, v.Theme)
		screen.DrawText(x+12, dy, maxW-13, cs.Status, detailStyle.Foreground(sColor))
		dy++

		// Retry progress takes priority over the Node row when both
		// would fit -- lifecycle state is more time-sensitive than the
		// static node id. Node still rendered when there's no retry.
		shown := false
		if v.OnEntryState != nil {
			if state, attempt, nextTryAt, ok := v.OnEntryState(cs.Config.Name); ok {
				switch state {
				case "connecting":
					screen.DrawText(x+1, dy, maxW-1, fmt.Sprintf("Attempt %d/3", attempt), subtleDetail)
					shown = true
				case "backoff":
					if nextTryAt != "" {
						screen.DrawText(x+1, dy, maxW-1, fmt.Sprintf("Retry %d/3 in %s", attempt+1, nextTryAt), subtleDetail)
					} else {
						screen.DrawText(x+1, dy, maxW-1, fmt.Sprintf("Retry %d/3", attempt+1), subtleDetail)
					}
					shown = true
				}
			}
		}
		if !shown && cs.NodeId != "" {
			screen.DrawText(x+1, dy, 10, "Node", subtleDetail)
			screen.DrawText(x+12, dy, maxW-13, cs.NodeId, detailStyle)
		}
	}

	// Bottom section -- anchored to the pane floor via
	// ui.DrawBottomBlocks, which wraps long lines and stacks colored
	// sections without manual row math. The universal "Tab:Switch
	// panes" hint lives in the top-of-screen header chrome
	// (cli/ui/header.go), so we don't repeat it here.
	//
	// Action hints are context-aware. Enter:Select only shows when:
	//   - the row is actually CONNECTED (you can't make an
	//     unreachable or still-dialing cluster the working one), AND
	//   - the row isn't ALREADY the working cluster (Enter on the
	//     selected row is a no-op, so don't advertise it).
	// Connection-state actions take its place in non-connected
	// states:
	//   Esc:Cancel -- mid-connect (connecting | backoff)
	//   R:Retry    -- terminal failure (failed)
	//
	// Same convention used by PartitionsView: Enter:Select only
	// surfaces for selectable, not-already-active rows. Future
	// list-bearing panes should follow this pattern.
	subtle := v.Theme.SubtleStyle()
	warnStyle := tcell.StyleDefault.Foreground(v.Theme.Warning).Background(v.Theme.BG)
	canDelete := v.Selected >= 0 && v.Selected < len(v.Clusters) && v.Clusters[v.Selected].Config.Name != "local"
	hint := "A:Add  E:Edit"
	if v.OnEntryState != nil && v.Selected >= 0 && v.Selected < len(v.Clusters) {
		name := v.Clusters[v.Selected].Config.Name
		alreadySelected := name == v.SelectedCluster
		if state, _, _, ok := v.OnEntryState(name); ok {
			switch state {
			case "connected":
				if !alreadySelected {
					hint += "  Enter:Select"
				}
			case "connecting", "backoff":
				hint += "  Esc:Cancel"
			case "failed":
				hint += "  R:Retry"
			}
		}
		// No state at all (no pool entry yet, e.g. for a row added
		// in the current frame): don't claim Enter does anything.
	}
	if canDelete {
		hint += "  D:Del"
	}

	if v.confirmDelete && v.Selected >= 0 && v.Selected < len(v.Clusters) {
		name := v.Clusters[v.Selected].Config.Name
		ui.DrawBottomBlocks(screen, bounds, 1,
			ui.BottomBlock{Lines: []string{fmt.Sprintf("Delete %q?", name)}, Style: warnStyle},
			ui.BottomBlock{Lines: []string{"Y:Confirm  Esc:Cancel"}, Style: subtle},
		)
		return
	}
	ui.DrawBottom(screen, bounds, subtle, 1, hint)
}

func (v *ClustersView) drawAddForm(screen *ui.Screen, x, y, maxW int, bounds ui.Rect) {
	// Pane title already reads 'ADD CLUSTER' / 'EDIT CLUSTER' in
	// drawManagement, so no inline title here -- avoid stacking two
	// copies of the same label. One blank line of breathing room
	// before the first field label.
	_ = bounds // retained for future width-dependent layout decisions
	y++

	// Keep these arrays indexed by formField* constants so a reorder in
	// one place stays consistent. Host + Port are split for clarity --
	// the connection layer sees them joined on save.
	labels := [formFieldCount]string{
		formFieldName:     "Name",
		formFieldHost:     "Host",
		formFieldPort:     "Port",
		formFieldIssuer:   "Issuer",
		formFieldClientId: "Client ID",
	}
	placeholders := [formFieldCount]string{
		formFieldName:     "my-cluster",
		formFieldHost:     "localhost",
		formFieldPort:     "50052",
		formFieldIssuer:   "(optional)",
		formFieldClientId: "(optional)",
	}

	for i := 0; i < formFieldCount; i++ {
		// Name is immutable in edit mode -- changing it would orphan any
		// saved token and break the active-cluster lookup.
		if v.addForm.editMode && i == formFieldName {
			screen.DrawText(x+1, y, 10, labels[i], v.Theme.SubtleStyle())
			screen.DrawText(x+12, y, maxW-13, v.addForm.fields[i], v.Theme.SubtleStyle())
			y += 2
			continue
		}
		labelStyle := v.Theme.BaseStyle()
		if i == v.addForm.cursor {
			labelStyle = v.Theme.AccentStyle()
		}
		screen.DrawText(x+1, y, 10, labels[i], labelStyle)

		fieldX := x + 12
		fieldW := min(maxW-13, 25)
		fieldBG := tcell.NewRGBColor(35, 38, 45)
		if i == v.addForm.cursor {
			fieldBG = tcell.NewRGBColor(50, 55, 65)
		}
		fieldStyle := tcell.StyleDefault.Foreground(v.Theme.FG).Background(fieldBG)
		screen.FillRect(fieldX, y, fieldW, 1, fieldStyle)

		text := v.addForm.fields[i]
		if text == "" && i != v.addForm.cursor {
			screen.DrawText(fieldX+1, y, fieldW-2, placeholders[i], fieldStyle.Foreground(v.Theme.Subtle))
		} else {
			screen.DrawText(fieldX+1, y, fieldW-2, text, fieldStyle)
			if i == v.addForm.cursor {
				cursorX := fieldX + 1 + len(text)
				if cursorX < fieldX+fieldW-1 {
					screen.SetCell(cursorX, y, ' ', tcell.StyleDefault.Background(v.Theme.FG))
				}
			}
		}
		y += 2
	}

	// Inline validation error (if any). Shown just above the hint
	// block so the user sees which field failed without hunting.
	// Wrapped via ui.WrapText so a long message doesn't truncate in
	// the narrow left column.
	if v.addForm.formError != "" {
		errStyle := tcell.StyleDefault.Foreground(v.Theme.Error).Background(v.Theme.BG)
		for _, ln := range ui.WrapText(v.addForm.formError, maxW) {
			screen.DrawText(x, y, maxW, ln, errStyle)
			y++
		}
		y++
	}

	// Hints split across two rows so the narrow left-pane width
	// doesn't truncate "Esc:Cancel". Primary editing keys on top,
	// save/exit on the bottom.
	subtle := v.Theme.SubtleStyle()
	screen.DrawText(x, y, maxW, "↑/↓:Next Field", subtle)
	y++
	screen.DrawText(x, y, maxW, "Enter:Save       Esc:Cancel", subtle)
}

// HandleEvent routes events based on focus and state.
// Per pool-based design: there is no global "busy" modal. Every key
// remains responsive at all times -- a row that's mid-retry shows
// that state via its icon, and Esc on such a row asks the lifecycle
// to cancel. Arrow keys, Tab, Ctrl+1..4 all keep working regardless
// of what any cluster is doing in the background.
// FormOpen reports whether this pane (cluster manager OR its nested
// partition manager) has a form active. Exposed so higher-level
// views (or the app shell) can short-circuit any Tab-like behavior
// that would pull focus out from under the user mid-edit.
func (v *ClustersView) FormOpen() bool {
	if v == nil {
		return false
	}
	if v.showAddForm {
		return true
	}
	if v.Partitions != nil && v.Partitions.FormOpen() {
		return true
	}
	return false
}

func (v *ClustersView) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// Tab switches focus between panes EXCEPT when a form is open on
	// ANY sub-pane. Typing inside a form and accidentally jumping to
	// Topology is confusing; the form owner always keeps Tab until
	// the form closes. Esc dismisses the form and re-enables pane
	// cycling. Cycle order: Management → Partitions → Topology.
	if keyEv.Key() == tcell.KeyTab {
		if v.showAddForm || (v.Partitions != nil && v.Partitions.FormOpen()) {
			return true // swallow silently so nothing else fires
		}
		switch v.Focus {
		case FocusManagement:
			v.Focus = FocusPartitions
		case FocusPartitions:
			v.Focus = FocusTopology
		default:
			v.Focus = FocusManagement
		}
		return true
	}

	if v.showAddForm {
		return v.handleAddFormEvent(keyEv)
	}

	if v.confirmDelete {
		return v.handleDeleteConfirm(keyEv)
	}

	if v.Focus == FocusPartitions && v.Partitions != nil {
		return v.Partitions.HandleEvent(ev)
	}

	if v.Focus == FocusTopology {
		return v.Topology.HandleEvent(ev)
	}

	// Management pane keys.
	switch keyEv.Key() {
	case tcell.KeyUp:
		if v.Selected > 0 {
			v.Selected--
			v.fireOnHighlight()
		}
		return true
	case tcell.KeyDown:
		if v.Selected < len(v.Clusters)-1 {
			v.Selected++
			v.fireOnHighlight()
		}
		return true
	case tcell.KeyEnter:
		// Pick the highlighted cluster as the user's "working
		// cluster" (drives Explorer / Automations). If the entry is
		// in stateFailed, the app layer's OnEnter handler also kicks
		// a manual Retry.
		if v.Selected >= 0 && v.Selected < len(v.Clusters) && v.OnEnter != nil {
			v.OnEnter(v.Clusters[v.Selected].Config.Name)
		}
		return true
	case tcell.KeyEscape:
		// Cancel the highlighted cluster's retry cycle, if any.
		if v.Selected >= 0 && v.Selected < len(v.Clusters) && v.OnCancel != nil {
			v.OnCancel(v.Clusters[v.Selected].Config.Name)
		}
		return true
	case tcell.KeyRune:
		switch keyEv.Rune() {
		case 'a', 'A':
			v.showAddForm = true
			v.addForm = addFormState{}
			return true
		case 'e', 'E':
			// Open the form in edit mode, pre-populated from the selected
			// cluster. Works for every cluster including "local" -- the
			// save path persists local overrides in clusters.yaml.
			if v.Selected >= 0 && v.Selected < len(v.Clusters) {
				v.showAddForm = true
				v.addForm = formStateFromConfig(v.Clusters[v.Selected].Config)
			}
			return true
		case 'd', 'D':
			// Only allow deleting user-added clusters (not "local").
			if v.Selected >= 0 && v.Selected < len(v.Clusters) {
				if v.Clusters[v.Selected].Config.Name != "local" {
					v.confirmDelete = true
				}
			}
			return true
		case 'r', 'R':
			// Retry the highlighted cluster. Only meaningful when
			// it's in stateFailed; OnRetry short-circuits if not.
			if v.Selected >= 0 && v.Selected < len(v.Clusters) && v.OnRetry != nil {
				v.OnRetry(v.Clusters[v.Selected].Config.Name)
			}
			return true
		}
	}
	return false
}

func (v *ClustersView) handleDeleteConfirm(ev *tcell.EventKey) bool {
	switch {
	case ev.Key() == tcell.KeyEscape:
		v.confirmDelete = false
		return true
	case ev.Key() == tcell.KeyRune && (ev.Rune() == 'y' || ev.Rune() == 'Y'):
		if v.Selected >= 0 && v.Selected < len(v.Clusters) && v.OnDelete != nil {
			v.OnDelete(v.Clusters[v.Selected].Config.Name)
		}
		v.confirmDelete = false
		if v.Selected >= len(v.Clusters) && v.Selected > 0 {
			v.Selected--
		}
		return true
	default:
		v.confirmDelete = false
		return true
	}
}

func (v *ClustersView) handleAddFormEvent(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyEscape:
		v.showAddForm = false
		return true
	// Tab is reserved globally for pane navigation; explicitly
	// swallow it here so it doesn't accidentally drive anything while
	// a form is open. Forms move between fields with ↑/↓ only.
	case tcell.KeyTab:
		return true
	case tcell.KeyDown:
		v.addForm.formError = ""
		v.addForm.cursor = v.nextFormCursor(v.addForm.cursor, +1)
		return true
	case tcell.KeyUp:
		v.addForm.formError = ""
		v.addForm.cursor = v.nextFormCursor(v.addForm.cursor, -1)
		return true
	case tcell.KeyEnter:
		// Validate every field (name shape/length, host, port range,
		// issuer URL, client id). The first failure is surfaced inline
		// below the form fields; the next keystroke clears it so the
		// user can retry without an explicit dismiss.
		normName, err := ui.ValidateName(v.addForm.fields[formFieldName])
		if err != nil {
			v.addForm.formError = "Name: " + err.Error()
			return true
		}
		if err := ui.ValidateHost(v.addForm.fields[formFieldHost]); err != nil {
			v.addForm.formError = "Host: " + err.Error()
			return true
		}
		if err := ui.ValidatePort(v.addForm.fields[formFieldPort]); err != nil {
			v.addForm.formError = "Port: " + err.Error()
			return true
		}
		if err := ui.ValidateOptionalURL(v.addForm.fields[formFieldIssuer]); err != nil {
			v.addForm.formError = "Issuer: " + err.Error()
			return true
		}
		if err := ui.ValidateOptionalClientId(v.addForm.fields[formFieldClientId]); err != nil {
			v.addForm.formError = "Client ID: " + err.Error()
			return true
		}
		c := config.ClusterConfig{
			Name:     normName,
			Endpoint: joinEndpoint(strings.TrimSpace(v.addForm.fields[formFieldHost]), strings.TrimSpace(v.addForm.fields[formFieldPort])),
			Issuer:   strings.TrimSpace(v.addForm.fields[formFieldIssuer]),
			ClientId: strings.TrimSpace(v.addForm.fields[formFieldClientId]),
		}
		switch {
		case v.addForm.editMode && v.OnSave != nil:
			v.OnSave(c)
		case !v.addForm.editMode && v.OnAdd != nil:
			v.OnAdd(c)
		}
		v.showAddForm = false
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		v.addForm.formError = ""
		i := v.addForm.cursor
		if len(v.addForm.fields[i]) > 0 {
			v.addForm.fields[i] = v.addForm.fields[i][:len(v.addForm.fields[i])-1]
		}
		return true
	case tcell.KeyRune:
		v.addForm.formError = ""
		// Per-field keystroke filter + length cap. Silently drop
		// characters the validator would later reject -- the input
		// stream stays clean and we never have to unwind a bad char.
		i := v.addForm.cursor
		r := ev.Rune()
		var allowed bool
		var cap int
		switch i {
		case formFieldName:
			allowed = ui.IsNameChar(r)
			cap = ui.MaxNameLen
			// Auto-lowercase so the stored form matches the server's
			// normalized id (and what the user sees in the list after save).
			if allowed && r >= 'A' && r <= 'Z' {
				r = r + ('a' - 'A')
			}
		case formFieldHost:
			allowed = ui.IsHostChar(r)
			cap = ui.MaxHostLen
		case formFieldPort:
			allowed = ui.IsPortChar(r)
			cap = ui.MaxPortLen
		case formFieldIssuer:
			// Let anything printable through and clip by length --
			// URLs carry a lot of characters legitimately.
			allowed = r >= 0x20 && r != 0x7f
			cap = ui.MaxURLLen
		case formFieldClientId:
			allowed = r >= 0x20 && r != 0x7f
			cap = ui.MaxClientIdLen
		default:
			allowed = true
			cap = ui.MaxURLLen
		}
		if !allowed {
			return true
		}
		if len(v.addForm.fields[i]) >= cap {
			return true
		}
		v.addForm.fields[i] += string(r)
		return true
	}
	return false
}

// nextFormCursor advances the form cursor by delta (+1 or -1), skipping
// the Name field in edit mode (which is rendered as read-only).
func (v *ClustersView) nextFormCursor(cur, delta int) int {
	for i := 0; i < formFieldCount; i++ {
		cur = (cur + delta + formFieldCount) % formFieldCount
		if v.addForm.editMode && cur == formFieldName {
			continue
		}
		return cur
	}
	return cur
}

func clusterStatusIcon(status string, theme ui.Theme) (rune, tcell.Color) {
	switch status {
	case "connected":
		return '●', theme.Success
	case "available":
		return '○', theme.Success
	case "unreachable":
		return '●', theme.Error
	case "connecting":
		return '◌', theme.Warning
	default:
		return '○', theme.Subtle
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
