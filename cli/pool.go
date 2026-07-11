package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql-cockpit/cli/auth"
	"github.com/znasllc-io/memql-cockpit/cli/cluster"
	"github.com/znasllc-io/memql-cockpit/cli/config"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// entryState is the single source of truth for one pool entry's
// connection lifecycle. The state drives the cluster-list row icon,
// the detail-pane retry display, and whether the Explorer/Agents
// tabs are usable (gated on the selected cluster being Connected).
//
// Transitions:
//
//	Idle -> Connecting (attempt 1)
//	Connecting -> Connected (dial succeeded)
//	Connecting -> Backoff (attempt failed, more remaining)
//	Backoff -> Connecting (next attempt)
//	Connecting -> Failed (attempt 3 failed) or Backoff -> Failed (cancel)
//	Connected -> Connecting (stream Unexpected close, fresh 3-attempt cycle)
//	Failed -> Connecting (manual Retry from user Enter)
type entryState int

const (
	stateIdle        entryState = iota // brand-new entry, goroutine hasn't started
	stateConnecting                    // dial in flight
	stateConnected                     // live stream, subscriber + monitor running
	stateBackoff                       // waiting between failed attempts
	stateFailed                        // all retries exhausted, awaiting manual retry
	stateNeedsConfig                   // missing endpoint/auth; no dial attempted, waiting for L:Authorize
	stateNeedsToken                    // fully configured but no cached token; waiting for L:Login
)

func (s entryState) String() string {
	switch s {
	case stateIdle:
		return "idle"
	case stateConnecting:
		return "connecting"
	case stateConnected:
		return "connected"
	case stateBackoff:
		return "backoff"
	case stateFailed:
		return "failed"
	case stateNeedsConfig:
		return "needs-auth"
	case stateNeedsToken:
		return "needs-login"
	default:
		return "unknown"
	}
}

// maxAttempts bounds an automatic (re)connect cycle. After the third
// consecutive failure the entry goes to stateFailed and waits for a
// user-driven Retry.
const maxAttempts = 3

// backoffFor returns the wait time before the next retry. The schedule
// is a linear +15s step (15s / 30s / 45s), totaling ~90s before the
// entry hits stateFailed. The interval is generous enough to ride
// out short network blips without overwhelming a flaky cluster with
// tight retries.
func backoffFor(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 15 * time.Second
	case 2:
		return 30 * time.Second
	default:
		return 45 * time.Second
	}
}

// connEntry owns everything tied to one cluster's live connection:
// the gRPC Connection, a SubscriptionManager subscribed to
// v1:cluster:node events, the last-known topology snapshot for that
// cluster, and the goroutines (subscriber + lifecycle) that keep them
// in sync.
type connEntry struct {
	app *App

	Config config.ClusterConfig

	mu        sync.Mutex
	Conn      *client.Connection
	SubMgr    *client.SubscriptionManager
	SubId     string
	Events    <-chan client.Event
	Nodes     []cluster.NodeInfo
	NodeTypes []cluster.NodeTypeInfo // seeded order from queryClusterNodeTypes; drives topology row layout
	State     entryState
	Attempt   int       // current attempt number within the 1..maxAttempts cycle
	NextTryAt time.Time // when the next attempt will fire (meaningful only in stateBackoff)

	// cancelCh interrupts a backoff wait or aborts an in-flight dial.
	// Recreated by Retry() so closed-channel sends don't panic.
	cancelCh chan struct{}

	// done is closed exactly once by Close(). Signals lifecycle /
	// subscriber / monitor goroutines to exit permanently.
	done chan struct{}
	// Lifecycle completion markers. Close() waits briefly on these
	// so callers know the entry is quiescent.
	lifecycleExited  chan struct{}
	subscriberExited chan struct{}

	// lifecycleStarted is true once runLifecycle has been launched
	// (via openEntry's markLifecycleStarted). Registered-but-not-
	// dialed entries (registerEntry, epic #239) never start it, so
	// Close must not block waiting on lifecycleExited for them.
	lifecycleStarted bool

	// probe is the last liveness-probe result for a registered-idle
	// (non-selected, non-dialed) cluster. It is DISTINCT from State:
	// the selected cluster's reachability is its live-connection
	// State, while every other configured cluster gets a lightweight
	// up/down probe instead of a full connection (epic #239, CM3
	// #242). Only meaningful while lifecycleStarted is false.
	probe probeStatus

	closed bool
}

// probeStatus is the reachability of a non-selected cluster as seen by
// the lightweight liveness probe -- separate from the full-connection
// entryState machine.
type probeStatus int

const (
	probeNone        probeStatus = iota // not probed yet
	probeReachable                      // server answered (or refused creds -- still up)
	probeUnreachable                    // dial/TLS/timeout failure -- down
)

// markLifecycleStarted records that runLifecycle was launched for
// this entry, so Close knows to wait for lifecycleExited. Entries
// that are only registered (registerEntry) never call this, and
// Close skips the wait for them.
func (e *connEntry) markLifecycleStarted() {
	e.mu.Lock()
	e.lifecycleStarted = true
	e.mu.Unlock()
}

// probeReachableEligible reports whether this entry should be liveness-
// probed: it must be a registered-idle entry (no running lifecycle,
// stateIdle) -- the selected cluster has a real connection, and
// needs-config / needs-login rows have nothing to probe (state is not
// idle for those). Caller additionally excludes the selected cluster.
func (e *connEntry) probeReachableEligible() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.lifecycleStarted && !e.closed && e.State == stateIdle
}

// setProbe records the probe result under lock and reports whether it
// changed (so the caller only redraws on a real transition -- no
// flicker from steady-state re-probes).
func (e *connEntry) setProbe(s probeStatus) (changed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.probe == s {
		return false
	}
	e.probe = s
	return true
}

// probeStatusSnapshot returns the entry's last probe result under lock.
func (e *connEntry) probeStatusSnapshot() probeStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.probe
}

// probeReachability performs ONE lightweight liveness probe: a short-
// timeout connect-and-close with no token, no subscribe, no initial
// loads, no refresher. The handshake completes without credentials,
// and an auth rejection still proves the server is reachable -- so
// only a transport-level failure (dial refused, TLS, timeout) counts
// as down. Returns probeReachable / probeUnreachable.
func probeReachability(ctx context.Context, cfg config.ClusterConfig) probeStatus {
	// Logger intentionally nil: this fires every probe interval per
	// cluster, so it must not spew "cockpit dialing cluster" lines.
	conn, err := client.Connect(ctx, client.ConnectConfig{Endpoint: cfg.Endpoint})
	if err == nil {
		conn.Close()
		return probeReachable
	}
	// A credentials rejection means the server answered -- it's up, we
	// just didn't present a token (we deliberately don't, to keep the
	// probe free of the auth machinery).
	if looksLikeAuthRejection(err) {
		return probeReachable
	}
	return probeUnreachable
}

// newConnEntry builds the entry metadata. Start the lifecycle
// goroutine (runLifecycle) via openEntry in app.go.
func newConnEntry(app *App, cfg config.ClusterConfig) *connEntry {
	return &connEntry{
		app:              app,
		Config:           cfg,
		State:            stateIdle,
		cancelCh:         make(chan struct{}),
		done:             make(chan struct{}),
		lifecycleExited:  make(chan struct{}),
		subscriberExited: nil, // allocated each time runSubscriber starts
	}
}

// snapshotNodes returns a copy of the entry's Nodes under lock, so
// callers can render/mutate without racing the subscriber.
func (e *connEntry) snapshotNodes() []cluster.NodeInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]cluster.NodeInfo, len(e.Nodes))
	copy(out, e.Nodes)
	return out
}

// snapshotNodeTypes returns a copy of the entry's cached NodeTypes
// (loaded once on connect via queryClusterNodeTypes). The topology
// view uses this to drive row order.
func (e *connEntry) snapshotNodeTypes() []cluster.NodeTypeInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]cluster.NodeTypeInfo, len(e.NodeTypes))
	copy(out, e.NodeTypes)
	return out
}

// stateSnapshot returns a consistent read of the fields the UI needs
// to render the cluster row and detail pane without holding the lock.
func (e *connEntry) stateSnapshot() (state entryState, attempt int, nextTryAt time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.State, e.Attempt, e.NextTryAt
}

// runLifecycle owns the connection-attempt state machine. It's
// launched once per entry by openEntry and runs until Close() is
// invoked or the stream is permanently failed and the user doesn't
// retry.
func (e *connEntry) runLifecycle() {
	defer close(e.lifecycleExited)

	for {
		// 3-attempt cycle. Each attempt either succeeds (we transition
		// to Connected and spawn the subscriber + wait for a drop) or
		// fails (we back off and try again, or exhaust retries).
		success := e.attemptConnectCycle()
		if !success {
			// Either we hit stateFailed (exhausted attempts) or the
			// entry was cancelled / closed. Either way, wait here
			// until either Retry() kicks us back into the cycle, or
			// the app / entry shuts down.
			select {
			case <-e.cancelWatch():
				// Retry() closed the old cancelCh to signal "go again".
				// The new cancelCh is already swapped in, the attempt
				// counter reset, and State is stateConnecting. Loop.
				continue
			case <-e.done:
				return
			case <-e.app.quitCh:
				return
			}
		}

		// success -> stateConnected. The subscriber goroutine is
		// running; we now watch for an Unexpected close. When it
		// fires we drop to the top of the loop for a fresh 3-attempt
		// reconnect cycle.
		if !e.watchConnection() {
			return
		}
	}
}

// refresherTickFloor is the minimum sleep between consecutive refresh
// attempts. Floor exists so a bug that mints a too-short TTL (or a
// clock-skew window where Expiry has already passed by the time we
// wake up) can't spin the refresher into a tight loop.
const refresherTickFloor = 30 * time.Second

// refresherLeadTime is how far ahead of Expiry the refresher fires.
// Picked > expiryBuffer (cli/config/credentials.go) so a freshly-
// rolled token always lands BEFORE IsExpired flips true -- otherwise
// any dial that interleaves with the refresher would see the cached
// token as already-expired and race the refresher instead of using
// it.
const refresherLeadTime = 90 * time.Second

// runTokenRefresher silently rolls the cached OIDC token forward
// before it expires, so a reconnect never has to fall through to the
// browser flow. The contract:
//
//   - PAT clusters and clusters with no cached token / no refresh
//     token exit immediately. The refresher only owns the
//     OAuth-with-refresh case.
//   - Wakes ~refresherLeadTime before the cached Expiry. Calls
//     auth.Refresh; on success saves the rolled pair and loops.
//   - On invalid_grant: stops. The next dial will discover the
//     dead refresh token and prompt the user. Spawning a browser
//     from the background would surprise the user mid-task.
//   - On transient error: backs off refresherTickFloor and retries.
//   - On e.done: exits cleanly.
//
// Note: the live gRPC stream's bearer was set at stream-open time
// and is not rotated by this refresher. The point of the refresher
// is keeping the ON-DISK credential fresh so that ANY reconnect
// (stream drop, sleep/wake, server restart) finds a usable token
// without a browser round-trip. Rotating the in-flight bearer would
// require a re-handshake message the server doesn't yet support.
func (e *connEntry) runTokenRefresher() {
	if e.Config.PAT != "" {
		// PATs don't expire client-side. Server-side revocation is the
		// only kill switch; nothing for the refresher to do.
		return
	}
	if e.Config.Issuer == "" || e.Config.ClientId == "" {
		// stateNeedsConfig path. No identity to refresh against.
		return
	}
	for {
		stored, err := config.LoadToken(e.Config.Name)
		if err != nil || stored == nil || stored.RefreshToken == "" {
			// Nothing to refresh yet. Wait for the foreground Login to
			// drop a token on disk, then pick up on the next tick.
			if e.sleepRefresher(refresherTickFloor) {
				return
			}
			continue
		}

		// How long until we should fire? "Now - lead" can be negative
		// when the cached token is already past its lead window; floor
		// to refresherTickFloor so we don't hammer identity in a tight
		// loop if the server hands us a too-short TTL.
		wait := time.Until(stored.Expiry) - refresherLeadTime
		if wait < refresherTickFloor {
			wait = refresherTickFloor
		}
		if e.sleepRefresher(wait) {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		result, err := auth.Refresh(ctx, e.Config.Issuer, stored.RefreshToken)
		cancel()
		if err != nil {
			if errors.Is(err, auth.ErrInvalidGrant) {
				// Refresh token is gone server-side. Don't pop a
				// browser; the next dial owns that decision. The
				// stream we're currently on (if any) keeps working
				// until it eventually drops -- at which point the
				// pool's reconnect cycle will hit EnsureValidToken
				// and surface the re-login affordance.
				if e.app.logger != nil {
					e.app.logger.Info("auth refresher: refresh token rejected, stopping background refresher",
						"cluster", e.Config.Name, "error", err)
				}
				return
			}
			// Transient: log + retry on next tick.
			if e.app.logger != nil {
				e.app.logger.Warn("auth refresher: transient refresh failure",
					"cluster", e.Config.Name, "error", err)
			}
			if e.sleepRefresher(refresherTickFloor) {
				return
			}
			continue
		}

		if err := config.SaveToken(e.Config.Name, &config.StoredToken{
			AccessToken:  result.AccessToken,
			RefreshToken: result.RefreshToken,
			Expiry:       result.Expiry,
		}); err != nil {
			if e.app.logger != nil {
				e.app.logger.Warn("auth refresher: rolled token but cache write failed",
					"cluster", e.Config.Name, "error", err)
			}
			// Keep looping -- the in-memory result is gone but the
			// next iteration will reload + retry.
		} else if e.app.logger != nil {
			e.app.logger.Info("auth refresher: rolled access token",
				"cluster", e.Config.Name,
				"expiresIn", time.Until(result.Expiry).Round(time.Second))
		}

		// In-stream rotation: ask the server to swap the bearer on the
		// live stream so admin-side revocation / role changes
		// propagate to the in-flight session in O(seconds) rather
		// than waiting for the next stream drop. The stream's gRPC
		// metadata is fixed at open time, so without RotateAuth the
		// server keeps verifying envelopes against the original
		// token until reconnect. With RotateAuth the server
		// re-verifies the new token and swaps
		// streamSession.identity + .access.
		//
		// Failure is non-fatal: the on-disk credential is fresh, so
		// any subsequent reconnect still lands on the rotated pair.
		// A rejected rotate (ErrRotateAuthRejected) typically means
		// the server's verifier doesn't know about the new token yet
		// (JWKS-refresh lag).
		e.mu.Lock()
		conn := e.Conn
		e.mu.Unlock()
		if conn != nil {
			rotateCtx, rotateCancel := context.WithTimeout(context.Background(), 10*time.Second)
			rotateErr := conn.Dispatcher().RotateAuth(rotateCtx, result.AccessToken)
			rotateCancel()
			if rotateErr != nil && e.app.logger != nil {
				switch {
				case errors.Is(rotateErr, client.ErrRotateAuthRejected):
					e.app.logger.Warn("auth refresher: in-stream rotate rejected; on-disk token still rolled",
						"cluster", e.Config.Name, "error", rotateErr)
				default:
					e.app.logger.Warn("auth refresher: in-stream rotate transport failure; on-disk token still rolled",
						"cluster", e.Config.Name, "error", rotateErr)
				}
			} else if e.app.logger != nil {
				e.app.logger.Info("auth refresher: in-stream bearer rotated",
					"cluster", e.Config.Name)
			}
		}
	}
}

// sleepRefresher blocks for d or until the entry shuts down. Returns
// true when the caller should exit (e.done fired).
func (e *connEntry) sleepRefresher(d time.Duration) bool {
	if d <= 0 {
		d = refresherTickFloor
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-e.done:
		return true
	case <-e.app.quitCh:
		return true
	}
}

// cancelWatch returns the current cancelCh under lock. Needed because
// Retry() swaps the channel out.
func (e *connEntry) cancelWatch() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelCh
}

// attemptConnectCycle runs up to maxAttempts dials with backoff
// between them. Returns true if one attempt succeeded (state is
// stateConnected and subscriber is running). Returns false if all
// attempts failed (state = stateFailed) or the entry was cancelled /
// closed mid-cycle.
func (e *connEntry) attemptConnectCycle() bool {
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		e.setStateAttempt(stateConnecting, attempt, time.Time{})
		e.app.postRedraw()

		ctx, cancelDial := context.WithTimeout(context.Background(), 30*time.Second)
		err := e.dialOnce(ctx)
		cancelDial()

		// If the server rejected our credentials (stale token from
		// before the cluster was wiped, server rotated keys, etc.),
		// stop retrying and prompt the user to re-authenticate. The
		// alternative is 90s of silent "Connecting to..." that ends
		// in stateFailed without telling the user the problem is
		// fixable with a fresh login.
		if err != nil && looksLikeAuthRejection(err) {
			_ = config.DeleteToken(e.Config.Name)
			e.setStateAttempt(stateNeedsToken, attempt, time.Time{})
			e.app.postRedraw()
			if e.app.logger != nil {
				e.app.logger.Info("cached token rejected; transitioning to stateNeedsToken",
					"cluster", e.Config.Name,
					"error", err,
				)
			}
			return false
		}

		if err == nil {
			// Connected. Start the cluster-node subscriber; it exits
			// when its Events channel closes (normal shutdown or
			// stream drop).
			e.mu.Lock()
			e.State = stateConnected
			e.subscriberExited = make(chan struct{})
			e.mu.Unlock()
			go e.runSubscriber()
			e.app.onEntryConnected(e)
			e.app.postRedraw()
			return true
		}

		if e.app.logger != nil {
			e.app.logger.Warn("dial failed",
				"cluster", e.Config.Name,
				"attempt", attempt,
				"error", err,
			)
		}

		// Check for cancel / shutdown before deciding whether to back
		// off for another attempt.
		if !e.stillAlive() {
			e.setStateAttempt(stateFailed, attempt, time.Time{})
			e.app.postRedraw()
			return false
		}

		if attempt == maxAttempts {
			e.setStateAttempt(stateFailed, attempt, time.Time{})
			e.app.postRedraw()
			return false
		}

		// Back off before the next attempt.
		wait := backoffFor(attempt)
		e.setStateAttempt(stateBackoff, attempt, time.Now().Add(wait))
		e.app.postRedraw()
		if !e.sleepCancellable(wait) {
			e.setStateAttempt(stateFailed, attempt, time.Time{})
			e.app.postRedraw()
			return false
		}
	}
	return false
}

// looksLikeAuthRejection returns true when err looks like the server
// refused our credentials (gRPC Unauthenticated / PermissionDenied
// codes, or the auth interceptor's well-known reject strings). Used
// to short-circuit a stale-token retry loop and prompt the user to
// re-login instead of silently spinning on "Connecting to...".
//
// String matching is a fallback for transport / handshake errors
// that don't propagate the gRPC status code cleanly.
func looksLikeAuthRejection(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unauthenticated, codes.PermissionDenied:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"unauthenticated",
		"permission denied",
		"invalid token",
		"token expired",
		"authentication required",
		"unauthorized",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// watchConnection blocks until the current Conn's stream errors
// unexpectedly, or the entry is closed / app shuts down. Returns
// false on terminal shutdown (caller should exit runLifecycle),
// true if we should loop back for reconnect.
func (e *connEntry) watchConnection() bool {
	e.mu.Lock()
	conn := e.Conn
	e.mu.Unlock()
	if conn == nil {
		return false
	}

	select {
	case <-conn.Dispatcher().Unexpected():
		// Stream dropped. Clear conn; outer loop will attempt a
		// fresh 3-attempt reconnect cycle.
		e.mu.Lock()
		e.Conn = nil
		e.SubMgr = nil
		e.Events = nil
		e.mu.Unlock()
		e.app.onEntryDisconnected(e)
		return true
	case <-e.done:
		return false
	case <-e.app.quitCh:
		return false
	}
}

// dialOnce performs a single connect + subscribe + initial-load. On
// success the entry's Conn/SubMgr/Events/Nodes are populated.
func (e *connEntry) dialOnce(ctx context.Context) error {
	// EnsureValidTokenWithLogger so refresh-path activity ("refreshing
	// access token", "refresh token rejected") lands in the same log
	// stream as the surrounding dial events. Without the logger the
	// silent refresh would be impossible to observe when something
	// goes wrong.
	token, err := auth.EnsureValidTokenWithLogger(ctx, e.Config, e.app.logger)
	if err != nil && e.app.logger != nil {
		e.app.logger.Warn("auth failed, connecting without token", "cluster", e.Config.Name, "error", err)
	}

	conn, err := client.Connect(ctx, client.ConnectConfig{
		Endpoint: e.Config.Endpoint,
		Token:    token,
		Logger:   e.app.logger,
	})
	if err != nil {
		return err
	}

	sm := client.NewSubscriptionManager(conn.Dispatcher())
	subCtx := context.Background()
	// Graph subscriptions are structured (concept + actions) since
	// memql#2460 -- the free-text Subscribe(GraphEvents, "node.created.
	// v1:cluster:node") form is now rejected server-side, which used to
	// surface as a non-auth dial error that spun the connect loop
	// forever ("Connecting..." / retrying). Subscribe to node.created on
	// v1:cluster:node via the structured API.
	subId, events, err := sm.SubscribeGraph(subCtx, client.GraphSubscribeOptions{
		Concept: "v1:cluster:node",
		Actions: []client.GraphAction{client.GraphActionCreated},
	})
	if err != nil {
		conn.Close()
		return err
	}

	e.mu.Lock()
	e.Conn = conn
	e.SubMgr = sm
	e.SubId = subId
	e.Events = events
	e.mu.Unlock()

	nodes := e.initialLoad(ctx)
	types := e.nodeTypesInitialLoad(ctx)
	e.mu.Lock()
	e.Nodes = nodes
	e.NodeTypes = types
	e.mu.Unlock()

	return nil
}

// nodeTypesInitialLoad pulls queryClusterNodeTypes({}) once on connect
// and parses the seed rows into NodeTypeInfo. The topology view uses
// the resulting order to draw one row per type. A failed/empty
// response is non-fatal -- the view falls back to the types present
// on registered nodes so the diagram still renders.
func (e *connEntry) nodeTypesInitialLoad(ctx context.Context) []cluster.NodeTypeInfo {
	e.mu.Lock()
	conn := e.Conn
	e.mu.Unlock()
	if conn == nil {
		return nil
	}
	queries := client.NewQueryClient(conn.Dispatcher())
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := queries.ClusterNodeTypes(ctx, client.ClusterNodeTypesArgs{})
	if err != nil {
		if e.app.logger != nil {
			e.app.logger.Warn("queryClusterNodeTypes failed, topology will fall back to node order",
				"cluster", e.Config.Name, "error", err)
		}
		return nil
	}
	return parseClusterNodeTypes(result)
}

// initialLoad pulls clusterNodes + clusterSpawnEvents from this
// entry's connection and returns the seeded node list.
func (e *connEntry) initialLoad(ctx context.Context) []cluster.NodeInfo {
	e.mu.Lock()
	conn := e.Conn
	e.mu.Unlock()
	if conn == nil {
		return nil
	}

	queries := client.NewQueryClient(conn.Dispatcher())
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var allNodes []cluster.NodeInfo
	if result, err := queries.ClusterNodes(ctx, client.ClusterNodesArgs{}); err != nil {
		// Don't swallow: an error here is indistinguishable from "no
		// nodes" and silently degrades the topology to the single
		// synthesized connection node below. Log it so the real cause
		// (authz / shape / timeout) is visible.
		if e.app.logger != nil {
			e.app.logger.Warn("queryClusterNodes failed, topology will fall back to the connected node",
				"cluster", e.Config.Name, "error", err)
		}
	} else {
		allNodes = parseClusterNodes(result)
	}
	if spawnResult, err := queries.ClusterSpawnEvents(ctx, client.ClusterSpawnEventsArgs{}); err != nil {
		if e.app.logger != nil {
			e.app.logger.Warn("queryClusterSpawnEvents failed",
				"cluster", e.Config.Name, "error", err)
		}
	} else {
		spawnNodes := parseSpawnEvents(spawnResult)
		seen := make(map[string]bool)
		for _, n := range allNodes {
			seen[n.Type] = true
		}
		for _, n := range spawnNodes {
			if !seen[n.Type] {
				allNodes = append(allNodes, n)
				seen[n.Type] = true
			}
		}
	}
	if len(allNodes) == 0 {
		allNodes = append(allNodes, cluster.NodeInfo{
			ID:      conn.NodeId,
			Name:    e.Config.Name,
			Type:    detectNodeType(conn.NodeId),
			Address: e.Config.Endpoint,
			Version: conn.Version,
			Health:  nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		})
	}
	// Local cluster: probe docker for the infrastructure containers
	// (LB, DB, IDENTITY, REDIS, LIVEKIT, voice-agent) that aren't
	// registered as memQL nodes via v1:cluster:node. Skipped for
	// non-local clusters -- docker on this machine has no
	// relationship to a remote cluster's topology.
	if e.Config.Name == "local" {
		allNodes = mergeInfraFromDocker(allNodes)
	}
	return allNodes
}

// runSubscriber drains the entry's subscription events and maintains
// e.Nodes in-place. If this entry is the viewed one, updates are
// mirrored to the live topology view too.
func (e *connEntry) runSubscriber() {
	e.mu.Lock()
	exited := e.subscriberExited
	events := e.Events
	e.mu.Unlock()
	defer close(exited)

	if events == nil {
		return
	}

	for ev := range events {
		info, ok := parseClusterNodeEvent(ev)
		if !ok {
			continue
		}
		e.applyUpdate(info)
		if e.app.isViewed(e.Config.Name) {
			e.app.clustersView.Topology.ApplyNodeUpdate(info)
			e.app.postRedraw()
		}
	}
}

// applyUpdate merges a single-node event into e.Nodes under lock.
func (e *connEntry) applyUpdate(incoming cluster.NodeInfo) {
	if incoming.Type == "" && incoming.ID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	idx := -1
	if incoming.ID != "" {
		for i, n := range e.Nodes {
			if n.ID == incoming.ID {
				idx = i
				break
			}
		}
	}
	if idx < 0 && incoming.Type != "" {
		for i, n := range e.Nodes {
			if n.ID == "" && n.Type == incoming.Type {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		e.Nodes = append(e.Nodes, incoming)
		return
	}
	existing := &e.Nodes[idx]
	if incoming.ID != "" {
		existing.ID = incoming.ID
	}
	if incoming.Name != "" {
		existing.Name = incoming.Name
	}
	if incoming.Type != "" {
		existing.Type = incoming.Type
	}
	if incoming.Address != "" {
		existing.Address = incoming.Address
	}
	if incoming.Version != "" {
		existing.Version = incoming.Version
	}
	if incoming.Health != nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED {
		existing.Health = incoming.Health
	}
	if incoming.Labels != nil {
		existing.Labels = incoming.Labels
	}
}

// Retry kicks a fresh 3-attempt cycle, typically invoked when the
// user presses Enter on a row that's in stateFailed. Idempotent: a
// Retry against an already-connecting or connected entry is a no-op.
func (e *connEntry) Retry() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	if !e.lifecycleStarted {
		// Registered-but-not-dialed entry (registerEntry, epic #239):
		// there is no lifecycle goroutine listening on cancelCh, so
		// signalling one would just leave the row wedged in a
		// cosmetic "connecting". Promoting an idle row to a live
		// connection is selection's job (CM2 #241), not Retry's.
		e.mu.Unlock()
		return
	}
	switch e.State {
	case stateConnecting, stateConnected, stateBackoff:
		// Already working -- nothing to do.
		e.mu.Unlock()
		return
	}
	// Failed (or Idle). Signal the lifecycle goroutine to loop again.
	old := e.cancelCh
	e.cancelCh = make(chan struct{})
	e.Attempt = 0
	e.State = stateConnecting // purely cosmetic until the loop picks it up
	e.mu.Unlock()

	// Closing the old cancelCh wakes the lifecycle goroutine's select
	// (it was blocked on cancelWatch) and lets it loop back into
	// attemptConnectCycle with the fresh cancel channel.
	close(old)
}

// Cancel interrupts an in-flight attempt or backoff wait, forcing
// the entry into stateFailed. Used by Esc.
func (e *connEntry) Cancel() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	// Only meaningful during Connecting or Backoff.
	switch e.State {
	case stateConnecting, stateBackoff:
	default:
		e.mu.Unlock()
		return
	}
	old := e.cancelCh
	e.cancelCh = make(chan struct{})
	e.mu.Unlock()
	close(old)
}

// stillAlive reports whether the entry is still logically active
// (not closed, not asked to stop via cancel or app quit).
func (e *connEntry) stillAlive() bool {
	select {
	case <-e.done:
		return false
	case <-e.app.quitCh:
		return false
	default:
	}
	// If cancelCh is already closed, caller wanted us to stop this
	// cycle. Don't consume it here -- the caller (sleepCancellable)
	// will see it too.
	return true
}

// sleepCancellable waits for d, returning false if the wait was
// interrupted by the entry's cancelCh, done, or the app's quitCh.
func (e *connEntry) sleepCancellable(d time.Duration) bool {
	e.mu.Lock()
	cancel := e.cancelCh
	e.mu.Unlock()

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-cancel:
		return false
	case <-e.done:
		return false
	case <-e.app.quitCh:
		return false
	}
}

// setStateAttempt updates State + Attempt + NextTryAt atomically and
// mirrors the state to the cluster-row Status field so the list icon
// reflects reality (connecting ◌, connected ●, unreachable ● red).
func (e *connEntry) setStateAttempt(s entryState, attempt int, nextTryAt time.Time) {
	e.mu.Lock()
	e.State = s
	e.Attempt = attempt
	e.NextTryAt = nextTryAt
	e.mu.Unlock()

	e.app.syncRowStatus(e.Config.Name, s)
}

// Close tears down the entry: signals goroutines to exit, closes the
// gRPC stream, waits briefly for clean shutdown. Idempotent.
func (e *connEntry) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	conn := e.Conn
	e.Conn = nil
	e.SubMgr = nil
	e.Events = nil
	subscriberExited := e.subscriberExited
	lifecycleStarted := e.lifecycleStarted
	e.mu.Unlock()

	close(e.done)
	if conn != nil {
		conn.Close()
	}

	if subscriberExited != nil {
		select {
		case <-subscriberExited:
		case <-time.After(500 * time.Millisecond):
		}
	}
	// Only wait on lifecycleExited if runLifecycle was actually
	// launched. Registered-but-not-dialed entries (registerEntry)
	// never close it, so waiting would just burn the 500ms timeout
	// per entry -- multiplied across every non-selected cluster at
	// quit (epic #239).
	if lifecycleStarted {
		select {
		case <-e.lifecycleExited:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// postRedraw triggers a screen redraw via the main event loop. Safe
// to call from any goroutine.
//
// Coalesces bursts via redrawPending: only the first caller in a
// quiet window actually posts an EventInterrupt. The event loop
// clears the flag right before draw() runs, so a postRedraw arriving
// while draw is mid-flight queues a fresh frame for the next tick
// (no missed updates). Callsites that fire dozens of times per
// second -- subscriber demuxes, lifecycle state machines, the
// backoff redraw loop -- collapse into one draw per quiet window
// instead of one draw per call.
func (a *App) postRedraw() {
	if a.screen == nil {
		return
	}
	if a.redrawPending.Swap(true) {
		return
	}
	a.screen.PostEvent(tcell.NewEventInterrupt(nil))
}

// isViewed reports whether the given cluster name is the one currently
// displayed in the topology pane.
func (a *App) isViewed(name string) bool {
	a.poolMu.RLock()
	defer a.poolMu.RUnlock()
	return a.viewed == name
}

// isSelectedCluster reports whether the given cluster name is the
// user's "working cluster".
func (a *App) isSelectedCluster(name string) bool {
	a.poolMu.RLock()
	defer a.poolMu.RUnlock()
	return a.selected == name
}

// setPendingSelect arms an auto-select for the named cluster: the next
// time it reaches stateConnected, onEntryConnected promotes it to the
// working cluster. Used by the Add flow so a just-added, just-signed-in
// cluster ends up selected without a manual Enter.
func (a *App) setPendingSelect(name string) {
	a.poolMu.Lock()
	a.pendingSelect = name
	a.poolMu.Unlock()
}

// takePendingSelect reports whether name is the armed pending-select
// cluster, clearing the arming if so (one-shot). Must be called WITHOUT
// poolMu held -- it takes the lock itself.
func (a *App) takePendingSelect(name string) bool {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if a.pendingSelect == name {
		a.pendingSelect = ""
		return true
	}
	return false
}

// onEntryConnected is called by an entry's lifecycle when it reaches
// stateConnected. Updates the list row and -- if this entry is the
// VIEWED one (arrow-key highlighted) -- paints its topology pane.
func (a *App) onEntryConnected(e *connEntry) {
	a.clustersView.SetConnected(e.Config.Name, true, e.Conn.NodeId, e.Conn.Version)
	if a.isViewed(e.Config.Name) {
		a.clustersView.Topology.SetNodeTypes(e.snapshotNodeTypes())
		a.clustersView.Topology.SetNodes(e.snapshotNodes())
		a.clustersView.Topology.SetDisconnected(false)
		// One-shot observability overlay fetch -- best-effort. The
		// Architecture navigator picks up the result on its next
		// draw; transient query failures leave the previous overlay
		// in place. A periodic refresh ticker is a follow-up
		// enhancement that can hang off this same fetcher.
		go a.refreshArchMetrics(e)
	}

	// If this entry IS the user's persisted working cluster
	// (selected on a prior session and restored from clusters.yaml),
	// re-fetch the Explorer + Agents data now that the dispatcher is
	// live. setSelected has the same hook for the manual-Enter path,
	// but the auto-connect-at-boot path never calls setSelected --
	// the selection field is filled in directly from yaml during
	// connect(). Without this call the Concepts tab stays empty on
	// every fresh launch even though ListConcepts would return
	// concepts the moment we ask.
	if a.isSelectedCluster(e.Config.Name) {
		a.refreshMyAccess(e.Config.Name, e.Conn)
		go a.refreshConcepts(context.Background())
		go a.refreshPacks(context.Background())
	}

	// Auto-select a just-added cluster on its first successful connect
	// (armed by the Add flow). setSelected promotes it to the working
	// cluster + refreshes the cluster-dependent tabs.
	if a.takePendingSelect(e.Config.Name) {
		a.setSelected(e.Config.Name)
	}
}

// refreshArchMetrics issues a single codeMetric query and hands the
// result to the architecture navigator. Runs off the connection-up
// callback in a goroutine so the connect path stays non-blocking;
// errors are swallowed (the navigator simply keeps its previous
// overlay, or shows none).
func (a *App) refreshArchMetrics(e *connEntry) {
	if e == nil || e.Conn == nil || a.clustersView == nil ||
		a.clustersView.Topology == nil || a.clustersView.Topology.Arch == nil {
		return
	}
	qc := client.NewQueryClient(e.Conn.Dispatcher())
	fetcher := &cluster.QueryClientMetricsFetcher{Client: qc}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.clustersView.Topology.Arch.RefreshMetrics(ctx, fetcher)
}

// onEntryDisconnected is called when an entry's stream drops
// unexpectedly, before the lifecycle enters a fresh retry cycle.
func (a *App) onEntryDisconnected(e *connEntry) {
	a.clustersView.SetConnected(e.Config.Name, false, "", "")
	if a.isViewed(e.Config.Name) {
		a.clustersView.Topology.SetDisconnected(true)
	}
}

// selectedName returns a.selected under lock (convenience for writing
// it to ClustersView.SelectedCluster on the UI thread).
func (a *App) selectedName() string {
	a.poolMu.RLock()
	defer a.poolMu.RUnlock()
	return a.selected
}

// syncRowStatus maps a pool entry's state to the cluster-row Status
// string used by the list icon renderer. Called from
// setStateAttempt after every transition. Connecting and Backoff
// both show the "connecting" ◌ so the user just sees a single
// "working on it" indicator during the 3-attempt cycle.
func (a *App) syncRowStatus(name string, s entryState) {
	var status string
	switch s {
	case stateConnected:
		status = "connected"
	case stateConnecting, stateBackoff:
		status = "connecting"
	case stateFailed:
		status = "unreachable"
	case stateNeedsConfig:
		status = "needs-auth"
	case stateNeedsToken:
		status = "needs-login"
	default:
		status = "unknown"
	}
	a.clustersView.SetRowStatus(name, status)
	a.postRedraw()
}
