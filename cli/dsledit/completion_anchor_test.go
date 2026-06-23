package dsledit

// Tests for the authoring completion live-filter + cursor-anchored
// popups (memql-cockpit#259).

import (
	"testing"

	"github.com/znasllc-io/memql/sdk/go/sense"

	"github.com/znasllc-io/memql-cockpit/cli/editor"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

func TestIdentifierPrefix(t *testing.T) {
	cases := []struct {
		line string
		col  int
		want string
	}{
		{"query foo", 5, "query"}, // cursor right after "query"
		{"query foo", 9, "foo"},   // cursor at end of "foo"
		{"query foo", 6, ""},      // cursor after the space
		{"  spec", 6, "spec"},     // leading whitespace
		{"a.b", 3, "b"},           // dot is not an identifier char
		{"", 0, ""},
	}
	for _, c := range cases {
		if got := identifierPrefix(c.line, c.col); got != c.want {
			t.Errorf("identifierPrefix(%q, %d) = %q, want %q", c.line, c.col, got, c.want)
		}
	}
}

func authoringWithEditor(t *testing.T, src string, col int) *authoring {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	a := newAuthoring(ui.DefaultTheme(), nilSense, nilAuthoring, func(string) {}, func() {})
	a.editor = editor.NewEditor(editor.NewBuffer(src, "f.memql", false), a.theme)
	a.editor.CursorLine = 0
	a.editor.CursorCol = col
	return a
}

func TestFilterCompletionNarrowsAndCloses(t *testing.T) {
	a := authoringWithEditor(t, "que", 3)
	a.completion.Show([]sense.CompletionItem{
		{Label: "query"}, {Label: "queue"}, {Label: "foo"},
	}, 0, 0)

	a.filterCompletionLocked()
	if len(a.completion.Filtered) != 2 {
		t.Fatalf("prefix 'que' -> %d filtered, want 2 (query, queue)", len(a.completion.Filtered))
	}

	// Cursor not in a word -> popup closes.
	a.editor = editor.NewEditor(editor.NewBuffer("que ", "f.memql", false), a.theme)
	a.editor.CursorCol = 4 // after the space
	a.completion.Show([]sense.CompletionItem{{Label: "query"}}, 0, 0)
	a.filterCompletionLocked()
	if a.completion.Visible {
		t.Fatal("popup should close when the cursor isn't in an identifier")
	}
}

func TestAcceptCompletionReplacesPrefix(t *testing.T) {
	a := authoringWithEditor(t, "que", 3)
	a.completion.Show([]sense.CompletionItem{{Label: "query"}}, 0, 0)
	a.acceptCompletionLocked(&sense.CompletionItem{Label: "query"})

	if got := a.editor.Buffer.Source(); got != "query" {
		t.Fatalf("after accepting 'query' over 'que', buffer = %q, want %q", got, "query")
	}
	if a.editor.CursorCol != 5 {
		t.Errorf("cursor col = %d, want 5 (end of 'query')", a.editor.CursorCol)
	}
	if a.completion.Visible {
		t.Error("popup should close after accepting")
	}
}
