//go:build gui

package tools

import "sort"

// buildHasGUI reports whether this binary was built with the gui
// tag. True here: the gui-tagged build carries the RobotGo backend.
const buildHasGUI = true

// supportedComputerActions derives the advertised action list from
// computerActionHandlers -- the exact routing table dispatchComputer
// uses (computer_gui.go) -- so the descriptor can never drift from
// what the router actually serves. Sorted for a stable wire shape.
//
// window_list / window_focus are advertised since memql-cockpit#167
// shipped real implementations (macOS CGWindowList, Linux/X11 EWMH).
// On a Wayland session they return a structured
// unsupported_on_platform failure at call time -- the descriptor's
// displayServer field is the up-front signal for that.
func supportedComputerActions() []string {
	out := make([]string, 0, len(computerActionHandlers))
	for action := range computerActionHandlers {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}
