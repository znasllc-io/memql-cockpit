package worker

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics is a tiny self-contained counter / histogram surface
// exposed at 127.0.0.1:9100/metrics in prometheus text format.
//
// Loopback-only by design: the worker is the user's own machine
// and the metrics endpoint is a debugging aid, not a publicly
// exposed scrape target. Operators can `curl
// http://127.0.0.1:9100/metrics` from the same box. No
// authentication; binding to 127.0.0.1 is the gate.
//
// We hand-roll the text format rather than importing
// prometheus/client_golang because the cockpit binary should stay
// small and CGO-free on the headless build.
type Metrics struct {
	startedAt time.Time

	callsTotal      atomic.Int64
	callsByOutcome  map[string]*atomic.Int64
	callsByOutcomeMu sync.RWMutex

	durationBucketsMs []int64
	bucketCounts      []atomic.Int64
	durationSumMs     atomic.Int64
	durationCount     atomic.Int64

	reconnects atomic.Int64

	server *http.Server
	listener net.Listener
}

// NewMetrics constructs a Metrics surface.
func NewMetrics() *Metrics {
	// Histogram buckets: 50ms, 100ms, 250ms, 500ms, 1s, 2.5s, 5s, 10s, 30s, 60s, +Inf.
	buckets := []int64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}
	m := &Metrics{
		startedAt:         time.Now().UTC(),
		callsByOutcome:    make(map[string]*atomic.Int64),
		durationBucketsMs: buckets,
		bucketCounts:      make([]atomic.Int64, len(buckets)+1), // +1 for +Inf overflow
	}
	return m
}

// RecordCall observes a single tool-dispatch outcome.
func (m *Metrics) RecordCall(outcome string, durationMs int64) {
	if m == nil {
		return
	}
	m.callsTotal.Add(1)
	m.incrementOutcome(outcome)
	m.durationSumMs.Add(durationMs)
	m.durationCount.Add(1)
	for i, b := range m.durationBucketsMs {
		if durationMs <= b {
			m.bucketCounts[i].Add(1)
			return
		}
	}
	m.bucketCounts[len(m.bucketCounts)-1].Add(1)
}

// RecordReconnect increments the reconnect counter.
func (m *Metrics) RecordReconnect() {
	if m == nil {
		return
	}
	m.reconnects.Add(1)
}

func (m *Metrics) incrementOutcome(outcome string) {
	m.callsByOutcomeMu.RLock()
	c, ok := m.callsByOutcome[outcome]
	m.callsByOutcomeMu.RUnlock()
	if ok {
		c.Add(1)
		return
	}
	m.callsByOutcomeMu.Lock()
	defer m.callsByOutcomeMu.Unlock()
	if c, ok := m.callsByOutcome[outcome]; ok {
		c.Add(1)
		return
	}
	c = &atomic.Int64{}
	c.Add(1)
	m.callsByOutcome[outcome] = c
}

// Listen starts the HTTP server on 127.0.0.1:port. Binding to
// loopback is a hard requirement -- the metrics endpoint is
// unauthenticated.
//
// If the requested port is already in use (commonly :9100, the
// Prometheus node_exporter default, which a developer's machine
// frequently has running), this falls back through the next four
// ports (port+1 ... port+4) before giving up. Callers see
// success on the first port that bound; the chosen port is
// readable via ListenAddr().
func (m *Metrics) Listen(port int) error {
	if m == nil {
		return nil
	}
	const fallbackAttempts = 5
	var (
		listener net.Listener
		addr     string
		lastErr  error
	)
	for offset := 0; offset < fallbackAttempts; offset++ {
		addr = fmt.Sprintf("127.0.0.1:%d", port+offset)
		l, err := net.Listen("tcp", addr)
		if err == nil {
			listener = l
			break
		}
		lastErr = err
	}
	if listener == nil {
		return fmt.Errorf("metrics: listen 127.0.0.1:%d-%d: %w", port, port+fallbackAttempts-1, lastErr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.handleMetrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	m.listener = listener
	m.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = m.server.Serve(listener)
	}()
	return nil
}

// ListenAddr returns the loopback address the metrics server
// bound to, e.g. "127.0.0.1:9101" when 9100 was taken and the
// fallback search picked the next free port.
func (m *Metrics) ListenAddr() string {
	if m == nil || m.listener == nil {
		return ""
	}
	return m.listener.Addr().String()
}

// Stop closes the metrics listener.
func (m *Metrics) Stop() {
	if m == nil || m.server == nil {
		return
	}
	_ = m.server.Close()
	if m.listener != nil {
		_ = m.listener.Close()
	}
}

func (m *Metrics) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	uptime := time.Since(m.startedAt).Seconds()

	fmt.Fprintln(&b, "# HELP worker_uptime_seconds Seconds since the worker process started.")
	fmt.Fprintln(&b, "# TYPE worker_uptime_seconds gauge")
	fmt.Fprintf(&b, "worker_uptime_seconds %.3f\n", uptime)

	fmt.Fprintln(&b, "# HELP worker_calls_total Total tool-dispatch calls observed.")
	fmt.Fprintln(&b, "# TYPE worker_calls_total counter")
	fmt.Fprintf(&b, "worker_calls_total %d\n", m.callsTotal.Load())

	fmt.Fprintln(&b, "# HELP worker_calls_by_outcome_total Tool-dispatch calls by terminal outcome.")
	fmt.Fprintln(&b, "# TYPE worker_calls_by_outcome_total counter")
	m.callsByOutcomeMu.RLock()
	keys := make([]string, 0, len(m.callsByOutcome))
	for k := range m.callsByOutcome {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "worker_calls_by_outcome_total{outcome=%q} %d\n", k, m.callsByOutcome[k].Load())
	}
	m.callsByOutcomeMu.RUnlock()

	fmt.Fprintln(&b, "# HELP worker_call_duration_ms Tool-dispatch call duration in milliseconds.")
	fmt.Fprintln(&b, "# TYPE worker_call_duration_ms histogram")
	cumulative := int64(0)
	for i, bound := range m.durationBucketsMs {
		cumulative += m.bucketCounts[i].Load()
		fmt.Fprintf(&b, "worker_call_duration_ms_bucket{le=\"%d\"} %d\n", bound, cumulative)
	}
	cumulative += m.bucketCounts[len(m.bucketCounts)-1].Load()
	fmt.Fprintf(&b, "worker_call_duration_ms_bucket{le=\"+Inf\"} %d\n", cumulative)
	fmt.Fprintf(&b, "worker_call_duration_ms_sum %d\n", m.durationSumMs.Load())
	fmt.Fprintf(&b, "worker_call_duration_ms_count %d\n", m.durationCount.Load())

	fmt.Fprintln(&b, "# HELP worker_reconnects_total Reconnect attempts since process start.")
	fmt.Fprintln(&b, "# TYPE worker_reconnects_total counter")
	fmt.Fprintf(&b, "worker_reconnects_total %d\n", m.reconnects.Load())

	_, _ = io.WriteString(w, b.String())
}
