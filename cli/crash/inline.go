package crash

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// DrawInline renders an in-pane error placeholder inside bounds when
// a tab's Draw call panicked. It paints a red-tinted message naming
// the error code + log path so the user can hand the code to support
// without leaving the cockpit.
//
// Called by app.go's draw dispatcher when Catch returned a non-nil
// Report. The rest of the cockpit chrome (header, tab bar, other
// tabs available via F1/F2/F3) stays functional.
func DrawInline(screen *ui.Screen, bounds ui.Rect, theme ui.Theme, r *Report) {
	if screen == nil || r == nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	// Fill the pane with the base background so any partial paint
	// from the crashed Draw call gets wiped.
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, theme.BaseStyle())

	// Error title at the top of the pane.
	errStyle := tcell.StyleDefault.Foreground(theme.Error).Bold(true)
	screen.DrawText(bounds.X+2, bounds.Y+1, bounds.Width-3, " SOMETHING WENT WRONG ", errStyle)

	// Body text, wrapped to bounds.Width. Centered vertically in the
	// remaining space so a tall pane doesn't leave the message
	// stranded at the top.
	body := []string{
		"",
		"This tab encountered an unexpected error.",
		"The rest of the cockpit is still usable -- press",
		"F1 / F2 / F3 to switch tabs.",
		"",
		fmt.Sprintf("Error code:  %s", r.Code),
	}
	if r.LogPath != "" {
		body = append(body, fmt.Sprintf("Crash log:   %s", r.LogPath))
	}
	body = append(body,
		"",
		"Please contact support and include the error code above.",
	)

	startY := bounds.Y + 3
	if room := bounds.Height - len(body) - 4; room > 0 {
		startY = bounds.Y + 2 + room/2
	}
	for i, line := range body {
		y := startY + i
		if y >= bounds.Y+bounds.Height-1 {
			break
		}
		style := theme.BaseStyle()
		if strings.HasPrefix(line, "Error code:") || strings.HasPrefix(line, "Crash log:") {
			style = style.Bold(true)
		}
		x := bounds.X + 2
		if avail := bounds.Width - 4; avail > 0 && len(line) < avail {
			// Center each body line.
			x = bounds.X + (bounds.Width-len(line))/2
			if x < bounds.X+2 {
				x = bounds.X + 2
			}
		}
		screen.DrawText(x, y, bounds.Width-3, line, style)
	}

	// Footer with timestamp, hugging the bottom edge.
	footer := fmt.Sprintf(" %s ", r.At.Local().Format(time.RFC822))
	screen.DrawText(bounds.X+1, bounds.Y+bounds.Height-1, bounds.Width-2, footer, theme.SubtleStyle())
}
