// Architecture navigator for the Topology pane.
//
// The live-topology grid (drawn by drawTopology) shows WHO is running
// right now -- cluster nodes, health colors, panable. The architecture
// navigator shows WHAT EXISTS in the code -- cluster -> service ->
// package -> type -> method, drawn from the embedded topology.model.json
// shipped by memql/component/architecture/embedded.
//
// The two views live in the same pane: pressing 'X' on the cluster
// topology toggles into the architecture navigator; Backspace zooms
// out one C4 level, Esc returns to the live topology, Enter zooms
// into the highlighted node. Both views share the Topology pane
// chrome (title + status bar) so the surrounding layout doesn't
// shift when the user toggles.

package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"

	archembed "github.com/znasllc-io/memql/component/architecture/embedded"
	"github.com/znasllc-io/memql/component/architecture/model"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// MetricSummary is the row-level observability overlay populated by
// RefreshMetrics. Keyed by model.Node.ID so the renderer can look it
// up O(1) per visible row.
type MetricSummary struct {
	CallCount  int64
	P95DurNs   int64
	ErrorRate  float64 // 0..1
}

// ArchView is the in-pane navigator for the architecture model.
// State is intentionally tiny: a pointer to the loaded model and an
// index over it, plus the zoom stack of node IDs the user has drilled
// into. The "current focus" is the top of the stack; everything
// rendered is a child of that focus.
type ArchView struct {
	Theme ui.Theme

	model *model.Model
	index *model.Index

	// active is the toggle. When false, the regular live-topology
	// grid is drawn instead and ArchView.Draw is a no-op.
	active bool

	// zoomStack is the breadcrumb of node IDs from the cluster root
	// (top of model's tree) down to the current focus. Empty stack
	// means "show the cluster root."
	zoomStack []model.ID

	// highlight is the index of the focused child within the current
	// view's child list. Reset to 0 when the focus changes.
	highlight int

	// loadErr captures any failure to decode the embedded model.
	// Surfaced once in the status line when the user toggles in, so
	// you find out about it without a silent empty pane.
	loadErr error

	// metrics is the live observability overlay -- callCount / p95 /
	// errorRate keyed by Node.ID. Populated by RefreshMetrics
	// (called by the parent View on a low-cadence ticker); nil when
	// no fetch has succeeded yet. Renderer treats nil and empty
	// values identically: just no badges.
	metrics map[model.ID]MetricSummary
}

// NewArchView decodes the embedded model. A decode error is captured
// on the view rather than returned: the cockpit should still launch
// even if the embedded JSON is malformed; the navigator just won't
// have anything to show and will say so.
func NewArchView(theme ui.Theme) *ArchView {
	v := &ArchView{Theme: theme}
	m, err := archembed.Load()
	if err != nil {
		v.loadErr = err
		return v
	}
	v.model = m
	v.index = model.NewIndex(m)
	return v
}

// Active reports whether the navigator should currently render
// instead of the live topology.
func (a *ArchView) Active() bool { return a != nil && a.active }

// Toggle flips between live topology and architecture navigator.
// Returns the new state so callers can update their hint bar.
func (a *ArchView) Toggle() bool {
	if a == nil {
		return false
	}
	a.active = !a.active
	if a.active {
		// Reset on enter so each toggle starts from the cluster root.
		a.zoomStack = a.zoomStack[:0]
		a.highlight = 0
	}
	return a.active
}

// Close forces the navigator back to the live topology, used when
// the parent pane wants to handle Escape without consulting the
// current zoom level.
func (a *ArchView) Close() {
	if a == nil {
		return
	}
	a.active = false
}

// HandleEvent processes a keypress and returns true if consumed.
// Routes for navigation (arrows / Enter / Backspace), bail-out (Esc),
// and toggling away (rune 'x' / 'X' mirrors the toggle key in the
// outer pane).
func (a *ArchView) HandleEvent(ev tcell.Event) bool {
	if a == nil || !a.active {
		return false
	}
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}
	children := a.currentChildren()

	switch keyEv.Key() {
	case tcell.KeyUp:
		if len(children) == 0 {
			return true
		}
		if a.highlight > 0 {
			a.highlight--
		}
		return true
	case tcell.KeyDown:
		if len(children) == 0 {
			return true
		}
		if a.highlight < len(children)-1 {
			a.highlight++
		}
		return true
	case tcell.KeyEnter:
		if len(children) == 0 {
			return true
		}
		next := children[a.highlight].ID
		a.zoomStack = append(a.zoomStack, next)
		a.highlight = 0
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(a.zoomStack) > 0 {
			a.zoomStack = a.zoomStack[:len(a.zoomStack)-1]
			a.highlight = 0
		}
		return true
	case tcell.KeyEsc:
		a.active = false
		return true
	case tcell.KeyRune:
		switch keyEv.Rune() {
		case 'x', 'X':
			a.active = false
			return true
		}
	}
	return false
}

// Draw renders the navigator into bounds. Three regions, top-to-bottom:
//
//	BREADCRUMB        cluster:projects > service:memql > pkg:.../component/auth
//	LIST              one row per child, with kind + name + observable tag
//	HINT              Enter:zoom in   Backspace:zoom out   X/Esc:back to live
//
// The chrome (cluster name title + status bar) is drawn by the parent
// View.Draw so the toggle doesn't shift the surrounding layout.
func (a *ArchView) Draw(screen *ui.Screen, bounds ui.Rect) {
	if a == nil || !a.active {
		return
	}
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, a.Theme.BaseStyle())

	if a.loadErr != nil {
		errStyle := tcell.StyleDefault.Foreground(a.Theme.Error).Background(a.Theme.BG)
		screen.DrawText(bounds.X+2, bounds.Y+2, bounds.Width-4,
			fmt.Sprintf("architecture model unavailable: %v", a.loadErr), errStyle)
		return
	}
	if a.model == nil {
		warnStyle := tcell.StyleDefault.Foreground(a.Theme.Warning).Background(a.Theme.BG)
		screen.DrawText(bounds.X+2, bounds.Y+2, bounds.Width-4,
			"architecture model not loaded -- run memql-arch and rebuild", warnStyle)
		return
	}

	// Breadcrumb. Builds top-down from cluster -> focus, separated by
	// the " > " glyph (ASCII-safe in every terminal we target).
	bcStyle := tcell.StyleDefault.Foreground(a.Theme.Info).Background(a.Theme.BG).Bold(true)
	breadcrumb := a.breadcrumbText()
	screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-4, breadcrumb, bcStyle)

	// Children list. Each row: [kind] name  (optional badge)
	listX := bounds.X + 2
	listY := bounds.Y + 3
	listW := bounds.Width - 4
	listH := bounds.Height - 5 // leave 2 rows for breadcrumb above + hint below

	children := a.currentChildren()
	if len(children) == 0 {
		subtle := tcell.StyleDefault.Foreground(a.Theme.Subtle).Background(a.Theme.BG)
		screen.DrawText(listX, listY, listW, "(nothing inside this node)", subtle)
	} else {
		// Simple non-scrolling list -- if there are more children than
		// rows, the slice is truncated at the bottom. A scrollable
		// pager can come in a follow-up; the navigator's value is in
		// the drill-down, not in deep within-level lists.
		visible := children
		if len(visible) > listH {
			visible = visible[:listH]
		}
		for i, n := range visible {
			rowY := listY + i
			style := tcell.StyleDefault.Foreground(a.Theme.FG).Background(a.Theme.BG)
			if i == a.highlight {
				style = tcell.StyleDefault.Foreground(a.Theme.BG).Background(a.Theme.Accent).Bold(true)
			}
			row := a.formatRow(n, listW)
			// Fill the full row width with the row background so
			// the highlight bar reads as a contiguous strip.
			screen.FillRect(listX, rowY, listW, 1, style)
			screen.DrawText(listX, rowY, listW, row, style)
		}
	}

	// Hint bar at the bottom of the navigator's area.
	hintStyle := tcell.StyleDefault.Foreground(a.Theme.Subtle).Background(a.Theme.BG)
	hints := "Enter:zoom in   Backspace:zoom out   X/Esc:back to live"
	hintY := bounds.Y + bounds.Height - 1
	screen.DrawText(bounds.X+2, hintY, bounds.Width-4, hints, hintStyle)
}

// currentChildren resolves the focus to its child list. The focus
// is either the top of zoomStack or, when the stack is empty, the
// cluster root (the model's first cluster node).
func (a *ArchView) currentChildren() []*model.Node {
	if a == nil || a.index == nil {
		return nil
	}
	focus := a.currentFocus()
	if focus == "" {
		return nil
	}
	return a.index.Children(focus)
}

// currentFocus returns the ID at the top of the zoom stack, or the
// cluster root when the stack is empty.
func (a *ArchView) currentFocus() model.ID {
	if len(a.zoomStack) > 0 {
		return a.zoomStack[len(a.zoomStack)-1]
	}
	// Cluster root: first node with Kind=cluster in the model. The
	// extractor always emits exactly one of these.
	for i := range a.model.Nodes {
		if a.model.Nodes[i].Kind == model.KindCluster {
			return a.model.Nodes[i].ID
		}
	}
	return ""
}

// breadcrumbText assembles the breadcrumb line. Format:
//
//	cluster:projects > service:memql > pkg:component/auth
//
// We use Name (the short label) per segment so the line stays
// readable even five levels deep.
func (a *ArchView) breadcrumbText() string {
	if a.index == nil {
		return ""
	}
	parts := []string{}
	// Always include the cluster root.
	if root := a.index.Node(a.currentClusterRoot()); root != nil {
		parts = append(parts, fmt.Sprintf("[%s] %s", root.Kind, root.Name))
	}
	for _, id := range a.zoomStack {
		n := a.index.Node(id)
		if n == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s] %s", n.Kind, n.Name))
	}
	return strings.Join(parts, " > ")
}

// currentClusterRoot returns the cluster node's ID. Cached at the
// model level, but cheap enough to find every render.
func (a *ArchView) currentClusterRoot() model.ID {
	for i := range a.model.Nodes {
		if a.model.Nodes[i].Kind == model.KindCluster {
			return a.model.Nodes[i].ID
		}
	}
	return ""
}

// formatRow renders one child row. Layout, left-to-right:
//
//	[kind] name   observable   observe=level   metric:n/p95/err   (count)
//
// where the observable badge appears only on nodes whose
// `observable` attr is true (Methods, Funcs, Interfaces from the
// L4 pass), and the count_n suffix shows how many grandchildren
// are inside (useful at the package / type level so you know
// whether drilling in is going to find anything). The metric
// triple is sourced from the observability overlay populated by
// RefreshMetrics; absent when there's been no traffic on this
// node since the last refresh.
func (a *ArchView) formatRow(n *model.Node, width int) string {
	kindLabel := fmt.Sprintf("[%s]", n.Kind)
	base := fmt.Sprintf("%-12s %s", kindLabel, n.Name)

	suffix := ""
	if n.Attrs["observable"] == "true" {
		suffix += "  observable"
	}
	if level := n.Attrs["observe_level"]; level != "" {
		suffix += "  observe=" + level
	}
	if a.metrics != nil {
		if ms, ok := a.metrics[n.ID]; ok && ms.CallCount > 0 {
			suffix += fmt.Sprintf("  n=%d p95=%dms err=%.0f%%",
				ms.CallCount, ms.P95DurNs/1_000_000, ms.ErrorRate*100)
		}
	}
	if kids := a.index.Children(n.ID); len(kids) > 0 {
		suffix += fmt.Sprintf("  (%d)", len(kids))
	}

	out := base + suffix
	if len(out) > width {
		out = out[:width]
	}
	return out
}

// SetMetrics installs the observability overlay. The parent View
// calls this from its periodic refresh goroutine after parsing the
// codeMetric query result; the navigator stores it verbatim and
// uses it on the next draw.
func (a *ArchView) SetMetrics(m map[model.ID]MetricSummary) {
	if a == nil {
		return
	}
	a.metrics = m
}

// MetricsFetcher is the seam between the cockpit's gRPC client and
// this navigator. Implementations call a MemQL query
// (`concept==v1:observability:codeMetric&filter=...`) and translate
// the result into the FQN-keyed map. Kept as an interface so the
// navigator stays decoupled from the QueryClient (and so tests can
// substitute a fixed map).
type MetricsFetcher interface {
	FetchRecentMetrics(ctx context.Context) (map[model.ID]MetricSummary, error)
}

// RefreshMetrics pulls a fresh overlay from the fetcher and installs
// it. Best-effort: fetcher errors are logged on stderr and the
// existing overlay is retained, so a transient gRPC failure doesn't
// blank out the diagram.
func (a *ArchView) RefreshMetrics(ctx context.Context, fetcher MetricsFetcher) {
	if a == nil || fetcher == nil {
		return
	}
	m, err := fetcher.FetchRecentMetrics(ctx)
	if err != nil {
		// Non-fatal; the renderer simply re-uses the previous
		// overlay (or shows none if there was none yet).
		return
	}
	a.SetMetrics(m)
}
