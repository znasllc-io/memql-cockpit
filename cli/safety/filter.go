package safety

import "strings"

// Filter narrows the visible classification rows by the operator-
// driven dimensions called out in memql-cockpit#134: surface,
// decision, source, tier, mode, plus a free-text Search that runs
// over surface/action/reason/ruleId.
//
// Empty string means "any" on every axis (the * chip in the
// filter-status row). Search is case-insensitive substring.
type Filter struct {
	Surface  string
	Decision string
	Source   string
	Tier     string
	Mode     string
	Search   string
}

// Match returns true when row passes every active dimension.
func (f Filter) Match(row map[string]any) bool {
	if f.Surface != "" && getString(row, "surface") != f.Surface {
		return false
	}
	if f.Decision != "" && getString(row, "decision") != f.Decision {
		return false
	}
	if f.Source != "" && getString(row, "source") != f.Source {
		return false
	}
	if f.Tier != "" && getString(row, "tier") != f.Tier {
		return false
	}
	if f.Mode != "" && getString(row, "mode") != f.Mode {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Search)); q != "" {
		hit := false
		for _, key := range []string{"surface", "action", "reason", "ruleId"} {
			if strings.Contains(strings.ToLower(getString(row, key)), q) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// Apply returns the subset of rows that match f, preserving order.
func (f Filter) Apply(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if f.Match(r) {
			out = append(out, r)
		}
	}
	return out
}

// Aggregate summarises a row set on the dimensions the operator
// cares about: total count + breakdowns by decision / source / tier
// / mode. Reported across whatever rows the caller passes in, so
// callers can compute "across the active filter" by passing the
// already-filtered slice.
type Aggregate struct {
	Total    int
	Decision map[string]int
	Source   map[string]int
	Tier     map[string]int
	Mode     map[string]int
}

// Summarise builds an Aggregate from rows. The breakdown maps are
// always non-nil so callers can index without nil-checks.
func Summarise(rows []map[string]any) Aggregate {
	agg := Aggregate{
		Decision: map[string]int{},
		Source:   map[string]int{},
		Tier:     map[string]int{},
		Mode:     map[string]int{},
	}
	for _, r := range rows {
		agg.Total++
		bump(agg.Decision, getString(r, "decision"))
		bump(agg.Source, getString(r, "source"))
		bump(agg.Tier, getString(r, "tier"))
		bump(agg.Mode, getString(r, "mode"))
	}
	return agg
}

func bump(m map[string]int, k string) {
	if k == "" {
		k = "(none)"
	}
	m[k]++
}

// distinctSurfaces returns the set of surface values present in
// rows, in stable (sorted) order. Drives the U:Surface cycle so
// the visible set tracks whatever the cluster is actually emitting
// -- no need to hard-code a surface list as new ones come online.
func distinctSurfaces(rows []map[string]any) []string {
	seen := map[string]struct{}{}
	for _, r := range rows {
		s := getString(r, "surface")
		if s == "" {
			continue
		}
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sortStrings(out)
	return out
}

// cycleValues are the fixed enums the dedicated filter keys cycle
// through. The empty string at index 0 is the "any" state -- the
// view advances through them with modular arithmetic and renders
// "*" for the empty slot.
//
// surface is dynamic (distinctSurfaces above), so it isn't here.
var (
	decisionCycle = []string{"", "allow", "ask", "deny"}
	sourceCycle   = []string{"", "rule", "model", "cache", "noop", "disabled"}
	tierCycle     = []string{"", "none", "low", "medium", "high", "critical"}
	modeCycle     = []string{"", "off", "shadow", "enforce"}
)

// cycleNext advances cur through cycle, wrapping past the end. When
// cur isn't in cycle (e.g. an unknown source value the operator
// hasn't seen before), we fall back to the empty state so the user
// can always reach "any" again.
func cycleNext(cycle []string, cur string) string {
	for i, v := range cycle {
		if v == cur {
			return cycle[(i+1)%len(cycle)]
		}
	}
	return ""
}

// cycleSurface advances cur through the dynamic surface set
// {"", <distinct surfaces>}, wrapping.
func cycleSurface(rows []map[string]any, cur string) string {
	surfaces := distinctSurfaces(rows)
	if len(surfaces) == 0 {
		return ""
	}
	full := append([]string{""}, surfaces...)
	return cycleNext(full, cur)
}

// chipValue returns the cell text for a filter chip: "*" when no
// filter is set, otherwise the literal value.
func chipValue(v string) string {
	if v == "" {
		return "*"
	}
	return v
}

// getString reads a string field off a row map, returning "" when
// missing or wrong type. Local copy so the package stands alone
// from the parallel helper in cli/planner / cli/concepts.
func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if raw, ok := m[key]; ok {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	return ""
}

// getFloat reads a float field off a row map. JSON numbers arrive
// as float64 through protojson, matching the shape used by the
// planner view's getInt / getFloat helpers.
func getFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	if raw, ok := m[key]; ok {
		if f, ok := raw.(float64); ok {
			return f
		}
	}
	return 0
}

// sortStrings is the dependency-free string sort used by
// distinctSurfaces. Inlined to avoid pulling sort across the
// filter helpers (the slice is always small -- one entry per
// distinct surface in the current row set).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
