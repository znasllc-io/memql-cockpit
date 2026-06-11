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
