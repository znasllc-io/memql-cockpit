package ui

import (
	"fmt"
	"unicode/utf8"
)

// PaneTitle renders a pane's top-row title chrome per the
// "Panel chrome contract" in cli/CLAUDE.md. The row is split into
// two halves:
//
//   - Title (left): anchored at bounds.X+1. The bare pane name --
//     no embedded ids, no trailing colons, no parentheses around any
//     count. ALL CAPS by convention; not enforced.
//   - Counter (right): right-anchored so its last cell lands at
//     bounds.X+bounds.Width-2. NO parentheses. Empty string means
//     "no counter on this pane".
//
// Focused flips both halves from subtle-bold to accent-bold so the
// active pane reads at a glance -- same rule existing panes already
// apply with their hand-rolled titleStyle.
//
// Truncation rule. If title + a 2-cell gap + counter doesn't fit in
// the pane's inner width, the counter is dropped first (the title is
// the load-bearing identity, the counter is supplementary). If the
// title still doesn't fit, DrawText clips it -- same behavior every
// other ui primitive uses, no special handling needed.
type PaneTitle struct {
	Title   string
	Counter string
	Focused bool
}

// Draw paints the title row at bounds.Y. Does NOT clear the row --
// callers FillRect the pane background once per frame; PaneTitle
// only writes the title and counter cells.
func (t PaneTitle) Draw(screen *Screen, bounds Rect, theme Theme) {
	if screen == nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	style := theme.SubtleStyle().Bold(true)
	if t.Focused {
		style = theme.AccentStyle().Bold(true)
	}

	// 1-cell gutter on each edge -- matches the rest of the pane
	// chrome (DrawBottom uses padX=1 + innerW = bounds.Width-padX-1).
	innerW := bounds.Width - 2
	if innerW <= 0 {
		return
	}

	titleLen := utf8.RuneCountInString(t.Title)
	counterLen := utf8.RuneCountInString(t.Counter)

	// Counter is dropped when there's not enough room for title +
	// 2-cell gap + counter inside the inner width.
	drawCounter := t.Counter != "" && titleLen+2+counterLen <= innerW

	titleW := innerW
	if drawCounter {
		// Reserve counterLen cells on the right plus a single cell
		// of visual buffer so the title can't visually touch the
		// counter even if a future caller passes a string longer
		// than its measured length suggested.
		titleW = innerW - counterLen - 1
		if titleW < 0 {
			titleW = 0
		}
	}
	if titleW > 0 && t.Title != "" {
		screen.DrawText(bounds.X+1, bounds.Y, titleW, t.Title, style)
	}

	if drawCounter {
		// Last cell of the counter lands at bounds.X+bounds.Width-2
		// (the rightmost cell inside the gutter). Its first cell is
		// counterLen-1 to the left of that, i.e. bounds.X+bounds.Width-1-counterLen.
		x := bounds.X + bounds.Width - 1 - counterLen
		screen.DrawText(x, bounds.Y, counterLen, t.Counter, style)
	}
}

// FormatCursor returns "N/M" for a 0-indexed cursor over a total of
// `total` items, one-indexed for display. Returns "" when total is
// zero so empty panes paint a bare title with no trailing counter.
func FormatCursor(selected, total int) string {
	if total <= 0 {
		return ""
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= total {
		selected = total - 1
	}
	return fmt.Sprintf("%d/%d", selected+1, total)
}

// FormatFiltered returns the counter for a list that may be narrowed
// by a search filter:
//
//   - "N/M"                    when matches == total (no filter active).
//   - "N/M filtered from K"    when matches < total.
//   - "0/0 filtered from K"    when a filter is active and nothing matched.
//   - ""                       when total is zero.
func FormatFiltered(selected, matches, total int) string {
	if total <= 0 {
		return ""
	}
	if matches <= 0 {
		return fmt.Sprintf("0/0 filtered from %d", total)
	}
	if matches < total {
		return fmt.Sprintf("%s filtered from %d", FormatCursor(selected, matches), total)
	}
	return FormatCursor(selected, total)
}

// FormatLine returns "line N/M" for scroll-position counters in
// scrollable detail panes. Returns "" when total is zero.
func FormatLine(scrollY, total int) string {
	s := FormatCursor(scrollY, total)
	if s == "" {
		return ""
	}
	return "line " + s
}

// FormatCount returns a bare "N" for panes that only track a total
// (no cursor position). Returns "" when n is zero so empty panes
// stay bare.
func FormatCount(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}
