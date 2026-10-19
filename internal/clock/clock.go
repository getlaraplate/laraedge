// Package clock abstracts time so that code with timers, sleeps and
// deadlines can be tested without real waiting and without timing assertions.
package clock

import (
	"context"
	"time"
)

// Clock is the source of time for everything in the agent that waits.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// NewTimer returns a timer that fires once, d after the call.
	NewTimer(d time.Duration) Timer
	// Sleep blocks for d or until ctx is done, in which case it returns ctx.Err().
	Sleep(ctx context.Context, d time.Duration) error
}

// Timer is a single-shot timer created by a Clock.
type Timer interface {
	// C returns the channel on which the fire time is delivered.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports true when the call
	// stopped a timer that had not fired yet.
	Stop() bool
}

// Real returns a Clock backed by the system clock.
func Real() Clock {
	return realClock{}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer {
	return realTimer{timer: time.NewTimer(d)}
}

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type realTimer struct {
	timer *time.Timer
}

func (t realTimer) C() <-chan time.Time { return t.timer.C }

func (t realTimer) Stop() bool { return t.timer.Stop() }
