package ui

import "github.com/gdamore/tcell/v2"

// ScrollTo clamps a scroll offset so that selectedIdx is inside the
// viewport on the next draw. Call once per Draw -- pass the stored
// offset, take the returned value back. Behavior:
//
//   - Selected above the current window scrolls up to expose it.
//   - Selected below scrolls down far enough to make it the last
//     visible row.
//   - offset is clamped to [0, totalRows-viewportH] so a shrinking
//     list can't leave blank rows at the bottom.
//
// Safe with empty lists and viewportH <= 0; returns 0 in those cases.
func ScrollTo(prevOffset, selectedIdx, totalRows, viewportH int) int {
	if viewportH <= 0 || totalRows <= 0 {
		return 0
	}
	if selectedIdx < 0 {
		selectedIdx = 0
	}
	if selectedIdx >= totalRows {
		selectedIdx = totalRows - 1
	}
	off := prevOffset
	if off < 0 {
		off = 0
	}
	maxOff := totalRows - viewportH
	if maxOff < 0 {
		maxOff = 0
	}
	if off > maxOff {
		off = maxOff
	}
	if selectedIdx < off {
		off = selectedIdx
	}
	if selectedIdx >= off+viewportH {
		off = selectedIdx - viewportH + 1
	}
	return off
}

// VisibleRange returns [start, end) -- the half-open row index range
// to render for a scrollable list of totalRows items with the given
// offset and viewport height. Always returns indices within the
// data; callers can iterate without bounds-checking.
func VisibleRange(offset, totalRows, viewportH int) (int, int) {
	if viewportH <= 0 || totalRows <= 0 {
		return 0, 0
	}
	if offset < 0 {
		offset = 0
	}
	end := offset + viewportH
	if end > totalRows {
		end = totalRows
	}
	if offset >= end {
		return 0, 0
	}
	return offset, end
}

// DrawScrollbar paints a single accent-colored thumb (`■`) on the
// rightmost column of bounds (`bounds.X + bounds.Width - 1`). No
// track lines -- the rest of the column stays at whatever the
// caller left there (typically the pane background), which reads
// as a single moving square instead of competing vertical bars
// against the pane borders.
//
// `pos` and `maxPos` are in caller-defined units (the units have
// to match each other, but the widget doesn't care which they
// are). The two ways the units are used in practice:
//
//   - ListPane passes (Selected, Count-1) -- item index. The
//     thumb tracks the user's cursor, which is what they
//     actually navigate by. Pre-fix the unit-mismatched math
//     (offset in items, viewport in rows-per-item) caused the
//     thumb to hit the bottom of the column at ~ScrollY = 47
//     when the actual max was ~62 in a 2-rows-per-item list,
//     then stay pinned for the remaining presses. Bound to
//     Selected, the mapping is straightforward and stable.
//   - Viewer / DetailPane pass (ScrollY, maxScroll) -- both in
//     rendered-row units (post-wrap). There's no cursor in
//     those widgets, so the viewport's top-edge offset is the
//     right input.
//
// Returns immediately when maxPos <= 0 -- nothing to indicate.
func DrawScrollbar(screen *Screen, theme Theme, bounds Rect, pos, maxPos int) {
	if screen == nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	if maxPos <= 0 {
		return
	}
	if pos < 0 {
		pos = 0
	}
	if pos > maxPos {
		pos = maxPos
	}

	col := bounds.X + bounds.Width - 1
	thumbStyle := tcell.StyleDefault.Foreground(theme.Accent).Background(theme.BG).Bold(true)

	// Map pos in [0, maxPos] -> thumbY in [0, height-1]. At pos == 0
	// the thumb sits on the first column row; at pos == maxPos it
	// sits on the last. Integer division rounds toward zero, which
	// means intermediate positions can land on a slightly lower row
	// than a true proportional placement would suggest -- acceptable
	// at single-cell resolution.
	thumbY := pos * (bounds.Height - 1) / maxPos
	if thumbY < 0 {
		thumbY = 0
	}
	if thumbY >= bounds.Height {
		thumbY = bounds.Height - 1
	}
	screen.SetCell(col, bounds.Y+thumbY, '■', thumbStyle)
}
