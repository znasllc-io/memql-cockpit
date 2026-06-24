package cluster

// Tests for the topology-as-canvas PHASE 2 (memql-cockpit#253): the pure
// canvas layout + click hit-test, the V list<->canvas toggle, a click
// selecting a node, and the apply/cut-from-composition flow (confirm ->
// async fire -> result) driven through a fake ApplyComposition callback.

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

func keyV() *tcell.EventKey { return keyRune('V') }

func mouseClick(x, y int) *tcell.EventMouse {
	return tcell.NewEventMouse(x, y, tcell.Button1, tcell.ModNone)
}

// --- pure layout ---

func TestLayoutComposeCanvas(t *testing.T) {
	region := ui.Rect{X: 0, Y: 0, Width: 100, Height: 20}
	boxes := layoutComposeCanvas(3, region)
	if len(boxes) != 3 {
		t.Fatalf("expected 3 boxes, got %d", len(boxes))
	}
	// First box anchored at region origin; second offset by box width + gap.
	if boxes[0].cell.X != region.X || boxes[0].cell.Y != region.Y {
		t.Errorf("box0 should anchor at region origin, got %+v", boxes[0].cell)
	}
	if boxes[1].cell.X != region.X+composeBoxCellW+composeBoxGapX {
		t.Errorf("box1 X should be one column over, got %d", boxes[1].cell.X)
	}
	for i, b := range boxes {
		if b.rowIdx != i {
			t.Errorf("box %d should map to row %d, got %d", i, i, b.rowIdx)
		}
	}
}

func TestLayoutComposeCanvas_ClipsAndDegenerate(t *testing.T) {
	if got := layoutComposeCanvas(0, ui.Rect{Width: 100, Height: 20}); got != nil {
		t.Errorf("zero rows should yield no boxes, got %d", len(got))
	}
	if got := layoutComposeCanvas(3, ui.Rect{Width: 4, Height: 2}); got != nil {
		t.Errorf("a region smaller than one box should yield no boxes, got %d", len(got))
	}
	// A single short row of vertical room clips the second grid row.
	boxes := layoutComposeCanvas(20, ui.Rect{X: 0, Y: 0, Width: 100, Height: composeBoxCellH})
	if len(boxes) == 0 {
		t.Fatal("expected at least the first grid row to fit")
	}
	for _, b := range boxes {
		if b.cell.Y+composeBoxCellH > composeBoxCellH {
			t.Fatalf("box overflows the single-row region: %+v", b.cell)
		}
	}
}

// --- hit-test ---

func TestComposeCanvasHit(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.composeBoxes = []composeNodeBox{
		{cell: ui.Rect{X: 0, Y: 0, Width: 10, Height: 4}, rowIdx: 0},
		{cell: ui.Rect{X: 20, Y: 0, Width: 10, Height: 4}, rowIdx: 1},
	}
	if got := v.composeCanvasHit(5, 2); got != 0 {
		t.Errorf("click inside box0 should hit row 0, got %d", got)
	}
	if got := v.composeCanvasHit(25, 1); got != 1 {
		t.Errorf("click inside box1 should hit row 1, got %d", got)
	}
	if got := v.composeCanvasHit(15, 2); got != -1 {
		t.Errorf("click in the gap should miss (-1), got %d", got)
	}
	if got := v.composeCanvasHit(5, 9); got != -1 {
		t.Errorf("click below the boxes should miss (-1), got %d", got)
	}
}

// --- view integration ---

func TestBuilder_CanvasToggleRendersAndClickSelects(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetNodeTypes([]NodeTypeInfo{{Name: "bff"}, {Name: "voice"}, {Name: "cognition"}})

	v.HandleEvent(keyN()) // open builder (list)
	if v.builder == nil || len(v.builder.rows) != 3 {
		t.Fatalf("builder should seed 3 types; got %+v", v.builder)
	}
	v.HandleEvent(keyV()) // toggle to canvas
	if !v.builderCanvas {
		t.Fatal("V should toggle the canvas rendering on")
	}

	joined := strings.Join(renderTopology(t, v, 120, 30), "\n")
	if !strings.Contains(joined, "TOPOLOGY BUILDER (canvas)") {
		t.Errorf("canvas mode should render its header; got:\n%s", joined)
	}
	if len(v.composeBoxes) != 3 {
		t.Fatalf("render should record 3 node boxes for hit-testing, got %d", len(v.composeBoxes))
	}

	// Click the center of the box mapped to row 2 -> cursor moves to it.
	box := v.composeBoxes[2]
	cx := box.cell.X + box.cell.Width/2
	cy := box.cell.Y + box.cell.Height/2
	if handled := v.HandleEvent(mouseClick(cx, cy)); !handled {
		t.Fatal("a click on a node box should be consumed")
	}
	if v.builder.cursor != 2 {
		t.Errorf("click should select row 2, got cursor %d", v.builder.cursor)
	}

	// A click in empty space is consumed but selects nothing new.
	v.HandleEvent(mouseClick(box.cell.X+box.cell.Width+1, box.cell.Y))
	if v.builder.cursor != 2 {
		t.Errorf("a miss click should not move the cursor, got %d", v.builder.cursor)
	}
}

// --- apply / cut-from-composition ---

type fakeComposer struct {
	calls  int
	gotEnv string
	gotN   int
	line   string
	ok     bool
}

func (f *fakeComposer) apply(env string, specs []NodeSpecInfo) (string, bool) {
	f.calls++
	f.gotEnv = env
	f.gotN = len(specs)
	return f.line, f.ok
}

func TestComposeApply_ConfirmFires(t *testing.T) {
	fc := &fakeComposer{line: "cut staging deployment dep-x; 2/2 tiers applied", ok: true}
	v := NewView(ui.DefaultTheme())
	v.SetClusterEnvironment("staging")
	v.ApplyComposition = fc.apply
	redrawCh := make(chan struct{}, 64)
	v.OnRedraw = func() {
		select {
		case redrawCh <- struct{}{}:
		default:
		}
	}
	v.SetNodeTypes([]NodeTypeInfo{{Name: "bff"}, {Name: "voice"}})

	v.HandleEvent(keyN())     // open builder
	v.HandleEvent(keyEnter()) // open apply confirm
	if v.compApply == nil || v.compApply.stage != caConfirm {
		t.Fatalf("Enter should open the apply confirm; got %+v", v.compApply)
	}
	joined := strings.Join(renderTopology(t, v, 120, 30), "\n")
	if !strings.Contains(joined, "CUT FROM COMPOSITION") {
		t.Errorf("apply confirm should render its header; got:\n%s", joined)
	}

	v.HandleEvent(keyRune('Y')) // fire
	// Wait for the async apply to reach a result.
	deadline := time.After(2 * time.Second)
	for {
		v.mu.RLock()
		done := v.compApply != nil && v.compApply.stage == caResult
		v.mu.RUnlock()
		if done {
			break
		}
		select {
		case <-redrawCh:
		case <-deadline:
			t.Fatal("apply did not reach a result")
		}
	}
	if fc.calls != 1 {
		t.Errorf("apply callback should fire exactly once, got %d", fc.calls)
	}
	if fc.gotEnv != "staging" {
		t.Errorf("apply should pass the cluster env, got %q", fc.gotEnv)
	}
	if fc.gotN != 2 {
		t.Errorf("apply should pass 2 tier specs, got %d", fc.gotN)
	}
	if v.compApply.result != fc.line || !v.compApply.ok {
		t.Errorf("apply result not surfaced; got %+v", v.compApply)
	}

	// Any key dismisses the result back to the builder.
	v.HandleEvent(keyRune('x'))
	if v.compApply != nil {
		t.Error("a key on the result should dismiss the apply overlay")
	}
}

func TestComposeApply_Cancel(t *testing.T) {
	fc := &fakeComposer{ok: true}
	v := NewView(ui.DefaultTheme())
	v.ApplyComposition = fc.apply
	v.SetNodeTypes([]NodeTypeInfo{{Name: "bff"}})

	v.HandleEvent(keyN())
	v.HandleEvent(keyEnter())
	v.HandleEvent(keyRune('n')) // cancel
	if v.compApply != nil {
		t.Error("N should cancel the apply confirm")
	}
	if fc.calls != 0 {
		t.Errorf("cancel must not fire the apply, got %d calls", fc.calls)
	}
}

func TestComposeApply_NoCallbackIsNoop(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.SetNodeTypes([]NodeTypeInfo{{Name: "bff"}})
	v.HandleEvent(keyN())
	v.HandleEvent(keyEnter()) // no ApplyComposition wired
	if v.compApply != nil {
		t.Error("Enter with no apply callback should be a no-op")
	}
}
