package ui

import (
	"github.com/gdamore/tcell/v2"
)

// FieldKind enumerates the input types a FormField can take. The
// kind drives input handling (which runes are accepted by default,
// which keys mutate the value) and rendering (boxed input vs. inline
// toggle vs. dimmed read-only).
type FieldKind int

const (
	// FieldText accepts any printable rune by default. The caller
	// can narrow the alphabet via FormField.Allow.
	FieldText FieldKind = iota

	// FieldURL accepts any printable non-whitespace rune by default.
	// The caller still chooses whether to enforce shape via Validate.
	FieldURL

	// FieldNumber accepts ASCII digits only by default.
	FieldNumber

	// FieldToggle is a two-state field. Value is "on" or "off";
	// Space / ← / → cycle. The field has no text cursor.
	FieldToggle

	// FieldChoice is an N-state field. Value matches one of Choices;
	// Space / → advance, ← retreats. The field has no text cursor.
	FieldChoice

	// FieldReadOnly displays Value in subtle style. ↑/↓ navigation
	// skips it; no input is accepted.
	FieldReadOnly
)

// FormField is one row in a Form. The zero value is a usable
// FieldText with an empty label and no constraints.
type FormField struct {
	// Label is the left-aligned identifier rendered alongside the
	// input. Kept short (≤ 10 chars renders cleanly in the default
	// layout); longer labels are truncated.
	Label string

	// Placeholder renders inside the input box in subtle style when
	// Value is empty and the field is not focused. Ignored for
	// FieldToggle / FieldChoice / FieldReadOnly.
	Placeholder string

	// Kind drives input semantics. Defaults to FieldText (the zero
	// value of FieldKind).
	Kind FieldKind

	// Value is the current value. For FieldToggle it is "on" / "off"
	// (empty defaults to "off" at render time). For FieldChoice it
	// matches one of Choices (empty defaults to Choices[0] when
	// Choices is non-empty).
	Value string

	// Choices is the ordered set of legal values for FieldChoice.
	// Ignored for other kinds.
	Choices []string

	// Validate is an optional per-field validator invoked by the
	// caller (the Form itself never calls it -- validation runs at
	// submit time on the caller side so it can layer cross-field
	// rules on top).
	Validate func(value string) error

	// Allow is an optional rune filter for text-style fields. When
	// nil, the per-kind default filter (see allowedRune) applies.
	// A rune that fails Allow is silently dropped at keystroke time.
	Allow func(r rune) bool

	// MaxLen caps the input length (in runes) for text-style fields.
	// Zero means unbounded. Ignored for FieldToggle / FieldChoice /
	// FieldReadOnly.
	MaxLen int
}

// Form is a reusable Add/Edit widget. The parent owns the data --
// Form just renders Fields[*].Value, mutates them in response to
// keys, and reports submission via HandleEvent's second return.
//
// Form is intentionally small: no built-in validation, no
// multi-step wizard, no scrolling. The cluster Add/Edit form in
// cli/cluster/clusters_view.go is the canonical caller; that
// migration is tracked separately (issue #92 follow-up) so the
// Discovery-URL-priority branch can be kept intact.
type Form struct {
	Fields []FormField

	// Cursor is the index of the focused field in Fields. The
	// caller initializes it; Form clamps it to the first non-
	// FieldReadOnly field on first event when the initial Cursor
	// lands on a read-only row.
	Cursor int

	// Error is set externally to surface a save-time error inline
	// below the last field. The Form itself never writes to Error;
	// it only renders it.
	Error string
}

// Draw paints the form into bounds. Each field consumes two rows
// (label + boxed input on row N, blank breather on row N+1). The
// error line, when set, renders after the last field.
//
// Bounds with Width < 14 or Height < 1 are no-ops -- the layout
// reserves 11 cells for the label column and at least 3 cells for
// the input box.
func (f *Form) Draw(screen *Screen, bounds Rect, theme Theme) {
	if screen == nil || bounds.Width < 14 || bounds.Height < 1 {
		return
	}
	f.clampCursor()

	x := bounds.X
	y := bounds.Y + 1 // one row of breathing room before the first field
	maxW := bounds.Width

	for i := range f.Fields {
		if y >= bounds.Y+bounds.Height {
			return
		}
		f.drawField(screen, theme, i, x, y, maxW)
		y += 2
	}

	if f.Error == "" || y >= bounds.Y+bounds.Height {
		return
	}
	errStyle := theme.ErrorStyle()
	for _, ln := range WrapText(f.Error, maxW) {
		if y >= bounds.Y+bounds.Height {
			return
		}
		screen.DrawText(x, y, maxW, ln, errStyle)
		y++
	}
}

// drawField paints one field row. Layout:
//
//	x+1   label (up to 10 cells)
//	x+12  input box (up to maxW-13 cells, capped at 30)
//
// FieldReadOnly skips the box and renders Value in subtle style
// alongside a subtle label so the row visually recedes.
func (f *Form) drawField(screen *Screen, theme Theme, idx, x, y, maxW int) {
	field := &f.Fields[idx]
	focused := idx == f.Cursor

	if field.Kind == FieldReadOnly {
		screen.DrawText(x+1, y, 10, field.Label, theme.SubtleStyle())
		screen.DrawText(x+12, y, maxW-13, field.Value, theme.SubtleStyle())
		return
	}

	labelStyle := theme.BaseStyle()
	if focused {
		labelStyle = theme.AccentStyle()
	}
	screen.DrawText(x+1, y, 10, field.Label, labelStyle)

	fieldX := x + 12
	fieldW := maxW - 13
	if fieldW > 30 {
		fieldW = 30
	}
	if fieldW < 3 {
		return
	}
	bg := tcell.NewRGBColor(35, 38, 45)
	if focused {
		bg = tcell.NewRGBColor(50, 55, 65)
	}
	boxStyle := tcell.StyleDefault.Foreground(theme.FG).Background(bg)
	screen.FillRect(fieldX, y, fieldW, 1, boxStyle)

	switch field.Kind {
	case FieldToggle:
		val := field.Value
		if val != "on" {
			val = "off"
		}
		label := "[ ] off"
		if val == "on" {
			label = "[x] on"
		}
		screen.DrawText(fieldX+1, y, fieldW-2, label, boxStyle)
	case FieldChoice:
		val := field.choiceValue()
		// "< value >" with arrow chevrons in subtle style so the
		// active value pops; chevrons fade when only one choice is
		// available.
		chevStyle := theme.SubtleStyle().Background(bg)
		if len(field.Choices) <= 1 {
			chevStyle = boxStyle
		}
		screen.DrawText(fieldX+1, y, 1, "<", chevStyle)
		screen.DrawText(fieldX+fieldW-2, y, 1, ">", chevStyle)
		// Center value between the chevrons (best effort -- left-
		// aligned at fieldX+3 reads fine across widths).
		screen.DrawText(fieldX+3, y, fieldW-5, val, boxStyle)
	default:
		// Text-style fields (Text / URL / Number).
		text := field.Value
		if text == "" && !focused && field.Placeholder != "" {
			placeStyle := tcell.StyleDefault.Foreground(theme.Subtle).Background(bg)
			screen.DrawText(fieldX+1, y, fieldW-2, field.Placeholder, placeStyle)
		} else {
			screen.DrawText(fieldX+1, y, fieldW-2, text, boxStyle)
			if focused {
				cursorX := fieldX + 1 + len(text)
				if cursorX < fieldX+fieldW-1 {
					screen.SetCell(cursorX, y, ' ', tcell.StyleDefault.Background(theme.FG))
				}
			}
		}
	}
}

// HandleEvent processes one key event. Returns:
//
//	consumed  -- true when the event mutated the form or moved the
//	             cursor (the caller should swallow it).
//	submitted -- true on the Enter that the caller should treat as
//	             "save". The form does not latch; a subsequent Enter
//	             reports submitted=true again.
//
// Non-key events return (false, false).
//
// Bindings:
//
//	↑          previous non-readonly field
//	↓          next non-readonly field
//	Enter      submit
//	Backspace  delete last rune (text-style fields)
//	Space      toggle / advance choice
//	←          previous choice / toggle
//	→          next choice / toggle
//	rune       append rune (text-style fields, filtered)
func (f *Form) HandleEvent(ev tcell.Event) (consumed, submitted bool) {
	key, ok := ev.(*tcell.EventKey)
	if !ok {
		return false, false
	}
	if len(f.Fields) == 0 {
		return false, false
	}
	f.clampCursor()

	switch key.Key() {
	case tcell.KeyUp:
		f.moveCursor(-1)
		return true, false
	case tcell.KeyDown:
		f.moveCursor(1)
		return true, false
	case tcell.KeyEnter:
		return true, true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		f.backspace()
		return true, false
	case tcell.KeyLeft:
		f.cycle(-1)
		return true, false
	case tcell.KeyRight:
		f.cycle(1)
		return true, false
	case tcell.KeyRune:
		r := key.Rune()
		if r == ' ' && f.kindAtCursor() != FieldText && f.kindAtCursor() != FieldURL && f.kindAtCursor() != FieldNumber {
			f.cycle(1)
			return true, false
		}
		if f.appendRune(r) {
			return true, false
		}
		return false, false
	}
	return false, false
}

// clampCursor moves Cursor onto a non-readonly field when possible.
// If every field is read-only, Cursor stays at its current value
// (the form is effectively display-only and no input applies).
func (f *Form) clampCursor() {
	if len(f.Fields) == 0 {
		return
	}
	if f.Cursor < 0 {
		f.Cursor = 0
	}
	if f.Cursor >= len(f.Fields) {
		f.Cursor = len(f.Fields) - 1
	}
	if f.Fields[f.Cursor].Kind != FieldReadOnly {
		return
	}
	// Search forward, then wrap to forward-from-zero, for the first
	// editable field.
	for i := f.Cursor + 1; i < len(f.Fields); i++ {
		if f.Fields[i].Kind != FieldReadOnly {
			f.Cursor = i
			return
		}
	}
	for i := 0; i < f.Cursor; i++ {
		if f.Fields[i].Kind != FieldReadOnly {
			f.Cursor = i
			return
		}
	}
}

func (f *Form) moveCursor(delta int) {
	if len(f.Fields) == 0 || delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	// Take |delta| editable steps; ↑/↓ always emit ±1 today, but the
	// loop keeps the API forward-compatible with PgUp-style jumps.
	count := delta
	if count < 0 {
		count = -count
	}
	for n := 0; n < count; n++ {
		i := f.Cursor
		for k := 0; k < len(f.Fields); k++ {
			i += step
			if i < 0 {
				i = len(f.Fields) - 1
			}
			if i >= len(f.Fields) {
				i = 0
			}
			if f.Fields[i].Kind != FieldReadOnly {
				f.Cursor = i
				break
			}
		}
	}
}

func (f *Form) kindAtCursor() FieldKind {
	if f.Cursor < 0 || f.Cursor >= len(f.Fields) {
		return FieldText
	}
	return f.Fields[f.Cursor].Kind
}

// appendRune returns true when the rune was accepted (consumed).
func (f *Form) appendRune(r rune) bool {
	if f.Cursor < 0 || f.Cursor >= len(f.Fields) {
		return false
	}
	field := &f.Fields[f.Cursor]
	switch field.Kind {
	case FieldText, FieldURL, FieldNumber:
		// fall through
	default:
		return false
	}
	if !f.allows(field, r) {
		return false
	}
	if field.MaxLen > 0 && runeLen(field.Value) >= field.MaxLen {
		return false
	}
	field.Value += string(r)
	return true
}

func (f *Form) backspace() {
	if f.Cursor < 0 || f.Cursor >= len(f.Fields) {
		return
	}
	field := &f.Fields[f.Cursor]
	switch field.Kind {
	case FieldText, FieldURL, FieldNumber:
		// fall through
	default:
		return
	}
	if field.Value == "" {
		return
	}
	runes := []rune(field.Value)
	field.Value = string(runes[:len(runes)-1])
}

// cycle advances FieldToggle / FieldChoice by `delta` positions
// (wrapping). No-op for other kinds.
func (f *Form) cycle(delta int) {
	if f.Cursor < 0 || f.Cursor >= len(f.Fields) {
		return
	}
	field := &f.Fields[f.Cursor]
	switch field.Kind {
	case FieldToggle:
		if field.Value == "on" {
			field.Value = "off"
		} else {
			field.Value = "on"
		}
	case FieldChoice:
		if len(field.Choices) == 0 {
			return
		}
		cur := -1
		for i, c := range field.Choices {
			if c == field.Value {
				cur = i
				break
			}
		}
		next := cur + delta
		n := len(field.Choices)
		next = ((next % n) + n) % n
		field.Value = field.Choices[next]
	}
}

// allows returns true when r passes the field's filter. A caller-
// supplied Allow takes precedence; otherwise per-kind defaults
// apply: digits-only for Number, printable-non-whitespace for URL,
// any printable for Text.
func (f *Form) allows(field *FormField, r rune) bool {
	if field.Allow != nil {
		return field.Allow(r)
	}
	switch field.Kind {
	case FieldNumber:
		return r >= '0' && r <= '9'
	case FieldURL:
		return r > 0x20 && r != 0x7f
	default:
		return r >= 0x20 && r != 0x7f
	}
}

// choiceValue returns the current value of a FieldChoice, defaulting
// to Choices[0] when Value doesn't match any registered choice and
// Choices is non-empty. Used at render time only.
func (field *FormField) choiceValue() string {
	for _, c := range field.Choices {
		if c == field.Value {
			return field.Value
		}
	}
	if len(field.Choices) > 0 {
		return field.Choices[0]
	}
	return field.Value
}

func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
