//go:build gui

package tools

import (
	"sort"
	"testing"
)

// TestComputeCapabilities_GUI pins the gui-build variant: the
// descriptor advertises exactly the actions the router serves (the
// computerActionHandlers table) -- including window_list /
// window_focus since memql-cockpit#167 -- guiAvailable=true, and
// the meta `capabilities` action stays out of the list. CI runs
// headless so this only executes on local gui-tagged runs, but it
// keeps the variant honest for anyone building -tags gui.
func TestComputeCapabilities_GUI(t *testing.T) {
	desc := computeCapabilities("darwin", func(string) string { return "" })
	if !desc.GUIAvailable {
		t.Error("gui build must report guiAvailable=true")
	}
	if desc.DisplayServer != "quartz" {
		t.Errorf("darwin gui build must report displayServer=quartz; got %q", desc.DisplayServer)
	}
	if !sort.StringsAreSorted(desc.Actions) {
		t.Errorf("actions must be sorted for a stable wire shape; got %v", desc.Actions)
	}
	if len(desc.Actions) != len(computerActionHandlers) {
		t.Errorf("actions length %d must match the router table %d", len(desc.Actions), len(computerActionHandlers))
	}
	for _, a := range desc.Actions {
		if _, ok := computerActionHandlers[a]; !ok {
			t.Errorf("advertised action %q is not in the router table", a)
		}
	}
	for _, a := range desc.Actions {
		if a == "capabilities" {
			t.Error(`"capabilities" must not be advertised in actions (it is build-agnostic and always available)`)
		}
	}
	// window_list / window_focus shipped for real in
	// memql-cockpit#167 -- the gui build must advertise them.
	for _, required := range []string{"window_list", "window_focus"} {
		found := false
		for _, a := range desc.Actions {
			if a == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q must be advertised in actions; got %v", required, desc.Actions)
		}
	}
}
