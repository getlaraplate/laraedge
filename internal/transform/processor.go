package transform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Processor applies the transform chains of one input's points to what the
// input emits, and serves their time-driven steps with one goroutine on the
// injected clock.
//
// Lock order (ruling R2): processor, then input instance, router, buffer. The
// processor holds its own mutex while it applies a chain and while it calls
// the downstream emit, so the samples of one signal keep their order across
// the input's goroutines and the deadline goroutine. The downstream emit must
// never call back into the processor, and Close must not be called while
// holding a lock the downstream emit takes.
type Processor struct {
	inputID string
	clk     clock.Clock
	log     *slog.Logger
	// state is nil without persistence.
	state *stateFile

	errors atomic.Uint64

	// rearm wakes the deadline goroutine after an Apply moved a deadline.
	rearm chan struct{}

	mu sync.Mutex
	// points holds the chains in configuration order; byKey indexes them.
	points []*pointChain
	byKey  map[pointKey]*pointChain
	// downstream is the emit of the last Wrap, used by the deadline goroutine.
	downstream func(model.Record)
	errLog     *errorLog
	started    bool
	closed     bool
	cancel     context.CancelFunc
	done       chan struct{}
	// armed is the deadline the goroutine waits on, when armedOK.
	armed   time.Time
	armedOK bool
}

type pointKey struct {
	device, signal string
}

type pointChain struct {
	key   pointKey
	chain *Chain
}

// NewProcessor compiles the chain of every point of cfg that has one, each
// from the value kind of the point's type. A compile error is returned; the
// callers have already checked the document with CheckDocument, so it is a
// programming error. The edge_counter state lives in
// dataDir/inputs/{input id}/transform-state.json; an empty dataDir keeps no
// state. A nil logger means slog.Default().
func NewProcessor(cfg config.Input, dataDir string, clk clock.Clock, logger *slog.Logger) (*Processor, error) {
	if logger == nil {
		logger = slog.Default()
	}

	p := &Processor{
		inputID: cfg.ID,
		clk:     clk,
		log:     logger,
		rearm:   make(chan struct{}, 1),
		byKey:   map[pointKey]*pointChain{},
		errLog:  &errorLog{log: logger, inputID: cfg.ID, seen: map[errorKey]struct{}{}},
	}

	var state *stateFile
	if dataDir != "" {
		state = loadStateFile(filepath.Join(dataDir, "inputs", cfg.ID), cfg.ID, clk, logger)
	}

	for _, d := range cfg.Devices {
		for _, pt := range d.Points {
			if len(pt.Transforms) == 0 {
				continue
			}

			var factory StateFactory
			if state != nil {
				factory = state.factory(d.Device, pt.Signal)
			}

			chain, err := Compile(pt.Transforms, KindOf(pt.Type), factory)
			if err != nil {
				return nil, fmt.Errorf("transform: input %s, device %s, signal %s: %w", cfg.ID, d.Device, pt.Signal, err)
			}

			pc := &pointChain{key: pointKey{device: d.Device, signal: pt.Signal}, chain: chain}
			p.points = append(p.points, pc)
			p.byKey[pc.key] = pc
		}
	}

	if state != nil {
		state.prune()
		p.state = state
	}

	return p, nil
}

// Wrap returns the emit the input calls: it applies the chain of the
// sample's point and passes the results to emit. Device events, and samples
// of a point without a chain, pass through untouched; a death first tells the
// chains of its device (ruling R14), and what they release is emitted before
// it. The deadline goroutine emits through the emit of the last Wrap. After
// Close returns, the returned function emits nothing.
func (p *Processor) Wrap(emit func(model.Record)) func(model.Record) {
	p.mu.Lock()
	p.downstream = emit
	p.mu.Unlock()

	return func(r model.Record) { p.process(emit, r) }
}

func (p *Processor) process(emit func(model.Record), r model.Record) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	if r.Event != nil && r.Event.Kind == model.Death {
		p.deviceDownLocked(emit, r.Event.Device)
	}

	var pc *pointChain
	if r.Sample != nil {
		pc = p.byKey[pointKey{device: r.Sample.Device, signal: r.Sample.Signal}]
	}

	if pc == nil {
		emit(r)

		return
	}

	p.applyLocked(emit, pc, func() ([]model.Sample, []error) { return pc.chain.Apply(*r.Sample) })
}

// deviceDownLocked tells the chains of device that it died (ruling R14) and
// emits what they release, before the death is forwarded. The caller holds
// p.mu.
func (p *Processor) deviceDownLocked(emit func(model.Record), device string) {
	for _, pc := range p.points {
		if pc.key.device == device {
			p.applyLocked(emit, pc, pc.chain.deviceDown)
		}
	}
}

// applyLocked runs one operation on a chain, reports its errors, emits what
// it produced and wakes the deadline goroutine when the chain's deadline
// moved. The caller holds p.mu.
func (p *Processor) applyLocked(emit func(model.Record), pc *pointChain, op func() ([]model.Sample, []error)) {
	before, hadBefore := pc.chain.Deadline()
	out, errs := op()
	p.reportLocked(pc.key, errs)

	for _, s := range out {
		emit(model.SampleRecord(s))
	}

	if after, hasAfter := pc.chain.Deadline(); hasAfter != hadBefore || !after.Equal(before) {
		p.wakeLocked()
	}
}

// wakeLocked makes the deadline goroutine read the deadlines again.
func (p *Processor) wakeLocked() {
	select {
	case p.rearm <- struct{}{}:
	default:
	}
}

// Start writes the state file when NewProcessor dropped the counters of
// steps no chain uses any more (ruling R11), then starts the deadline
// goroutine. A failed write is logged; the next Save retries it. The
// goroutine runs until Close, or until ctx is done; a second Start, or a
// Start after Close, does nothing.
func (p *Processor) Start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started || p.closed {
		return
	}

	if p.state != nil {
		if err := p.state.persistPruned(); err != nil {
			p.log.Error("transform: the state file cannot be written; dropped counters stay in it until the next count",
				"input", p.inputID, "error", err)
		}
	}

	ctx, p.cancel = context.WithCancel(ctx)
	p.done = make(chan struct{})
	p.started = true

	go p.run(ctx, p.done)
}

// Close stops the deadline goroutine and waits for it. After Close returns
// the processor emits nothing. The runtime errors counted and not yet logged
// are logged in a last summary. Close may be called more than once.
func (p *Processor) Close() {
	p.mu.Lock()
	already := p.closed
	p.closed = true
	cancel, done := p.cancel, p.done

	if !already {
		p.errLog.summary(p.clk.Now())
	}
	p.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
}

// Errors returns the number of runtime errors of the chains since
// NewProcessor.
func (p *Processor) Errors() uint64 { return p.errors.Load() }

// run waits on the earliest deadline of the chains and of the error log, and
// fires the due ones.
func (p *Processor) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)

	for ctx.Err() == nil {
		timer, due, stop := p.arm()
		if stop {
			return
		}

		if due {
			p.fire()

			continue
		}

		var fired <-chan time.Time
		if timer != nil {
			fired = timer.C()
		}

		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}

			return
		case <-p.rearm:
			if timer != nil {
				timer.Stop()
			}
		case <-fired:
			p.fire()
		}
	}
}

// arm returns a timer for the earliest deadline, due when that deadline has
// already passed, or stop when the processor is closed.
func (p *Processor) arm() (timer clock.Timer, due, stop bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.armedOK = false

	if p.closed {
		return nil, false, true
	}

	// A wake-up sent before this point is stale: the deadlines read below
	// already include the Apply that sent it.
	select {
	case <-p.rearm:
	default:
	}

	var (
		earliest time.Time
		found    bool
	)

	for _, pc := range p.points {
		if d, ok := pc.chain.Deadline(); ok && (!found || d.Before(earliest)) {
			earliest, found = d, true
		}
	}

	if d, ok := p.errLog.deadline(); ok && (!found || d.Before(earliest)) {
		earliest, found = d, true
	}

	if !found {
		return nil, false, false
	}

	wait := earliest.Sub(p.clk.Now())
	if wait <= 0 {
		return nil, true, false
	}

	// The clock may move between Now and NewTimer, which would arm the timer
	// past the deadline: a deadline reached meanwhile is due at once.
	timer = p.clk.NewTimer(wait)
	if !p.clk.Now().Before(earliest) {
		timer.Stop()

		return nil, true, false
	}

	p.armed, p.armedOK = earliest, true

	return timer, false, false
}

// fire logs the error summary when it is due, then lets the due chains emit
// what is due now, in configuration order.
func (p *Processor) fire() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	now := p.clk.Now()
	p.errLog.flush(now)

	for _, pc := range p.points {
		if d, ok := pc.chain.Deadline(); !ok || d.After(now) {
			continue
		}

		out, errs := pc.chain.Fire(now)
		p.reportLocked(pc.key, errs)

		if p.downstream == nil {
			continue
		}

		for _, s := range out {
			p.downstream(model.SampleRecord(s))
		}
	}
}

// reportLocked counts the runtime errors of a chain and logs them, rate
// limited. The caller holds p.mu.
func (p *Processor) reportLocked(key pointKey, errs []error) {
	if len(errs) == 0 {
		return
	}

	p.errors.Add(uint64(len(errs)))

	before, hadBefore := p.errLog.deadline()

	now := p.clk.Now()
	for _, err := range errs {
		p.errLog.report(now, key, err)
	}

	if after, hasAfter := p.errLog.deadline(); hasAfter && (!hadBefore || after.Before(before)) {
		p.wakeLocked()
	}
}

// errorLogWindow is the shortest interval between two summaries of the
// runtime errors of one processor.
const errorLogWindow = time.Minute

type errorKey struct {
	device, signal string
	// position is the 1-based step position; 0 is the chain exit.
	position int
}

// errorLog keeps a failing chain from logging once per sample. The first
// runtime error of each device, signal and step position in an episode is
// logged in full at WARN; later ones are only counted, and the count is
// logged in one summary at most once per errorLogWindow, measured on the
// processor's clock. errorLogWindow without any error ends the episode: the
// errors still counted are summarised and the next error is logged in full
// again. The processor's deadline goroutine wakes at that end (deadline), so
// the closing summary does not wait for another error or Close. Nothing
// logged carries a sample value. The owner holds the processor's mutex
// around every call. Same shape as the buffer's failure log.
type errorLog struct {
	log     *slog.Logger
	inputID string
	// start is when the current window began.
	start time.Time
	// last is when the last error was reported.
	last time.Time
	// seen holds the keys logged in full in this episode.
	seen map[errorKey]struct{}
	// suppressed counts the errors counted and not logged since the last
	// summary.
	suppressed uint64
}

func (l *errorLog) report(now time.Time, key pointKey, err error) {
	l.flush(now)

	l.last = now

	ek := errorKey{device: key.device, signal: key.signal}

	var stepErr *StepError
	if errors.As(err, &stepErr) {
		ek.position = stepErr.Position
	}

	if _, seen := l.seen[ek]; seen {
		l.suppressed++

		return
	}

	if len(l.seen) == 0 {
		l.start = now
	}

	l.seen[ek] = struct{}{}

	if stepErr == nil {
		l.log.Warn("transform: a sample left the chain with an unexpected value kind and is sent bad",
			"input", l.inputID, "device", key.device, "signal", key.signal)

		return
	}

	l.log.Warn("transform: runtime error, the sample is sent bad",
		"input", l.inputID, "device", key.device, "signal", key.signal,
		"step", stepErr.Position, "transform", stepErr.Name, "error", stepErr.Err.Error())
}

// deadline is when the episode ends if no error comes, while errors are
// counted and not yet logged: flush then logs their summary.
func (l *errorLog) deadline() (time.Time, bool) {
	if l.suppressed == 0 {
		return time.Time{}, false
	}

	return l.last.Add(errorLogWindow), true
}

// flush logs the summary once the window has passed, and ends the episode
// after a quiet errorLogWindow.
func (l *errorLog) flush(now time.Time) {
	if len(l.seen) == 0 {
		return
	}

	quiet := now.Sub(l.last) >= errorLogWindow
	if !quiet && now.Sub(l.start) < errorLogWindow {
		return
	}

	l.summary(now)

	if quiet {
		clear(l.seen)
	}
}

// summary logs the count of the errors not logged, if any, and opens a new
// window.
func (l *errorLog) summary(now time.Time) {
	if l.suppressed == 0 {
		return
	}

	l.log.Warn("transform: runtime errors repeated since the last report",
		"input", l.inputID, "suppressed", l.suppressed, "window_seconds", int(now.Sub(l.start).Seconds()))
	l.suppressed = 0
	l.start = now
}
