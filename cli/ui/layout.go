package ui

// Rect represents a rectangular region on screen.
type Rect struct {
	X, Y, Width, Height int
}

// FlexItem describes a panel in a flex layout.
type FlexItem struct {
	// Fixed is a fixed size in cells (0 means use Flex proportion).
	Fixed int
	// Flex is the proportional weight when Fixed is 0.
	Flex float64
	// MinSize is the minimum size in cells.
	MinSize int
}

// FlexRow distributes the total height among items vertically.
// Returns a Rect for each item within the given bounds.
func FlexRow(bounds Rect, items []FlexItem) []Rect {
	return flexLayout(bounds, items, false)
}

// FlexColumn distributes the total width among items horizontally.
// Returns a Rect for each item within the given bounds.
func FlexColumn(bounds Rect, items []FlexItem) []Rect {
	return flexLayout(bounds, items, true)
}

func flexLayout(bounds Rect, items []FlexItem, horizontal bool) []Rect {
	total := bounds.Width
	if !horizontal {
		total = bounds.Height
	}

	// First pass: allocate fixed items, compute flex total.
	remaining := total
	flexTotal := 0.0
	for _, item := range items {
		if item.Fixed > 0 {
			remaining -= item.Fixed
		} else {
			flexTotal += item.Flex
		}
	}
	if remaining < 0 {
		remaining = 0
	}

	// Second pass: compute sizes.
	sizes := make([]int, len(items))
	for i, item := range items {
		if item.Fixed > 0 {
			sizes[i] = item.Fixed
		} else if flexTotal > 0 {
			sizes[i] = int(float64(remaining) * item.Flex / flexTotal)
		}
		if sizes[i] < item.MinSize {
			sizes[i] = item.MinSize
		}
	}

	// Third pass: build rects.
	rects := make([]Rect, len(items))
	offset := 0
	for i, size := range sizes {
		if horizontal {
			rects[i] = Rect{X: bounds.X + offset, Y: bounds.Y, Width: size, Height: bounds.Height}
		} else {
			rects[i] = Rect{X: bounds.X, Y: bounds.Y + offset, Width: bounds.Width, Height: size}
		}
		offset += size
	}
	return rects
}
