// Package automations provides the Automations tab for memQL Cockpit.
// It renders automation flow diagrams on the pixel canvas and links
// steps to the code editor for viewing MemQL source.
package automations

import "github.com/visionarys-io/memql-cockpit/cli/canvas"

// StepType identifies the visual shape for a step in the flow diagram.
type StepType string

const (
	StepTrigger  StepType = "trigger"
	StepFunction StepType = "function"
	StepBranch   StepType = "branch"
	StepParallel StepType = "parallel"
	StepEnd      StepType = "end"
)

// AutomationInfo describes an automation for the list pane.
type AutomationInfo struct {
	Name        string
	Description string
	Domain      string // e.g., "cognition", "cluster", "data"
	Trigger     string // e.g., "graph.node.created.*.v1:cognition:participant"
	Enabled     bool
	Steps       []StepInfo
	Source      string // full .memql source for the editor
}

// StepInfo describes a single step in an automation flow.
type StepInfo struct {
	ID          string
	Name        string
	Type        StepType
	Description string
	SourceLine  int    // line number in automation source (for editor linking)
	Children    []int  // indices of child steps (for branches/parallel)
}

// FlowNode is a positioned step in the diagram layout.
type FlowNode struct {
	Step  StepInfo
	Index int
	X, Y  int // world pixel coordinates (top-left)
}

const (
	flowNodeW  = 50
	flowNodeH  = 18
	flowHSpace = 20
	flowVSpace = 30
)

// LayoutFlow computes positions for automation steps in a top-to-bottom flow.
// Returns positioned nodes and edges (pairs of node indices).
func LayoutFlow(steps []StepInfo) ([]FlowNode, [][2]int) {
	if len(steps) == 0 {
		return nil, nil
	}

	nodes := make([]FlowNode, len(steps))
	var edges [][2]int

	// Simple linear layout with branches expanding horizontally.
	centerX := 150
	y := 20

	for i, step := range steps {
		x := centerX - flowNodeW/2

		// Branch children fan out horizontally.
		if step.Type == StepBranch && len(step.Children) > 0 {
			totalW := len(step.Children)*flowNodeW + (len(step.Children)-1)*flowHSpace
			x = centerX - totalW/2
		}

		nodes[i] = FlowNode{
			Step:  step,
			Index: i,
			X:     x,
			Y:     y,
		}

		// Edge from previous step.
		if i > 0 {
			edges = append(edges, [2]int{i - 1, i})
		}

		// Branch children get edges from branch node.
		for _, childIdx := range step.Children {
			if childIdx >= 0 && childIdx < len(steps) {
				edges = append(edges, [2]int{i, childIdx})
			}
		}

		y += flowNodeH + flowVSpace
	}

	return nodes, edges
}

// RenderFlow draws the automation flow onto the canvas.
func RenderFlow(c *canvas.Canvas, nodes []FlowNode, edges [][2]int) {
	// Draw edges first (behind nodes).
	edgeColor := canvas.Color{R: 80, G: 85, B: 100}
	for _, edge := range edges {
		if edge[0] >= len(nodes) || edge[1] >= len(nodes) {
			continue
		}
		n1 := nodes[edge[0]]
		n2 := nodes[edge[1]]
		// Arrow from bottom-center of source to top-center of target.
		fromX := n1.X + flowNodeW/2
		fromY := n1.Y + flowNodeH
		toX := n2.X + flowNodeW/2
		toY := n2.Y
		c.DrawArrow(fromX, fromY, toX, toY, edgeColor)
	}

	// Draw nodes.
	for _, node := range nodes {
		drawFlowNode(c, node)
	}
}

func drawFlowNode(c *canvas.Canvas, node FlowNode) {
	x, y := node.X, node.Y
	step := node.Step

	bodyColor, borderColor, labelColor := stepColors(step.Type)

	switch step.Type {
	case StepTrigger:
		// Rounded rectangle for trigger.
		c.DrawRoundedRect(x, y, flowNodeW, flowNodeH, 4, bodyColor)
		c.DrawRectOutline(x-1, y-1, flowNodeW+2, flowNodeH+2, borderColor)

	case StepBranch:
		// Diamond shape approximated with filled triangles.
		cx := x + flowNodeW/2
		cy := y + flowNodeH/2
		hw := flowNodeW / 2
		hh := flowNodeH / 2
		for dy := -hh; dy <= hh; dy++ {
			span := hw * (hh - abs(dy)) / hh
			for dx := -span; dx <= span; dx++ {
				c.SetPixel(cx+dx, cy+dy, bodyColor)
			}
		}
		// Diamond border.
		c.DrawLine(cx, y, x+flowNodeW, cy, borderColor)
		c.DrawLine(x+flowNodeW, cy, cx, y+flowNodeH, borderColor)
		c.DrawLine(cx, y+flowNodeH, x, cy, borderColor)
		c.DrawLine(x, cy, cx, y, borderColor)

	case StepParallel:
		// Double-bordered rectangle for parallel.
		c.DrawRect(x, y, flowNodeW, flowNodeH, bodyColor)
		c.DrawRectOutline(x, y, flowNodeW, flowNodeH, borderColor)
		c.DrawRectOutline(x+2, y+2, flowNodeW-4, flowNodeH-4, borderColor)

	default:
		// Standard rectangle for function/end steps.
		c.DrawRect(x, y, flowNodeW, flowNodeH, bodyColor)
		c.DrawRectOutline(x, y, flowNodeW, flowNodeH, borderColor)
	}

	// Step name (pixel font).
	name := step.Name
	if len(name) > 8 {
		name = name[:8]
	}
	c.DrawText(x+4, y+3, name, labelColor)

	// Type indicator below name.
	typeLabel := string(step.Type)
	if len(typeLabel) > 8 {
		typeLabel = typeLabel[:8]
	}
	dimColor := canvas.Color{R: labelColor.R / 2, G: labelColor.G / 2, B: labelColor.B / 2}
	c.DrawText(x+4, y+10, typeLabel, dimColor)
}

func stepColors(t StepType) (body, border, label canvas.Color) {
	switch t {
	case StepTrigger:
		return canvas.Color{R: 30, G: 50, B: 80},
			canvas.Color{R: 86, G: 156, B: 214},
			canvas.Color{R: 200, G: 220, B: 255}
	case StepFunction:
		return canvas.Color{R: 45, G: 45, B: 50},
			canvas.Color{R: 130, G: 135, B: 150},
			canvas.Color{R: 212, G: 212, B: 216}
	case StepBranch:
		return canvas.Color{R: 60, G: 50, B: 30},
			canvas.Color{R: 229, G: 192, B: 123},
			canvas.Color{R: 240, G: 220, B: 160}
	case StepParallel:
		return canvas.Color{R: 40, G: 55, B: 45},
			canvas.Color{R: 152, G: 195, B: 121},
			canvas.Color{R: 200, G: 230, B: 200}
	case StepEnd:
		return canvas.Color{R: 50, G: 35, B: 35},
			canvas.Color{R: 180, G: 100, B: 100},
			canvas.Color{R: 220, G: 180, B: 180}
	default:
		return canvas.Color{R: 45, G: 45, B: 50},
			canvas.Color{R: 100, G: 100, B: 110},
			canvas.Color{R: 200, G: 200, B: 210}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
