package output

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/buffer"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/envelope"
	"github.com/getlaraplate/laraedge/internal/model"
)

// preparingSender is a Sender with a prepare step that answers the scripted
// outcomes, not ready, and fails the test if a batch is ever sent.
type preparingSender struct {
	t        *testing.T
	outcomes []Outcome
	prepared atomic.Int32
}

func (s *preparingSender) prepare(context.Context) (Outcome, bool) {
	n := int(s.prepared.Add(1))
	if n > len(s.outcomes) {
		return Outcome{}, false
	}

	return s.outcomes[n-1], false
}

func (s *preparingSender) Send(context.Context, *Batch) Outcome {
	s.t.Error("a batch was sent by an output that is not ready")

	return Outcome{Kind: Confirmed}
}

func (s *preparingSender) ReloadCredentials() {}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
	}
}

// TestLoopPreparerNotReady: an output that is not ready seals no batch; a
// Suspend suspends it and any other kind backs it off (M11).
func TestLoopPreparerNotReady(t *testing.T) {
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	buf, err := buffer.Open(dir, buffer.Options{
		Clock:     clock.NewFake(clk.Now()),
		DiskUsage: func(string) (uint64, uint64, error) { return 100, 100, nil },
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = buf.Close() })

	if err := buf.Append(model.SampleRecord(model.Sample{Device: "d", Signal: "s", TS: clk.Now(), Value: model.IntValue(1), Quality: model.Good, Priority: model.Bulk})); err != nil {
		t.Fatal(err)
	}

	inf := NewInflight(dir)
	bt, err := NewBatcher(buf, inf, envelope.NewIDGenerator(clk), clk, BatchLimits{MaxSamples: 10, MaxBody: 1 << 20}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	snd := &preparingSender{t: t, outcomes: []Outcome{{Kind: Suspend, Reason: "no token"}, {Kind: Retry, Reason: "not yet"}}}
	l := NewLoop(LoopConfig{ID: "o", Sender: snd, Batcher: bt, Clock: clk, Logger: slog.New(slog.DiscardHandler), Rand: func() float64 { return 1 }})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitCond(t, "the suspension", func() bool { return l.State() == SuspendedAuth && clk.PendingTimers() == 1 })
	if msg, _ := l.LastError(); msg != "no token" {
		t.Errorf("LastError() = %q, want %q", msg, "no token")
	}

	clk.Advance(authRetry)
	waitCond(t, "the backoff", func() bool { return l.State() == Backoff && clk.PendingTimers() == 1 })
	if msg, _ := l.LastError(); msg != "not yet" {
		t.Errorf("LastError() = %q, want %q", msg, "not yet")
	}
}
