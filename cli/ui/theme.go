// Package ui provides the terminal UI framework for memQL Cockpit.
// It wraps tcell with a lightweight panel/tab/focus system.
package ui

import "github.com/gdamore/tcell/v2"

// Theme holds the color scheme for the entire TUI.
type Theme struct {
	BG      tcell.Color // Main background
	FG      tcell.Color // Default text
	Accent  tcell.Color // Highlights, active tab, selections
	Subtle  tcell.Color // Dimmed text, borders, separators
	Error   tcell.Color // Errors, critical health
	Warning tcell.Color // Warnings, degraded health
	Success tcell.Color // Healthy nodes, confirmations
	Info    tcell.Color // Informational, links

	// Editor token styles
	Keyword     tcell.Color
	String      tcell.Color
	Number      tcell.Color
	Comment     tcell.Color
	Annotation  tcell.Color
	Concept     tcell.Color
	Operator    tcell.Color
	Punctuation tcell.Color
}

// DefaultTheme returns the standard dark color scheme.
func DefaultTheme() Theme {
	return Theme{
		BG: tcell.NewRGBColor(24, 24, 28),
		FG: tcell.NewRGBColor(212, 212, 216),
		// Vivid violet -- distinct from semantic colors (Error red,
		// Warning amber, Success green, Info blue, Concept teal) and far
		// more eye-catching than the prior blue. Used everywhere a focus
		// indicator or "selected" marker needs to read at a glance.
		Accent:  tcell.NewRGBColor(189, 147, 249),
		Subtle:  tcell.NewRGBColor(90, 90, 100),
		Error:   tcell.NewRGBColor(244, 71, 71),
		Warning: tcell.NewRGBColor(229, 192, 123),
		Success: tcell.NewRGBColor(152, 195, 121),
		Info:    tcell.NewRGBColor(97, 175, 239),

		Keyword:     tcell.NewRGBColor(198, 120, 221),
		String:      tcell.NewRGBColor(152, 195, 121),
		Number:      tcell.NewRGBColor(209, 154, 102),
		Comment:     tcell.NewRGBColor(92, 99, 112),
		Annotation:  tcell.NewRGBColor(229, 192, 123),
		Concept:     tcell.NewRGBColor(86, 182, 194),
		Operator:    tcell.NewRGBColor(171, 178, 191),
		Punctuation: tcell.NewRGBColor(92, 99, 112),
	}
}

// Style helpers.

// BaseStyle returns the default style (FG on BG).
func (t Theme) BaseStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(t.FG).Background(t.BG)
}

// AccentStyle returns the accent foreground on default BG.
func (t Theme) AccentStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(t.Accent).Background(t.BG)
}

// SubtleStyle returns dimmed text on default BG.
func (t Theme) SubtleStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(t.Subtle).Background(t.BG)
}

// SelectionStyle returns the highlighted-row style: default FG on a
// slightly-lighter-than-BG slate so the cursor row reads at a glance
// without overwhelming the rest of the pane. Used by ListPane and
// any pane rendering a selectable list. Matches the dark-slate
// hardcoded in concepts/agents/planner today; centralizing it here
// is what lets the migration drop those inline RGB literals.
func (t Theme) SelectionStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(t.FG).Background(tcell.NewRGBColor(40, 44, 52))
}

// ErrorStyle returns error-colored text on default BG.
func (t Theme) ErrorStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(t.Error).Background(t.BG)
}

// TokenStyle returns the style for a MemQL Sense token type.
func (t Theme) TokenStyle(tokenType string) tcell.Style {
	base := tcell.StyleDefault.Background(t.BG)
	switch tokenType {
	case "keyword":
		return base.Foreground(t.Keyword).Bold(true)
	case "string":
		return base.Foreground(t.String)
	case "number":
		return base.Foreground(t.Number)
	case "comment":
		return base.Foreground(t.Comment).Italic(true)
	case "annotation":
		return base.Foreground(t.Annotation)
	case "concept":
		return base.Foreground(t.Concept)
	case "operator":
		return base.Foreground(t.Operator)
	case "punctuation":
		return base.Foreground(t.Punctuation)
	default:
		return base.Foreground(t.FG)
	}
}
