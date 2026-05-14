package canvas

import "math"

// ZoomLevel defines the supported zoom factors.
// 0.5 = overview (nodes as small icons). Zoom in to see detail.
const (
	ZoomMin     = 0.1
	ZoomDefault = 0.5
	ZoomMax     = 10.0
	ZoomStep    = 0.1
)

// Camera controls the viewport into the virtual world.
// World coordinates are in pixels; the camera projects them to screen space.
type Camera struct {
	X    float64 // world X offset (top-left of viewport)
	Y    float64 // world Y offset (top-left of viewport)
	Zoom float64 // zoom factor (1.0 = 1 world pixel per screen pixel)
}

// NewCamera creates a camera at the origin with default zoom.
func NewCamera() *Camera {
	return &Camera{Zoom: ZoomDefault}
}

// Pan moves the camera by the given delta in world units.
func (c *Camera) Pan(dx, dy float64) {
	c.X += dx
	c.Y += dy
}

// ZoomIn increases the zoom level by one step.
func (c *Camera) ZoomIn() {
	c.Zoom += ZoomStep
	if c.Zoom > ZoomMax {
		c.Zoom = ZoomMax
	}
}

// ZoomOut decreases the zoom level by one step.
func (c *Camera) ZoomOut() {
	c.Zoom -= ZoomStep
	if c.Zoom < ZoomMin {
		c.Zoom = ZoomMin
	}
}

// WorldToScreen converts a world coordinate to a screen pixel coordinate.
func (c *Camera) WorldToScreen(wx, wy float64) (int, int) {
	sx := int(math.Round((wx - c.X) * c.Zoom))
	sy := int(math.Round((wy - c.Y) * c.Zoom))
	return sx, sy
}

// ScreenToWorld converts a screen pixel coordinate to a world coordinate.
func (c *Camera) ScreenToWorld(sx, sy int) (float64, float64) {
	wx := float64(sx)/c.Zoom + c.X
	wy := float64(sy)/c.Zoom + c.Y
	return wx, wy
}

// VisibleRect returns the world-space bounding box visible through the viewport.
// screenW and screenH are in screen pixels (not terminal cells).
func (c *Camera) VisibleRect(screenW, screenH int) (minX, minY, maxX, maxY float64) {
	minX = c.X
	minY = c.Y
	maxX = c.X + float64(screenW)/c.Zoom
	maxY = c.Y + float64(screenH)/c.Zoom
	return
}

// CenterOn moves the camera so the given world point is at the center of the viewport.
func (c *Camera) CenterOn(wx, wy float64, screenW, screenH int) {
	halfW := float64(screenW) / c.Zoom / 2
	halfH := float64(screenH) / c.Zoom / 2
	c.X = wx - halfW
	c.Y = wy - halfH
}
