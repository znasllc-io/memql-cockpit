package cluster

// Interactive topology-as-canvas, PHASE 2 (memql-cockpit#253).
//
// Phase 1 (#237) shipped the LIST-based composition builder (builder.go).
// Phase 2 adds a CLICKABLE-NODE CANVAS rendering of the same pure
// composeBuilder, plus an apply/cut-from-composition action so an operator
// can lay out a target topology visually and cut it.
//
// The canvas is rendered through the cli/canvas pixel framebuffer: each
// composeRow becomes a node box laid out on a grid, drawn into a Canvas and
// projected to the pane via a Renderer. A fixed camera (origin, zoom 1) is
// used so the screen-cell rect of every node is exact -- one world pixel per
// column, two world pixels per row -- which makes click hit-testing a plain
// containment check (no inverse-projection rounding). Labels are overlaid as
// crisp terminal text on top of the pixel boxes.
//
// The model stays in builder.go and pure; this file only renders + hit-tests
// + drives the apply flow off the View.

import (
	"fmt"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/canvas"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// composeNodeBox is one laid-out node: its screen-cell rect (for hit-testing
// + label overlay) and the composeBuilder row index it represents.
type composeNodeBox struct {
	cell   ui.Rect // screen-cell bounds of the box
	rowIdx int     // index into composeBuilder.rows
}

const (
	composeBoxCellW = 18 // node box width in screen cells
	composeBoxCellH = 4  // node box height in screen cells
	composeBoxGapX  = 2  // horizontal gap between boxes (cells)
	composeBoxGapY  = 1  // vertical gap between boxes (cells)
)

// canvas colors (independent of the tcell theme; the framebuffer has its own
// Color type). Kept muted so overlaid label text stays legible.
var (
	composeFill     = canvas.Color{R: 40, G: 44, B: 52}
	composeOutline  = canvas.Color{R: 90, G: 98, B: 112}
	composeSelected = canvas.Color{R: 86, G: 182, B: 232}
)

// layoutComposeCanvas lays out the builder rows as a grid of node boxes
// within region (screen cells) and returns the per-node boxes. Pure: no
// drawing, no View state -- unit-testable.
func layoutComposeCanvas(rowCount int, region ui.Rect) []composeNodeBox {
	if rowCount <= 0 || region.Width < composeBoxCellW || region.Height < composeBoxCellH {
		return nil
	}
	cols := (region.Width + composeBoxGapX) / (composeBoxCellW + composeBoxGapX)
	if cols < 1 {
		cols = 1
	}
	boxes := make([]composeNodeBox, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		c := i % cols
		r := i / cols
		x := region.X + c*(composeBoxCellW+composeBoxGapX)
		y := region.Y + r*(composeBoxCellH+composeBoxGapY)
		if y+composeBoxCellH > region.Y+region.Height {
			break // out of vertical room; remaining rows are clipped
		}
		boxes = append(boxes, composeNodeBox{
			cell:   ui.Rect{X: x, Y: y, Width: composeBoxCellW, Height: composeBoxCellH},
			rowIdx: i,
		})
	}
	return boxes
}

// drawBuilderCanvas renders the composition as a clickable-node canvas into
// region and records the node boxes on the View for hit-testing. Caller holds
// v.mu (RLock during Draw); composeBoxes/composeRegion are written here and
// read by mouse handling under the write lock, which is acceptable because
// tcell delivers Draw + HandleEvent on the same goroutine.
func (v *View) drawBuilderCanvas(screen *ui.Screen, region ui.Rect) {
	titleStyle := tcell.StyleDefault.Foreground(v.Theme.Accent).Background(v.Theme.BG).Bold(true)
	muted := v.Theme.SubtleStyle()
	x := region.X + 2
	w := region.Width - 4
	if w <= 0 {
		return
	}
	screen.DrawText(x, region.Y+1, w, "TOPOLOGY BUILDER (canvas)", titleStyle)
	screen.DrawText(x, region.Y+2, w, "Click a node to select; +/- replicas, A add, D del.", muted)

	gridRegion := ui.Rect{X: region.X + 2, Y: region.Y + 4, Width: region.Width - 4, Height: region.Height - 5}
	boxes := layoutComposeCanvas(len(v.builder.rows), gridRegion)
	v.composeBoxes = boxes
	v.composeRegion = gridRegion

	if len(boxes) == 0 {
		if v.builder.empty() {
			screen.DrawText(x, region.Y+4, w, "No node types yet. Press A to add one.", muted)
		}
		return
	}

	// Pixel framebuffer for the grid region: 1 world px per column, 2 per row.
	cv := canvas.New(gridRegion.Width, gridRegion.Height*2, canvas.Transparent)
	for _, b := range boxes {
		px := b.cell.X - gridRegion.X
		py := (b.cell.Y - gridRegion.Y) * 2
		pw := b.cell.Width
		ph := b.cell.Height * 2
		cv.DrawRoundedRect(px, py, pw, ph, 2, composeFill)
		outline := composeOutline
		if b.rowIdx == v.builder.cursor {
			outline = composeSelected
		}
		cv.DrawRectOutline(px, py, pw, ph, outline)
	}
	r := canvas.NewRenderer(&canvas.Camera{X: 0, Y: 0, Zoom: 1})
	r.DrawCanvas(screen, gridRegion, cv)

	// Overlay crisp labels on top of the pixel boxes.
	for _, b := range boxes {
		row := v.builder.rows[b.rowIdx]
		label := nodeTypeShort(row.NodeType)
		sub := fmt.Sprintf("x%d", row.Replicas)
		if row.Version != "" {
			sub += " " + row.Version
		}
		style := v.Theme.BaseStyle()
		if b.rowIdx == v.builder.cursor {
			style = style.Foreground(v.Theme.Accent).Bold(true)
		}
		lw := b.cell.Width - 2
		screen.DrawText(b.cell.X+1, b.cell.Y+1, lw, clipText(label, lw), style)
		screen.DrawText(b.cell.X+1, b.cell.Y+2, lw, clipText(sub, lw), v.Theme.SubtleStyle())
	}
}

// composeCanvasHit returns the builder-row index of the node box containing
// screen cell (sx, sy), or -1 when the click misses every box. Caller holds
// v.mu.
func (v *View) composeCanvasHit(sx, sy int) int {
	for _, b := range v.composeBoxes {
		if sx >= b.cell.X && sx < b.cell.X+b.cell.Width &&
			sy >= b.cell.Y && sy < b.cell.Y+b.cell.Height {
			return b.rowIdx
		}
	}
	return -1
}
