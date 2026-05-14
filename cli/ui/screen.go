package ui

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
)

// Screen wraps a tcell.Screen with convenience methods.
type Screen struct {
	tcell tcell.Screen
}

// NewScreen creates and initializes a tcell screen.
func NewScreen() (*Screen, error) {
	s, err := tcell.NewScreen()
	if err != nil {
		return nil, fmt.Errorf("create screen: %w", err)
	}
	if err := s.Init(); err != nil {
		return nil, fmt.Errorf("init screen: %w", err)
	}
	s.SetStyle(tcell.StyleDefault)
	return &Screen{tcell: s}, nil
}

// EnableInteraction enables mouse and paste support.
// Call after the first draw to avoid escape sequence interference.
func (s *Screen) EnableInteraction() {
	s.tcell.EnableMouse()
	s.tcell.EnablePaste()
	s.tcell.HideCursor()
}

// Inner returns the underlying tcell.Screen for direct access.
func (s *Screen) Inner() tcell.Screen {
	return s.tcell
}

// Size returns the current terminal dimensions (columns, rows).
func (s *Screen) Size() (int, int) {
	return s.tcell.Size()
}

// PollEvent blocks until a terminal event occurs.
func (s *Screen) PollEvent() tcell.Event {
	return s.tcell.PollEvent()
}

// PostEvent injects an event into the event queue (for programmatic triggers).
func (s *Screen) PostEvent(ev tcell.Event) {
	s.tcell.PostEvent(ev)
}

// Clear fills the entire screen with the given style.
func (s *Screen) Clear(style tcell.Style) {
	s.tcell.Fill(' ', style)
}

// Show synchronizes the internal buffer to the terminal.
func (s *Screen) Show() {
	s.tcell.Show()
}

// Sync forces a full terminal resync (use after resize).
func (s *Screen) Sync() {
	s.tcell.Sync()
}

// Fini restores the terminal to its original state.
func (s *Screen) Fini() {
	s.tcell.Fini()
}

// SetCell writes a single character at (x, y) with the given style.
func (s *Screen) SetCell(x, y int, ch rune, style tcell.Style) {
	s.tcell.SetContent(x, y, ch, nil, style)
}

// DrawText writes a string horizontally starting at (x, y), clipped to maxWidth.
func (s *Screen) DrawText(x, y, maxWidth int, text string, style tcell.Style) {
	col := x
	for _, ch := range text {
		if col-x >= maxWidth {
			break
		}
		s.tcell.SetContent(col, y, ch, nil, style)
		col++
	}
}

// DrawHLine draws a horizontal line of the given rune from (x, y) for length cells.
func (s *Screen) DrawHLine(x, y, length int, ch rune, style tcell.Style) {
	for i := 0; i < length; i++ {
		s.SetCell(x+i, y, ch, style)
	}
}

// DrawBox draws a bordered rectangle.
func (s *Screen) DrawBox(x, y, w, h int, style tcell.Style) {
	s.tcell.SetContent(x, y, '┌', nil, style)
	s.tcell.SetContent(x+w-1, y, '┐', nil, style)
	s.tcell.SetContent(x, y+h-1, '└', nil, style)
	s.tcell.SetContent(x+w-1, y+h-1, '┘', nil, style)
	for i := x + 1; i < x+w-1; i++ {
		s.tcell.SetContent(i, y, '─', nil, style)
		s.tcell.SetContent(i, y+h-1, '─', nil, style)
	}
	for i := y + 1; i < y+h-1; i++ {
		s.tcell.SetContent(x, i, '│', nil, style)
		s.tcell.SetContent(x+w-1, i, '│', nil, style)
	}
}

// FillRect fills a rectangle with spaces in the given style (clears an area).
func (s *Screen) FillRect(x, y, w, h int, style tcell.Style) {
	for row := y; row < y+h; row++ {
		for col := x; col < x+w; col++ {
			s.tcell.SetContent(col, row, ' ', nil, style)
		}
	}
}
