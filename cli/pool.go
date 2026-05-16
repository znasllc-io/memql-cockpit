package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/auth"
	"github.com/visionarys-io/memql-cockpit/cli/client"
	"github.com/visionarys-io/memql-cockpit/cli/cluster"
	"github.com/visionarys-io/memql-cockpit/cli/config"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	nodev1 "github.com/visionarys-io/memql/component/node/gen"
)

// entryState is the single source of truth for one pool entry's
// connection lifecycle. The state drives the cluster-list row icon,
// the detail-pane retry display, and whether the Explorer/Automations
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
	stateIdle       entryState = iota // brand-new entry, goroutine hasn't started
	stateConnecting                   // dial in flight
	stateConnected                    // live stream, subscriber + monitor running
	stateBackoff                      // waiting between failed attempts
	stateFailed                       // all retries exhausted, awaiting manual retry
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
	Events    <-chan *memqlv1.EventNotification
	Nodes     []cluster.NodeInfo
	NodeTypes []cluster.NodeTypeInfo // seeded order from queryClusterNodeTypes; drives topology row layout
	State     entryState
	Attempt   int       // current attempt number within the 1..maxAttempts cycle
	NextTryAt time.Time // when the next attempt will fire (meaningful only in stateBackoff)

	// SelectedPartition is the partition the user has chosen to work
	// against in this cluster. Restored from clusters.yaml on startup
	// (per-cluster sticky selection); falls back to "default" when
	// missing. Pushed onto the live Dispatcher so every outbound
	// request stamps this partition on the envelope.
	SelectedPartition string

	// Partitions is the live snapshot of v1:platform:partition rows
	// for this cluster, fed by a dedicated subscription. The CLI's
	// partition list reads from here on every redraw.
	Partitions []cluster.PartitionInfo

	// PartitionsSubId + PartitionsEvents track the secondary
	// subscription used to keep Partitions in sync.
	PartitionsSubId  string
	PartitionsEvents <-chan *memqlv1.EventNotification

	// cancelCh interrupts a backoff wait or aborts an in-flight dial.
	// Recreated by Retry() so closed-channel sends don't panic.
	cancelCh chan struct{}

	// done is closed exactly once by Close(). Signals lifecycle /
	// subscriber / monitor goroutines to exit permanently.
	done chan struct{}
	// Lifecycle completion markers. Close() waits briefly on these
	// so callers know the entry is quiescent.
	lifecycleExited            chan struct{}
	subscriberExited           chan struct{}
	partitionsSubscriberExited chan struct{}

	closed bool
}

// newConnEntry builds the entry metadata. Start the lifecycle
// goroutine (runLifecycle) via openEntry in app.go.
func newConnEntry(app *App, cfg config.ClusterConfig) *connEntry {
	// Restore per-cluster sticky partition. Empty string falls back to
	// "default" downstream so a brand-new cluster works without any
	// extra config -- the bootstrap automation seeds default on startup.
	selected := cfg.SelectedPartition
	if selected == "" {
		selected = "default"
	}
	return &connEntry{
		app:               app,
		Config:            cfg,
		State:             stateIdle,
		SelectedPartition: selected,
		cancelCh:          make(chan struct{}),
		done:              make(chan struct{}),
		lifecycleExited:   make(chan struct{}),
		subscriberExited:  nil, // allocated each time runSubscriber starts
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

		if err == nil {
			// Connected. Start both subscribers; each exits when its
			// Events channel closes (either normal shutdown or stream
			// drop). One for v1:cluster:node, one for
			// v1:platform:partition.
			e.mu.Lock()
			e.State = stateConnected
			e.subscriberExited = make(chan struct{})
			e.partitionsSubscriberExited = make(chan struct{})
			e.mu.Unlock()
			go e.runSubscriber()
			go e.runPartitionsSubscriber()
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
	token, err := auth.EnsureValidToken(ctx, e.Config)
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

	// Push the user's selected partition (if any) onto the dispatcher
	// so every outbound query/mutation/subscribe is partition-stamped
	// from the very first request.
	e.mu.Lock()
	selectedPart := e.SelectedPartition
	e.mu.Unlock()
	if selectedPart == "" {
		selectedPart = "default"
	}
	conn.Dispatcher().SetPartition(selectedPart)

	sm := client.NewSubscriptionManager(conn.Dispatcher())
	subCtx := context.Background()
	subId, events, err := sm.Subscribe(subCtx,
		memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
		"node.*.*.v1:cluster:node",
	)
	if err != nil {
		conn.Close()
		return err
	}

	// Second subscription: v1:platform:partition CRUD. Keeps the
	// partition list live as users add / edit / soft-delete from any
	// CLI session against the same cluster.
	partsSubId, partsEvents, err := sm.Subscribe(subCtx,
		memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS,
		"node.*.*.v1:platform:partition",
	)
	if err != nil {
		conn.Close()
		return err
	}

	e.mu.Lock()
	e.Conn = conn
	e.SubMgr = sm
	e.SubId = subId
	e.Events = events
	e.PartitionsSubId = partsSubId
	e.PartitionsEvents = partsEvents
	e.mu.Unlock()

	nodes := e.initialLoad(ctx)
	parts := e.partitionsInitialLoad(ctx)
	types := e.nodeTypesInitialLoad(ctx)
	e.mu.Lock()
	e.Nodes = nodes
	e.Partitions = parts
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
	result, err := queries.Execute(ctx, `queryClusterNodeTypes({})`)
	if err != nil {
		if e.app.logger != nil {
			e.app.logger.Warn("queryClusterNodeTypes failed, topology will fall back to node order",
				"cluster", e.Config.Name, "error", err)
		}
		return nil
	}
	return parseClusterNodeTypes(result)
}

// partitionsInitialLoad pulls queryListPartitions({}) and dedupes to the
// latest row per partition name. Soft-deleted (status="draining")
// rows are filtered out so the CLI list shows only active partitions.
// "default" is guaranteed to appear in the result so the CLI never
// renders a partition-less list (the bootstrap automation seeds it
// on every cluster; if the query didn't surface it, we add it back).
func (e *connEntry) partitionsInitialLoad(ctx context.Context) []cluster.PartitionInfo {
	e.mu.Lock()
	conn := e.Conn
	e.mu.Unlock()
	if conn == nil {
		return ensureDefaultPartition(nil)
	}
	queries := client.NewQueryClient(conn.Dispatcher())
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := queries.Execute(ctx, `queryListPartitions({})`)
	if err != nil {
		// Server may not have the listPartitions function loaded yet
		// (e.g. against an older deployment) -- show "default" as a
		// safe fallback so the CLI isn't empty.
		if e.app.logger != nil {
			e.app.logger.Warn("listPartitions query failed",
				"cluster", e.Config.Name, "error", err)
		}
		return ensureDefaultPartition(nil)
	}
	parsed := parsePartitions(result)
	if e.app.logger != nil {
		// Log the raw response shape so we can diagnose any parse
		// surprises -- "default disappeared after creating test"-class
		// bugs have usually been the parser dropping rows silently.
		e.app.logger.Debug("listPartitions parsed",
			"cluster", e.Config.Name,
			"parsed_count", len(parsed),
			"raw", result,
		)
	}
	parsed = ensureDefaultPartition(parsed)
	return parsed
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
	if result, err := queries.Execute(ctx, `queryClusterNodes({})`); err == nil {
		allNodes = parseClusterNodes(result)
	}
	if spawnResult, err := queries.Execute(ctx, `queryClusterSpawnEvents({})`); err == nil {
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

// runPartitionsSubscriber drains v1:platform:partition CDC events and
// keeps e.Partitions in sync. Re-renders the partition pane whenever
// this entry is the selected/active cluster.
func (e *connEntry) runPartitionsSubscriber() {
	e.mu.Lock()
	exited := e.partitionsSubscriberExited
	events := e.PartitionsEvents
	e.mu.Unlock()
	defer close(exited)

	if events == nil {
		return
	}

	for ev := range events {
		info, ok := parsePartitionEvent(ev)
		if !ok {
			continue
		}
		e.applyPartitionUpdate(info)
		// Partition pane mirrors the VIEWED cluster (arrow-key
		// highlighted), so redraw when this entry is the one in view.
		if e.app.isViewed(e.Config.Name) {
			e.app.refreshPartitionsView()
			e.app.postRedraw()
		}
	}
}

// applyPartitionUpdate merges a single partition CDC event into
// e.Partitions under lock. Replaces by name (each partition is its
// own concept-id time-series so name uniquely identifies the row).
func (e *connEntry) applyPartitionUpdate(incoming cluster.PartitionInfo) {
	if incoming.Name == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.Partitions {
		if e.Partitions[i].Name == incoming.Name {
			e.Partitions[i] = incoming
			return
		}
	}
	e.Partitions = append(e.Partitions, incoming)
}

// snapshotPartitions returns a copy of the entry's active partitions
// (status="active"; "draining" rows are filtered out). "default" is
// always included even if the backing data hasn't surfaced it -- the
// CLI treats default as an invariant.
func (e *connEntry) snapshotPartitions() []cluster.PartitionInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]cluster.PartitionInfo, 0, len(e.Partitions))
	for _, p := range e.Partitions {
		if p.Status == "draining" {
			continue
		}
		out = append(out, p)
	}
	return ensureDefaultPartition(out)
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
	// Partitions pane mirrors the VIEWED cluster's lifecycle --
	// "Connecting..." / "Unreachable..." messages swap in/out as the
	// arrow-key-highlighted cluster transitions through states.
	if e.app.isViewed(e.Config.Name) {
		e.app.refreshPartitionsView()
	}
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
	select {
	case <-e.lifecycleExited:
	case <-time.After(500 * time.Millisecond):
	}
}

// postRedraw triggers a screen redraw via the main event loop. Safe
// to call from any goroutine.
func (a *App) postRedraw() {
	if a.screen != nil {
		a.screen.PostEvent(tcell.NewEventInterrupt(nil))
	}
}

// isViewed reports whether the given cluster name is the one currently
// displayed in the topology pane.
func (a *App) isViewed(name string) bool {
	a.poolMu.RLock()
	defer a.poolMu.RUnlock()
	return a.viewed == name
}

// isSelectedCluster reports whether the given cluster name is the
// user's "working cluster". Used by the partitions subscriber to
// decide whether to repaint the partition pane on every CDC event.
func (a *App) isSelectedCluster(name string) bool {
	a.poolMu.RLock()
	defer a.poolMu.RUnlock()
	return a.selected == name
}

// refreshPartitionsView pushes the VIEWED (arrow-key-highlighted)
// cluster's latest partition snapshot into the PartitionsView. The
// pane mirrors whichever cluster the user is looking at -- same
// semantics as the topology pane on the right. The working (selected)
// cluster is a separate concept that drives Explorer / Automations
// and is NOT what the partitions pane follows.
//
// States:
//   - No viewed cluster at all (very rare -- startup highlights the
//     saved selection): "Highlight a cluster..."
//   - Viewed but not connected: contextual "connecting..." /
//     "unreachable..." message, matches topology's
//     "Waiting for cluster data..." behavior.
//   - Connected: live partition snapshot (ensureDefaultPartition keeps
//     default visible even during a parse race).
func (a *App) refreshPartitionsView() {
	if a.clustersView == nil || a.clustersView.Partitions == nil {
		return
	}
	a.poolMu.RLock()
	name := a.viewed
	entry := a.pool[name]
	a.poolMu.RUnlock()

	if name == "" || entry == nil {
		a.clustersView.Partitions.Reset("Highlight a cluster to manage partitions.")
		return
	}

	entry.mu.Lock()
	state := entry.State
	entry.mu.Unlock()

	if state != stateConnected {
		var msg string
		switch state {
		case stateConnecting, stateBackoff:
			msg = fmt.Sprintf("Connecting to %q -- partitions will appear once connected.", name)
		case stateFailed:
			msg = fmt.Sprintf("%q is unreachable. Press R on its row to retry.", name)
		default:
			msg = fmt.Sprintf("Cluster %q is not connected.", name)
		}
		a.clustersView.Partitions.Reset(msg)
		return
	}

	parts := entry.snapshotPartitions()
	active := entry.SelectedPartition
	if active == "" {
		active = "default"
	}
	a.clustersView.Partitions.SetPartitions(parts)
	a.clustersView.Partitions.Active = active
}

// onEntryConnected is called by an entry's lifecycle when it reaches
// stateConnected. Updates the list row and -- if this entry is the
// VIEWED one (arrow-key highlighted) -- paints its topology and
// partitions pane. partitionsInitialLoad has just completed in
// dialOnce, so this is the first opportunity to show real partition
// data instead of the pre-connect placeholder. The user's working
// cluster selection is NOT touched here; it's sticky across sessions
// (see persistSelected).
func (a *App) onEntryConnected(e *connEntry) {
	a.clustersView.SetConnected(e.Config.Name, true, e.Conn.NodeId, e.Conn.Version)
	if a.isViewed(e.Config.Name) {
		a.clustersView.Topology.SetNodeTypes(e.snapshotNodeTypes())
		a.clustersView.Topology.SetNodes(e.snapshotNodes())
		a.clustersView.Topology.SetDisconnected(false)
		a.refreshPartitionsView()
		// One-shot observability overlay fetch -- best-effort. The
		// Architecture navigator picks up the result on its next
		// draw; transient query failures leave the previous overlay
		// in place. A periodic refresh ticker is a follow-up
		// enhancement that can hang off this same fetcher.
		go a.refreshArchMetrics(e)
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
		// Last-known partitions are still in entry.Partitions until the
		// next dial wipes them, so this preserves whatever the user was
		// looking at + keeps the active marker correct.
		a.refreshPartitionsView()
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
	default:
		status = "unknown"
	}
	a.clustersView.SetRowStatus(name, status)
	a.postRedraw()
}
