package editor

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// CompletionItem mirrors a Sense completion suggestion.
type CompletionItem struct {
	Label         string
	Kind          string // keyword, function, concept, annotation, etc.
	Detail        string
	Documentation string
	InsertText    string
	SortPriority  int
}

// CompletionPopup renders and manages an autocomplete popup overlay.
type CompletionPopup struct {
	Items    []CompletionItem
	Filtered []CompletionItem
	Selected int
	Visible  bool
	Filter   string // current filter prefix
	Theme    ui.Theme

	// Position of the popup (set by the editor before drawing).
	AnchorX int
	AnchorY int
}

// NewCompletionPopup creates a hidden completion popup.
func NewCompletionPopup(theme ui.Theme) *CompletionPopup {
	return &CompletionPopup{Theme: theme}
}

// Show displays the popup with the given items at the anchor position.
func (cp *CompletionPopup) Show(items []CompletionItem, anchorX, anchorY int) {
	cp.Items = items
	cp.Filtered = items
	cp.Selected = 0
	cp.Visible = true
	cp.Filter = ""
	cp.AnchorX = anchorX
	cp.AnchorY = anchorY
}

// Hide closes the popup.
func (cp *CompletionPopup) Hide() {
	cp.Visible = false
	cp.Items = nil
	cp.Filtered = nil
	cp.Selected = 0
	cp.Filter = ""
}

// ApplyFilter narrows the completion list by prefix.
func (cp *CompletionPopup) ApplyFilter(prefix string) {
	cp.Filter = prefix
	cp.Filtered = nil
	lower := strings.ToLower(prefix)
	for _, item := range cp.Items {
		if strings.HasPrefix(strings.ToLower(item.Label), lower) {
			cp.Filtered = append(cp.Filtered, item)
		}
	}
	if cp.Selected >= len(cp.Filtered) {
		cp.Selected = 0
	}
	if len(cp.Filtered) == 0 {
		cp.Hide()
	}
}

// SelectedItem returns the currently highlighted completion item.
func (cp *CompletionPopup) SelectedItem() *CompletionItem {
	if cp.Selected >= 0 && cp.Selected < len(cp.Filtered) {
		return &cp.Filtered[cp.Selected]
	}
	return nil
}

// Draw renders the completion popup overlay.
func (cp *CompletionPopup) Draw(screen *ui.Screen, bounds ui.Rect) {
	if !cp.Visible || len(cp.Filtered) == 0 {
		return
	}

	maxVisible := 8
	if len(cp.Filtered) < maxVisible {
		maxVisible = len(cp.Filtered)
	}

	// Determine popup dimensions.
	popupW := 40
	for _, item := range cp.Filtered[:maxVisible] {
		w := len(item.Label) + len(item.Kind) + 4
		if w > popupW {
			popupW = w
		}
	}
	if popupW > bounds.Width-4 {
		popupW = bounds.Width - 4
	}
	popupH := maxVisible

	// Position below the anchor, constrained to screen.
	popupX := cp.AnchorX
	popupY := cp.AnchorY + 1
	if popupY+popupH > bounds.Y+bounds.Height {
		popupY = cp.AnchorY - popupH // Above the cursor.
	}
	if popupX+popupW > bounds.X+bounds.Width {
		popupX = bounds.X + bounds.Width - popupW
	}

	// Draw background.
	bgStyle := tcell.StyleDefault.Background(tcell.NewRGBColor(40, 44, 52)).Foreground(cp.Theme.FG)
	screen.FillRect(popupX, popupY, popupW, popupH, bgStyle)

	// Draw items.
	for i := 0; i < maxVisible; i++ {
		if i >= len(cp.Filtered) {
			break
		}
		item := cp.Filtered[i]
		y := popupY + i
		style := bgStyle
		if i == cp.Selected {
			style = tcell.StyleDefault.Background(cp.Theme.Accent).Foreground(tcell.NewRGBColor(255, 255, 255))
		}

		screen.FillRect(popupX, y, popupW, 1, style)

		// Kind icon.
		kindIcon := completionKindIcon(item.Kind)
		screen.SetCell(popupX+1, y, kindIcon, style.Foreground(cp.Theme.Annotation))

		// Label.
		screen.DrawText(popupX+3, y, popupW-4, item.Label, style)
	}
}

// HandleEvent processes keys for the completion popup.
// Returns: consumed bool, accepted *CompletionItem (non-nil if user selected an item).
func (cp *CompletionPopup) HandleEvent(ev *tcell.EventKey) (bool, *CompletionItem) {
	if !cp.Visible {
		return false, nil
	}

	switch ev.Key() {
	case tcell.KeyUp:
		if cp.Selected > 0 {
			cp.Selected--
		}
		return true, nil
	case tcell.KeyDown:
		if cp.Selected < len(cp.Filtered)-1 {
			cp.Selected++
		}
		return true, nil
	case tcell.KeyEnter, tcell.KeyTab:
		item := cp.SelectedItem()
		cp.Hide()
		return true, item
	case tcell.KeyEscape:
		cp.Hide()
		return true, nil
	}

	return false, nil
}

func completionKindIcon(kind string) rune {
	switch kind {
	case "keyword":
		return 'K'
	case "function":
		return 'F'
	case "concept":
		return 'C'
	case "annotation":
		return '@'
	case "builtin":
		return 'B'
	case "spec":
		return 'S'
	case "shape":
		return 'H'
	case "provider":
		return 'P'
	case "tool":
		return 'T'
	case "field":
		return 'f'
	case "snippet":
		return '#'
	case "receiver":
		return 'R'
	default:
		return '·'
	}
}
