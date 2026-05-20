package editor

import (
	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/sdk/go/sense"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// StyledSpan maps a character range within a line to a tcell style.
type StyledSpan struct {
	Start int // column (0-based)
	End   int // exclusive
	Style tcell.Style
}

// HighlightMap builds a per-line style map from Sense tokens.
// Returns map[lineIndex][]StyledSpan (0-based line index).
func HighlightMap(tokens []sense.Token, theme ui.Theme) map[int][]StyledSpan {
	result := make(map[int][]StyledSpan)
	for _, tok := range tokens {
		if tok.Range.Start.Line < 1 {
			continue
		}
		lineIdx := tok.Range.Start.Line - 1
		style := theme.TokenStyle(tok.Type)
		result[lineIdx] = append(result[lineIdx], StyledSpan{
			Start: tok.Range.Start.Column - 1,
			End:   tok.Range.End.Column - 1,
			Style: style,
		})
	}
	return result
}

// DiagnosticMap builds a per-line diagnostic list from Sense diagnostics.
// Returns map[lineIndex][]sense.Diagnostic (0-based line index).
func DiagnosticMap(diagnostics []sense.Diagnostic) map[int][]sense.Diagnostic {
	result := make(map[int][]sense.Diagnostic)
	for _, d := range diagnostics {
		if d.Range.Start.Line < 1 {
			continue
		}
		lineIdx := d.Range.Start.Line - 1
		result[lineIdx] = append(result[lineIdx], d)
	}
	return result
}

// DiagnosticGutterIcon returns the gutter rune and style for the highest-severity
// diagnostic on a line. Returns 0 rune if no diagnostics.
func DiagnosticGutterIcon(diagnostics []sense.Diagnostic, theme ui.Theme) (rune, tcell.Style) {
	if len(diagnostics) == 0 {
		return 0, tcell.StyleDefault
	}
	// Find highest severity (lowest number = highest severity).
	best := diagnostics[0]
	for _, d := range diagnostics[1:] {
		if d.Severity < best.Severity {
			best = d
		}
	}
	switch best.Severity {
	case 1: // Error
		return '●', tcell.StyleDefault.Foreground(theme.Error).Background(theme.BG)
	case 2: // Warning
		return '●', tcell.StyleDefault.Foreground(theme.Warning).Background(theme.BG)
	case 3: // Info
		return 'ℹ', tcell.StyleDefault.Foreground(theme.Info).Background(theme.BG)
	default:
		return '·', tcell.StyleDefault.Foreground(theme.Subtle).Background(theme.BG)
	}
}
