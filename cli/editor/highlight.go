package editor

import (
	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// StyledSpan maps a character range within a line to a tcell style.
type StyledSpan struct {
	Start int // column (0-based)
	End   int // exclusive
	Style tcell.Style
}

// SenseToken mirrors the data from a SenseTokenizeResult entry.
type SenseToken struct {
	Type    string // keyword, identifier, string, number, operator, annotation, comment, punctuation, concept
	Literal string
	StartLine int // 1-indexed
	StartCol  int // 1-indexed
	EndLine   int // 1-indexed
	EndCol    int // 1-indexed
}

// SenseDiagnostic mirrors the data from a SenseDiagnoseResult entry.
type SenseDiagnostic struct {
	StartLine int // 1-indexed
	StartCol  int // 1-indexed
	EndLine   int // 1-indexed
	EndCol    int // 1-indexed
	Severity  int // 1=Error, 2=Warning, 3=Info, 4=Hint
	Message   string
	Code      string
}

// HighlightMap builds a per-line style map from Sense tokens.
// Returns map[lineIndex][]StyledSpan (0-based line index).
func HighlightMap(tokens []SenseToken, theme ui.Theme) map[int][]StyledSpan {
	result := make(map[int][]StyledSpan)
	for _, tok := range tokens {
		if tok.StartLine < 1 {
			continue
		}
		lineIdx := tok.StartLine - 1
		style := theme.TokenStyle(tok.Type)
		result[lineIdx] = append(result[lineIdx], StyledSpan{
			Start: tok.StartCol - 1,
			End:   tok.EndCol - 1,
			Style: style,
		})
	}
	return result
}

// DiagnosticMap builds a per-line diagnostic list from Sense diagnostics.
// Returns map[lineIndex][]SenseDiagnostic (0-based line index).
func DiagnosticMap(diagnostics []SenseDiagnostic) map[int][]SenseDiagnostic {
	result := make(map[int][]SenseDiagnostic)
	for _, d := range diagnostics {
		if d.StartLine < 1 {
			continue
		}
		lineIdx := d.StartLine - 1
		result[lineIdx] = append(result[lineIdx], d)
	}
	return result
}

// DiagnosticGutterIcon returns the gutter rune and style for the highest-severity
// diagnostic on a line. Returns 0 rune if no diagnostics.
func DiagnosticGutterIcon(diagnostics []SenseDiagnostic, theme ui.Theme) (rune, tcell.Style) {
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
