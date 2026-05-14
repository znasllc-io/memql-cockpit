package ui

import (
	"sync"
	"time"
)

// Debouncer delays execution of a function until a quiet period has passed.
// Each call to Trigger() resets the timer. Only the last trigger fires.
type Debouncer struct {
	delay time.Duration
	timer *time.Timer
	mu    sync.Mutex
	fn    func()
}

// NewDebouncer creates a debouncer with the given delay.
func NewDebouncer(delay time.Duration, fn func()) *Debouncer {
	return &Debouncer{
		delay: delay,
		fn:    fn,
	}
}

// Trigger resets the debounce timer. The function runs after `delay` of inactivity.
func (d *Debouncer) Trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(d.delay, d.fn)
}

// Cancel stops any pending execution.
func (d *Debouncer) Cancel() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}
