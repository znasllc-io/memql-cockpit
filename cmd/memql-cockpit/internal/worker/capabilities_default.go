//go:build !gui

package worker

// capabilitiesForBuildTagImpl returns the cockpit-cockpit-worker's
// advertised capability set on the headless build. HEADLESS is
// mandatory; GUI is added only by the gui-tagged sibling.
func capabilitiesForBuildTagImpl() []string {
	return []string{"HEADLESS"}
}

// BuildHasGUI reports whether this binary was built with the gui
// tag. The wizard uses this to decide whether to run the TCC
// pre-flight (no-op on headless builds since there's nothing to
// probe).
func BuildHasGUI() bool { return false }
