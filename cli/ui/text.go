package ui

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// WrapText splits text into lines no wider than maxWidth. Breaks on
// whitespace, preserving word boundaries. Words that are themselves
// longer than maxWidth are emitted on their own line (no mid-word
// breaking -- keeping URLs / long identifiers readable matters more
// than hitting the width limit exactly).
//
// Callers should pass the INNER width of the region they're drawing
// into (i.e. bounds.Width minus any padding), not the total width.
func WrapText(text string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{text}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) <= maxWidth {
		return []string{text}
	}

	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{text}
	}

	var lines []string
	var line string
	for _, w := range words {
		if len(w) > maxWidth {
			// Oversized single word. Flush whatever's buffered, then
			// let the long word occupy its own line -- the caller's
			// DrawText will clip it visually if it overflows.
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			lines = append(lines, w)
			continue
		}
		if line == "" {
			line = w
			continue
		}
		if len(line)+1+len(w) > maxWidth {
			lines = append(lines, line)
			line = w
		} else {
			line += " " + w
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// DrawBottom renders one or more text lines anchored to the bottom
// row of the given bounds. Any single line longer than the drawable
// width is wrapped across rows (via WrapText) so a narrow pane
// can't truncate "Press Enter to retry" style hints.
//
// The lines are drawn starting at bounds.X + padX so a 1-cell gutter
// on the left matches the rest of the pane chrome. Returns the
// top Y of the drawn block so callers can gate other rendering
// against it (e.g. the row list stops short of the hint block).
func DrawBottom(screen *Screen, bounds Rect, style tcell.Style, padX int, lines ...string) int {
	if screen == nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return bounds.Y + bounds.Height
	}
	innerW := bounds.Width - padX - 1
	if innerW <= 0 {
		innerW = bounds.Width
	}

	wrapped := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		wrapped = append(wrapped, WrapText(ln, innerW)...)
	}
	if len(wrapped) == 0 {
		return bounds.Y + bounds.Height
	}

	startY := bounds.Y + bounds.Height - len(wrapped)
	if startY < bounds.Y {
		// Block is taller than the bounds; clip from the top.
		wrapped = wrapped[len(wrapped)-bounds.Height:]
		startY = bounds.Y
	}
	for i, ln := range wrapped {
		screen.DrawText(bounds.X+padX, startY+i, innerW, ln, style)
	}
	return startY
}

// DrawBottomBlocks is DrawBottom for multiple separately-styled
// blocks stacked from bottom up. The last block in the slice is the
// LOWEST on screen; earlier blocks sit above it. Useful for
// "confirmation prompt + hint line" layouts where the two lines
// want different colors.
func DrawBottomBlocks(screen *Screen, bounds Rect, padX int, blocks ...BottomBlock) int {
	if screen == nil || len(blocks) == 0 {
		return bounds.Y + bounds.Height
	}
	innerW := bounds.Width - padX - 1
	if innerW <= 0 {
		innerW = bounds.Width
	}

	type wrappedBlock struct {
		lines []string
		style tcell.Style
	}
	total := 0
	out := make([]wrappedBlock, 0, len(blocks))
	for _, b := range blocks {
		lines := []string{}
		for _, ln := range b.Lines {
			if ln == "" {
				continue
			}
			lines = append(lines, WrapText(ln, innerW)...)
		}
		if len(lines) > 0 {
			out = append(out, wrappedBlock{lines: lines, style: b.Style})
			total += len(lines)
		}
	}
	if total == 0 {
		return bounds.Y + bounds.Height
	}

	startY := bounds.Y + bounds.Height - total
	if startY < bounds.Y {
		startY = bounds.Y
	}
	y := startY
	for _, wb := range out {
		for _, ln := range wb.lines {
			if y >= bounds.Y+bounds.Height {
				break
			}
			screen.DrawText(bounds.X+padX, y, innerW, ln, wb.style)
			y++
		}
	}
	return startY
}

// BottomBlock is one colored section inside a DrawBottomBlocks call.
type BottomBlock struct {
	Lines []string
	Style tcell.Style
}
