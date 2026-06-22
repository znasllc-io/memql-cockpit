package concepts

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// TestRowListHint_MoreChip_OnlyWhenCursorRemains proves the M:More
// affordance is advertised exactly when a keyset continuation cursor
// remains (memql#2008) -- a hint that lies rots trust (panel chrome
// contract). rowsHasMore is the cursor-present flag the row fetch +
// LoadMoreRows maintain from PageResult.NextCursor.
func TestRowListHint_MoreChip_OnlyWhenCursorRemains(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.Rows = []map[string]any{
		{"id": "v1:cognition:utterance:u-1"},
	}
	v.rowMatches = []int{0}
	v.rowList.Count = 1

	// No cursor remaining -> no M:More chip.
	v.rowsHasMore = false
	if got := hintsForRows(v).text; strings.Contains(got, "M:More") {
		t.Errorf("M:More must NOT appear when the concept is fully loaded; got %q", got)
	}

	// Cursor remaining -> M:More chip present.
	v.rowsHasMore = true
	if got := hintsForRows(v).text; !strings.Contains(got, "M:More") {
		t.Errorf("M:More must appear while a continuation cursor remains; got %q", got)
	}
}

// TestLoadMoreRows_NoOpWhenExhausted guards the early-return guards on
// LoadMoreRows: with no cursor / no more pages it must not panic or mutate
// state, even with a nil QueryClient.
func TestLoadMoreRows_NoOpWhenExhausted(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.rowsHasMore = false
	v.rowsCursor = ""
	before := len(v.Rows)
	v.LoadMoreRows() // must be a no-op, not a panic
	if len(v.Rows) != before {
		t.Errorf("LoadMoreRows mutated rows when exhausted: before=%d after=%d", before, len(v.Rows))
	}
}
