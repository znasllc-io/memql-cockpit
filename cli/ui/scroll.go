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

// DrawScrollbar paints a 1-column position indicator on the
// rightmost column of bounds (bounds.X + bounds.Width - 1). The
// track uses a subtle pipe glyph; a SINGLE accent-colored square
// marks the current scroll position, sliding linearly from the top
// of the track (offset = 0) to the bottom (offset = maxOffset).
// No-op when all rows fit (totalRows <= viewportH).
//
// A single-cell indicator reads more cleanly than a proportional
// thumb at narrow widths -- the extra information about "window
// size" from a proportional thumb doesn't pay for the visual
// noise of multiple stacked block glyphs.
func DrawScrollbar(screen *Screen, theme Theme, bounds Rect, offset, totalRows int) {
	if screen == nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	viewportH := bounds.Height
	if totalRows <= viewportH {
		return
	}

	col := bounds.X + bounds.Width - 1
	trackStyle := tcell.StyleDefault.Foreground(theme.Subtle).Background(theme.BG)
	thumbStyle := tcell.StyleDefault.Foreground(theme.Accent).Background(theme.BG).Bold(true)

	// Track: a dim pipe down the column. Thumb will overdraw one cell.
	for i := 0; i < viewportH; i++ {
		screen.SetCell(col, bounds.Y+i, '│', trackStyle)
	}

	// Position the 1-cell thumb. Map offset in [0, maxOff] to a row
	// index in [0, viewportH-1]. At the top (offset == 0) the
	// indicator sits on the first track cell; at the bottom
	// (offset == maxOff) it sits on the last. When maxOff is 0 we
	// wouldn't be drawing the scrollbar at all (guarded above).
	maxOff := totalRows - viewportH
	thumbY := 0
	if maxOff > 0 {
		thumbY = offset * (viewportH - 1) / maxOff
	}
	if thumbY < 0 {
		thumbY = 0
	}
	if thumbY >= viewportH {
		thumbY = viewportH - 1
	}
	screen.SetCell(col, bounds.Y+thumbY, '■', thumbStyle)
}
