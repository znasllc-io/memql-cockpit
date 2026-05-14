package canvas

import "testing"

func TestNewCanvas(t *testing.T) {
	bg := Color{R: 10, G: 10, B: 10}
	c := New(100, 80, bg)
	if c.Width != 100 || c.Height != 80 {
		t.Errorf("expected 100x80, got %dx%d", c.Width, c.Height)
	}
	// All pixels should be background.
	for y := 0; y < c.Height; y++ {
		for x := 0; x < c.Width; x++ {
			got := c.GetPixel(x, y)
			if !ColorEq(got, bg) {
				t.Fatalf("pixel (%d,%d) should be background", x, y)
			}
		}
	}
}

func TestSetGetPixel(t *testing.T) {
	c := New(50, 50, Transparent)
	red := Color{R: 255}
	c.SetPixel(10, 20, red)
	got := c.GetPixel(10, 20)
	if !ColorEq(got, red) {
		t.Errorf("expected red, got %+v", got)
	}
}

func TestOutOfBounds(t *testing.T) {
	c := New(10, 10, Transparent)
	// Should not panic.
	c.SetPixel(-1, 0, Color{R: 255})
	c.SetPixel(0, -1, Color{R: 255})
	c.SetPixel(10, 0, Color{R: 255})
	c.SetPixel(0, 10, Color{R: 255})

	got := c.GetPixel(-1, 0)
	if !ColorEq(got, Transparent) {
		t.Errorf("out-of-bounds read should return background")
	}
}

func TestDrawRect(t *testing.T) {
	c := New(20, 20, Transparent)
	blue := Color{B: 255}
	c.DrawRect(5, 5, 10, 10, blue)

	// Center should be blue.
	if !ColorEq(c.GetPixel(10, 10), blue) {
		t.Error("center of rect should be blue")
	}
	// Outside should be transparent.
	if !ColorEq(c.GetPixel(0, 0), Transparent) {
		t.Error("outside rect should be transparent")
	}
}

func TestDrawLine(t *testing.T) {
	c := New(20, 20, Transparent)
	white := Color{R: 255, G: 255, B: 255}
	c.DrawLine(0, 0, 19, 19, white)

	// Diagonal should have non-transparent pixels.
	if ColorEq(c.GetPixel(10, 10), Transparent) {
		t.Error("diagonal pixel should be non-transparent")
	}
}

func TestCameraWorldToScreen(t *testing.T) {
	cam := NewCamera()
	cam.X = 10
	cam.Y = 20
	cam.Zoom = 1.0

	sx, sy := cam.WorldToScreen(15, 25)
	if sx != 5 || sy != 5 {
		t.Errorf("expected screen (5,5), got (%d,%d)", sx, sy)
	}
}

func TestCameraZoom(t *testing.T) {
	cam := NewCamera()
	cam.Zoom = 2.0

	sx, sy := cam.WorldToScreen(10, 10)
	// At 2x zoom, world (10,10) with camera at (0,0) = screen (20,20).
	if sx != 20 || sy != 20 {
		t.Errorf("expected screen (20,20) at 2x zoom, got (%d,%d)", sx, sy)
	}
}

func TestCameraZoomClamp(t *testing.T) {
	cam := NewCamera()

	// Zoom in past max.
	for i := 0; i < 20; i++ {
		cam.ZoomIn()
	}
	if cam.Zoom > ZoomMax {
		t.Errorf("zoom should be clamped to %v, got %v", ZoomMax, cam.Zoom)
	}

	// Zoom out past min.
	for i := 0; i < 20; i++ {
		cam.ZoomOut()
	}
	if cam.Zoom < ZoomMin {
		t.Errorf("zoom should be clamped to %v, got %v", ZoomMin, cam.Zoom)
	}
}

func TestDrawCircle(t *testing.T) {
	c := New(30, 30, Transparent)
	green := Color{G: 255}
	c.DrawCircle(15, 15, 5, green)

	// Center should be green.
	if !ColorEq(c.GetPixel(15, 15), green) {
		t.Error("center of circle should be green")
	}
	// Far away should be transparent.
	if !ColorEq(c.GetPixel(0, 0), Transparent) {
		t.Error("far from circle should be transparent")
	}
}

func TestResize(t *testing.T) {
	c := New(10, 10, Transparent)
	red := Color{R: 255}
	c.SetPixel(5, 5, red)

	c.Resize(20, 20)
	if c.Width != 20 || c.Height != 20 {
		t.Errorf("expected 20x20, got %dx%d", c.Width, c.Height)
	}
	// Original pixel preserved.
	if !ColorEq(c.GetPixel(5, 5), red) {
		t.Error("pixel should be preserved after resize")
	}
	// New area should be background.
	if !ColorEq(c.GetPixel(15, 15), Transparent) {
		t.Error("new area should be background")
	}
}
