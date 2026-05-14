package automations

import (
	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/canvas"
	"github.com/visionarys-io/memql-cockpit/cli/editor"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// FocusPane identifies which pane has keyboard focus.
type FocusPane int

const (
	FocusList    FocusPane = 0
	FocusDiagram FocusPane = 1
	FocusEditor  FocusPane = 2
)

// View renders the Automations tab with three panes:
// left (automation list), center (flow diagram canvas), right (step code editor).
type View struct {
	Theme       ui.Theme
	Automations []AutomationInfo
	Selected    int // index in Automations list
	ListScrollY int
	Focus       FocusPane

	// Diagram state.
	Canvas   *canvas.Canvas
	Camera   *canvas.Camera
	Renderer *canvas.Renderer
	Nodes    []FlowNode
	Edges    [][2]int

	// Editor for viewing step source code.
	Editor        *editor.Editor
	SelectedStep  int // index in current automation's steps (-1 = none)

	// GatedMessage, when non-empty, replaces the three-pane layout
	// with a centered "not available" message. Set by the app layer
	// when the selected cluster has no live connection.
	GatedMessage string

	// Callbacks set by the app layer.
	OnLoadAutomations func() []AutomationInfo
	OnRequestTokens   func(source string) []editor.SenseToken
	OnRequestDiags    func(source string) []editor.SenseDiagnostic
}

// NewView creates an empty automations view.
func NewView(theme ui.Theme) *View {
	cam := canvas.NewCamera()
	return &View{
		Theme:        theme,
		Canvas:       canvas.New(350, 400, canvas.Color{R: 24, G: 24, B: 28}),
		Camera:       cam,
		Renderer:     canvas.NewRenderer(cam),
		Editor:       editor.NewEditor(nil, theme),
		SelectedStep: -1,
		Focus:        FocusList,
	}
}

// SetAutomations updates the automation list.
func (v *View) SetAutomations(autos []AutomationInfo) {
	v.Automations = autos
	if len(autos) > 0 {
		v.selectAutomation(0)
	}
}

// Draw renders the three-pane automations tab.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	if v.GatedMessage != "" {
		screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
		title := " AUTOMATIONS "
		screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))
		midY := bounds.Y + bounds.Height/2
		lineX := bounds.X + (bounds.Width-len(v.GatedMessage))/2
		if lineX < bounds.X+1 {
			lineX = bounds.X + 1
		}
		screen.DrawText(lineX, midY, bounds.Width-1, v.GatedMessage, v.Theme.SubtleStyle())
		return
	}
	// Three-column layout: list 20%, diagram 40%, editor 40%.
	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.20, MinSize: 18},
		{Flex: 0.40, MinSize: 20},
		{Flex: 0.40, MinSize: 20},
	})

	listBounds := panes[0]
	diagBounds := panes[1]
	editorBounds := panes[2]

	// Dividers.
	div1X := listBounds.X + listBounds.Width
	div2X := diagBounds.X + diagBounds.Width
	divStyle := v.Theme.SubtleStyle()
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		screen.SetCell(div1X-1, y, '│', divStyle)
		screen.SetCell(div2X-1, y, '│', divStyle)
	}
	listBounds.Width--
	diagBounds.Width--

	v.drawList(screen, listBounds)
	v.drawDiagram(screen, diagBounds)
	v.drawEditor(screen, editorBounds)
}

// drawList renders the automation list pane.
func (v *View) drawList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	// Title: gray bold when unfocused, accent bold when focused. Matches
	// the Clusters tab convention -- no underline, so focus is signaled
	// by color change only (consistent across all tabs).
	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusList {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " AUTOMATIONS ", titleStyle)

	y := bounds.Y + 1
	for i, auto := range v.Automations {
		if y >= bounds.Y+bounds.Height {
			break
		}

		style := v.Theme.BaseStyle()
		if i == v.Selected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)

		// Enabled indicator.
		indicator := '●'
		indStyle := style.Foreground(v.Theme.Success)
		if !auto.Enabled {
			indStyle = style.Foreground(v.Theme.Subtle)
		}
		screen.SetCell(bounds.X+1, y, indicator, indStyle)

		// Name.
		screen.DrawText(bounds.X+3, y, bounds.Width-4, auto.Name, style)
		y++
	}
}

// drawDiagram renders the flow diagram canvas pane.
func (v *View) drawDiagram(screen *ui.Screen, bounds ui.Rect) {
	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusDiagram {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " FLOW ", titleStyle)

	canvasBounds := ui.Rect{X: bounds.X, Y: bounds.Y + 1, Width: bounds.Width, Height: bounds.Height - 1}
	v.Renderer.Draw(screen, canvasBounds, v.Canvas)
}

// drawEditor renders the step code editor pane.
func (v *View) drawEditor(screen *ui.Screen, bounds ui.Rect) {
	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusEditor {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " SOURCE ", titleStyle)

	edBounds := ui.Rect{X: bounds.X, Y: bounds.Y + 1, Width: bounds.Width, Height: bounds.Height - 1}
	v.Editor.Draw(screen, edBounds)
}

// HandleEvent processes keyboard input for the automations tab.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// Tab key cycles focus: list → diagram → editor → list.
	if keyEv.Key() == tcell.KeyTab {
		v.Focus = (v.Focus + 1) % 3
		return true
	}

	switch v.Focus {
	case FocusList:
		return v.handleListKey(keyEv)
	case FocusDiagram:
		return v.handleDiagramKey(keyEv)
	case FocusEditor:
		return v.Editor.HandleEvent(ev)
	}

	return false
}

func (v *View) handleListKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		if v.Selected > 0 {
			v.Selected--
			v.selectAutomation(v.Selected)
		}
		return true
	case tcell.KeyDown:
		if v.Selected < len(v.Automations)-1 {
			v.Selected++
			v.selectAutomation(v.Selected)
		}
		return true
	case tcell.KeyEnter:
		v.selectAutomation(v.Selected)
		v.Focus = FocusDiagram
		return true
	}
	return false
}

func (v *View) handleDiagramKey(ev *tcell.EventKey) bool {
	panStep := 10.0 / v.Camera.Zoom

	switch ev.Key() {
	case tcell.KeyUp:
		v.Camera.Pan(0, -panStep)
		return true
	case tcell.KeyDown:
		v.Camera.Pan(0, panStep)
		return true
	case tcell.KeyLeft:
		v.Camera.Pan(-panStep, 0)
		return true
	case tcell.KeyRight:
		v.Camera.Pan(panStep, 0)
		return true
	case tcell.KeyRune:
		switch ev.Rune() {
		case '+', '=':
			v.Camera.ZoomIn()
			return true
		case '-':
			v.Camera.ZoomOut()
			return true
		case '0':
			v.Camera.X, v.Camera.Y, v.Camera.Zoom = 0, 0, canvas.ZoomDefault
			return true
		}
	}
	return false
}

func (v *View) selectAutomation(idx int) {
	if idx < 0 || idx >= len(v.Automations) {
		return
	}
	auto := v.Automations[idx]

	// Layout the flow diagram.
	v.Nodes, v.Edges = LayoutFlow(auto.Steps)

	// Render to the canvas.
	v.Canvas.Clear()
	RenderFlow(v.Canvas, v.Nodes, v.Edges)

	// Load the source into the editor.
	if auto.Source != "" {
		buf := editor.NewBuffer(auto.Source, auto.Name+".memql", true)
		v.Editor = editor.NewEditor(buf, v.Theme)

		// Request Sense tokens if callback is set.
		if v.OnRequestTokens != nil {
			tokens := v.OnRequestTokens(auto.Source)
			v.Editor.SetTokens(tokens)
		}
		if v.OnRequestDiags != nil {
			diags := v.OnRequestDiags(auto.Source)
			v.Editor.SetDiagnostics(diags)
		}
	}

	// Reset camera for the new diagram.
	v.Camera.X = 0
	v.Camera.Y = 0
	v.Camera.Zoom = canvas.ZoomDefault
}
