package canvas

import "math"

// DrawRect fills a rectangle with the given color.
func (c *Canvas) DrawRect(x, y, w, h int, color Color) {
	for py := y; py < y+h; py++ {
		for px := x; px < x+w; px++ {
			c.SetPixel(px, py, color)
		}
	}
}

// DrawRectOutline draws an unfilled rectangle border (1 pixel wide).
func (c *Canvas) DrawRectOutline(x, y, w, h int, color Color) {
	// Top and bottom edges.
	for px := x; px < x+w; px++ {
		c.SetPixel(px, y, color)
		c.SetPixel(px, y+h-1, color)
	}
	// Left and right edges.
	for py := y; py < y+h; py++ {
		c.SetPixel(x, py, color)
		c.SetPixel(x+w-1, py, color)
	}
}

// DrawRoundedRect draws a filled rectangle with rounded corners.
func (c *Canvas) DrawRoundedRect(x, y, w, h, radius int, color Color) {
	// Fill main body.
	c.DrawRect(x+radius, y, w-2*radius, h, color)
	c.DrawRect(x, y+radius, w, h-2*radius, color)

	// Fill corner quadrants using circle equation.
	for dy := 0; dy < radius; dy++ {
		for dx := 0; dx < radius; dx++ {
			dist := math.Sqrt(float64(dx*dx + dy*dy))
			if dist <= float64(radius) {
				// Top-left corner.
				c.SetPixel(x+radius-1-dx, y+radius-1-dy, color)
				// Top-right corner.
				c.SetPixel(x+w-radius+dx, y+radius-1-dy, color)
				// Bottom-left corner.
				c.SetPixel(x+radius-1-dx, y+h-radius+dy, color)
				// Bottom-right corner.
				c.SetPixel(x+w-radius+dx, y+h-radius+dy, color)
			}
		}
	}
}

// DrawLine draws a line from (x1,y1) to (x2,y2) using Bresenham's algorithm.
func (c *Canvas) DrawLine(x1, y1, x2, y2 int, color Color) {
	dx := abs(x2 - x1)
	dy := -abs(y2 - y1)
	sx := 1
	if x1 > x2 {
		sx = -1
	}
	sy := 1
	if y1 > y2 {
		sy = -1
	}
	err := dx + dy

	for {
		c.SetPixel(x1, y1, color)
		if x1 == x2 && y1 == y2 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x1 += sx
		}
		if e2 <= dx {
			err += dx
			y1 += sy
		}
	}
}

// DrawArrow draws a line with an arrowhead at (x2, y2).
func (c *Canvas) DrawArrow(x1, y1, x2, y2 int, color Color) {
	c.DrawLine(x1, y1, x2, y2, color)

	// Arrowhead: two short lines at ~30 degrees from the line direction.
	angle := math.Atan2(float64(y2-y1), float64(x2-x1))
	headLen := 6.0
	for _, offset := range []float64{2.5, -2.5} {
		a := angle + math.Pi + offset*math.Pi/6
		hx := x2 + int(math.Round(headLen*math.Cos(a)))
		hy := y2 + int(math.Round(headLen*math.Sin(a)))
		c.DrawLine(x2, y2, hx, hy, color)
	}
}

// DrawText renders text as pixel glyphs on the canvas using a simple 4x5 bitmap font.
// Each character occupies a 4x5 pixel cell. Only ASCII printable characters are supported.
// For larger/cleaner text, use tcell text overlay via the screen directly.
func (c *Canvas) DrawText(x, y int, text string, color Color) {
	cx := x
	for _, ch := range text {
		// Convert lowercase to uppercase since font only has caps.
		if ch >= 'a' && ch <= 'z' {
			ch = ch - 'a' + 'A'
		}
		glyph, ok := miniFont[ch]
		if !ok {
			cx += 5 // Skip unknown characters (no question marks).
			continue
		}
		for row := 0; row < 5; row++ {
			for col := 0; col < 4; col++ {
				if row < len(glyph) && col < len(glyph[row]) && glyph[row][col] {
					c.SetPixel(cx+col, y+row, color)
				}
			}
		}
		cx += 5 // 4px char + 1px spacing
	}
}

// DrawCircle draws a filled circle centered at (cx, cy) with the given radius.
func (c *Canvas) DrawCircle(cx, cy, radius int, color Color) {
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			if dx*dx+dy*dy <= radius*radius {
				c.SetPixel(cx+dx, cy+dy, color)
			}
		}
	}
}

// DrawCircleOutline draws an unfilled circle border.
func (c *Canvas) DrawCircleOutline(cx, cy, radius int, color Color) {
	for angle := 0.0; angle < 2*math.Pi; angle += 0.01 {
		px := cx + int(math.Round(float64(radius)*math.Cos(angle)))
		py := cy + int(math.Round(float64(radius)*math.Sin(angle)))
		c.SetPixel(px, py, color)
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// miniFont is a minimal 4x5 bitmap font for a subset of ASCII characters.
// Each character is a 5-row x 4-column boolean grid.
var miniFont = map[rune][5][4]bool{
	'A': {{false, true, true, false}, {true, false, false, true}, {true, true, true, true}, {true, false, false, true}, {true, false, false, true}},
	'B': {{true, true, true, false}, {true, false, false, true}, {true, true, true, false}, {true, false, false, true}, {true, true, true, false}},
	'C': {{false, true, true, true}, {true, false, false, false}, {true, false, false, false}, {true, false, false, false}, {false, true, true, true}},
	'D': {{true, true, true, false}, {true, false, false, true}, {true, false, false, true}, {true, false, false, true}, {true, true, true, false}},
	'E': {{true, true, true, true}, {true, false, false, false}, {true, true, true, false}, {true, false, false, false}, {true, true, true, true}},
	'F': {{true, true, true, true}, {true, false, false, false}, {true, true, true, false}, {true, false, false, false}, {true, false, false, false}},
	'G': {{false, true, true, true}, {true, false, false, false}, {true, false, true, true}, {true, false, false, true}, {false, true, true, true}},
	'H': {{true, false, false, true}, {true, false, false, true}, {true, true, true, true}, {true, false, false, true}, {true, false, false, true}},
	'I': {{true, true, true, false}, {false, true, false, false}, {false, true, false, false}, {false, true, false, false}, {true, true, true, false}},
	'J': {{false, false, false, true}, {false, false, false, true}, {false, false, false, true}, {true, false, false, true}, {false, true, true, false}},
	'K': {{true, false, false, true}, {true, false, true, false}, {true, true, false, false}, {true, false, true, false}, {true, false, false, true}},
	'L': {{true, false, false, false}, {true, false, false, false}, {true, false, false, false}, {true, false, false, false}, {true, true, true, true}},
	'M': {{true, false, false, true}, {true, true, true, true}, {true, false, false, true}, {true, false, false, true}, {true, false, false, true}},
	'N': {{true, false, false, true}, {true, true, false, true}, {true, false, true, true}, {true, false, false, true}, {true, false, false, true}},
	'O': {{false, true, true, false}, {true, false, false, true}, {true, false, false, true}, {true, false, false, true}, {false, true, true, false}},
	'P': {{true, true, true, false}, {true, false, false, true}, {true, true, true, false}, {true, false, false, false}, {true, false, false, false}},
	'R': {{true, true, true, false}, {true, false, false, true}, {true, true, true, false}, {true, false, true, false}, {true, false, false, true}},
	'S': {{false, true, true, true}, {true, false, false, false}, {false, true, true, false}, {false, false, false, true}, {true, true, true, false}},
	'T': {{true, true, true, true}, {false, true, false, false}, {false, true, false, false}, {false, true, false, false}, {false, true, false, false}},
	'U': {{true, false, false, true}, {true, false, false, true}, {true, false, false, true}, {true, false, false, true}, {false, true, true, false}},
	'W': {{true, false, false, true}, {true, false, false, true}, {true, false, false, true}, {true, true, true, true}, {true, false, false, true}},
	'V': {{true, false, false, true}, {true, false, false, true}, {true, false, false, true}, {false, true, true, false}, {false, true, true, false}},
	'X': {{true, false, false, true}, {false, true, true, false}, {false, true, true, false}, {false, true, true, false}, {true, false, false, true}},
	'Y': {{true, false, false, true}, {true, false, false, true}, {false, true, true, false}, {false, true, false, false}, {false, true, false, false}},
	'Z': {{true, true, true, true}, {false, false, true, false}, {false, true, false, false}, {true, false, false, false}, {true, true, true, true}},
	'0': {{false, true, true, false}, {true, false, false, true}, {true, false, false, true}, {true, false, false, true}, {false, true, true, false}},
	'1': {{false, true, false, false}, {true, true, false, false}, {false, true, false, false}, {false, true, false, false}, {true, true, true, false}},
	'2': {{false, true, true, false}, {true, false, false, true}, {false, false, true, false}, {false, true, false, false}, {true, true, true, true}},
	'3': {{true, true, true, false}, {false, false, false, true}, {false, true, true, false}, {false, false, false, true}, {true, true, true, false}},
	':': {{false, false, false, false}, {false, true, false, false}, {false, false, false, false}, {false, true, false, false}, {false, false, false, false}},
	'.': {{false, false, false, false}, {false, false, false, false}, {false, false, false, false}, {false, false, false, false}, {false, true, false, false}},
	' ': {{false, false, false, false}, {false, false, false, false}, {false, false, false, false}, {false, false, false, false}, {false, false, false, false}},
	'-': {{false, false, false, false}, {false, false, false, false}, {true, true, true, true}, {false, false, false, false}, {false, false, false, false}},
	'?': {{false, true, true, false}, {true, false, false, true}, {false, false, true, false}, {false, false, false, false}, {false, false, true, false}},
}
