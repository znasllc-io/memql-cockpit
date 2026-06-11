//go:build !gui

package tools

// buildHasGUI reports whether this binary was built with the gui
// tag. The headless build has no RobotGo backend, so the capability
// descriptor reports guiAvailable=false and an empty action list
// regardless of what the host environment looks like.
const buildHasGUI = false

// supportedComputerActions on the headless build is empty: no
// workerComputer GUI action can be dispatched (computer.go rejects
// them all with gui_unavailable). The `capabilities` action itself
// still works -- it's routed by the dispatcher before the per-build
// router and is deliberately not part of this list.
func supportedComputerActions() []string {
	return []string{}
}

// displayCount on the headless build is 0: there is no RobotGo
// backend to probe displays with, and the descriptor's displayServer
// is always "none" here so computeCapabilities never reaches this --
// it exists so both build variants satisfy the same contract.
func displayCount() int {
	return 0
}
