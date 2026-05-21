package ui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// makeFormSim sets up a SimulationScreen wrapped in *Screen for the
// Form widget. Width must be ≥ 14 (Form's hard floor); tests below
// pick 40 so the input box has comfortable inner space.
func makeFormSim(t *testing.T, w, h int) (*Screen, tcell.SimulationScreen) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	sim.SetSize(w, h)
	return NewScreenFromTcell(sim), sim
}

func formRow(sim tcell.SimulationScreen, y, w int) string {
	var b strings.Builder
	for x := 0; x < w; x++ {
		ch, _, _, _ := sim.GetContent(x, y)
		b.WriteRune(ch)
	}
	return b.String()
}

func keyRune(r rune) *tcell.EventKey {
	return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone)
}
func keyNamed(k tcell.Key) *tcell.EventKey {
	return tcell.NewEventKey(k, 0, tcell.ModNone)
}

// TestForm_DrawRendersLabelAndPlaceholder confirms a fresh form
// paints the label and the placeholder (since the unfocused empty
// field falls back to placeholder rendering).
func TestForm_DrawRendersLabelAndPlaceholder(t *testing.T) {
	screen, sim := makeFormSim(t, 40, 10)
	defer sim.Fini()

	form := &Form{
		Fields: []FormField{
			{Label: "Name", Placeholder: "my-cluster"},
			{Label: "Host", Placeholder: "host"},
		},
		Cursor: 0,
	}
	form.Draw(screen, Rect{X: 0, Y: 0, Width: 40, Height: 10}, DefaultTheme())
	sim.Sync()

	// First field is focused -> no placeholder (cursor is active);
	// second field is unfocused with empty value -> placeholder visible.
	wants := []string{"Name", "Host", "host"}
	for _, want := range wants {
		found := false
		for y := 0; y < 10; y++ {
			if strings.Contains(formRow(sim, y, 40), want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q somewhere in the form", want)
		}
	}
}

// TestForm_NavSkipsReadOnly: ↓ from a Text field over a ReadOnly
// lands on the next text field, not the read-only one.
func TestForm_NavSkipsReadOnly(t *testing.T) {
	form := &Form{
		Fields: []FormField{
			{Label: "A", Kind: FieldText},
			{Label: "B", Kind: FieldReadOnly, Value: "frozen"},
			{Label: "C", Kind: FieldText},
		},
	}
	consumed, submitted := form.HandleEvent(keyNamed(tcell.KeyDown))
	if !consumed || submitted {
		t.Fatalf("Down: consumed=%v submitted=%v want consumed=true submitted=false", consumed, submitted)
	}
	if form.Cursor != 2 {
		t.Errorf("Cursor after Down: got %d, want 2 (C)", form.Cursor)
	}
	// ↑ from C wraps back to A (skipping B again).
	form.HandleEvent(keyNamed(tcell.KeyUp))
	if form.Cursor != 0 {
		t.Errorf("Cursor after Up: got %d, want 0 (A)", form.Cursor)
	}
}

// TestForm_InitialCursorOnReadOnlyMovesForward: if the caller sets
// Cursor onto a ReadOnly field, the first event should clamp to the
// next editable one.
func TestForm_InitialCursorOnReadOnlyMovesForward(t *testing.T) {
	form := &Form{
		Fields: []FormField{
			{Label: "A", Kind: FieldReadOnly, Value: "frozen"},
			{Label: "B", Kind: FieldText},
		},
		Cursor: 0,
	}
	form.HandleEvent(keyRune('x'))
	if form.Cursor != 1 {
		t.Fatalf("Cursor: got %d, want 1 (B)", form.Cursor)
	}
	if form.Fields[1].Value != "x" {
		t.Errorf("Fields[1].Value: got %q, want %q", form.Fields[1].Value, "x")
	}
}

// TestForm_AppendAndBackspace covers the text-field happy path:
// type characters, backspace deletes the last one.
func TestForm_AppendAndBackspace(t *testing.T) {
	form := &Form{Fields: []FormField{{Label: "Name", Kind: FieldText}}}
	for _, r := range "abc" {
		consumed, submitted := form.HandleEvent(keyRune(r))
		if !consumed || submitted {
			t.Fatalf("rune %q: consumed=%v submitted=%v", r, consumed, submitted)
		}
	}
	if form.Fields[0].Value != "abc" {
		t.Fatalf("Value after typing: got %q, want %q", form.Fields[0].Value, "abc")
	}
	form.HandleEvent(keyNamed(tcell.KeyBackspace))
	if form.Fields[0].Value != "ab" {
		t.Errorf("Value after Backspace: got %q, want %q", form.Fields[0].Value, "ab")
	}
	form.HandleEvent(keyNamed(tcell.KeyBackspace2))
	if form.Fields[0].Value != "a" {
		t.Errorf("Value after Backspace2: got %q, want %q", form.Fields[0].Value, "a")
	}
}

// TestForm_AllowFilterDrops verifies a caller-supplied Allow filter
// silently drops disallowed runes (return value is not consumed,
// state stays untouched).
func TestForm_AllowFilterDrops(t *testing.T) {
	form := &Form{Fields: []FormField{{
		Label: "Name",
		Kind:  FieldText,
		Allow: func(r rune) bool { return r >= 'a' && r <= 'z' },
	}}}
	consumed, _ := form.HandleEvent(keyRune('A')) // uppercase -> rejected
	if consumed {
		t.Errorf("uppercase 'A' should not be consumed when Allow rejects it")
	}
	if form.Fields[0].Value != "" {
		t.Errorf("Value after rejected rune: got %q, want empty", form.Fields[0].Value)
	}
	form.HandleEvent(keyRune('a'))
	if form.Fields[0].Value != "a" {
		t.Errorf("Value after accepted rune: got %q, want %q", form.Fields[0].Value, "a")
	}
}

// TestForm_DefaultFiltersByKind: FieldNumber accepts digits only,
// FieldURL accepts non-whitespace printables.
func TestForm_DefaultFiltersByKind(t *testing.T) {
	form := &Form{Fields: []FormField{
		{Label: "Port", Kind: FieldNumber},
		{Label: "URL", Kind: FieldURL},
	}}
	// Number rejects letters.
	form.HandleEvent(keyRune('1'))
	form.HandleEvent(keyRune('a'))
	form.HandleEvent(keyRune('2'))
	if form.Fields[0].Value != "12" {
		t.Errorf("Number value: got %q, want %q", form.Fields[0].Value, "12")
	}
	// Move to URL field. ↓ should land on Fields[1] (not read-only).
	form.HandleEvent(keyNamed(tcell.KeyDown))
	if form.Cursor != 1 {
		t.Fatalf("Cursor after Down: got %d, want 1", form.Cursor)
	}
	// URL rejects space (whitespace) but accepts ':' and '/'.
	form.HandleEvent(keyRune('h'))
	consumed, _ := form.HandleEvent(keyRune(' ')) // space -> URL rejects (cycle path returns false here too because it's text-kind)
	if consumed {
		t.Errorf("URL space should not be consumed")
	}
	form.HandleEvent(keyRune(':'))
	form.HandleEvent(keyRune('/'))
	if form.Fields[1].Value != "h:/" {
		t.Errorf("URL value: got %q, want %q", form.Fields[1].Value, "h:/")
	}
}

// TestForm_MaxLenCap rejects further input past the cap.
func TestForm_MaxLenCap(t *testing.T) {
	form := &Form{Fields: []FormField{{Label: "L", Kind: FieldText, MaxLen: 3}}}
	for _, r := range "abcde" {
		form.HandleEvent(keyRune(r))
	}
	if form.Fields[0].Value != "abc" {
		t.Errorf("Value at cap: got %q, want %q", form.Fields[0].Value, "abc")
	}
}

// TestForm_EnterSubmits: Enter returns (true, true) and does not
// latch -- a second Enter also returns (true, true).
func TestForm_EnterSubmits(t *testing.T) {
	form := &Form{Fields: []FormField{{Label: "L", Kind: FieldText, Value: "x"}}}
	consumed, submitted := form.HandleEvent(keyNamed(tcell.KeyEnter))
	if !consumed || !submitted {
		t.Fatalf("first Enter: consumed=%v submitted=%v want (true,true)", consumed, submitted)
	}
	consumed, submitted = form.HandleEvent(keyNamed(tcell.KeyEnter))
	if !consumed || !submitted {
		t.Errorf("second Enter: consumed=%v submitted=%v want (true,true)", consumed, submitted)
	}
}

// TestForm_ToggleCycles confirms Space + ←/→ flip a FieldToggle.
func TestForm_ToggleCycles(t *testing.T) {
	form := &Form{Fields: []FormField{{Label: "TLS", Kind: FieldToggle, Value: "off"}}}
	form.HandleEvent(keyRune(' '))
	if form.Fields[0].Value != "on" {
		t.Errorf("after Space: %q, want %q", form.Fields[0].Value, "on")
	}
	form.HandleEvent(keyNamed(tcell.KeyLeft))
	if form.Fields[0].Value != "off" {
		t.Errorf("after Left: %q, want %q", form.Fields[0].Value, "off")
	}
	form.HandleEvent(keyNamed(tcell.KeyRight))
	if form.Fields[0].Value != "on" {
		t.Errorf("after Right: %q, want %q", form.Fields[0].Value, "on")
	}
}

// TestForm_ChoiceCycles walks through the Choices slice forward and
// backward, wrapping at the ends.
func TestForm_ChoiceCycles(t *testing.T) {
	form := &Form{Fields: []FormField{{
		Label:   "Mode",
		Kind:    FieldChoice,
		Choices: []string{"dev", "stage", "prod"},
		Value:   "dev",
	}}}
	form.HandleEvent(keyRune(' '))
	if form.Fields[0].Value != "stage" {
		t.Errorf("after Space: %q, want %q", form.Fields[0].Value, "stage")
	}
	form.HandleEvent(keyNamed(tcell.KeyRight))
	form.HandleEvent(keyNamed(tcell.KeyRight)) // wraps prod -> dev
	if form.Fields[0].Value != "dev" {
		t.Errorf("after two Rights: %q, want %q (wrap)", form.Fields[0].Value, "dev")
	}
	form.HandleEvent(keyNamed(tcell.KeyLeft)) // wraps dev -> prod
	if form.Fields[0].Value != "prod" {
		t.Errorf("after Left from dev: %q, want %q (wrap)", form.Fields[0].Value, "prod")
	}
}

// TestForm_ErrorRenders confirms the Error line appears below the
// fields in error style. We only assert text presence here -- style
// assertions are too brittle to keep stable.
func TestForm_ErrorRenders(t *testing.T) {
	screen, sim := makeFormSim(t, 40, 10)
	defer sim.Fini()

	form := &Form{
		Fields: []FormField{{Label: "Name", Kind: FieldText}},
		Error:  "name is required",
	}
	form.Draw(screen, Rect{X: 0, Y: 0, Width: 40, Height: 10}, DefaultTheme())
	sim.Sync()

	found := false
	for y := 0; y < 10; y++ {
		if strings.Contains(formRow(sim, y, 40), "name is required") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Error message not rendered")
	}
}

// TestForm_TextKindIgnoresLeftRightCycle confirms ←/→ do not corrupt
// a text field's value (they are reserved for Toggle/Choice).
func TestForm_TextKindIgnoresLeftRightCycle(t *testing.T) {
	form := &Form{Fields: []FormField{{Label: "L", Kind: FieldText, Value: "abc"}}}
	form.HandleEvent(keyNamed(tcell.KeyLeft))
	form.HandleEvent(keyNamed(tcell.KeyRight))
	if form.Fields[0].Value != "abc" {
		t.Errorf("text Value mutated by Left/Right: got %q, want %q", form.Fields[0].Value, "abc")
	}
}

// TestForm_AllReadOnlyDoesNotPanic ensures the clamp logic survives
// the degenerate "every field is read-only" shape (the form is a
// display widget in that case; input is a no-op).
func TestForm_AllReadOnlyDoesNotPanic(t *testing.T) {
	form := &Form{
		Fields: []FormField{
			{Label: "A", Kind: FieldReadOnly, Value: "x"},
			{Label: "B", Kind: FieldReadOnly, Value: "y"},
		},
	}
	form.HandleEvent(keyRune('a'))
	form.HandleEvent(keyNamed(tcell.KeyDown))
	form.HandleEvent(keyNamed(tcell.KeyEnter))
	if form.Fields[0].Value != "x" || form.Fields[1].Value != "y" {
		t.Errorf("read-only values mutated: %q / %q", form.Fields[0].Value, form.Fields[1].Value)
	}
}

// TestForm_NonKeyEventReturnsFalse: anything that isn't an EventKey
// should be (false, false) so the parent can route it elsewhere.
func TestForm_NonKeyEventReturnsFalse(t *testing.T) {
	form := &Form{Fields: []FormField{{Label: "L", Kind: FieldText}}}
	consumed, submitted := form.HandleEvent(tcell.NewEventResize(80, 24))
	if consumed || submitted {
		t.Errorf("EventResize: got (%v, %v), want (false, false)", consumed, submitted)
	}
}
