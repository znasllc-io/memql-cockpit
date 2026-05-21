package ui

import (
	"github.com/gdamore/tcell/v2"
)

// DetailLineKind enumerates the structural kinds of a DetailLine.
// The kind drives style (header/dim/kv vs. plain) and whether the
// renderer wraps the line, leaves it as-is, or paints it in two
// segments.
type DetailLineKind int

const (
	// LinePlain is normal text in the theme's base style. Wrapped
	// when the line exceeds the pane's inner width.
	LinePlain DetailLineKind = iota

	// LineHeader is a section header in accent + bold. Wrapped if
	// needed; intended for short labels like "PROVIDER" or
	// "CAPABILITIES" that introduce a block of key/value rows
	// underneath.
	LineHeader

	// LineDim is muted text in the subtle style. Use for separators,
	// hints, and any low-signal information that should fade into
	// the chrome.
	LineDim

	// LineKV is a two-segment row: a dimmed `Key:` prefix followed
	// by `Value` in base style on the same row. KV rows do NOT
	// wrap -- if the value exceeds remaining width it's truncated.
	// (The reusable read-only viewer in #50 will add wrap support
	// for syntax-aware variants.)
	LineKV

	// LineSection is a muted-but-bold marker for the "─ name ─"
	// dividers planner / agents use to chunk a detail pane into
	// blocks. Renders in theme.SubtleStyle().Bold(true) so the
	// marker recedes into the chrome rather than competing with
	// LineHeader's accent emphasis. Wrapped like LinePlain.
	LineSection
)

// DetailLine is one logical line in a DetailPane. For LinePlain /
// LineHeader / LineDim only Text is used; for LineKV the row is
// "<Key>: <Value>" with Key dimmed and Value in base style.
type DetailLine struct {
	Kind  DetailLineKind
	Text  string
	Key   string
	Value string
}

// DetailPane renders a scrollable, read-only column of structured
// DetailLine entries. Used by views with a "right pane" that shows
// the highlighted row's detail -- concepts / planner / agents
// today, more to come.
//
// Lines that exceed the pane's inner width are wrapped via the
// existing WrapText helper (single-line LineKV rows are the
// intentional exception -- see DetailLineKind for the reasoning).
//
// Scroll is in *rendered-row* space, not DetailLine space. Wrap
// turns one source line into N rendered rows; ScrollY counts the
// latter so PgUp/PgDn jumps stay predictable as the bounds resize.
type DetailPane struct {
	Lines        []DetailLine
	ScrollY      int
	Focused      bool
	EmptyMessage string

	// viewportRowsCache holds the row count of the last Draw so
	// HandleEvent's PgUp/PgDn can size their jumps correctly.
	viewportRowsCache int
}

// renderedRow is the internal flattened form: one terminal row,
// fully styled, ready to draw at a given Y.
type renderedRow struct {
	text   string
	style  tcell.Style
	kvKey  string // when non-empty, render this dimmed first then text in base
	kvOnly bool   // marker so Draw uses the two-segment path
}

// Draw paints visible rendered rows into bounds. Auto-clamps
// ScrollY so the caller doesn't have to track it across resizes.
func (d *DetailPane) Draw(screen *Screen, bounds Rect, theme Theme) {
	if screen == nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}

	// One column reserved for the scrollbar when content overflows,
	// matching the layout discipline elsewhere in the chrome.
	innerW := bounds.Width
	if innerW < 1 {
		return
	}

	flat := d.flatten(innerW-1, theme)
	d.viewportRowsCache = bounds.Height

	if len(flat) == 0 {
		if d.EmptyMessage != "" {
			y := bounds.Y + bounds.Height/2
			screen.DrawText(bounds.X+1, y, bounds.Width-2, d.EmptyMessage, theme.SubtleStyle())
		}
		return
	}

	// Clamp ScrollY to [0, max(0, total - viewport)].
	maxScroll := len(flat) - bounds.Height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if d.ScrollY > maxScroll {
		d.ScrollY = maxScroll
	}
	if d.ScrollY < 0 {
		d.ScrollY = 0
	}

	scrollbarVisible := len(flat) > bounds.Height
	contentW := innerW
	if scrollbarVisible {
		contentW = innerW - 1
		if contentW < 1 {
			contentW = 1
		}
	}

	end := d.ScrollY + bounds.Height
	if end > len(flat) {
		end = len(flat)
	}
	for i := d.ScrollY; i < end; i++ {
		row := flat[i]
		y := bounds.Y + (i - d.ScrollY)
		if row.kvOnly {
			// Two-segment render: dimmed key + space + value.
			keyLen := len(row.kvKey)
			screen.DrawText(bounds.X, y, contentW, row.kvKey, theme.SubtleStyle())
			if keyLen+1 < contentW {
				screen.DrawText(bounds.X+keyLen+1, y, contentW-keyLen-1, row.text, row.style)
			}
		} else {
			screen.DrawText(bounds.X, y, contentW, row.text, row.style)
		}
	}

	if scrollbarVisible {
		// (ScrollY, maxScroll) -- both row-unit. Same pattern as Viewer.
		DrawScrollbar(screen, theme, bounds, d.ScrollY, maxScroll)
	}
}

// HandleEvent processes scroll keys when Focused is true. Returns
// true if the event was consumed; false (always) when not focused.
//
// Bindings (focused only):
//
//	↑ / k       Scroll up one row
//	↓ / j       Scroll down one row
//	PgUp        Scroll up one viewport
//	PgDn        Scroll down one viewport
//	Home        Scroll to top
//	End         Scroll to bottom
//
// End uses the last known content length from Draw to find the
// bottom; before any Draw, End is a no-op so it can't desync.
func (d *DetailPane) HandleEvent(ev tcell.Event) bool {
	if !d.Focused {
		return false
	}
	key, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}
	page := d.viewportRowsCache
	if page <= 0 {
		page = 1
	}
	switch key.Key() {
	case tcell.KeyUp:
		d.ScrollY--
		d.clampScroll()
		return true
	case tcell.KeyDown:
		d.ScrollY++
		// Draw will clamp on next paint using the current length.
		return true
	case tcell.KeyPgUp:
		d.ScrollY -= page
		d.clampScroll()
		return true
	case tcell.KeyPgDn:
		d.ScrollY += page
		// Draw clamps the upper bound.
		return true
	case tcell.KeyHome:
		d.ScrollY = 0
		return true
	case tcell.KeyEnd:
		// Big number; Draw clamps to the real maximum.
		d.ScrollY = 1<<30 - 1
		return true
	case tcell.KeyRune:
		switch key.Rune() {
		case 'k':
			d.ScrollY--
			d.clampScroll()
			return true
		case 'j':
			d.ScrollY++
			return true
		}
	}
	return false
}

func (d *DetailPane) clampScroll() {
	if d.ScrollY < 0 {
		d.ScrollY = 0
	}
}

// flatten expands DetailLines into rendered rows after wrapping
// (where wrap applies). Exposed only via Draw; tests reach into it
// indirectly through Draw + the simulation screen.
func (d *DetailPane) flatten(innerW int, theme Theme) []renderedRow {
	if innerW < 1 {
		innerW = 1
	}
	out := make([]renderedRow, 0, len(d.Lines))
	for _, ln := range d.Lines {
		switch ln.Kind {
		case LineHeader:
			out = wrapInto(out, ln.Text, innerW, theme.AccentStyle().Bold(true))
		case LineSection:
			out = wrapInto(out, ln.Text, innerW, theme.SubtleStyle().Bold(true))
		case LineDim:
			out = wrapInto(out, ln.Text, innerW, theme.SubtleStyle())
		case LineKV:
			// KV is a single-row two-segment render. No wrap.
			out = append(out, renderedRow{
				text:   ln.Value,
				style:  theme.BaseStyle(),
				kvKey:  ln.Key + ":",
				kvOnly: true,
			})
		default: // LinePlain (zero value of DetailLineKind)
			out = wrapInto(out, ln.Text, innerW, theme.BaseStyle())
		}
	}
	return out
}

// wrapInto appends one or more rendered rows for a single source
// line. Empty source text emits exactly one blank rendered row --
// callers use blank lines as section separators (e.g. between
// INTRINSICS / PAYLOAD / PROVENANCE blocks), so dropping them would
// visually collapse sections together.
func wrapInto(out []renderedRow, text string, innerW int, style tcell.Style) []renderedRow {
	if text == "" {
		return append(out, renderedRow{text: "", style: style})
	}
	for _, w := range WrapText(text, innerW) {
		out = append(out, renderedRow{text: w, style: style})
	}
	return out
}
