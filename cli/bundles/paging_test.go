package bundles

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// TestBundleListHint_MoreChip_OnlyWhenCursorRemains proves the M:More
// affordance is advertised exactly when a keyset continuation cursor
// remains (memql#1997). bundlesHasMore is the cursor-present flag
// Refresh / LoadMoreBundles maintain from PageResult.NextCursor.
func TestBundleListHint_MoreChip_OnlyWhenCursorRemains(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.bundles = []map[string]any{
		{"id": "v1:authoring:bundle:aaa", "title": "Draft", "createdAt": "2026-06-08T10:00:00Z"},
	}
	v.bundleList.Count = 1

	v.bundlesHasMore = false
	if got := hintsForList(v); strings.Contains(got, "M:More") {
		t.Errorf("M:More must NOT appear when the set is exhausted; got %q", got)
	}

	v.bundlesHasMore = true
	if got := hintsForList(v); !strings.Contains(got, "M:More") {
		t.Errorf("M:More must appear while a continuation cursor remains; got %q", got)
	}
}
