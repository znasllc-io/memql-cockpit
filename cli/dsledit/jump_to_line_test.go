package dsledit

// Jump-to-line coverage (memql#2375): the Validate/Inject results overlay
// renders a failing construct's authored line:col and Enter jumps the editor to
// it -- mapping the concatenated-bundle line back to the owning file.

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	authoringsdk "github.com/znasllc-io/memql/sdk/go/authoring"
)

// diagnosticLine prefixes the authored position and marks a failing diagnostic
// jumpable; an OK/skipped one (or one with no position) is not jumpable.
func TestDiagnosticLine_PositionAndJumpable(t *testing.T) {
	pos := diagnosticLine(authoringsdk.Diagnostic{Name: "queryBad", Kind: "query", Line: 12, Column: 8, Error: "bad token"})
	if pos.line != 12 || pos.col != 8 {
		t.Errorf("jump coords = %d:%d, want 12:8", pos.line, pos.col)
	}
	if !pos.jumpable() {
		t.Error("a positioned failure must be jumpable")
	}
	if want := "@12:8"; !contains(pos.text, want) {
		t.Errorf("text %q missing %q", pos.text, want)
	}

	// Line but no column -> "@L" (never "@L:0"). No error text, so the only
	// colon that could appear is a bogus column separator.
	lineOnly := diagnosticLine(authoringsdk.Diagnostic{Name: "x", Kind: "logic", Line: 5})
	if !contains(lineOnly.text, "@5") || contains(lineOnly.text, "@5:") {
		t.Errorf("line-only text %q should read @5 with no column", lineOnly.text)
	}

	// No position -> not jumpable, no "@".
	noPos := diagnosticLine(authoringsdk.Diagnostic{Name: "y", Kind: "spec", Error: "e"})
	if noPos.jumpable() || contains(noPos.text, "@") {
		t.Errorf("unpositioned diagnostic must not be jumpable: %+v", noPos)
	}
	// An OK diagnostic is never jumpable.
	if diagnosticLine(authoringsdk.Diagnostic{Name: "z", Kind: "query", OK: true}).jumpable() {
		t.Error("an OK diagnostic must not be jumpable")
	}
}

// bundleFileForLine maps a concatenated-bundle line back to (file, file-line).
func TestBundleFileForLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mustBundle(t, "b")
	// a.memql = 2 lines, z.memql = 2 lines. Concatenation (sorted, "\n\n"
	// separated): a -> blob 1-2, empty separator -> blob 3, z -> blob 4-5.
	mustFile(t, "b", "a", "query a {\n}")
	mustFile(t, "b", "z", "query z {\n}")

	cases := []struct {
		blobLine int
		wantFile string
		wantLine int
		wantOK   bool
	}{
		{1, "a.memql", 1, true},
		{2, "a.memql", 2, true},
		{4, "z.memql", 1, true},
		{5, "z.memql", 2, true},
		{3, "", 0, false},   // the empty separator line
		{100, "", 0, false}, // out of range
		{0, "", 0, false},   // invalid
	}
	for _, tc := range cases {
		file, line, ok := bundleFileForLine("b", tc.blobLine)
		if ok != tc.wantOK || file != tc.wantFile || line != tc.wantLine {
			t.Errorf("bundleFileForLine(%d) = (%q, %d, %v), want (%q, %d, %v)",
				tc.blobLine, file, line, ok, tc.wantFile, tc.wantLine, tc.wantOK)
		}
	}
}

// Enter on a positioned diagnostic opens the owning file (a different one than
// the currently-open file) and moves the cursor to the authored line:col.
func TestJumpToSelectedResult_OpensFileAndMovesCursor(t *testing.T) {
	a, _ := newTestAuthoring(t)
	mustBundle(t, "b")
	mustFile(t, "b", "a", "query a {\n}")
	mustFile(t, "b", "z", "query z {\n}")

	a.mu.Lock()
	a.loadBundleFilesLocked("b")
	a.openFileLocked("a.memql") // start in a.memql
	a.mu.Unlock()

	// A diagnostic pointing at blob line 4, col 3 -> z.memql line 1, col 3.
	a.resultLines = []resultEntry{{text: "[!!] queryZ (query) @4:3: bad", sev: sevErr, line: 4, col: 3}}
	a.resultShown = true
	a.selectFirstJumpable()
	if a.resultSel != 0 {
		t.Fatalf("expected the positioned entry selected, got resultSel=%d", a.resultSel)
	}

	a.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if a.openFile != "z.memql" {
		t.Errorf("jump should open z.memql, got %q", a.openFile)
	}
	if a.editor == nil || a.editor.CursorLine != 0 || a.editor.CursorCol != 2 {
		t.Errorf("cursor = (%d,%d), want (0,2) [line 1, col 3 -> 0-based]",
			cursorLine(a), cursorCol(a))
	}
	if a.focus != authorEditor {
		t.Errorf("focus = %v, want authorEditor", a.focus)
	}
	if a.resultShown {
		t.Error("overlay should be dismissed after a jump")
	}
}

// Up/Down move the selection among jumpable entries only, skipping OK/prose
// lines, and wrap at the ends.
func TestMoveResultSelection_SkipsNonJumpable(t *testing.T) {
	a, _ := newTestAuthoring(t)
	a.resultLines = []resultEntry{
		{text: "prose", sev: sevPlain},                       // 0: not jumpable
		{text: "[!!] q1 @3:1", sev: sevErr, line: 3, col: 1}, // 1: jumpable
		{text: "[ok] q2", sev: sevOK},                        // 2: not jumpable
		{text: "[!!] q3 @9:2", sev: sevErr, line: 9, col: 2}, // 3: jumpable
	}
	a.selectFirstJumpable()
	if a.resultSel != 1 {
		t.Fatalf("first jumpable = %d, want 1", a.resultSel)
	}
	a.moveResultSelection(1)
	if a.resultSel != 3 {
		t.Errorf("after down = %d, want 3 (skips the OK line)", a.resultSel)
	}
	a.moveResultSelection(1)
	if a.resultSel != 1 {
		t.Errorf("after wrap-down = %d, want 1", a.resultSel)
	}
	a.moveResultSelection(-1)
	if a.resultSel != 3 {
		t.Errorf("after wrap-up = %d, want 3", a.resultSel)
	}
}

// --- helpers ---

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func mustBundle(t *testing.T, name string) {
	t.Helper()
	if err := createBundle(name); err != nil {
		t.Fatalf("createBundle %q: %v", name, err)
	}
}

func mustFile(t *testing.T, bundle, name, content string) {
	t.Helper()
	if _, err := createBundleFile(bundle, name); err != nil {
		t.Fatalf("createBundleFile %q/%q: %v", bundle, name, err)
	}
	if err := writeBundleFile(bundle, name+".memql", content); err != nil {
		t.Fatalf("writeBundleFile %q/%q: %v", bundle, name, err)
	}
}

func cursorLine(a *authoring) int {
	if a.editor == nil {
		return -1
	}
	return a.editor.CursorLine
}

func cursorCol(a *authoring) int {
	if a.editor == nil {
		return -1
	}
	return a.editor.CursorCol
}
