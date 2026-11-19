package serial

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Options are what the serial inputs take from the agent.
type Options struct {
	// Logger receives the messages of the driver; nil discards.
	Logger *slog.Logger
}

// Factory returns the factory of serial inputs. It validates the connection
// and every point and never opens the port (core R47).
func Factory(opts Options) edgeinput.Factory {
	return func(cfg config.Input, clk clock.Clock) (edgeinput.Input, error) {
		in, err := New(cfg, clk, opts)
		if err != nil {
			return nil, err
		}
		return in, nil
	}
}

// Input is one serial input: one port, opened in the background with the
// common backoff and read by one goroutine that cuts the bytes into frames
// and routes them (spec section 8). It implements edgeinput.HealthReporter.
type Input struct {
	cfg     config.Input
	clk     clock.Clock
	log     *slog.Logger
	conn    Connection
	router  *router
	devices []string

	// deps are fixed by the constructor and never change afterwards.
	deps deps

	health driverkit.Health

	// mu guards the fields below.
	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	// dropLogged is set once the context drop was logged by Close.
	dropLogged bool

	wg sync.WaitGroup
}

var _ edgeinput.HealthReporter = (*Input)(nil)

// deps are what an input takes besides its configuration: the opener of its
// port, the lister of the host's ports and the draw of the backoff jitter, in
// [0, 1). New gives the production ones; the tests build their inputs through
// newInput, where a nil field means the production one.
type deps struct {
	open   Opener
	list   func() ([]PortInfo, error)
	jitter func() float64
}

// New returns the serial input of cfg. The errors are fixed texts naming the
// input and the field path or the position of the point
// (devices.<d>.points.<p>), never a value or a connection setting. A point
// whose address repeats an earlier one is logged at warn: it never receives a
// frame.
func New(cfg config.Input, clk clock.Clock, opts Options) (*Input, error) {
	return newInput(cfg, clk, opts, deps{})
}

// newInput is New with the given deps.
func newInput(cfg config.Input, clk clock.Clock, opts Options, d deps) (*Input, error) {
	conn, err := ParseConnection(cfg.Connection)
	if err != nil {
		return nil, fmt.Errorf("serial input %q: %w", cfg.ID, err)
	}
	r, err := newRouter(cfg, conn)
	if err != nil {
		return nil, err
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	for _, path := range r.duplicateAddresses() {
		log.Warn(msgDuplicateAddress, "input", cfg.ID, "point", path)
	}

	if d.open == nil {
		d.open = OpenPort
	}
	if d.list == nil {
		d.list = ListPorts
	}
	if d.jitter == nil {
		d.jitter = rand.Float64
	}

	in := &Input{cfg: cfg, clk: clk, log: log, conn: conn, router: r, deps: d}
	for _, dev := range cfg.Devices {
		if !slices.Contains(in.devices, dev.Device) {
			in.devices = append(in.devices, dev.Device)
		}
	}
	return in, nil
}

// Start starts the device lifecycle and the goroutine that opens and reads
// the port, and returns at once: a port missing, busy or refused is retried
// with the backoff, never a failing Start (spec S8). The goroutines stop when
// ctx is done or on Close.
func (in *Input) Start(ctx context.Context, emit func(model.Record)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("serial input %q: start: %w", in.cfg.ID, err)
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	switch {
	case in.closed:
		return fmt.Errorf("serial input %q: start after close", in.cfg.ID)
	case in.started:
		return fmt.Errorf("serial input %q: already started", in.cfg.ID)
	}

	runCtx, cancel := context.WithCancel(ctx)
	in.cancel = cancel
	in.started = true

	life := edgeinput.NewLifecycle(in.cfg, in.clk, emit)
	in.wg.Go(func() { life.Run(runCtx) })
	in.wg.Go(func() { in.run(runCtx, emit, life) })

	return nil
}

// Close stops the input and waits for its goroutines, closing the port.
// After Close returns emit is never called. The context held in memory is
// dropped, which is logged once (without its values). It is safe on an input
// never started and when called twice.
func (in *Input) Close() error {
	in.mu.Lock()
	in.closed = true
	cancel := in.cancel
	in.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	in.wg.Wait()

	in.mu.Lock()
	defer in.mu.Unlock()
	if in.started && !in.dropLogged && len(in.router.keys) > 0 {
		in.dropLogged = true
		in.log.Info(msgContextDropped, "input", in.cfg.ID)
	}
	return nil
}

// Health returns whether the port is open and the read errors of the input.
func (in *Input) Health() edgeinput.Health {
	return in.health.Snapshot()
}

// Probe is not available yet for serial inputs.
//
// TODO(serial, Task 7): open the port once, send the requests and route
// frames without emitting.
func (in *Input) Probe(ctx context.Context) ([]model.Sample, error) {
	return nil, fmt.Errorf("serial input %q: probe is not available yet", in.cfg.ID)
}
