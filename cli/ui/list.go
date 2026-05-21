package ui

import "github.com/gdamore/tcell/v2"

// RowRenderer paints one item's row(s) inside ListPane. The
// `bounds` rectangle has `Height == RowsPerItem` -- a 1-row renderer
// gets a 1-row Rect, a 2-row renderer gets 2 rows, etc. `idx` is
// the item index in the parent's data slice; `selected` is true for
// the cursor row (the highlighted item).
//
// Callers DO NOT need to paint a selection background -- ListPane
// fills `bounds` with the selection style before invoking the
// renderer when `selected` is true. The renderer just draws content.
type RowRenderer func(screen *Screen, bounds Rect, idx int, selected bool, theme Theme)

// ListPane renders a scrollable, selectable list of items inside a
// bounded Rect. The widget is intentionally index-based: the parent
// owns the data slice, ListPane only knows there are `Count` items
// and asks the parent to paint each visible one via `Render`. This
// keeps ListPane fully type-safe at the call site (no `any` boxing,
// no type assertions) and lets a single ListPane back any data
// shape -- concept rows, plans, tasks, agents, clusters, etc.
//
// Scroll math reuses ScrollTo / VisibleRange / DrawScrollbar from
// scroll.go. RowsPerItem makes 2-row items (e.g. plan list with
// dimmed subtitle row) first-class: the viewport is computed in
// item-space, and each item's bounds span RowsPerItem rows.
//
// Selection is the "highlight" (cursor) per the chrome contract --
// it moves with arrow keys and never persists across renders.
// "Selected" / "active" items in the contract sense (marked with
// `*`) are the parent's responsibility, rendered inside RowRenderer
// using whatever marker glyph fits the data shape.
type ListPane struct {
	Count        int         // number of items
	RowsPerItem  int         // default 1; e.g. 2 for plan list (primary + subtitle)
	Selected     int         // highlighted (cursor) item index
	ScrollY      int         // item-space scroll offset
	Render       RowRenderer // callback to paint each visible row
	Focused      bool        // when true, HandleEvent consumes nav keys
	EmptyMessage string      // shown when Count == 0

	// viewportItemsCache is updated each Draw so HandleEvent's PgUp /
	// PgDn can size their jumps using whatever bounds Draw last saw.
	// Unexported -- not part of the call-site API.
	viewportItemsCache int
}

// Draw paints the visible items into `bounds`. Auto-clamps Selected
// and ScrollY so callers don't have to clamp before drawing.
func (l *ListPane) Draw(screen *Screen, bounds Rect, theme Theme) {
	if screen == nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return
	}
	rpi := l.rowsPerItem()

	if l.Count == 0 {
		if l.EmptyMessage != "" {
			y := bounds.Y + bounds.Height/2
			screen.DrawText(bounds.X+1, y, bounds.Width-2, l.EmptyMessage, theme.SubtleStyle())
		}
		return
	}

	// Clamp Selected to [0, Count).
	if l.Selected < 0 {
		l.Selected = 0
	}
	if l.Selected >= l.Count {
		l.Selected = l.Count - 1
	}

	// Items that fit vertically.
	viewportItems := bounds.Height / rpi
	if viewportItems <= 0 {
		// Bounds too small to render even one item; nothing to do.
		return
	}
	l.viewportItemsCache = viewportItems

	l.ScrollY = ScrollTo(l.ScrollY, l.Selected, l.Count, viewportItems)

	start, end := VisibleRange(l.ScrollY, l.Count, viewportItems)
	innerWidth := bounds.Width
	// Leave one column for the scrollbar when needed so the
	// rightmost cell of every row doesn't overlap the indicator.
	scrollbarVisible := l.Count > viewportItems
	if scrollbarVisible {
		innerWidth = bounds.Width - 1
		if innerWidth < 1 {
			innerWidth = 1
		}
	}

	for i := start; i < end; i++ {
		rowY := bounds.Y + (i-l.ScrollY)*rpi
		isSel := i == l.Selected
		itemBounds := Rect{
			X:      bounds.X,
			Y:      rowY,
			Width:  innerWidth,
			Height: rpi,
		}
		if isSel {
			screen.FillRect(itemBounds.X, itemBounds.Y, itemBounds.Width, itemBounds.Height, theme.SelectionStyle())
		}
		if l.Render != nil {
			l.Render(screen, itemBounds, i, isSel, theme)
		}
	}

	if scrollbarVisible {
		sbBounds := Rect{
			X:      bounds.X,
			Y:      bounds.Y,
			Width:  bounds.Width,
			Height: viewportItems * rpi,
		}
		// Thumb tracks the user's CURSOR (Selected), not the
		// viewport top edge. Two reasons: (1) it's the position the
		// user moves directly, so the thumb stays under their mental
		// model; (2) it dodges the unit-mismatch bug -- previously
		// the call was (ScrollY in items, Count in items) but the
		// widget treated bounds.Height as if it were the totalRows
		// argument, mixing item-units and row-units. Selected and
		// Count-1 share the same unit (item index) end-to-end.
		DrawScrollbar(screen, theme, sbBounds, l.Selected, l.Count-1)
	}
}

// HandleEvent processes navigation keys when Focused is true.
// Returns true if the event was consumed. When not focused, returns
// false unconditionally so the parent can route the key elsewhere.
//
// Bindings (focused only):
//
//	↑ / k       Move cursor up one item
//	↓ / j       Move cursor down one item
//	PgUp        Move cursor up one viewport
//	PgDn        Move cursor down one viewport
//	Home        Jump to first item
//	End         Jump to last item
//
// PgUp / PgDn use the LAST known viewport size (set by the most
// recent Draw). Before any Draw, they fall back to a one-item step.
func (l *ListPane) HandleEvent(ev tcell.Event) bool {
	if !l.Focused {
		return false
	}
	key, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}
	if l.Count == 0 {
		return false
	}

	page := l.viewportItemsCache
	if page <= 0 {
		page = 1
	}

	switch key.Key() {
	case tcell.KeyUp:
		l.moveSelection(-1)
		return true
	case tcell.KeyDown:
		l.moveSelection(1)
		return true
	case tcell.KeyPgUp:
		l.moveSelection(-page)
		return true
	case tcell.KeyPgDn:
		l.moveSelection(page)
		return true
	case tcell.KeyHome:
		l.Selected = 0
		l.ScrollY = 0
		return true
	case tcell.KeyEnd:
		l.Selected = l.Count - 1
		// Draw will clamp ScrollY via ScrollTo on the next paint.
		return true
	case tcell.KeyRune:
		switch key.Rune() {
		case 'k':
			l.moveSelection(-1)
			return true
		case 'j':
			l.moveSelection(1)
			return true
		}
	}
	return false
}

// EnsureVisible adjusts ScrollY so Selected falls inside the
// viewport. Draw already calls this internally with the bounds it
// received; callers should only need this directly in tests or when
// programmatically jumping Selected far without a redraw.
func (l *ListPane) EnsureVisible(viewportItems int) {
	if l.Count == 0 || viewportItems <= 0 {
		return
	}
	l.ScrollY = ScrollTo(l.ScrollY, l.Selected, l.Count, viewportItems)
}

// rowsPerItem returns the effective rows-per-item, treating zero
// as 1 so the zero-value ListPane behaves like a simple 1-row list.
func (l *ListPane) rowsPerItem() int {
	if l.RowsPerItem <= 0 {
		return 1
	}
	return l.RowsPerItem
}

func (l *ListPane) moveSelection(delta int) {
	l.Selected += delta
	if l.Selected < 0 {
		l.Selected = 0
	}
	if l.Selected >= l.Count {
		l.Selected = l.Count - 1
	}
}
