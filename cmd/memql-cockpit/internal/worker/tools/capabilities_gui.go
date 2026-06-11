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
// window_list / window_focus are NOT handlers in that table today
// (they're explicit unsupported_on_platform stubs), so they are
// correctly absent here until they ship for real.
func supportedComputerActions() []string {
	out := make([]string, 0, len(computerActionHandlers))
	for action := range computerActionHandlers {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}
