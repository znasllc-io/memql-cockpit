package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/appsession"
	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
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
	// model-triggered reconnects. RequestImmediateReadvertise is its one
	// exception, and the only one: see that method for why a pull
	// somebody is watching does not wait it out.
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
	pulls     *modelPuller
	heartbeat time.Duration
	serve     func() string
	metrics   *Metrics

	conn            atomic.Pointer[Connection]
	active          sync.WaitGroup
	activeCalls     atomic.Int64
	lastReadvertise atomic.Int64

	// pendingReadvertise is the one-shot floor bypass armed by
	// RequestImmediateReadvertise, and readvertiseNow is how that
	// request wakes the heartbeat loop instead of waiting up to
	// modelRefreshInterval for the next tick. Buffered with room for
	// ONE: a burst of pulls is one evaluation, not one reconnect each.
	pendingReadvertise atomic.Bool
	readvertiseNow     chan struct{}

	// now is the clock the re-advertise guards read. Injectable because
	// the floor is two minutes of WALL clock, and a test that waited it
	// out would spend two minutes of every CI run.
	now func() time.Time

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
	// InferenceServe reads this machine's sharing consent from the live
	// policy (memql-cockpit#399). A FUNCTION rather than a value,
	// because the value is re-read on SIGHUP and a snapshot taken at
	// startup would advertise a consent the file no longer states --
	// and this is the one setting whose stale value hands somebody
	// else's prompt to this machine's GPU.
	//
	// Nil reports tools.ServeOwner, which is the fail-closed default a
	// build that does not wire this should send.
	InferenceServe func() string
	// ModelPull wires the cluster-driven pull (engine epic memql#5103;
	// the install wizard's D13). Nil, or a Models of nil, means this
	// build pulls nothing and answers every ModelPullStart with ok=false
	// and a sentence -- a pull onto a machine that advertises nothing
	// would fetch gigabytes the cluster is never told about.
	ModelPull *ModelPullOptions
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
		hb = DefaultHeartbeat
	}
	r := &Runner{
		logger:    opts.Logger,
		cfg:       opts.Config,
		tools:     opts.Tools,
		apps:      opts.Apps,
		modelsInv: opts.Models,
		calls:     opts.Calls,
		sessions:  opts.Sessions,
		heartbeat: hb,
		metrics:   opts.Metrics,
		serve:     opts.InferenceServe,
		closed:    make(chan struct{}),
		// Room for one. A nil channel would be safe (both the send and
		// the receive sit in a select), but it would make every request
		// wait for the refresh ticker, which is the wait this exists to
		// remove.
		readvertiseNow: make(chan struct{}, 1),
	}
	pull := opts.ModelPull
	if opts.Models == nil {
		pull = nil
	}
	r.pulls = newModelPuller(opts.Logger, pull, r.RequestImmediateReadvertise)
	return r, nil
}

// Run blocks until ctx is cancelled or the runner is closed. It
// reconnects with exponential backoff (1s -> 15s, jitter) on every
// disconnect. The ceiling is deliberately short: a multi-home fleet must
// regain availability aggressively after a real network blip; a 60s wait
// left prod machines looking "gone" long after the laptop was awake.
// RegistrationId returns this machine's v1:worker:registration id, or "" when
// no stream is currently up.
//
// A GETTER rather than a field handed out once, because the id is only known
// after a RegisterAck and the runner may still be backing off through a
// reconnect. Callers that need it (the backup sweeper) re-ask each time and
// skip the pass when it is empty, rather than capturing an empty string at
// startup and never noticing it filled in.
func (r *Runner) RegistrationId() string {
	if r == nil {
		return ""
	}
	if conn := r.conn.Load(); conn != nil {
		return conn.RegistrationId
	}
	return ""
}

func (r *Runner) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("worker.runner: not initialized")
	}
	defer close(r.closed)

	backoff := time.Second
	const maxBackoff = DefaultReconnectMaxBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		conn, err := Connect(ctx, r.cfg, r.inventory(ctx), r.modelInventory(ctx), r.inferenceServe(), r.logger)
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
		code, reason := streamEndCodeReason(streamErr)
		r.logger.Warn("worker stream ended; will reconnect",
			"error", streamErr,
			"code", code,
			"reason", reason,
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
			// And for pulls, with one difference: the engine has already
			// given up every pull on this stream (worker_disconnected),
			// so the download is stopped rather than finished into a
			// void. The blobs already fetched stay on disk for the next
			// pull of the same model to resume.
			r.pulls.StopAll("the worker's stream to the cluster was lost")
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

// DefaultHeartbeat is the beat interval when the caller states none.
//
// A named constant rather than a literal because the hardware refresh
// cadence is stated in BEATS and its wall-clock interval follows from
// this number -- so a test that pinned the interval against its own
// copy of 15s would stay green while this moved, and the refresh would
// silently become five minutes.
const DefaultHeartbeat = 15 * time.Second

// DefaultReconnectMaxBackoff caps stream reconnect delay. Kept at one
// heartbeat interval so a home that was healthy recovers inside the
// cluster OnlineWindow rather than sitting dark for a minute.
const DefaultReconnectMaxBackoff = 15 * time.Second

// hardwareRefreshBeats is how often the hardware inventory is re-scanned
// onto the heartbeat (design record D1: "refreshed on every tenth
// heartbeat"). At the 15-second default that is a refresh every two and
// a half minutes, so a pulled model or an installed runtime shows within
// minutes -- the record's own words -- without shelling out to
// nvidia-smi and `docker version` four times a minute.
const hardwareRefreshBeats = 10

// hardwareOnBeat reports whether this beat carries the inventory.
//
// Register carries the first one, so beat 10 is the first REFRESH
// rather than the first report -- a machine that reported on beat 1 as
// well would send the same payload twice within fifteen seconds of
// connecting.
func hardwareOnBeat(beat int) bool { return beat > 0 && beat%hardwareRefreshBeats == 0 }

func (r *Runner) heartbeatLoop(ctx context.Context, conn *Connection) {
	t := time.NewTicker(r.heartbeat)
	defer t.Stop()
	refresh := time.NewTicker(modelRefreshInterval)
	defer refresh.Stop()
	beat := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			r.maybeReadvertiseModels(ctx, conn)
		case <-r.readvertiseNow:
			// A pull just finished with somebody watching. Evaluating
			// here rather than at the next refresh tick is the
			// difference between a model that appears now and one that
			// appears in up to a minute -- and a minute of nothing
			// happening reads as a pull that failed.
			r.maybeReadvertiseModels(ctx, conn)
		case <-t.C:
			// The inventory is re-taken on every beat rather than
			// captured at connect. The engine applies an inventory
			// change to its live registry immediately and persists it
			// outside the 60s lastSeenAt throttle precisely because it
			// is a routing change -- so signing into Claude Code makes
			// this machine selectable on the NEXT BEAT, not the next
			// reconnect. Sending a snapshot would give that back.
			beat++
			// The hardware inventory rides every tenth beat and nothing
			// in between. It is scanned HERE rather than cached on the
			// Runner because a scan whose result is held across beats
			// would report a runtime that has since been uninstalled --
			// and the whole reason for the refresh is that a machine
			// changes under the worker.
			var hw *hardware.Inventory
			if hardwareOnBeat(beat) {
				inv := hardware.Local(ctx)
				hw = &inv
			}
			if err := conn.SendHeartbeat(0, nil, r.inventory(ctx), hw); err != nil {
				return
			}
		}
	}
}

// inferenceServe reads the live sharing consent, defaulting closed.
func (r *Runner) inferenceServe() string {
	if r.serve == nil {
		return tools.ServeOwner
	}
	if v := r.serve(); v == tools.ServeCluster {
		return tools.ServeCluster
	}
	return tools.ServeOwner
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
// has changed, so the reconnect re-registers with the new labels. It
// reports whether it did.
//
// Three guards, each closing a different failure:
//
//   - Nothing happens unless the ADVERTISED labels differ. Discovery
//     running again is not news; a model appearing is.
//   - Nothing happens while work is in flight. A model finishing its pull
//     must not kill somebody's hour-long app session, a tool call halfway
//     through, or a sibling pull still downloading -- and the change will
//     still be there in a minute.
//   - Nothing happens twice inside modelReadvertiseMinInterval. A runtime
//     flapping between up and down would otherwise turn this worker into
//     one that reconnects forever, which is worse than a stale label.
//
// The THIRD guard, and only the third, has an exception: a one-shot
// request from RequestImmediateReadvertise. It is spent by the reconnect
// it asks for and by nothing else. An early return leaves it armed --
// busy, or labels that have not changed YET because the policy reload
// naming the new model landed a moment after the request -- so the next
// evaluation still gets the bypass, rather than the machine that was
// mid-call being the one that waits out the floor.
func (r *Runner) maybeReadvertiseModels(ctx context.Context, conn *Connection) bool {
	if r == nil || r.modelsInv == nil || conn == nil {
		return false
	}
	current := advertisedFingerprint(r.modelInventory(ctx).Labels())
	if current == conn.ModelFingerprint {
		return false
	}
	if r.busy() {
		r.logger.Debug("model inventory changed; deferring re-advertisement until this worker is idle")
		return false
	}
	now := r.clock()
	last := r.lastReadvertise.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < modelReadvertiseMinInterval {
		// Swap rather than Load: the bypass must be consumed by the one
		// reconnect it paid for. Left armed, it would sit there and let
		// a flapping runtime through the floor at some unrelated later
		// moment.
		if !r.pendingReadvertise.Swap(false) {
			return false
		}
		r.logger.Info("a watched model pull changed this machine's model set; re-advertising without waiting out the floor")
	} else {
		r.pendingReadvertise.Store(false)
	}
	r.lastReadvertise.Store(now.UnixNano())
	r.logger.Info("local model inventory changed; reconnecting to re-advertise",
		"models_offered", len(r.modelInventory(ctx).Advertised()),
	)
	// Closing the connection surfaces as a Recv error in runStream,
	// which returns and lets Run reconnect. There is no lighter way to
	// re-register: the engine binds labels at the handshake.
	conn.Close()
	return true
}

// RequestImmediateReadvertise says the local model set just changed and
// SOMEBODY IS WATCHING -- the caller pulled a model, on purpose, in
// front of a person who pressed a button.
//
// Two things happen, and they are one call because a caller that did
// only the first would ship the bug this method exists to prevent:
//
//  1. The cached inventory is dropped. Discovery is cached for
//     DefaultModelInventoryTTL, and a model pulled a moment ago is not in
//     a probe taken a minute before it. Re-advertising without this
//     spends a reconnect to re-register the labels from BEFORE the pull:
//     the reconnect happens, nothing changes, and the pull reads to the
//     person watching as one that did nothing.
//  2. The two-minute floor is waived for the next evaluation, once. Two
//     minutes of a model that does not appear is indistinguishable from
//     a failed pull, and the person is looking at the screen.
//
// The BUSY guard is NOT waived, deliberately. A reconnect that kills a
// running model call or an hour-old app session is worse than a label
// that is a minute stale, and somebody watching a pull is not a reason
// to throw away somebody else's work. The request survives that wait.
//
// WHO CALLS IT, and from where. Only something inside THIS process can,
// and the cockpit's own pull path runs in another one: `memql worker
// models --pull` and `memql worker setup --inference` reach the running
// worker through the SIGHUP they already send after writing
// models.allow, and the handler in cli.go asks for this after reloading
// the policy -- a reloaded allow list that nobody re-advertised is a
// model the cluster still cannot see. The cluster's own ModelPullStart
// arm (modelpull.go) calls it directly, after reloading the policy
// itself and AFTER sending its End: the reconnect this asks for closes
// the stream that End rides.
func (r *Runner) RequestImmediateReadvertise() {
	if r == nil {
		return
	}
	r.pendingReadvertise.Store(true)
	if r.modelsInv != nil {
		r.modelsInv.Invalidate()
	}
	select {
	case r.readvertiseNow <- struct{}{}:
	default:
		// Already woken and not yet evaluated, or no loop running (the
		// worker is reconnecting). Either way the arm above is what
		// carries the request; a second token would only buy a second
		// evaluation of the same answer.
	}
}

// clock reads the injectable time source, defaulting to time.Now for
// every runner but the ones tests build.
func (r *Runner) clock() time.Time {
	if r != nil && r.now != nil {
		return r.now()
	}
	return time.Now()
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
	// A pull holds this guard until AFTER its End is sent, so the
	// re-advertise one model earns cannot close the stream under its
	// sibling's download -- the recommended set is a pair, asked for at
	// once -- and cannot lose the End that reports its own success.
	if r.pulls.Live() > 0 {
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
	case *memqlv1.WorkerServerMessage_Ping:
		r.answerPing(conn, payload.Ping)
	case *memqlv1.WorkerServerMessage_ModelPullStart:
		// Nil-safe: a runner built without the seam refuses in a
		// sentence rather than dropping the request.
		r.pulls.Start(ctx, conn, payload.ModelPullStart)
	case *memqlv1.WorkerServerMessage_ModelPullCancel:
		r.pulls.Cancel(payload.ModelPullCancel)
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
		r.pulls.StopAll("the cluster asked this worker to drain")
		r.active.Wait()
		return errServerDrain
	case *memqlv1.WorkerServerMessage_RotationResponse:
		r.logger.Info("worker received rotation response (ignored in MVP)")
	}
	return nil
}

// answerPing is the Pong (epic memql#5218, D11): the cluster's evidence
// that the return path to this machine works, and how fast. It is
// answered at once, on the recv goroutine, because anything queued behind
// a tool dispatch would measure the queue rather than the path.
//
// DEBUG, NEVER INFO. The cluster pings every machine once a minute for as
// long as it is connected; an info line for each is a log that says
// nothing forever.
func (r *Runner) answerPing(conn *Connection, ping *memqlv1.Ping) {
	if ping == nil {
		return
	}
	if err := conn.SendPong(ping.GetRequestId(), ping.GetSentAt()); err != nil {
		r.logger.Debug("pong not sent", "request_id", ping.GetRequestId(), "error", err)
		return
	}
	r.logger.Debug("answered the cluster's ping", "request_id", ping.GetRequestId())
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
