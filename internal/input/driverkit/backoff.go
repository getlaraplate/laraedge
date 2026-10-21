// Package driverkit holds the pieces shared by the drivers that talk to a
// machine over a connection: the reconnection backoff, the request rate
// limiter and the health counters behind input.HealthReporter.
package driverkit

import (
	"context"
	"math"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
)

const (
	// BackoffMin is the first reconnection wait.
	BackoffMin = time.Second
	// BackoffMax caps the reconnection wait.
	BackoffMax = time.Minute
)

// Backoff is the exponential backoff with jitter of one connection: the step
// doubles from BackoffMin to BackoffMax after every failed attempt and the
// wait is drawn in [step/2, step]. It is owned by one goroutine.
type Backoff struct {
	clk     clock.Clock
	rand    func() float64
	attempt int
}

// NewBackoff returns a Backoff on clk; rand returns a value in [0, 1) and
// draws the jitter.
func NewBackoff(clk clock.Clock, rand func() float64) *Backoff {
	return &Backoff{clk: clk, rand: rand}
}

// Next returns the wait before the next attempt and raises the step.
func (b *Backoff) Next() time.Duration {
	step := BackoffMin
	for i := 0; i < b.attempt && step < BackoffMax; i++ {
		step *= 2
	}

	step = min(step, BackoffMax)
	if step < BackoffMax {
		b.attempt++
	}

	r := b.rand()
	switch {
	case r < 0 || math.IsNaN(r):
		r = 0
	case r > 1:
		r = 1
	}

	half := step / 2

	return half + time.Duration(r*float64(step-half))
}

// Wait sleeps for Next() on the backoff's clock, or until ctx is done, in
// which case it returns ctx.Err().
func (b *Backoff) Wait(ctx context.Context) error {
	return b.clk.Sleep(ctx, b.Next())
}

// Reset starts the backoff again from BackoffMin, after a connection that
// worked.
func (b *Backoff) Reset() {
	b.attempt = 0
}
