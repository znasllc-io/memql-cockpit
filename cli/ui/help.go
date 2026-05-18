package ui

import "github.com/gdamore/tcell/v2"

// HelpOverlay renders a centered keyboard shortcut reference overlay.
type HelpOverlay struct {
	Visible bool
	Theme   Theme
}

// NewHelpOverlay creates a hidden help overlay.
func NewHelpOverlay(theme Theme) *HelpOverlay {
	return &HelpOverlay{Theme: theme}
}

// Toggle shows or hides the overlay.
func (h *HelpOverlay) Toggle() {
	h.Visible = !h.Visible
}

// Draw renders the help overlay centered on screen.
func (h *HelpOverlay) Draw(screen *Screen, bounds Rect) {
	if !h.Visible {
		return
	}

	// Semi-transparent background (dim the content behind).
	dimStyle := tcell.StyleDefault.Background(tcell.NewRGBColor(10, 10, 14)).Foreground(h.Theme.Subtle)
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, dimStyle)

	// Centered popup.
	popW := 60
	popH := 30
	if popW > bounds.Width-4 {
		popW = bounds.Width - 4
	}
	if popH > bounds.Height-4 {
		popH = bounds.Height - 4
	}
	popX := bounds.X + (bounds.Width-popW)/2
	popY := bounds.Y + (bounds.Height-popH)/2

	bgStyle := tcell.StyleDefault.Background(tcell.NewRGBColor(30, 33, 39)).Foreground(h.Theme.FG)
	screen.FillRect(popX, popY, popW, popH, bgStyle)
	screen.DrawBox(popX, popY, popW, popH, bgStyle.Foreground(h.Theme.Accent))

	// Title.
	title := " Keyboard Shortcuts "
	titleStyle := bgStyle.Foreground(h.Theme.Accent).Bold(true)
	screen.DrawText(popX+(popW-len(title))/2, popY, len(title), title, titleStyle)

	y := popY + 2
	x := popX + 3
	w := popW - 6

	alt := AltKey()
	entries := []helpEntry{
		{section: "TABS"},
		{key: "F1 / " + alt + "+1", desc: "Clusters"},
		{key: "F2 / " + alt + "+2", desc: "Concepts"},
		{key: "F3 / " + alt + "+3", desc: "Planner"},
		{key: "F4 / " + alt + "+4", desc: "Agents"},
		{key: "F5 / " + alt + "+5", desc: "Chat"},
		{key: "F6 / " + alt + "+6", desc: "Settings"},
		{},
		{section: "NAVIGATION"},
		{key: "Tab", desc: "Cycle focus between panes"},
		{key: "Ctrl+Q / Ctrl+C", desc: "Quit"},
		{key: "Escape", desc: "Close popup / cancel"},
		{},
		{section: "CLUSTERS"},
		{key: "Up / Down", desc: "Navigate cluster list"},
		{key: "Enter", desc: "Connect to selected cluster"},
		{key: "A / E / D", desc: "Add / Edit / Delete cluster"},
		{},
		{section: "TOPOLOGY"},
		{key: "W / A / S / D", desc: "Pan viewport"},
		{key: "R", desc: "Reset pan"},
		{},
		{section: "EXPLORER"},
		{key: "Up / Down", desc: "Navigate tree"},
		{key: "Enter", desc: "Open file / expand folder"},
		{key: "Ctrl+Space", desc: "Trigger completion"},
		{},
		{section: "EDITOR"},
		{key: "Arrows", desc: "Move cursor"},
		{key: "Home / End", desc: "Line start / end"},
		{key: "PgUp / PgDn", desc: "Scroll page"},
		{},
		{key: "Ctrl+?", desc: "Toggle this help"},
	}

	for _, e := range entries {
		if y >= popY+popH-2 {
			break
		}
		if e.section != "" {
			sectionStyle := bgStyle.Foreground(h.Theme.Subtle).Bold(true)
			screen.DrawText(x, y, w, e.section, sectionStyle)
			y++
			continue
		}
		if e.key == "" {
			y++ // blank spacer
			continue
		}
		keyStyle := bgStyle.Foreground(h.Theme.Annotation)
		screen.DrawText(x, y, 22, e.key, keyStyle)
		screen.DrawText(x+22, y, w-22, e.desc, bgStyle)
		y++
	}

	// Footer.
	footer := " Press Escape or Ctrl+? to close "
	footerStyle := bgStyle.Foreground(h.Theme.Subtle)
	screen.DrawText(popX+(popW-len(footer))/2, popY+popH-1, len(footer), footer, footerStyle)
}

type helpEntry struct {
	section string
	key     string
	desc    string
}
