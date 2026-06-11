//go:build gui

package tools

import "testing"

// TestDispatcher_MouseClickRoutesThroughAllowsAt_GUI is the gui half
// of the build-tag split from memql-cockpit#171. On the gui build
// cursorLocation() reads the live cursor via robotgo.Location() and
// always reports Known=true, so the dispatcher must hand a known
// position to AllowsAt for the strict-mode region exemption
// (memql-cockpit#131).
//
// Safe on a real desktop: the recording gate DENIES, so the
// mouse_click handler never runs -- the test only reads the cursor
// position, it never moves it or clicks.
func TestDispatcher_MouseClickRoutesThroughAllowsAt_GUI(t *testing.T) {
	cursor := mouseClickAllowsAtCursor(t)
	if !cursor.Known {
		t.Errorf("gui build must resolve the live cursor (Known=true); got %+v", cursor)
	}
}
