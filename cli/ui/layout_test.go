package ui

import "testing"

func TestFlexColumn(t *testing.T) {
	bounds := Rect{X: 0, Y: 0, Width: 100, Height: 50}
	items := []FlexItem{
		{Flex: 0.3, MinSize: 10},
		{Flex: 0.7, MinSize: 10},
	}

	rects := FlexColumn(bounds, items)

	if len(rects) != 2 {
		t.Fatalf("expected 2 rects, got %d", len(rects))
	}

	// First pane should be ~30% width.
	if rects[0].Width < 25 || rects[0].Width > 35 {
		t.Errorf("expected first pane ~30 wide, got %d", rects[0].Width)
	}

	// Second pane should be ~70% width.
	if rects[1].Width < 65 || rects[1].Width > 75 {
		t.Errorf("expected second pane ~70 wide, got %d", rects[1].Width)
	}

	// Panes should be adjacent.
	if rects[1].X != rects[0].X+rects[0].Width {
		t.Errorf("panes should be adjacent: pane1 ends at %d, pane2 starts at %d",
			rects[0].X+rects[0].Width, rects[1].X)
	}
}

func TestFlexRow(t *testing.T) {
	bounds := Rect{X: 0, Y: 0, Width: 80, Height: 40}
	items := []FlexItem{
		{Flex: 1.0},
		{Fixed: 1}, // tab bar
	}

	rects := FlexRow(bounds, items)

	if rects[1].Height != 1 {
		t.Errorf("expected fixed height 1, got %d", rects[1].Height)
	}
	if rects[0].Height != 39 {
		t.Errorf("expected flex height 39, got %d", rects[0].Height)
	}
}

func TestFlexMinSize(t *testing.T) {
	bounds := Rect{X: 0, Y: 0, Width: 20, Height: 10}
	items := []FlexItem{
		{Flex: 0.1, MinSize: 15},
		{Flex: 0.9},
	}

	rects := FlexColumn(bounds, items)

	if rects[0].Width < 15 {
		t.Errorf("expected min width 15, got %d", rects[0].Width)
	}
}
