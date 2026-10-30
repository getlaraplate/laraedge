package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlaraplate/laraedge-sdk/handshake"
	pb "github.com/getlaraplate/laraedge-sdk/pluginv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

const (
	// processorQueueSize bounds the copies waiting for the stream of one
	// processor instance (decision Q4).
	processorQueueSize = 1000
	// livenessTick is how often the derived devices are marked read while
	// the stream is up (decision Q5).
	livenessTick = time.Second
	// maxRefusalWarnings bounds the distinct refusals logged once each, so a
	// plugin inventing names cannot grow the memory or flood the log.
	maxRefusalWarnings = 100
)

// ProcessorStats is the state of a processor instance.
type ProcessorStats struct {
	// Up is true while the Process stream is open.
	Up bool
	// Refused counts what the plugin sent outside its declared derived
	// devices and signals, or with a value kind other than the declared
	// type.
	Refused uint64
	// Dropped counts the copies of observed records the queue had no room
	// for.
	Dropped uint64
}

// derivedSignal is a declared derived signal as the adapter checks it.
type derivedSignal struct {
	kind     model.ValueKind
	priority model.Priority
}

// Processor is one processor instance served by the plugin h runs (spec P4):
// it receives copies of the records of the observed devices through Offer
// and emits the samples the plugin derives for its declared derived devices.
// It never touches machine data: Offer never blocks, and a full queue drops
// the copy. Its run goroutine owns the connection to the plugin: it
// configures the instance, opens the one Process stream, and after a plugin
// restart does both again.
type Processor struct {
	h        *Host
	resolver SecretResolver
	cfg      config.Processor
	clk      clock.Clock
	log      *slog.Logger

	virtual  config.Input
	devices  []string
	observed map[string]bool
	declared map[string]map[string]derivedSignal // device, signal
	queue    chan *pb.Record

	up      atomic.Bool
	down    atomic.Bool // bad samples emitted for the current outage
	closed  atomic.Bool
	refused atomic.Uint64
	dropped atomic.Uint64

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// cfgGen is the host generation the instance is configured for, 0 when
	// it is not; secretValues are the values resolved for that Configure.
	// Start uses them before the run goroutine starts, the run goroutine
	// afterwards.
	cfgGen       uint64
	secretValues []string

	// warned holds the refusals already logged; only the run goroutine
	// touches it.
	warned map[string]bool
}

// NewProcessor returns processor cfg served by the plugin h runs. It only
// prepares: nothing contacts the plugin before Start. Secrets of config are
// resolved with resolver at every Configure, and the resolved values go to
// the plugin only. clk times the deadlines, the liveness tick and the death
// tolerance; nil means clock.Real(). log nil logs to the host's logger.
func NewProcessor(h *Host, cfg config.Processor, resolver SecretResolver, clk clock.Clock, log *slog.Logger) *Processor {
	if clk == nil {
		clk = clock.Real()
	}
	if log == nil {
		log = h.log
	}

	p := &Processor{
		h: h, resolver: resolver, cfg: cfg, clk: clk,
		log:      log.With("processor", cfg.ID),
		observed: make(map[string]bool, len(cfg.Observe)),
		declared: make(map[string]map[string]derivedSignal, len(cfg.Devices)),
		queue:    make(chan *pb.Record, processorQueueSize),
		warned:   map[string]bool{},
	}
	for _, device := range cfg.Observe {
		p.observed[device] = true
	}

	p.virtual = config.Input{ID: cfg.ID, DeathTolerance: cfg.DeathTolerance, Raw: cfg.Raw}
	for _, d := range cfg.Devices {
		if _, seen := p.declared[d.Device]; !seen {
			p.devices = append(p.devices, d.Device)
			p.declared[d.Device] = map[string]derivedSignal{}
		}
		device := config.Device{Device: d.Device}
		for _, s := range d.Signals {
			priority := derivedPriority(s.Priority)
			p.declared[d.Device][s.Signal] = derivedSignal{kind: derivedKind(s.Type), priority: priority}
			device.Points = append(device.Points, config.Point{Signal: s.Signal, Type: s.Type, Priority: priority, Unit: s.Unit})
		}
		p.virtual.Devices = append(p.virtual.Devices, device)
	}

	return p
}

// derivedKind is the value kind of a derived signal type (decision Q3).
func derivedKind(signalType string) model.ValueKind {
	switch signalType {
	case "int":
		return model.KindInt
	case "bool":
		return model.KindBool
	case "string":
		return model.KindString
	default:
		return model.KindFloat
	}
}

func derivedPriority(priority string) model.Priority {
	if p, err := model.ParsePriority(priority); err == nil {
		return p
	}

	return model.Critical
}

// Input returns the virtual input of the derived devices: one device per
// derived device, one point per derived signal with its type (whose birth
// data type is number, number, boolean or string), priority and unit, and
// the death tolerance of the processor. The router sends the births of the
// derived devices from it; input.NewLifecycle accepts it. Callers must not
// modify it.
func (p *Processor) Input() config.Input { return p.virtual }

// Stats returns the state of the instance; safe for concurrent use.
func (p *Processor) Stats() ProcessorStats {
	return ProcessorStats{Up: p.up.Load(), Refused: p.refused.Load(), Dropped: p.dropped.Load()}
}

// Offer queues a copy of rec for the plugin when rec is a sample or a device
// event of an observed device; other records are ignored. It never blocks:
// when the queue is full the copy is dropped and counted (decision Q4).
// Safe for concurrent use; a no-op after Close.
func (p *Processor) Offer(rec model.Record) {
	if p.closed.Load() {
		return
	}

	var device string
	switch {
	case rec.Sample != nil:
		device = rec.Sample.Device
	case rec.Event != nil:
		device = rec.Event.Device
	default:
		return
	}
	if !p.observed[device] {
		return
	}

	select {
	case p.queue <- recordToWire(rec):
	default:
		p.dropped.Add(1)
	}
}

// Start configures the instance when the plugin is up, opens the stream and
// starts the lifecycle of the derived devices; it does not wait for a
// sample. It fails when the plugin does not offer processors, refuses the
// configuration or a secret cannot be resolved (*ConfigRefusedError naming
// paths under processors.<id>). A plugin that is down or fails the call is
// not a failure: the derived devices go bad, then die, and the instance is
// configured when the plugin is back. emit receives the derived samples,
// the bad samples and the births and deaths of the derived devices, from
// more than one goroutine.
func (p *Processor) Start(ctx context.Context, emit func(model.Record)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("plugin processor %q: start: %w", p.cfg.ID, err)
	}
	if p.h.spec.Manifest.Kinds.Processor == nil {
		return fmt.Errorf("plugin processor %q: the plugin does not offer the processor kind", p.cfg.ID)
	}
	if len(p.cfg.Raw) == 0 {
		return fmt.Errorf("plugin processor %q: the processor has no section to configure the plugin with", p.cfg.ID)
	}

	p.mu.Lock()
	switch {
	case p.closed.Load():
		p.mu.Unlock()

		return fmt.Errorf("plugin processor %q: start after close", p.cfg.ID)
	case p.started:
		p.mu.Unlock()

		return fmt.Errorf("plugin processor %q: already started", p.cfg.ID)
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.started = true
	p.mu.Unlock()

	// fail undoes the start: the processor can be started again.
	fail := func(err error) error {
		cancel()
		p.mu.Lock()
		p.started, p.cancel = false, nil
		p.mu.Unlock()

		return err
	}

	// Watch before reading the connection: a restart in between is seen.
	changes, unwatch := p.h.Watch()
	if conn, gen := p.h.Conn(); conn != nil {
		if err := p.ensureConfigured(runCtx, conn, gen); err != nil {
			var refused *ConfigRefusedError
			if errors.As(err, &refused) || runCtx.Err() != nil {
				unwatch()

				return fail(err)
			}
			p.log.Warn("plugin processor could not be configured at start, retrying", "error", err.Error())
		}
	}

	// The goroutines start under mu, so a Close either sees them or comes
	// first and refuses the start: none starts after Close returned.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		unwatch()
		cancel()

		return fmt.Errorf("plugin processor %q: start after close", p.cfg.ID)
	}
	life := input.NewLifecycle(p.virtual, p.clk, emit)
	p.wg.Go(func() { life.Run(runCtx) })
	p.wg.Go(func() {
		defer unwatch()
		p.run(runCtx, emit, life, changes)
	})

	return nil
}

// Close ends the stream and waits for the goroutines of the processor;
// nothing is emitted afterwards and Offer does nothing. The instance is not
// released in the plugin (decision Q7). Safe on a processor never started
// and safe to call more than once.
func (p *Processor) Close() error {
	p.mu.Lock()
	p.closed.Store(true)
	cancel := p.cancel
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
	p.up.Store(false)

	return nil
}

// ensureConfigured configures the instance unless it is configured for the
// running generation of the plugin.
func (p *Processor) ensureConfigured(ctx context.Context, conn *grpc.ClientConn, gen uint64) error {
	if p.cfgGen == gen {
		return nil
	}
	p.cfgGen = 0

	payload, values, err := p.payload(ctx)
	if err != nil {
		return err
	}

	callCtx, release := deadline(p.clk, ctx, configureTimeout)
	defer release()

	resp, err := pb.NewProcessorClient(conn).Configure(callCtx, &pb.ConfigureRequest{InstanceId: p.cfg.ID, Config: payload})
	if err != nil {
		return fmt.Errorf("plugin processor %q: %s", p.cfg.ID, callFailure(ctx, callCtx, "configure", err))
	}
	if len(resp.GetErrors()) > 0 {
		refused := p.refusal(nil)
		for _, e := range resp.GetErrors() {
			refused.Problems = append(refused.Problems, ConfigProblem{
				Path:    redact(e.GetPath(), values),
				Message: redact(e.GetMessage(), values),
			})
		}

		return refused
	}
	p.cfgGen, p.secretValues = gen, values

	return nil
}

func (p *Processor) refusal(problems []ConfigProblem) *ConfigRefusedError {
	return &ConfigRefusedError{Input: p.cfg.ID, Section: "processors", Problems: problems}
}

// payload returns the processor section with every secret reference of
// config resolved now, and the resolved values (for redaction).
func (p *Processor) payload(ctx context.Context) ([]byte, []string, error) {
	var section map[string]any
	dec := json.NewDecoder(bytes.NewReader(p.cfg.Raw))
	dec.UseNumber()
	if err := dec.Decode(&section); err != nil || section == nil {
		return nil, nil, p.refusal([]ConfigProblem{{Message: "the processor section is not a JSON object"}})
	}

	var (
		values   []string
		problems []ConfigProblem
	)
	if cfg, ok := section["config"]; ok {
		section["config"] = resolveSecrets(ctx, p.resolver, cfg, "config", &values, &problems)
	}
	if len(problems) > 0 {
		return nil, nil, p.refusal(problems)
	}

	out, err := json.Marshal(section)
	if err != nil {
		return nil, nil, p.refusal([]ConfigProblem{{Message: "the processor section cannot be encoded"}})
	}

	return out, values, nil
}

// run keeps the instance streaming until ctx ends, as an input's run does:
// when the stream or the plugin goes away the derived signals go bad once,
// and it waits for the plugin to come back (a new generation) or, when the
// plugin is still up, for a backoff, then configures again if the plugin
// restarted.
func (p *Processor) run(ctx context.Context, emit func(model.Record), life *input.Lifecycle, changes <-chan uint64) {
	var (
		backoff = driverkit.NewBackoff(p.clk, rand.Float64)
		busyAt  time.Time // first refusal of the current streak, zero when none
		lastGen uint64
	)

	for ctx.Err() == nil {
		conn, gen := p.h.Conn()
		if gen != lastGen {
			lastGen, busyAt = gen, time.Time{}
		}
		if conn == nil {
			p.goDown(emit)
			if !waitForPlugin(ctx, p.clk, changes, nil) {
				return
			}

			continue
		}

		if err := p.ensureConfigured(ctx, conn, gen); err != nil {
			if ctx.Err() != nil {
				return
			}
			p.goDown(emit)

			var refused *ConfigRefusedError
			if errors.As(err, &refused) {
				// Decision H6: valid when applied, so no rollback; the
				// derived devices stay down until the plugin restarts again.
				p.log.Warn("plugin refused the configuration after a restart, the processor stays down",
					"problems", strings.Join(refused.Messages("processors."+p.cfg.ID), "; "))
				if !waitForPlugin(ctx, p.clk, changes, nil) {
					return
				}

				continue
			}
			p.log.Warn("plugin processor could not be configured, retrying", "error", err.Error())
			if !waitForPlugin(ctx, p.clk, changes, backoff) {
				return
			}

			continue
		}

		lived, err := p.stream(ctx, conn, emit, life)
		if ctx.Err() != nil {
			return
		}
		if lived {
			backoff.Reset()
			busyAt = time.Time{}
		}
		if !lived && status.Code(err) == codes.AlreadyExists {
			// The previous stream of the instance is still unwinding in the
			// plugin: not an outage until busyBound has passed.
			if busyAt.IsZero() {
				busyAt = p.clk.Now()
			}
			if p.clk.Now().Sub(busyAt) < busyBound {
				if p.clk.Sleep(ctx, busyWait) != nil {
					return
				}

				continue
			}
		}
		p.goDown(emit)
		p.log.Warn("plugin processor stream ended", "code", status.Code(err).String())
		if !waitForPlugin(ctx, p.clk, changes, backoff) {
			return
		}
	}
}

// stream opens the one Process stream of the instance and runs it until it
// ends: a feeder goroutine sends the queued copies, a ticker goroutine marks
// the derived devices read every livenessTick, and this goroutine receives
// the derived samples. Both goroutines have ended when stream returns.
// lived tells whether the stream ticked or delivered a sample.
func (p *Processor) stream(ctx context.Context, conn *grpc.ClientConn, emit func(model.Record), life *input.Lifecycle) (bool, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := pb.NewProcessorClient(conn).Process(streamCtx)
	if err != nil {
		return false, err
	}
	start := &pb.ProcessorIn{Msg: &pb.ProcessorIn_Start{Start: &pb.ProcessStart{InstanceId: p.cfg.ID}}}
	if err := stream.Send(start); err != nil {
		if errors.Is(err, io.EOF) {
			// The status of a stream the plugin ended is read by Recv.
			_, err = stream.Recv()
		}

		return false, err
	}

	p.up.Store(true)
	defer p.up.Store(false)

	var (
		lived atomic.Bool
		wg    sync.WaitGroup
	)
	wg.Go(func() { p.feed(streamCtx, stream) })
	wg.Go(func() { p.tick(streamCtx, life, &lived) })
	for {
		out, err := stream.Recv()
		if err != nil {
			cancel()
			wg.Wait()

			return lived.Load(), err
		}
		if p.handle(out, emit, life) {
			lived.Store(true)
		}
	}
}

// feed sends the queued copies until ctx ends or the stream fails. A copy
// over the message limit of the protocol is dropped and counted.
func (p *Processor) feed(ctx context.Context, stream pb.Processor_ProcessClient) {
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-p.queue:
			msg := &pb.ProcessorIn{Msg: &pb.ProcessorIn_Record{Record: rec}}
			if proto.Size(msg) > handshake.MaxMessageSize {
				p.dropped.Add(1)

				continue
			}
			if stream.Send(msg) != nil {
				return
			}
		}
	}
}

// tick marks every derived device read every livenessTick until ctx ends:
// a derived device lives while the stream is up (no per-sample liveness).
func (p *Processor) tick(ctx context.Context, life *input.Lifecycle, lived *atomic.Bool) {
	for {
		timer := p.clk.NewTimer(livenessTick)
		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C():
			lived.Store(true)
			p.down.Store(false)
			for _, device := range p.devices {
				life.Read(device)
			}
		}
	}
}

// handle checks one message of the plugin and emits its sample when the
// sample belongs to a declared derived signal with the declared value kind;
// anything else is refused and counted. The sample keeps the ts and the
// quality of the plugin and takes the declared priority. It reports whether
// the sample was emitted.
func (p *Processor) handle(out *pb.ProcessorOut, emit func(model.Record), life *input.Lifecycle) bool {
	wire := out.GetSample()
	device, signal := wire.GetDevice(), wire.GetSignal()

	s, err := sampleFromWire(wire)
	if err != nil {
		p.refuse("the sample is invalid", device, signal)

		return false
	}
	signals, ok := p.declared[s.Device]
	if !ok {
		p.refuse("the device is not a derived device of the processor", device, signal)

		return false
	}
	declared, ok := signals[s.Signal]
	if !ok {
		p.refuse("the signal is not declared on the derived device", device, signal)

		return false
	}
	if s.Value.Kind != declared.kind {
		p.refuse("the value kind is not the declared type", device, signal)

		return false
	}
	s.Priority = declared.priority

	p.down.Store(false)
	life.Read(s.Device)
	emit(model.SampleRecord(s))

	return true
}

// refuse counts a refusal and logs it once per reason, device and signal,
// naming them and never the value.
func (p *Processor) refuse(reason, device, signal string) {
	p.refused.Add(1)

	key := reason + "\x00" + device + "\x00" + signal
	if p.warned[key] || len(p.warned) >= maxRefusalWarnings {
		return
	}
	p.warned[key] = true
	p.log.Warn("plugin processor sample refused: "+reason,
		"device", redact(device, p.secretValues), "signal", redact(signal, p.secretValues))
}

// goDown reports the processor down once per outage: one bad sample per
// derived signal with the zero value of its type. The derived devices die
// after the death tolerance through the lifecycle, the tick having stopped.
func (p *Processor) goDown(emit func(model.Record)) {
	p.up.Store(false)
	if p.down.Swap(true) {
		return
	}

	now := p.clk.Now().UTC()
	for _, d := range p.cfg.Devices {
		for _, s := range d.Signals {
			emit(model.SampleRecord(model.Sample{
				Device: d.Device, Signal: s.Signal, TS: now,
				Value: model.Value{Kind: derivedKind(s.Type)}, Quality: model.Bad, Priority: derivedPriority(s.Priority),
			}))
		}
	}
}
