// Package supervisor runs the components of the applied configuration
// document: it applies a new document by difference, rolls back to the
// previous one when a component does not start, and reports the state of
// every input and output (spec 4 supervisor, 5.4).
package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/swolley/laraplate-edge/internal/buffer"
	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/envelope"
	"github.com/swolley/laraplate-edge/internal/input"
	"github.com/swolley/laraplate-edge/internal/model"
	"github.com/swolley/laraplate-edge/internal/output"
	"github.com/swolley/laraplate-edge/internal/pipeline"
)

// DefaultStartTimeout bounds the start of one input (ruling R10).
const DefaultStartTimeout = 30 * time.Second

// Results of an Apply, as reported to the backend.
const (
	// ResultApplied: the document runs.
	ResultApplied = "applied"
	// ResultRejected: the document was refused before anything changed.
	ResultRejected = "rejected"
	// ResultRolledBack: a component did not start; the previous document
	// runs again.
	ResultRolledBack = "rolled_back"
)

// Component states of Status besides the output sending states.
const (
	// InputRunning is an input that started.
	InputRunning = "running"
	// StateFailed is a component of the applied document that could not be
	// started again during a rollback.
	StateFailed = "failed"
)

// ErrClosed is returned by Apply after Close.
var ErrClosed = errors.New("supervisor: closed")

// Outcome is the result of an Apply with its messages: what the backend
// receives as outcome and errors. Messages name paths, never values.
type Outcome struct {
	// Result is ResultApplied, ResultRejected or ResultRolledBack.
	Result string
	// Errors explain a rejection or a rollback.
	Errors []string
}

// Config configures a Supervisor.
type Config struct {
	// DataDir holds the output buffers (outputs/{id}) and the last valid
	// document.
	DataDir string
	// Store keeps the last valid document; nil means config.NewStore(DataDir).
	Store *config.Store
	// Drivers builds the inputs, by driver name.
	Drivers map[string]input.Factory
	// Resolver resolves the output tokens.
	Resolver output.SecretResolver
	// Clock drives every timer; nil means clock.Real().
	Clock clock.Clock
	// Logger receives the logs; nil means slog.Default().
	Logger *slog.Logger
	// StartTimeout bounds the start of an input; zero means
	// DefaultStartTimeout.
	StartTimeout time.Duration
	// HTTPClient is a test seam passed to the HTTP outputs. Production code
	// leaves it nil: the outputs then use their verifying default client.
	HTTPClient *http.Client
}

// Supervisor owns the running inputs and outputs and every goroutine they
// need. Apply, Close, Status, ReloadCredentials and AppliedVersion are safe
// for concurrent use; Apply calls are serialized.
type Supervisor struct {
	dataDir  string
	store    *config.Store
	drivers  map[string]input.Factory
	resolver output.SecretResolver
	clk      clock.Clock
	log      *slog.Logger
	timeout  time.Duration
	client   *http.Client
	router   *pipeline.Router

	// ctx is the lifetime of every component; wg counts every goroutine the
	// supervisor starts.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// trace is a test hook called, from Apply, at each start step.
	trace func(step string)

	applyMu sync.Mutex
	closed  bool

	mu        sync.Mutex
	applied   bool
	version   int64
	inputs    map[string]*runningInput
	outputs   map[string]*runningOutput
	failedIn  map[string]failure
	failedOut map[string]failure
}

type runningInput struct {
	cfg    config.Input
	in     input.Input
	cancel context.CancelFunc
	inst   *inputInstance
}

// inputInstance is one start of an input, as the router sees it. It
// registers the input on its first record or when Start returns, whichever
// comes first, and routes its records; retire ends both, before the input is
// closed. mu makes the live check, the registration and each routed record
// one step with retire, so after retire nothing is registered or routed for
// this instance, and its router registration, owned by its token, is removed
// without touching one of a later instance of the same id (ruling R50).
//
// Lock order: inputInstance.mu, then the router's locks (Router.mu, then a
// hold queue's), then an output buffer's lock inside Append or AddLoss.
// Supervisor.mu is taken before a buffer's lock (Status reads the buffer
// figures under it) and never while an instance's or the router's lock is
// held. No lock is ever taken in the other direction.
type inputInstance struct {
	router *pipeline.Router
	cfg    config.Input

	mu         sync.Mutex
	live       bool
	registered bool
	token      pipeline.InputToken
}

// emit routes rec once the instance is registered; after retire it drops it.
func (i *inputInstance) emit(rec model.Record) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.registerLocked() {
		i.router.Emit(rec)
	}
}

// register registers the instance's devices and sends their births, once,
// unless it is retired.
func (i *inputInstance) register() {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.registerLocked()
}

func (i *inputInstance) registerLocked() bool {
	if !i.live {
		return false
	}
	if !i.registered {
		i.registered = true
		i.token = i.router.SetInput(i.cfg)
		i.router.BirthInput(i.cfg.ID)
	}

	return true
}

// retire ends the instance's emits and removes its own registration. Later
// calls do nothing.
func (i *inputInstance) retire() {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.live = false
	if i.registered {
		i.registered = false
		i.router.RemoveInput(i.cfg.ID, i.token)
	}
}

type runningOutput struct {
	cfg    config.Output
	dir    string
	buf    *buffer.Buffer
	out    *output.HTTP
	cancel context.CancelFunc
	done   chan struct{}
}

type failure struct {
	kind string
	err  string
	// output is the configuration of a failed output, whose route its
	// held records follow.
	output config.Output
}

// New returns a supervisor running nothing.
func New(cfg Config) (*Supervisor, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("supervisor: no data directory")
	}
	if cfg.Resolver == nil {
		return nil, errors.New("supervisor: no secret resolver")
	}

	s := &Supervisor{
		dataDir:   cfg.DataDir,
		store:     cfg.Store,
		drivers:   cfg.Drivers,
		resolver:  cfg.Resolver,
		clk:       cfg.Clock,
		log:       cfg.Logger,
		timeout:   cfg.StartTimeout,
		client:    cfg.HTTPClient,
		inputs:    map[string]*runningInput{},
		outputs:   map[string]*runningOutput{},
		failedIn:  map[string]failure{},
		failedOut: map[string]failure{},
	}
	if s.store == nil {
		s.store = config.NewStore(cfg.DataDir)
	}
	if s.clk == nil {
		s.clk = clock.Real()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.timeout <= 0 {
		s.timeout = DefaultStartTimeout
	}
	s.router = pipeline.NewRouter(s.clk, s.log)
	s.ctx, s.cancel = context.WithCancel(context.Background())

	return s, nil
}

// plan is what an Apply changes: components to stop (changed or removed,
// by id) and to start (changed or added, by index in the new document).
type plan struct {
	stopInputs     []string
	stopOutputs    []string
	removedOutputs map[string]bool
	startInputs    []int
	startOutputs   []int
	kept           int
}

// outputStop says what follows the stop of an output.
type outputStop int

const (
	// stopForRestart: the output starts again; the router holds its records.
	stopForRestart outputStop = iota
	// stopRemoved: the output leaves the configuration; its directory is
	// kept, with a warning when it holds unsent records.
	stopRemoved
	// stopShutdown: the supervisor closes.
	stopShutdown
)

// journal records what an Apply did, so that a rollback can undo it.
type journal struct {
	stoppedInputs  []config.Input
	stoppedOutputs []config.Output
	startedInputs  []string
	startedOutputs []string
	// failedIn and failedOut are the failed components before the Apply: a
	// rollback puts back those this Apply started.
	failedIn  map[string]failure
	failedOut map[string]failure
}

// Apply makes doc the running document. It refuses, with ResultRejected and
// nothing changed, a document the build cannot run (config.CheckBuild, a
// driver missing from the registry, an input its driver cannot build) or
// whose version is lower than the applied one (a missing version counts as
// 0; ruling R9). Otherwise it applies by difference, comparing each
// component's Raw by id: unchanged inputs and outputs keep running untouched,
// changed ones are restarted, removed ones stop (an output's directory is
// kept) and added ones start. When a component fails to start, or an input
// does not start within the start timeout, what this Apply started is
// stopped and what it stopped is started again: ResultRolledBack with the
// errors. When ctx is cancelled during a failed apply, the rollback still
// runs but there is no outcome: Apply returns the context error. On success
// the document is saved as the last valid one; the error is then only for
// that save failing, with ResultApplied.
func (s *Supervisor) Apply(ctx context.Context, doc config.Document) (Outcome, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	if s.closed {
		return Outcome{}, ErrClosed
	}

	if msgs := s.check(doc); len(msgs) > 0 {
		s.log.Warn("supervisor: configuration rejected", "version", doc.Version, "errors", msgs)

		return Outcome{Result: ResultRejected, Errors: msgs}, nil
	}

	p := s.diff(doc)
	built, msgs := s.build(doc, p)
	if len(msgs) > 0 {
		closeInputs(built)
		s.log.Warn("supervisor: configuration rejected", "version", doc.Version, "errors", msgs)

		return Outcome{Result: ResultRejected, Errors: msgs}, nil
	}

	j, errs := s.execute(ctx, doc, p, built)
	closeInputs(built)
	if len(errs) > 0 {
		errs = append(errs, s.rollback(j)...)
		if err := ctx.Err(); err != nil {
			// A shutdown is not an attempt: the document was never given its
			// chance, so there is no outcome to report (ruling R45).
			s.log.Warn("supervisor: configuration apply cancelled; the previous one runs", "version", doc.Version, "errors", errs)

			return Outcome{}, fmt.Errorf("supervisor: apply cancelled: %w", err)
		}
		s.log.Error("supervisor: configuration rolled back to the previous one", "version", doc.Version, "errors", errs)

		return Outcome{Result: ResultRolledBack, Errors: errs}, nil
	}

	s.mu.Lock()
	s.applied, s.version = true, doc.Version
	clear(s.failedIn)
	failedOut := sortedKeys(s.failedOut)
	clear(s.failedOut)
	s.mu.Unlock()

	// A failed output the document no longer has drops its held records;
	// one it has was started, which flushed them.
	for _, id := range failedOut {
		if !slices.ContainsFunc(doc.Outputs, func(o config.Output) bool { return o.ID == id }) {
			s.router.RemoveOutput(id, "output removed from the configuration")
		}
	}

	s.log.Info("supervisor: configuration applied", "version", doc.Version,
		"started", len(p.startInputs)+len(p.startOutputs),
		"stopped", len(p.stopInputs)+len(p.stopOutputs),
		"unchanged", p.kept)

	if err := s.store.SaveValid(doc); err != nil {
		return Outcome{Result: ResultApplied}, fmt.Errorf("supervisor: %w", err)
	}

	return Outcome{Result: ResultApplied}, nil
}

// check returns the reasons to refuse doc before anything is built.
func (s *Supervisor) check(doc config.Document) []string {
	var msgs []string

	s.mu.Lock()
	applied, version := s.applied, s.version
	s.mu.Unlock()
	if applied && doc.Version < version {
		msgs = append(msgs, fmt.Sprintf("version: %d is older than the applied version %d", doc.Version, version))
	}

	build := config.CheckBuild(doc)
	msgs = append(msgs, build...)
	if len(build) == 0 {
		for i, in := range doc.Inputs {
			if s.drivers[in.Driver] == nil {
				msgs = append(msgs, fmt.Sprintf("inputs.%d.driver: not available in this build", i))
			}
		}
	}

	return msgs
}

// diff compares doc with what runs. Only Apply changes the running maps, so
// reading them under mu once is enough.
func (s *Supervisor) diff(doc config.Document) plan {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := plan{removedOutputs: map[string]bool{}}

	inDoc := map[string]bool{}
	for i, in := range doc.Inputs {
		inDoc[in.ID] = true
		running := s.inputs[in.ID]
		if running != nil && bytes.Equal(running.cfg.Raw, in.Raw) {
			p.kept++

			continue
		}
		p.startInputs = append(p.startInputs, i)
		if running != nil {
			p.stopInputs = append(p.stopInputs, in.ID)
		}
	}
	for id := range s.inputs {
		if !inDoc[id] {
			p.stopInputs = append(p.stopInputs, id)
		}
	}

	outDoc := map[string]bool{}
	for i, out := range doc.Outputs {
		outDoc[out.ID] = true
		running := s.outputs[out.ID]
		if running != nil && bytes.Equal(running.cfg.Raw, out.Raw) {
			p.kept++

			continue
		}
		p.startOutputs = append(p.startOutputs, i)
		if running != nil {
			p.stopOutputs = append(p.stopOutputs, out.ID)
		}
	}
	for id := range s.outputs {
		if !outDoc[id] {
			p.stopOutputs = append(p.stopOutputs, id)
			p.removedOutputs[id] = true
		}
	}

	slices.Sort(p.stopInputs)
	slices.Sort(p.stopOutputs)

	return p
}

// build asks the drivers for every input to start. Nothing runs yet, so a
// driver refusing its configuration rejects the document.
func (s *Supervisor) build(doc config.Document, p plan) (map[int]input.Input, []string) {
	built := map[int]input.Input{}
	var msgs []string
	for _, i := range p.startInputs {
		cfg := doc.Inputs[i]
		in, err := s.drivers[cfg.Driver](cfg, s.clk)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("inputs.%d: %v", i, err))

			continue
		}
		built[i] = in
	}

	return built, msgs
}

// execute stops, then starts, what p says: inputs stop before outputs, so
// nothing is routed to an output being stopped, and outputs start before
// inputs, so a new input finds its outputs. It stops at the first failure.
// The inputs it starts are removed from built.
func (s *Supervisor) execute(ctx context.Context, doc config.Document, p plan, built map[int]input.Input) (journal, []string) {
	s.mu.Lock()
	j := journal{failedIn: maps.Clone(s.failedIn), failedOut: maps.Clone(s.failedOut)}
	s.mu.Unlock()
	for _, id := range p.stopInputs {
		j.stoppedInputs = append(j.stoppedInputs, s.stopInput(id))
	}
	for _, id := range p.stopOutputs {
		how := stopForRestart
		if p.removedOutputs[id] {
			how = stopRemoved
		}
		cfg, _ := s.stopOutput(id, how, config.Route{})
		j.stoppedOutputs = append(j.stoppedOutputs, cfg)
	}

	for _, i := range p.startOutputs {
		cfg := doc.Outputs[i]
		if err := s.startOutput(cfg); err != nil {
			return j, []string{fmt.Sprintf("outputs.%d: %v", i, err)}
		}
		j.startedOutputs = append(j.startedOutputs, cfg.ID)
	}
	for _, i := range p.startInputs {
		cfg := doc.Inputs[i]
		in := built[i]
		delete(built, i)
		if err := s.startInput(ctx, cfg, in); err != nil {
			return j, []string{fmt.Sprintf("inputs.%d: %v", i, err)}
		}
		j.startedInputs = append(j.startedInputs, cfg.ID)
	}

	return j, nil
}

// rollback stops what j started and starts again what j stopped. An output
// the failed document added is stopped like a removed one: its directory is
// kept. A previous component that does not start again is reported failed in
// Status, its error is returned and, for an output, the router keeps holding
// its records (ruling R48).
func (s *Supervisor) rollback(j journal) []string {
	for _, id := range slices.Backward(j.startedInputs) {
		s.stopInput(id)
		if f, ok := j.failedIn[id]; ok {
			s.restoreFailure(s.failedIn, id, f)
		}
	}
	for _, id := range slices.Backward(j.startedOutputs) {
		i := slices.IndexFunc(j.stoppedOutputs, func(o config.Output) bool { return o.ID == id })
		f, wasFailed := j.failedOut[id]
		switch {
		case i >= 0:
			// It starts again below with its previous configuration.
			_, _ = s.stopOutput(id, stopForRestart, j.stoppedOutputs[i].Route)
		case wasFailed:
			// The restored document has it failed: it is held again under
			// its previous configuration, with its previous error.
			_, _ = s.stopOutput(id, stopForRestart, f.output.Route)
			s.restoreFailure(s.failedOut, id, f)
		default:
			_, _ = s.stopOutput(id, stopRemoved, config.Route{})
		}
	}

	var errs []string
	for _, cfg := range j.stoppedOutputs {
		if err := s.startOutput(cfg); err != nil {
			errs = append(errs, fmt.Sprintf("rollback: output %s: %v", cfg.ID, err))
			s.restoreFailure(s.failedOut, cfg.ID, failure{kind: cfg.Type, err: err.Error(), output: cfg})
			// The router keeps holding its records until a later document
			// starts or removes it (ruling R48).
		}
	}
	for _, cfg := range j.stoppedInputs {
		err := s.restartInput(cfg)
		if err != nil {
			errs = append(errs, fmt.Sprintf("rollback: input %s: %v", cfg.ID, err))
			s.setFailed(s.failedIn, cfg.ID, cfg.Driver, err)
		}
	}

	return errs
}

func (s *Supervisor) restartInput(cfg config.Input) error {
	factory := s.drivers[cfg.Driver]
	if factory == nil {
		return errors.New("driver not available in this build")
	}
	in, err := factory(cfg, s.clk)
	if err != nil {
		return err
	}

	return s.startInput(s.ctx, cfg, in)
}

func (s *Supervisor) setFailed(m map[string]failure, id, kind string, err error) {
	s.restoreFailure(m, id, failure{kind: kind, err: err.Error()})
}

func (s *Supervisor) restoreFailure(m map[string]failure, id string, f failure) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m[id] = f
}

// startOutput opens the output's buffer, builds its batcher, which loads the
// inflight batch and marks it sealed, and only then hands the buffer to the
// router: nothing is appended before the sealed records are protected from
// eviction (Task 5 review).
func (s *Supervisor) startOutput(cfg config.Output) error {
	dir := filepath.Join(s.dataDir, "outputs", cfg.ID)
	log := s.log.With("output", cfg.ID)

	buf, err := buffer.Open(dir, buffer.Options{
		MaxSize:       cfg.Buffer.MaxSize,
		MinFreeRatio:  cfg.Buffer.MinFreePercent / 100,
		FsyncInterval: cfg.Buffer.FsyncInterval,
		Clock:         s.clk,
		Logger:        log,
	})
	if err != nil {
		return err
	}
	s.step("buffer " + cfg.ID)

	batcher, err := output.NewBatcher(buf, output.NewInflight(dir), envelope.NewIDGenerator(s.clk), s.clk, output.BatchLimits{
		MaxSamples: cfg.Batch.MaxSamples,
		MaxBody:    int(cfg.Batch.MaxBody),
		MaxWait:    cfg.Batch.MaxWait,
	}, log)
	if err != nil {
		_ = buf.Close()

		return err
	}
	s.step("batcher " + cfg.ID)

	id := cfg.ID
	out, err := output.NewHTTP(output.HTTPConfig{
		ID:          id,
		URL:         cfg.URL,
		Token:       cfg.Token,
		Resolver:    s.resolver,
		Batcher:     batcher,
		Clock:       s.clk,
		Logger:      s.log,
		OnReconnect: func() { s.router.BirthTo(id) },
		Client:      s.client,
	})
	if err != nil {
		_ = buf.Close()

		return err
	}

	runCtx, cancel := context.WithCancel(s.ctx)
	ro := &runningOutput{cfg: cfg, dir: dir, buf: buf, out: out, cancel: cancel, done: make(chan struct{})}
	s.wg.Go(func() {
		defer close(ro.done)
		_ = out.Run(runCtx)
	})

	s.router.SetOutput(id, cfg.Route, buf)
	s.step("route " + id)

	s.mu.Lock()
	s.outputs[id] = ro
	delete(s.failedOut, id)
	s.mu.Unlock()

	return nil
}

// stopOutput stops routing to the output, or holds its records when it
// restarts (ruling R46), stops its sending and closes its buffer, which syncs
// it. The hold routes with holdRoute, the route of the configuration being
// restored, or the output's own when holdRoute has no pattern. A removed
// output with unsent records leaves a warning: its directory is kept.
func (s *Supervisor) stopOutput(id string, how outputStop, holdRoute config.Route) (config.Output, error) {
	s.mu.Lock()
	ro := s.outputs[id]
	delete(s.outputs, id)
	s.mu.Unlock()

	if how == stopForRestart {
		if len(holdRoute.Include) == 0 {
			holdRoute = ro.cfg.Route
		}
		s.router.Hold(id, holdRoute)
	} else {
		s.router.RemoveOutput(id, "output stopped")
	}
	ro.cancel()
	<-ro.done

	if how == stopRemoved {
		if n := ro.buf.Records(); n > 0 {
			s.log.Warn("supervisor: output removed; its unsent records remain in its directory", "output", id, "records", n, "dir", ro.dir)
		}
	}

	err := ro.buf.Close()
	if err != nil {
		s.log.Error("supervisor: cannot close the output buffer", "output", id, "error", err)
	}

	return ro.cfg, err
}

// startInput starts in within the start timeout. Its devices are registered
// with the router, and their births sent, once the input is live: before its
// first record or when Start returns, whichever comes first. Until then an
// output reconnecting sends no birth for them. A failed or abandoned start is
// retired before the input is closed: a goroutine it left behind cannot
// register it or send a record, and its late cleanup cannot unregister the
// instance a rollback started meanwhile.
func (s *Supervisor) startInput(ctx context.Context, cfg config.Input, in input.Input) error {
	lifeCtx, cancel := context.WithCancel(s.ctx)
	inst := &inputInstance{router: s.router, cfg: cfg, live: true}

	err := s.startBounded(ctx, cancel, func() error { return in.Start(lifeCtx, inst.emit) }, func() {
		inst.retire()
		if err := in.Close(); err != nil {
			s.log.Warn("supervisor: cannot close an input that did not start", "input", cfg.ID, "error", err)
		}
	})
	if err != nil {
		inst.retire()
		cancel()

		return err
	}

	inst.register()
	s.step("input " + cfg.ID)

	s.mu.Lock()
	s.inputs[cfg.ID] = &runningInput{cfg: cfg, in: in, cancel: cancel, inst: inst}
	delete(s.failedIn, cfg.ID)
	s.mu.Unlock()

	return nil
}

// startBounded runs start in a goroutine and waits for it at most the start
// timeout, or until ctx is done. When start fails, cleanup runs at once; when
// the wait gives up, cancelStart is called and cleanup runs once start
// returns, in the goroutine, which the supervisor's wait group owns.
func (s *Supervisor) startBounded(ctx context.Context, cancelStart context.CancelFunc, start func() error, cleanup func()) error {
	res := make(chan error)
	abandon := make(chan struct{})

	timer := s.clk.NewTimer(s.timeout)
	defer timer.Stop()

	s.wg.Go(func() {
		err := start()
		select {
		case res <- err:
		case <-abandon:
			cleanup()
		}
	})

	select {
	case err := <-res:
		if err != nil {
			cleanup()
		}

		return err
	case <-timer.C():
		close(abandon)
		cancelStart()

		return fmt.Errorf("did not start within %s", s.timeout)
	case <-ctx.Done():
		close(abandon)
		cancelStart()

		return fmt.Errorf("start cancelled: %w", ctx.Err())
	}
}

// stopInput retires the input, which forgets its devices, then stops it:
// nothing it emits while it closes is routed.
func (s *Supervisor) stopInput(id string) config.Input {
	s.mu.Lock()
	ri := s.inputs[id]
	delete(s.inputs, id)
	s.mu.Unlock()

	ri.inst.retire()
	ri.cancel()
	if err := ri.in.Close(); err != nil {
		s.log.Warn("supervisor: cannot close an input", "input", id, "error", err)
	}

	return ri.cfg
}

func (s *Supervisor) step(name string) {
	if s.trace != nil {
		s.trace(name)
	}
}

func closeInputs(built map[int]input.Input) {
	for _, in := range built {
		_ = in.Close()
	}
}

// Close stops every input, then every output, syncing and closing the
// buffers, and waits for every goroutine of the supervisor. The records held
// for an output left failed by a rollback are discarded at shutdown, once the
// inputs are stopped, with a warning per priority giving the count and the ts
// range. Later calls return nil.
func (s *Supervisor) Close() error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	s.mu.Lock()
	inputs := sortedKeys(s.inputs)
	outputs := sortedKeys(s.outputs)
	failedOut := sortedKeys(s.failedOut)
	s.mu.Unlock()

	for _, id := range inputs {
		s.step("stop input " + id)
		s.stopInput(id)
	}
	for _, id := range failedOut {
		s.step("discard output " + id)
		s.router.RemoveOutput(id, "agent shutting down")
	}

	var errs []error
	for _, id := range outputs {
		if _, err := s.stopOutput(id, stopShutdown, config.Route{}); err != nil {
			errs = append(errs, fmt.Errorf("supervisor: output %s: %w", id, err))
		}
	}

	s.cancel()
	s.wg.Wait()

	return errors.Join(errs...)
}

// ReloadCredentials makes every running output resolve its token again
// before its next attempt and ends a suspension at once (SIGHUP).
func (s *Supervisor) ReloadCredentials() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, ro := range s.outputs {
		ro.out.ReloadCredentials()
	}
}

// AppliedVersion returns the version of the applied document, 0 before any
// or when it has none.
func (s *Supervisor) AppliedVersion() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.version
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	return keys
}
