package safety

import "testing"

// Three rows covering the dimensions the operator filters on. The
// fixtures encode the dimensions the view's chip cycle exposes:
// surface, decision, source, tier, mode, plus the free-text reason
// the search hits.
var sampleRows = []map[string]any{
	{
		"surface":  "workbench",
		"action":   "exec",
		"decision": "deny",
		"source":   "rule",
		"tier":     "high",
		"mode":     "shadow",
		"reason":   "rm -rf root",
		"ruleId":   "shell.destructive",
	},
	{
		"surface":  "computer_use_headless",
		"action":   "fs_read",
		"decision": "allow",
		"source":   "cache",
		"tier":     "low",
		"mode":     "enforce",
		"reason":   "read project config",
		"ruleId":   "",
	},
	{
		"surface":  "workbench",
		"action":   "http_fetch",
		"decision": "ask",
		"source":   "model",
		"tier":     "medium",
		"mode":     "shadow",
		"reason":   "credential_access pattern in URL",
		"ruleId":   "model.classify_v1",
	},
}

func TestFilter_MatchEmpty(t *testing.T) {
	// Zero-value filter passes every row.
	f := Filter{}
	for i, r := range sampleRows {
		if !f.Match(r) {
			t.Errorf("row %d: empty filter should match every row", i)
		}
	}
}

func TestFilter_MatchByEachAxis(t *testing.T) {
	cases := []struct {
		name string
		f    Filter
		want int // number of matching rows from sampleRows
	}{
		{"surface=workbench", Filter{Surface: "workbench"}, 2},
		{"decision=deny", Filter{Decision: "deny"}, 1},
		{"decision=ask", Filter{Decision: "ask"}, 1},
		{"source=cache", Filter{Source: "cache"}, 1},
		{"tier=low", Filter{Tier: "low"}, 1},
		{"mode=shadow", Filter{Mode: "shadow"}, 2},
		{"search reason substring", Filter{Search: "credential"}, 1},
		{"search ruleId substring", Filter{Search: "shell."}, 1},
		{"search case-insensitive", Filter{Search: "ROOT"}, 1},
		{"combined surface+decision", Filter{Surface: "workbench", Decision: "ask"}, 1},
		{"unmatched combo", Filter{Surface: "workbench", Decision: "allow"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := len(tc.f.Apply(sampleRows))
			if got != tc.want {
				t.Errorf("Apply: want %d rows, got %d", tc.want, got)
			}
		})
	}
}

func TestSummarise(t *testing.T) {
	agg := Summarise(sampleRows)
	if agg.Total != 3 {
		t.Fatalf("total: want 3, got %d", agg.Total)
	}
	checks := []struct {
		label string
		m     map[string]int
		key   string
		want  int
	}{
		{"decision.allow", agg.Decision, "allow", 1},
		{"decision.ask", agg.Decision, "ask", 1},
		{"decision.deny", agg.Decision, "deny", 1},
		{"source.rule", agg.Source, "rule", 1},
		{"source.model", agg.Source, "model", 1},
		{"source.cache", agg.Source, "cache", 1},
		{"tier.high", agg.Tier, "high", 1},
		{"tier.low", agg.Tier, "low", 1},
		{"tier.medium", agg.Tier, "medium", 1},
		{"mode.shadow", agg.Mode, "shadow", 2},
		{"mode.enforce", agg.Mode, "enforce", 1},
	}
	for _, c := range checks {
		if got := c.m[c.key]; got != c.want {
			t.Errorf("%s: want %d, got %d", c.label, c.want, got)
		}
	}
}

func TestSummarise_Empty(t *testing.T) {
	agg := Summarise(nil)
	if agg.Total != 0 {
		t.Fatalf("empty: total %d, want 0", agg.Total)
	}
	if agg.Decision == nil || agg.Source == nil || agg.Tier == nil || agg.Mode == nil {
		t.Fatal("empty Summarise must still return non-nil breakdown maps so callers can index safely")
	}
}

func TestCycleNext_Wraps(t *testing.T) {
	cycle := []string{"", "a", "b", "c"}
	got := []string{}
	cur := ""
	for i := 0; i < 5; i++ {
		cur = cycleNext(cycle, cur)
		got = append(got, cur)
	}
	want := []string{"a", "b", "c", "", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestCycleNext_UnknownResetsToEmpty(t *testing.T) {
	// Unknown current value should fall back to "" so the user can
	// always reach the "any" state via repeated presses.
	got := cycleNext(decisionCycle, "bogus")
	if got != "" {
		t.Errorf("unknown -> want \"\", got %q", got)
	}
}

func TestCycleSurface_DynamicSet(t *testing.T) {
	// Surface cycle is dynamic: derived from distinct values present
	// in the row set. Expected order is sorted ascending after the
	// "any" slot.
	rows := []map[string]any{
		{"surface": "zeta"},
		{"surface": "alpha"},
		{"surface": "alpha"}, // duplicate, must dedupe
		{"surface": ""},      // empty, must skip
	}
	steps := []string{}
	cur := ""
	for i := 0; i < 4; i++ {
		cur = cycleSurface(rows, cur)
		steps = append(steps, cur)
	}
	want := []string{"alpha", "zeta", "", "alpha"}
	for i := range want {
		if steps[i] != want[i] {
			t.Errorf("step %d: want %q, got %q", i, want[i], steps[i])
		}
	}
}

func TestChipValue(t *testing.T) {
	if chipValue("") != "*" {
		t.Errorf("empty filter should render as * chip, got %q", chipValue(""))
	}
	if chipValue("rule") != "rule" {
		t.Errorf("set filter should render as literal value")
	}
}
