// Package editor provides a terminal text editor component with MemQL Sense
// integration for syntax highlighting, diagnostics, and autocompletion.
package editor

import "strings"

// Buffer holds the text content of an open file as a slice of lines.
type Buffer struct {
	Lines    []string
	FilePath string
	ReadOnly bool
}

// NewBuffer creates a buffer from source text.
func NewBuffer(source, filePath string, readOnly bool) *Buffer {
	lines := strings.Split(source, "\n")
	if len(lines) == 0 {
		lines = []string{""}
	}
	return &Buffer{
		Lines:    lines,
		FilePath: filePath,
		ReadOnly: readOnly,
	}
}

// LineCount returns the number of lines in the buffer.
func (b *Buffer) LineCount() int {
	return len(b.Lines)
}

// Line returns the text at line index i (0-based). Returns "" if out of bounds.
func (b *Buffer) Line(i int) string {
	if i < 0 || i >= len(b.Lines) {
		return ""
	}
	return b.Lines[i]
}

// Source returns the full buffer content as a single string.
func (b *Buffer) Source() string {
	return strings.Join(b.Lines, "\n")
}

// InsertChar inserts a character at the given position (0-based line and col).
func (b *Buffer) InsertChar(line, col int, ch rune) {
	if b.ReadOnly || line < 0 || line >= len(b.Lines) {
		return
	}
	l := b.Lines[line]
	if col < 0 {
		col = 0
	}
	if col > len(l) {
		col = len(l)
	}
	b.Lines[line] = l[:col] + string(ch) + l[col:]
}

// DeleteChar removes the character before the cursor (backspace).
// Returns the new cursor position (line, col).
func (b *Buffer) DeleteChar(line, col int) (int, int) {
	if b.ReadOnly {
		return line, col
	}
	if col > 0 && line >= 0 && line < len(b.Lines) {
		l := b.Lines[line]
		if col <= len(l) {
			b.Lines[line] = l[:col-1] + l[col:]
		}
		return line, col - 1
	}
	// At beginning of line — merge with previous line.
	if col == 0 && line > 0 {
		prevLen := len(b.Lines[line-1])
		b.Lines[line-1] += b.Lines[line]
		b.Lines = append(b.Lines[:line], b.Lines[line+1:]...)
		return line - 1, prevLen
	}
	return line, col
}

// InsertNewline splits the current line at the cursor position.
func (b *Buffer) InsertNewline(line, col int) {
	if b.ReadOnly || line < 0 || line >= len(b.Lines) {
		return
	}
	l := b.Lines[line]
	if col > len(l) {
		col = len(l)
	}
	before := l[:col]
	after := l[col:]
	b.Lines[line] = before
	// Insert new line after current.
	newLines := make([]string, len(b.Lines)+1)
	copy(newLines, b.Lines[:line+1])
	newLines[line+1] = after
	copy(newLines[line+2:], b.Lines[line+1:])
	b.Lines = newLines
}

// DeleteLine removes the line at index i.
func (b *Buffer) DeleteLine(i int) {
	if b.ReadOnly || i < 0 || i >= len(b.Lines) {
		return
	}
	if len(b.Lines) == 1 {
		b.Lines[0] = ""
		return
	}
	b.Lines = append(b.Lines[:i], b.Lines[i+1:]...)
}
