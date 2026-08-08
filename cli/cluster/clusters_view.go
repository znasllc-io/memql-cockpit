package cluster

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/config"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
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
	FocusTopology   FocusPane = 1
)

// ClustersView is the top-level Clusters tab: left = management, right = topology.
//
// Connection lifecycle state lives outside this view (in the app's
// pool). ClustersView only renders: per-row status decorated via
// ClusterStatus, "selected" marker via SelectedCluster, retry/backoff
// details pulled via OnEntryState. No global Busy modal; all keys
// remain responsive while dials happen in the background.
//
// Thread-safety: SetClusters / SetConnected / SetRowStatus are called
// from pool-lifecycle goroutines whenever a cluster's dial state
// changes; the tcell event loop concurrently calls Draw + HandleEvent.
// Mu (inherited from ui.BaseView) serializes writers (Lock) against
// Draw (RLock). The embedded Topology view owns its own mu, so
// locking here is per-row state only (Clusters slice, Selected,
// SelectedCluster, form state) -- nested re-entry through
// Topology.Draw etc. happens outside this view's lock.
//
// Migrated to the cli/ui widget layer. Hint composition uses
// ui.HintBar; the inline selection-background RGB literal is gone
// in favor of Theme.SelectionStyle(); the cluster list itself uses
// ui.ListPane with RowsPerItem=2 (primary: status icon + name,
// subtitle: endpoint + status) so every list-bearing pane in the
// cockpit reads with the same primary/subtitle rhythm. "local"
// stays sorted first via the slice order (see SetClusters) but no
// longer holds the viewport across scroll.
type ClustersView struct {
	ui.BaseView // Mu / Theme / GatedMessage / OnStatus / OnRedraw
	Focus       FocusPane
	Topology    *View // node topology diagram (right pane)

	// Cluster management state.
	Clusters []ClusterStatus
	Selected int // index of the currently-highlighted row (arrow keys)

	// clusterList renders the management pane's cluster list.
	// RowsPerItem=2 matches the planner/agents/concepts/chat lists.
	// Selected/ScrollY live in the widget; the view syncs Selected
	// before each Draw and reads it back after HandleEvent.
	clusterList ui.ListPane

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
	// OnLogin runs the OAuth / magic-link browser flow against a
	// fully-configured cluster (one that already has Issuer +
	// ClientId, or a PAT). Used when the user presses L on a row
	// that doesn't need to be reconfigured -- only re-authenticated.
	OnLogin func(clusterName string)
	// OnEntryState returns the pool-entry state for rendering row
	// details (retry attempt, next-try countdown). Returns false when
	// no entry exists yet. Called only during Draw, from the UI thread.
	OnEntryState func(clusterName string) (state string, attempt int, nextTryAt string, ok bool)
}

// addFormState backs the Add/Edit cluster form. The user types one
// Domain (e.g. "staging.copresent.ai"); Endpoint / Issuer / ClientId
// are composed from it by convention on save (see composeFromDomain).
// Deployments that don't follow the cockpit./identity. convention are
// edited directly in ~/.memql/clusters.yaml.
type addFormState struct {
	domain string
	// local is the Local toggle -- "this cluster runs on my machine".
	// Persisted to clusters.yaml as `local:`, where the memQL VS Code
	// extension reads it to gate its non-local write confirmation
	// (znasllc-io/memql#3313).
	local    bool
	editMode bool   // true when re-opening the form for an existing cluster
	editName string // original (immutable) cluster name when editMode is true
	// cursor is the focused field: formFieldDomain or formFieldLocal.
	cursor int
	// formError is set by Enter-handling when validation fails. Cleared
	// on the next keystroke so the hint disappears once the user
	// acknowledges it and continues editing.
	formError string
}

// Focusable fields in the Add/Edit form, in render order.
const (
	formFieldDomain = iota
	formFieldLocal
	formFieldCount
)

// defaultLocalClusterConfig is the seed used when no user override
// exists in clusters.yaml. The name slot is always "local"; the
// endpoint is intentionally blank so the binary carries no
// domain-specific assumption. `genesis init` populates the endpoint
// from IDENTITY_BOOTSTRAP_DOMAIN at bootstrap; if the user hasn't
// run it yet, the row shows up as "needs-auth" and the TUI surfaces
// `L:Authorize` for them to wire up a cluster.
var defaultLocalClusterConfig = config.ClusterConfig{
	Name: "local",
}

// LocalClusterConfig returns the active local-cluster default. Exposed as
// a function so tests can swap the underlying value.
func LocalClusterConfig() config.ClusterConfig {
	return defaultLocalClusterConfig
}

// composeFromDomain builds a full ClusterConfig from a single domain by
// the cockpit./identity. convention the local stack ships with (and that
// autoSeedLocalFromGenesis uses): Endpoint=https://cockpit.<domain>,
// Issuer=https://identity.<domain>, ClientId="cockpit". The caller has
// already validated + normalized the domain.
func composeFromDomain(domain string) config.ClusterConfig {
	return config.ClusterConfig{
		Name:        ui.DomainToName(domain),
		DisplayName: domain,
		Domain:      domain,
		Endpoint:    "https://cockpit." + domain,
		Issuer:      "https://identity." + domain,
		ClientId:    "cockpit",
	}
}

// domainFromConfig recovers the domain for the Edit form: the stored
// Domain when present, else derived from a conventional
// https://cockpit.<domain> endpoint, else blank (hand-edited rows that
// don't follow the convention just start empty -- retype the domain or
// keep editing clusters.yaml by hand).
func domainFromConfig(c config.ClusterConfig) string {
	if d := strings.TrimSpace(c.Domain); d != "" {
		return d
	}
	ep := strings.TrimSpace(c.Endpoint)
	ep = strings.TrimPrefix(ep, "https://")
	ep = strings.TrimPrefix(ep, "http://")
	ep = strings.TrimPrefix(ep, "cockpit.")
	if i := strings.LastIndex(ep, ":"); i >= 0 {
		ep = ep[:i]
	}
	return ep
}

// formStateFromConfig builds an addFormState pre-populated from an
// existing ClusterConfig, for opening the form in edit mode.
func formStateFromConfig(c config.ClusterConfig) addFormState {
	return addFormState{
		domain:   domainFromConfig(c),
		local:    c.Local,
		editMode: true,
		editName: c.Name,
	}
}

// NewClustersView creates the combined Clusters tab.
// The "local" cluster is always present and cannot be deleted.
func NewClustersView(theme ui.Theme) *ClustersView {
	v := &ClustersView{
		Topology: NewView(theme),
		Focus:    FocusManagement,
	}
	v.Theme = theme
	v.clusterList.RowsPerItem = 2
	v.clusterList.Render = v.renderClusterRow
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
	v.Mu.Lock()
	defer v.Mu.Unlock()
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
	v.Mu.Lock()
	defer v.Mu.Unlock()
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
	v.Mu.Lock()
	defer v.Mu.Unlock()
	for i := range v.Clusters {
		if v.Clusters[i].Config.Name == name {
			v.Clusters[i].Status = status
		}
	}
}

// Draw renders the DevOps tab. Holds the read lock for the full
// frame so a concurrent pool-lifecycle mutator (SetClusters /
// SetConnected / SetRowStatus) can't tear the Clusters slice or
// per-row Status mid-render.
func (v *ClustersView) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.Mu.RLock()
	defer v.Mu.RUnlock()
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

	v.drawManagement(screen, leftBounds)

	// Topology pane.
	ui.PaneTitle{
		Title:   "TOPOLOGY",
		Focused: v.Focus == FocusTopology,
	}.Draw(screen, rightBounds, v.Theme)
	topoBounds := ui.Rect{X: rightBounds.X, Y: rightBounds.Y + 1, Width: rightBounds.Width, Height: rightBounds.Height - 1}
	v.Topology.Draw(screen, topoBounds)
}

// drawManagement renders the cluster management left pane.
func (v *ClustersView) drawManagement(screen *ui.Screen, bounds ui.Rect) {
	y := bounds.Y
	x := bounds.X + 1
	maxW := bounds.Width - 2

	// Title reflects the pane's current mode. Entering add/edit
	// mode rewrites the title instead of stacking a second one
	// inside the form -- one status strip per pane, no duplicates.
	title := "CLUSTERS"
	if v.showAddForm {
		if v.addForm.editMode {
			title = "EDIT CLUSTER"
		} else {
			title = "ADD CLUSTER"
		}
	}
	ui.PaneTitle{
		Title:   title,
		Focused: v.Focus == FocusManagement,
	}.Draw(screen, bounds, v.Theme)
	y++

	if v.showAddForm {
		v.drawAddForm(screen, x, y, maxW, bounds)
		return
	}
	// Layout below the title is a three-band stack, bottom-up:
	//
	//   [chrome]     -- hints or delete-confirmation prompt (chromeH)
	//   [gap]        -- 1 blank row so chrome always has breathing room
	//   [detail]     -- selected row's endpoint/auth/status/retry/node
	//                   (detailMaxH rows; empty slots stay blank)
	//   [list]       -- scrollable cluster list (whatever remains)
	//
	// Fixed height is reserved for gap + detail + chrome so the list
	// can't grow into them when the cluster count balloons. The list
	// itself is a ui.ListPane with RowsPerItem=2 -- matches every
	// other list-bearing pane in the cockpit.
	const detailMaxH = 7 // divider + Endpoint label + 2 wrapped value rows + Auth + Status + (retry|Node)
	const chromeGapH = 1 // clear row above the action hints
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

	// Sync Selected -> widget, paint, then read back. ListPane owns
	// ScrollY across renders; the view stays out of the scroll math.
	v.clusterList.Count = len(v.Clusters)
	if v.Selected < 0 || v.Selected >= len(v.Clusters) {
		v.Selected = 0
	}
	v.clusterList.Selected = v.Selected
	v.clusterList.Focused = v.Focus == FocusManagement
	v.clusterList.Draw(screen, ui.Rect{
		X:      bounds.X,
		Y:      viewportTop,
		Width:  bounds.Width,
		Height: viewportH,
	}, v.Theme)

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
		screen.DrawHLine(x, dy, maxW, '─', v.Theme.SubtleStyle())
		dy++

		// Endpoint gets a full-width, wrapped value -- URLs are long and
		// must show in full, so the label sits on its own row and the
		// value wraps beneath at the full pane width (capped at 2 rows so
		// the block height stays stable as the user arrows around).
		screen.DrawText(x+1, dy, maxW-1, "Endpoint", subtleDetail)
		dy++
		epLines := ui.WrapText(cs.Config.Endpoint, maxW-2)
		if len(epLines) > 2 {
			epLines = epLines[:2]
		}
		for _, l := range epLines {
			screen.DrawText(x+2, dy, maxW-2, l, detailStyle)
			dy++
		}
		for i := len(epLines); i < 2; i++ {
			dy++ // pad to a fixed 2 value rows
		}

		authStr, authColor := authLabel(cs.Config, v.Theme)
		screen.DrawText(x+1, dy, 9, "Auth", subtleDetail)
		screen.DrawText(x+10, dy, maxW-10, authStr, detailStyle.Foreground(authColor))
		dy++

		screen.DrawText(x+1, dy, 9, "Status", subtleDetail)
		_, sColor := clusterStatusIcon(cs.Status, v.Theme)
		screen.DrawText(x+10, dy, maxW-10, cs.Status, detailStyle.Foreground(sColor))
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
			screen.DrawText(x+1, dy, 9, "Node", subtleDetail)
			screen.DrawText(x+10, dy, maxW-10, cs.NodeId, detailStyle)
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
	// Convention: Enter:Select only surfaces for selectable,
	// not-already-active rows. Future list-bearing panes should
	// follow this pattern.
	subtle := v.Theme.SubtleStyle()
	warnStyle := tcell.StyleDefault.Foreground(v.Theme.Warning).Background(v.Theme.BG)

	if v.confirmDelete && v.Selected >= 0 && v.Selected < len(v.Clusters) {
		name := v.Clusters[v.Selected].Config.Name
		ui.DrawBottomBlocks(screen, bounds, 1,
			ui.BottomBlock{Lines: []string{fmt.Sprintf("Delete %q?", name)}, Style: warnStyle},
			ui.BottomBlock{Lines: []string{"Y:Confirm  Esc:Cancel"}, Style: subtle},
		)
		return
	}
	ui.DrawBottom(screen, bounds, subtle, 1, v.hintsForManagement())
}

// renderClusterRow paints a 2-row cluster-list entry. Primary line:
// status icon + cluster name + optional '*' marker on the right
// edge for the currently-selected working cluster (v.SelectedCluster).
// Subtitle line: endpoint + status text in the dimmed planner-style
// metadata slot. ListPane fills both rows with the SelectionStyle
// background when sel is true, so the renderer just paints text.
func (v *ClustersView) renderClusterRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.Clusters) {
		return
	}
	cs := v.Clusters[idx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	// Status icon at column 1 (in the icon's own color, BG matches the
	// row so the selection background still reads correctly).
	icon, color := clusterStatusIcon(cs.Status, theme)
	screen.SetCell(bounds.X+1, bounds.Y, icon, primary.Foreground(color))

	// Name in accent + bold when this row is the active working
	// cluster, otherwise inherits the row's primary style.
	nameStyle := primary
	if cs.Config.Name == v.SelectedCluster {
		nameStyle = primary.Foreground(theme.Accent).Bold(true)
	}
	screen.DrawText(bounds.X+3, bounds.Y, bounds.Width-4, cs.Config.Display(), nameStyle)

	// `*` marker on the right edge of the primary row for the active
	// working cluster. Strictly single-cell ASCII per the layout-edge
	// glyph rule in cli/CLAUDE.md.
	if cs.Config.Name == v.SelectedCluster {
		markerX := bounds.X + bounds.Width - 2
		screen.SetCell(markerX, bounds.Y, '*', primary.Foreground(theme.Accent).Bold(true))
	}

	// Subtitle: "endpoint · status". Compose what's available so the
	// row reads usefully on partially-configured clusters too.
	endpoint := strings.TrimSpace(cs.Config.Endpoint)
	status := strings.TrimSpace(cs.Status)
	var subStr string
	switch {
	case endpoint != "" && status != "":
		subStr = endpoint + "  ·  " + status
	case endpoint != "":
		subStr = endpoint
	case status != "":
		subStr = status
	}
	if subStr != "" {
		screen.DrawText(bounds.X+3, bounds.Y+1, bounds.Width-4, subStr, sub)
	}
}

// hintsForManagement builds the context-aware action-hint band for
// the cluster manager pane. Every chip is gated on the row's state
// (and the pool's lifecycle state via OnEntryState) so the bar only
// advertises actions that work right now -- per the chrome contract's
// "Hints that lie rot trust" rule.
//
// Caller must hold the read lock.
func (v *ClustersView) hintsForManagement() string {
	canEdit := v.Selected >= 0 && v.Selected < len(v.Clusters) && v.Clusters[v.Selected].Config.Name != "local"
	canDelete := canEdit // same predicate today; kept distinct so future divergence (e.g. soft-delete on local) lands one chip at a time

	// Lifecycle chips are mutually exclusive -- a row is in at most
	// one of (connected / connecting / failed / needs-auth /
	// needs-login). Compute booleans for each and let the HintBar
	// omit the disabled ones.
	var (
		canSelect    bool // Enter:Select  (promote an already-live cluster)
		canConnect   bool // Enter:Connect (selection drives the dial, epic #239)
		canCancel    bool // Esc:Cancel
		canRetry     bool // R:Retry
		canAuthorize bool // L:Authorize
		canLogin     bool // L:Login
	)
	if v.OnEntryState != nil && v.Selected >= 0 && v.Selected < len(v.Clusters) {
		name := v.Clusters[v.Selected].Config.Name
		alreadySelected := name == v.SelectedCluster
		if state, _, _, ok := v.OnEntryState(name); ok {
			switch state {
			case "connected":
				canSelect = !alreadySelected
			case "connecting", "backoff":
				canCancel = true
			case "failed":
				// Selection now drives the (re)connect: Enter on a
				// non-selected dead cluster brings it up as the working
				// cluster. R:Retry stays for the in-place repair (and
				// works on the selected row, where Enter is hidden).
				canConnect = !alreadySelected
				canRetry = true
			case "idle":
				// Registered-but-not-dialed (epic #239): Enter connects
				// it. The selected cluster is never idle, so this only
				// surfaces on non-selected rows.
				canConnect = !alreadySelected
			case "needs-auth":
				canAuthorize = true
			case "needs-login":
				canLogin = true
			}
		}
	}

	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "A", Label: "Add"},
		{Key: "E", Label: "Edit", Disabled: !canEdit},
		{Key: "Enter", Label: "Select", Disabled: !canSelect},
		{Key: "Enter", Label: "Connect", Disabled: !canConnect},
		{Key: "Esc", Label: "Cancel", Disabled: !canCancel},
		{Key: "R", Label: "Retry", Disabled: !canRetry},
		{Key: "L", Label: "Authorize", Disabled: !canAuthorize},
		{Key: "L", Label: "Login", Disabled: !canLogin},
		{Key: "D", Label: "Del", Disabled: !canDelete},
	}}
	return bar.String()
}

func (v *ClustersView) drawAddForm(screen *ui.Screen, x, y, maxW int, bounds ui.Rect) {
	// Pane title already reads 'ADD CLUSTER' / 'EDIT CLUSTER' in
	// drawManagement, so no inline title here. One blank line of
	// breathing room before the field.
	_ = bounds
	y++

	// In edit mode show the immutable config key for context -- changing
	// the domain recomposes the URLs but keeps the key.
	if v.addForm.editMode {
		screen.DrawText(x+1, y, maxW-1, "Cluster  "+v.addForm.editName, v.Theme.SubtleStyle())
		y += 2
	}

	// Field 1: Domain. Endpoint / Issuer / ClientId are composed from
	// it on save (see composeFromDomain).
	domainFocused := v.addForm.cursor == formFieldDomain
	screen.DrawText(x+1, y, 10, "Domain", v.formLabelStyle(domainFocused))
	fieldX := x + 12
	fieldW := min(maxW-13, 28)
	if fieldW < 3 {
		return
	}
	caretStyle := tcell.StyleDefault.Background(v.Theme.FG)
	fieldStyle := v.formFieldStyle(domainFocused)
	screen.FillRect(fieldX, y, fieldW, 1, fieldStyle)
	if v.addForm.domain == "" {
		screen.DrawText(fieldX+1, y, fieldW-2, "example.copresent.ai", fieldStyle.Foreground(v.Theme.Subtle))
		if domainFocused {
			screen.SetCell(fieldX+1, y, ' ', caretStyle)
		}
	} else {
		// Horizontal scroll so a long domain keeps its end + caret on
		// screen (memql-cockpit#193).
		ui.DrawInputValue(screen, fieldX, y, fieldW, v.addForm.domain, domainFocused, fieldStyle, caretStyle)
	}
	y += 2

	// Field 2: Local -- a toggle, not free text. Rendered in the same
	// "[x] on" / "[ ] off" shape ui.Form uses for FieldToggle so the
	// two form surfaces read alike.
	localFocused := v.addForm.cursor == formFieldLocal
	screen.DrawText(x+1, y, 10, "Local", v.formLabelStyle(localFocused))
	localStyle := v.formFieldStyle(localFocused)
	screen.FillRect(fieldX, y, fieldW, 1, localStyle)
	toggle := "[ ] off"
	if v.addForm.local {
		toggle = "[x] on"
	}
	screen.DrawText(fieldX+1, y, fieldW-2, toggle, localStyle)
	y += 2

	// Inline validation error (if any). Wrapped via ui.WrapText so a
	// long message doesn't truncate in the narrow left column.
	if v.addForm.formError != "" {
		errStyle := tcell.StyleDefault.Foreground(v.Theme.Error).Background(v.Theme.BG)
		for _, ln := range ui.WrapText(v.addForm.formError, maxW) {
			screen.DrawText(x, y, maxW, ln, errStyle)
			y++
		}
		y++
	}

	subtle := v.Theme.SubtleStyle()
	screen.DrawText(x, y, maxW, "↑/↓:Field      Space:Toggle", subtle)
	y++
	screen.DrawText(x, y, maxW, "Enter:Save       Esc:Cancel", subtle)
}

// formLabelStyle / formFieldStyle paint an Add/Edit form row focused or
// not, matching ui.Form's convention (accent label + lighter input box
// on the focused row).
func (v *ClustersView) formLabelStyle(focused bool) tcell.Style {
	if focused {
		return v.Theme.AccentStyle()
	}
	return v.Theme.BaseStyle()
}

func (v *ClustersView) formFieldStyle(focused bool) tcell.Style {
	bg := tcell.NewRGBColor(35, 38, 45)
	if focused {
		bg = tcell.NewRGBColor(50, 55, 65)
	}
	return tcell.StyleDefault.Foreground(v.Theme.FG).Background(bg)
}

// HandleEvent routes events based on focus and state.
// Per pool-based design: there is no global "busy" modal. Every key
// remains responsive at all times -- a row that's mid-retry shows
// that state via its icon, and Esc on such a row asks the lifecycle
// to cancel. Arrow keys, Tab, Ctrl+1..4 all keep working regardless
// of what any cluster is doing in the background.
// FormOpen reports whether this pane (cluster manager OR its nested
// any sub-pane) has a form active. Exposed so higher-level views
// (or the app shell) can short-circuit any Tab-like behavior that
// would pull focus out from under the user mid-edit.
func (v *ClustersView) FormOpen() bool {
	if v == nil {
		return false
	}
	return v.showAddForm
}

// HandleEvent processes keys. Takes the write lock for the duration
// so a concurrent pool-lifecycle SetX call can't race row-level
// state. The Topology sub-view owns its own lock and is dispatched
// without this lock held -- the routing fan-out is state-free.
// fireUnlocked invokes an app-layer callback WITHOUT v.Mu held, then
// re-acquires the lock. HandleEvent holds v.Mu for its whole body, but
// the OnAdd / OnSave / OnDelete callbacks re-enter the view via
// SetClusters / SetConnected (which take v.Mu). Go's mutex is not
// reentrant, so calling them under the lock self-deadlocks the event
// loop (memql-cockpit#201). Releasing around the call-out, then
// re-locking, keeps HandleEvent's deferred Unlock balanced. MUST be
// called only while v.Mu is held (i.e. from within HandleEvent).
func (v *ClustersView) fireUnlocked(cb func()) {
	v.Mu.Unlock()
	cb()
	v.Mu.Lock()
}

func (v *ClustersView) HandleEvent(ev tcell.Event) bool {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// Tab switches focus between Management and Topology, EXCEPT
	// when a form is open. Typing inside a form and accidentally
	// jumping to Topology is confusing; the form owner always
	// keeps Tab until the form closes.
	if keyEv.Key() == tcell.KeyTab {
		if v.showAddForm {
			return true // swallow silently so nothing else fires
		}
		switch v.Focus {
		case FocusManagement:
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

	if v.Focus == FocusTopology {
		return v.Topology.HandleEvent(ev)
	}

	// Try the cluster list widget first for navigation keys (Up/Down,
	// PgUp/PgDn, Home/End, j/k). If consumed and Selected moved,
	// re-fire OnHighlight so the topology pane follows.
	v.clusterList.Focused = true
	v.clusterList.Count = len(v.Clusters)
	v.clusterList.Selected = v.Selected
	if v.clusterList.HandleEvent(keyEv) {
		if v.clusterList.Selected != v.Selected {
			v.Selected = v.clusterList.Selected
			v.fireOnHighlight()
		}
		return true
	}

	// Management pane keys.
	switch keyEv.Key() {
	case tcell.KeyEnter:
		// Pick the highlighted cluster as the user's "working
		// cluster" (drives Explorer / Agents). If the entry is
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
			// Open the form in edit mode, pre-populated from the
			// selected cluster. Blocked for "local" -- its config
			// comes from the genesis envelope, not the form.
			if v.Selected >= 0 && v.Selected < len(v.Clusters) {
				if v.Clusters[v.Selected].Config.Name == "local" {
					return true // swallow silently; hint never advertised E for local
				}
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
		case 'l', 'L':
			// L's behavior depends on what the row needs:
			//
			//   - "local" row -- ALWAYS triggers OnLogin. local's
			//     config comes from the genesis envelope, so the
			//     form path doesn't apply. If genesis didn't seed
			//     issuer/clientId, OnLogin's error path surfaces a
			//     useful notification instead.
			//   - Other rows in stateNeedsConfig -- open the form
			//     prefocused on Discovery URL, so the user can
			//     paste a URL and authorize from scratch.
			//   - All other states (needs-login, connected,
			//     failed, ...) -- run OAuth via OnLogin to mint or
			//     refresh a token.
			if v.Selected < 0 || v.Selected >= len(v.Clusters) {
				return true
			}
			name := v.Clusters[v.Selected].Config.Name
			if name == "local" {
				if v.OnLogin != nil {
					v.OnLogin(name)
				}
				return true
			}
			rowState := ""
			if v.OnEntryState != nil {
				if s, _, _, ok := v.OnEntryState(name); ok {
					rowState = s
				}
			}
			if rowState == "needs-auth" {
				// Row is missing issuer/clientId (hand-edited or pre-seed
				// local). Open the domain form so typing the domain
				// composes the URLs + client id.
				v.showAddForm = true
				v.addForm = formStateFromConfig(v.Clusters[v.Selected].Config)
				return true
			}
			if v.OnLogin != nil {
				v.OnLogin(name)
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
		var fire func()
		if v.Selected >= 0 && v.Selected < len(v.Clusters) && v.OnDelete != nil {
			name := v.Clusters[v.Selected].Config.Name
			cb := v.OnDelete
			fire = func() { cb(name) }
		}
		v.confirmDelete = false
		if v.Selected >= len(v.Clusters) && v.Selected > 0 {
			v.Selected--
		}
		// OnDelete re-enters the view (SetClusters/SetConnected); run it
		// without v.Mu held to avoid the re-entrant deadlock (#201).
		if fire != nil {
			v.fireUnlocked(fire)
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
	// ↑/↓ move between Domain and Local. Tab is swallowed rather than
	// used for navigation -- HandleEvent reserves it for pane focus and
	// deliberately eats it while a form is open.
	case tcell.KeyUp, tcell.KeyDown:
		v.addForm.formError = ""
		v.addForm.cursor = (v.addForm.cursor + 1) % formFieldCount
		return true
	case tcell.KeyTab:
		return true
	case tcell.KeyLeft, tcell.KeyRight:
		// Arrows cycle a toggle, matching ui.Form's FieldToggle
		// bindings. Text fields have no mid-string caret, so there is
		// nothing else for them to do.
		if v.addForm.cursor == formFieldLocal {
			v.addForm.formError = ""
			v.addForm.local = !v.addForm.local
		}
		return true
	case tcell.KeyEnter:
		domain, err := ui.ValidateDomain(v.addForm.domain)
		if err != nil {
			v.addForm.formError = err.Error()
			return true
		}
		c := composeFromDomain(domain)
		c.Local = v.addForm.local
		var fire func()
		if v.addForm.editMode {
			// Keep the original config key immutable so a saved token /
			// active-cluster lookup isn't orphaned; only the composed
			// URLs + display follow the (possibly new) domain.
			c.Name = v.addForm.editName
			if v.OnSave != nil {
				cb := v.OnSave
				fire = func() { cb(c) }
			}
		} else if v.OnAdd != nil {
			cb := v.OnAdd
			fire = func() { cb(c) }
		}
		v.showAddForm = false
		// OnAdd/OnSave re-enter the view (refreshClusterList ->
		// SetClusters); run without v.Mu held to avoid deadlock (#201).
		if fire != nil {
			v.fireUnlocked(fire)
		}
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		v.addForm.formError = ""
		if v.addForm.cursor != formFieldDomain {
			return true
		}
		if r := []rune(v.addForm.domain); len(r) > 0 {
			v.addForm.domain = string(r[:len(r)-1])
		}
		return true
	case tcell.KeyRune:
		v.addForm.formError = ""
		r := ev.Rune()
		if v.addForm.cursor == formFieldLocal {
			// Space flips the toggle; every other rune is swallowed so
			// stray typing can't leak into the domain field behind it.
			if r == ' ' {
				v.addForm.local = !v.addForm.local
			}
			return true
		}
		// Domains are a subset of the host charset; lower-case on the way
		// in so the stored value matches the server's normalized form.
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if !ui.IsHostChar(r) {
			return true
		}
		if len([]rune(v.addForm.domain)) >= ui.MaxHostLen {
			return true
		}
		v.addForm.domain += string(r)
		return true
	}
	return false
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
	case "needs-auth":
		// Empty diamond -- distinct shape from the dot family so the
		// "you haven't set this up yet" state can't be confused with
		// the "set up but down" red dot.
		return '◇', theme.Warning
	case "needs-login":
		// Hollow circle -- closer to "connected" than "needs-auth"
		// because the row IS configured; only the token is missing.
		// Coloured warning, not error, since a login fixes it
		// without any reconfiguration.
		return '◯', theme.Warning
	default:
		return '○', theme.Subtle
	}
}

// authLabel renders the Auth field in the cluster detail panel.
// Returns both the human-readable string and the colour to paint
// it -- "not configured" is a real state the user needs to action,
// so it gets the warning colour rather than the muted text style.
func authLabel(c config.ClusterConfig, theme ui.Theme) (string, tcell.Color) {
	switch {
	case c.PAT != "":
		return "PAT", theme.FG
	case c.Issuer != "" && c.ClientId != "":
		return "OIDC", theme.FG
	default:
		return "not configured", theme.Warning
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
