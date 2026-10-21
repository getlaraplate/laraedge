package modbus

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"time"

	mbus "github.com/simonvetter/modbus"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Factory returns the factory of Modbus inputs. It validates the connection
// and every point (decision M1) and plans the read blocks; it never opens a
// connection (core R47).
func Factory() input.Factory {
	return func(cfg config.Input, clk clock.Clock) (input.Input, error) {
		in, err := New(cfg, clk)
		if err != nil {
			return nil, err
		}

		return in, nil
	}
}

// Input is one Modbus input: one connection, owned by one poll goroutine that
// reads every block at its interval (decision M5). It implements
// input.HealthReporter.
type Input struct {
	cfg    config.Input
	clk    clock.Clock
	conn   Connection
	blocks []Block
	// devices lists, for each block, the devices that have a point in it.
	devices [][]string

	newClient clientFactory
	// jitter draws the backoff jitter, in [0, 1).
	jitter func() float64

	health driverkit.Health

	// mu guards the fields below.
	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc

	wg sync.WaitGroup
}

var _ input.HealthReporter = (*Input)(nil)

// New returns the Modbus input of cfg. The errors are fixed texts naming the
// input and the position of the point (devices.<d>.points.<p>), never a
// value or a connection setting.
func New(cfg config.Input, clk clock.Clock) (*Input, error) {
	conn, err := ParseConnection(cfg.Connection)
	if err != nil {
		return nil, fmt.Errorf("modbus input %q: %w", cfg.ID, err)
	}

	var points []PlannedPoint
	for d, dev := range cfg.Devices {
		for i, p := range dev.Points {
			planned, err := planPoint(dev.Device, p)
			if err != nil {
				return nil, fmt.Errorf("modbus input %q: devices.%d.points.%d: %w", cfg.ID, d, i, err)
			}
			points = append(points, planned)
		}
	}

	blocks := Plan(points, conn.UnitID)
	devices := make([][]string, len(blocks))
	for i, b := range blocks {
		for _, p := range b.Points {
			if !slices.Contains(devices[i], p.Device) {
				devices[i] = append(devices[i], p.Device)
			}
		}
	}

	return &Input{
		cfg:       cfg,
		clk:       clk,
		conn:      conn,
		blocks:    blocks,
		devices:   devices,
		newClient: newLibraryClient,
		jitter:    rand.Float64,
	}, nil
}

// planPoint checks a point and returns it ready for Plan, which trusts the
// size of points that passed CheckPoint.
func planPoint(device string, p config.Point) (PlannedPoint, error) {
	addr, err := ParseAddress(p.Address)
	if err != nil {
		return PlannedPoint{}, err
	}
	if err := CheckPoint(p, addr); err != nil {
		return PlannedPoint{}, err
	}
	size, err := Size(p, addr)
	if err != nil {
		return PlannedPoint{}, err
	}

	return PlannedPoint{Device: device, Signal: p.Signal, Point: p, Addr: addr, Size: size}, nil
}

// Start starts the poll goroutine and the device lifecycle and returns: the
// connection is opened in the background, so an unreachable device is death
// after the death tolerance, never a failing Start (core R47). The goroutines
// stop when ctx is done or on Close.
func (in *Input) Start(ctx context.Context, emit func(model.Record)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("modbus input %q: start: %w", in.cfg.ID, err)
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	switch {
	case in.closed:
		return fmt.Errorf("modbus input %q: start after close", in.cfg.ID)
	case in.started:
		return fmt.Errorf("modbus input %q: already started", in.cfg.ID)
	}

	runCtx, cancel := context.WithCancel(ctx)
	in.cancel = cancel
	in.started = true

	life := input.NewLifecycle(in.cfg, in.clk, emit)
	start := in.clk.Now()
	in.wg.Go(func() { life.Run(runCtx) })
	in.wg.Go(func() { in.poll(runCtx, emit, life, start) })

	return nil
}

// Close stops the input and waits for its goroutines. A request in flight
// ends within the connection timeout and its answer is dropped; the
// library's connection attempt is bounded by the library itself. It is safe
// on an input never started and when called twice.
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

// Health returns the connection state and the read errors of the input.
func (in *Input) Health() input.Health {
	return in.health.Snapshot()
}

// Probe opens a connection of its own, reads every block once and returns one
// sample per point, in configuration order: bad for the points of a block
// answered with exceptions 1 to 4 and for a value that does not decode. It
// starts no loop and emits nothing. A device that cannot be reached, does
// not answer in time, is busy or sits behind a failing gateway is an error.
func (in *Input) Probe(ctx context.Context) ([]model.Sample, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("modbus input %q: probe: %w", in.cfg.ID, err)
	}

	c, err := in.newClient(in.conn)
	if err == nil {
		err = c.Open()
	}
	if err != nil {
		return nil, fmt.Errorf("modbus input %q: probe: cannot connect to the device", in.cfg.ID)
	}
	defer func() { _ = c.Close() }()

	type key struct{ device, signal string }
	read := make(map[key]model.Sample)
	limiter := driverkit.NewRateLimiter(in.clk, in.conn.RateLimit)
	for i := range in.blocks {
		if err := limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("modbus input %q: probe: %w", in.cfg.ID, err)
		}

		r := in.exchange(c, &in.blocks[i])
		if err := probeError(r.outcome); err != nil {
			return nil, fmt.Errorf("modbus input %q: probe: %w", in.cfg.ID, err)
		}
		for _, s := range r.samples {
			read[key{s.Device, s.Signal}] = s
		}
	}

	out := make([]model.Sample, 0, len(read))
	for _, d := range in.cfg.Devices {
		for _, p := range d.Points {
			if s, ok := read[key{d.Device, p.Signal}]; ok {
				out = append(out, s)
			}
		}
	}

	return out, nil
}

// probeError is the fixed text of an outcome without samples.
func probeError(o outcome) error {
	switch o {
	case outcomeBusy:
		return errors.New("the device is busy (exception 5 or 6); try again")
	case outcomeUnavailable:
		return errors.New("the gateway did not reach the device (exception 10 or 11)")
	case outcomeTimeout:
		return errors.New("the device did not answer in time")
	case outcomeConnection:
		return errors.New("the connection to the device failed")
	default:
		return nil
	}
}

// poll is the input's goroutine: it connects with backoff, then reads each
// block when its interval comes due, through the rate limiter, until ctx is
// done. A timeout or a connection error closes the client and reconnects
// after the backoff (ruling R8). The blocks are first due at start.
func (in *Input) poll(ctx context.Context, emit func(model.Record), life *input.Lifecycle, start time.Time) {
	if len(in.blocks) == 0 {
		<-ctx.Done()
		return
	}

	backoff := driverkit.NewBackoff(in.clk, in.jitter)
	limiter := driverkit.NewRateLimiter(in.clk, in.conn.RateLimit)
	next := make([]time.Time, len(in.blocks))
	for i := range next {
		next[i] = start
	}

	var c client
	defer func() {
		if c != nil {
			in.disconnect(c)
		}
	}()

	for {
		if c == nil {
			if c = in.connect(ctx, backoff); c == nil {
				return
			}
		}
		if !in.sleepUntil(ctx, slices.MinFunc(next, func(a, b time.Time) int { return a.Compare(b) })) {
			return
		}

		now := in.clk.Now()
		for i := range in.blocks {
			if next[i].After(now) {
				continue
			}
			if limiter.Wait(ctx) != nil {
				return
			}

			b := &in.blocks[i]
			next[i] = nextDue(next[i], b.Interval, now)
			r := in.exchange(c, b)
			if ctx.Err() != nil {
				return
			}
			if !in.apply(r, in.devices[i], emit, life, backoff) {
				in.disconnect(c)
				c = nil
				if backoff.Wait(ctx) != nil {
					return
				}

				break
			}
		}
	}
}

// nextDue returns the next time a block read at due is due again: one
// interval later, or one interval after now when the reads fell behind.
func nextDue(due time.Time, interval time.Duration, now time.Time) time.Time {
	next := due.Add(interval)
	if !next.After(now) {
		next = now.Add(interval)
	}

	return next
}

// sleepUntil waits on the clock until t; it reports false when ctx is done.
func (in *Input) sleepUntil(ctx context.Context, t time.Time) bool {
	if d := t.Sub(in.clk.Now()); d > 0 {
		return in.clk.Sleep(ctx, d) == nil
	}

	return ctx.Err() == nil
}

// connect opens a client, waiting the backoff after each failure, until it
// succeeds or ctx is done (nil).
func (in *Input) connect(ctx context.Context, backoff *driverkit.Backoff) client {
	for ctx.Err() == nil {
		c, err := in.newClient(in.conn)
		if err == nil {
			if err = c.Open(); err == nil {
				in.health.SetConnected(true)
				return c
			}
		}

		in.health.CountError(input.ReadErrorConnection)
		if backoff.Wait(ctx) != nil {
			return nil
		}
	}

	return nil
}

func (in *Input) disconnect(c client) {
	_ = c.Close()
	in.health.SetConnected(false)
}

// apply counts the result of one exchange and, when the device answered,
// records the read of the block's devices and emits the samples (ruling R8).
// It reports whether the connection is still usable.
func (in *Input) apply(r result, devices []string, emit func(model.Record), life *input.Lifecycle, backoff *driverkit.Backoff) bool {
	switch r.outcome {
	case outcomeTimeout:
		in.health.CountError(input.ReadErrorTimeout)
		return false
	case outcomeConnection:
		in.health.CountError(input.ReadErrorConnection)
		return false
	case outcomeUnavailable:
		in.health.CountError(input.ReadErrorException)
		return true
	case outcomeRefused, outcomeBusy:
		in.health.CountError(input.ReadErrorException)
	}
	for range r.decodeErrors {
		in.health.CountError(input.ReadErrorDecode)
	}

	backoff.Reset()
	for _, device := range devices {
		life.Read(device)
	}
	for _, s := range r.samples {
		emit(model.SampleRecord(s))
	}

	return true
}

// outcome is how a device answered one read request.
type outcome int

const (
	// outcomeValues is an answer with the values.
	outcomeValues outcome = iota
	// outcomeRefused is exceptions 1 to 4 (and 8, memory parity): the
	// device answered, its points are bad.
	outcomeRefused
	// outcomeBusy is exceptions 5 and 6: the device answered, retry at
	// the next poll.
	outcomeBusy
	// outcomeUnavailable is exceptions 10 and 11: a gateway answered for
	// a device it could not reach.
	outcomeUnavailable
	// outcomeTimeout is a request without an answer in time.
	outcomeTimeout
	// outcomeConnection is a connection that failed, or an answer that
	// cannot be trusted to be in sync with the request.
	outcomeConnection
)

// classify maps a library error to its outcome. Anything not recognised
// (I/O errors, protocol errors, a bad unit id or CRC, an unknown exception
// code) is a connection error: the stream may be out of sync, so the
// connection is reopened.
func classify(err error) outcome {
	var netErr net.Error
	switch {
	case err == nil:
		return outcomeValues
	case errors.Is(err, mbus.ErrIllegalFunction), errors.Is(err, mbus.ErrIllegalDataAddress),
		errors.Is(err, mbus.ErrIllegalDataValue), errors.Is(err, mbus.ErrServerDeviceFailure),
		errors.Is(err, mbus.ErrMemoryParityError):
		return outcomeRefused
	case errors.Is(err, mbus.ErrAcknowledge), errors.Is(err, mbus.ErrServerDeviceBusy):
		return outcomeBusy
	case errors.Is(err, mbus.ErrGWPathUnavailable), errors.Is(err, mbus.ErrGWTargetFailedToRespond):
		return outcomeUnavailable
	case errors.Is(err, mbus.ErrRequestTimedOut), errors.As(err, &netErr) && netErr.Timeout():
		return outcomeTimeout
	default:
		return outcomeConnection
	}
}

// result is the outcome of one read of a block and its samples.
type result struct {
	outcome outcome
	// samples are the block's points when the device answered with
	// values or with exceptions 1 to 4.
	samples []model.Sample
	// decodeErrors counts the points whose value did not decode.
	decodeErrors int
}

// exchange reads block b once on c. The samples carry the agent clock at
// the answer.
func (in *Input) exchange(c client, b *Block) result {
	regs, bits, err := readBlock(c, b)
	r := result{outcome: classify(err)}
	ts := in.clk.Now().UTC()

	switch r.outcome {
	case outcomeValues:
		r.samples = make([]model.Sample, 0, len(b.Points))
		for _, p := range b.Points {
			var v model.Value
			if b.Area.IsRegister() {
				v, err = DecodeRegisters(regs, p, b.Start)
			} else {
				v = DecodeBits(bits, p, b.Start)
				err = v.Validate()
			}
			if err != nil {
				r.decodeErrors++
				r.samples = append(r.samples, badSample(p, ts))
				continue
			}
			r.samples = append(r.samples, newSample(p, v, model.Good, ts))
		}
	case outcomeRefused:
		r.samples = make([]model.Sample, 0, len(b.Points))
		for _, p := range b.Points {
			r.samples = append(r.samples, badSample(p, ts))
		}
	}

	return r
}

// readBlock sends the read request of b to its unit.
func readBlock(c client, b *Block) ([]uint16, []bool, error) {
	if err := c.SetUnitId(b.Unit); err != nil {
		return nil, nil, err
	}

	switch b.Area {
	case AreaHoldingRegister:
		regs, err := c.ReadRegisters(b.Start, b.Count, mbus.HOLDING_REGISTER)
		return regs, nil, err
	case AreaInputRegister:
		regs, err := c.ReadRegisters(b.Start, b.Count, mbus.INPUT_REGISTER)
		return regs, nil, err
	case AreaCoil:
		bits, err := c.ReadCoils(b.Start, b.Count)
		return nil, bits, err
	default:
		bits, err := c.ReadDiscreteInputs(b.Start, b.Count)
		return nil, bits, err
	}
}

func newSample(p PlannedPoint, v model.Value, q model.Quality, ts time.Time) model.Sample {
	priority := p.Point.Priority
	if !priority.Valid() {
		priority = model.Critical
	}

	return model.Sample{Device: p.Device, Signal: p.Signal, TS: ts, Value: v, Quality: q, Priority: priority}
}

// badSample is a bad sample of p carrying the zero value of its type's kind
// (ruling R9).
func badSample(p PlannedPoint, ts time.Time) model.Sample {
	var v model.Value
	switch p.Point.Type {
	case "bool":
		v = model.BoolValue(false)
	case "string":
		v = model.StringValue("")
	case "float32", "float64":
		v, _ = model.FloatValue(0)
	default:
		v = model.IntValue(0)
	}

	return newSample(p, v, model.Bad, ts)
}
