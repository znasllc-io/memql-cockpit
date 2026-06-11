//go:build gui

package tools

import (
	"strings"
	"testing"
)

// These tests cover the pure (pre-RobotGo) validation paths of the
// memql-cockpit#166 handlers only -- nothing here posts real input.
// The post-validation paths drive the live cursor / keyboard and are
// exercised manually (and by the agent itself once the bff enum
// unlocks the actions).

func TestMouseButtonArg(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr bool
	}{
		{"absent defaults to left", map[string]any{}, "left", false},
		{"left", map[string]any{"button": "left"}, "left", false},
		{"right", map[string]any{"button": "right"}, "right", false},
		{"middle maps to robotgo center", map[string]any{"button": "middle"}, "center", false},
		{"center accepted as alias", map[string]any{"button": "center"}, "center", false},
		{"case + whitespace tolerant", map[string]any{"button": " Right "}, "right", false},
		// robotgo's CheckMouse silently falls back to LEFT_BUTTON on
		// unknown names -- we must reject instead of misclicking.
		{"unknown button rejected", map[string]any{"button": "back"}, "", true},
		{"wheel buttons rejected", map[string]any{"button": "wheelDown"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fail := mouseButtonArg(tc.args, "mouse_down")
			if tc.wantErr {
				if fail == nil || fail.GetErrorCode() != "bad_request" {
					t.Fatalf("expected bad_request; got button=%q fail=%+v", got, fail)
				}
				return
			}
			if fail != nil {
				t.Fatalf("unexpected failure: %+v", fail)
			}
			if got != tc.want {
				t.Errorf("button = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGuiKeyHold_RequiresKey(t *testing.T) {
	success, fail := guiKeyHold(map[string]any{"durationMs": float64(10)})
	if success != nil {
		t.Fatalf("key_hold without a key must not succeed; got %+v", success)
	}
	if fail == nil || fail.GetErrorCode() != "bad_request" {
		t.Fatalf("expected bad_request; got %+v", fail)
	}
	if !strings.Contains(fail.GetErrorMessage(), "key required") {
		t.Errorf("message should say the key is required; got %q", fail.GetErrorMessage())
	}
}

func TestGuiMouseDownUp_RejectBadButton(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(map[string]any) (errCode string)
	}{
		{"mouse_down", func(args map[string]any) string {
			_, fail := guiMouseDown(args)
			if fail == nil {
				return ""
			}
			return fail.GetErrorCode()
		}},
		{"mouse_up", func(args map[string]any) string {
			_, fail := guiMouseUp(args)
			if fail == nil {
				return ""
			}
			return fail.GetErrorCode()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := tc.call(map[string]any{"button": "nope"}); code != "bad_request" {
				t.Errorf("%s with an unknown button must be bad_request; got %q", tc.name, code)
			}
		})
	}
}
