package cluster

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/architecture/model"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// QueryClientMetricsFetcher is the production MetricsFetcher
// implementation. Issues a single broad query against the
// v1:observability:codeMetric concept and groups the latest row
// per codeReference. The aggregation is done client-side because
// the codeMetric concept ships per-window rows; the navigator only
// wants the most recent window per FQN.
//
// The query is intentionally simple ("all codeMetric rows") rather
// than passing the visible FQN list as filter args -- the MemQL
// parser doesn't yet support IN-style array filters, and a single
// scan is cheap enough at the cardinality the codeMetric table
// reaches in practice (one row per (FQN, 1m bucket), bounded by
// the continuous aggregate's retention).
//
// If the server returns an error or an unexpected shape, the
// fetcher returns (nil, err) and the navigator keeps the previous
// overlay; nothing on the diagram blanks out on transient failures.
type QueryClientMetricsFetcher struct {
	Client *client.QueryClient
}

// FetchRecentMetrics implements ArchView.MetricsFetcher.
func (f *QueryClientMetricsFetcher) FetchRecentMetrics(ctx context.Context) (map[model.ID]MetricSummary, error) {
	if f == nil || f.Client == nil {
		return nil, fmt.Errorf("metrics fetcher has no QueryClient")
	}
	// Admin / observability surface -- concept-agnostic row browse
	// for the architecture-model overlay. Reach the SDK's
	// BrowseConcept escape hatch (see sdk/go/CLAUDE.md) so the
	// raw DSL string lives once, on the SDK side. Future
	// enhancement: switch to a struct-form query under
	// dsl/observability/queries.memql once we have a stable shape.
	res, err := f.Client.BrowseConcept(ctx, "v1:observability:codeMetric")
	if err != nil {
		return nil, err
	}
	return parseMetricsResult(res), nil
}

// parseMetricsResult walks the typed *Result from BrowseConcept and
// collapses it to one MetricSummary per codeReference, keeping the
// row with the latest windowEnd. The SDK's Result.Rows() flattens
// bundle payloads to top-level keys, so we read `row["codeReference"]`
// directly rather than reaching through a nested `payload` map.
func parseMetricsResult(res *client.Result) map[model.ID]MetricSummary {
	rows := res.Rows()
	if len(rows) == 0 {
		return map[model.ID]MetricSummary{}
	}

	latest := make(map[model.ID]struct {
		summary   MetricSummary
		windowEnd string
	}, len(rows))

	for _, row := range rows {
		fqn := stringField(row, "codeReference")
		if fqn == "" {
			continue
		}
		id := model.ID(fqn)
		windowEnd := stringField(row, "windowEnd")
		existing, seen := latest[id]
		if seen && existing.windowEnd >= windowEnd {
			// Already have a same-or-later window for this FQN.
			continue
		}
		latest[id] = struct {
			summary   MetricSummary
			windowEnd string
		}{
			summary: MetricSummary{
				CallCount: intField(row, "callCount"),
				P95DurNs:  intField(row, "p95DurationNs"),
				ErrorRate: floatField(row, "errorRate"),
			},
			windowEnd: windowEnd,
		}
	}

	out := make(map[model.ID]MetricSummary, len(latest))
	for id, v := range latest {
		out[id] = v.summary
	}
	return out
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func intField(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		var n int64
		_, _ = fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

func floatField(m map[string]any, key string) float64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case string:
		var f float64
		_, _ = fmt.Sscanf(x, "%f", &f)
		return f
	}
	return 0
}
