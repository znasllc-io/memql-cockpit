package consent

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock yields the value the test most recently set. Concurrent-
// safe so the broadcast/subscribe paths can race against grant
// transitions without flaking.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock                { return &fakeClock{now: t} }
func (c *fakeClock) Now() time.Time                      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) advance(d time.Duration)             { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func TestManager_AllowsRejectsWithoutGrant(t *testing.T) {
	m := NewManager()
	dec := m.Allows("workerComputer", "mouse_click")
	if dec.Allowed {
		t.Fatal("first call must be rejected -- no consent granted yet")
	}
	if !strings.Contains(dec.Reason, "consent has not been granted") {
		t.Errorf("rejection reason should hint at the grant flow; got %q", dec.Reason)
	}
}

func TestManager_GrantedWindowAdmits(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)

	if _, err := m.Grant(time.Hour, false); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	for _, tc := range []struct {
		tool, action string
	}{
		{"workerHost", "exec"},
		{"workerHost", "fs_read"},
		{"workerComputer", "mouse_click"},
		{"workerComputer", "screenshot"},
	} {
		dec := m.Allows(tc.tool, tc.action)
		if !dec.Allowed {
			t.Errorf("%s.%s should be admitted within an open window; reason=%s", tc.tool, tc.action, dec.Reason)
		}
	}
}

func TestManager_RevokeImmediatelyDenies(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)

	if _, err := m.Grant(time.Hour, false); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if dec := m.Allows("workerHost", "exec"); !dec.Allowed {
		t.Fatalf("expected admit pre-revoke; got %v", dec)
	}
	m.Revoke()
	dec := m.Allows("workerHost", "exec")
	if dec.Allowed {
		t.Errorf("post-revoke call must be denied")
	}
	if !strings.Contains(dec.Reason, "consent has not been granted") {
		t.Errorf("post-revoke reason should match the no-window reason; got %q", dec.Reason)
	}
}

func TestManager_WindowExpiryDenies(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	clk := newFakeClock(start)
	m := NewManagerWithClock(clk.Now)

	if _, err := m.Grant(10*time.Minute, false); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// Mid-window: admit.
	clk.advance(5 * time.Minute)
	if dec := m.Allows("workerHost", "exec"); !dec.Allowed {
		t.Fatalf("mid-window call should admit; got %v", dec)
	}
	// Past expiry: deny.
	clk.advance(6 * time.Minute) // total = 11m, window was 10m
	dec := m.Allows("workerHost", "exec")
	if dec.Allowed {
		t.Errorf("post-expiry call must be denied")
	}
	if !strings.Contains(dec.Reason, "expired") {
		t.Errorf("post-expiry reason should mention expiry; got %q", dec.Reason)
	}
}

func TestManager_SecondGrantOverwrites(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)

	if _, err := m.Grant(time.Hour, false); err != nil {
		t.Fatalf("Grant 1: %v", err)
	}
	first := m.Snapshot().ExpiresAt
	clk.advance(5 * time.Minute)
	if _, err := m.Grant(2*time.Hour, true); err != nil {
		t.Fatalf("Grant 2: %v", err)
	}
	second := m.Snapshot().ExpiresAt
	if !second.After(first) {
		t.Errorf("second Grant should advance expiry; got first=%v second=%v", first, second)
	}
	if !m.Snapshot().Strict {
		t.Errorf("second Grant set strict=true; snapshot should reflect it")
	}
}

func TestManager_NegativeWindowRejected(t *testing.T) {
	m := NewManager()
	if _, err := m.Grant(0, false); err == nil {
		t.Error("Grant(0) should reject")
	}
	if _, err := m.Grant(-time.Second, false); err == nil {
		t.Error("Grant(-1s) should reject")
	}
}

func TestManager_SubscribeReceivesEvents(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)
	ch, cancel := m.Subscribe()
	defer cancel()

	if _, err := m.Grant(time.Hour, false); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	m.Allows("workerHost", "exec")
	m.Revoke()

	// Drain the channel and inspect what arrived. We expect 3
	// events (granted, dispatch, revoked) in order.
	type seen struct{ kind EventKind }
	var got []seen
	timeout := time.After(time.Second)
	for len(got) < 3 {
		select {
		case ev := <-ch:
			got = append(got, seen{kind: ev.Kind})
		case <-timeout:
			t.Fatalf("only %d events arrived: %v", len(got), got)
		}
	}
	want := []seen{{EventGranted}, {EventDispatch}, {EventRevoked}}
	for i := range want {
		if got[i].kind != want[i].kind {
			t.Errorf("event[%d] = %q, want %q", i, got[i].kind, want[i].kind)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		tool, action string
		want         Class
	}{
		{"workerHost", "fs_read", ClassObserve},
		{"workerHost", "fs_list", ClassObserve},
		{"workerHost", "fs_stat", ClassObserve},
		{"workerHost", "http_fetch", ClassObserve},
		{"workerHost", "exec", ClassInteract},
		{"workerHost", "fs_write", ClassInteract},
		{"workerComputer", "screenshot", ClassObserve},
		{"workerComputer", "mouse_click", ClassInteract},
		{"workerComputer", "mouse_move", ClassInteract},
		{"workerComputer", "key_type", ClassInteract},
		{"workerComputer", "key_press", ClassInteract},
		{"workerHost", "experimental_new_action", ClassUnknown},
		{"workerOther", "foo", ClassUnknown},
		{"  WORKERHOST  ", "  FS_READ  ", ClassObserve}, // case + whitespace
	}
	for _, tc := range cases {
		got := Classify(tc.tool, tc.action)
		if got != tc.want {
			t.Errorf("Classify(%q, %q) = %q, want %q", tc.tool, tc.action, got, tc.want)
		}
	}
}

func TestSnapshot_GrantedShowsExpiry(t *testing.T) {
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	m := NewManagerWithClock(clk.Now)

	if snap := m.Snapshot(); snap.Granted {
		t.Fatal("Snapshot before any Grant should report Granted=false")
	}
	if _, err := m.Grant(30*time.Minute, true); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	snap := m.Snapshot()
	if !snap.Granted {
		t.Error("Snapshot after Grant should report Granted=true")
	}
	if snap.Window != 30*time.Minute {
		t.Errorf("Snapshot.Window = %v, want 30m", snap.Window)
	}
	if !snap.Strict {
		t.Error("Snapshot.Strict should reflect the Grant's strict flag")
	}
	// Advance past expiry; Snapshot should flip back to ungranted.
	clk.advance(31 * time.Minute)
	if snap := m.Snapshot(); snap.Granted {
		t.Error("Snapshot after expiry should report Granted=false")
	}
}

// --- 64-B: strict-mode per-action approval ---

// TestStrictMode_KeyTypeBlocksAwaitingApproval drives the load-bearing
// path: an open strict window admits ClassObserve immediately, but
// workerComputer.key_type now blocks on a pending approval until an
// operator clicks Allow.
func TestStrictMode_KeyTypeBlocksAwaitingApproval(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(2 * time.Second)
	if _, err := m.Grant(time.Hour, true); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}

	// ClassObserve under a strict window still admits without a gate.
	if dec := m.Allows("workerComputer", "screenshot"); !dec.Allowed {
		t.Errorf("screenshot must still admit under strict: %s", dec.Reason)
	}

	// key_type blocks; another goroutine watches the pending queue
	// and Approves it.
	done := make(chan Decision, 1)
	go func() {
		done <- m.Allows("workerComputer", "key_type")
	}()

	// Poll for the pending entry to land (registration is racy with
	// the goroutine schedule; 100ms is generous enough not to flake).
	deadline := time.Now().Add(500 * time.Millisecond)
	var pending []PendingApprovalInfo
	for time.Now().Before(deadline) {
		pending = m.PendingApprovals()
		if len(pending) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending approval, got %d", len(pending))
	}
	if pending[0].Tool != "workerComputer" || pending[0].Action != "key_type" {
		t.Errorf("pending entry mis-shaped: %+v", pending[0])
	}

	// Approve and verify the blocked call returns Allowed=true.
	if err := m.Approve(pending[0].Id); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	select {
	case dec := <-done:
		if !dec.Allowed {
			t.Errorf("key_type after Approve must admit; reason=%s", dec.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("Allows did not return after Approve")
	}
	// Pending queue drained.
	if got := m.PendingApprovals(); len(got) != 0 {
		t.Errorf("pending queue should be empty after Approve; got %d", len(got))
	}
}

// TestStrictMode_MouseClickDeny exercises the deny path -- a denied
// strict-mode call returns Allowed=false with the deny reason in
// the Decision.
func TestStrictMode_MouseClickDeny(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(2 * time.Second)
	if _, err := m.Grant(time.Hour, true); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}

	done := make(chan Decision, 1)
	go func() {
		done <- m.Allows("workerComputer", "mouse_click")
	}()
	pending := waitForPending(t, m, 1)
	if err := m.Deny(pending[0].Id); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	select {
	case dec := <-done:
		if dec.Allowed {
			t.Error("mouse_click after Deny must reject")
		}
		if !strings.Contains(dec.Reason, "denied per-action") {
			t.Errorf("reject reason should mention per-action denial; got %q", dec.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("Allows did not return after Deny")
	}
}

// TestStrictMode_TimeoutDenies pins the timeout safety: an
// unanswered approval defaults to deny once the timeout fires.
func TestStrictMode_TimeoutDenies(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	if _, err := m.Grant(time.Hour, true); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}
	dec := m.Allows("workerComputer", "key_type")
	if dec.Allowed {
		t.Error("timeout should default to deny")
	}
	if !strings.Contains(dec.Reason, "timed out") {
		t.Errorf("deny reason should mention timeout; got %q", dec.Reason)
	}
	if got := m.PendingApprovals(); len(got) != 0 {
		t.Errorf("pending queue should be empty after timeout; got %d", len(got))
	}
}

// TestStrictMode_RevokeCancelsPending pins the kill-switch
// behaviour: Revoke must drop every in-flight strict-mode
// approval. The blocked dispatcher goroutine receives an
// immediate deny carrying the revoke reason.
func TestStrictMode_RevokeCancelsPending(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(5 * time.Second)
	if _, err := m.Grant(time.Hour, true); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}
	done := make(chan Decision, 1)
	go func() {
		done <- m.Allows("workerComputer", "key_type")
	}()
	waitForPending(t, m, 1)

	m.Revoke()

	select {
	case dec := <-done:
		if dec.Allowed {
			t.Error("revoke during pending approval must deny")
		}
		if !strings.Contains(dec.Reason, "consent revoked") {
			t.Errorf("deny reason should mention revoke; got %q", dec.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("Allows did not return after Revoke")
	}
}

// TestStrictMode_HighRiskSubset confirms the gate ONLY applies to
// the key_type + mouse_click subset. The other ClassInteract calls
// stay admitted by the standing window.
func TestStrictMode_HighRiskSubset(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	if _, err := m.Grant(time.Hour, true); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}
	for _, tc := range []struct {
		tool, action string
		wantBlocked  bool
	}{
		{"workerComputer", "key_type", true},
		{"workerComputer", "mouse_click", true},
		{"workerComputer", "key_press", false}, // ClassInteract but NOT high-risk
		{"workerComputer", "mouse_move", false},
		{"workerHost", "exec", false}, // ClassInteract but workerHost, not workerComputer
		{"workerHost", "fs_write", false},
		{"workerComputer", "screenshot", false}, // ClassObserve
	} {
		t.Run(tc.tool+"."+tc.action, func(t *testing.T) {
			dec := m.Allows(tc.tool, tc.action)
			if tc.wantBlocked {
				// blocked subset: with no operator response, the
				// 40ms timeout fires and the call denies.
				if dec.Allowed {
					t.Errorf("high-risk %s.%s must be gated by strict mode", tc.tool, tc.action)
				}
			} else {
				if !dec.Allowed {
					t.Errorf("non-high-risk %s.%s must admit under strict mode; reason=%s",
						tc.tool, tc.action, dec.Reason)
				}
			}
		})
	}
}

// TestStrictMode_NonStrictBypassesApproval pins that a NON-strict
// open window still admits key_type / mouse_click without going
// through the approval gate.
func TestStrictMode_NonStrictBypassesApproval(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	if _, err := m.Grant(time.Hour, false); err != nil {
		t.Fatalf("Grant non-strict: %v", err)
	}
	dec := m.Allows("workerComputer", "key_type")
	if !dec.Allowed {
		t.Errorf("non-strict window must admit key_type without approval; reason=%s", dec.Reason)
	}
}

// TestStrictMode_BroadcastsApprovalRequestedEvent confirms the
// EventApprovalRequested broadcast lands on subscribers with the
// id + tool + action populated -- this is the event the TUI's
// modal queue is driven by.
func TestStrictMode_BroadcastsApprovalRequestedEvent(t *testing.T) {
	m := NewManager()
	m.SetApprovalTimeout(40 * time.Millisecond)
	// Subscribe BEFORE Grant so the EventGranted broadcast lands
	// in the channel and we don't race the Subscribe registration.
	ch, cancel := m.Subscribe()
	defer cancel()
	if _, err := m.Grant(time.Hour, true); err != nil {
		t.Fatalf("Grant strict: %v", err)
	}

	// Drain the EventGranted broadcast.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected EventGranted before approval flow")
	}

	go m.Allows("workerComputer", "key_type")

	var requested Event
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-ch:
			if ev.Kind == EventApprovalRequested {
				requested = ev
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
		if requested.Kind != "" {
			break
		}
	}
	if requested.Kind != EventApprovalRequested {
		t.Fatalf("expected EventApprovalRequested broadcast, got kind=%q", requested.Kind)
	}
	if requested.ApprovalId == "" {
		t.Error("EventApprovalRequested must carry an approval_id")
	}
	if requested.Tool != "workerComputer" || requested.Action != "key_type" {
		t.Errorf("EventApprovalRequested fields wrong: %+v", requested)
	}
}

// TestApprove_UnknownIdErrors confirms a stale Allow (after timeout
// or revoke) reports an error to the TUI so its modal can clear
// gracefully.
func TestApprove_UnknownIdErrors(t *testing.T) {
	m := NewManager()
	if err := m.Approve("not-a-real-id"); err == nil {
		t.Error("Approve must error on unknown id")
	}
	if err := m.Deny("not-a-real-id"); err == nil {
		t.Error("Deny must error on unknown id")
	}
}

// TestIsHighRiskAction guards the high-risk classifier so accidental
// classification changes (e.g. dropping mouse_click) fail loudly.
func TestIsHighRiskAction(t *testing.T) {
	for _, tc := range []struct {
		tool, action string
		want         bool
	}{
		{"workerComputer", "key_type", true},
		{"workerComputer", "mouse_click", true},
		{"workerComputer", "KEY_TYPE", true}, // case + whitespace tolerant
		{" workerComputer ", " mouse_click ", true},
		{"workerComputer", "mouse_move", false},
		{"workerComputer", "key_press", false},
		{"workerComputer", "screenshot", false},
		{"workerHost", "exec", false},
		{"workerHost", "fs_write", false},
		{"", "", false},
	} {
		if got := isHighRiskAction(tc.tool, tc.action); got != tc.want {
			t.Errorf("isHighRiskAction(%q, %q) = %v, want %v", tc.tool, tc.action, got, tc.want)
		}
	}
}

// waitForPending polls until at least `n` approvals are pending or
// the deadline fires. Helper used by the strict-mode tests to avoid
// duplicating the race-safe poll loop.
func waitForPending(t *testing.T, m *Manager, n int) []PendingApprovalInfo {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		pending := m.PendingApprovals()
		if len(pending) >= n {
			return pending
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending approvals", n)
	return nil
}
