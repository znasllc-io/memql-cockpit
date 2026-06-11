//go:build !gui

package tools

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/worker/consent"
)

// fakeEnv returns a getenv func backed by the supplied map -- the
// env-injectable seam for detectDisplayServer / computeCapabilities
// so the tests never mutate the process environment.
func fakeEnv(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestComputeCapabilities_Headless(t *testing.T) {
	// This test file is !gui, so it always exercises the headless
	// variant: guiAvailable=false, empty actions, displayServer
	// "none" regardless of what the host env claims.
	desc := computeCapabilities("linux", fakeEnv(map[string]string{
		"DISPLAY":         ":0",
		"WAYLAND_DISPLAY": "wayland-0",
	}))
	if desc.GUIAvailable {
		t.Error("headless build must report guiAvailable=false")
	}
	if desc.DisplayServer != "none" {
		t.Errorf("headless build must report displayServer=none regardless of env; got %q", desc.DisplayServer)
	}
	if desc.Actions == nil {
		t.Error("actions must be an empty slice (marshals to []), not nil")
	}
	if len(desc.Actions) != 0 {
		t.Errorf("headless build must advertise no actions; got %v", desc.Actions)
	}
	if desc.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d, want 1", desc.SchemaVersion)
	}
	if desc.Platform != "linux" {
		t.Errorf("platform = %q, want %q", desc.Platform, "linux")
	}
}

func TestComputeCapabilities_PlatformFollowsRuntime(t *testing.T) {
	desc := ComputeCapabilities()
	if desc.Platform != runtime.GOOS {
		t.Errorf("platform = %q, want runtime.GOOS %q", desc.Platform, runtime.GOOS)
	}
}

func TestDetectDisplayServer(t *testing.T) {
	cases := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{"darwin is always quartz", "darwin", nil, "quartz"},
		{"darwin ignores DISPLAY", "darwin", map[string]string{"DISPLAY": ":0"}, "quartz"},
		{"linux wayland via WAYLAND_DISPLAY", "linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, "wayland"},
		{"linux wayland via XDG_SESSION_TYPE", "linux", map[string]string{"XDG_SESSION_TYPE": "wayland"}, "wayland"},
		{"linux wayland XDG case-insensitive", "linux", map[string]string{"XDG_SESSION_TYPE": "Wayland"}, "wayland"},
		{"linux wayland wins over x11", "linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"}, "wayland"},
		{"linux x11 via DISPLAY", "linux", map[string]string{"DISPLAY": ":0"}, "x11"},
		{"linux x11 with non-wayland session type", "linux", map[string]string{"XDG_SESSION_TYPE": "x11", "DISPLAY": ":1"}, "x11"},
		{"linux headless", "linux", nil, "none"},
		{"linux tty session without DISPLAY", "linux", map[string]string{"XDG_SESSION_TYPE": "tty"}, "none"},
		{"windows is none for now", "windows", nil, "none"},
		{"freebsd is none for now", "freebsd", map[string]string{"DISPLAY": ":0"}, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectDisplayServer(tc.goos, fakeEnv(tc.env))
			if got != tc.want {
				t.Errorf("detectDisplayServer(%q, %v) = %q, want %q", tc.goos, tc.env, got, tc.want)
			}
		})
	}
}

// TestDispatcher_CapabilitiesWorksHeadless is the core #162
// contract: workerComputer.capabilities must answer on the headless
// build instead of returning gui_unavailable -- it's the one action
// that must never fail.
func TestDispatcher_CapabilitiesWorksHeadless(t *testing.T) {
	gate := &fakeGate{decision: consent.Decision{Allowed: true, Reason: "consent granted"}}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), gate)

	dispatch := &memqlv1.ToolDispatch{
		Tool:     "workerComputer",
		Action:   "capabilities",
		ArgsJson: []byte(`{}`),
		CallId:   "caps-1",
	}
	success, failure := d.Dispatch(context.Background(), dispatch)
	if failure != nil {
		t.Fatalf("capabilities must never fail; got %s: %s", failure.GetErrorCode(), failure.GetErrorMessage())
	}
	if success == nil {
		t.Fatal("expected a success payload")
	}
	if gate.calls != 1 {
		t.Errorf("consent gate should be consulted exactly once; got %d", gate.calls)
	}

	var desc CapabilityDescriptor
	if err := json.Unmarshal(success.GetResultJson(), &desc); err != nil {
		t.Fatalf("result is not valid descriptor JSON: %v (raw: %s)", err, success.GetResultJson())
	}
	if desc.GUIAvailable {
		t.Error("headless dispatch must report guiAvailable=false")
	}
	if len(desc.Actions) != 0 {
		t.Errorf("headless dispatch must report empty actions; got %v", desc.Actions)
	}
	if desc.DisplayServer != "none" {
		t.Errorf("headless dispatch must report displayServer=none; got %q", desc.DisplayServer)
	}
	if desc.SchemaVersion != capabilitySchemaVersion {
		t.Errorf("schemaVersion = %d, want %d", desc.SchemaVersion, capabilitySchemaVersion)
	}
	if desc.Platform != runtime.GOOS {
		t.Errorf("platform = %q, want %q", desc.Platform, runtime.GOOS)
	}

	// The wire shape must carry "actions": [] (not null) so JSON
	// consumers can iterate without a nil check.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(success.GetResultJson(), &raw); err != nil {
		t.Fatalf("re-decode raw: %v", err)
	}
	if string(raw["actions"]) != "[]" {
		t.Errorf(`"actions" must marshal to []; got %s`, raw["actions"])
	}
}

// TestDispatcher_CapabilitiesGatedAsObserve confirms the call still
// routes through the consent gate (a denied gate denies it like any
// other call) and that the classifier files it as observe-class.
func TestDispatcher_CapabilitiesGatedAsObserve(t *testing.T) {
	if got := consent.Classify("workerComputer", "capabilities"); got != consent.ClassObserve {
		t.Errorf("Classify(workerComputer, capabilities) = %q, want %q", got, consent.ClassObserve)
	}

	gate := &fakeGate{decision: consent.Decision{Allowed: false, Reason: "no window open"}}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), gate)
	dispatch := &memqlv1.ToolDispatch{
		Tool:     "workerComputer",
		Action:   "capabilities",
		ArgsJson: []byte(`{}`),
		CallId:   "caps-2",
	}
	_, failure := d.Dispatch(context.Background(), dispatch)
	if failure == nil || failure.GetErrorCode() != "consent_required" {
		t.Fatalf("a denying gate must still gate capabilities; got %+v", failure)
	}
}

// TestDispatcher_OtherComputerActionsStillGuiUnavailable pins the
// headless build's pre-existing behavior for every NON-capabilities
// workerComputer action: gui_unavailable, not a silent success.
func TestDispatcher_OtherComputerActionsStillGuiUnavailable(t *testing.T) {
	d := NewDispatcher(quietLogger(), DefaultPolicy(), nil)
	for _, action := range []string{"screenshot", "mouse_click", "display_info"} {
		dispatch := &memqlv1.ToolDispatch{
			Tool:     "workerComputer",
			Action:   action,
			ArgsJson: []byte(`{}`),
			CallId:   "caps-3-" + action,
		}
		_, failure := d.Dispatch(context.Background(), dispatch)
		if failure == nil || failure.GetErrorCode() != "gui_unavailable" {
			t.Errorf("headless workerComputer.%s should be gui_unavailable; got %+v", action, failure)
		}
	}
}
