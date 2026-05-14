package ui

import "runtime"

// AltKey returns the display label for the "alt" modifier on the current
// operating system. macOS calls that key "Option" (⌥); Linux, Windows,
// and other platforms call it "Alt". Display-only: the tcell key constant
// stays the same either way.
func AltKey() string {
	if runtime.GOOS == "darwin" {
		return "Option"
	}
	return "Alt"
}

// CtrlKey returns the display label for the "ctrl" modifier. Always
// "Ctrl" today; exposed as a function so future platform differences
// (e.g. mapping to Cmd on macOS) can be routed through a single place.
func CtrlKey() string {
	return "Ctrl"
}
