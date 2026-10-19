package clock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func fired(t *testing.T, timer Timer) bool {
	t.Helper()
	select {
	case <-timer.C():
		return true
	default:
		return false
	}
}

func TestFakeNow(t *testing.T) {
	f := NewFake(epoch)
	if !f.Now().Equal(epoch) {
		t.Fatalf("Now() = %v, want %v", f.Now(), epoch)
	}
	f.Advance(90 * time.Second)
	if want := epoch.Add(90 * time.Second); !f.Now().Equal(want) {
		t.Fatalf("Now() after Advance = %v, want %v", f.Now(), want)
	}
}

func TestFakeTimersFireOnlyOnAdvance(t *testing.T) {
	f := NewFake(epoch)
	short := f.NewTimer(time.Second)
	long := f.NewTimer(time.Minute)

	if fired(t, short) || fired(t, long) {
		t.Fatal("timers fired before any Advance")
	}

	f.Advance(500 * time.Millisecond)
	if fired(t, short) || fired(t, long) {
		t.Fatal("timers fired before their deadline")
	}

	f.Advance(500 * time.Millisecond)
	if !fired(t, short) {
		t.Fatal("short timer did not fire at its deadline")
	}
	if fired(t, long) {
		t.Fatal("long timer fired early")
	}

	f.Advance(time.Minute)
	if !fired(t, long) {
		t.Fatal("long timer did not fire")
	}
}

func TestFakeTimerDeliversDeadlineTime(t *testing.T) {
	f := NewFake(epoch)
	timer := f.NewTimer(time.Second)
	f.Advance(time.Hour)

	got := <-timer.C()
	if want := epoch.Add(time.Second); !got.Equal(want) {
		t.Fatalf("timer delivered %v, want its deadline %v", got, want)
	}
}

func TestFakeTimerStop(t *testing.T) {
	f := NewFake(epoch)

	stopped := f.NewTimer(time.Second)
	if !stopped.Stop() {
		t.Fatal("Stop() on a pending timer = false, want true")
	}
	if stopped.Stop() {
		t.Fatal("second Stop() = true, want false")
	}
	f.Advance(time.Minute)
	if fired(t, stopped) {
		t.Fatal("stopped timer fired")
	}

	done := f.NewTimer(time.Second)
	f.Advance(time.Second)
	if done.Stop() {
		t.Fatal("Stop() on a fired timer = true, want false")
	}
}

func TestFakeZeroDurationTimerFiresOnNextAdvance(t *testing.T) {
	f := NewFake(epoch)
	timer := f.NewTimer(0)
	if fired(t, timer) {
		t.Fatal("zero timer fired before Advance")
	}
	f.Advance(0)
	if !fired(t, timer) {
		t.Fatal("zero timer did not fire on Advance(0)")
	}
}

func TestFakeSleepReturnsWhenAdvanced(t *testing.T) {
	f := NewFake(epoch)
	errc := make(chan error, 1)
	started := make(chan struct{})

	go func() {
		close(started)
		errc <- f.Sleep(context.Background(), time.Minute)
	}()
	<-started
	waitForTimers(t, f, 1)

	f.Advance(time.Minute)
	if err := <-errc; err != nil {
		t.Fatalf("Sleep() = %v, want nil", err)
	}
}

func TestFakeSleepReturnsOnCancel(t *testing.T) {
	f := NewFake(epoch)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)

	go func() { errc <- f.Sleep(ctx, time.Hour) }()
	waitForTimers(t, f, 1)

	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep() = %v, want context.Canceled", err)
	}
	if n := f.PendingTimers(); n != 0 {
		t.Fatalf("PendingTimers() = %d after cancelled Sleep, want 0", n)
	}
}

func TestFakeSleepWithCancelledContextReturnsAtOnce(t *testing.T) {
	f := NewFake(epoch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep() = %v, want context.Canceled", err)
	}
}

func TestFakeConcurrentUse(t *testing.T) {
	f := NewFake(epoch)
	var wg sync.WaitGroup

	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timer := f.NewTimer(time.Second)
			_ = f.Now()
			<-timer.C()
		}()
	}

	waitForTimers(t, f, 20)
	f.Advance(time.Second)
	wg.Wait()
}

func TestRealClock(t *testing.T) {
	c := Real()

	if c.Now().IsZero() {
		t.Fatal("Real().Now() is zero")
	}

	timer := c.NewTimer(time.Millisecond)
	<-timer.C()
	if timer.Stop() {
		t.Fatal("Stop() on a fired timer = true, want false")
	}

	if err := c.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep() = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep() with cancelled ctx = %v, want context.Canceled", err)
	}
}

// waitForTimers blocks until the fake has n pending timers, so a goroutine
// registering a timer is ordered before the test advances time.
func waitForTimers(t *testing.T, f *Fake, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.PendingTimers() != n {
		if time.Now().After(deadline) {
			t.Fatalf("PendingTimers() = %d, want %d", f.PendingTimers(), n)
		}
		time.Sleep(time.Millisecond)
	}
}
