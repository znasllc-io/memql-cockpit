package editor

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// HoverTooltip renders a positioned tooltip popup for hover information.
type HoverTooltip struct {
	Content string // Hover text content (markdown stripped to plain text).
	Visible bool
	AnchorX int
	AnchorY int
	Theme   ui.Theme
}

// NewHoverTooltip creates a hidden hover tooltip.
func NewHoverTooltip(theme ui.Theme) *HoverTooltip {
	return &HoverTooltip{Theme: theme}
}

// Show displays the tooltip at the given position with content.
func (ht *HoverTooltip) Show(content string, anchorX, anchorY int) {
	ht.Content = stripMarkdown(content)
	ht.Visible = true
	ht.AnchorX = anchorX
	ht.AnchorY = anchorY
}

// Hide closes the tooltip.
func (ht *HoverTooltip) Hide() {
	ht.Visible = false
	ht.Content = ""
}

// Draw renders the tooltip overlay.
func (ht *HoverTooltip) Draw(screen *ui.Screen, bounds ui.Rect) {
	if !ht.Visible || ht.Content == "" {
		return
	}

	lines := strings.Split(ht.Content, "\n")
	maxW := 0
	for _, line := range lines {
		if len(line) > maxW {
			maxW = len(line)
		}
	}

	popupW := maxW + 4
	if popupW > bounds.Width-4 {
		popupW = bounds.Width - 4
	}
	popupH := len(lines) + 2
	if popupH > bounds.Height/2 {
		popupH = bounds.Height / 2
	}

	// Position above the anchor.
	popupX := ht.AnchorX
	popupY := ht.AnchorY - popupH
	if popupY < bounds.Y {
		popupY = ht.AnchorY + 1 // Below instead.
	}
	if popupX+popupW > bounds.X+bounds.Width {
		popupX = bounds.X + bounds.Width - popupW
	}

	// Draw border and background.
	bgStyle := tcell.StyleDefault.Background(tcell.NewRGBColor(50, 54, 62)).Foreground(ht.Theme.FG)
	screen.FillRect(popupX, popupY, popupW, popupH, bgStyle)
	screen.DrawBox(popupX, popupY, popupW, popupH, bgStyle.Foreground(ht.Theme.Subtle))

	// Draw content.
	for i, line := range lines {
		if i+1 >= popupH-1 {
			break
		}
		screen.DrawText(popupX+2, popupY+1+i, popupW-4, line, bgStyle)
	}
}

// stripMarkdown does a basic conversion from markdown to plain text.
func stripMarkdown(md string) string {
	md = strings.ReplaceAll(md, "**", "")
	md = strings.ReplaceAll(md, "__", "")
	md = strings.ReplaceAll(md, "`", "")
	md = strings.ReplaceAll(md, "###", "")
	md = strings.ReplaceAll(md, "##", "")
	md = strings.ReplaceAll(md, "#", "")
	return strings.TrimSpace(md)
}
