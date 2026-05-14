//go:build gui

package worker

// capabilitiesForBuildTagImpl on the gui build advertises HEADLESS
// + GUI. The cockpit's actual ability to deliver GUI tools depends
// on TCC permissions / X11 display reachability at register time,
// which the wizard probes; this list is just what the binary is
// CAPABLE of when permissions allow.
func capabilitiesForBuildTagImpl() []string {
	return []string{"HEADLESS", "GUI"}
}

// BuildHasGUI reports whether this binary was built with the gui
// tag. The wizard uses this to decide whether to run the TCC
// pre-flight.
func BuildHasGUI() bool { return true }
