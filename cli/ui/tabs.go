package ui

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
)

// Tab represents a single tab in the tab bar.
type Tab struct {
	Name    string
	Content Drawable
}

// Drawable is any component that can render itself within a bounding rectangle.
type Drawable interface {
	Draw(screen *Screen, bounds Rect)
	HandleEvent(ev tcell.Event) bool // returns true if event was consumed
}

// TabBar manages a set of tabs rendered at the bottom of the screen.
type TabBar struct {
	Tabs        []Tab
	ActiveIndex int
	Theme       Theme
}

// NewTabBar creates a tab bar with the given tab definitions.
func NewTabBar(theme Theme, tabs ...Tab) *TabBar {
	return &TabBar{
		Tabs:  tabs,
		Theme: theme,
	}
}

// ActiveTab returns the currently active tab.
func (tb *TabBar) ActiveTab() *Tab {
	if tb.ActiveIndex >= 0 && tb.ActiveIndex < len(tb.Tabs) {
		return &tb.Tabs[tb.ActiveIndex]
	}
	return nil
}

// SetActive switches to the tab at the given index.
func (tb *TabBar) SetActive(index int) {
	if index >= 0 && index < len(tb.Tabs) {
		tb.ActiveIndex = index
	}
}

// Draw renders the tab bar at the bottom of the screen (1 row).
func (tb *TabBar) Draw(screen *Screen, y, width int) {
	// Clear the tab bar row.
	screen.FillRect(0, y, width, 1, tb.Theme.BaseStyle().Background(tcell.NewRGBColor(18, 18, 22)))

	barBG := tcell.NewRGBColor(18, 18, 22)
	x := 1

	for i, tab := range tb.Tabs {
		label := fmt.Sprintf(" %d:%s ", i+1, tab.Name)

		var style tcell.Style
		if i == tb.ActiveIndex {
			style = tcell.StyleDefault.Foreground(tcell.NewRGBColor(255, 255, 255)).Background(tb.Theme.Accent).Bold(true)
		} else {
			style = tcell.StyleDefault.Foreground(tb.Theme.Subtle).Background(barBG)
		}

		screen.DrawText(x, y, len(label), label, style)
		x += len(label) + 1
	}

	// Draw key hints on the right. Modifier label is OS-aware -- macOS
	// calls it Option, everything else calls it Alt.
	hint := fmt.Sprintf("F1..F4 / %s+1..4:Tabs  Ctrl+Q:Quit", AltKey())
	hintStyle := tcell.StyleDefault.Foreground(tb.Theme.Subtle).Background(barBG)
	hintStart := width - len(hint) - 1
	if hintStart > x {
		screen.DrawText(hintStart, y, len(hint), hint, hintStyle)
	}
}

// HandleKey checks for tab switching keys (Ctrl+1 through Ctrl+4).
// Returns the new active index or -1 if the key was not a tab switch.
func (tb *TabBar) HandleKey(ev *tcell.EventKey) int {
	// Ctrl+1 through Ctrl+4 (some terminals send these as Alt+1..4).
	if ev.Modifiers()&tcell.ModAlt != 0 {
		switch ev.Rune() {
		case '1':
			return 0
		case '2':
			return 1
		case '3':
			return 2
		case '4':
			return 3
		}
	}
	// F1-F4 as alternative.
	switch ev.Key() {
	case tcell.KeyF1:
		return 0
	case tcell.KeyF2:
		return 1
	case tcell.KeyF3:
		return 2
	case tcell.KeyF4:
		return 3
	}
	return -1
}
