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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlaraplate/laraedge/internal/buffer"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/envelope"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/output"
	"github.com/getlaraplate/laraedge/internal/pipeline"
	"github.com/getlaraplate/laraedge/internal/plugin"
	"github.com/getlaraplate/laraedge/internal/transform"
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
	// DataDir holds the output buffers (outputs/{id}), the transform state of
	// the inputs (inputs/{id}) and the last valid document.
	DataDir string
	// Store keeps the last valid document; nil means config.NewStore(DataDir).
	Store *config.Store
	// Drivers builds the inputs, by driver name. The inputs of a plugin,
	// driver plugin:<id>, are built by the plugin of that id the applied
	// document declares (decision H4), not from this registry.
	Drivers map[string]input.Factory
	// Resolver resolves the output tokens and the secrets of the plugin
	// inputs.
	Resolver output.SecretResolver
	// ConfigDir is the directory of the configuration file, against which
	// relative plugin paths resolve; "" for a provider without a file.
	ConfigDir string
	// Provider is the bootstrap's provider name; with the laraplate provider
	// plugins are disabled unless PluginsDir is set.
	Provider string
	// PluginsDir is the bootstrap's plugins_dir, "" when unset: the local
	// trust anchor every plugin binary must lie inside.
	PluginsDir string
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
	dataDir    string
	store      *config.Store
	drivers    map[string]input.Factory
	resolver   output.SecretResolver
	configDir  string
	provider   string
	pluginsDir string
	clk        clock.Clock
	log        *slog.Logger
	timeout    time.Duration
	client     *http.Client
	router     *pipeline.Router
	// taps holds the running processors, which the input instances offer
	// their records to (decision Q4). It is replaced, never modified, under
	// mu; the emit path reads it without a lock.
	taps atomic.Pointer[[]*plugin.Processor]

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
	// plugins holds the started plugins, by id; failedPlugins those a
	// rollback could not start again.
	plugins       map[string]*runningPlugin
	failedPlugins map[string]failure
	// processors holds the started processors, by id; failedProc those a
	// rollback could not start again.
	processors map[string]*runningProcessor
	failedProc map[string]failure
	// appliedInputs holds the input ids of the applied document.
	appliedInputs map[string]bool
	// transformErrors holds, by input id, the runtime errors counted by the
	// closed processors of that input, so the count reported never goes
	// back when an input restarts.
	transformErrors map[string]uint64
}

// runningInput is a started input with its transform processor. The
// processor wraps the instance's emit: its lock comes before the instance's
// in the lock order (ruling R2).
type runningInput struct {
	cfg    config.Input
	in     input.Input
	cancel context.CancelFunc
	inst   *inputInstance
	proc   *transform.Processor
}

// inputInstance is one start of an input, as the router sees it. It
// registers the input on its first record or when Start returns, whichever
// comes first, and routes its records; retire ends both, before the input is
// closed. mu makes the live check, the registration and each routed record
// one step with retire, so after retire nothing is registered or routed for
// this instance, and its router registration, owned by its token, is removed
// without touching one of a later instance of the same id (ruling R50).
//
// Lock order: the input's transform processor, then inputInstance.mu, then
// the router's locks (Router.mu, then a hold queue's), then an output
// buffer's lock inside Append or AddLoss.
// The processors are offered a routed record after inputInstance.mu is
// released, through Supervisor.taps, read without a lock; Offer never
// blocks (decision Q4).
// Supervisor.mu is taken before a buffer's lock (Status reads the buffer
// figures under it) and before a plugin host's lock (Status reads the plugin
// states under it), and never while an instance's or the router's lock is
// held. No lock is ever taken in the other direction.
type inputInstance struct {
	router *pipeline.Router
	cfg    config.Input
	// taps are the processors offered a copy of every record routed, nil
	// for the instance of a processor: derived records are not observed.
	taps *atomic.Pointer[[]*plugin.Processor]

	mu         sync.Mutex
	live       bool
	registered bool
	token      pipeline.InputToken
}

// emit routes rec once the instance is registered; after retire it drops
// it. A routed record, as the input's transforms left it, is then offered
// to the running processors, after the births its registration sent, with
// no lock of the instance or the supervisor held: Offer never blocks, and
// machine data never waits for a processor (decision Q4).
func (i *inputInstance) emit(rec model.Record) {
	births, ok := i.route(rec)
	if !ok {
		return
	}
	i.offer(births)
	i.offer([]model.Record{rec})
}

// offer offers recs to the running processors. The caller holds no lock.
func (i *inputInstance) offer(recs []model.Record) {
	if i.taps == nil || len(recs) == 0 {
		return
	}
	if taps := i.taps.Load(); taps != nil {
		for _, p := range *taps {
			for _, rec := range recs {
				p.Offer(rec)
			}
		}
	}
}

// route routes rec unless the instance is retired, and reports whether it
// did, with the births its registration sent when rec registered it.
func (i *inputInstance) route(rec model.Record) ([]model.Record, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	births, ok := i.registerLocked()
	if !ok {
		return nil, false
	}
	i.router.Emit(rec)

	return births, true
}

// register registers the instance's devices and sends their births, once,
// unless it is retired; the births are then offered to the running
// processors, the instance's lock released.
func (i *inputInstance) register() {
	i.mu.Lock()
	births, _ := i.registerLocked()
	i.mu.Unlock()

	i.offer(births)
}

// registerLocked registers the instance on its first call while it is
// live, and returns the births it sent then, as records. It reports
// whether the instance is live. The caller holds i.mu.
func (i *inputInstance) registerLocked() ([]model.Record, bool) {
	if !i.live {
		return nil, false
	}
	if i.registered {
		return nil, true
	}
	i.registered = true
	i.token = i.router.SetInput(i.cfg)

	return birthRecords(i.router.BirthInput(i.cfg.ID)), true
}

// birthRecords returns births as records.
func birthRecords(births []model.DeviceEvent) []model.Record {
	recs := make([]model.Record, 0, len(births))
	for _, ev := range births {
		recs = append(recs, model.EventRecord(ev))
	}

	return recs
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

// runningPlugin is a plugin with its host. Its spec is the one the host was
// built from: a document changing the entry (Raw) restarts it (decision H5).
type runningPlugin struct {
	spec plugin.Spec
	host *plugin.Host
}

// runningOutput is a started output: an HTTP output, or a plugin output
// whose output.Loop sends through sender (ruling R3).
type runningOutput struct {
	cfg    config.Output
	dir    string
	buf    *buffer.Buffer
	out    outputLoop
	sender *plugin.OutputSender
	cancel context.CancelFunc
	done   chan struct{}
}

// outputLoop is what the supervisor uses of a running output's delivery:
// *output.HTTP and *output.Loop.
type outputLoop interface {
	Run(ctx context.Context) error
	State() output.State
	LastConfirmation() time.Time
	LastError() (string, time.Time)
	ReloadCredentials()
}

// clockOffsetter is an output that estimates the backend clock (HTTP).
type clockOffsetter interface {
	ClockOffset() time.Duration
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
		dataDir:    cfg.DataDir,
		store:      cfg.Store,
		drivers:    cfg.Drivers,
		resolver:   cfg.Resolver,
		configDir:  cfg.ConfigDir,
		provider:   cfg.Provider,
		pluginsDir: cfg.PluginsDir,
		clk:        cfg.Clock,
		log:        cfg.Logger,
		timeout:    cfg.StartTimeout,
		client:     cfg.HTTPClient,
		inputs:     map[string]*runningInput{},
		outputs:    map[string]*runningOutput{},
		failedIn:   map[string]failure{},
		failedOut:  map[string]failure{},

		plugins:         map[string]*runningPlugin{},
		failedPlugins:   map[string]failure{},
		processors:      map[string]*runningProcessor{},
		failedProc:      map[string]failure{},
		transformErrors: map[string]uint64{},
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
// Plugins count as components: only the plugins a component of the
// document uses run.
type plan struct {
	stopInputs      []string
	stopOutputs     []string
	stopPlugins     []string
	stopProcessors  []string
	removedOutputs  map[string]bool
	startInputs     []int
	startOutputs    []int
	startPlugins    []int
	startProcessors []int
	kept            int
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
	stoppedInputs     []config.Input
	stoppedOutputs    []config.Output
	stoppedPlugins    []plugin.Spec
	stoppedProcessors []config.Processor
	startedInputs     []string
	startedOutputs    []string
	startedPlugins    []string
	startedProcessors []string
	// failedIn, failedOut, failedPlugins and failedProc are the failed
	// components before the Apply: a rollback puts back those this Apply
	// started.
	failedIn      map[string]failure
	failedOut     map[string]failure
	failedPlugins map[string]failure
	failedProc    map[string]failure
}

// Apply makes doc the running document. It refuses, with ResultRejected and
// nothing changed, a document the build cannot run (config.CheckBuild, a
// driver missing from the registry, a plugin whose files or manifest do not
// check, a transform chain that does not type check, an input its driver
// cannot build) or whose version is lower than the applied one (a missing
// version counts as 0; ruling R9). Otherwise it applies by difference,
// comparing each component's Raw by id: unchanged inputs and outputs keep
// running untouched, changed ones are restarted, removed ones stop (an
// output's directory is kept) and added ones start; processors likewise. A
// plugin runs while an input, a processor or an output of the document uses
// it: it starts before them, restarts with all of them when its entry or its
// binary changes, and stops after the last of them (decisions H5, Q6). A
// plugin output or a processor whose plugin refuses its section fails to
// start (ruling R6). When a component fails to start, or an input or a
// processor does not start within the start timeout, what this Apply started is
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

	specs, msgs := s.check(doc)
	if len(msgs) > 0 {
		s.log.Warn("supervisor: configuration rejected", "version", doc.Version, "errors", msgs)

		return Outcome{Result: ResultRejected, Errors: msgs}, nil
	}

	p := s.diff(doc, specs)
	built, msgs := s.build(doc, p, specs)
	if len(msgs) > 0 {
		built.close()
		s.log.Warn("supervisor: configuration rejected", "version", doc.Version, "errors", msgs)

		return Outcome{Result: ResultRejected, Errors: msgs}, nil
	}

	s.mu.Lock()
	applied, appliedInputs := s.applied, s.appliedInputs
	s.mu.Unlock()
	if !applied {
		// The first document of this run: no input runs yet, and the state
		// left by inputs it does not have goes before any input starts
		// (ruling R15).
		s.pruneInputStates(inputIDs(doc))
	}

	j, errs := s.execute(ctx, doc, p, built)
	built.close()
	if len(errs) > 0 {
		errs = append(errs, s.rollback(j, inputIDs(doc))...)
		if applied {
			// An input this document added and the rollback stopped must not
			// keep what it counted (ruling R15).
			s.pruneInputStates(appliedInputs)
		}
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
	s.applied, s.version, s.appliedInputs = true, doc.Version, inputIDs(doc)
	clear(s.failedIn)
	clear(s.failedPlugins)
	clear(s.failedProc)
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
		"started", len(p.startInputs)+len(p.startOutputs)+len(p.startPlugins)+len(p.startProcessors),
		"stopped", len(p.stopInputs)+len(p.stopOutputs)+len(p.stopPlugins)+len(p.stopProcessors),
		"unchanged", p.kept)

	s.pruneInputStates(inputIDs(doc))

	if err := s.store.SaveValid(doc); err != nil {
		return Outcome{Result: ResultApplied}, fmt.Errorf("supervisor: %w", err)
	}

	return Outcome{Result: ResultApplied}, nil
}

// pruneInputStates removes the transform state, DataDir/inputs/{id}, of
// every input not in keep (ruling R15): the backend keys a counter by device
// and signal, not by input, so a count resumed from an input's old directory
// (renamed back, removed and added again) would read as parts produced. Only
// inputs that do not run are removed: keep holds every running one. A failure
// is logged; the directory then stays until the next document.
func (s *Supervisor) pruneInputStates(keep map[string]bool) {
	removed, err := transform.RemoveStaleInputStates(s.dataDir, func(id string) bool { return keep[id] })
	for _, id := range removed {
		s.log.Info("supervisor: removed the transform state of an input not in the configuration", "input", id)
	}
	if err != nil {
		s.log.Warn("supervisor: cannot remove the transform state of an input not in the configuration", "error", err)
	}
}

// removeInputState removes the transform state, DataDir/inputs/{id}, of the
// input id, which does not run (ruling R17). A failure is logged; the input
// then starts from its saved count.
func (s *Supervisor) removeInputState(id string) {
	removed, err := transform.RemoveStaleInputStates(s.dataDir, func(other string) bool { return other != id })
	if len(removed) > 0 {
		s.log.Info("supervisor: removed the transform state of an input restarted by a rollback", "input", id)
	}
	if err != nil {
		s.log.Warn("supervisor: cannot remove the transform state of an input restarted by a rollback", "input", id, "error", err)
	}
}

// inputIDs returns the input ids of doc.
func inputIDs(doc config.Document) map[string]bool {
	ids := make(map[string]bool, len(doc.Inputs))
	for _, in := range doc.Inputs {
		ids[in.ID] = true
	}

	return ids
}

// check returns the reasons to refuse doc before anything is built, and the
// verified plugin entries of doc.
func (s *Supervisor) check(doc config.Document) ([]plugin.Spec, []string) {
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
			// A plugin input is built by its plugin (decision H4), which
			// plugin.Validate checks below.
			if !strings.HasPrefix(in.Driver, plugin.DriverPrefix) && s.drivers[in.Driver] == nil {
				msgs = append(msgs, fmt.Sprintf("inputs.%d.driver: not available in this build", i))
			}
		}
	}

	// Decision P1: config cannot type-check the chains, the supervisor does
	// before anything is built.
	msgs = append(msgs, transform.CheckDocument(doc)...)

	// Plugin entries and plugin inputs: files, hashes, manifests (H2, H3).
	specs, pluginMsgs := plugin.Resolve(doc, plugin.Paths{BaseDir: s.configDir, Provider: s.provider, PluginsDir: s.pluginsDir})
	msgs = append(msgs, pluginMsgs...)
	msgs = append(msgs, plugin.Validate(doc, specs)...)

	return specs, msgs
}

// pluginOf returns the plugin id of a plugin driver, plugin:<id>.
func pluginOf(driver string) (string, bool) {
	return strings.CutPrefix(driver, plugin.DriverPrefix)
}

// diff compares doc with what runs. Only Apply changes the running maps, so
// reading them under mu once is enough. A plugin an input, a processor or
// an output of doc uses is kept when its entry and the hash of its binary
// are those it runs with; otherwise it starts, after the running one
// stopped, and every component using it restarts with it, a plugin output
// keeping its buffer. A running plugin no component of doc uses stops
// (decisions H5, Q6).
func (s *Supervisor) diff(doc config.Document, specs []plugin.Spec) plan {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := plan{removedOutputs: map[string]bool{}}

	used := usedPlugins(doc)
	restarted := map[string]bool{}
	for i, entry := range doc.Plugins {
		if !used[entry.ID] {
			continue
		}
		running := s.plugins[entry.ID]
		k := slices.IndexFunc(specs, func(sp plugin.Spec) bool { return sp.ID == entry.ID })
		if running != nil && k >= 0 && bytes.Equal(running.spec.Raw, specs[k].Raw) &&
			strings.EqualFold(running.host.BinarySHA256(), specs[k].SHA256) {
			p.kept++

			continue
		}
		p.startPlugins = append(p.startPlugins, i)
		if running != nil {
			p.stopPlugins = append(p.stopPlugins, entry.ID)
			restarted[entry.ID] = true
		}
	}
	for id := range s.plugins {
		if !used[id] {
			p.stopPlugins = append(p.stopPlugins, id)
		}
	}

	inDoc := map[string]bool{}
	for i, in := range doc.Inputs {
		inDoc[in.ID] = true
		running := s.inputs[in.ID]
		pluginID, _ := pluginOf(running.driver())
		if running != nil && bytes.Equal(running.cfg.Raw, in.Raw) && !restarted[pluginID] {
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

	s.diffProcessors(doc, &p, restarted)

	outDoc := map[string]bool{}
	for i, out := range doc.Outputs {
		outDoc[out.ID] = true
		running := s.outputs[out.ID]
		pluginID, _ := pluginOf(out.Type)
		if running != nil && bytes.Equal(running.cfg.Raw, out.Raw) && !restarted[pluginID] {
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
	slices.Sort(p.stopPlugins)
	slices.Sort(p.stopProcessors)

	return p
}

// driver returns the driver of a running input, "" for none.
func (ri *runningInput) driver() string {
	if ri == nil {
		return ""
	}

	return ri.cfg.Driver
}

// pending is what build prepared: the inputs to start, by index in the
// document, and the plugins to start, by id, whose hosts are built but not
// started.
type pending struct {
	inputs  map[int]input.Input
	plugins map[string]*runningPlugin
}

// close closes the inputs never started. The plugins never started hold
// nothing to release.
func (b pending) close() { closeInputs(b.inputs) }

// build prepares the plugins and asks the drivers for every input to start.
// Nothing runs yet, so a driver refusing its configuration rejects the
// document. A plugin input is built by the plugin of its id the document
// declares: the new host when the plugin starts, else the running one.
func (s *Supervisor) build(doc config.Document, p plan, specs []plugin.Spec) (pending, []string) {
	b := pending{inputs: map[int]input.Input{}, plugins: map[string]*runningPlugin{}}
	for _, i := range p.startPlugins {
		k := slices.IndexFunc(specs, func(sp plugin.Spec) bool { return sp.ID == doc.Plugins[i].ID })
		b.plugins[specs[k].ID] = s.newPlugin(specs[k])
	}

	hosts := s.pluginHosts()
	for id, rp := range b.plugins {
		hosts[id] = rp.host
	}

	var msgs []string
	for _, i := range p.startInputs {
		cfg := doc.Inputs[i]
		in, err := s.factory(cfg.Driver, hosts)(cfg, s.clk)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("inputs.%d: %v", i, err))

			continue
		}
		b.inputs[i] = in
	}

	return b, msgs
}

// newPlugin returns the plugin of spec, its host built and not started.
func (s *Supervisor) newPlugin(spec plugin.Spec) *runningPlugin {
	return &runningPlugin{spec: spec, host: plugin.NewHost(spec, plugin.HostOptions{
		DataDir: s.dataDir,
		Clock:   s.clk,
		Log:     s.log,
	})}
}

// pluginHosts returns the hosts of the running plugins, by id.
func (s *Supervisor) pluginHosts() map[string]*plugin.Host {
	s.mu.Lock()
	defer s.mu.Unlock()

	hosts := make(map[string]*plugin.Host, len(s.plugins))
	for id, rp := range s.plugins {
		hosts[id] = rp.host
	}

	return hosts
}

// factory returns the factory of driver: for plugin:<id> the input factory
// of the host of that id in hosts (decision H4), else the registry's. It
// returns a factory that refuses when there is none, which check prevents
// for the documents it accepts.
func (s *Supervisor) factory(driver string, hosts map[string]*plugin.Host) input.Factory {
	if id, ok := pluginOf(driver); ok {
		if h := hosts[id]; h != nil {
			return plugin.InputFactory(h, s.resolver)
		}

		return func(config.Input, clock.Clock) (input.Input, error) {
			return nil, errors.New("its plugin is not running")
		}
	}
	if f := s.drivers[driver]; f != nil {
		return f
	}

	return func(config.Input, clock.Clock) (input.Input, error) {
		return nil, errors.New("driver not available in this build")
	}
}

// execute stops, then starts, what p says: inputs and processors stop
// before outputs, so nothing is routed to an output being stopped, and
// before the plugins they use; plugins start first, so a plugin output, a
// processor and a plugin input find their plugin, then outputs, then
// processors and inputs, which find their outputs. A plugin's host is
// stopped, and its Stop waited for, before a new host of the same id starts
// (ruling R12). It stops at the first failure. The inputs it starts are
// removed from b.
func (s *Supervisor) execute(ctx context.Context, doc config.Document, p plan, b pending) (journal, []string) {
	s.mu.Lock()
	j := journal{
		failedIn: maps.Clone(s.failedIn), failedOut: maps.Clone(s.failedOut),
		failedPlugins: maps.Clone(s.failedPlugins), failedProc: maps.Clone(s.failedProc),
	}
	s.mu.Unlock()
	for _, id := range p.stopInputs {
		j.stoppedInputs = append(j.stoppedInputs, s.stopInput(id))
	}
	for _, id := range p.stopProcessors {
		j.stoppedProcessors = append(j.stoppedProcessors, s.stopProcessor(id))
	}
	for _, id := range p.stopOutputs {
		how := stopForRestart
		if p.removedOutputs[id] {
			how = stopRemoved
		}
		cfg, _ := s.stopOutput(id, how, config.Route{})
		j.stoppedOutputs = append(j.stoppedOutputs, cfg)
	}
	for _, id := range p.stopPlugins {
		j.stoppedPlugins = append(j.stoppedPlugins, s.stopPlugin(id))
	}

	for _, i := range p.startPlugins {
		id := doc.Plugins[i].ID
		if err := s.startPlugin(ctx, b.plugins[id]); err != nil {
			return j, []string{fmt.Sprintf("plugins.%d: %v", i, err)}
		}
		j.startedPlugins = append(j.startedPlugins, id)
	}
	for _, i := range p.startOutputs {
		cfg := doc.Outputs[i]
		if err := s.startOutput(ctx, cfg); err != nil {
			return j, componentErrors(fmt.Sprintf("outputs.%d", i), err)
		}
		j.startedOutputs = append(j.startedOutputs, cfg.ID)
	}
	for _, i := range p.startProcessors {
		cfg := doc.Processors[i]
		if err := s.startProcessor(ctx, cfg); err != nil {
			return j, componentErrors(fmt.Sprintf("processors.%d", i), err)
		}
		j.startedProcessors = append(j.startedProcessors, cfg.ID)
	}
	for _, i := range p.startInputs {
		cfg := doc.Inputs[i]
		in := b.inputs[i]
		delete(b.inputs, i)
		if err := s.startInput(ctx, cfg, in); err != nil {
			return j, componentErrors(fmt.Sprintf("inputs.%d", i), err)
		}
		j.startedInputs = append(j.startedInputs, cfg.ID)
	}

	return j, nil
}

// componentErrors are the messages of a component at document path prefix
// that did not start: a plugin refusing the section names its paths in it
// (R13, R6).
func componentErrors(prefix string, err error) []string {
	var refused *plugin.ConfigRefusedError
	if errors.As(err, &refused) {
		return refused.Messages(prefix)
	}

	return []string{fmt.Sprintf("%s: %v", prefix, err)}
}

// startPlugin starts the host of rp and records it as running. A host that
// does not start is stopped, which removes what its launch left.
func (s *Supervisor) startPlugin(ctx context.Context, rp *runningPlugin) error {
	if err := rp.host.Start(ctx); err != nil {
		if serr := rp.host.Stop(context.Background()); serr != nil {
			s.log.Warn("supervisor: cannot stop a plugin that did not start", "plugin", rp.spec.ID, "error", serr)
		}

		return err
	}
	s.step("plugin " + rp.spec.ID)

	s.mu.Lock()
	s.plugins[rp.spec.ID] = rp
	delete(s.failedPlugins, rp.spec.ID)
	s.mu.Unlock()

	return nil
}

// stopPlugin stops the running plugin id and waits for its host's Stop,
// bounded by the stop deadline of spec section 6, and returns its spec. The
// inputs using it are stopped first.
func (s *Supervisor) stopPlugin(id string) plugin.Spec {
	s.mu.Lock()
	rp := s.plugins[id]
	delete(s.plugins, id)
	s.mu.Unlock()

	if err := rp.host.Stop(context.Background()); err != nil {
		s.log.Warn("supervisor: cannot stop a plugin", "plugin", id, "error", err)
	}

	return rp.spec
}

// rollback stops what j started, in the order execute stops (inputs,
// processors, outputs, plugins), and starts again what j stopped, in the
// order execute starts (plugins, outputs, processors, inputs). An output
// the failed document added is stopped like a removed one: its directory is
// kept. A previous component that does not start again is reported failed in
// Status, its error is returned and, for an output, the router keeps holding
// its records (ruling R48). A stopped input whose id the failed document
// does not have starts again from a count of 0 (ruling R17): the failed
// document may have renamed it, and the new id counted the same device and
// signal, so its old count would read as parts produced.
func (s *Supervisor) rollback(j journal, failedInputs map[string]bool) []string {
	for _, id := range slices.Backward(j.startedInputs) {
		s.stopInput(id)
		if f, ok := j.failedIn[id]; ok {
			s.restoreFailure(s.failedIn, id, f)
		}
	}
	for _, id := range slices.Backward(j.startedProcessors) {
		s.stopProcessor(id)
		if f, ok := j.failedProc[id]; ok {
			s.restoreFailure(s.failedProc, id, f)
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
	for _, id := range slices.Backward(j.startedPlugins) {
		s.stopPlugin(id)
		if f, ok := j.failedPlugins[id]; ok {
			s.restoreFailure(s.failedPlugins, id, f)
		}
	}

	var errs []string
	for _, spec := range j.stoppedPlugins {
		if err := s.startPlugin(s.ctx, s.newPlugin(spec)); err != nil {
			errs = append(errs, fmt.Sprintf("rollback: plugin %s: %v", spec.ID, err))
			s.setFailed(s.failedPlugins, spec.ID, "plugin", err)
		}
	}
	for _, cfg := range j.stoppedOutputs {
		if err := s.startOutput(s.ctx, cfg); err != nil {
			errs = append(errs, fmt.Sprintf("rollback: output %s: %v", cfg.ID, err))
			s.restoreFailure(s.failedOut, cfg.ID, failure{kind: cfg.Type, err: err.Error(), output: cfg})
			// The router keeps holding its records until a later document
			// starts or removes it (ruling R48).
		}
	}
	for _, cfg := range j.stoppedProcessors {
		if err := s.startProcessor(s.ctx, cfg); err != nil {
			errs = append(errs, fmt.Sprintf("rollback: processor %s: %v", cfg.ID, err))
			s.setFailed(s.failedProc, cfg.ID, cfg.Plugin, err)
		}
	}
	for _, cfg := range j.stoppedInputs {
		if !failedInputs[cfg.ID] {
			s.removeInputState(cfg.ID)
		}
		err := s.restartInput(cfg)
		if err != nil {
			errs = append(errs, fmt.Sprintf("rollback: input %s: %v", cfg.ID, err))
			s.setFailed(s.failedIn, cfg.ID, cfg.Driver, err)
		}
	}

	return errs
}

func (s *Supervisor) restartInput(cfg config.Input) error {
	in, err := s.factory(cfg.Driver, s.pluginHosts())(cfg, s.clk)
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
// eviction (Task 5 review). A plugin output is first configured in its
// plugin, which runs already: a refusal fails the start (ruling R6).
func (s *Supervisor) startOutput(ctx context.Context, cfg config.Output) error {
	dir := filepath.Join(s.dataDir, "outputs", cfg.ID)
	log := s.log.With("output", cfg.ID)

	var sender *plugin.OutputSender
	if pluginID, ok := pluginOf(cfg.Type); ok {
		var err error
		if sender, err = s.newOutputSender(ctx, pluginID, cfg, log); err != nil {
			return err
		}
	}
	fail := func(err error) error {
		if sender != nil {
			sender.Close()
		}

		return err
	}

	buf, err := buffer.Open(dir, buffer.Options{
		MaxSize:       cfg.Buffer.MaxSize,
		MinFreeRatio:  cfg.Buffer.MinFreePercent / 100,
		FsyncInterval: cfg.Buffer.FsyncInterval,
		Clock:         s.clk,
		Logger:        log,
	})
	if err != nil {
		return fail(err)
	}
	s.step("buffer " + cfg.ID)

	batcher, err := output.NewBatcher(buf, output.NewInflight(dir), envelope.NewIDGenerator(s.clk), s.clk, output.BatchLimits{
		MaxSamples: cfg.Batch.MaxSamples,
		MaxBody:    int(cfg.Batch.MaxBody),
		MaxWait:    cfg.Batch.MaxWait,
	}, log)
	if err != nil {
		_ = buf.Close()

		return fail(err)
	}
	s.step("batcher " + cfg.ID)

	id := cfg.ID
	var out outputLoop
	if sender != nil {
		out = output.NewLoop(output.LoopConfig{
			ID:          id,
			Sender:      sender,
			Batcher:     batcher,
			Clock:       s.clk,
			Logger:      s.log,
			OnReconnect: func() { s.router.BirthTo(id) },
		})
	} else {
		out, err = output.NewHTTP(output.HTTPConfig{
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
	}

	runCtx, cancel := context.WithCancel(s.ctx)
	ro := &runningOutput{cfg: cfg, dir: dir, buf: buf, out: out, sender: sender, cancel: cancel, done: make(chan struct{})}
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
	if ro.sender != nil {
		ro.sender.Close()
	}

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

// startInput starts in within the start timeout, behind a transform
// processor built from cfg, which applies the point chains to what the input
// emits and keeps its edge_counter state in DataDir/inputs/{id}. The
// processor starts before the input. Its devices are registered with the
// router, and their births sent, once the input is live: before its first
// record or when Start returns, whichever comes first. Until then an output
// reconnecting sends no birth for them. A failed or abandoned start is
// retired before the input is closed: a goroutine it left behind cannot
// register it or send a record, and its late cleanup cannot unregister the
// instance a rollback started meanwhile. Its processor is closed before
// startInput returns, the input of an abandoned start still running: no two
// live processors of one input ever share the state file (ruling R11).
func (s *Supervisor) startInput(ctx context.Context, cfg config.Input, in input.Input) error {
	proc, err := transform.NewProcessor(cfg, s.dataDir, s.clk, s.log)
	if err != nil {
		// Unreachable: NewProcessor fails only on a chain that does not
		// compile, which check refused before anything was built, and no seam
		// injects a failing processor, so no test reaches it. The built input
		// never started: it is closed here, since the caller no longer owns
		// it. The error names no device or signal: its text is fixed.
		if cerr := in.Close(); cerr != nil {
			s.log.Warn("supervisor: cannot close an input that did not start", "input", cfg.ID, "error", cerr)
		}

		return errors.New("the transform chains cannot be built")
	}

	lifeCtx, cancel := context.WithCancel(s.ctx)
	inst := &inputInstance{router: s.router, cfg: cfg, live: true, taps: &s.taps}
	proc.Start(lifeCtx)
	emit := proc.Wrap(inst.emit)

	err = s.startBounded(ctx, cancel, func() error { return in.Start(lifeCtx, emit) }, func() {
		inst.retire()
		if err := in.Close(); err != nil {
			s.log.Warn("supervisor: cannot close an input that did not start", "input", cfg.ID, "error", err)
		}
	})
	if err != nil {
		inst.retire()
		cancel()
		s.closeProcessor(cfg.ID, proc)

		return err
	}

	inst.register()
	s.step("input " + cfg.ID)

	s.mu.Lock()
	s.inputs[cfg.ID] = &runningInput{cfg: cfg, in: in, cancel: cancel, inst: inst, proc: proc}
	delete(s.failedIn, cfg.ID)
	s.mu.Unlock()

	return nil
}

// closeProcessor closes the processor of input id, which waits for its
// deadline goroutine, and keeps its error count. The caller holds no lock:
// the processor's downstream emit takes the instance's and the router's.
func (s *Supervisor) closeProcessor(id string, proc *transform.Processor) {
	proc.Close()

	s.mu.Lock()
	s.transformErrors[id] += proc.Errors()
	s.mu.Unlock()
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
// nothing it emits while it closes is routed. Its processor is closed once
// the input's Close returned, so a restart of the input builds the next one
// with the state file no longer written.
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
	s.closeProcessor(id, ri.proc)

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

// Close stops every input, then every processor, then every plugin, all at
// once so that the stop deadline of spec section 6 bounds them together,
// then every output (a plugin output's batches stay in its buffer),
// syncing and closing the buffers, and waits for every goroutine of the
// supervisor. The records held for an output left failed by a rollback are
// discarded at shutdown, once the inputs are stopped, with a warning per
// priority giving the count and the ts range. Later calls return nil.
func (s *Supervisor) Close() error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	s.mu.Lock()
	inputs := sortedKeys(s.inputs)
	processors := sortedKeys(s.processors)
	outputs := sortedKeys(s.outputs)
	failedOut := sortedKeys(s.failedOut)
	plugins := sortedKeys(s.plugins)
	clear(s.failedPlugins)
	clear(s.failedProc)
	s.mu.Unlock()

	for _, id := range inputs {
		s.step("stop input " + id)
		s.stopInput(id)
	}
	for _, id := range processors {
		s.step("stop processor " + id)
		s.stopProcessor(id)
	}
	var stops sync.WaitGroup
	for _, id := range plugins {
		s.step("stop plugin " + id)
		stops.Go(func() { s.stopPlugin(id) })
	}
	stops.Wait()
	for _, id := range failedOut {
		s.step("discard output " + id)
		s.router.RemoveOutput(id, "agent shutting down")
	}

	var errs []error
	for _, id := range outputs {
		s.step("stop output " + id)
		if _, err := s.stopOutput(id, stopShutdown, config.Route{}); err != nil {
			errs = append(errs, fmt.Errorf("supervisor: output %s: %w", id, err))
		}
	}

	s.cancel()
	s.wg.Wait()

	return errors.Join(errs...)
}

// ReloadCredentials makes every running output resolve its token, or the
// secrets of its plugin config, again before its next attempt and ends a
// suspension at once (SIGHUP); a plugin output goes through its Loop
// (ruling R3).
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
