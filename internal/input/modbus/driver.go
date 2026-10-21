package modbus

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
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

// Start starts the poll goroutine and the device lifecycle and returns. For
// rtu it first opens the serial port, a local resource, and fails when it
// cannot (core R47, ruling R2); a port failing later is reopened in the
// background like a network connection. For tcp and rtuovertcp the
// connection is opened in the background, so an unreachable device is death
// after the death tolerance, never a failing Start. The goroutines stop when
// ctx is done or on Close.
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

	var port client
	if in.conn.Transport == TransportRTU {
		c, err := in.newClient(in.conn)
		if err != nil {
			return fmt.Errorf("modbus input %q: start: %w", in.cfg.ID, err)
		}
		// The library's error names the device: the text is fixed.
		if err := c.Open(); err != nil {
			return fmt.Errorf("modbus input %q: start: cannot open the serial port", in.cfg.ID)
		}
		in.health.SetConnected(true)
		port = c
	}

	runCtx, cancel := context.WithCancel(ctx)
	in.cancel = cancel
	in.started = true

	life := input.NewLifecycle(in.cfg, in.clk, emit)
	start := in.clk.Now()
	in.wg.Go(func() { life.Run(runCtx) })
	in.wg.Go(func() { in.poll(runCtx, port, emit, life, start) })

	return nil
}

// Close stops the input and waits for its goroutines. A request in flight
// ends within the connection timeout and its answer is dropped. A connection
// attempt is not waited for (ruling R10): it ends on its own and the client
// it may open is closed unused. It is safe on an input never started and
// when called twice.
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
	case outcomeUnknownException:
		return errors.New("the device answered an exception code the driver does not know")
	case outcomeUnavailable:
		return errors.New("the gateway did not reach the device (exception 10 or 11)")
	case outcomeTimeout:
		return errors.New("the device did not answer in time")
	case outcomeGarbled, outcomeOutOfStep:
		return errors.New("the answer of the device could not be read (bad CRC or protocol error)")
	case outcomeConnection:
		return errors.New("the connection to the device failed")
	default:
		return nil
	}
}

// poll is the input's goroutine: it connects with backoff, unless Start
// handed it the open client c, then reads the blocks as their intervals come
// due, the one due the longest first, through the rate limiter, until ctx is
// done. The blocks are first due at start. A unit that does not answer only
// times out its own blocks (ruling R13): the connection is closed and
// reopened after the backoff on an I/O error, on a protocol error or a short
// frame on tcp (ruling R19), or when nothing answered on it for the death
// tolerance.
func (in *Input) poll(ctx context.Context, c client, emit func(model.Record), life *input.Lifecycle, start time.Time) {
	defer func() {
		if c != nil {
			in.disconnect(c)
		}
	}()
	if len(in.blocks) == 0 {
		<-ctx.Done()
		return
	}

	backoff := driverkit.NewBackoff(in.clk, in.jitter)
	limiter := driverkit.NewRateLimiter(in.clk, in.conn.RateLimit)
	tolerance := in.cfg.DeathTolerance
	if tolerance <= 0 {
		tolerance = config.DefaultDeathTolerance
	}
	next := make([]time.Time, len(in.blocks))
	for i := range next {
		next[i] = start
	}
	// answered is the last time anything answered on the connection c, or
	// the time c was opened.
	answered := start

	for {
		if c == nil {
			if c = in.connect(ctx, backoff); c == nil {
				return
			}
			answered = in.clk.Now()
		}

		i := earliest(next)
		if !in.sleepUntil(ctx, next[i]) {
			return
		}
		if limiter.Wait(ctx) != nil {
			return
		}

		b := &in.blocks[i]
		r := in.exchange(c, b)
		if ctx.Err() != nil {
			return
		}
		// Read after the rate limit wait and the exchange, both of which
		// take time: a block that fell behind is due again one interval
		// after it was actually served.
		now := in.clk.Now()
		next[i] = nextDue(next[i], b.Interval, now)

		switch in.apply(r, in.devices[i], emit, life, backoff) {
		case linkAnswered:
			answered = now
			continue
		case linkSilent:
			if now.Sub(answered) < tolerance {
				continue
			}
		}

		in.disconnect(c)
		c = nil
		if backoff.Wait(ctx) != nil {
			return
		}
	}
}

// earliest returns the index of the block due first; between blocks due at
// the same time, the first in plan order.
func earliest(next []time.Time) int {
	first := 0
	for i := range next {
		if next[i].Before(next[first]) {
			first = i
		}
	}

	return first
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
		c, err := in.open(ctx)
		if err == nil {
			in.health.SetConnected(true)
			return c
		}
		if ctx.Err() != nil {
			return nil
		}

		in.health.CountError(input.ReadErrorConnection)
		if backoff.Wait(ctx) != nil {
			return nil
		}
	}

	return nil
}

// open builds a client and opens it in a goroutine of its own, waiting for
// it or for ctx (ruling R10): the library dials with a fixed timeout that
// nothing cancels, and Close must not wait for it. When ctx is done first,
// open returns at once and the goroutine closes the client if its Open
// succeeds late. The goroutine never touches the health nor emits.
func (in *Input) open(ctx context.Context) (client, error) {
	c, err := in.newClient(in.conn)
	if err != nil {
		return nil, err
	}

	// Unbuffered: the result is handed over only to a waiting open, so
	// exactly one side owns the client.
	opened := make(chan error)
	go func() {
		err := c.Open()
		select {
		case opened <- err:
		case <-ctx.Done():
			if err == nil {
				_ = c.Close()
			}
		}
	}()

	select {
	case err := <-opened:
		if err != nil {
			return nil, err
		}
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (in *Input) disconnect(c client) {
	_ = c.Close()
	in.health.SetConnected(false)
}

// link is what one exchange tells of the connection.
type link int

const (
	// linkAnswered: a device or a gateway answered, the connection works.
	linkAnswered link = iota
	// linkSilent: nothing usable came back (a timeout, a garbled answer);
	// the library resynchronises by itself, so the connection is kept.
	linkSilent
	// linkBroken: an I/O error, or a tcp stream out of step; the
	// connection must be reopened.
	linkBroken
)

// apply counts the result of one exchange and, when the device answered,
// records the read of the block's devices and emits the samples (rulings R8,
// R13, R15 and R19). It reports what the exchange tells of the connection.
func (in *Input) apply(r result, devices []string, emit func(model.Record), life *input.Lifecycle, backoff *driverkit.Backoff) link {
	switch r.outcome {
	case outcomeTimeout:
		in.health.CountError(input.ReadErrorTimeout)
		return linkSilent
	case outcomeGarbled:
		in.health.CountError(input.ReadErrorConnection)
		return linkSilent
	case outcomeConnection, outcomeOutOfStep:
		in.health.CountError(input.ReadErrorConnection)
		return linkBroken
	case outcomeUnavailable:
		in.health.CountError(input.ReadErrorException)
		return linkAnswered
	case outcomeRefused, outcomeBusy, outcomeUnknownException:
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

	return linkAnswered
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
	// outcomeUnknownException is an exception code the library does not
	// know (ruling R15): the device answered, like exceptions 5 and 6.
	outcomeUnknownException
	// outcomeUnavailable is exceptions 10 and 11: a gateway answered for
	// a device it could not reach.
	outcomeUnavailable
	// outcomeTimeout is a request without an answer in time.
	outcomeTimeout
	// outcomeGarbled is an answer that cannot be used: a bad CRC, a short
	// frame or a protocol error on rtu and rtuovertcp, another unit or
	// transaction. The library skips or flushes it, so the connection
	// stays usable.
	outcomeGarbled
	// outcomeOutOfStep is a protocol error or a short frame on tcp (ruling
	// R19): the library may have stopped inside a frame, read its header
	// and not its PDU, and it never resynchronises the MBAP stream, so the
	// connection is reopened.
	outcomeOutOfStep
	// outcomeConnection is an I/O error: the connection failed.
	outcomeConnection
)

// unknownExceptionPrefix starts the error the library returns for an
// exception code it does not map, a plain fmt.Errorf("unknown exception code
// (%v)") with no error value to match (simonvetter/modbus v1.6.4, modbus.go
// line 88, mapExceptionCodeToError).
const unknownExceptionPrefix = "unknown exception code ("

// classify maps a library error on a connection of transport to its
// outcome. Anything not recognised is an I/O error (EOF, reset, refused, a
// write failure, a closed port), and the connection is reopened.
func classify(err error, transport string) outcome {
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
	case strings.HasPrefix(err.Error(), unknownExceptionPrefix):
		return outcomeUnknownException
	case errors.Is(err, mbus.ErrGWPathUnavailable), errors.Is(err, mbus.ErrGWTargetFailedToRespond):
		return outcomeUnavailable
	case errors.Is(err, mbus.ErrRequestTimedOut), errors.As(err, &netErr) && netErr.Timeout():
		return outcomeTimeout
	case transport == TransportTCP && (errors.Is(err, mbus.ErrProtocolError) || errors.Is(err, mbus.ErrShortFrame)):
		return outcomeOutOfStep
	case errors.Is(err, mbus.ErrBadCRC), errors.Is(err, mbus.ErrShortFrame), errors.Is(err, mbus.ErrProtocolError),
		errors.Is(err, mbus.ErrBadUnitId), errors.Is(err, mbus.ErrBadTransactionId), errors.Is(err, mbus.ErrUnknownProtocolId):
		return outcomeGarbled
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
	r := result{outcome: classify(err, in.conn.Transport)}
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
