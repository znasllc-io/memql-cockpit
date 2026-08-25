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
)

// Runner owns the worker's main loop: reconnect-with-backoff, the
// inbound-message dispatcher, the heartbeat ticker, and the
// in-process tool execution.
type Runner struct {
	logger    *slog.Logger
	cfg       Config
	tools     ToolDispatcher
	apps      AppInventory
	sessions  *appsession.Manager
	heartbeat time.Duration
	metrics   *Metrics

	conn   atomic.Pointer[Connection]
	active sync.WaitGroup

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

		conn, err := Connect(ctx, r.cfg, r.inventory(ctx), r.logger)
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
	for {
		select {
		case <-ctx.Done():
			return
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
		go func() {
			defer r.active.Done()
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
