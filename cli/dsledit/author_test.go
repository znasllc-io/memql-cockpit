package dsledit

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql/sdk/go/sense"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

func nilSense() *sense.Client { return nil }

func rune_(r rune) *tcell.EventKey { return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone) }
func typeStr(a *authoring, s string) {
	for _, r := range s {
		a.HandleEvent(rune_(r))
	}
}

// TestModeToggle: Ctrl+B flips the Editor tab into authoring mode and
// back, lazily building the authoring sub-view.
func TestModeToggle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v := NewView(ui.DefaultTheme())
	v.SenseClient = nilSense
	if v.modeIsAuthor() {
		t.Fatal("new view should start in browse mode")
	}
	ctrlB := tcell.NewEventKey(tcell.KeyCtrlB, 0, tcell.ModCtrl)
	v.HandleEvent(ctrlB)
	if !v.modeIsAuthor() {
		t.Fatal("Ctrl+B should switch to authoring mode")
	}
	if v.author == nil {
		t.Fatal("authoring sub-view not built on switch")
	}
	v.HandleEvent(ctrlB)
	if v.modeIsAuthor() {
		t.Fatal("Ctrl+B should switch back to browse mode")
	}
}

// TestAuthoringCreateEditSave drives the full authoring flow against an
// isolated HOME with no Sense client: create a bundle, create a file,
// type into it, save, and verify the bytes hit disk.
func TestAuthoringCreateEditSave(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAuthoring(ui.DefaultTheme(), nilSense, func(string) {}, func() {})

	// New bundle: N -> type -> Enter.
	a.HandleEvent(rune_('N'))
	if a.prompt != promptNewBundle {
		t.Fatalf("expected new-bundle prompt, got %v", a.prompt)
	}
	typeStr(a, "demo")
	a.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if a.prompt != promptNone {
		t.Fatalf("prompt should be cleared after commit; promptErr=%q", a.promptErr)
	}
	if len(a.bundles) != 1 || a.bundles[0] != "demo" {
		t.Fatalf("bundles = %v, want [demo]", a.bundles)
	}

	// Open the bundle (Enter on the selected bundle row).
	a.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if a.openBundle != "demo" || a.focus != authorFiles {
		t.Fatalf("after opening bundle: openBundle=%q focus=%v", a.openBundle, a.focus)
	}

	// New file: N -> type -> Enter. Opens it in the editor.
	a.HandleEvent(rune_('N'))
	if a.prompt != promptNewFile {
		t.Fatalf("expected new-file prompt, got %v", a.prompt)
	}
	typeStr(a, "queries")
	a.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if a.editor == nil || a.openFile != "queries.memql" || a.focus != authorEditor {
		t.Fatalf("after new file: editor=%v openFile=%q focus=%v", a.editor != nil, a.openFile, a.focus)
	}

	// Type into the editor.
	typeStr(a, "concept foo")
	if !a.editor.IsDirty() {
		t.Fatal("editor should be dirty after typing")
	}

	// Save (Ctrl+S) and verify on disk.
	a.HandleEvent(tcell.NewEventKey(tcell.KeyCtrlS, 0, tcell.ModCtrl))
	if a.editor.IsDirty() {
		t.Fatal("editor should be clean after save")
	}
	got, err := readBundleFile("demo", "queries.memql")
	if err != nil {
		t.Fatalf("readBundleFile: %v", err)
	}
	if !strings.Contains(got, "concept foo") {
		t.Fatalf("saved content = %q, want it to contain 'concept foo'", got)
	}
}

// TestAuthoringChromeRenders renders the authoring surface and asserts
// the three pane titles + a hint band land on screen.
func TestAuthoringChromeRenders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(viewWidth, viewHeight)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)

	a := newAuthoring(ui.DefaultTheme(), nilSense, func(string) {}, func() {})
	a.Draw(screen, ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight})
	sim.Show()

	rows := flattenSim(sim)
	top := rows[0]
	for _, want := range []string{"BUNDLES", "FILES", "EDITOR"} {
		if !strings.Contains(top, want) {
			t.Errorf("top row missing pane title %q; got %q", want, top)
		}
	}
	bottom := rows[viewHeight-1]
	if !strings.Contains(bottom, "N:New") {
		t.Errorf("bottom band missing the BUNDLES N:New hint; got %q", bottom)
	}
}
