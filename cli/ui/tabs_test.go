package ui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// makeTabBar builds a TabBar with n placeholder tabs. Contents are
// nil since these tests only exercise HandleKey / hintText, neither
// of which touches Tab.Content.
func makeTabBar(n int) *TabBar {
	tabs := make([]Tab, n)
	for i := range tabs {
		tabs[i] = Tab{Name: "t"}
	}
	return NewTabBar(DefaultTheme(), tabs...)
}

// TestHandleKey_AltDigit pins the Alt+1..Alt+9 dispatch behavior
// against the tab count. Regression coverage for the bug where the
// handler hardcoded Alt+1..Alt+4 -- adding the Agents + Chat tabs
// left Settings (then at index 5) unreachable from the keyboard.
func TestHandleKey_AltDigit(t *testing.T) {
	cases := []struct {
		name    string
		tabs    int
		key     rune
		wantIdx int
	}{
		{"alt+1 with 6 tabs -> 0", 6, '1', 0},
		{"alt+5 with 6 tabs -> 4", 6, '5', 4},
		{"alt+6 with 6 tabs -> 5", 6, '6', 5},
		{"alt+7 with 6 tabs -> -1 (out of range)", 6, '7', -1},
		{"alt+5 with 4 tabs -> -1 (out of range)", 4, '5', -1},
		{"alt+1 with 0 tabs -> -1", 0, '1', -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tb := makeTabBar(tc.tabs)
			ev := tcell.NewEventKey(tcell.KeyRune, tc.key, tcell.ModAlt)
			if got := tb.HandleKey(ev); got != tc.wantIdx {
				t.Errorf("HandleKey(Alt+%c) on %d-tab bar = %d, want %d", tc.key, tc.tabs, got, tc.wantIdx)
			}
		})
	}
}

// TestHandleKey_FKey pins F1..F12 mapping and the per-tab-count
// upper bound. Same regression as TestHandleKey_AltDigit but along
// the F-key axis.
func TestHandleKey_FKey(t *testing.T) {
	cases := []struct {
		name    string
		tabs    int
		key     tcell.Key
		wantIdx int
	}{
		{"F1 with 6 tabs -> 0", 6, tcell.KeyF1, 0},
		{"F5 with 6 tabs -> 4", 6, tcell.KeyF5, 4},
		{"F6 with 6 tabs -> 5", 6, tcell.KeyF6, 5},
		{"F7 with 6 tabs -> -1 (out of range)", 6, tcell.KeyF7, -1},
		{"F5 with 4 tabs -> -1 (out of range)", 4, tcell.KeyF5, -1},
		{"F12 with 12 tabs -> 11", 12, tcell.KeyF12, 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tb := makeTabBar(tc.tabs)
			ev := tcell.NewEventKey(tc.key, 0, tcell.ModNone)
			if got := tb.HandleKey(ev); got != tc.wantIdx {
				t.Errorf("HandleKey(%v) on %d-tab bar = %d, want %d", tc.key, tc.tabs, got, tc.wantIdx)
			}
		})
	}
}

// TestHandleKey_OtherKeysReturnMinusOne keeps non-tab keys from
// being misinterpreted as tab switches.
func TestHandleKey_OtherKeysReturnMinusOne(t *testing.T) {
	tb := makeTabBar(6)
	cases := []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyRune, 'a', tcell.ModNone),
		tcell.NewEventKey(tcell.KeyRune, '1', tcell.ModNone), // 1 without Alt is a regular keystroke
		tcell.NewEventKey(tcell.KeyCtrlQ, 0, tcell.ModCtrl),
		tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone),
	}
	for _, ev := range cases {
		if got := tb.HandleKey(ev); got != -1 {
			t.Errorf("HandleKey(%v) should not match a tab; got %d", ev, got)
		}
	}
}

// TestHintText_ReflectsTabCount makes sure the chrome-band hint
// advertises the real range. The hardcoded "F1..F4" string was the
// user-facing symptom of the original bug -- the keyboard worked
// for some tabs but the UI lied about which ones.
func TestHintText_ReflectsTabCount(t *testing.T) {
	cases := []struct {
		tabs       int
		wantFRange string
		wantAltMax string
	}{
		{1, "F1 ", "+1:Tab"},
		{4, "F1..F4", "+1..4:Tabs"},
		{6, "F1..F6", "+1..6:Tabs"},
		{9, "F1..F9", "+1..9:Tabs"},
		{12, "F1..F12", "+1..9:Tabs"}, // Alt range caps at 9; F range goes to 12
	}
	for _, tc := range cases {
		got := makeTabBar(tc.tabs).hintText()
		if !strings.Contains(got, tc.wantFRange) {
			t.Errorf("hintText for %d tabs = %q, missing F range %q", tc.tabs, got, tc.wantFRange)
		}
		if !strings.Contains(got, tc.wantAltMax) {
			t.Errorf("hintText for %d tabs = %q, missing Alt range %q", tc.tabs, got, tc.wantAltMax)
		}
	}
}
