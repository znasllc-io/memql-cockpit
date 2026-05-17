// Package settings provides the Settings tab for memQL Cockpit.
package settings

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
)

// View renders the Settings tab — version info, MY ACCESS, and keyboard shortcuts.
//
// Thread-safety: SetMyAccess / ClearMyAccess are called from the
// app-layer refreshMyAccess goroutine when the active cluster
// changes. The tcell event loop concurrently calls Draw. mu
// serializes them.
type View struct {
	mu     sync.RWMutex
	Theme  ui.Theme
	CLIVer string

	// access is the resolved My Access record for the currently
	// selected cluster (nil when no cluster is connected or the
	// fetch hasn't completed yet).
	access        *memqlv1.MyAccessResult
	accessCluster string // for the "For cluster X" header line
}

// NewView creates a Settings view.
func NewView(theme ui.Theme, cliVersion string) *View {
	return &View{
		Theme:  theme,
		CLIVer: cliVersion,
	}
}

// SetMyAccess attaches the caller's access record to the view. The
// clusterName is shown alongside the data so the user knows which
// cluster's access is being rendered.
func (v *View) SetMyAccess(clusterName string, access *memqlv1.MyAccessResult) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.access = access
	v.accessCluster = clusterName
}

// ClearMyAccess wipes the current access record. Called when the
// active cluster goes away (disconnect, cluster deleted).
func (v *View) ClearMyAccess() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.access = nil
	v.accessCluster = ""
}

// Draw renders the Settings tab. Read lock for the full frame so
// SetMyAccess from refreshMyAccess can't tear access mid-render.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	// Top-level tab title. Matches the focus-accent blue used on other
	// tabs so the user has a clear "you are here" cue. Settings has no
	// focus switching between panes, so the title is always in accent.
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " SETTINGS ", v.Theme.AccentStyle().Bold(true))

	// Shift content down so the title has its own row.
	contentBounds := ui.Rect{X: bounds.X, Y: bounds.Y + 1, Width: bounds.Width, Height: bounds.Height - 1}

	panes := ui.FlexColumn(contentBounds, []ui.FlexItem{
		{Flex: 0.45, MinSize: 25},
		{Flex: 0.55, MinSize: 30},
	})

	leftBounds := panes[0]
	rightBounds := panes[1]

	// Divider.
	divX := leftBounds.X + leftBounds.Width
	for y := contentBounds.Y; y < contentBounds.Y+contentBounds.Height; y++ {
		screen.SetCell(divX-1, y, '│', v.Theme.SubtleStyle())
	}
	leftBounds.Width--

	v.drawInfo(screen, leftBounds)
	v.drawKeyBindings(screen, rightBounds)
}

func (v *View) drawInfo(screen *ui.Screen, bounds ui.Rect) {
	y := bounds.Y + 1
	x := bounds.X + 2
	maxW := bounds.Width - 4

	// Section header uses plain bold FG so it doesn't compete with the
	// accent color reserved for focused-pane indication. No leading
	// space -- the header left-aligns with the body text below it.
	screen.DrawText(x, y, maxW, "ABOUT", v.Theme.BaseStyle().Bold(true))
	y += 2

	screen.DrawText(x, y, maxW, "memQL Cockpit", v.Theme.BaseStyle().Bold(true))
	y++
	drawField(screen, x, y, maxW, "Version", v.CLIVer, v.Theme.SubtleStyle(), v.Theme.BaseStyle())
	y += 3

	// MY ACCESS block -- rendered only when we have an AccessContext
	// from the server. When no cluster is connected we show a placeholder
	// hint so users know where to look.
	y = v.drawMyAccess(screen, x, y, maxW)
	y++

	screen.DrawText(x, y, maxW, "QUICK START", v.Theme.SubtleStyle().Bold(true))
	y++
	screen.DrawHLine(x, y, min(maxW, 35), '─', v.Theme.SubtleStyle())
	y += 2

	steps := []string{
		"1. Go to the Clusters tab (F1)",
		"2. Press A to add a cluster",
		"3. Enter name and endpoint",
		"4. Press Enter to save",
		"5. Select the cluster, press Enter to connect",
	}
	for _, step := range steps {
		screen.DrawText(x+1, y, maxW-2, step, v.Theme.BaseStyle())
		y++
	}
}

// drawMyAccess renders the "MY ACCESS" section. Returns the Y after
// the block so the caller can keep stacking sections below.
func (v *View) drawMyAccess(screen *ui.Screen, x, y, maxW int) int {
	screen.DrawText(x, y, maxW, "MY ACCESS", v.Theme.SubtleStyle().Bold(true))
	y++
	screen.DrawHLine(x, y, min(maxW, 35), '─', v.Theme.SubtleStyle())
	y += 2

	if v.access == nil {
		screen.DrawText(x+1, y, maxW-2,
			"Connect to a cluster to see your access.",
			v.Theme.SubtleStyle())
		return y + 2
	}

	// ACCOUNT sub-block.
	headerLabel := "ACCOUNT"
	if v.accessCluster != "" {
		headerLabel = fmt.Sprintf("ACCOUNT (on %s)", v.accessCluster)
	}
	screen.DrawText(x+1, y, maxW-2, headerLabel, v.Theme.BaseStyle().Bold(true))
	y++
	drawField(screen, x+1, y, maxW-1, "User ID", v.access.GetUserId(), v.Theme.SubtleStyle(), v.Theme.BaseStyle())
	y++
	drawField(screen, x+1, y, maxW-1, "Email", v.access.GetPrimaryEmail(), v.Theme.SubtleStyle(), v.Theme.BaseStyle())
	y++
	drawField(screen, x+1, y, maxW-1, "Cluster role", roleLabel(v.access.GetClusterRole()), v.Theme.SubtleStyle(), v.Theme.BaseStyle())
	y += 2

	// PARTITIONS sub-block.
	grants := v.access.GetPartitions()
	screen.DrawText(x+1, y, maxW-2, "PARTITIONS", v.Theme.BaseStyle().Bold(true))
	y++
	if len(grants) == 0 {
		if v.access.GetClusterRole() == memqlv1.UserRole_USER_ROLE_OWNER {
			screen.DrawText(x+2, y, maxW-3,
				"(cluster owner -- implicit access to all)",
				v.Theme.SubtleStyle())
		} else {
			screen.DrawText(x+2, y, maxW-3,
				"No partitions accessible. Contact a cluster admin.",
				v.Theme.SubtleStyle())
		}
		return y + 2
	}

	sorted := append([]*memqlv1.PartitionGrant{}, grants...)
	sort.Slice(sorted, func(i, j int) bool {
		// Pin "default" first, alphabetical otherwise -- matches the
		// Partitions pane convention in the Clusters tab.
		a, b := sorted[i].GetPartition(), sorted[j].GetPartition()
		if a == "default" {
			return true
		}
		if b == "default" {
			return false
		}
		return a < b
	})
	for _, g := range sorted {
		drawField(screen, x+2, y, maxW-3, g.GetPartition(), roleLabel(g.GetRole()), v.Theme.SubtleStyle(), v.Theme.BaseStyle())
		y++
	}
	return y + 1
}

// roleLabel returns the lowercase string form of a UserRole enum.
func roleLabel(r memqlv1.UserRole) string {
	switch r {
	case memqlv1.UserRole_USER_ROLE_OWNER:
		return "owner"
	case memqlv1.UserRole_USER_ROLE_ADMIN:
		return "admin"
	case memqlv1.UserRole_USER_ROLE_WRITER:
		return "writer"
	case memqlv1.UserRole_USER_ROLE_READER:
		return "reader"
	default:
		return strings.ToLower(r.String())
	}
}

func (v *View) drawKeyBindings(screen *ui.Screen, bounds ui.Rect) {
	y := bounds.Y + 1
	x := bounds.X + 2
	maxW := bounds.Width - 4

	screen.DrawText(x, y, maxW, "KEYBOARD SHORTCUTS", v.Theme.BaseStyle().Bold(true))
	y += 2

	alt := ui.AltKey()
	sections := []keySection{
		{
			Title: "GLOBAL",
			Bindings: []keyBinding{
				{"F1 / " + alt + "+1", "Clusters tab"},
				{"F2 / " + alt + "+2", "Concepts tab"},
				{"F3 / " + alt + "+3", "Settings tab"},
				{"Tab", "Switch focus between panes"},
				{"Ctrl+Y", "Copy header notification to clipboard"},
				{"Ctrl+K", "Dismiss header notification"},
				{"Ctrl+Q", "Quit"},
			},
		},
		{
			Title: "CLUSTERS (F1)",
			Bindings: []keyBinding{
				{"Up / Down", "Navigate cluster list"},
				{"Enter", "Connect to selected cluster"},
				{"A", "Add new cluster"},
				{"E", "Edit selected cluster"},
				{"D", "Delete selected cluster"},
			},
		},
		{
			Title: "TOPOLOGY (CLUSTERS right pane)",
			Bindings: []keyBinding{
				{"W / A / S / D", "Pan viewport"},
				{"R", "Reset pan"},
				{"X", "Toggle architecture navigator"},
			},
		},
		{
			Title: "CONCEPTS (F2)",
			Bindings: []keyBinding{
				{"Up / Down", "Navigate list"},
				{"Enter", "Open / drill into selection"},
				{":", "Search rows (bottom band)"},
				{"V", "Toggle version history"},
				{"Esc", "Back / clear active filter"},
			},
		},
	}

	for _, section := range sections {
		if y >= bounds.Y+bounds.Height-2 {
			break
		}
		screen.DrawText(x, y, maxW, section.Title, v.Theme.SubtleStyle().Bold(true))
		y++
		screen.DrawHLine(x, y, min(maxW, 35), '─', v.Theme.SubtleStyle())
		y++
		for _, b := range section.Bindings {
			if y >= bounds.Y+bounds.Height-1 {
				break
			}
			keyStyle := tcell.StyleDefault.Foreground(v.Theme.Annotation).Background(v.Theme.BG)
			screen.DrawText(x+1, y, 20, b.Key, keyStyle)
			screen.DrawText(x+22, y, maxW-22, b.Desc, v.Theme.BaseStyle())
			y++
		}
		y++
	}
}

// HandleEvent handles key events in the settings view.
func (v *View) HandleEvent(ev tcell.Event) bool {
	return false
}

type keySection struct {
	Title    string
	Bindings []keyBinding
}

type keyBinding struct {
	Key  string
	Desc string
}

func drawField(screen *ui.Screen, x, y, maxW int, label, value string, labelStyle, valueStyle tcell.Style) {
	// No leading indent -- the field label aligns with the heading above
	// it ("memQL Cockpit") rather than being nested under it.
	text := fmt.Sprintf("%-12s ", label)
	screen.DrawText(x, y, len(text), text, labelStyle)
	screen.DrawText(x+len(text), y, maxW-len(text), value, valueStyle)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
