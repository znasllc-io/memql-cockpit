// Package runlocal is a placeholder wizard slot for the future
// "Run local cluster" flow -- a dependency-check + spin-up walkthrough
// for operators who want to host their own cluster on this machine.
//
// Today it just renders a "coming soon" panel so the splash option
// has somewhere to land. The real wizard will live here without a
// rename.
package runlocal

import (
	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// Run blocks polling events on screen until the user dismisses the
// placeholder. Returns when the user presses Esc / Enter / Ctrl+Q.
func Run(screen *ui.Screen, theme ui.Theme) {
	for {
		draw(screen, theme)
		ev := screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventInterrupt:
		case *tcell.EventResize:
			screen.Sync()
		case *tcell.EventKey:
			if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
				return
			}
			if ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyEnter {
				return
			}
		}
	}
}

func draw(screen *ui.Screen, theme ui.Theme) {
	screen.Clear(theme.BaseStyle())
	sw, sh := screen.Size()

	panelW, panelH := 64, 12
	if panelW > sw-4 {
		panelW = sw - 4
	}
	if panelH > sh-4 {
		panelH = sh - 4
	}
	px := (sw - panelW) / 2
	py := (sh - panelH) / 2
	screen.DrawBox(px, py, panelW, panelH, theme.SubtleStyle())

	title := " Run local cluster "
	screen.DrawText(px+(panelW-len(title))/2, py+1, len(title), title, theme.AccentStyle().Bold(true))

	lines := []string{
		"Coming soon.",
		"",
		"This wizard will check the dependencies a local memQL cluster",
		"needs (docker, mkcert, ports, etc.), then bring the stack up",
		"and seed it from your genesis.znas.",
		"",
		"For now, follow the manual instructions in the memql repo.",
	}
	for i, ln := range lines {
		screen.DrawText(px+4, py+3+i, panelW-8, ln, theme.BaseStyle())
	}

	hint := "Enter / Esc -- back"
	screen.DrawText(px+(panelW-len(hint))/2, py+panelH-2, len(hint), hint, theme.SubtleStyle())

	screen.Show()
}
