package driverkit

import (
	"context"
	"errors"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
)

var epoch = time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)

func constant(v float64) func() float64 { return func() float64 { return v } }

func TestBackoffSequence(t *testing.T) {
	tests := []struct {
		name string
		rand float64
		want []time.Duration
	}{
		{"no jitter draw, lower bound", 0, secs(0.5, 1, 2, 4, 8, 16, 30, 30, 30)},
		{"full jitter draw, upper bound", 1, secs(1, 2, 4, 8, 16, 32, 60, 60, 60)},
		{"middle draw", 0.5, secs(0.75, 1.5, 3, 6, 12, 24, 45, 45, 45)},
		{"draw below 0 is 0", -3, secs(0.5, 1, 2, 4, 8, 16, 30, 30, 30)},
		{"draw above 1 is 1", 7, secs(1, 2, 4, 8, 16, 32, 60, 60, 60)},
		{"NaN draw is 0", math.NaN(), secs(0.5, 1, 2, 4, 8, 16, 30, 30, 30)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBackoff(clock.NewFake(epoch), constant(tt.rand))
			for i, want := range tt.want {
				if got := b.Next(); got != want {
					t.Fatalf("Next() #%d = %v, want %v", i+1, got, want)
				}
			}
		})
	}
}

func TestBackoffStaysWithinBoundsWithJitter(t *testing.T) {
	draws := []float64{0.1, 0.9, 0.33, 0.77, 0.5, 0.01, 0.99, 0.6}
	i := 0
	b := NewBackoff(clock.NewFake(epoch), func() float64 { i++; return draws[i%len(draws)] })

	step := BackoffMin
	for n := 0; n < 20; n++ {
		got := b.Next()
		if got < step/2 || got > step {
			t.Fatalf("Next() #%d = %v, want within [%v, %v]", n+1, got, step/2, step)
		}
		step = min(step*2, BackoffMax)
	}
}

func TestBackoffReset(t *testing.T) {
	b := NewBackoff(clock.NewFake(epoch), constant(1))
	for range 5 {
		b.Next()
	}

	b.Reset()

	if got := b.Next(); got != time.Second {
		t.Fatalf("Next() after Reset = %v, want 1s", got)
	}
}

func TestBackoffWaitSleepsOnTheClock(t *testing.T) {
	clk := clock.NewFake(epoch)
	b := NewBackoff(clk, constant(1))

	done := make(chan error, 1)
	go func() { done <- b.Wait(t.Context()) }()
	waitPending(t, clk, 1)

	clk.Advance(time.Second - 1)
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v before the step elapsed", err)
	default:
	}

	clk.Advance(1)
	if err := <-done; err != nil {
		t.Fatalf("Wait() = %v", err)
	}
}

func TestBackoffWaitStopsOnCancel(t *testing.T) {
	clk := clock.NewFake(epoch)
	b := NewBackoff(clk, constant(1))
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- b.Wait(ctx) }()
	waitPending(t, clk, 1)

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() = %v, want context.Canceled", err)
	}
}

func secs(v ...float64) []time.Duration {
	out := make([]time.Duration, len(v))
	for i, s := range v {
		out[i] = time.Duration(s * float64(time.Second))
	}

	return out
}

// waitPending yields until n timers wait on clk, so a test knows a goroutine
// is blocked on the clock; guard bounds the wait against a deadlock.
func waitPending(t *testing.T, clk *clock.Fake, n int) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for clk.PendingTimers() < n {
		if time.Now().After(deadline) {
			t.Fatalf("PendingTimers() = %d, want %d", clk.PendingTimers(), n)
		}

		runtime.Gosched()
	}
}
