package ui

import (
	"context"
	"sync"
	"time"
)

// BaseView is the embed-able scaffolding shared by every tab in the
// cockpit TUI. Each view (Concepts / Planner / Agents / Chat /
// Clusters) re-declared the same `mu / Theme / GatedMessage /
// OnStatus / OnRedraw` skeleton and re-implemented the same
// `StartRefreshLoop` helper. Embedding BaseView lets a view focus
// on its domain state.
//
// Intentionally NOT in BaseView:
//
//   - `Focus FocusPane` -- every view has its own typed enum with
//     view-specific members (FocusPlans/FocusTasks for Planner,
//     FocusList/FocusDetail for Agents, etc.). Hoisting a single
//     FocusPane type into ui/ would either be useless (the enum
//     values differ) or force every view onto the same alphabet.
//
//   - `QueryClient` -- adding it here would make cli/ui/ depend on
//     the SDK client package, which inverts the layering (ui is
//     imported by features, not the other way around). Views keep
//     their own `QueryClient func() *client.QueryClient` field.
//
// The lock is named `Mu` (exported) so embedding views can RLock /
// Lock against it directly without a method on BaseView. Helper
// methods on BaseView intentionally do NOT touch the lock --
// otherwise a view that locks Mu before calling a BaseView helper
// would deadlock on re-entry.
type BaseView struct {
	Mu           sync.RWMutex
	Theme        Theme
	GatedMessage string
	OnStatus     func(string)
	OnRedraw     func()
}

// StartRefreshLoop runs fn once immediately, then again every
// `interval` until `ctx` is canceled. After each call, OnRedraw
// fires so the screen picks up whatever state fn mutated. The
// immediate-fire-then-tick shape is what the agents and chat views
// already do today (so the first paint isn't blank); the planner
// view will pick up that behavior on migration.
//
// Safe to call once at app-init after cluster wiring is in place.
// Returns immediately; the loop runs on its own goroutine.
func (b *BaseView) StartRefreshLoop(ctx context.Context, interval time.Duration, fn func()) {
	if fn == nil || interval <= 0 {
		return
	}
	go func() {
		fn()
		b.Redraw()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fn()
				b.Redraw()
			}
		}
	}()
}

// Notify forwards a status string to OnStatus if it's wired up.
// Safe to call from any goroutine; the callback owns its own
// synchronization.
func (b *BaseView) Notify(msg string) {
	if b.OnStatus != nil {
		b.OnStatus(msg)
	}
}

// Redraw signals OnRedraw if it's wired up. Used by both
// StartRefreshLoop after each tick and by view code that mutated
// state outside the refresh loop (e.g. handling a key press that
// changed selection).
func (b *BaseView) Redraw() {
	if b.OnRedraw != nil {
		b.OnRedraw()
	}
}
