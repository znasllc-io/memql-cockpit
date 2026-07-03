package dsledit

// E3 (memql#2374) tests: the browse-mode "fork the currently-viewed pack
// file into a bundle" affordance. Two layers, mirroring the package's
// existing split (bundle_test.go for on-disk IO, *_test.go for model-
// update flow against the tcell model):
//
//   - the non-TUI file-write backend (copyFileIntoBundle /
//     uniqueBundleFileName): create-or-reuse the bundle, base-name
//     forking, auto-suffix collision policy;
//   - the model-update flow (View.HandleEvent): C opens the prompt, a
//     name commits the copy + switches into authoring mode, the BUNDLES
//     list refreshes, and the gate / cancel paths.

import (
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// --- non-TUI file-write backend -------------------------------------

// TestCopyFileIntoBundleCreatesAndSuffixes covers the happy path (a new
// bundle is created on demand and the source lands under the pack file's
// base name) plus the auto-suffix collision policy (a second/third fork
// of the same file never clobbers the first).
func TestCopyFileIntoBundleCreatesAndSuffixes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	name, err := copyFileIntoBundle("mybundle", "automations.memql", "automation a {}")
	if err != nil {
		t.Fatalf("copyFileIntoBundle: %v", err)
	}
	if name != "automations.memql" {
		t.Fatalf("first copy name = %q, want automations.memql", name)
	}
	// The bundle was created on demand.
	if bs, _ := listBundles(); len(bs) != 1 || bs[0] != "mybundle" {
		t.Fatalf("listBundles = %v, want [mybundle]", bs)
	}
	if got, _ := readBundleFile("mybundle", "automations.memql"); got != "automation a {}" {
		t.Fatalf("read back = %q, want %q", got, "automation a {}")
	}

	// Second fork of the same name auto-suffixes and leaves the original
	// untouched.
	name2, err := copyFileIntoBundle("mybundle", "automations.memql", "automation b {}")
	if err != nil {
		t.Fatalf("second copy: %v", err)
	}
	if name2 != "automations-1.memql" {
		t.Fatalf("collision name = %q, want automations-1.memql", name2)
	}
	if got, _ := readBundleFile("mybundle", "automations.memql"); got != "automation a {}" {
		t.Errorf("original clobbered on collision: got %q", got)
	}
	if got, _ := readBundleFile("mybundle", "automations-1.memql"); got != "automation b {}" {
		t.Errorf("suffixed file content = %q, want %q", got, "automation b {}")
	}

	// Third collision advances to -2.
	if name3, _ := copyFileIntoBundle("mybundle", "automations.memql", "c"); name3 != "automations-2.memql" {
		t.Errorf("third collision name = %q, want automations-2.memql", name3)
	}
}

// TestCopyFileIntoBundleBaseName checks a pack path carrying a sub-
// directory forks under its base name only (bundles are flat), preserving
// the .tmpl extension.
func TestCopyFileIntoBundleBaseName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	name, err := copyFileIntoBundle("b", "prompts/agentReply.tmpl", "hi")
	if err != nil {
		t.Fatalf("copyFileIntoBundle: %v", err)
	}
	if name != "agentReply.tmpl" {
		t.Fatalf("name = %q, want agentReply.tmpl (base name only)", name)
	}
	if _, err := readBundleFile("b", "agentReply.tmpl"); err != nil {
		t.Errorf("file not written under base name: %v", err)
	}
}

// TestCopyFileIntoBundleReuseAndReject covers reusing a pre-existing
// bundle (create-or-reuse, not duplicate) and rejecting an unsafe bundle
// name.
func TestCopyFileIntoBundleReuseAndReject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := createBundle("existing"); err != nil {
		t.Fatalf("createBundle: %v", err)
	}
	if _, err := copyFileIntoBundle("existing", "queries.memql", "query q {}"); err != nil {
		t.Fatalf("copy into existing bundle: %v", err)
	}
	if bs, _ := listBundles(); len(bs) != 1 {
		t.Fatalf("listBundles = %v, want exactly one (reused, not duplicated)", bs)
	}
	if _, err := copyFileIntoBundle("../evil", "q.memql", "x"); err == nil {
		t.Error("copyFileIntoBundle with a traversal bundle name should error")
	}
}

// --- model-update flow ----------------------------------------------

// sourceView builds a browse-mode View with a pack file already "viewed"
// (loadedFile + srcText populated), focused on the SOURCE pane -- the
// precondition for the copy affordance.
func sourceView(t *testing.T, file, src string) *View {
	t.Helper()
	v := NewView(ui.DefaultTheme())
	v.SenseClient = nilSense
	v.loadedDomain = "deployment"
	v.loadedFile = file
	v.srcText = src
	v.viewer.Lines = coloredLines(src, nil, v.Theme)
	v.Focus = FocusSource
	return v
}

func typeInto(v *View, s string) {
	for _, r := range s {
		v.HandleEvent(rune_(r))
	}
}

// TestCopyToBundleFlow drives the full affordance: C opens the prompt,
// typing a name + Enter forks the viewed file into a new bundle, switches
// into authoring mode on the copy, and refreshes the BUNDLES list so the
// new bundle is open + editable immediately (deliverables 1 + 2).
func TestCopyToBundleFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v := sourceView(t, "automations.memql", "automation deployX {}")

	v.HandleEvent(rune_('C'))
	if !v.copyPrompt {
		t.Fatal("C on a loaded SOURCE should open the copy prompt")
	}

	typeInto(v, "mybundle")
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if v.copyPrompt {
		t.Fatal("prompt should be closed after commit")
	}
	if !v.modeIsAuthor() {
		t.Fatal("commit should switch the Editor tab into authoring mode")
	}
	if v.author == nil {
		t.Fatal("authoring sub-view should be built on commit")
	}

	// File landed on disk under the original name with the viewed source.
	got, err := readBundleFile("mybundle", "automations.memql")
	if err != nil {
		t.Fatalf("readBundleFile: %v", err)
	}
	if got != "automation deployX {}" {
		t.Fatalf("copied content = %q, want the viewed source", got)
	}

	// Deliverable 2: the authoring BUNDLES list refreshed and the copy is
	// open in the editor pane.
	a := v.author
	a.mu.Lock()
	bundles := slices.Clone(a.bundles)
	openBundle, openFile, focus, hasEditor := a.openBundle, a.openFile, a.focus, a.editor != nil
	a.mu.Unlock()

	if !slices.Contains(bundles, "mybundle") {
		t.Fatalf("authoring bundles = %v, want it to contain the new bundle", bundles)
	}
	if openBundle != "mybundle" || openFile != "automations.memql" {
		t.Fatalf("authoring open = %q/%q, want mybundle/automations.memql", openBundle, openFile)
	}
	if focus != authorEditor || !hasEditor {
		t.Fatalf("authoring focus=%v hasEditor=%v, want the editor pane focused with a buffer", focus, hasEditor)
	}
}

// TestCopyToBundleReuseExistingBundle verifies typing an existing bundle
// name reuses that bundle (pick-or-create via one text field) rather than
// erroring on a duplicate.
func TestCopyToBundleReuseExistingBundle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := createBundle("shared"); err != nil {
		t.Fatalf("createBundle: %v", err)
	}
	v := sourceView(t, "queries.memql", "query q {}")

	v.HandleEvent(rune_('C'))
	typeInto(v, "shared")
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if v.copyPrompt {
		t.Fatalf("commit into an existing bundle should succeed; promptErr=%q", v.copyPromptErr)
	}
	if _, err := readBundleFile("shared", "queries.memql"); err != nil {
		t.Fatalf("file not written into the existing bundle: %v", err)
	}
	if bs, _ := listBundles(); len(bs) != 1 {
		t.Fatalf("listBundles = %v, want exactly one (reused)", bs)
	}
}

// TestCopyToBundleGatedNoFile: C with no file loaded doesn't open the
// prompt and nudges the user to view a file first.
func TestCopyToBundleGatedNoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v := NewView(ui.DefaultTheme())
	v.SenseClient = nilSense
	var status []string
	v.OnStatus = func(m string) { status = append(status, m) }
	v.Focus = FocusSource

	v.HandleEvent(rune_('C'))
	if v.copyPrompt {
		t.Fatal("C with no loaded file should not open the prompt")
	}
	if !strings.Contains(strings.Join(status, " "), "view a file first") {
		t.Errorf("expected a 'view a file first' status, got %v", status)
	}
}

// TestCopyToBundleEscCancels: Esc dismisses the prompt without writing
// anything or switching modes.
func TestCopyToBundleEscCancels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v := sourceView(t, "queries.memql", "query q {}")

	v.HandleEvent(rune_('C'))
	if !v.copyPrompt {
		t.Fatal("C should open the prompt")
	}
	typeInto(v, "x")
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))

	if v.copyPrompt {
		t.Fatal("Esc should cancel the copy prompt")
	}
	if v.modeIsAuthor() {
		t.Fatal("Esc-cancel should not switch into authoring mode")
	}
	if bs, _ := listBundles(); len(bs) != 0 {
		t.Errorf("Esc-cancel should not create any bundle; got %v", bs)
	}
}

// TestCopyToBundleInvalidNameKeepsPrompt: an unsafe bundle name surfaces
// an inline error and leaves the prompt open (no mode switch, no write).
func TestCopyToBundleInvalidNameKeepsPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v := sourceView(t, "queries.memql", "query q {}")

	v.HandleEvent(rune_('C'))
	typeInto(v, "../evil")
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if !v.copyPrompt {
		t.Fatal("an invalid name should keep the prompt open")
	}
	if v.copyPromptErr == "" {
		t.Error("expected an inline validation error on an unsafe name")
	}
	if v.modeIsAuthor() {
		t.Error("invalid name should not switch into authoring mode")
	}
}

// TestCopyToBundleFromFilesFocus: the affordance is also reachable from
// the FILES pane (it forks the loaded source either way).
func TestCopyToBundleFromFilesFocus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v := sourceView(t, "specs.memql", "spec s {}")
	v.Focus = FocusFiles

	v.HandleEvent(rune_('C'))
	if !v.copyPrompt {
		t.Fatal("C from FILES focus should open the copy prompt")
	}
}

// TestCopyPromptModalSwallowsToggle: while the prompt is open it owns
// every key, so Ctrl+B (the mode toggle) does NOT fire underneath it.
func TestCopyPromptModalSwallowsToggle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	v := sourceView(t, "queries.memql", "query q {}")

	v.HandleEvent(rune_('C'))
	if !v.copyPrompt {
		t.Fatal("C should open the prompt")
	}
	// Ctrl+B must be swallowed by the modal prompt, not toggle the mode.
	v.HandleEvent(tcell.NewEventKey(tcell.KeyCtrlB, 0, tcell.ModCtrl))
	if v.modeIsAuthor() {
		t.Fatal("Ctrl+B should be swallowed while the copy prompt is modal")
	}
	if !v.copyPrompt {
		t.Fatal("prompt should still be open after a swallowed key")
	}
}

// TestSourceHintAdvertisesCopy: the SOURCE hint band advertises the new
// C:Copy action (deliverable 3).
func TestSourceHintAdvertisesCopy(t *testing.T) {
	v := seededView()
	v.viewer.Lines = []ui.ViewerLine{{Text: "query foo {}"}}
	if hint := v.hintsForSource(); !strings.Contains(hint, "C:Copy") {
		t.Errorf("SOURCE hint should advertise C:Copy; got %q", hint)
	}
}

// TestCopyPromptRenders: the prompt paints its label + typed input over
// the SOURCE pane.
func TestCopyPromptRenders(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(viewWidth, viewHeight)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)

	v := seededView()
	v.loadedFile = "automations.memql"
	v.srcText = "query foo"
	v.viewer.Lines = coloredLines(v.srcText, nil, v.Theme)
	v.Focus = FocusSource
	v.copyPrompt = true
	v.copyPromptInput = "mybundle"

	v.Draw(screen, ui.Rect{X: 0, Y: 0, Width: viewWidth, Height: viewHeight})
	sim.Show()

	joined := strings.Join(flattenSim(sim), "")
	if !strings.Contains(joined, "Copy to bundle") {
		t.Errorf("copy prompt label not rendered")
	}
	if !strings.Contains(joined, "mybundle") {
		t.Errorf("copy prompt typed input not rendered")
	}
}
