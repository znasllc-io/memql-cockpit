// Package explorer provides the file explorer tree view for memQL Cockpit.
package explorer

import (
	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// TreeNode represents a node in the file tree.
type TreeNode struct {
	Name     string
	Path     string // file path or identifier for loading content
	Type     string // "category", "file"
	Icon     rune
	Children []*TreeNode
	Expanded bool
	Depth    int
}

// Tree is the file explorer tree view component.
type Tree struct {
	Root     []*TreeNode // top-level category nodes
	Selected int         // flat index of selected node
	ScrollY  int         // vertical scroll offset
	Theme    ui.Theme

	// OnSelect is called when a file node is selected (Enter pressed).
	OnSelect func(node *TreeNode)

	// flattened is the visible (expanded) node list for rendering/navigation.
	flattened []*TreeNode
}

// NewTree creates a tree with the given root categories.
func NewTree(theme ui.Theme, roots []*TreeNode) *Tree {
	t := &Tree{Root: roots, Theme: theme}
	t.flatten()
	return t
}

// flatten builds the visible node list from expanded state.
func (t *Tree) flatten() {
	t.flattened = nil
	for _, root := range t.Root {
		t.flattenNode(root, 0)
	}
}

func (t *Tree) flattenNode(node *TreeNode, depth int) {
	node.Depth = depth
	t.flattened = append(t.flattened, node)
	if node.Expanded {
		for _, child := range node.Children {
			t.flattenNode(child, depth+1)
		}
	}
}

// SelectedNode returns the currently selected tree node.
func (t *Tree) SelectedNode() *TreeNode {
	if t.Selected >= 0 && t.Selected < len(t.flattened) {
		return t.flattened[t.Selected]
	}
	return nil
}

// Draw renders the tree view.
func (t *Tree) Draw(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, t.Theme.BaseStyle())

	visibleRows := bounds.Height
	for i := 0; i < visibleRows; i++ {
		idx := t.ScrollY + i
		if idx >= len(t.flattened) {
			break
		}
		node := t.flattened[idx]
		y := bounds.Y + i

		// Highlight selected row.
		style := t.Theme.BaseStyle()
		if idx == t.Selected {
			style = tcell.StyleDefault.Foreground(t.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}

		// Clear row.
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)

		// Indent.
		x := bounds.X + node.Depth*2

		// Expand/collapse indicator for categories.
		if node.Type == "category" {
			arrow := '▸'
			if node.Expanded {
				arrow = '▾'
			}
			screen.SetCell(x, y, arrow, style)
			x += 2
		} else {
			x += 2 // align with category children
		}

		// Icon.
		if node.Icon != 0 {
			iconStyle := style
			if node.Type == "category" {
				iconStyle = style.Foreground(t.Theme.Accent)
			}
			screen.SetCell(x, y, node.Icon, iconStyle)
			x += 2
		}

		// Name.
		nameStyle := style
		if node.Type == "category" {
			nameStyle = style.Bold(true)
		}
		maxW := bounds.Width - (x - bounds.X)
		if maxW > 0 {
			screen.DrawText(x, y, maxW, node.Name, nameStyle)
		}
	}
}

// HandleEvent processes keyboard navigation for the tree.
func (t *Tree) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	switch keyEv.Key() {
	case tcell.KeyUp:
		if t.Selected > 0 {
			t.Selected--
			t.ensureVisible()
		}
		return true

	case tcell.KeyDown:
		if t.Selected < len(t.flattened)-1 {
			t.Selected++
			t.ensureVisible()
		}
		return true

	case tcell.KeyRight:
		if node := t.SelectedNode(); node != nil && node.Type == "category" && !node.Expanded {
			node.Expanded = true
			t.flatten()
		}
		return true

	case tcell.KeyLeft:
		if node := t.SelectedNode(); node != nil && node.Type == "category" && node.Expanded {
			node.Expanded = false
			t.flatten()
		}
		return true

	case tcell.KeyEnter:
		node := t.SelectedNode()
		if node == nil {
			return false
		}
		if node.Type == "category" {
			node.Expanded = !node.Expanded
			t.flatten()
		} else if t.OnSelect != nil {
			t.OnSelect(node)
		}
		return true
	}

	return false
}

func (t *Tree) ensureVisible() {
	// Scrolling is adjusted externally based on viewport height.
	// For now, just keep selected in bounds.
	if t.Selected < t.ScrollY {
		t.ScrollY = t.Selected
	}
}

// AdjustScroll ensures the selected item is visible within the given height.
func (t *Tree) AdjustScroll(viewportHeight int) {
	if t.Selected < t.ScrollY {
		t.ScrollY = t.Selected
	}
	if t.Selected >= t.ScrollY+viewportHeight {
		t.ScrollY = t.Selected - viewportHeight + 1
	}
}
