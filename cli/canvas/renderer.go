package canvas

import (
	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

const (
	upperHalf = '▀'
	lowerHalf = '▄'
	fullBlock = '█'
)

// Renderer draws world-space objects onto a tcell Screen using half-block
// Unicode characters. The camera determines the visible viewport.
//
// Instead of rendering from a fixed pixel buffer, the renderer draws
// directly to the screen — enabling infinite canvas.
type Renderer struct {
	Camera *Camera
}

// NewRenderer creates a renderer with the given camera.
func NewRenderer(camera *Camera) *Renderer {
	return &Renderer{Camera: camera}
}

// Draw renders a fixed-size pixel canvas onto the screen.
// Alias for DrawCanvas for backward compatibility.
func (r *Renderer) Draw(screen *ui.Screen, bounds ui.Rect, canvas *Canvas) {
	r.DrawCanvas(screen, bounds, canvas)
}

// DrawCanvas renders a fixed-size pixel canvas onto the screen.
// Used for pre-rendered content (node interiors, complex shapes).
func (r *Renderer) DrawCanvas(screen *ui.Screen, bounds ui.Rect, canvas *Canvas) {
	bg := canvas.Background()
	zoom := r.Camera.Zoom
	camX := r.Camera.X
	camY := r.Camera.Y

	for row := 0; row < bounds.Height; row++ {
		for col := 0; col < bounds.Width; col++ {
			screenX := bounds.X + col
			screenY := bounds.Y + row

			worldX := int(camX + float64(col)/zoom)
			topWorldY := int(camY + float64(row*2)/zoom)
			botWorldY := int(camY + float64(row*2+1)/zoom)

			topColor := canvas.GetPixel(worldX, topWorldY)
			botColor := canvas.GetPixel(worldX, botWorldY)

			topIsBG := ColorEq(topColor, bg)
			botIsBG := ColorEq(botColor, bg)

			ch, fg, bgc := halfBlockChar(topColor, botColor, bg, topIsBG, botIsBG)
			style := tcell.StyleDefault.Foreground(fg).Background(bgc)
			screen.SetCell(screenX, screenY, ch, style)
		}
	}
}

// DrawGrid renders an infinite grid pattern based on the camera viewport.
// gridSpacing is in world pixels. The grid extends infinitely in all directions.
func (r *Renderer) DrawGrid(screen *ui.Screen, bounds ui.Rect, gridSpacing int, gridColor, bgColor Color) {
	zoom := r.Camera.Zoom
	camX := r.Camera.X
	camY := r.Camera.Y

	gridTcell := colorToTcell(gridColor)
	bgTcell := colorToTcell(bgColor)

	for row := 0; row < bounds.Height; row++ {
		for col := 0; col < bounds.Width; col++ {
			screenX := bounds.X + col
			screenY := bounds.Y + row

			worldX := int(camX + float64(col)/zoom)
			topWorldY := int(camY + float64(row*2)/zoom)
			botWorldY := int(camY + float64(row*2+1)/zoom)

			topIsGrid := worldX%gridSpacing == 0 || topWorldY%gridSpacing == 0
			botIsGrid := worldX%gridSpacing == 0 || botWorldY%gridSpacing == 0

			if !topIsGrid && !botIsGrid {
				continue // Both pixels are background — skip (already cleared).
			}

			var ch rune
			var fg, bg tcell.Color

			switch {
			case topIsGrid && botIsGrid:
				ch = fullBlock
				fg = gridTcell
				bg = bgTcell
			case topIsGrid:
				ch = upperHalf
				fg = gridTcell
				bg = bgTcell
			case botIsGrid:
				ch = lowerHalf
				fg = gridTcell
				bg = bgTcell
			}

			style := tcell.StyleDefault.Foreground(fg).Background(bg)
			screen.SetCell(screenX, screenY, ch, style)
		}
	}
}

// DrawFilledRect draws a filled rectangle in world coordinates onto the screen.
func (r *Renderer) DrawFilledRect(screen *ui.Screen, bounds ui.Rect, wx, wy, ww, wh int, color Color) {
	r.drawWorldRect(screen, bounds, wx, wy, ww, wh, color, true)
}

// DrawOutlineRect draws a 1-pixel outline rectangle in world coordinates.
func (r *Renderer) DrawOutlineRect(screen *ui.Screen, bounds ui.Rect, wx, wy, ww, wh int, color Color) {
	r.drawWorldRect(screen, bounds, wx, wy, ww, wh, color, false)
}

func (r *Renderer) drawWorldRect(screen *ui.Screen, bounds ui.Rect, wx, wy, ww, wh int, color Color, filled bool) {
	zoom := r.Camera.Zoom
	camX := r.Camera.X
	camY := r.Camera.Y
	tcellColor := colorToTcell(color)

	for row := 0; row < bounds.Height; row++ {
		for col := 0; col < bounds.Width; col++ {
			worldX := int(camX + float64(col)/zoom)
			topWorldY := int(camY + float64(row*2)/zoom)
			botWorldY := int(camY + float64(row*2+1)/zoom)

			topIn := isInRect(worldX, topWorldY, wx, wy, ww, wh, filled)
			botIn := isInRect(worldX, botWorldY, wx, wy, ww, wh, filled)

			if !topIn && !botIn {
				continue
			}

			screenX := bounds.X + col
			screenY := bounds.Y + row

			// Read existing cell to blend with.
			existCh, _, existStyle, _ := screen.Inner().GetContent(screenX, screenY)
			existFG, existBG, _ := existStyle.Decompose()

			var ch rune
			var fg, bg tcell.Color

			switch {
			case topIn && botIn:
				ch = fullBlock
				fg = tcellColor
				bg = existBG
			case topIn:
				// Top pixel is ours. Need to check if existing cell has a bottom pixel.
				if existCh == lowerHalf || existCh == fullBlock {
					ch = fullBlock
					fg = tcellColor
					bg = existFG // Existing bottom pixel becomes BG
				} else {
					ch = upperHalf
					fg = tcellColor
					bg = existBG
				}
			case botIn:
				if existCh == upperHalf || existCh == fullBlock {
					ch = upperHalf
					fg = existFG // Existing top pixel stays as FG
					bg = tcellColor
				} else {
					ch = lowerHalf
					fg = tcellColor
					bg = existBG
				}
			}

			style := tcell.StyleDefault.Foreground(fg).Background(bg)
			screen.SetCell(screenX, screenY, ch, style)
		}
	}
}

func isInRect(px, py, rx, ry, rw, rh int, filled bool) bool {
	if px < rx || px >= rx+rw || py < ry || py >= ry+rh {
		return false
	}
	if filled {
		return true
	}
	// Outline only: edges.
	return px == rx || px == rx+rw-1 || py == ry || py == ry+rh-1
}

func halfBlockChar(topColor, botColor, bg Color, topIsBG, botIsBG bool) (rune, tcell.Color, tcell.Color) {
	switch {
	case topIsBG && botIsBG:
		c := colorToTcell(bg)
		return ' ', c, c
	case !topIsBG && !botIsBG && ColorEq(topColor, botColor):
		return fullBlock, colorToTcell(topColor), colorToTcell(bg)
	case !topIsBG && !botIsBG:
		return upperHalf, colorToTcell(topColor), colorToTcell(botColor)
	case !topIsBG && botIsBG:
		return upperHalf, colorToTcell(topColor), colorToTcell(bg)
	default: // topIsBG && !botIsBG
		return lowerHalf, colorToTcell(botColor), colorToTcell(bg)
	}
}

func colorToTcell(c Color) tcell.Color {
	return tcell.NewRGBColor(int32(c.R), int32(c.G), int32(c.B))
}
