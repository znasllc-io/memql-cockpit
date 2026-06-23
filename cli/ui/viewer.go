package ui

import (
	"github.com/gdamore/tcell/v2"
)

// HighlightSpan is one styled segment inside a ViewerLine. Coordinates
// are rune-based (Start inclusive, End exclusive) over the line's
// Text. Spans never carry the gutter -- they're indexed against the
// line's raw text, not the rendered row.
//
// Overlap rule: when spans overlap, later spans in the slice win for
// the overlapping runes. Callers that produce spans from a tokenizer
// can rely on this to layer per-token styling over a base style.
type HighlightSpan struct {
	Start int // first rune index covered (inclusive)
	End   int // last rune index covered (exclusive)
	Style tcell.Style
}

// ViewerLine is one source line in a Viewer. Text is the raw line
// content (no embedded newlines -- callers split on '\n' before
// constructing the slice). Spans is optional per-line highlighting.
// Block groups consecutive lines with the same id under a single
// subtle background band; id 0 means "no block."
type ViewerLine struct {
	Text  string
	Spans []HighlightSpan
	Block int
}

// Viewer is a reusable read-only viewer panel for showing returned
// MemQL data (or any line-oriented text) without the horizontal
// overflow LinePlain/DetailPane allow. Differences from DetailPane:
//
//  1. **Line numbers** -- a right-aligned subtle gutter on every
//     source line. Continuation rows for a wrapped line render a
//     blank gutter so the eye can scan numbers cleanly.
//  2. **Hard wrap** -- lines longer than the inner content width
//     break mid-token. No rune can render past the right edge. (The
//     old WrapText helper breaks on whitespace only and lets long
//     JSON values escape, which is the bug closed in #50.)
//  3. **Block tinting** -- consecutive lines sharing a non-zero
//     Block id receive a subtle background tint so logical sections
//     scan as bands.
//  4. **Highlight spans** -- per-line rune ranges painted in the
//     supplied style; everything outside a span renders in BaseStyle.
//
// The viewer is read-only: no cursor, no editing surface, no input
// beyond scroll keys. Selection / copy is the terminal's job.
type Viewer struct {
	Lines        []ViewerLine
	ScrollY      int
	Focused      bool
	EmptyMessage string

	// CursorLine is the 0-based source-line index of an optional
	// "active line" highlight, used by read-only inspectors (e.g. the
	// Editor tab's hover cursor). Honored only when ShowCursor is true;
	// the default zero value leaves the viewer in its original
	// scroll-only mode (the Concepts detail pane relies on that). When
	// shown, Draw tints the cursor line's rows -- wrapping included --
	// and auto-scrolls to keep the line in view.
	CursorLine int
	ShowCursor bool

	// viewportRowsCache is populated by Draw so HandleEvent's PgUp /
	// PgDn can size their jumps using the bounds Draw last saw, and
	// maxScrollCache so KeyDown / KeyEnd / KeyPgDn can stop ScrollY
	// at the last valid position instead of overshooting. The
	// overshoot bug was visible to users as "arrow keys stop
	// scrolling": ScrollY drifted past the displayable max, and
	// KeyUp had to decrement back through the dead range before the
	// viewport moved -- looked like a stuck cursor. Unexported
	// because callers don't construct Viewer with these set; they're
	// pure draw-to-event side channels.
	viewportRowsCache int
	maxScrollCache    int
}

// gutterWidth is the number of cells reserved for the line-number
// column (4 digits + 1 trailing space). Hard-coded because every
// realistic concept-row payload fits well under 10,000 lines; if a
// surface ever needs 5-digit gutters we'll widen this and revisit
// the alignment math in renderRow.
const gutterWidth = 5

// Draw paints the viewer into bounds. Auto-clamps ScrollY so callers
// don't have to track it across resizes (same contract as DetailPane
// + ListPane).
func (v *Viewer) Draw(screen *Screen, bounds Rect, theme Theme) {
	if screen == nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	if bounds.Width < gutterWidth+2 {
		// Too narrow to render anything useful; bail out so we don't
		// paint a corrupted half-gutter.
		return
	}

	if len(v.Lines) == 0 {
		if v.EmptyMessage != "" {
			y := bounds.Y + bounds.Height/2
			screen.DrawText(bounds.X+1, y, bounds.Width-2, v.EmptyMessage, theme.SubtleStyle())
		}
		return
	}

	// Reserve one column for the scrollbar so the rightmost cell
	// doesn't get overwritten when content overflows.
	innerW := bounds.Width - 1
	if innerW < gutterWidth+1 {
		innerW = gutterWidth + 1
	}
	contentW := innerW - gutterWidth
	if contentW < 1 {
		return
	}

	rows := v.flatten(contentW, theme)
	v.viewportRowsCache = bounds.Height

	maxScroll := len(rows) - bounds.Height
	if maxScroll < 0 {
		maxScroll = 0
	}
	v.maxScrollCache = maxScroll

	// When the cursor is shown, auto-scroll so its (possibly wrapped)
	// source line stays in view -- the wrap math lives here because
	// only flatten knows how a source line maps onto rendered rows.
	if v.ShowCursor {
		if first := firstRowForLine(rows, v.CursorLine); first >= 0 {
			if first < v.ScrollY {
				v.ScrollY = first
			} else if first >= v.ScrollY+bounds.Height {
				v.ScrollY = first - bounds.Height + 1
			}
		}
	}

	if v.ScrollY > maxScroll {
		v.ScrollY = maxScroll
	}
	if v.ScrollY < 0 {
		v.ScrollY = 0
	}

	scrollbarVisible := len(rows) > bounds.Height
	end := v.ScrollY + bounds.Height
	if end > len(rows) {
		end = len(rows)
	}

	// Track which source line each rendered row belongs to (continuation
	// rows carry lineNum 0) so the cursor tint can span a wrapped line.
	cursorBG := tcell.StyleDefault.Foreground(theme.FG).Background(tcell.NewRGBColor(45, 50, 60))
	curLine := -1
	for i := 0; i < end; i++ {
		if rows[i].lineNum > 0 {
			curLine = rows[i].lineNum - 1
		}
		if i < v.ScrollY {
			continue
		}
		r := rows[i]
		// When this row is part of the cursor's source line and carries
		// no block tint, render it with the cursor highlight (drawRow
		// applies the bg to the gutter + every token, so colored tokens
		// keep their fg under the highlight).
		if v.ShowCursor && r.blockBG == nil && curLine == v.CursorLine {
			cb := cursorBG
			r.blockBG = &cb
		}
		y := bounds.Y + (i - v.ScrollY)
		// Block tint covers the entire inner width (gutter + content)
		// so the band reads as one continuous chrome stripe.
		if r.blockBG != nil {
			screen.FillRect(bounds.X, y, innerW, 1, *r.blockBG)
		}
		v.drawRow(screen, theme, bounds.X, y, contentW, r)
	}

	if scrollbarVisible {
		// (ScrollY, maxScroll) -- both row-unit. The widget needs
		// `maxPosition`, not `total`, so pass maxScroll directly.
		DrawScrollbar(screen, theme, bounds, v.ScrollY, maxScroll)
	}
}

// firstRowForLine returns the index of the first rendered row that
// belongs to source line `line` (0-based), or -1 if no such row. Used
// to map a source-line cursor onto the flattened rendered rows for
// auto-scroll.
func firstRowForLine(rows []renderedViewerRow, line int) int {
	if line < 0 {
		return -1
	}
	for i, r := range rows {
		if r.lineNum == line+1 {
			return i
		}
	}
	return -1
}

// renderedViewerRow is one terminal row in flattened form. lineNum > 0
// means "draw N right-aligned in the gutter"; 0 means continuation
// (blank gutter). Runes carries the slice of (char, style) pairs that
// occupy the content area. blockBG, when non-nil, is the background
// style to fill the whole row with before painting the runes (so
// block-tinted bands span the gutter too).
type renderedViewerRow struct {
	lineNum int
	runes   []styledRune
	blockBG *tcell.Style
}

type styledRune struct {
	r     rune
	style tcell.Style
}

func (v *Viewer) drawRow(screen *Screen, theme Theme, x, y, contentW int, r renderedViewerRow) {
	gutterStyle := theme.SubtleStyle()
	if r.blockBG != nil {
		gutterStyle = gutterStyle.Background(extractBG(*r.blockBG))
	}
	if r.lineNum > 0 {
		// Right-align: format up to gutterWidth-1 digits, one trailing
		// space. For lineNums > 9999 we truncate visually -- realistic
		// concept rows don't get there.
		num := formatLineNum(r.lineNum, gutterWidth-1)
		screen.DrawText(x, y, gutterWidth, num+" ", gutterStyle)
	} else {
		// Continuation row -- blank gutter, but keep the trailing
		// space cell painted so the block-tint background extends
		// uniformly.
		for i := 0; i < gutterWidth; i++ {
			screen.SetCell(x+i, y, ' ', gutterStyle)
		}
	}

	for i, sr := range r.runes {
		if i >= contentW {
			break
		}
		style := sr.style
		if r.blockBG != nil {
			style = style.Background(extractBG(*r.blockBG))
		}
		screen.SetCell(x+gutterWidth+i, y, sr.r, style)
	}
}

// flatten expands ViewerLines into rendered rows. Each source line
// produces one or more rows depending on the rune count relative to
// contentW (the inner content width after the gutter is subtracted).
func (v *Viewer) flatten(contentW int, theme Theme) []renderedViewerRow {
	if contentW < 1 {
		contentW = 1
	}
	base := theme.BaseStyle()
	out := make([]renderedViewerRow, 0, len(v.Lines))

	// Block tinting: a single subtle-ish background slightly darker
	// than BG so consecutive same-block rows read as a band.
	blockTint := tcell.StyleDefault.Foreground(theme.FG).Background(tcell.NewRGBColor(28, 28, 33))

	for idx, ln := range v.Lines {
		runes := styleRunes(ln.Text, ln.Spans, base)

		// Decide block tint -- supplied at line granularity, applied
		// to every rendered row of that line.
		var bg *tcell.Style
		if ln.Block != 0 {
			b := blockTint
			bg = &b
		}

		// Empty source line -- still emit one row so blank separators
		// render as expected. Line number 0 (continuation) feels
		// wrong for the FIRST row of an empty line, so we still stamp
		// the source line number; the row's runes slice is empty.
		if len(runes) == 0 {
			out = append(out, renderedViewerRow{
				lineNum: idx + 1,
				runes:   nil,
				blockBG: bg,
			})
			continue
		}

		// Hard-wrap by rune count -- no whitespace bias. This is the
		// behaviour that closes #50: a 200-char JSON blob with zero
		// spaces still wraps at contentW.
		first := true
		for start := 0; start < len(runes); start += contentW {
			end := start + contentW
			if end > len(runes) {
				end = len(runes)
			}
			row := renderedViewerRow{
				runes:   runes[start:end],
				blockBG: bg,
			}
			if first {
				row.lineNum = idx + 1
				first = false
			}
			out = append(out, row)
		}
	}
	return out
}

// styleRunes converts a source line + its spans into a styled-rune
// slice in source order. Runes not covered by any span receive the
// base style; overlapping spans resolve "later wins" per HighlightSpan
// doc.
func styleRunes(text string, spans []HighlightSpan, base tcell.Style) []styledRune {
	if text == "" {
		return nil
	}
	rs := []rune(text)
	out := make([]styledRune, len(rs))
	for i, r := range rs {
		out[i] = styledRune{r: r, style: base}
	}
	for _, sp := range spans {
		lo, hi := sp.Start, sp.End
		if lo < 0 {
			lo = 0
		}
		if hi > len(rs) {
			hi = len(rs)
		}
		for i := lo; i < hi; i++ {
			out[i].style = sp.Style
		}
	}
	return out
}

// HandleEvent processes scroll keys when Focused is true. Returns
// true if the event was consumed. Bindings mirror DetailPane so the
// muscle memory transfers across surfaces.
//
// Both edges of the scroll range are clamped on every key press
// (using the maxScroll the last Draw observed). Pre-fix the
// downward keys (KeyDown / KeyPgDn / KeyEnd / 'j') let ScrollY
// drift past the displayable max -- visually a no-op, but the
// internal counter accumulated phantom presses and KeyUp then had
// to decrement back through the dead range before the viewport
// moved. To the user that read as "arrow keys do nothing"; see
// memql-cockpit#115.
func (v *Viewer) HandleEvent(ev tcell.Event) bool {
	if !v.Focused {
		return false
	}
	key, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}
	page := v.viewportRowsCache
	if page <= 0 {
		page = 1
	}
	switch key.Key() {
	case tcell.KeyUp:
		v.ScrollY--
		v.clampScroll()
		return true
	case tcell.KeyDown:
		v.ScrollY++
		v.clampScroll()
		return true
	case tcell.KeyPgUp:
		v.ScrollY -= page
		v.clampScroll()
		return true
	case tcell.KeyPgDn:
		v.ScrollY += page
		v.clampScroll()
		return true
	case tcell.KeyHome:
		v.ScrollY = 0
		return true
	case tcell.KeyEnd:
		v.ScrollY = v.maxScrollCache
		v.clampScroll()
		return true
	case tcell.KeyRune:
		switch key.Rune() {
		case 'k':
			v.ScrollY--
			v.clampScroll()
			return true
		case 'j':
			v.ScrollY++
			v.clampScroll()
			return true
		}
	}
	return false
}

// MaxScroll returns the largest valid ScrollY observed during the
// most recent Draw -- i.e. the scroll offset at which the last
// content row is just visible at the bottom of the viewport.
// Returns 0 before the first Draw, when the content fits in the
// viewport, or when there are no lines. Useful for callers that
// need to coordinate with the viewer's scroll state (e.g. testing
// scroll bindings against the actual content size).
func (v *Viewer) MaxScroll() int {
	return v.maxScrollCache
}

// clampScroll clamps ScrollY to [0, maxScrollCache]. maxScrollCache
// is set by Draw; before the first Draw it's 0, which makes every
// scroll a no-op until a frame has been painted. That's the desired
// behavior -- without a known viewport the widget can't tell where
// the scroll range ends.
func (v *Viewer) clampScroll() {
	if v.ScrollY < 0 {
		v.ScrollY = 0
	}
	if v.ScrollY > v.maxScrollCache {
		v.ScrollY = v.maxScrollCache
	}
}

// formatLineNum right-pads a 1-indexed line number to width cells
// (leading spaces). Numbers wider than `width` truncate to "####"
// rather than break alignment.
func formatLineNum(n, width int) string {
	if width <= 0 {
		return ""
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if len(digits) == 0 {
		digits = []byte{'0'}
	}
	if len(digits) > width {
		out := make([]byte, width)
		for i := range out {
			out[i] = '#'
		}
		return string(out)
	}
	pad := width - len(digits)
	buf := make([]byte, width)
	for i := 0; i < pad; i++ {
		buf[i] = ' '
	}
	copy(buf[pad:], digits)
	return string(buf)
}

// extractBG pulls the background color off a tcell.Style. tcell
// doesn't expose a direct getter on Style, so we use Decompose. Lifted
// here as a small helper because we apply block-tint backgrounds to
// spans that already carry token-fg colors -- swapping the bg without
// touching the fg keeps token highlighting intact under the tint.
func extractBG(style tcell.Style) tcell.Color {
	_, bg, _ := style.Decompose()
	return bg
}
