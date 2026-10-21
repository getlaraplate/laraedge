package modbus

import (
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mbus "github.com/simonvetter/modbus"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

// idleTimers is the number of clock timers of a started input at rest: the
// lifecycle's next check and the poll goroutine's wait (next poll, backoff
// or rate limit).
const idleTimers = 2

func pollPoint(signal, address, typ string, interval time.Duration) config.Point {
	return config.Point{
		Signal:   signal,
		Address:  address,
		Type:     typ,
		Read:     config.Read{Mode: config.ReadPoll, Interval: interval},
		Priority: model.Critical,
	}
}

func modbusInput(connection []byte, tolerance time.Duration, devices ...config.Device) config.Input {
	return config.Input{
		ID:             "line",
		Driver:         config.DriverModbus,
		Connection:     connection,
		DeathTolerance: tolerance,
		Devices:        devices,
	}
}

// harness runs one input on a fake clock and records what it emits.
type harness struct {
	t   *testing.T
	clk *clock.Fake
	in  *Input

	mu      sync.Mutex
	records []model.Record

	// closed is set once Close returned; late counts the records emitted
	// after it.
	closed atomic.Bool
	late   atomic.Int32
}

// newHarness builds the input of cfg on a fake clock, with a backoff jitter
// of 0 (waits of 0.5 s, 1 s, 2 s...).
func newHarness(t *testing.T, cfg config.Input) *harness {
	t.Helper()

	clk := clock.NewFake(epoch)
	in, err := New(cfg, clk)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in.jitter = func() float64 { return 0 }

	return &harness{t: t, clk: clk, in: in}
}

// start starts the input and waits until its first poll is done.
func (h *harness) start() {
	h.t.Helper()

	if err := h.in.Start(h.t.Context(), h.emit); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	h.t.Cleanup(h.close)
	h.idle()
}

func (h *harness) emit(r model.Record) {
	if h.closed.Load() {
		h.late.Add(1)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, r)
}

func (h *harness) close() {
	if err := h.in.Close(); err != nil {
		h.t.Errorf("Close: %v", err)
	}
	h.closed.Store(true)
}

// idle waits until the input's goroutines are blocked on the clock.
func (h *harness) idle() {
	h.t.Helper()
	waitPending(h.t, h.clk, idleTimers)
}

// step advances the clock by d and waits until the input is idle again.
func (h *harness) step(d time.Duration) {
	h.t.Helper()
	h.clk.Advance(d)
	h.idle()
}

// take returns the records emitted since the last take.
func (h *harness) take() []model.Record {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := h.records
	h.records = nil

	return out
}

// reading is a sample reduced to what the tests compare.
type reading struct {
	device, signal string
	value          model.Value
	quality        model.Quality
	ts             time.Time
}

func readings(t *testing.T, records []model.Record) []reading {
	t.Helper()

	var out []reading
	for _, r := range records {
		if r.Sample == nil {
			continue
		}
		if err := r.Sample.Validate(); err != nil {
			t.Errorf("invalid sample %+v: %v", *r.Sample, err)
		}
		out = append(out, reading{r.Sample.Device, r.Sample.Signal, r.Sample.Value, r.Sample.Quality, r.Sample.TS})
	}

	return out
}

// events lists the device events as "<kind> <device>".
func events(records []model.Record) []string {
	var out []string
	for _, r := range records {
		if r.Event != nil {
			out = append(out, r.Event.Kind.String()+" "+r.Event.Device)
		}
	}

	return out
}

func floatOf(t *testing.T, f float64) model.Value {
	t.Helper()

	v, err := model.FloatValue(f)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

func TestPollsEachBlockAtItsIntervalWithTheAgentClock(t *testing.T) {
	dev := newTestDevice()
	level := math.Float32bits(1.5)
	dev.set(func(d *testDevice) {
		d.holding[0] = 0xFFFE // -2 as int16
		d.holding[1] = 7
		d.coils[5] = true
		d.inputs[10] = uint16(level >> 16)
		d.inputs[11] = uint16(level)
	})
	srv := startServer(t, dev)

	h := newHarness(t, modbusInput(srv.connection(`"timeout":"2s"`), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{
			pollPoint("temp", "hr:0", "int16", time.Second),
			pollPoint("count", "hr:1", "uint16", time.Second),
			pollPoint("door", "co:5", "bool", 2*time.Second),
			pollPoint("level", "ir:10@2", "float32", 2*time.Second),
		},
	}))
	h.start()

	every := []reading{
		{"press-07", "temp", model.IntValue(-2), model.Good, epoch},
		{"press-07", "count", model.IntValue(7), model.Good, epoch},
	}
	slow := []reading{
		{"press-07", "door", model.BoolValue(true), model.Good, epoch},
		{"press-07", "level", floatOf(t, 1.5), model.Good, epoch},
	}
	at := func(rs []reading, ts time.Time) []reading {
		out := slices.Clone(rs)
		for i := range out {
			out[i].ts = ts
		}
		return out
	}
	sortReadings := func(rs []reading) []reading {
		slices.SortFunc(rs, func(a, b reading) int { return strings.Compare(a.signal, b.signal) })
		return rs
	}

	steps := []struct {
		advance time.Duration
		want    []reading
	}{
		{0, append(slices.Clone(every), slow...)},
		{time.Second, at(every, epoch.Add(time.Second))},
		{time.Second, append(at(every, epoch.Add(2*time.Second)), at(slow, epoch.Add(2*time.Second))...)},
		{time.Second, at(every, epoch.Add(3*time.Second))},
	}
	for i, s := range steps {
		if s.advance > 0 {
			h.step(s.advance)
		}
		records := h.take()
		if got := events(records); len(got) != 0 {
			t.Errorf("step %d: events %v, want none", i, got)
		}
		got := sortReadings(readings(t, records))
		if want := sortReadings(s.want); !slices.Equal(got, want) {
			t.Errorf("step %d: samples\n got %+v\nwant %+v", i, got, want)
		}
	}

	// Blocks are read in plan order: unit, then area, then interval.
	wantRequests := []request{
		{1, AreaCoil, 5, 1}, {1, AreaHoldingRegister, 0, 2}, {2, AreaInputRegister, 10, 2},
		{1, AreaHoldingRegister, 0, 2},
		{1, AreaCoil, 5, 1}, {1, AreaHoldingRegister, 0, 2}, {2, AreaInputRegister, 10, 2},
		{1, AreaHoldingRegister, 0, 2},
	}
	if got := dev.served(); !slices.Equal(got, wantRequests) {
		t.Errorf("requests\n got %+v\nwant %+v", got, wantRequests)
	}
	if n := dev.writeCount(); n != 0 {
		t.Errorf("the driver sent %d writes", n)
	}

	health := h.in.Health()
	if !health.Connected || len(health.ReadErrors) != 0 {
		t.Errorf("Health = %+v, want connected without errors", health)
	}
}

func TestExceptionOutcomes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		// bad: the block's points get bad samples; alive: the device
		// answered, so it is not reported dead.
		bad, alive bool
	}{
		{"1 illegal function", mbus.ErrIllegalFunction, true, true},
		{"2 illegal data address", mbus.ErrIllegalDataAddress, true, true},
		{"3 illegal data value", mbus.ErrIllegalDataValue, true, true},
		{"4 server device failure", mbus.ErrServerDeviceFailure, true, true},
		{"5 acknowledge", mbus.ErrAcknowledge, false, true},
		{"6 server device busy", mbus.ErrServerDeviceBusy, false, true},
		{"10 gateway path unavailable", mbus.ErrGWPathUnavailable, false, false},
		{"11 gateway target failed to respond", mbus.ErrGWTargetFailedToRespond, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dev := newTestDevice()
			dev.set(func(d *testDevice) {
				d.holding[0], d.holding[1], d.holding[2] = 1, 2, 3
				d.errs[1] = tt.err
				d.holding[20] = 42
			})
			srv := startServer(t, dev)

			h := newHarness(t, modbusInput(srv.connection(""), 3*time.Second,
				config.Device{Device: "a", Points: []config.Point{
					pollPoint("x", "hr:0", "int16", time.Second),
					pollPoint("y", "hr:1", "float32", time.Second),
				}},
				config.Device{Device: "b", Points: []config.Point{
					pollPoint("z", "hr:20", "uint16", time.Second),
				}},
			))
			h.start()

			want := []reading{{"b", "z", model.IntValue(42), model.Good, epoch}}
			if tt.bad {
				want = append([]reading{
					{"a", "x", model.IntValue(0), model.Bad, epoch},
					{"a", "y", floatOf(t, 0), model.Bad, epoch},
				}, want...)
			}
			if got := readings(t, h.take()); !slices.Equal(got, want) {
				t.Errorf("samples\n got %+v\nwant %+v", got, want)
			}

			health := h.in.Health()
			if !health.Connected {
				t.Error("an exception closed the connection")
			}
			if health.ReadErrors[input.ReadErrorException] != 1 || len(health.ReadErrors) != 1 {
				t.Errorf("ReadErrors = %v, want one exception", health.ReadErrors)
			}

			var evs []string
			for range 3 {
				h.step(time.Second)
				evs = append(evs, events(h.take())...)
			}
			wantEvents := []string{"death a"}
			if tt.alive {
				wantEvents = nil
			}
			if !slices.Equal(evs, wantEvents) {
				t.Errorf("events = %v, want %v", evs, wantEvents)
			}
			if health := h.in.Health(); !health.Connected || health.ReadErrors[input.ReadErrorConnection] != 0 {
				t.Errorf("Health = %+v, want the connection kept", health)
			}
		})
	}
}

func TestUnmappedAddressMakesOnlyItsBlockBad(t *testing.T) {
	dev := newTestDevice()
	dev.set(func(d *testDevice) {
		d.holding[0] = 0
		// hr:1 is not mapped: the block hr:0-2 fails with exception 2.
		d.holding[2] = 5
		d.holding[10] = 9
	})
	srv := startServer(t, dev)

	h := newHarness(t, modbusInput(srv.connection(""), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{
			pollPoint("total", "hr:0", "uint32", time.Second),
			pollPoint("mode", "hr:2", "int16", time.Second),
			pollPoint("speed", "hr:10", "int16", time.Second),
		},
	}))
	h.start()

	want := []reading{
		{"press-07", "total", model.IntValue(0), model.Bad, epoch},
		{"press-07", "mode", model.IntValue(0), model.Bad, epoch},
		{"press-07", "speed", model.IntValue(9), model.Good, epoch},
	}
	if got := readings(t, h.take()); !slices.Equal(got, want) {
		t.Errorf("samples\n got %+v\nwant %+v", got, want)
	}
}

func TestDecodeErrorIsABadSample(t *testing.T) {
	dev := newTestDevice()
	dev.set(func(d *testDevice) {
		d.holding[0] = 0xFF00 // 0xFF is not UTF-8
		d.holding[1] = 3
	})
	srv := startServer(t, dev)

	h := newHarness(t, modbusInput(srv.connection(""), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{
			pollPoint("label", "hr:0#1", "string", time.Second),
			pollPoint("mode", "hr:1", "int16", time.Second),
		},
	}))
	h.start()

	want := []reading{
		{"press-07", "label", model.StringValue(""), model.Bad, epoch},
		{"press-07", "mode", model.IntValue(3), model.Good, epoch},
	}
	if got := readings(t, h.take()); !slices.Equal(got, want) {
		t.Errorf("samples\n got %+v\nwant %+v", got, want)
	}
	if got := h.in.Health().ReadErrors; got[input.ReadErrorDecode] != 1 {
		t.Errorf("ReadErrors = %v, want one decode error", got)
	}
}

// TestServerStopsAndRestarts is the review focus: a device that stops
// answering mid-run gives no samples, is disconnected, dies after the death
// tolerance and is born again, with data, once the server is back.
func TestServerStopsAndRestarts(t *testing.T) {
	dev := newTestDevice()
	dev.set(func(d *testDevice) { d.holding[0] = 11 })
	srv := startServer(t, dev)

	h := newHarness(t, modbusInput(srv.connection(""), 3*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}))
	h.start()

	if got := readings(t, h.take()); len(got) != 1 || got[0].quality != model.Good {
		t.Fatalf("first poll: %+v", got)
	}
	if !h.in.Health().Connected {
		t.Fatal("not connected after the first poll")
	}

	srv.stop()
	h.step(time.Second)
	if got := h.take(); len(got) != 0 {
		t.Fatalf("records after the server stopped: %+v", got)
	}
	health := h.in.Health()
	if health.Connected {
		t.Error("connected after the server stopped")
	}
	if health.ReadErrors[input.ReadErrorConnection] == 0 {
		t.Errorf("ReadErrors = %v, want connection errors", health.ReadErrors)
	}

	// Last read at epoch, tolerance 3 s: death at epoch+3s, nothing else.
	var records []model.Record
	for range 3 {
		h.step(time.Second)
		records = append(records, h.take()...)
	}
	if got := events(records); !slices.Equal(got, []string{"death press-07"}) {
		t.Fatalf("events while stopped = %v, want one death", got)
	}
	if got := readings(t, records); len(got) != 0 {
		t.Fatalf("samples while stopped: %+v", got)
	}

	dev.set(func(d *testDevice) { d.holding[0] = 12 })
	srv.start()

	// The backoff has grown; step until the input reconnects.
	for range 120 {
		h.step(time.Second)
		records = h.take()
		if len(records) > 0 {
			break
		}
	}
	if len(records) != 2 || records[0].Event == nil || records[0].Event.Kind != model.Birth {
		t.Fatalf("records after the restart = %+v, want a birth then a sample", records)
	}
	if got := readings(t, records[1:]); len(got) != 1 || got[0].value != model.IntValue(12) || got[0].quality != model.Good || !got[0].ts.Equal(h.clk.Now()) {
		t.Fatalf("sample after the restart = %+v", got)
	}
	if !h.in.Health().Connected {
		t.Error("not connected after the restart")
	}
}

// TestTimeoutKeepsTheConnectionUntilTheDeathTolerance (ruling R13): a
// timeout counts toward death but keeps the connection. The held request
// blocks the library server's connection, so nothing answers on it: once the
// death tolerance passed without an answer, the connection is closed and
// reopened after the backoff, and the data comes back on the new one.
func TestTimeoutKeepsTheConnectionUntilTheDeathTolerance(t *testing.T) {
	dev := newTestDevice()
	dev.set(func(d *testDevice) { d.holding[0] = 11 })
	release := dev.holdAt(0)
	srv := startServer(t, dev)
	t.Cleanup(release)

	h := newHarness(t, modbusInput(srv.connection(`"timeout":"100ms"`), 2*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}))
	h.start()

	for i, want := range []uint64{1, 2} {
		if i > 0 {
			h.step(time.Second)
		}
		if got := h.take(); len(got) != 0 {
			t.Fatalf("records after a timeout: %+v", got)
		}
		health := h.in.Health()
		if !health.Connected || health.ReadErrors[input.ReadErrorTimeout] != want || len(health.ReadErrors) != 1 {
			t.Fatalf("Health = %+v, want connected after %d timeouts", health, want)
		}
	}

	h.step(time.Second)
	if got := events(h.take()); !slices.Equal(got, []string{"death press-07"}) {
		t.Fatalf("events = %v, want one death", got)
	}
	if health := h.in.Health(); health.Connected || health.ReadErrors[input.ReadErrorTimeout] != 3 {
		t.Fatalf("Health = %+v, want disconnected after 3 timeouts over the death tolerance", health)
	}

	release()
	var records []model.Record
	for range 120 {
		h.step(time.Second)
		records = h.take()
		if len(records) > 0 {
			break
		}
	}
	if got := events(records); !slices.Equal(got, []string{"birth press-07"}) {
		t.Fatalf("events after the device answered = %v, want a birth", got)
	}
	if got := readings(t, records); len(got) != 1 || got[0].value != model.IntValue(11) {
		t.Fatalf("samples after the device answered = %+v", got)
	}
	if !h.in.Health().Connected {
		t.Error("not connected after the device answered")
	}
}

// TestCloseDuringAnInFlightRequest is the review focus: Close returns once the
// request times out, and nothing is emitted afterwards.
func TestCloseDuringAnInFlightRequest(t *testing.T) {
	dev := newTestDevice()
	dev.set(func(d *testDevice) { d.holding[0] = 11 })
	release := dev.holdAt(0)
	srv := startServer(t, dev)
	t.Cleanup(release)

	h := newHarness(t, modbusInput(srv.connection(`"timeout":"300ms"`), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}))
	if err := h.in.Start(t.Context(), h.emit); err != nil {
		t.Fatal(err)
	}

	select {
	case <-dev.arrived:
	case <-time.After(guard):
		t.Fatal("the request never reached the server")
	}

	done := make(chan error, 1)
	go func() { done <- h.in.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(guard):
		t.Fatal("Close did not return")
	}
	h.closed.Store(true)

	// Once the server answers, the input must stay silent.
	release()
	if got := h.take(); len(got) != 0 {
		t.Errorf("records emitted for a request cut by Close: %+v", got)
	}
	if n := h.late.Load(); n != 0 {
		t.Errorf("%d records emitted after Close returned", n)
	}
	if h.in.Health().Connected {
		t.Error("connected after Close")
	}
	if err := h.in.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestRateLimitBoundsRequestsPerSecond(t *testing.T) {
	dev := newTestDevice()
	var points []config.Point
	for i := range 5 {
		addr := uint16(i * 10)
		dev.set(func(d *testDevice) { d.holding[addr] = addr })
		points = append(points, pollPoint("s"+strconv.Itoa(i), "hr:"+strconv.Itoa(int(addr)), "int16", time.Second))
	}
	srv := startServer(t, dev)

	h := newHarness(t, modbusInput(srv.connection(`"rate_limit":2`), 30*time.Second, config.Device{Device: "press-07", Points: points}))
	h.start()

	if got := len(dev.served()); got != 2 {
		t.Fatalf("requests in the first second = %d, want 2", got)
	}
	for second := 1; second <= 5; second++ {
		before := len(dev.served())
		h.step(time.Second)
		if got := len(dev.served()) - before; got != 2 {
			t.Fatalf("requests in second %d = %d, want 2", second, got)
		}
	}
}

func TestProbeReadsEveryPointOnceWithoutStarting(t *testing.T) {
	dev := newTestDevice()
	dev.set(func(d *testDevice) {
		d.holding[0] = 5
		d.holding[1] = 6
		d.errs[11] = mbus.ErrIllegalDataAddress
		d.discrete[3] = true
	})
	srv := startServer(t, dev)

	clk := clock.NewFake(epoch)
	in, err := New(modbusInput(srv.connection(""), 30*time.Second,
		config.Device{Device: "press-07", Points: []config.Point{
			pollPoint("b", "hr:1", "int16", time.Second),
			pollPoint("bad", "hr:11", "int16", 5*time.Second),
			pollPoint("a", "hr:0", "int16", time.Second),
		}},
		config.Device{Device: "oven-02", Points: []config.Point{
			pollPoint("open", "di:3", "bool", time.Second),
		}},
	), clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close() })

	samples, err := in.Probe(t.Context())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	var got []reading
	for _, s := range samples {
		if err := s.Validate(); err != nil {
			t.Errorf("invalid sample %+v: %v", s, err)
		}
		got = append(got, reading{s.Device, s.Signal, s.Value, s.Quality, s.TS})
	}
	want := []reading{
		{"press-07", "b", model.IntValue(6), model.Good, epoch},
		{"press-07", "bad", model.IntValue(0), model.Bad, epoch},
		{"press-07", "a", model.IntValue(5), model.Good, epoch},
		{"oven-02", "open", model.BoolValue(true), model.Good, epoch},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Probe\n got %+v\nwant %+v", got, want)
	}
	if n := len(dev.served()); n != 3 {
		t.Errorf("Probe sent %d requests, want one per block (3)", n)
	}
	if n := clk.PendingTimers(); n != 0 {
		t.Errorf("Probe left %d timers: it started a loop", n)
	}
}

func TestProbeErrorsAreFixedTexts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, d *testDevice, s *testServer)
		want  string
	}{
		{"unreachable", func(_ *testing.T, _ *testDevice, s *testServer) { s.stop() }, "cannot connect to the device"},
		{"timeout", func(t *testing.T, d *testDevice, _ *testServer) { t.Cleanup(d.holdAt(0)) }, "the device did not answer in time"},
		{"busy", func(_ *testing.T, d *testDevice, _ *testServer) {
			d.set(func(d *testDevice) { d.errs[0] = mbus.ErrServerDeviceBusy })
		}, "the device is busy"},
		{"gateway", func(_ *testing.T, d *testDevice, _ *testServer) {
			d.set(func(d *testDevice) { d.errs[0] = mbus.ErrGWTargetFailedToRespond })
		}, "the gateway did not reach the device"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dev := newTestDevice()
			dev.set(func(d *testDevice) { d.holding[0] = 1 })
			srv := startServer(t, dev)
			tt.setup(t, dev, srv)

			in, err := New(modbusInput(srv.connection(`"timeout":"100ms"`), 30*time.Second, config.Device{
				Device: "press-07",
				Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
			}), clock.NewFake(epoch))
			if err != nil {
				t.Fatal(err)
			}

			_, err = in.Probe(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Probe error = %v, want it to contain %q", err, tt.want)
			}
			for _, leak := range []string{srv.host, strconv.Itoa(int(srv.port))} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("Probe error %q names the connection (%s)", err, leak)
				}
			}
		})
	}
}

func TestNewRefusesInvalidConfiguration(t *testing.T) {
	const conn = `{"transport":"tcp","host":"plc-hidden.example","port":1502}`
	good := pollPoint("ok", "hr:0", "int16", time.Second)
	subscribe := good
	subscribe.Read = config.Read{Mode: config.ReadSubscribe}

	tests := []struct {
		name       string
		connection string
		points     []config.Point
		want       string
	}{
		{"connection", `{"transport":"tcp"}`, []config.Point{good}, `modbus input "line": connection.host: required for tcp`},
		{"unknown connection field", `{"transport":"tcp","host":"h","secret_token":"hunter2"}`, []config.Point{good}, `modbus input "line": connection.secret_token: unknown field`},
		{"address", conn, []config.Point{good, pollPoint("x", "zz:secret-9", "int16", time.Second)}, `modbus input "line": devices.0.points.1: address: area: want hr, ir, co or di`},
		{"type against area", conn, []config.Point{pollPoint("x", "hr:3", "bool", time.Second)}, `modbus input "line": devices.0.points.0: type bool: only on co or di`},
		{"read mode", conn, []config.Point{subscribe}, `modbus input "line": devices.0.points.0: read: only poll is supported`},
		{"end of the address space", conn, []config.Point{pollPoint("x", "hr:65535", "int32", time.Second)}, `modbus input "line": devices.0.points.0: address: offset and size go beyond 65535`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(modbusInput([]byte(tt.connection), 0, config.Device{Device: "d", Points: tt.points}), clock.NewFake(epoch))
			if err == nil || err.Error() != tt.want {
				t.Fatalf("New error = %v, want %q", err, tt.want)
			}
			for _, leak := range []string{"plc-hidden", "1502", "hunter2", "secret-9"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q echoes a value", err)
				}
			}
		})
	}
}

func TestFactoryNeverConnects(t *testing.T) {
	dev := newTestDevice()
	srv := startServer(t, dev)
	cfg := modbusInput(srv.connection(""), 0, config.Device{Device: "d", Points: []config.Point{pollPoint("ok", "hr:0", "int16", time.Second)}})

	in, err := Factory()(cfg, clock.NewFake(epoch))
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	if _, ok := in.(input.HealthReporter); !ok {
		t.Error("the Modbus input does not report its health")
	}
	if err := in.Close(); err != nil {
		t.Errorf("Close of an input never started: %v", err)
	}
	if n := len(dev.served()); n != 0 {
		t.Errorf("the factory sent %d requests", n)
	}
	if err := in.Start(t.Context(), func(model.Record) {}); err == nil {
		t.Error("Start after Close succeeded")
	}
}

func TestStartDoesNotWaitForAnUnreachableDevice(t *testing.T) {
	srv := startServer(t, newTestDevice())
	srv.stop()

	h := newHarness(t, modbusInput(srv.connection(""), 3*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}))
	h.start()

	if h.in.Health().Connected {
		t.Error("connected to a stopped server")
	}
	var records []model.Record
	for range 3 {
		h.step(time.Second)
		records = append(records, h.take()...)
	}
	if got := events(records); !slices.Equal(got, []string{"death press-07"}) {
		t.Errorf("events = %v, want one death", got)
	}
}

// TestLibraryWritesNothingToStdout: the library logs to stdout unless given
// a logger; its log lines carry the host and port, so they must never reach
// the agent's stdout, which carries the JSON logs.
func TestLibraryWritesNothingToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	c, err := newLibraryClient(Connection{Transport: TransportTCP, Host: "127.0.0.1", Port: 1, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// A quantity of 0 is refused by the library, which logs an error.
	if _, err := c.ReadRegisters(0, 0, mbus.HOLDING_REGISTER); !errors.Is(err, mbus.ErrUnexpectedParameters) {
		t.Fatalf("ReadRegisters(0, 0) = %v", err)
	}

	os.Stdout = stdout
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("the library wrote to stdout: %q", out)
	}
}
