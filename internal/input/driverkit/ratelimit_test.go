package driverkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
)

// guard bounds a wait for a goroutine; it is a deadlock guard, never a
// timing assertion.
const guard = 10 * time.Second

func TestRateLimiterLetsTwentyThroughThenBlocksTheNext(t *testing.T) {
	clk := clock.NewFake(epoch)
	l := NewRateLimiter(clk, 20)

	for i := range 20 {
		if err := l.Wait(t.Context()); err != nil {
			t.Fatalf("Wait() #%d = %v", i+1, err)
		}
	}
	if clk.PendingTimers() != 0 {
		t.Fatalf("the first 20 requests waited: %d timers", clk.PendingTimers())
	}

	done := make(chan error, 1)
	go func() { done <- l.Wait(t.Context()) }()
	waitPending(t, clk, 1)

	clk.Advance(time.Second - 1)
	select {
	case err := <-done:
		t.Fatalf("the 21st request returned %v before the window moved", err)
	default:
	}

	clk.Advance(1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait() #21 = %v", err)
		}
	case <-time.After(guard):
		t.Fatal("the 21st request was not released after a second")
	}
}

func TestRateLimiterSlidesTheWindow(t *testing.T) {
	clk := clock.NewFake(epoch)
	l := NewRateLimiter(clk, 2)

	mustWait(t, l)
	clk.Advance(600 * time.Millisecond)
	mustWait(t, l)

	// The third request waits for the first to leave the window, 400 ms on.
	done := make(chan error, 1)
	go func() { done <- l.Wait(t.Context()) }()
	waitPending(t, clk, 1)

	clk.Advance(400 * time.Millisecond)
	if err := <-done; err != nil {
		t.Fatalf("Wait() = %v", err)
	}

	// The second request left at 1.6 s: with the third in, the window is full.
	if got := l.reserve(); got != 600*time.Millisecond {
		t.Fatalf("reserve() = %v, want 600ms until the second request leaves", got)
	}
}

func TestRateLimiterWaitStopsOnCancel(t *testing.T) {
	clk := clock.NewFake(epoch)
	l := NewRateLimiter(clk, 1)
	mustWait(t, l)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Wait(ctx) }()
	waitPending(t, clk, 1)

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait() = %v, want context.Canceled", err)
		}
	case <-time.After(guard):
		t.Fatal("Wait did not return after the context was cancelled")
	}

	// The cancelled request counted nothing: the window still holds one.
	if len(l.sent) != 1 {
		t.Fatalf("the window holds %d requests, want 1", len(l.sent))
	}
}

func TestRateLimiterWaitWithADoneContextCountsNothing(t *testing.T) {
	l := NewRateLimiter(clock.NewFake(epoch), 5)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() = %v, want context.Canceled", err)
	}
	if len(l.sent) != 0 {
		t.Fatalf("the window holds %d requests, want 0", len(l.sent))
	}
}

func TestRateLimiterRateBelowOneIsOne(t *testing.T) {
	for _, rate := range []int{0, -4} {
		clk := clock.NewFake(epoch)
		l := NewRateLimiter(clk, rate)
		mustWait(t, l)

		if got := l.reserve(); got != time.Second {
			t.Errorf("rate %d: reserve() = %v, want 1s", rate, got)
		}
	}
}

func mustWait(t *testing.T, l *RateLimiter) {
	t.Helper()

	if err := l.Wait(t.Context()); err != nil {
		t.Fatalf("Wait() = %v", err)
	}
}
