package serial

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
)

// runEpoch is the fake clock's start in the input tests.
var runEpoch = time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)

const (
	// runTolerance is the death tolerance of the test inputs.
	runTolerance = 5 * time.Second
	// idleTimers is the number of clock timers of a started input at rest:
	// the lifecycle's next check and either the port's read wait or the
	// backoff wait.
	idleTimers = 2
)

// errUnplugged is the error of a fake port that failed.
var errUnplugged = errors.New("fake port: device unplugged")

// fakePort is a Port on the fake clock. Read returns the next queued chunk at
// once; with nothing queued it waits ReadTimeout on the clock, then returns
// the chunk queued meanwhile or 0, nil. It returns 0, nil at once when ctx is
// done, so an input being closed never waits on the clock. fail makes the
// next Read and Write return an error until the port is opened again.
type fakePort struct {
	clk *clock.Fake
	ctx context.Context

	mu     sync.Mutex
	queue  [][]byte
	failed bool
	// writeFailed makes only Write fail.
	writeFailed bool
	open        bool
	writes      [][]byte
}

func (p *fakePort) Read(b []byte) (int, error) {
	if n, ok, err := p.take(b); ok {
		return n, err
	}
	if p.clk.Sleep(p.ctx, ReadTimeout) != nil {
		return 0, nil
	}
	n, _, err := p.take(b)
	return n, err
}

// take returns the failure or the next chunk; ok is false when there is
// neither.
func (p *fakePort) take(b []byte) (int, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed {
		return 0, true, errUnplugged
	}
	if len(p.queue) == 0 {
		return 0, false, nil
	}
	n := copy(b, p.queue[0])
	if n < len(p.queue[0]) {
		p.queue[0] = p.queue[0][n:]
	} else {
		p.queue = p.queue[1:]
	}
	return n, true, nil
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed || p.writeFailed {
		return 0, errUnplugged
	}
	p.writes = append(p.writes, bytes.Clone(b))
	return len(b), nil
}

func (p *fakePort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.open = false
	return nil
}

// feed queues one chunk, read whole by one Read.
func (p *fakePort) feed(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, []byte(s))
}

// failWrite makes the next Write fail, until the port is opened again.
func (p *fakePort) failWrite() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeFailed = true
}

// fail makes the next Read and Write fail, until the port is opened again.
func (p *fakePort) fail() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed = true
}

func (p *fakePort) written() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.writes))
	for i, w := range p.writes {
		out[i] = string(w)
	}
	return out
}

func (p *fakePort) isOpen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.open
}

// fakeOpener opens the one fakePort of a harness. Each call takes the next
// queued result: an error, or nil for a successful open. With nothing queued
// the open succeeds.
type fakeOpener struct {
	port *fakePort

	mu      sync.Mutex
	results []error
	calls   int
}

func (o *fakeOpener) open(device string, mode Mode) (Port, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if len(o.results) > 0 {
		err := o.results[0]
		o.results = o.results[1:]
		if err != nil {
			return nil, err
		}
	}
	o.port.mu.Lock()
	o.port.failed = false
	o.port.writeFailed = false
	o.port.open = true
	o.port.mu.Unlock()
	return o.port, nil
}

// then queues the results of the next calls.
func (o *fakeOpener) then(results ...error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.results = append(o.results, results...)
}

func (o *fakeOpener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

// runHarness runs one serial input on a fake clock and a fake port and
// records what it emits.
type runHarness struct {
	t      *testing.T
	clk    *clock.Fake
	cfg    config.Input
	in     *Input
	port   *fakePort
	opener *fakeOpener
	logs   *logBuffer
	stop   context.CancelFunc

	mu      sync.Mutex
	records []model.Record
}

// gaugeConnection is the connection of the test inputs: a terminator \r and
// the serial context key from SN<digits>.
func gaugeConnection(extra map[string]any) json.RawMessage {
	fields := map[string]any{
		"device":  "/dev/ttyUSB0",
		"baud":    9600,
		"context": []map[string]any{{"key": "serial", "match": `^SN(\d+)$`}},
	}
	for k, v := range extra {
		fields[k] = v
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return raw
}

// gaugePoint is an event point of type typ at address.
func gaugePoint(signal, address, typ string) config.Point {
	return config.Point{Signal: signal, Address: address, Type: typ, Read: config.Read{Mode: config.ReadEvent}, Priority: model.Critical}
}

// newRunHarness builds an input with two devices, a (point w from 01A) and b
// (point w from 02A), on the connection fields of gaugeConnection plus extra.
// The backoff jitter is 0: waits of 0.5 s, 1 s, 2 s, 4 s...
func newRunHarness(t *testing.T, extra map[string]any) *runHarness {
	t.Helper()
	cfg := config.Input{
		ID:             "gauges",
		Driver:         config.DriverSerial,
		Connection:     gaugeConnection(extra),
		DeathTolerance: runTolerance,
		Devices: []config.Device{
			{Device: "a", Points: []config.Point{gaugePoint("w", `^01A(.+)$`, "float64")}},
			{Device: "b", Points: []config.Point{gaugePoint("w", `^02A(.+)$`, "float64")}},
		},
	}
	return newRunHarnessFor(t, cfg)
}

func newRunHarnessFor(t *testing.T, cfg config.Input) *runHarness {
	t.Helper()
	clk := clock.NewFake(runEpoch)
	ctx, stop := context.WithCancel(context.Background())
	port := &fakePort{clk: clk, ctx: ctx}
	h := &runHarness{t: t, clk: clk, cfg: cfg, port: port, opener: &fakeOpener{port: port}, logs: &logBuffer{}, stop: stop}
	in, err := newInput(cfg, clk, Options{Logger: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}, deps{
		open:   h.opener.open,
		jitter: func() float64 { return 0 },
	})
	if err != nil {
		t.Fatalf("newInput: %v", err)
	}
	h.in = in
	return h
}

// start starts the input and waits until it rests.
func (h *runHarness) start() {
	h.t.Helper()
	if err := h.in.Start(h.t.Context(), h.emit); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	h.t.Cleanup(h.close)
	h.clk.BlockUntil(h.t, idleTimers)
}

// close lets a pending read return, then closes the input.
func (h *runHarness) close() {
	h.stop()
	if err := h.in.Close(); err != nil {
		h.t.Errorf("Close: %v", err)
	}
}

func (h *runHarness) emit(r model.Record) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
}

// step moves the clock one read cycle forward and waits until the input
// rests again.
func (h *runHarness) step() {
	h.t.Helper()
	h.clk.Advance(ReadTimeout)
	h.clk.BlockUntil(h.t, idleTimers)
}

// advance moves the clock d forward one read cycle at a time.
func (h *runHarness) advance(d time.Duration) {
	h.t.Helper()
	for range int(d / ReadTimeout) {
		h.step()
	}
}

// stepUntil steps until cond holds, at most limit read cycles.
func (h *runHarness) stepUntil(limit int, cond func() bool) {
	h.t.Helper()
	for range limit {
		if cond() {
			return
		}
		h.step()
	}
	if !cond() {
		h.t.Fatalf("condition not met after %d read cycles", limit)
	}
}

func (h *runHarness) samples() []model.Sample {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []model.Sample
	for _, r := range h.records {
		if r.Sample != nil {
			out = append(out, *r.Sample)
		}
	}
	return out
}

// events returns the device events of kind.
func (h *runHarness) events(kind model.EventKind) []model.DeviceEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []model.DeviceEvent
	for _, r := range h.records {
		if r.Event != nil && r.Event.Kind == kind {
			out = append(out, *r.Event)
		}
	}
	return out
}

func (h *runHarness) recordCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

func (h *runHarness) readErrors(kind string) uint64 {
	return h.in.Health().ReadErrors[kind]
}

// logBuffer collects the JSON log lines of a logger, safe for concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the decoded log lines.
func (b *logBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b.buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// at returns the log lines of level whose message is msg.
func (b *logBuffer) at(t *testing.T, level, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range b.lines(t) {
		if m["level"] == level && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}
