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
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
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
	live   *atomic.Bool
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

// journal records what an Apply did, so that a rollback can undo it.
type journal struct {
	stoppedInputs  []config.Input
	stoppedOutputs []config.Output
	startedInputs  []string
	startedOutputs []string
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
// errors. On success the document is saved as the last valid one; the error
// is only for that save failing, with ResultApplied.
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
		s.log.Error("supervisor: configuration rolled back to the previous one", "version", doc.Version, "errors", errs)

		return Outcome{Result: ResultRolledBack, Errors: errs}, nil
	}

	s.mu.Lock()
	s.applied, s.version = true, doc.Version
	clear(s.failedIn)
	clear(s.failedOut)
	s.mu.Unlock()

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
	var j journal
	for _, id := range p.stopInputs {
		j.stoppedInputs = append(j.stoppedInputs, s.stopInput(id))
	}
	for _, id := range p.stopOutputs {
		cfg, _ := s.stopOutput(id, p.removedOutputs[id])
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
// Status; its error is returned.
func (s *Supervisor) rollback(j journal) []string {
	for _, id := range slices.Backward(j.startedInputs) {
		s.stopInput(id)
	}
	for _, id := range slices.Backward(j.startedOutputs) {
		added := !slices.ContainsFunc(j.stoppedOutputs, func(o config.Output) bool { return o.ID == id })
		_, _ = s.stopOutput(id, added)
	}

	var errs []string
	for _, cfg := range j.stoppedOutputs {
		if err := s.startOutput(cfg); err != nil {
			errs = append(errs, fmt.Sprintf("rollback: output %s: %v", cfg.ID, err))
			s.setFailed(s.failedOut, cfg.ID, cfg.Type, err)
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
	s.mu.Lock()
	defer s.mu.Unlock()

	m[id] = failure{kind: kind, err: err.Error()}
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

// stopOutput stops routing to the output, stops its sending and closes its
// buffer, which syncs it. A removed output with unsent records leaves a
// warning: its directory is kept.
func (s *Supervisor) stopOutput(id string, removed bool) (config.Output, error) {
	s.mu.Lock()
	ro := s.outputs[id]
	delete(s.outputs, id)
	s.mu.Unlock()

	s.router.RemoveOutput(id)
	ro.cancel()
	<-ro.done

	if removed {
		if n := ro.buf.Stats().Records; n > 0 {
			s.log.Warn("supervisor: output removed; its unsent records remain in its directory", "output", id, "records", n, "dir", ro.dir)
		}
	}

	err := ro.buf.Close()
	if err != nil {
		s.log.Error("supervisor: cannot close the output buffer", "output", id, "error", err)
	}

	return ro.cfg, err
}

// startInput starts in within the start timeout. Its births are sent once,
// before its first record or when Start returns, whichever comes first.
func (s *Supervisor) startInput(ctx context.Context, cfg config.Input, in input.Input) error {
	lifeCtx, cancel := context.WithCancel(s.ctx)
	live := &atomic.Bool{}
	live.Store(true)

	var once sync.Once
	births := func() { once.Do(func() { s.router.BirthInput(cfg.ID) }) }
	emit := func(rec model.Record) {
		if !live.Load() {
			return
		}
		births()
		s.router.Emit(rec)
	}

	s.router.SetInput(cfg)
	err := s.startBounded(ctx, cancel, func() error { return in.Start(lifeCtx, emit) }, func() {
		if err := in.Close(); err != nil {
			s.log.Warn("supervisor: cannot close an input that did not start", "input", cfg.ID, "error", err)
		}
	})
	if err != nil {
		live.Store(false)
		cancel()
		s.router.RemoveInput(cfg.ID)

		return err
	}

	births()
	s.step("input " + cfg.ID)

	s.mu.Lock()
	s.inputs[cfg.ID] = &runningInput{cfg: cfg, in: in, cancel: cancel, live: live}
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

// stopInput stops the input and forgets its devices.
func (s *Supervisor) stopInput(id string) config.Input {
	s.mu.Lock()
	ri := s.inputs[id]
	delete(s.inputs, id)
	s.mu.Unlock()

	ri.cancel()
	if err := ri.in.Close(); err != nil {
		s.log.Warn("supervisor: cannot close an input", "input", id, "error", err)
	}
	ri.live.Store(false)
	s.router.RemoveInput(id)

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
// buffers, and waits for every goroutine of the supervisor. Later calls
// return nil.
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
	s.mu.Unlock()

	for _, id := range inputs {
		s.stopInput(id)
	}

	var errs []error
	for _, id := range outputs {
		if _, err := s.stopOutput(id, false); err != nil {
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
