package input_test

import (
	"context"
	"encoding/json"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/clock"
	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/input"
	"github.com/getlaraplate/laraplate-edge/internal/model"
)

// guard bounds every wait for a goroutine; it is a deadlock guard, never a
// timing assertion.
const guard = 10 * time.Second

var start = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

// recorder collects emitted records; safe for concurrent use.
type recorder struct {
	mu   sync.Mutex
	recs []model.Record
}

func (r *recorder) emit(rec model.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.recs = append(r.recs, rec)
}

// events returns "kind device" for every device event emitted so far.
func (r *recorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []string
	for _, rec := range r.recs {
		if rec.Event != nil {
			out = append(out, rec.Event.Kind.String()+" "+rec.Event.Device)
		}
	}

	return out
}

func twoDevices() config.Input {
	return config.Input{
		ID:             "line",
		Driver:         config.DriverSimulated,
		DeathTolerance: 30 * time.Second,
		Devices: []config.Device{
			{Device: "press-07", Points: []config.Point{
				{Signal: "state", Type: "string"},
				{Signal: "good_count", Type: "uint32", Unit: "pcs"},
			}},
			{Device: "oven-02", Points: []config.Point{{Signal: "temp", Type: "float32", Unit: "°C"}}},
		},
	}
}

func TestSignals(t *testing.T) {
	chain := func(name, params string) []config.Transform {
		return []config.Transform{{Name: name, Params: json.RawMessage(params)}}
	}
	got := input.Signals(config.Device{Device: "d", Points: []config.Point{
		{Signal: "on", Type: "bool"},
		{Signal: "name", Type: "string"},
		{Signal: "count", Type: "uint64", Unit: "pcs"},
		{Signal: "raw"},
		{Signal: "alarm", Type: "int16", Transforms: chain("bit", `0`)},
		{Signal: "temp", Type: "float32", Unit: "°C", Transforms: chain("round", `1`)},
		{Signal: "weight", Type: "string", Unit: "g", Transforms: chain("regex_extract", `{"pattern": "(\\d+)", "as": "integer"}`)},
		{Signal: "parts", Type: "bool", Transforms: chain("edge_counter", `"rising"`)},
	}})
	want := []model.SignalInfo{
		{Signal: "on", DataType: model.DataTypeBoolean},
		{Signal: "name", DataType: model.DataTypeString},
		{Signal: "count", DataType: model.DataTypeNumber, Unit: "pcs"},
		{Signal: "raw", DataType: model.DataTypeNumber},
		{Signal: "alarm", DataType: model.DataTypeBoolean},
		{Signal: "temp", DataType: model.DataTypeNumber, Unit: "°C"},
		{Signal: "weight", DataType: model.DataTypeNumber, Unit: "g"},
		{Signal: "parts", DataType: model.DataTypeNumber},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Signals() = %+v, want %+v", got, want)
	}
}

func TestLifecycleDeathAfterToleranceAndBirthOnRecovery(t *testing.T) {
	type step struct {
		advance time.Duration
		read    []string
		check   bool
		want    []string
	}

	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "no death before the tolerance",
			steps: []step{
				{advance: 30*time.Second - time.Nanosecond, check: true, want: nil},
			},
		},
		{
			name: "death of every device without a read at the tolerance",
			steps: []step{
				{advance: 30 * time.Second, check: true, want: []string{"death oven-02", "death press-07"}},
			},
		},
		{
			name: "a read keeps its device alive",
			steps: []step{
				{advance: 20 * time.Second, read: []string{"press-07"}},
				{advance: 10 * time.Second, check: true, want: []string{"death oven-02"}},
				{advance: 20 * time.Second, check: true, want: []string{"death oven-02", "death press-07"}},
			},
		},
		{
			name: "death once, then birth on recovery",
			steps: []step{
				{advance: 31 * time.Second, check: true, want: []string{"death oven-02", "death press-07"}},
				{advance: time.Minute, check: true, want: []string{"death oven-02", "death press-07"}},
				{read: []string{"press-07"}, want: []string{"death oven-02", "death press-07", "birth press-07"}},
				{read: []string{"press-07"}, want: []string{"death oven-02", "death press-07", "birth press-07"}},
			},
		},
		{
			name: "a read while alive emits nothing",
			steps: []step{
				{read: []string{"press-07", "oven-02"}, check: true, want: nil},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := clock.NewFake(start)
			rec := &recorder{}
			lc := input.NewLifecycle(twoDevices(), clk, rec.emit)

			for i, s := range tt.steps {
				clk.Advance(s.advance)
				for _, d := range s.read {
					lc.Read(d)
				}
				if s.check {
					lc.Check()
				}
				if got := rec.events(); !reflect.DeepEqual(got, s.want) {
					t.Fatalf("step %d: events = %q, want %q", i, got, s.want)
				}
			}
		})
	}
}

func TestLifecycleBirthListsTheInputSignals(t *testing.T) {
	clk := clock.NewFake(start)
	rec := &recorder{}
	lc := input.NewLifecycle(twoDevices(), clk, rec.emit)

	clk.Advance(30 * time.Second)
	lc.Check()
	clk.Advance(5 * time.Second)
	lc.Read("press-07")

	last := rec.recs[len(rec.recs)-1]
	want := model.DeviceEvent{
		Kind:   model.Birth,
		Device: "press-07",
		TS:     start.Add(35 * time.Second),
		Signals: []model.SignalInfo{
			{Signal: "state", DataType: model.DataTypeString},
			{Signal: "good_count", DataType: model.DataTypeNumber, Unit: "pcs"},
		},
	}
	if last.Event == nil || !reflect.DeepEqual(*last.Event, want) {
		t.Fatalf("last record = %+v, want birth %+v", last, want)
	}

	death := rec.recs[0].Event
	if death.Kind != model.Death || !death.TS.Equal(start.Add(30*time.Second)) || death.Signals != nil {
		t.Fatalf("first record = %+v, want a death at the tolerance without signals", death)
	}
}

func TestLifecycleUnknownDeviceIsIgnored(t *testing.T) {
	clk := clock.NewFake(start)
	rec := &recorder{}
	lc := input.NewLifecycle(twoDevices(), clk, rec.emit)

	lc.Read("not-configured")
	clk.Advance(time.Hour)
	lc.Check()

	for _, e := range rec.events() {
		if e == "birth not-configured" || e == "death not-configured" {
			t.Fatalf("events = %q, want nothing for an unknown device", rec.events())
		}
	}
}

func TestLifecycleRunChecksOnTheClock(t *testing.T) {
	clk := clock.NewFake(start)
	rec := &recorder{}
	lc := input.NewLifecycle(twoDevices(), clk, rec.emit)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		lc.Run(ctx)
	}()

	waitTimers(t, clk, 1)
	clk.Advance(20 * time.Second)
	lc.Read("press-07")
	waitTimers(t, clk, 1)
	clk.Advance(10 * time.Second)
	waitFor(t, func() bool { return len(rec.events()) == 1 })
	if got := rec.events(); got[0] != "death oven-02" {
		t.Fatalf("events = %q, want death oven-02 at 30s", got)
	}

	waitTimers(t, clk, 1)
	clk.Advance(20 * time.Second)
	waitFor(t, func() bool { return len(rec.events()) == 2 })
	if got := rec.events(); got[1] != "death press-07" {
		t.Fatalf("events = %q, want death press-07 at 50s", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(guard):
		t.Fatal("Run did not return after cancel")
	}
}

// waitTimers waits until the fake clock holds at least n pending timers.
func waitTimers(t *testing.T, clk *clock.Fake, n int) {
	t.Helper()
	waitFor(t, func() bool { return clk.PendingTimers() >= n })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		runtime.Gosched()
	}
}
