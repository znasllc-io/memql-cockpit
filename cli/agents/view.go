// Package agents renders the Agents tab: a list of materialized
// v1:agents:agent rows on the left and the RenderAgent detail block
// on the right. The data layer reuses cli/explorer's AgentEntry +
// RenderAgent so both the Explorer tree and this tab share one
// canonical projection of an agent row.
package agents

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/explorer"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// FocusPane identifies which pane has keyboard focus.
type FocusPane int

const (
	FocusList   FocusPane = 0
	FocusDetail FocusPane = 1
)

// View renders the Agents tab with two panes: list (left) and the
// RenderAgent block (right).
type View struct {
	Theme  ui.Theme
	Agents []explorer.AgentEntry

	Selected    int // index in Agents list
	ListScrollY int
	DetailLines []string
	DetailScroll int
	Focus       FocusPane

	// GatedMessage, when non-empty, replaces the layout with a
	// centered "not available" message. Set by the app layer when
	// the selected cluster has no live connection.
	GatedMessage string
}

// NewView creates an empty agents view.
func NewView(theme ui.Theme) *View {
	return &View{
		Theme: theme,
		Focus: FocusList,
	}
}

// SetAgents replaces the agent list with the materialized rows from
// the cache. The order is name-asc with a stable fallback so the
// caller doesn't have to pre-sort. The current selection is reset to
// the first row.
func (v *View) SetAgents(cache map[string]explorer.AgentEntry) {
	v.Agents = v.Agents[:0]
	for _, a := range cache {
		v.Agents = append(v.Agents, a)
	}
	sort.Slice(v.Agents, func(i, j int) bool {
		ni, nj := displayName(v.Agents[i]), displayName(v.Agents[j])
		if ni == nj {
			return v.Agents[i].ID < v.Agents[j].ID
		}
		return strings.ToLower(ni) < strings.ToLower(nj)
	})
	v.Selected = 0
	v.ListScrollY = 0
	v.refreshDetail()
}

// Draw renders the Agents tab.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	if v.GatedMessage != "" {
		screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
		title := " AGENTS "
		screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))
		midY := bounds.Y + bounds.Height/2
		lineX := bounds.X + (bounds.Width-len(v.GatedMessage))/2
		if lineX < bounds.X+1 {
			lineX = bounds.X + 1
		}
		screen.DrawText(lineX, midY, bounds.Width-1, v.GatedMessage, v.Theme.SubtleStyle())
		return
	}

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.35, MinSize: 24},
		{Flex: 0.65, MinSize: 30},
	})
	listBounds := panes[0]
	detailBounds := panes[1]

	// Divider column (single-cell box-drawing char -- safe at the
	// pane edge per cli/CLAUDE.md "Layout-edge glyph rule").
	divX := listBounds.X + listBounds.Width - 1
	divStyle := v.Theme.SubtleStyle()
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		screen.SetCell(divX, y, '│', divStyle)
	}
	listBounds.Width--

	v.drawList(screen, listBounds)
	v.drawDetail(screen, detailBounds)
}

func (v *View) drawList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusList {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " AGENTS ", titleStyle)

	if len(v.Agents) == 0 {
		empty := "No agents loaded."
		midY := bounds.Y + bounds.Height/2
		lineX := bounds.X + (bounds.Width-len(empty))/2
		if lineX < bounds.X+1 {
			lineX = bounds.X + 1
		}
		screen.DrawText(lineX, midY, bounds.Width-1, empty, v.Theme.SubtleStyle())
		return
	}

	listTop := bounds.Y + 2
	listHeight := bounds.Height - 2
	if listHeight < 1 {
		return
	}

	v.clampListScroll(listHeight)

	for i := 0; i < listHeight && v.ListScrollY+i < len(v.Agents); i++ {
		idx := v.ListScrollY + i
		a := v.Agents[idx]
		y := listTop + i

		style := v.Theme.BaseStyle()
		if idx == v.Selected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)

		// Active indicator (interior cell -- ambiguous-width glyph
		// is fine here; cf. layout-edge rule).
		indicator := '●'
		indStyle := style.Foreground(v.Theme.Success)
		if !a.Active || a.Deleted {
			indStyle = style.Foreground(v.Theme.Subtle)
		}
		screen.SetCell(bounds.X+1, y, indicator, indStyle)

		name := displayName(a)
		screen.DrawText(bounds.X+3, y, bounds.Width-4, name, style)
	}

	// Footer count.
	footer := footerCount(v.Selected, len(v.Agents))
	screen.DrawText(bounds.X+1, bounds.Y+bounds.Height-1, bounds.Width-2, footer, v.Theme.SubtleStyle())
}

func (v *View) drawDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusDetail {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " AGENT ", titleStyle)

	if len(v.DetailLines) == 0 {
		hint := "Select an agent to see its definition."
		midY := bounds.Y + bounds.Height/2
		lineX := bounds.X + (bounds.Width-len(hint))/2
		if lineX < bounds.X+1 {
			lineX = bounds.X + 1
		}
		screen.DrawText(lineX, midY, bounds.Width-1, hint, v.Theme.SubtleStyle())
		return
	}

	contentTop := bounds.Y + 2
	contentH := bounds.Height - 2
	if contentH < 1 {
		return
	}

	v.clampDetailScroll(contentH)

	for i := 0; i < contentH && v.DetailScroll+i < len(v.DetailLines); i++ {
		line := v.DetailLines[v.DetailScroll+i]
		screen.DrawText(bounds.X+2, contentTop+i, bounds.Width-3, line, v.Theme.BaseStyle())
	}
}

// HandleEvent processes keyboard input for the Agents tab.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	if keyEv.Key() == tcell.KeyTab {
		if v.Focus == FocusList {
			v.Focus = FocusDetail
		} else {
			v.Focus = FocusList
		}
		return true
	}

	switch v.Focus {
	case FocusList:
		return v.handleListKey(keyEv)
	case FocusDetail:
		return v.handleDetailKey(keyEv)
	}
	return false
}

func (v *View) handleListKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		if v.Selected > 0 {
			v.Selected--
			v.refreshDetail()
		}
		return true
	case tcell.KeyDown:
		if v.Selected < len(v.Agents)-1 {
			v.Selected++
			v.refreshDetail()
		}
		return true
	case tcell.KeyEnter:
		v.refreshDetail()
		v.Focus = FocusDetail
		return true
	}
	return false
}

func (v *View) handleDetailKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		if v.DetailScroll > 0 {
			v.DetailScroll--
		}
		return true
	case tcell.KeyDown:
		v.DetailScroll++
		return true
	case tcell.KeyPgUp:
		v.DetailScroll -= 10
		if v.DetailScroll < 0 {
			v.DetailScroll = 0
		}
		return true
	case tcell.KeyPgDn:
		v.DetailScroll += 10
		return true
	case tcell.KeyHome:
		v.DetailScroll = 0
		return true
	case tcell.KeyEsc:
		v.Focus = FocusList
		return true
	}
	return false
}

func (v *View) refreshDetail() {
	if v.Selected < 0 || v.Selected >= len(v.Agents) {
		v.DetailLines = nil
		v.DetailScroll = 0
		return
	}
	block := explorer.RenderAgent(v.Agents[v.Selected])
	v.DetailLines = strings.Split(strings.TrimRight(block, "\n"), "\n")
	v.DetailScroll = 0
}

func (v *View) clampListScroll(visibleRows int) {
	if v.Selected < v.ListScrollY {
		v.ListScrollY = v.Selected
	}
	if v.Selected >= v.ListScrollY+visibleRows {
		v.ListScrollY = v.Selected - visibleRows + 1
	}
	if v.ListScrollY < 0 {
		v.ListScrollY = 0
	}
}

func (v *View) clampDetailScroll(visibleRows int) {
	maxScroll := len(v.DetailLines) - visibleRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	if v.DetailScroll > maxScroll {
		v.DetailScroll = maxScroll
	}
	if v.DetailScroll < 0 {
		v.DetailScroll = 0
	}
}

func displayName(a explorer.AgentEntry) string {
	if a.Name != "" {
		return a.Name
	}
	if a.RoleSlug != "" {
		return a.RoleSlug
	}
	return a.ID
}

func footerCount(sel, total int) string {
	if total == 0 {
		return ""
	}
	return fmt.Sprintf(" %d/%d ", sel+1, total)
}
