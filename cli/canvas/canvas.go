// Package canvas provides a virtual pixel framebuffer rendered with Unicode
// half-block characters. Each terminal character cell represents 2 vertical
// pixels using ▀ ▄ █ with 24-bit foreground/background colors, giving
// approximately square pixels in most terminal emulators.
package canvas

// Color represents a 24-bit RGB color.
type Color struct {
	R, G, B uint8
}

// Transparent is the zero-value color used as the canvas background.
var Transparent = Color{}

// ColorEq returns true if two colors are identical.
func ColorEq(a, b Color) bool {
	return a.R == b.R && a.G == b.G && a.B == b.B
}

// Canvas is a virtual pixel framebuffer. Pixels are addressed by (x, y)
// where (0, 0) is the top-left corner. The canvas grows on demand.
type Canvas struct {
	Width  int
	Height int
	pixels []Color // row-major: pixels[y*Width + x]
	bg     Color   // background/clear color
}

// New creates a canvas with the given dimensions and background color.
func New(width, height int, bg Color) *Canvas {
	c := &Canvas{
		Width:  width,
		Height: height,
		bg:     bg,
		pixels: make([]Color, width*height),
	}
	c.Clear()
	return c
}

// Clear fills the entire canvas with the background color.
func (c *Canvas) Clear() {
	for i := range c.pixels {
		c.pixels[i] = c.bg
	}
}

// SetPixel sets the color at (x, y). Out-of-bounds writes are silently ignored.
func (c *Canvas) SetPixel(x, y int, color Color) {
	if x < 0 || x >= c.Width || y < 0 || y >= c.Height {
		return
	}
	c.pixels[y*c.Width+x] = color
}

// GetPixel returns the color at (x, y). Out-of-bounds reads return the background.
func (c *Canvas) GetPixel(x, y int) Color {
	if x < 0 || x >= c.Width || y < 0 || y >= c.Height {
		return c.bg
	}
	return c.pixels[y*c.Width+x]
}

// Background returns the canvas background color.
func (c *Canvas) Background() Color {
	return c.bg
}

// Resize changes the canvas dimensions. Existing pixels within the new bounds
// are preserved; new pixels are filled with the background color.
func (c *Canvas) Resize(newWidth, newHeight int) {
	newPixels := make([]Color, newWidth*newHeight)
	for i := range newPixels {
		newPixels[i] = c.bg
	}

	// Copy existing pixels that fit.
	copyW := c.Width
	if newWidth < copyW {
		copyW = newWidth
	}
	copyH := c.Height
	if newHeight < copyH {
		copyH = newHeight
	}
	for y := 0; y < copyH; y++ {
		for x := 0; x < copyW; x++ {
			newPixels[y*newWidth+x] = c.pixels[y*c.Width+x]
		}
	}

	c.Width = newWidth
	c.Height = newHeight
	c.pixels = newPixels
}
