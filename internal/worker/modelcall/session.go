package modelcall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// Stable error codes on ModelCallEnd. They are read by the engine's
// refusal report, which lists every machine considered and why each was
// ruled out, so each one has to name a DIFFERENT fix.
const (
	// CodeModelNotOffered: this machine does not currently advertise the
	// model. Reached when the router's view is a beat stale, or when the
	// owner removed it from models.allow since the advertisement.
	CodeModelNotOffered = "model_not_offered"
	// CodeConcurrencyExceeded: the model is offered but at its cap.
	CodeConcurrencyExceeded = "model_concurrency_exceeded"
	// CodeUnsupportedKind: neither chat nor embedding.
	CodeUnsupportedKind = "unsupported_kind"
	// CodeSchemaUnsupported: a response schema arrived for a model this
	// machine never advertised structured output for.
	CodeSchemaUnsupported = "schema_unsupported"
	// CodeDuplicateRequest: a request id already live on this worker.
	CodeDuplicateRequest = "duplicate_request"
	// CodeRuntimeError: the local runtime failed the call.
	CodeRuntimeError = "runtime_error"
	// CodeCancelled: the cluster cancelled it.
	CodeCancelled = "cancelled"
	// CodeTimeout: a deadline on the envelope expired.
	CodeTimeout = "timeout"
	// CodeWorkerStopped: the stream went, or the worker is draining.
	CodeWorkerStopped = "worker_stopped"
)

// Sender is what the manager needs from the worker's connection.
type Sender interface {
	SendModelCallDelta(requestID string, seq uint64, content string, keepalive bool) error
	SendModelCallEnd(end *memqlv1.ModelCallEnd) error
}

// Inventory is the live view of what this machine offers. It is read at
// ADMISSION rather than captured at connect, so a model dropped from
// models.allow stops being servable at the next call rather than at the
// next reconnect -- the advertisement is a promise, and honouring a call
// outside it would keep a revoked model running on somebody's hardware.
type Inventory interface {
	Models(ctx context.Context) models.Inventory
}

// Options configures NewManager.
type Options struct {
	Logger     *slog.Logger
	Inventory  Inventory
	HTTPClient *http.Client
	// Getenv resolves a declared runtime's api_key_env. Defaults to
	// os.Getenv.
	Getenv func(string) string
}

// Manager owns the live model calls on this worker.
type Manager struct {
	logger    *slog.Logger
	inventory Inventory
	http      *http.Client
	getenv    func(string) string

	mu       sync.Mutex
	live     map[string]*call
	perModel map[string]int
}

type call struct {
	requestID string
	modelID   string
	cancel    context.CancelFunc

	mu     sync.Mutex
	reason string // why it was aborted: FinishCancelled / FinishTimeout
	code   string
	detail string // the human sentence, when the aborter supplied one
}

// NewManager builds the manager the worker runs with.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	client := opts.HTTPClient
	if client == nil {
		// No client-level timeout: the envelope owns the deadlines, and
		// a second one here would cut a legitimate ten-minute generation
		// off at whatever number this file happened to pick.
		client = &http.Client{}
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return &Manager{
		logger:    opts.Logger,
		inventory: opts.Inventory,
		http:      client,
		getenv:    getenv,
		live:      make(map[string]*call),
		perModel:  make(map[string]int),
	}
}

// Live reports how many calls are running. The runner reads it before
// spending a reconnect on a changed model set.
func (m *Manager) Live() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

// Start admits and runs one call. It returns immediately; the call
// reports itself on the stream.
func (m *Manager) Start(ctx context.Context, sender Sender, start *memqlv1.ModelCallStart) {
	if m == nil || start == nil {
		return
	}
	requestID := start.GetRequestId()
	if requestID == "" {
		// Nothing to correlate a refusal with, so there is nothing to
		// send. Dropping it is the only honest option.
		m.logger.Warn("model call arrived with no request id; dropped")
		return
	}

	info, limits, refusal := m.admit(ctx, start)
	if refusal != nil {
		refusal.RequestId = requestID
		if err := sender.SendModelCallEnd(refusal); err != nil {
			m.logger.Warn("failed to send model call refusal", "request_id", requestID, "error", err)
		}
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, limits.timeout)
	c := &call{requestID: requestID, modelID: info.ID, cancel: cancel}

	m.mu.Lock()
	m.live[requestID] = c
	m.perModel[info.ID]++
	m.mu.Unlock()

	go func() {
		defer cancel()
		defer m.finish(requestID, info.ID)
		m.run(callCtx, sender, c, info, limits, start)
	}()
}

// Cancel stops a running call. A cancel for a call this worker is not
// running is ignored rather than reported: the engine cancels
// optimistically on its own timeout paths, and a refusal for a call that
// already ended would be noise the operator has to learn to skip.
func (m *Manager) Cancel(c *memqlv1.ModelCallCancel) {
	if m == nil || c == nil {
		return
	}
	m.abort(c.GetRequestId(), FinishCancelled, CodeCancelled, c.GetReason())
}

// StopAll ends every live call. Called on disconnect and on drain.
//
// The stream is the only channel back to the caller, so a call that
// outlived it has nowhere to report -- but the GENERATION is still
// running on somebody's GPU, and cancelling it is the point. Whether the
// End reaches the cluster is secondary; freeing the hardware is not.
func (m *Manager) StopAll(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.live))
	for id := range m.live {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.abort(id, FinishError, CodeWorkerStopped, reason)
	}
}

func (m *Manager) abort(requestID, finish, code, reason string) {
	m.mu.Lock()
	c := m.live[requestID]
	m.mu.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	// FIRST abort wins. A cancel that arrives while the watchdog is
	// already tearing the call down must not rewrite the reason the
	// caller will be told, or the End would name whichever goroutine
	// happened to be scheduled second.
	if c.reason == "" {
		c.reason, c.code, c.detail = finish, code, reason
	}
	c.mu.Unlock()
	c.cancel()
}

func (m *Manager) finish(requestID, modelID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.live, requestID)
	if n := m.perModel[modelID]; n <= 1 {
		delete(m.perModel, modelID)
	} else {
		m.perModel[modelID] = n - 1
	}
}

// -----------------------------------------------------------------------------
// Admission
// -----------------------------------------------------------------------------

type callLimits struct {
	timeout   time.Duration
	idle      time.Duration
	keepalive time.Duration
}

func limitsFrom(l *memqlv1.ModelCallLimits) callLimits {
	out := callLimits{timeout: DefaultTimeout, idle: DefaultIdleTimeout, keepalive: DefaultKeepalive}
	if l == nil {
		return out
	}
	if n := l.GetTimeoutSeconds(); n > 0 {
		out.timeout = time.Duration(n) * time.Second
	}
	if n := l.GetIdleTimeoutSeconds(); n > 0 {
		out.idle = time.Duration(n) * time.Second
	}
	if n := l.GetKeepaliveSeconds(); n > 0 {
		out.keepalive = time.Duration(n) * time.Second
	}
	// A keepalive at or above the idle ceiling cannot keep anything
	// alive: the first one would arrive after the deadline it exists to
	// push back. Rather than fail a call over a caller's arithmetic, the
	// cadence is tightened to something that works.
	if out.keepalive >= out.idle {
		out.keepalive = out.idle / 2
	}
	if out.keepalive <= 0 {
		out.keepalive = DefaultKeepalive
	}
	return out
}

// admit resolves the model and takes a concurrency slot, or returns the
// End that refuses the call.
func (m *Manager) admit(ctx context.Context, start *memqlv1.ModelCallStart) (models.Info, callLimits, *memqlv1.ModelCallEnd) {
	limits := limitsFrom(start.GetLimits())

	kind := start.GetKind()
	if kind != KindChat && kind != KindEmbedding {
		return models.Info{}, limits, refuse(CodeUnsupportedKind,
			fmt.Sprintf("this worker serves %q and %q; the call asked for %q", KindChat, KindEmbedding, kind))
	}

	var inv models.Inventory
	if m.inventory != nil {
		inv = m.inventory.Models(ctx)
	}
	info, ok := inv.Find(start.GetModel())
	if !ok {
		return models.Info{}, limits, refuse(CodeModelNotOffered,
			fmt.Sprintf("this machine does not currently offer model %q", start.GetModel()))
	}
	if len(start.GetResponseFormatSchema()) > 0 && !info.StructuredOutput {
		// The router only sends a schema to a machine that advertised
		// the capability, so this is a stale advertisement rather than a
		// routing bug -- and answering prose instead would defeat the
		// gating that put the call here.
		return models.Info{}, limits, refuse(CodeSchemaUnsupported,
			fmt.Sprintf("model %q does not advertise structured output on this machine", info.ID))
	}
	if kind == KindEmbedding && !info.Embeddings {
		return models.Info{}, limits, refuse(CodeModelNotOffered,
			fmt.Sprintf("model %q does not advertise embeddings on this machine", info.ID))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.live[start.GetRequestId()]; exists {
		return models.Info{}, limits, refuse(CodeDuplicateRequest,
			"a call with this request id is already running on this worker")
	}
	// Both ceilings, and both are this machine's to hold. The engine
	// rations by the advertised numbers, but the advertisement is a claim
	// about this hardware and two replicas selecting at the same moment
	// is an ordinary race rather than a bug to fix upstream.
	if info.MaxConcurrent > 0 && m.perModel[info.ID] >= info.MaxConcurrent {
		return models.Info{}, limits, refuse(CodeConcurrencyExceeded,
			fmt.Sprintf("model %q is at its concurrency limit of %d on this machine", info.ID, info.MaxConcurrent))
	}
	if ceiling := machineCap(inv); ceiling > 0 && len(m.live) >= ceiling {
		return models.Info{}, limits, refuse(CodeConcurrencyExceeded,
			fmt.Sprintf("this machine is at its model concurrency limit of %d", ceiling))
	}
	return info, limits, nil
}

// machineCap is the machine-wide ceiling, derived from the SAME numbers
// the registration advertises, so what is enforced and what is claimed
// cannot drift apart.
func machineCap(inv models.Inventory) int {
	total := 0
	for _, mi := range inv.Advertised() {
		total += mi.MaxConcurrent
	}
	return total
}

func refuse(code, message string) *memqlv1.ModelCallEnd {
	return &memqlv1.ModelCallEnd{
		FinishReason: FinishError,
		Error:        message,
		ErrorCode:    code,
	}
}

// -----------------------------------------------------------------------------
// Execution
// -----------------------------------------------------------------------------

func (m *Manager) run(ctx context.Context, sender Sender, c *call, info models.Info, limits callLimits, start *memqlv1.ModelCallStart) {
	stream := &deltaStream{sender: sender, requestID: c.requestID}
	stream.touch()

	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go m.watchdog(ctx, c, stream, limits, watchdogDone)

	client := m.clientFor(info)
	var (
		res Result
		err error
	)
	if start.GetKind() == KindEmbedding {
		res, err = client.Embed(ctx, EmbedRequest{Model: info.ID, Input: start.GetEmbeddingInput()})
	} else {
		res, err = client.Chat(ctx, ChatRequest{
			Model:    info.ID,
			Messages: messagesFrom(start.GetMessages()),
			Params:   paramsFrom(start.GetParams()),
			Schema:   start.GetResponseFormatSchema(),
		}, stream.emit)
	}

	end := &memqlv1.ModelCallEnd{RequestId: c.requestID}
	if err != nil {
		finish, code, detail := m.classify(c, err)
		end.FinishReason = finish
		end.ErrorCode = code
		end.Error = detail
	} else {
		end.FinishReason = res.FinishReason
		if end.FinishReason == "" {
			end.FinishReason = FinishStop
		}
	}
	// Usage and embeddings ride even on a failure: a call that produced
	// three hundred tokens and then lost its runtime still SPENT those
	// tokens, and a loop cap that never heard about them is a loop cap
	// that misses exactly the runaway it exists to catch.
	end.Usage = usageProto(res.Usage)
	end.Embeddings = embeddingsProto(res.Embeddings)

	if err := sender.SendModelCallEnd(end); err != nil {
		m.logger.Warn("failed to send model call end", "request_id", c.requestID, "error", err)
	}
}

// watchdog enforces the idle ceiling and emits the keepalives that make
// it enforceable on the other side too.
func (m *Manager) watchdog(ctx context.Context, c *call, stream *deltaStream, limits callLimits, done <-chan struct{}) {
	t := time.NewTicker(limits.keepalive)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			idle := stream.idleFor()
			if idle >= limits.idle {
				// This machine's own runtime has gone quiet past the
				// ceiling. Ending it here frees the GPU; waiting for the
				// engine's copy of the same deadline would not.
				c.mu.Lock()
				if c.reason == "" {
					c.reason, c.code = FinishTimeout, CodeTimeout
				}
				c.mu.Unlock()
				c.cancel()
				return
			}
			if idle >= limits.keepalive {
				// A delta with no content, carrying its own seq so it can
				// never be mistaken for a replayed content delta.
				if err := stream.keepalive(); err != nil {
					c.cancel()
					return
				}
			}
		}
	}
}

// classify turns a transport error into the envelope's closed set. An
// abort reason recorded by Cancel or the watchdog WINS over the context
// error it produced, because "cancelled" and "timed out" are different
// answers to the caller and both present here as context.Canceled.
func (m *Manager) classify(c *call, err error) (finish, code, detail string) {
	c.mu.Lock()
	reason, recorded, supplied := c.reason, c.code, c.detail
	c.mu.Unlock()
	if reason != "" {
		if supplied != "" {
			return reason, recorded, supplied
		}
		switch reason {
		case FinishCancelled:
			return FinishCancelled, recorded, "the cluster cancelled this call"
		case FinishTimeout:
			return FinishTimeout, recorded, "the local runtime stopped producing output past the idle ceiling"
		default:
			return reason, recorded, err.Error()
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FinishTimeout, CodeTimeout, "the call exceeded its timeout"
	}
	if errors.Is(err, context.Canceled) {
		return FinishCancelled, CodeCancelled, "the call was cancelled"
	}
	return FinishError, CodeRuntimeError, err.Error()
}

func (m *Manager) clientFor(info models.Info) client {
	if info.Kind == models.KindOpenAICompatible {
		key := ""
		if info.APIKeyEnv != "" {
			key = m.getenv(info.APIKeyEnv)
		}
		return &openAIClient{baseURL: info.BaseURL, apiKey: key, http: m.http}
	}
	return &ollamaClient{baseURL: info.BaseURL, http: m.http}
}

// -----------------------------------------------------------------------------
// The delta stream
// -----------------------------------------------------------------------------

// deltaStream assigns the monotonic seq and tracks when output last
// moved. Content deltas and keepalives share the counter on purpose: the
// engine's rule is "strictly increasing", and two sources numbering
// independently would collide on the first keepalive.
type deltaStream struct {
	sender    Sender
	requestID string

	mu   sync.Mutex
	seq  uint64
	last time.Time
}

func (s *deltaStream) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

func (s *deltaStream) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.last)
}

func (s *deltaStream) emit(content string) error {
	return s.send(content, false)
}

func (s *deltaStream) keepalive() error {
	return s.send("", true)
}

func (s *deltaStream) send(content string, keepalive bool) error {
	s.mu.Lock()
	seq := s.seq
	s.seq++
	s.last = time.Now()
	s.mu.Unlock()
	return s.sender.SendModelCallDelta(s.requestID, seq, content, keepalive)
}

// -----------------------------------------------------------------------------
// Wire conversions
// -----------------------------------------------------------------------------

func messagesFrom(in []*memqlv1.ModelCallMessage) []Message {
	out := make([]Message, 0, len(in))
	for _, m := range in {
		out = append(out, Message{Role: m.GetRole(), Content: m.GetContent()})
	}
	return out
}

func paramsFrom(p *memqlv1.ModelCallParams) Params {
	if p == nil {
		return Params{}
	}
	return Params{
		Temperature:     p.GetTemperature(),
		TemperatureSet:  p.GetTemperatureSet(),
		TopP:            p.GetTopP(),
		TopPSet:         p.GetTopPSet(),
		MaxOutputTokens: p.GetMaxOutputTokens(),
		Stop:            p.GetStop(),
		Seed:            p.GetSeed(),
		SeedSet:         p.GetSeedSet(),
	}
}

// usageProto returns nil when the runtime reported nothing. Absent is not
// zero: the engine records the first as billing "unknown" and the second
// as a measured zero.
func usageProto(u Usage) *memqlv1.ModelCallUsage {
	if !u.Known && u.Model == "" {
		return nil
	}
	return &memqlv1.ModelCallUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		Known:        u.Known,
		Model:        u.Model,
	}
}

func embeddingsProto(in [][]float32) []*memqlv1.ModelCallEmbedding {
	if len(in) == 0 {
		return nil
	}
	out := make([]*memqlv1.ModelCallEmbedding, 0, len(in))
	for _, v := range in {
		out = append(out, &memqlv1.ModelCallEmbedding{Values: v})
	}
	return out
}
