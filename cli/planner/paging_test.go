package planner

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// TestPlanListHint_MoreChip_OnlyWhenCursorRemains proves the M:More
// affordance is advertised exactly when a keyset continuation cursor
// remains (memql#1997) -- a hint that lies rots trust (panel chrome
// contract). plansHasMore is the cursor-present flag RefreshPlans /
// LoadMorePlans maintain from PageResult.NextCursor.
func TestPlanListHint_MoreChip_OnlyWhenCursorRemains(t *testing.T) {
	v := NewView(ui.DefaultTheme())
	v.plans = []map[string]any{
		{"id": "plan-1", "status": "running", "createdAt": "2026-05-17T12:00:00Z"},
	}
	v.planList.Count = 1

	// No cursor remaining -> no M:More chip.
	v.plansHasMore = false
	if got := hintsForPlans(v); strings.Contains(got, "M:More") {
		t.Errorf("M:More must NOT appear when the set is exhausted; got %q", got)
	}

	// Cursor remaining -> M:More chip present.
	v.plansHasMore = true
	if got := hintsForPlans(v); !strings.Contains(got, "M:More") {
		t.Errorf("M:More must appear while a continuation cursor remains; got %q", got)
	}
}
