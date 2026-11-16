// Package backoff holds the exponential backoff with jitter shared by the
// connection drivers (internal/input/driverkit) and the outputs
// (internal/output). It lives outside both so that output does not import
// input.
package backoff

import (
	"math"
	"time"
)

// Exponential is an exponential backoff with jitter: the step doubles from
// its minimum to its maximum after every failed attempt and the wait is
// drawn in [step/2, step]. It is owned by one goroutine.
type Exponential struct {
	min     time.Duration
	max     time.Duration
	rand    func() float64
	attempt int
}

// New returns an Exponential from minStep to maxStep; rand returns a value in
// [0, 1) and draws the jitter. A draw outside [0, 1], or NaN, is clamped.
func New(minStep, maxStep time.Duration, rand func() float64) Exponential {
	return Exponential{min: minStep, max: maxStep, rand: rand}
}

// Next returns the wait before the next attempt and raises the step.
func (b *Exponential) Next() time.Duration {
	step := b.min
	for i := 0; i < b.attempt && step < b.max; i++ {
		step *= 2
	}

	step = min(step, b.max)
	if step < b.max {
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

// Reset starts the backoff again from its minimum, after an attempt that
// worked.
func (b *Exponential) Reset() {
	b.attempt = 0
}
