package mqtt

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

// driverEpoch is the fake clock's start in the input tests.
var driverEpoch = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

// testTolerance is the death tolerance of the test inputs.
const testTolerance = 10 * time.Second

// idleTimers is the number of clock timers of a started input at rest: the
// lifecycle's next check and the owner goroutine's wait (the next keep-alive
// or the backoff).
const idleTimers = 2

// fakeResolver resolves every password reference to value.
type fakeResolver struct {
	value string
	calls atomic.Int32
}

func (r *fakeResolver) Resolve(_ context.Context, _ string) (string, error) {
	r.calls.Add(1)

	return r.value, nil
}

// subscribePoint is a subscribe point of type typ at address.
func subscribePoint(signal, address, typ string) config.Point {
	return config.Point{
		Signal:   signal,
		Address:  address,
		Read:     config.Read{Mode: config.ReadSubscribe},
		Type:     typ,
		Priority: model.Critical,
	}
}

// device is a device of the given points.
func device(name string, points ...config.Point) config.Device {
	return config.Device{Device: name, Points: points}
}

// mqttInput is the input id on broker b with the connection fields of conn
// (the broker is added).
func mqttInput(t *testing.T, id string, b *mqtttest.Broker, conn map[string]any, devices ...config.Device) config.Input {
	t.Helper()

	fields := map[string]any{"broker": "mqtt://" + b.Addr()}
	for k, v := range conn {
		fields[k] = v
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	return config.Input{ID: id, Driver: config.DriverMQTT, Connection: raw, DeathTolerance: testTolerance, Devices: devices}
}

// harness runs one input on a fake clock and records what it emits.
type harness struct {
	t      *testing.T
	clk    *clock.Fake
	in     *Input
	events chan string

	mu      sync.Mutex
	records []model.Record
	changed chan struct{}

	closed atomic.Bool
	late   atomic.Int32
}

// newHarness builds the input of cfg with a backoff jitter of 0 (waits of
// 0.5 s, 1 s, 2 s...). opts.Logger is left nil (discarded).
func newHarness(t *testing.T, cfg config.Input, opts Options) *harness {
	t.Helper()

	clk := clock.NewFake(driverEpoch)
	h := &harness{t: t, clk: clk, events: make(chan string, 1024), changed: make(chan struct{})}

	in, err := newInput(cfg, clk, opts, deps{
		jitter: func() float64 { return 0 },
		observe: func(event string) {
			select {
			case h.events <- event:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("newInput: %v", err)
	}
	h.in = in

	return h
}

func (h *harness) start() {
	h.t.Helper()

	if err := h.in.Start(h.t.Context(), h.emit); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	h.t.Cleanup(h.close)
}

func (h *harness) emit(r model.Record) {
	if h.closed.Load() {
		h.late.Add(1)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, r)
	close(h.changed)
	h.changed = make(chan struct{})
}

func (h *harness) close() {
	if err := h.in.Close(); err != nil {
		h.t.Errorf("Close: %v", err)
	}
	h.closed.Store(true)
}

// waitEvent waits for the next trace event named event, skipping others.
func (h *harness) waitEvent(event string) {
	h.t.Helper()

	bound := time.NewTimer(waitTimeout)
	defer bound.Stop()

	for {
		select {
		case got := <-h.events:
			if got == event {
				return
			}
		case <-bound.C:
			h.t.Fatalf("no %q event within %v", event, waitTimeout)
		}
	}
}

// serving waits until the input is connected, subscribed and its keep-alive
// timer is set.
func (h *harness) serving() {
	h.t.Helper()

	h.waitEvent(traceServing)
	h.clk.BlockUntil(h.t, idleTimers)
}

// waitRecords waits until ok holds for the records emitted so far and
// returns them.
func (h *harness) waitRecords(what string, ok func([]model.Record) bool) []model.Record {
	h.t.Helper()

	bound := time.NewTimer(waitTimeout)
	defer bound.Stop()

	for {
		h.mu.Lock()
		records := slices.Clone(h.records)
		changed := h.changed
		h.mu.Unlock()

		if ok(records) {
			return records
		}

		select {
		case <-changed:
		case <-bound.C:
			h.t.Fatalf("waiting for %s: records = %s", what, describe(records))
		}
	}
}

// snapshot returns the records emitted so far.
func (h *harness) snapshot() []model.Record {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.records)
}

// waitSamples waits for n samples of signal and returns them.
func (h *harness) waitSamples(signal string, n int) []model.Sample {
	h.t.Helper()

	records := h.waitRecords(signal+" samples", func(rs []model.Record) bool { return len(samplesOf(rs, signal)) >= n })

	return samplesOf(records, signal)
}

// waitEvents waits until every device has n events of kind.
func (h *harness) waitEvents(kind model.EventKind, n int, devices ...string) {
	h.t.Helper()

	h.waitRecords("events", func(rs []model.Record) bool {
		for _, d := range devices {
			if countEvents(rs, kind, d) < n {
				return false
			}
		}

		return true
	})
}

// advanceToBackoffEnd lets the pending backoff wait end: the input must be
// waiting in it (after a down event).
func (h *harness) advanceToBackoffEnd() {
	h.t.Helper()

	h.clk.BlockUntil(h.t, idleTimers)
	h.clk.Advance(driverkit.BackoffMax)
}

func samplesOf(rs []model.Record, signal string) []model.Sample {
	var out []model.Sample
	for _, r := range rs {
		if r.Sample != nil && r.Sample.Signal == signal {
			out = append(out, *r.Sample)
		}
	}

	return out
}

func countEvents(rs []model.Record, kind model.EventKind, dev string) int {
	n := 0
	for _, r := range rs {
		if r.Event != nil && r.Event.Kind == kind && r.Event.Device == dev {
			n++
		}
	}

	return n
}

func describe(rs []model.Record) string {
	out := ""
	for _, r := range rs {
		switch {
		case r.Sample != nil:
			out += "[sample " + r.Sample.Device + "/" + r.Sample.Signal + "]"
		case r.Event != nil:
			out += "[event " + r.Event.Kind.String() + " " + r.Event.Device + "]"
		}
	}

	return out
}
