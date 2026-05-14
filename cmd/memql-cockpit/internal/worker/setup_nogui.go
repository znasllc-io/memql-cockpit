//go:build !gui

package worker

import "fmt"

// runSetupWizard on the default (non-gui) build prints the
// guidance message that this binary cannot enable GUI capability.
// The gui-tagged sibling (setup_gui.go) drives the real wizard.
func runSetupWizard() error {
	fmt.Println("memql-cockpit-worker setup (headless build)")
	fmt.Println()
	fmt.Println("This binary was built without the GUI tag, so it can")
	fmt.Println("only register HEADLESS capability. There is nothing to")
	fmt.Println("set up beyond writing ~/.memql/worker.yaml with a token.")
	fmt.Println()
	fmt.Println("To enable GUI tools (mouse / keyboard / screenshot),")
	fmt.Println("install memql-cockpit-gui:")
	fmt.Println()
	fmt.Println("  make cockpit-gui          (host build)")
	fmt.Println("  make cockpit-gui-darwin-arm64  (cross-platform)")
	fmt.Println()
	fmt.Println("Then run `memql-cockpit-gui worker setup` for the")
	fmt.Println("interactive macOS TCC / Linux X11 permissions wizard.")
	return nil
}
