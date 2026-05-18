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
	// calls it Option, everything else calls it Alt. The range tracks
	// the registered tab count so adding a tab automatically extends
	// the advertised hotkey range (previously hardcoded to F1..F4 /
	// Alt+1..4, which became misleading the moment the Agents + Chat
	// tabs landed and pushed Settings out to F6 / Alt+6).
	hint := tb.hintText()
	hintStyle := tcell.StyleDefault.Foreground(tb.Theme.Subtle).Background(barBG)
	hintStart := width - len(hint) - 1
	if hintStart > x {
		screen.DrawText(hintStart, y, len(hint), hint, hintStyle)
	}
}

// hintText builds the bottom-row chrome string for the current tab
// count. Caps the F-key range at the number of registered tabs (the
// `maxTabs` ceiling below mirrors the HandleKey ceiling), and the
// Alt-digit range at min(tabs, 9) since Alt+0 collides with terminal
// keymaps on most platforms.
func (tb *TabBar) hintText() string {
	n := len(tb.Tabs)
	if n <= 0 {
		return fmt.Sprintf("%s+1..N:Tabs  Ctrl+Q:Quit", AltKey())
	}
	if n > maxTabs {
		n = maxTabs
	}
	altMax := n
	if altMax > 9 {
		altMax = 9
	}
	if n == 1 {
		return fmt.Sprintf("F1 / %s+1:Tab  Ctrl+Q:Quit", AltKey())
	}
	return fmt.Sprintf("F1..F%d / %s+1..%d:Tabs  Ctrl+Q:Quit", n, AltKey(), altMax)
}

// maxTabs caps the dynamic key-binding range so a runaway caller
// registering hundreds of tabs doesn't try to map F1..Fn for n>12.
// tcell exposes KeyF1..KeyF12 (KeyF13+ exist but no terminal sends
// them in practice); twelve tabs is already well past any sensible UI.
const maxTabs = 12

// HandleKey checks for tab-switching keys. Returns the new active
// index or -1 if the key wasn't a tab switch. Two routes:
//
//   - Alt+1 through Alt+9 (some terminals deliver Ctrl+digit as
//     ModAlt+digit, so the bind name doubles as both). Alt+0 is
//     deliberately skipped -- some terminal emulators consume it
//     for window/profile switching.
//   - F1 through F12. Mapped one-to-one to tab indices; only the
//     prefix matching the actual registered tab count fires, so
//     pressing F11 with five tabs is a no-op rather than a
//     panic / wraparound.
func (tb *TabBar) HandleKey(ev *tcell.EventKey) int {
	tabCount := len(tb.Tabs)
	if tabCount <= 0 {
		return -1
	}
	if tabCount > maxTabs {
		tabCount = maxTabs
	}

	// Alt + digit. Map '1' -> index 0, '2' -> 1, ... up to '9' -> 8.
	if ev.Modifiers()&tcell.ModAlt != 0 {
		r := ev.Rune()
		if r >= '1' && r <= '9' {
			idx := int(r - '1')
			if idx < tabCount {
				return idx
			}
		}
	}

	// F-keys. KeyF1 -> index 0, KeyF2 -> 1, ..., KeyF12 -> 11. tcell's
	// F-key constants are sequential ints so a single subtraction
	// gives the index; the bounds check rejects F-keys beyond the
	// registered tab count and any non-F-key.
	k := ev.Key()
	if k >= tcell.KeyF1 && k <= tcell.KeyF12 {
		idx := int(k - tcell.KeyF1)
		if idx < tabCount {
			return idx
		}
	}
	return -1
}
