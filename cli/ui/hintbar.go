package ui

import "strings"

// HintChip is one Key:Label hint inside a HintBar. Key may be empty
// (e.g. `:Search` -- the colon hint for search invocation, where the
// literal `:` is both the separator-between-key-and-label glyph AND
// the keystroke the user presses).
//
// When Disabled is true the chip is OMITTED from the rendered bar
// entirely -- not dimmed. The Panel chrome contract in cli/CLAUDE.md
// is explicit about this: "Hints that lie rot trust." A dimmed chip
// still claims the action exists; an omitted chip is honest.
type HintChip struct {
	Key      string
	Label    string
	Disabled bool
}

// HintBar renders a single-row bottom-band of Key:Label chips per
// the "Panel chrome contract" in cli/CLAUDE.md. Construct one per
// pane-and-state, then call Draw against the pane's bounds.
//
// Zero value is a no-op bar. Default Separator is two spaces.
type HintBar struct {
	Chips     []HintChip
	Separator string
}

// String returns the rendered chip string with disabled chips
// omitted. Useful for chrome-contract tests that assert specific
// chip layouts without touching a screen.
func (h HintBar) String() string {
	sep := h.Separator
	if sep == "" {
		sep = "  "
	}
	parts := make([]string, 0, len(h.Chips))
	for _, c := range h.Chips {
		if c.Disabled {
			continue
		}
		parts = append(parts, c.Key+":"+c.Label)
	}
	return strings.Join(parts, sep)
}

// Draw anchors the chip string to the bottom row of bounds, padded
// one cell from the left, in the theme's subtle style -- the same
// chrome treatment every existing view uses via DrawBottom.
func (h HintBar) Draw(screen *Screen, bounds Rect, theme Theme) {
	text := h.String()
	if text == "" {
		return
	}
	DrawBottom(screen, bounds, theme.SubtleStyle(), 1, text)
}
