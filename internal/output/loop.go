package output

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/model"
)

// authRetry is the wait of a suspended output before it tries again.
const authRetry = 5 * time.Minute

// State is the sending state of an output (spec 6.8).
type State int32

const (
	// Sending is the normal state: batches are sent as they are sealed.
	Sending State = iota
	// Backoff waits after a temporary error before resending the batch.
	Backoff
	// SuspendedAuth waits after the destination refused the credentials, or
	// they could not be resolved; records keep buffering.
	SuspendedAuth
)

// String returns the state's metric name.
func (s State) String() string {
	switch s {
	case Sending:
		return "sending"
	case Backoff:
		return "backoff"
	case SuspendedAuth:
		return "suspended_auth"
	default:
		return fmt.Sprintf("state(%d)", int32(s))
	}
}

// Output delivers the batches of one output to its destination.
type Output interface {
	// Run sends until ctx is done.
	Run(ctx context.Context) error
	// State returns the current sending state; safe for concurrent use.
	State() State
	// ReloadCredentials resolves the credentials again before the next
	// attempt and ends a suspension at once; safe for concurrent use.
	ReloadCredentials()
}

// OutcomeKind is how a destination answered one batch.
type OutcomeKind int

const (
	// Confirmed: the destination holds the batch (accepted or duplicate);
	// it is settled and the next one is sent at once.
	Confirmed OutcomeKind = iota
	// Retry: a temporary failure; the same batch is resent after a backoff
	// of at least RetryAfter, capped at five minutes.
	Retry
	// Split: the batch is too large; it is split in two, the first half
	// keeping its source_seq, or dropped with a rejected_invalid loss
	// marker when it holds one record.
	Split
	// Invalid: the destination refused the batch as invalid; it is dropped
	// with a rejected_invalid loss marker.
	Invalid
	// Suspend: the destination refused the credentials, or they could not
	// be resolved; sending waits five minutes or ReloadCredentials.
	Suspend
)

// Outcome is the answer of a Sender to one batch.
type Outcome struct {
	// Kind says how the Loop settles the batch.
	Kind OutcomeKind
	// RetryAfter is the destination's wait hint for Retry, zero for none.
	RetryAfter time.Duration
	// Reason is a short text without secrets, recorded as the last error
	// unless Kind is Confirmed; the Sender logs the details itself.
	Reason string
}

// Sender delivers one batch to a destination and says how it answered. The
// Loop owns the batch: a Sender never confirms, splits or drops it.
type Sender interface {
	// Send delivers b once and returns the destination's answer; a failure
	// to reach the destination is a Retry. Called from Run only.
	Send(ctx context.Context, b *Batch) Outcome
	// ReloadCredentials makes the next Send use credentials resolved again;
	// safe for concurrent use.
	ReloadCredentials()
}

// preparer is implemented by a Sender with a step to take before the next
// batch is sealed, such as resolving its credentials. ok false makes the
// Loop suspend (Suspend) or back off (any other kind) without sealing a
// batch, so an output that cannot send waits before taking records from
// its buffer.
type preparer interface {
	prepare(ctx context.Context) (outcome Outcome, ok bool)
}

// LoopConfig configures a Loop.
type LoopConfig struct {
	// ID is the output id, used in logs.
	ID string
	// Sender delivers the batches; required.
	Sender Sender
	// Batcher seals the batches of this output; required.
	Batcher *Batcher
	// Clock times the backoff and the suspension; nil means clock.Real().
	Clock clock.Clock
	// Logger receives the output's logs; nil means slog.Default().
	Logger *slog.Logger
	// OnReconnect is called, from Run, on the first confirmation after
	// Backoff or SuspendedAuth (ruling R5). It must not block for long.
	OnReconnect func()
	// Rand returns the backoff jitter in [0, 1); nil means math/rand/v2.
	Rand func() float64
}

// Loop is the transport-agnostic delivery loop of one output: it takes the
// sealed batches of its Batcher one at a time, hands each to its Sender
// and settles it from the outcome (spec 6.5).
type Loop struct {
	id          string
	sender      Sender
	batcher     *Batcher
	clk         clock.Clock
	log         *slog.Logger
	onReconnect func()

	reload        chan struct{}
	reloadPending atomic.Bool

	// Owned by the Run goroutine.
	backoff    backoff
	recovering bool

	mu          sync.Mutex
	state       State
	lastConfirm time.Time
	lastErr     string
	lastErrAt   time.Time
}

var _ Output = (*Loop)(nil)

// NewLoop returns the loop of cfg. Nothing is sent before Run. It panics
// without a Sender or a Batcher.
func NewLoop(cfg LoopConfig) *Loop {
	if cfg.Sender == nil || cfg.Batcher == nil {
		panic("output: NewLoop without a sender or a batcher")
	}

	l := &Loop{
		id:          cfg.ID,
		sender:      cfg.Sender,
		batcher:     cfg.Batcher,
		clk:         cfg.Clock,
		log:         cfg.Logger,
		onReconnect: cfg.OnReconnect,
		reload:      make(chan struct{}, 1),
		backoff:     backoff{rand: cfg.Rand},
		state:       Sending,
	}

	if l.clk == nil {
		l.clk = clock.Real()
	}

	if l.log == nil {
		l.log = slog.Default()
	}

	if l.onReconnect == nil {
		l.onReconnect = func() {}
	}

	if l.backoff.rand == nil {
		l.backoff.rand = rand.Float64
	}

	return l
}

// Run sends the batches of the output until ctx is done, then returns nil;
// call it once.
// A batch is settled only from the destination's answer: confirmed,
// split, or dropped with a rejected_invalid loss marker; a Retry or a
// Suspend, and every local failure, keeps it in flight to be resent
// identical after a backoff or a suspension.
func (l *Loop) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		l.reloadPending.Store(false)

		if p, ok := l.sender.(preparer); ok {
			if out, ready := p.prepare(ctx); !ready {
				l.recordReason(out.Reason)
				if out.Kind == Suspend {
					l.suspend(ctx)
				} else {
					l.backOff(ctx, out.RetryAfter)
				}

				continue
			}
		}

		b, err := l.batcher.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}

			l.log.Error("output: cannot seal the next batch", "output", l.id, "error", err)
			l.recordError("next batch: " + err.Error())
			l.backOff(ctx, 0)

			continue
		}

		out := l.sender.Send(ctx, b)
		if out.Kind == Retry && ctx.Err() != nil {
			break
		}

		l.settle(ctx, b, out)
	}

	return nil
}

// settle acts on the outcome of b.
func (l *Loop) settle(ctx context.Context, b *Batch, out Outcome) {
	switch out.Kind {
	case Confirmed:
		l.confirm(ctx, b)
	case Split:
		l.split(ctx, b, out.Reason)
	case Invalid:
		l.drop(ctx, b, out.Reason, "batch dropped as rejected_invalid")
	case Suspend:
		l.recordReason(out.Reason)
		l.suspend(ctx)
	default:
		l.recordReason(out.Reason)
		l.backOff(ctx, out.RetryAfter)
	}
}

// confirm settles b as delivered.
func (l *Loop) confirm(ctx context.Context, b *Batch) {
	if err := l.batcher.Confirm(b); err != nil {
		l.log.Error("output: cannot settle a delivered batch; it will be resent", "output", l.id, "seq", b.Seq, "error", err)
		l.recordError("confirm: " + err.Error())
		l.backOff(ctx, 0)

		return
	}

	l.log.Debug("output: batch delivered", "output", l.id, "seq", b.Seq, "records", b.Len())

	l.backoff.reset()
	if l.recovering {
		l.recovering = false
		l.onReconnect()
	}

	now := l.clk.Now()
	l.mu.Lock()
	l.lastConfirm, l.state = now, Sending
	l.mu.Unlock()
}

// split answers a batch too large: split b in two, or drop a one-record
// batch, which cannot be split, with a rejected_invalid loss marker.
func (l *Loop) split(ctx context.Context, b *Batch, reason string) {
	if b.Len() <= 1 {
		l.log.Warn("output: the destination refused a one-record batch as too large; dropping it with a rejected_invalid loss marker", "output", l.id, "seq", b.Seq)
		l.drop(ctx, b, reason, "one record dropped as rejected_invalid")

		return
	}

	n := b.Len() / 2
	if _, err := l.batcher.Shrink(b, n); err != nil {
		l.log.Error("output: cannot split the refused batch; it will be resent", "output", l.id, "seq", b.Seq, "error", err)
		l.recordError("split: " + err.Error())
		l.backOff(ctx, 0)

		return
	}

	l.log.Warn("output: the destination refused the batch as too large; split", "output", l.id, "seq", b.Seq, "records", b.Len(), "kept", n)
	l.setState(Sending)
}

// drop drops b with a rejected_invalid loss marker and records reason with
// what was done.
func (l *Loop) drop(ctx context.Context, b *Batch, reason, done string) {
	if err := l.batcher.Drop(b, model.LossRejectedInvalid); err != nil {
		l.log.Error("output: cannot drop the refused batch; it will be resent", "output", l.id, "seq", b.Seq, "error", err)
		l.recordError("drop: " + err.Error())
		l.backOff(ctx, 0)

		return
	}

	if reason != "" {
		done = reason + ": " + done
	}

	l.recordError(done)
	l.setState(Sending)
}

// backOff waits the next backoff step, at least retryAfter (capped).
func (l *Loop) backOff(ctx context.Context, retryAfter time.Duration) {
	wait := max(l.backoff.next(), min(retryAfter, backoffMax))

	l.setState(Backoff)
	l.recovering = true
	l.log.Debug("output: waiting before resending", "output", l.id, "wait", wait.String())
	l.pause(ctx, wait, false)
}

// suspend waits authRetry, or until ReloadCredentials.
func (l *Loop) suspend(ctx context.Context) {
	l.setState(SuspendedAuth)
	l.recovering = true
	l.pause(ctx, authRetry, true)
}

// pause waits d on the clock, until ctx is done or, when wakeOnReload,
// until ReloadCredentials is called.
func (l *Loop) pause(ctx context.Context, d time.Duration, wakeOnReload bool) {
	if wakeOnReload && l.reloadPending.Load() {
		return
	}

	timer := l.clk.NewTimer(d)
	defer timer.Stop()

	var reload <-chan struct{}
	if wakeOnReload {
		reload = l.reload
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			return
		case <-reload:
			if l.reloadPending.Load() {
				return
			}
		}
	}
}

// ReloadCredentials makes the Sender resolve its credentials again before
// the next attempt and ends a suspension at once (bootstrap reload, SIGHUP).
func (l *Loop) ReloadCredentials() {
	l.sender.ReloadCredentials()
	l.reloadPending.Store(true)
	select {
	case l.reload <- struct{}{}:
	default:
	}
}

func (l *Loop) setState(s State) {
	l.mu.Lock()
	l.state = s
	l.mu.Unlock()
}

// recordReason records a Sender's reason, when it gave one.
func (l *Loop) recordReason(reason string) {
	if reason != "" {
		l.recordError(reason)
	}
}

func (l *Loop) recordError(msg string) {
	now := l.clk.Now()

	l.mu.Lock()
	l.lastErr, l.lastErrAt = msg, now
	l.mu.Unlock()
}

// State returns the current sending state.
func (l *Loop) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.state
}

// LastConfirmation returns when the last batch was confirmed, zero before any.
func (l *Loop) LastConfirmation() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.lastConfirm
}

// LastError returns the last error, a short text without secrets, and when
// it happened; empty before any.
func (l *Loop) LastError() (string, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.lastErr, l.lastErrAt
}
