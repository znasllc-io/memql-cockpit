package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// capabilitySchemaVersion is the wire-shape version of the
// capability descriptor. Bump when the JSON shape changes in a way
// consumers must branch on; additive fields don't need a bump.
const capabilitySchemaVersion = 1

// computerCapabilitiesAction is the one workerComputer action that
// works on EVERY build variant. The dispatcher routes it before the
// per-build dispatchComputer so a headless binary answers honestly
// (guiAvailable=false, actions=[]) instead of returning
// gui_unavailable. It's the agent runtime's introspection path:
// "what can this worker actually do?" must never itself fail.
const computerCapabilitiesAction = "capabilities"

// computerWaitAction is the build-agnostic pacing primitive
// (memql-cockpit#166): a bounded sleep that needs no GUI backend.
// Like `capabilities` it is routed by the dispatcher BEFORE the
// per-build router so it works identically on headless and gui
// builds -- see wait.go.
const computerWaitAction = "wait"

// buildAgnosticComputerActions lists workerComputer actions that are
// dispatchable on EVERY build variant and therefore belong in the
// descriptor's Actions list on both builds. Decision
// (memql-cockpit#166): Actions means "dispatchable actions", so
// `wait` is advertised even when guiAvailable=false -- a headless
// worker really can serve it. The meta `capabilities` action stays
// out of the list (it is the introspection channel itself and is
// unconditionally available; advertising it would be circular).
var buildAgnosticComputerActions = []string{computerWaitAction}

// CapabilityDescriptor is the worker's computer-use capability
// self-description (memql-cockpit#162). Computed from the build tag
// (gui vs headless) plus the host environment at call time.
//
// Actions lists the workerComputer actions this build can dispatch:
// the GUI-backed set (derived from the same table the GUI action
// router uses, so it can never drift from reality) plus the
// build-agnostic set (`wait`, memql-cockpit#166) that works on every
// variant -- so a headless build advertises ["wait"], not []. The
// `capabilities` action itself is deliberately NOT listed -- it is
// available on every build unconditionally.
//
// GUIAvailable reports whether the binary was built with the gui
// tag. DisplayServer reports what the environment looks like at
// call time ("none" when no display is reachable); consumers that
// want "can I actually screenshot right now" should check both.
//
// Displays is the number of attached displays at call time
// (memql-cockpit#165): 0 on headless builds and when no display
// server is reachable, at least 1 otherwise. An additive field --
// schemaVersion stays 1.
type CapabilityDescriptor struct {
	Platform      string   `json:"platform"`
	DisplayServer string   `json:"displayServer"`
	GUIAvailable  bool     `json:"guiAvailable"`
	Actions       []string `json:"actions"`
	Displays      int      `json:"displays"`
	SchemaVersion int      `json:"schemaVersion"`
}

// ComputeCapabilities builds the descriptor for the running binary
// and host. The build-variant bits (buildHasGUI +
// supportedComputerActions) come from capabilities_headless.go /
// capabilities_gui.go.
func ComputeCapabilities() CapabilityDescriptor {
	return computeCapabilities(runtime.GOOS, os.Getenv)
}

// computeCapabilities is the env-injectable core of
// ComputeCapabilities so tests can table-drive the display-server
// detection without mutating the process environment.
func computeCapabilities(goos string, getenv func(string) string) CapabilityDescriptor {
	// Dispatchable actions = the per-build GUI router table plus the
	// build-agnostic set (`wait`). Merged + sorted for a stable wire
	// shape; the slices never overlap (the router table is GUI-only,
	// the agnostic set is routed before the router).
	actions := append(supportedComputerActions(), buildAgnosticComputerActions...)
	sort.Strings(actions)
	if actions == nil {
		// Keep the JSON shape stable: "actions": [] -- never null.
		actions = []string{}
	}
	displayServer := "none"
	if buildHasGUI {
		displayServer = detectDisplayServer(goos, getenv)
	}
	// Probe the display count only when a RobotGo-drivable display
	// server is actually reachable: the probe touches the display
	// server, and capabilities must never fail -- not even on a gui
	// build running headless or under Wayland.
	displays := 0
	if displayServer == "quartz" || displayServer == "x11" {
		displays = displayCount()
	}
	return CapabilityDescriptor{
		Platform:      goos,
		DisplayServer: displayServer,
		GUIAvailable:  buildHasGUI,
		Actions:       actions,
		Displays:      displays,
		SchemaVersion: capabilitySchemaVersion,
	}
}

// DetectDisplayServer reports the display server the live host
// session exposes ("quartz" / "wayland" / "x11" / "none"). Exported
// so the worker package can wire it as the Linux display-server
// preflight hook (memql-cockpit#164) and the setup wizard can print
// the same verdict the dispatcher will enforce -- one detection, no
// drift.
func DetectDisplayServer() string {
	return detectDisplayServer(runtime.GOOS, os.Getenv)
}

// detectDisplayServer names the display server the host environment
// exposes: "quartz" on macOS (always present), and on Linux
// "wayland" / "x11" / "none" from the session env vars. Everything
// else (windows, BSDs without the env vars) reports "none" until a
// dedicated probe lands.
func detectDisplayServer(goos string, getenv func(string) string) string {
	switch goos {
	case "darwin":
		return "quartz"
	case "linux":
		if getenv("WAYLAND_DISPLAY") != "" ||
			strings.EqualFold(strings.TrimSpace(getenv("XDG_SESSION_TYPE")), "wayland") {
			return "wayland"
		}
		if getenv("DISPLAY") != "" {
			return "x11"
		}
		return "none"
	}
	return "none"
}

// CapabilityDescriptorJSON marshals the live capability descriptor
// for the registration handshake (memql-cockpit#166): the worker
// copies it into Register.capability_descriptor_json so the server
// learns the action surface up front, without a round-trip through
// workerComputer.capabilities. Same source of truth as that action
// (ComputeCapabilities), so the two can never disagree.
func CapabilityDescriptorJSON() (string, error) {
	body, err := json.Marshal(ComputeCapabilities())
	if err != nil {
		return "", fmt.Errorf("marshal capability descriptor: %w", err)
	}
	return string(body), nil
}

// runComputerCapabilities serves workerComputer.capabilities on
// every build variant. Routed by the dispatcher BEFORE the per-build
// dispatchComputer so the headless build answers instead of
// rejecting with gui_unavailable.
func runComputerCapabilities() (*memqlv1.Success, *memqlv1.Failure) {
	desc := ComputeCapabilities()
	preview := fmt.Sprintf("platform=%s displayServer=%s guiAvailable=%t actions=%d displays=%d",
		desc.Platform, desc.DisplayServer, desc.GUIAvailable, len(desc.Actions), desc.Displays)
	return successComputerJSON(map[string]any{
		"platform":      desc.Platform,
		"displayServer": desc.DisplayServer,
		"guiAvailable":  desc.GUIAvailable,
		"actions":       desc.Actions,
		"displays":      desc.Displays,
		"schemaVersion": desc.SchemaVersion,
	}, preview, 0), nil
}

// successComputerJSON is the structured-success helper for
// workerComputer handlers. Lives here (untagged) because both build
// variants need it: the GUI handlers in computer_gui.go and the
// build-agnostic capabilities handler above.
func successComputerJSON(payload map[string]any, preview string, bytesOut int) *memqlv1.Success {
	body, _ := json.Marshal(payload)
	return &memqlv1.Success{
		ResultJson:    body,
		BytesOut:      uint64(bytesOut),
		OutputPreview: clampPreview(preview),
	}
}
