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
