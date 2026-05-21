package editor

import (
	"fmt"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/sdk/go/sense"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

const gutterWidth = 5 // line numbers + diagnostic icon

// Editor is a text editor component with syntax highlighting and diagnostics.
type Editor struct {
	Buffer     *Buffer
	Theme      ui.Theme
	CursorLine int // 0-based
	CursorCol  int // 0-based
	ScrollY    int // first visible line
	ScrollX    int // horizontal scroll offset

	// Sense data (updated asynchronously from gRPC).
	Tokens      []sense.Token
	Diagnostics []sense.Diagnostic

	// Cached maps rebuilt when Sense data changes.
	highlights    map[int][]StyledSpan
	diagnosticMap map[int][]sense.Diagnostic
	dirty         bool // true when Sense data needs re-mapping

	// Status
	ErrorCount   int
	WarningCount int
}

// NewEditor creates an editor with the given buffer and theme.
func NewEditor(buf *Buffer, theme ui.Theme) *Editor {
	return &Editor{
		Buffer: buf,
		Theme:  theme,
	}
}

// SetTokens updates the syntax highlighting tokens from Sense.
func (e *Editor) SetTokens(tokens []sense.Token) {
	e.Tokens = tokens
	e.highlights = HighlightMap(tokens, e.Theme)
}

// SetDiagnostics updates the diagnostic markers from Sense.
func (e *Editor) SetDiagnostics(diagnostics []sense.Diagnostic) {
	e.Diagnostics = diagnostics
	e.diagnosticMap = DiagnosticMap(diagnostics)

	// Count errors and warnings.
	e.ErrorCount = 0
	e.WarningCount = 0
	for _, d := range diagnostics {
		switch d.Severity {
		case 1:
			e.ErrorCount++
		case 2:
			e.WarningCount++
		}
	}
}

// Draw renders the editor within the given bounds.
func (e *Editor) Draw(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, e.Theme.BaseStyle())

	if e.Buffer == nil {
		msg := "No file open"
		screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-4, msg, e.Theme.SubtleStyle())
		return
	}

	visibleLines := bounds.Height - 1 // reserve 1 row for status bar
	textAreaX := bounds.X + gutterWidth
	textAreaW := bounds.Width - gutterWidth

	// Ensure scroll keeps cursor visible.
	e.ensureVisible(visibleLines)

	// Draw lines.
	for i := 0; i < visibleLines; i++ {
		lineIdx := e.ScrollY + i
		y := bounds.Y + i

		if lineIdx >= e.Buffer.LineCount() {
			// Draw tilde for empty area.
			screen.SetCell(bounds.X+gutterWidth-2, y, '~', e.Theme.SubtleStyle())
			continue
		}

		// Gutter: diagnostic icon.
		if diags, ok := e.diagnosticMap[lineIdx]; ok && len(diags) > 0 {
			icon, style := DiagnosticGutterIcon(diags, e.Theme)
			if icon != 0 {
				screen.SetCell(bounds.X, y, icon, style)
			}
		}

		// Gutter: line number.
		lineNum := fmt.Sprintf("%3d ", lineIdx+1)
		screen.DrawText(bounds.X+1, y, gutterWidth-1, lineNum, e.Theme.SubtleStyle())

		// Text area: render line with syntax highlighting.
		line := e.Buffer.Line(lineIdx)
		e.drawLine(screen, textAreaX, y, textAreaW, lineIdx, line)

		// Cursor.
		if lineIdx == e.CursorLine {
			cursorX := textAreaX + e.CursorCol - e.ScrollX
			if cursorX >= textAreaX && cursorX < textAreaX+textAreaW {
				ch := ' '
				if e.CursorCol < len(line) {
					ch = rune(line[e.CursorCol])
				}
				cursorStyle := tcell.StyleDefault.Foreground(e.Theme.BG).Background(e.Theme.FG)
				screen.SetCell(cursorX, y, ch, cursorStyle)
			}
		}
	}

	// Status bar at the bottom of the editor.
	statusY := bounds.Y + bounds.Height - 1
	e.drawStatusBar(screen, bounds.X, statusY, bounds.Width)
}

// drawLine renders a single line with syntax highlighting applied.
func (e *Editor) drawLine(screen *ui.Screen, x, y, maxW, lineIdx int, line string) {
	spans := e.highlights[lineIdx]

	// Build a style for each character.
	baseStyle := e.Theme.BaseStyle()
	col := 0
	for _, ch := range line {
		if col-e.ScrollX >= maxW {
			break
		}
		if col-e.ScrollX < 0 {
			col++
			continue
		}

		style := baseStyle
		for _, span := range spans {
			if col >= span.Start && col < span.End {
				style = span.Style
				break
			}
		}

		// Underline for diagnostic ranges.
		if diags, ok := e.diagnosticMap[lineIdx]; ok {
			for _, d := range diags {
				if col >= d.Range.Start.Column-1 && col < d.Range.End.Column-1 {
					if d.Severity == 1 {
						style = style.Underline(true).Foreground(e.Theme.Error)
					} else if d.Severity == 2 {
						style = style.Underline(true)
					}
					break
				}
			}
		}

		screen.SetCell(x+col-e.ScrollX, y, ch, style)
		col++
	}
}

// drawStatusBar renders the editor status line.
func (e *Editor) drawStatusBar(screen *ui.Screen, x, y, w int) {
	barStyle := tcell.StyleDefault.Foreground(e.Theme.FG).Background(tcell.NewRGBColor(30, 33, 39))
	screen.FillRect(x, y, w, 1, barStyle)

	// File name.
	name := e.Buffer.FilePath
	if e.Buffer.ReadOnly {
		name += " [readonly]"
	}
	screen.DrawText(x+1, y, w/2, name, barStyle)

	// Cursor position.
	pos := fmt.Sprintf("Ln %d, Col %d", e.CursorLine+1, e.CursorCol+1)
	screen.DrawText(x+w-len(pos)-15, y, len(pos), pos, barStyle)

	// Diagnostic counts.
	if e.ErrorCount > 0 {
		errText := fmt.Sprintf(" %d errors", e.ErrorCount)
		errStyle := barStyle.Foreground(e.Theme.Error)
		screen.DrawText(x+w-len(errText)-1, y, len(errText), errText, errStyle)
	} else if e.WarningCount > 0 {
		warnText := fmt.Sprintf(" %d warnings", e.WarningCount)
		warnStyle := barStyle.Foreground(e.Theme.Warning)
		screen.DrawText(x+w-len(warnText)-1, y, len(warnText), warnText, warnStyle)
	}
}

// HandleEvent processes keyboard input for the editor.
func (e *Editor) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	if e.Buffer == nil {
		return false
	}

	switch keyEv.Key() {
	case tcell.KeyUp:
		if e.CursorLine > 0 {
			e.CursorLine--
			e.clampCol()
		}
		return true
	case tcell.KeyDown:
		if e.CursorLine < e.Buffer.LineCount()-1 {
			e.CursorLine++
			e.clampCol()
		}
		return true
	case tcell.KeyLeft:
		if e.CursorCol > 0 {
			e.CursorCol--
		} else if e.CursorLine > 0 {
			e.CursorLine--
			e.CursorCol = len(e.Buffer.Line(e.CursorLine))
		}
		return true
	case tcell.KeyRight:
		lineLen := len(e.Buffer.Line(e.CursorLine))
		if e.CursorCol < lineLen {
			e.CursorCol++
		} else if e.CursorLine < e.Buffer.LineCount()-1 {
			e.CursorLine++
			e.CursorCol = 0
		}
		return true
	case tcell.KeyHome:
		e.CursorCol = 0
		return true
	case tcell.KeyEnd:
		e.CursorCol = len(e.Buffer.Line(e.CursorLine))
		return true
	case tcell.KeyPgUp:
		e.CursorLine -= 20
		if e.CursorLine < 0 {
			e.CursorLine = 0
		}
		e.clampCol()
		return true
	case tcell.KeyPgDn:
		e.CursorLine += 20
		if e.CursorLine >= e.Buffer.LineCount() {
			e.CursorLine = e.Buffer.LineCount() - 1
		}
		e.clampCol()
		return true

	// Editing keys (only in non-readonly mode).
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if !e.Buffer.ReadOnly {
			e.CursorLine, e.CursorCol = e.Buffer.DeleteChar(e.CursorLine, e.CursorCol)
			e.dirty = true
		}
		return true
	case tcell.KeyEnter:
		if !e.Buffer.ReadOnly {
			e.Buffer.InsertNewline(e.CursorLine, e.CursorCol)
			e.CursorLine++
			e.CursorCol = 0
			e.dirty = true
		}
		return true
	case tcell.KeyRune:
		if !e.Buffer.ReadOnly {
			e.Buffer.InsertChar(e.CursorLine, e.CursorCol, keyEv.Rune())
			e.CursorCol++
			e.dirty = true
		}
		return true
	}

	return false
}

// IsDirty returns true if the buffer was modified since last Sense update.
func (e *Editor) IsDirty() bool {
	return e.dirty
}

// ClearDirty resets the dirty flag (call after sending Sense request).
func (e *Editor) ClearDirty() {
	e.dirty = false
}

func (e *Editor) ensureVisible(viewportHeight int) {
	if e.CursorLine < e.ScrollY {
		e.ScrollY = e.CursorLine
	}
	if e.CursorLine >= e.ScrollY+viewportHeight {
		e.ScrollY = e.CursorLine - viewportHeight + 1
	}
}

func (e *Editor) clampCol() {
	lineLen := len(e.Buffer.Line(e.CursorLine))
	if e.CursorCol > lineLen {
		e.CursorCol = lineLen
	}
}
