// Package splash renders the launch splash screen -- the first
// surface a user sees when they launch memql-cockpit (after any
// pre-flight wizards have run). Single-panel layout with numbered
// options at the bottom; picking one returns the corresponding
// Choice and the caller mounts the chosen mode.
//
// The splash is launch-only: once the user picks, the main TUI takes
// over for the rest of the session. There's no "return to splash"
// key binding.
package splash

import (
	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// Choice is what the user selected on the splash screen.
type Choice int

const (
	ChoiceQuit              Choice = iota // Ctrl+Q / Ctrl+C / 'q'
	ChoiceOperatingConsole                // '1' -- open the multi-tab IDE
	ChoiceRunLocalCluster                 // '2' -- placeholder wizard for now
)

// Run blocks polling events from screen until the user picks an
// option (or quits). Returns the picked Choice.
func Run(screen *ui.Screen, theme ui.Theme) Choice {
	for {
		draw(screen, theme)
		ev := screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventInterrupt:
			// Spurious redraw triggers are fine -- the loop redraws
			// on every iteration anyway.
		case *tcell.EventResize:
			screen.Sync()
		case *tcell.EventKey:
			if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
				return ChoiceQuit
			}
			switch ev.Rune() {
			case '1':
				return ChoiceOperatingConsole
			case '2':
				return ChoiceRunLocalCluster
			case 'q', 'Q':
				return ChoiceQuit
			}
		}
	}
}

// draw paints the splash. Centered title block over a numbered
// option list. The visible labels are deliberately placeholders --
// the splash's role here is to establish the launch-time entry
// surface, not lock in final IA.
func draw(screen *ui.Screen, theme ui.Theme) {
	screen.Clear(theme.BaseStyle())
	w, h := screen.Size()

	panelW, panelH := 60, 14
	if panelW > w-4 {
		panelW = w - 4
	}
	if panelH > h-4 {
		panelH = h - 4
	}
	px := (w - panelW) / 2
	py := (h - panelH) / 2
	screen.DrawBox(px, py, panelW, panelH, theme.SubtleStyle())

	titleY := py + 2
	title := " memQL Cockpit "
	screen.DrawText(px+(panelW-len(title))/2, titleY, len(title), title, theme.AccentStyle().Bold(true))

	subtitle := "Choose an option to continue"
	screen.DrawText(px+(panelW-len(subtitle))/2, titleY+1, len(subtitle), subtitle, theme.SubtleStyle())

	options := []string{
		"  1   Open operating console",
		"  2   Set up local cluster   (coming soon)",
		"  Q   Quit",
	}
	optY := titleY + 4
	for i, opt := range options {
		screen.DrawText(px+4, optY+i, panelW-8, opt, theme.BaseStyle())
	}

	hint := "Press 1 / 2 / Q ..."
	screen.DrawText(px+(panelW-len(hint))/2, py+panelH-2, len(hint), hint, theme.SubtleStyle())

	screen.Show()
}
