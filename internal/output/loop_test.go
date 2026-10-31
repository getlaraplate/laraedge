package output_test

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/output"
)

// sent is what the fake sender saw of one batch.
type sent struct {
	messageID string
	seq       uint64
	records   []model.Record
	losses    []model.Loss
}

// fakeSender answers each Send with the next scripted outcome, Confirmed
// once the script is over, and reports every batch on calls.
type fakeSender struct {
	mu       sync.Mutex
	outcomes []output.Outcome
	calls    chan sent
	reloads  atomic.Int32
}

func newFakeSender(outcomes ...output.Outcome) *fakeSender {
	return &fakeSender{outcomes: outcomes, calls: make(chan sent, 64)}
}

func (s *fakeSender) Send(_ context.Context, b *output.Batch) output.Outcome {
	s.mu.Lock()
	out := output.Outcome{Kind: output.Confirmed}
	if len(s.outcomes) > 0 {
		out, s.outcomes = s.outcomes[0], s.outcomes[1:]
	}
	s.mu.Unlock()

	s.calls <- sent{messageID: b.MessageID, seq: b.Seq, records: b.Records(), losses: append([]model.Loss(nil), b.Losses...)}

	return out
}

func (s *fakeSender) ReloadCredentials() {
	s.reloads.Add(1)
}

// next returns the next batch sent, failing after guard.
func (s *fakeSender) next(t *testing.T) sent {
	t.Helper()

	select {
	case c := <-s.calls:
		return c
	case <-time.After(guard):
		t.Fatal("no batch sent")

		return sent{}
	}
}

// loopFixture runs a Loop over a real batcher and a fake sender.
type loopFixture struct {
	t          *testing.T
	h          *harness
	snd        *fakeSender
	loop       *output.Loop
	reconnects atomic.Int32
	cancel     context.CancelFunc
	done       chan error
}

func startLoop(t *testing.T, maxSamples int, snd *fakeSender, records ...model.Record) *loopFixture {
	t.Helper()

	f := &loopFixture{t: t, h: newHarness(t, output.BatchLimits{MaxSamples: maxSamples, MaxBody: 1 << 20}), snd: snd, done: make(chan error, 1)}
	f.h.open()
	f.h.append(records...)

	f.loop = output.NewLoop(output.LoopConfig{
		ID:          "plugin-out",
		Sender:      snd,
		Batcher:     f.h.bt,
		Clock:       f.h.clk,
		Logger:      slog.New(slog.DiscardHandler),
		OnReconnect: func() { f.reconnects.Add(1) },
		Rand:        func() float64 { return 1 },
	})

	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() { f.done <- f.loop.Run(ctx) }()

	t.Cleanup(f.stop)

	return f
}

func (f *loopFixture) stop() {
	f.cancel()
	select {
	case err := <-f.done:
		if err != nil {
			f.t.Errorf("Run() = %v, want nil on cancellation", err)
		}

		f.done <- nil
	case <-time.After(guard):
		f.t.Error("Run did not return after cancellation")
	}
}

// expectWait asserts that the loop waits exactly d on the fake clock in
// state, then lets it go.
func (f *loopFixture) expectWait(d time.Duration, state output.State) {
	f.t.Helper()

	waitTimers(f.t, f.h.clk, 1)
	if got := f.loop.State(); got != state {
		f.t.Fatalf("State() = %s while waiting, want %s", got, state)
	}

	f.h.clk.Advance(d - time.Nanosecond)
	if f.h.clk.PendingTimers() != 1 {
		f.t.Fatalf("the wait ended before %v", d)
	}

	f.h.clk.Advance(time.Nanosecond)
}

func (f *loopFixture) waitDelivered() {
	f.t.Helper()

	waitFor(f.t, "the last batch to be confirmed", func() bool {
		return f.h.buf.Stats().Records == 0 && f.loop.State() == output.Sending && f.loop.LastConfirmation().Equal(f.h.clk.Now())
	})
}

func sameSent(t *testing.T, got, want sent) {
	t.Helper()

	if got.messageID != want.messageID || got.seq != want.seq || len(got.records) != len(want.records) {
		t.Fatalf("resent batch %q seq %d with %d records, want %q seq %d with %d", got.messageID, got.seq, len(got.records), want.messageID, want.seq, len(want.records))
	}
}

func TestLoopConfirmAdvances(t *testing.T) {
	f := startLoop(t, 2, newFakeSender(), bulk(0), bulk(1), bulk(2), bulk(3))

	first := f.snd.next(t)
	second := f.snd.next(t)
	f.waitDelivered()

	if len(first.records) != 2 || len(second.records) != 2 || second.seq != first.seq+1 {
		t.Errorf("batches seq %d (%d records), seq %d (%d records); want two of 2 in sequence", first.seq, len(first.records), second.seq, len(second.records))
	}

	if f.h.clk.PendingTimers() != 0 {
		t.Errorf("the loop waits after a confirmation: %d timers", f.h.clk.PendingTimers())
	}

	if got := f.reconnects.Load(); got != 0 {
		t.Errorf("OnReconnect called %d times without a prior failure", got)
	}

	if msg, at := f.loop.LastError(); msg != "" || !at.IsZero() {
		t.Errorf("LastError() = %q at %v, want none", msg, at)
	}
}

func TestLoopRetryBacksOffHonouringRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter time.Duration
		wait       time.Duration
	}{
		{name: "no hint waits the first step", retryAfter: 0, wait: time.Second},
		{name: "shorter hint waits the step", retryAfter: 300 * time.Millisecond, wait: time.Second},
		{name: "longer hint is honoured", retryAfter: 42 * time.Second, wait: 42 * time.Second},
		{name: "hint capped at five minutes", retryAfter: time.Hour, wait: 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startLoop(t, 2, newFakeSender(output.Outcome{Kind: output.Retry, RetryAfter: tc.retryAfter, Reason: "busy"}), bulk(0), bulk(1))

			first := f.snd.next(t)
			failedAt := f.h.clk.Now()
			f.expectWait(tc.wait, output.Backoff)

			if msg, at := f.loop.LastError(); msg != "busy" || !at.Equal(failedAt) {
				t.Errorf("LastError() = %q at %v, want %q at %v", msg, at, "busy", failedAt)
			}

			sameSent(t, f.snd.next(t), first)
			f.waitDelivered()

			if got := f.reconnects.Load(); got != 1 {
				t.Errorf("OnReconnect called %d times, want 1", got)
			}
		})
	}
}

func TestLoopBackoffDoublesFromOneSecondToFiveMinutes(t *testing.T) {
	steps := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300}

	var script []output.Outcome
	for range steps {
		script = append(script, output.Outcome{Kind: output.Retry})
	}

	f := startLoop(t, 1, newFakeSender(script...), bulk(0))

	first := f.snd.next(t)
	for _, s := range steps {
		f.expectWait(s*time.Second, output.Backoff)
		sameSent(t, f.snd.next(t), first)
	}

	f.waitDelivered()
	if got := f.reconnects.Load(); got != 1 {
		t.Errorf("OnReconnect called %d times, want 1", got)
	}
}

func TestLoopSplitReusesTheSourceSeq(t *testing.T) {
	f := startLoop(t, 2, newFakeSender(output.Outcome{Kind: output.Split, Reason: "too large"}), bulk(0), bulk(1))

	refused := f.snd.next(t)
	kept := f.snd.next(t)
	rest := f.snd.next(t)
	f.waitDelivered()

	if kept.seq != refused.seq || kept.messageID == refused.messageID || len(kept.records) != 1 {
		t.Errorf("split batch seq %d id %q with %d records; want seq %d under a new id with 1", kept.seq, kept.messageID, len(kept.records), refused.seq)
	}

	if rest.seq != refused.seq+1 || len(rest.records) != 1 {
		t.Errorf("rest batch seq %d with %d records; want seq %d with 1", rest.seq, len(rest.records), refused.seq+1)
	}

	if f.h.clk.PendingTimers() != 0 {
		t.Errorf("the split waited: %d timers", f.h.clk.PendingTimers())
	}
}

func TestLoopDropsWithARejectedInvalidMarker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome output.Outcome
		limit   int
		records []model.Record
	}{
		{name: "invalid batch", outcome: output.Outcome{Kind: output.Invalid, Reason: "field x"}, limit: 2, records: []model.Record{bulk(0), bulk(1), bulk(2)}},
		{name: "one-record split", outcome: output.Outcome{Kind: output.Split, Reason: "too large"}, limit: 1, records: []model.Record{bulk(0), bulk(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startLoop(t, tc.limit, newFakeSender(tc.outcome), tc.records...)

			refused := f.snd.next(t)
			next := f.snd.next(t)
			f.waitDelivered()

			dropped := len(refused.records)
			if len(next.losses) != 1 || next.losses[0].Reason != model.LossRejectedInvalid || next.losses[0].Samples != dropped {
				t.Errorf("next batch losses = %+v, want one rejected_invalid marker of %d samples", next.losses, dropped)
			}

			if next.seq != refused.seq || next.messageID == refused.messageID {
				t.Errorf("next batch seq %d id %q, want the refused seq %d under a new id", next.seq, next.messageID, refused.seq)
			}

			if msg, _ := f.loop.LastError(); msg == "" {
				t.Error("LastError() is empty after a drop")
			}

			if f.h.clk.PendingTimers() != 0 {
				t.Errorf("the drop waited: %d timers", f.h.clk.PendingTimers())
			}
		})
	}
}

func TestLoopSuspendWaitsFiveMinutesOrReload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		recover func(f *loopFixture)
		reloads int32
	}{
		{name: "after five minutes", recover: func(f *loopFixture) { f.expectWait(5*time.Minute, output.SuspendedAuth) }},
		{name: "on reload", recover: func(f *loopFixture) {
			waitTimers(f.t, f.h.clk, 1)
			f.loop.ReloadCredentials()
		}, reloads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startLoop(t, 2, newFakeSender(output.Outcome{Kind: output.Suspend, Reason: "refused"}), bulk(0), bulk(1))

			first := f.snd.next(t)
			waitTimers(t, f.h.clk, 1)
			if got := f.loop.State(); got != output.SuspendedAuth {
				t.Fatalf("State() = %s, want suspended_auth", got)
			}

			if msg, _ := f.loop.LastError(); msg != "refused" {
				t.Errorf("LastError() = %q, want %q", msg, "refused")
			}

			tc.recover(f)
			sameSent(t, f.snd.next(t), first)
			f.waitDelivered()

			if got := f.snd.reloads.Load(); got != tc.reloads {
				t.Errorf("Sender.ReloadCredentials called %d times, want %d", got, tc.reloads)
			}

			if got := f.reconnects.Load(); got != 1 {
				t.Errorf("OnReconnect called %d times, want 1", got)
			}

			if got := f.h.buf.Stats().Dropped; len(got) != 0 {
				t.Errorf("Dropped = %v, want nothing dropped on a suspension", got)
			}
		})
	}
}
