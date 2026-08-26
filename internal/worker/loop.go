package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/appsession"
	"github.com/znasllc-io/memql-cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// Re-advertising a changed model set (memql-cockpit#361).
//
// The engine accepts Register EXACTLY ONCE, at the handshake -- its
// stream dispatcher has no case for a second one -- and Heartbeat carries
// apps but no labels. So a model pulled, removed, or newly allowed while
// this worker is connected is invisible to the cluster until the worker
// registers again, and registering again means reconnecting.
//
// That makes re-advertisement expensive in a way the app inventory is
// not, and the two constants below are what keep it from being reckless:
// the set is re-checked often enough that `ollama pull` is noticed within
// a minute, and a reconnect is spent only when the ADVERTISED labels
// actually changed, only when no work is in flight, and never twice in
// quick succession -- a runtime flapping up and down must not turn into a
// worker that reconnects forever.
const (
	// modelRefreshInterval is how often the offered set is re-checked.
	modelRefreshInterval = 60 * time.Second
	// modelReadvertiseMinInterval floors the gap between two
	// model-triggered reconnects.
	modelReadvertiseMinInterval = 2 * time.Minute
)

// Runner owns the worker's main loop: reconnect-with-backoff, the
// inbound-message dispatcher, the heartbeat ticker, and the
// in-process tool execution.
type Runner struct {
	logger    *slog.Logger
	cfg       Config
	tools     ToolDispatcher
	apps      AppInventory
	modelsInv ModelInventory
	calls     *modelcall.Manager
	sessions  *appsession.Manager
	heartbeat time.Duration
	metrics   *Metrics

	conn            atomic.Pointer[Connection]
	active          sync.WaitGroup
	activeCalls     atomic.Int64
	lastReadvertise atomic.Int64

	closeOnce sync.Once
	closed    chan struct{}
}

// ToolDispatcher resolves a ToolDispatch to either a Success or a
// Failure. The cockpit's tool implementations satisfy this
// interface.
type ToolDispatcher interface {
	Dispatch(ctx context.Context, dispatch *memqlv1.ToolDispatch) (*memqlv1.Success, *memqlv1.Failure)
}

// Options configures NewRunner.
type Options struct {
	Logger    *slog.Logger
	Config    Config
	Tools     ToolDispatcher
	Apps      AppInventory
	Models    ModelInventory
	Calls     *modelcall.Manager
	Sessions  *appsession.Manager
	Heartbeat time.Duration
	Metrics   *Metrics
}

// NewRunner constructs a Runner. The runner is not yet running; call
// Run to start the connect / dispatch loop.
func NewRunner(opts Options) (*Runner, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	hb := opts.Heartbeat
	if hb <= 0 {
		hb = 15 * time.Second
	}
	return &Runner{
		logger:    opts.Logger,
		cfg:       opts.Config,
		tools:     opts.Tools,
		apps:      opts.Apps,
		modelsInv: opts.Models,
		calls:     opts.Calls,
		sessions:  opts.Sessions,
		heartbeat: hb,
		metrics:   opts.Metrics,
		closed:    make(chan struct{}),
	}, nil
}

// Run blocks until ctx is cancelled or the runner is closed. It
// reconnects with exponential backoff (1s -> 60s, jitter) on every
// disconnect.
func (r *Runner) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("worker.runner: not initialized")
	}
	defer close(r.closed)

	backoff := time.Second
	const maxBackoff = 60 * time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		conn, err := Connect(ctx, r.cfg, r.inventory(ctx), r.modelInventory(ctx), r.logger)
		if err != nil {
			if r.metrics != nil {
				r.metrics.RecordReconnect()
			}
			r.logger.Warn("worker connect failed; will retry",
				"error", err,
				"backoff_seconds", int(backoff.Seconds()),
			)
			if !sleepWithJitter(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		backoff = time.Second
		r.conn.Store(conn)

		streamErr := r.runStream(ctx, conn)
		conn.Close()
		r.conn.Store(nil)

		if streamErr == nil {
			return nil
		}
		if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
			return streamErr
		}
		r.logger.Warn("worker stream ended; will reconnect",
			"error", streamErr,
		)
		if !sleepWithJitter(ctx, backoff) {
			return ctx.Err()
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// Close requests the runner stop on the next loop iteration.
func (r *Runner) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		if conn := r.conn.Load(); conn != nil {
			conn.Close()
		}
	})
	<-r.closed
}

// runStream handles inbound traffic on an active connection.
func (r *Runner) runStream(ctx context.Context, conn *Connection) error {
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go r.heartbeatLoop(hbCtx, conn)

	for {
		msg, err := conn.Recv()
		if err != nil {
			// A disconnect ends every live app session with a named
			// error. The stream is the only channel back to the caller,
			// so a session that outlived it has nowhere to report and
			// nobody waiting -- and the process it is supervising would
			// keep working on somebody's machine with nothing watching
			// it, which is precisely what cancel exists to prevent.
			if r.sessions != nil {
				r.sessions.StopAll("the worker's stream to the cluster was lost")
			}
			// The same argument for model calls, and it is the stronger
			// one: an abandoned generation keeps a GPU busy producing
			// output that has nowhere to go.
			if r.calls != nil {
				r.calls.StopAll("the worker's stream to the cluster was lost")
			}
			r.active.Wait()
			return err
		}
		if err := r.handleMessage(ctx, conn, msg); err != nil {
			r.logger.Warn("worker message handling failed",
				"error", err,
			)
		}
	}
}

func (r *Runner) heartbeatLoop(ctx context.Context, conn *Connection) {
	t := time.NewTicker(r.heartbeat)
	defer t.Stop()
	refresh := time.NewTicker(modelRefreshInterval)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			r.maybeReadvertiseModels(ctx, conn)
		case <-t.C:
			// The inventory is re-taken on every beat rather than
			// captured at connect. The engine applies an inventory
			// change to its live registry immediately and persists it
			// outside the 60s lastSeenAt throttle precisely because it
			// is a routing change -- so signing into Claude Code makes
			// this machine selectable on the NEXT BEAT, not the next
			// reconnect. Sending a snapshot would give that back.
			if err := conn.SendHeartbeat(0, nil, r.inventory(ctx)); err != nil {
				return
			}
		}
	}
}

// modelInventory takes the current local model inventory, or the zero
// value when this build reports none. The zero value advertises nothing,
// which is what a cockpit that cannot serve models should say.
func (r *Runner) modelInventory(ctx context.Context) models.Inventory {
	if r == nil || r.modelsInv == nil {
		return models.Inventory{}
	}
	return r.modelsInv.Models(ctx)
}

// maybeReadvertiseModels ends the stream when what this machine offers
// has changed, so the reconnect re-registers with the new labels.
//
// Three guards, each closing a different failure:
//
//   - Nothing happens unless the ADVERTISED labels differ. Discovery
//     running again is not news; a model appearing is.
//   - Nothing happens while work is in flight. A model finishing its pull
//     must not kill somebody's hour-long app session or a tool call
//     halfway through, and the change will still be there in a minute.
//   - Nothing happens twice inside modelReadvertiseMinInterval. A runtime
//     flapping between up and down would otherwise turn this worker into
//     one that reconnects forever, which is worse than a stale label.
func (r *Runner) maybeReadvertiseModels(ctx context.Context, conn *Connection) {
	if r == nil || r.modelsInv == nil || conn == nil {
		return
	}
	current := advertisedFingerprint(r.modelInventory(ctx).Labels())
	if current == conn.ModelFingerprint {
		return
	}
	if r.busy() {
		r.logger.Debug("model inventory changed; deferring re-advertisement until this worker is idle")
		return
	}
	now := time.Now()
	last := r.lastReadvertise.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < modelReadvertiseMinInterval {
		return
	}
	r.lastReadvertise.Store(now.UnixNano())
	r.logger.Info("local model inventory changed; reconnecting to re-advertise",
		"models_offered", len(r.modelInventory(ctx).Advertised()),
	)
	// Closing the connection surfaces as a Recv error in runStream,
	// which returns and lets Run reconnect. There is no lighter way to
	// re-register: the engine binds labels at the handshake.
	conn.Close()
}

// busy reports whether this worker has work a reconnect would interrupt.
func (r *Runner) busy() bool {
	if r.activeCalls.Load() > 0 {
		return true
	}
	if r.sessions != nil && r.sessions.Live() > 0 {
		return true
	}
	if r.calls != nil && r.calls.Live() > 0 {
		return true
	}
	return false
}

// inventory takes the current local app inventory, or nil when this
// build reports none.
func (r *Runner) inventory(ctx context.Context) []apps.Info {
	if r == nil || r.apps == nil {
		return nil
	}
	return r.apps.Apps(ctx)
}

func (r *Runner) handleMessage(ctx context.Context, conn *Connection, msg *memqlv1.WorkerServerMessage) error {
	if msg == nil {
		return nil
	}
	switch payload := msg.GetPayload().(type) {
	case *memqlv1.WorkerServerMessage_ToolDispatch:
		r.active.Add(1)
		r.activeCalls.Add(1)
		go func() {
			defer r.active.Done()
			defer r.activeCalls.Add(-1)
			r.runToolDispatch(ctx, conn, payload.ToolDispatch)
		}()
	case *memqlv1.WorkerServerMessage_ToolCancel:
		r.logger.Info("worker received tool cancel",
			"call_id", payload.ToolCancel.GetCallId(),
			"reason", payload.ToolCancel.GetReason(),
		)
	case *memqlv1.WorkerServerMessage_AppSessionStart:
		if r.sessions == nil {
			r.logger.Warn("app session start received but this build runs no sessions",
				"session_id", payload.AppSessionStart.GetSessionId())
			break
		}
		r.sessions.Start(ctx, conn, payload.AppSessionStart)
	case *memqlv1.WorkerServerMessage_ModelCallStart:
		if r.calls == nil {
			r.logger.Warn("model call start received but this build serves no models",
				"request_id", payload.ModelCallStart.GetRequestId())
			break
		}
		r.calls.Start(ctx, conn, payload.ModelCallStart)
	case *memqlv1.WorkerServerMessage_ModelCallCancel:
		if r.calls != nil {
			r.calls.Cancel(payload.ModelCallCancel)
		}
	case *memqlv1.WorkerServerMessage_AppSessionControl:
		if r.sessions != nil {
			r.sessions.Control(payload.AppSessionControl)
		}
	case *memqlv1.WorkerServerMessage_Drain:
		r.logger.Info("worker received drain; will exit after in-flight calls finish")
		// App sessions are not tool calls and are not in r.active: a
		// session can run for an hour, and draining is not a reason to
		// abandon one silently. Cancel them by name so each reports its
		// own end before the stream goes.
		if r.sessions != nil {
			r.sessions.StopAll("the cluster asked this worker to drain")
		}
		if r.calls != nil {
			r.calls.StopAll("the cluster asked this worker to drain")
		}
		r.active.Wait()
		return fmt.Errorf("server requested drain")
	case *memqlv1.WorkerServerMessage_RotationResponse:
		r.logger.Info("worker received rotation response (ignored in MVP)")
	}
	return nil
}

func (r *Runner) runToolDispatch(ctx context.Context, conn *Connection, dispatch *memqlv1.ToolDispatch) {
	if dispatch == nil {
		return
	}
	if r.tools == nil {
		_ = conn.SendToolResult(dispatch.GetCallId(), nil, &memqlv1.Failure{
			ErrorCode:    "no_tools_configured",
			ErrorMessage: "worker has no tool dispatcher configured",
		})
		return
	}
	timeout := time.Duration(dispatch.GetTimeout().GetSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	dispatchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	startedAt := time.Now()
	success, failure := r.tools.Dispatch(dispatchCtx, dispatch)
	durationMs := time.Since(startedAt).Milliseconds()

	if r.metrics != nil {
		outcome := "success"
		if failure != nil {
			outcome = failure.GetErrorCode()
			if outcome == "" {
				outcome = "failure"
			}
		}
		r.metrics.RecordCall(outcome, durationMs)
	}

	if err := conn.SendToolResult(dispatch.GetCallId(), success, failure); err != nil {
		r.logger.Warn("worker failed to send tool result",
			"call_id", dispatch.GetCallId(),
			"error", err,
		)
	}
}

func nextBackoff(current, max time.Duration) time.Duration {
	doubled := current * 2
	if doubled > max {
		return max
	}
	return doubled
}

func sleepWithJitter(ctx context.Context, base time.Duration) bool {
	jitter := time.Duration(rand.Int63n(int64(base / 4)))
	t := time.NewTimer(base + jitter)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
