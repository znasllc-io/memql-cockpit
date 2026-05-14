package ui

import "github.com/gdamore/tcell/v2"

// Header renders the 1-row chrome at the top of the screen. It mirrors
// the TabBar's dark band so the top + bottom of the window form a
// matched frame around the tab content.
//
// Left side:  app branding ("memQL Cockpit").
// Right side: a single short hint about Tab navigating between panes.
//             Tab is universal across every tab content view, so the
//             hint lives in the global chrome instead of being repeated
//             in each per-pane footer (and saves a footer row in the
//             tabs that previously had to dedicate one to "Tab:...").
//
// Tab-switching shortcuts (F1..F4 / Alt+1..4) still live in the bottom
// TabBar footer so the keys cluster physically near the tabs they pick.
type Header struct {
	Theme         Theme
	Title         string         // app branding shown on the left
	NavHint       string         // short universal hint shown on the right
	Notifications *Notifications // optional center-of-header message feed
}

// NewHeader builds the default header. Call once at startup.
func NewHeader(theme Theme, notifications *Notifications) *Header {
	return &Header{
		Theme:         theme,
		Title:         "memQL Cockpit",
		NavHint:       "Tab:Switch Panes",
		Notifications: notifications,
	}
}

// Draw paints the header into the given row, spanning width.
func (h *Header) Draw(screen *Screen, y, width int) {
	barBG := tcell.NewRGBColor(18, 18, 22)

	// Clear the row with the chrome background.
	screen.FillRect(0, y, width, 1, StyleDefaultBG(barBG))

	// Left: app branding. Plain bold FG -- accent stays reserved for
	// the active tab and pane-focus indicators in the content area.
	titleStyle := tcell.StyleDefault.Foreground(h.Theme.FG).Background(barBG).Bold(true)
	screen.DrawText(1, y, width-2, h.Title, titleStyle)

	// Right: navigation hint, right-justified one column from the edge
	// (mirrors the F1..F4 hint's placement in the footer).
	hintStart := width
	if h.NavHint != "" && width >= len(h.NavHint)+len(h.Title)+4 {
		hintStyle := tcell.StyleDefault.Foreground(h.Theme.Subtle).Background(barBG)
		hintStart = width - len(h.NavHint) - 1
		screen.DrawText(hintStart, y, len(h.NavHint), h.NavHint, hintStyle)
	}

	// Middle: notifications feed. Lives in the gap between the title
	// and the nav hint. Keeping messages up here means transient state
	// (reconnect attempts, warnings, info) never shoves tab content
	// around -- the header row always stays the same height.
	if h.Notifications != nil {
		// Leave 3 blank columns of padding on each side of the slot so
		// the notification isn't pinned against the title / hint text.
		slotX := 1 + len(h.Title) + 3
		slotEnd := hintStart - 3
		slotW := slotEnd - slotX
		if slotW > 0 {
			h.Notifications.Render(screen, h.Theme, slotX, y, slotW)
		}
	}
}

// StyleDefaultBG is a tiny convenience for "no foreground change, just
// paint this background" -- used when filling the chrome row.
func StyleDefaultBG(bg tcell.Color) tcell.Style {
	return tcell.StyleDefault.Background(bg)
}
