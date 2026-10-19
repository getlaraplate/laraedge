package clock

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Fake is a Clock whose time only moves when Advance is called. It is safe
// for concurrent use.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// NewFake returns a Fake clock reading start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start}
}

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.now
}

// NewTimer returns a timer that fires when Advance moves the clock to or past
// now+d. A timer with d <= 0 fires on the next Advance, including Advance(0).
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()

	timer := &fakeTimer{
		clock:    f,
		deadline: f.now.Add(d),
		ch:       make(chan time.Time, 1),
	}
	f.timers = append(f.timers, timer)

	return timer
}

// Sleep blocks until Advance moves the clock d forward or ctx is done, in
// which case it returns ctx.Err().
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	timer := f.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Advance moves the clock forward by d and fires every timer whose deadline
// is reached, in deadline order. Each timer is delivered its own deadline.
// Timers registered by other goroutines while Advance runs are considered on
// the next call.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	target := f.now.Add(d)

	var due []*fakeTimer
	remaining := f.timers[:0]
	for _, timer := range f.timers {
		if timer.deadline.After(target) {
			remaining = append(remaining, timer)
		} else {
			due = append(due, timer)
		}
	}
	f.timers = remaining

	sort.SliceStable(due, func(i, j int) bool { return due[i].deadline.Before(due[j].deadline) })
	for _, timer := range due {
		if timer.deadline.After(f.now) {
			f.now = timer.deadline
		}
		timer.ch <- timer.deadline
	}

	f.now = target
}

// PendingTimers returns the number of timers created and neither fired nor
// stopped. Tests use it to wait until a goroutine has started waiting.
func (f *Fake) PendingTimers() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.timers)
}

type fakeTimer struct {
	clock    *Fake
	deadline time.Time
	ch       chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()

	for i, pending := range t.clock.timers {
		if pending == t {
			t.clock.timers = append(t.clock.timers[:i], t.clock.timers[i+1:]...)
			return true
		}
	}

	return false
}
