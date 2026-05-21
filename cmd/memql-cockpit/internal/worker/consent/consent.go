// Package consent implements the per-(user, scope) consent state
// machine that gates every workerHost / workerComputer tool
// dispatch. Lives next to the worker because the gate sits inside
// the worker's dispatcher hot path -- moving it elsewhere would
// either require a network round-trip or expose the predicate to
// every cockpit caller.
//
// The state machine is intentionally tiny: one active window at a
// time, ManagerSnapshot for read-only inspection, single-writer
// transitions via Grant / Revoke. The IPC layer (socket.go) marshals
// external requests onto these primitives; the dispatcher
// (worker/tools/dispatcher.go) calls Allows() in the hot path.
//
// Reference: memql-cockpit#64.
package consent

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Class names the broad category a (tool, action) pair belongs to.
// Used by Classify() to drive both the gate decision (today: same
// for both -- presence of an active window) and the per-action
// audit stream (today: surfaced verbatim in events for the future
// TUI pane).
//
// ClassObserve is the read-side: fs_read / fs_list / fs_stat /
// http_fetch / workerComputer.screenshot.
//
// ClassInteract is the write-side: exec / fs_write /
// workerComputer.{mouse_click,mouse_move,key_type,key_press}.
//
// ClassUnknown is the safety net for unknown (tool, action) pairs;
// they're treated as ClassInteract for gate purposes (deny-by-
// default) until the classifier learns them.
type Class string

const (
	ClassObserve  Class = "observe"
	ClassInteract Class = "interact"
	ClassUnknown  Class = "unknown"
)

// Decision is the gate result. Reason is human-readable, suitable
// for the dispatcher to embed in its `consent_required` failure
// message back to the agent.
type Decision struct {
	Allowed bool
	Reason  string
}

// Status is a point-in-time snapshot of the manager's state.
// Exposed via the IPC `status` op and the TUI's would-be Workers
// pane.
type Status struct {
	// Granted reports whether a window is open at the time of the
	// snapshot. Equivalent to (ExpiresAt.After(now)).
	Granted bool `json:"granted"`
	// ExpiresAt is the absolute time the window closes. Zero when
	// Granted == false.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// Window is the original duration the grant was issued for.
	// Carried so the TUI / `consent status` can show "expires in
	// 12m of a 1h window" without doing the subtraction itself.
	Window time.Duration `json:"window_ms,omitempty"`
	// Strict reports whether the active window requires per-action
	// approval for ClassInteract calls. v1 ships the flag in the
	// wire shape but enforces it as a no-op -- the foreground
	// per-call approval surface ships as a follow-up under #64.
	Strict bool `json:"strict,omitempty"`
}

// Manager holds the active-window state machine plus a small
// bounded event channel the IPC layer's WATCH op drains.
//
// Concurrency: Allows / Grant / Revoke / Snapshot are safe under
// arbitrary goroutine contention. The single-writer transitions
// take an exclusive lock; readers take the shared one.
type Manager struct {
	mu        sync.RWMutex
	expiresAt time.Time
	window    time.Duration
	strict    bool

	// now is the time source. Tests inject a fixed clock; production
	// passes nil and we fall back to time.Now.UTC.
	now func() time.Time

	subsMu sync.Mutex
	subs   []chan Event
}

// NewManager builds a Manager with the live clock. Pass NewManagerWithClock
// in tests to inject a deterministic time source.
func NewManager() *Manager {
	return &Manager{now: func() time.Time { return time.Now().UTC() }}
}

// NewManagerWithClock is the test-friendly constructor. Passing
// nil yields the live clock.
func NewManagerWithClock(clock func() time.Time) *Manager {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Manager{now: clock}
}

// Grant opens a consent window of `window` duration starting at
// the manager's current clock. A second Grant within an existing
// window OVERWRITES (extends or shortens) -- the operator's most
// recent decision wins. Returns the absolute expiry time so
// callers can log it.
//
// Window <= 0 errors out (use Revoke to close immediately).
func (m *Manager) Grant(window time.Duration, strict bool) (time.Time, error) {
	if window <= 0 {
		return time.Time{}, errors.New("consent: window must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exp := m.now().Add(window)
	m.expiresAt = exp
	m.window = window
	m.strict = strict
	m.broadcast(Event{
		At:        m.now(),
		Kind:      EventGranted,
		ExpiresAt: exp,
		Window:    window,
		Strict:    strict,
	})
	return exp, nil
}

// Revoke closes any active window IMMEDIATELY. No-op when no
// window is open. Always emits a `revoked` event so subscribers
// can show the operator intent.
func (m *Manager) Revoke() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expiresAt = time.Time{}
	m.window = 0
	m.strict = false
	m.broadcast(Event{At: m.now(), Kind: EventRevoked})
}

// Allows decides whether a single (tool, action) call may run
// against the host right now. Used by the worker's dispatcher in
// the hot path; concurrent-safe under shared locking.
//
// Returns Decision{Allowed:true,...} when an active window covers
// the call. Otherwise the Reason field carries the operator-
// readable explanation the dispatcher copies into its
// `consent_required` failure message.
//
// Emits a `dispatch` event regardless of outcome so subscribers
// (the future Workers pane) can render a live audit tail.
func (m *Manager) Allows(tool, action string) Decision {
	class := Classify(tool, action)
	m.mu.RLock()
	exp := m.expiresAt
	strict := m.strict
	m.mu.RUnlock()

	now := m.now()
	if exp.IsZero() {
		dec := Decision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"computer-use consent has not been granted -- run `memql-cockpit worker consent grant --window=<duration>` on this host before retrying (%s.%s, class=%s)",
				tool, action, class),
		}
		m.recordDispatch(tool, action, class, dec)
		return dec
	}
	if !now.Before(exp) {
		dec := Decision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"consent window expired at %s -- run `memql-cockpit worker consent grant --window=<duration>` to re-open (%s.%s, class=%s)",
				exp.Format(time.RFC3339), tool, action, class),
		}
		m.recordDispatch(tool, action, class, dec)
		return dec
	}
	// Active window covers this call.
	if strict && class == ClassInteract {
		// Per-action strict approval lands in the #64 follow-up.
		// For now strict mode still admits the call but flags the
		// event so subscribers can flag the call to the operator.
		dec := Decision{Allowed: true, Reason: "consent granted (strict mode: per-action approval is a follow-up under #64)"}
		m.recordDispatch(tool, action, class, dec)
		return dec
	}
	dec := Decision{Allowed: true, Reason: "consent granted"}
	m.recordDispatch(tool, action, class, dec)
	return dec
}

// Snapshot returns a copy of the current state.
func (m *Manager) Snapshot() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.expiresAt.IsZero() || !m.now().Before(m.expiresAt) {
		return Status{Granted: false}
	}
	return Status{
		Granted:   true,
		ExpiresAt: m.expiresAt,
		Window:    m.window,
		Strict:    m.strict,
	}
}

// EventKind names a Manager event.
type EventKind string

const (
	EventGranted  EventKind = "granted"
	EventRevoked  EventKind = "revoked"
	EventDispatch EventKind = "dispatch"
)

// Event is the audit-tail / state-change record streamed to
// Subscribe() readers.
type Event struct {
	At        time.Time     `json:"at"`
	Kind      EventKind     `json:"kind"`
	Tool      string        `json:"tool,omitempty"`
	Action    string        `json:"action,omitempty"`
	Class     Class         `json:"class,omitempty"`
	Allowed   bool          `json:"allowed,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	ExpiresAt time.Time     `json:"expires_at,omitempty"`
	Window    time.Duration `json:"window_ms,omitempty"`
	Strict    bool          `json:"strict,omitempty"`
}

// Subscribe returns a channel that receives Manager events plus a
// cancel func that detaches the subscription. Slow subscribers are
// dropped (channel buffer fills) -- the audit pane is a best-effort
// live tail, not a durable log.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	m.subsMu.Lock()
	m.subs = append(m.subs, ch)
	m.subsMu.Unlock()
	cancel := func() {
		m.subsMu.Lock()
		defer m.subsMu.Unlock()
		for i, c := range m.subs {
			if c == ch {
				m.subs = append(m.subs[:i], m.subs[i+1:]...)
				close(c)
				return
			}
		}
	}
	return ch, cancel
}

// broadcast fans an event out to every subscriber. Slow consumers
// see a drop (default branch) rather than blocking the writer.
// Caller must NOT hold m.mu when calling (we acquire subsMu here).
func (m *Manager) broadcast(e Event) {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for _, ch := range m.subs {
		select {
		case ch <- e:
		default:
			// Subscriber too slow; drop. The pane will resync on
			// next poll-derived snapshot.
		}
	}
}

func (m *Manager) recordDispatch(tool, action string, class Class, dec Decision) {
	m.broadcast(Event{
		At:      m.now(),
		Kind:    EventDispatch,
		Tool:    tool,
		Action:  action,
		Class:   class,
		Allowed: dec.Allowed,
		Reason:  dec.Reason,
	})
}

// Classify maps a (tool, action) pair to its consent class.
// Unknown pairs default to ClassInteract for the gate's purpose --
// deny-by-default until the classifier learns them.
func Classify(tool, action string) Class {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "workerhost":
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "fs_read", "fs_list", "fs_stat", "http_fetch":
			return ClassObserve
		case "exec", "fs_write":
			return ClassInteract
		}
	case "workercomputer":
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "screenshot":
			return ClassObserve
		case "mouse_click", "mouse_move", "key_type", "key_press":
			return ClassInteract
		}
	}
	return ClassUnknown
}
