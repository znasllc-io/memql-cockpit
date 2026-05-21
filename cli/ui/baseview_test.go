package ui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestBaseView_StartRefreshLoop_FiresImmediately pins the
// "immediate refresh on launch so the first paint after the user
// opens the tab isn't blank" behavior. Without this, the tab shows
// a blank list for one full interval after the user navigates to
// it -- the regression that planner has today vs. agents/chat.
func TestBaseView_StartRefreshLoop_FiresImmediately(t *testing.T) {
	var b BaseView
	var calls int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.StartRefreshLoop(ctx, time.Hour, func() { atomic.AddInt64(&calls, 1) })

	// Give the goroutine a chance to run the immediate call.
	deadline := time.Now().Add(500 * time.Millisecond)
	for atomic.LoadInt64(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("expected exactly 1 immediate call, got %d", got)
	}
}

// TestBaseView_StartRefreshLoop_TicksRepeatedly verifies fn fires
// on each ticker interval. Uses a small interval so the test runs
// fast.
func TestBaseView_StartRefreshLoop_TicksRepeatedly(t *testing.T) {
	var b BaseView
	var calls int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.StartRefreshLoop(ctx, 15*time.Millisecond, func() { atomic.AddInt64(&calls, 1) })

	// Wait for at least 3 calls (1 immediate + 2 ticks).
	deadline := time.Now().Add(500 * time.Millisecond)
	for atomic.LoadInt64(&calls) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&calls); got < 3 {
		t.Fatalf("expected >= 3 calls within 500ms, got %d", got)
	}
}

// TestBaseView_StartRefreshLoop_CancelStopsLoop verifies the
// goroutine exits when ctx is canceled. Counts calls before vs.
// after cancellation; if cancellation didn't take, the count would
// keep climbing.
//
// The drain sleep after cancel() is load-bearing: when the ticker
// case and ctx.Done() case both become ready in the same select,
// Go picks one at random, so one extra fn() call can complete
// AFTER cancel() returns. The test only cares that the loop
// eventually stops -- not that cancel is observed at the exact
// instant the goroutine is mid-fn. Sleep > tick interval so any
// in-flight tick has time to land, then sample pre.
func TestBaseView_StartRefreshLoop_CancelStopsLoop(t *testing.T) {
	var b BaseView
	var calls int64
	ctx, cancel := context.WithCancel(context.Background())

	b.StartRefreshLoop(ctx, 10*time.Millisecond, func() { atomic.AddInt64(&calls, 1) })

	// Let it run for ~50ms so a few ticks fire.
	time.Sleep(50 * time.Millisecond)
	cancel()
	// Drain any in-flight tick. 50ms is 5x the tick interval -- if
	// the loop is still ticking, calls would climb several times
	// over the drain window. After this sleep, the goroutine has
	// either exited cleanly or is permanently parked.
	time.Sleep(50 * time.Millisecond)
	pre := atomic.LoadInt64(&calls)

	// Wait long enough that, if the loop hadn't stopped, several
	// more ticks would have fired.
	time.Sleep(100 * time.Millisecond)
	post := atomic.LoadInt64(&calls)

	if post != pre {
		t.Errorf("loop kept ticking after cancel: pre=%d post=%d", pre, post)
	}
}

// TestBaseView_StartRefreshLoop_NilFnIsNoOp guards against a
// crashing goroutine if a view accidentally passes nil.
func TestBaseView_StartRefreshLoop_NilFnIsNoOp(t *testing.T) {
	var b BaseView
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Should not panic, should not spawn a goroutine.
	b.StartRefreshLoop(ctx, time.Millisecond, nil)
	time.Sleep(20 * time.Millisecond) // give a hypothetical loop a chance to misbehave
}

// TestBaseView_StartRefreshLoop_ZeroIntervalIsNoOp guards against
// ticker construction panicking on a non-positive interval.
func TestBaseView_StartRefreshLoop_ZeroIntervalIsNoOp(t *testing.T) {
	var b BaseView
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var called int64
	b.StartRefreshLoop(ctx, 0, func() { atomic.AddInt64(&called, 1) })
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt64(&called); got != 0 {
		t.Errorf("zero interval should be a no-op, fn ran %d times", got)
	}
}

// TestBaseView_StartRefreshLoop_FiresRedrawAfterEachCall confirms
// OnRedraw runs once per fn invocation -- the contract every
// existing per-view StartRefreshLoop honored.
func TestBaseView_StartRefreshLoop_FiresRedrawAfterEachCall(t *testing.T) {
	var b BaseView
	var fnCalls, redrawCalls int64
	b.OnRedraw = func() { atomic.AddInt64(&redrawCalls, 1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.StartRefreshLoop(ctx, 15*time.Millisecond, func() { atomic.AddInt64(&fnCalls, 1) })

	deadline := time.Now().Add(500 * time.Millisecond)
	for atomic.LoadInt64(&fnCalls) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	fc := atomic.LoadInt64(&fnCalls)
	rc := atomic.LoadInt64(&redrawCalls)
	if fc < 3 {
		t.Fatalf("expected >= 3 fn calls, got %d", fc)
	}
	if rc < fc {
		t.Errorf("expected redraw calls >= fn calls, got fn=%d redraw=%d", fc, rc)
	}
}

func TestBaseView_Notify_NilOnStatusIsNoOp(t *testing.T) {
	var b BaseView
	b.Notify("anything") // must not panic
}

func TestBaseView_Notify_ForwardsToOnStatus(t *testing.T) {
	var b BaseView
	got := ""
	b.OnStatus = func(s string) { got = s }
	b.Notify("hello")
	if got != "hello" {
		t.Errorf("OnStatus got %q, want %q", got, "hello")
	}
}

func TestBaseView_Redraw_NilOnRedrawIsNoOp(t *testing.T) {
	var b BaseView
	b.Redraw() // must not panic
}

func TestBaseView_Redraw_FiresOnRedraw(t *testing.T) {
	var b BaseView
	var calls int64
	b.OnRedraw = func() { atomic.AddInt64(&calls, 1) }
	b.Redraw()
	b.Redraw()
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("OnRedraw called %d times, want 2", got)
	}
}
