package file

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Options are what the file inputs take from the agent: the data directory
// holding the ledger of read files and the logger.
type Options struct {
	DataDir string
	// Logger receives the messages of the driver; nil discards.
	Logger *slog.Logger
}

// Factory returns the factory of file inputs. It validates the connection
// and every point and never touches the disk (decision P1, core R47).
func Factory(opts Options) edgeinput.Factory {
	return func(cfg config.Input, clk clock.Clock) (edgeinput.Input, error) {
		in, err := New(cfg, clk, opts)
		if err != nil {
			return nil, err
		}

		return in, nil
	}
}

// Input is one file input: one watched directory, scanned every interval by
// one goroutine that commits the files emitted a scan earlier, lists the
// directory, keeps the devices alive while the listing works and reads the
// files that became stable (spec section 5). It implements
// edgeinput.HealthReporter.
type Input struct {
	cfg     config.Input
	clk     clock.Clock
	opts    Options
	log     *slog.Logger
	conn    Connection
	plan    *plan
	devices []string

	// deps are fixed by the constructor and never change afterwards.
	deps deps

	health driverkit.Health

	// mu guards the fields below.
	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	// probeSource is the file the last probe read.
	probeSource string

	wg sync.WaitGroup
}

var _ edgeinput.HealthReporter = (*Input)(nil)

// deps are the file system operations of an input on the watched directory.
// New gives the production ones; the tests build their inputs through
// newInput, where a nil field means the production one.
type deps struct {
	list     func(dir string) ([]fileInfo, error)
	readFile func(path string) ([]byte, error)
	rename   func(oldPath, newPath string) error
}

// New returns the file input of cfg. The errors are fixed texts naming the
// input and the field path or the position of the point
// (devices.<d>.points.<p>), never a value or a connection setting.
func New(cfg config.Input, clk clock.Clock, opts Options) (*Input, error) {
	return newInput(cfg, clk, opts, deps{})
}

// newInput is New with the given deps.
func newInput(cfg config.Input, clk clock.Clock, opts Options, d deps) (*Input, error) {
	conn, err := ParseConnection(cfg.Connection)
	if err != nil {
		return nil, fmt.Errorf("file input %q: %w", cfg.ID, err)
	}

	for d, dev := range cfg.Devices {
		for i, p := range dev.Points {
			if err := CheckPoint(p); err != nil {
				return nil, fmt.Errorf("file input %q: devices.%d.points.%d: %w", cfg.ID, d, i, err)
			}
		}
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	in := &Input{
		cfg:  cfg,
		clk:  clk,
		opts: opts,
		log:  log,
		conn: conn,
		plan: newPlan(cfg, conn),
		deps: d,
	}
	if in.deps.list == nil {
		in.deps.list = listDir
	}
	if in.deps.readFile == nil {
		in.deps.readFile = os.ReadFile
	}
	if in.deps.rename == nil {
		in.deps.rename = os.Rename
	}
	for _, dev := range cfg.Devices {
		if !slices.Contains(in.devices, dev.Device) {
			in.devices = append(in.devices, dev.Device)
		}
	}

	return in, nil
}

// Start loads the ledger of read files from data_dir, a local resource whose
// failure fails Start, starts the device lifecycle and the scanning
// goroutine, and returns. It does nothing on the watched directory: a
// directory missing or unreadable is death after the death tolerance, never
// a failing Start (core R47, spec D6). The goroutines stop when ctx is done
// or on Close.
func (in *Input) Start(ctx context.Context, emit func(model.Record)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("file input %q: start: %w", in.cfg.ID, err)
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	switch {
	case in.closed:
		return fmt.Errorf("file input %q: start after close", in.cfg.ID)
	case in.started:
		return fmt.Errorf("file input %q: already started", in.cfg.ID)
	}

	led, err := loadLedger(in.opts.DataDir, in.cfg.ID, in.log)
	if err != nil {
		return fmt.Errorf("file input %q: start: %w", in.cfg.ID, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	in.cancel = cancel
	in.started = true

	life := edgeinput.NewLifecycle(in.cfg, in.clk, emit)
	s := newScanner(in, emit, life, led)

	in.wg.Go(func() { life.Run(runCtx) })
	in.wg.Go(func() { in.run(runCtx, s) })

	return nil
}

// Close stops the scanning and waits for the goroutines. The files emitted
// and not yet committed stay uncommitted: the next start reads them again
// (spec section 5). After Close returns emit is never called. It is safe on
// an input never started and when called twice.
func (in *Input) Close() error {
	in.mu.Lock()
	in.closed = true
	cancel := in.cancel
	in.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	in.wg.Wait()

	return nil
}

// Health returns whether the last listing worked and the read errors of the
// input.
func (in *Input) Health() edgeinput.Health {
	return in.health.Snapshot()
}
