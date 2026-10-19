package pipeline_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/buffer"
	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/model"
	"github.com/swolley/laraplate-edge/internal/pipeline"
)

var start = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

// sink is a fake output buffer recording what it receives.
type sink struct {
	mu     sync.Mutex
	recs   []model.Record
	losses []model.Loss
	err    error
}

func (s *sink) AddLoss(l model.Loss) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.losses = append(s.losses, l)

	return nil
}

func (s *sink) Append(r model.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return s.err
	}
	s.recs = append(s.recs, r)

	return nil
}

// got renders the received records: "device/signal" for samples, "kind
// device [signals]" for events.
func (s *sink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string
	for _, r := range s.recs {
		switch {
		case r.Sample != nil:
			out = append(out, r.Sample.Device+"/"+r.Sample.Signal)
		case r.Event != nil:
			var names []string
			for _, si := range r.Event.Signals {
				names = append(names, si.Signal)
			}
			out = append(out, fmt.Sprintf("%s %s %v", r.Event.Kind, r.Event.Device, names))
		}
	}

	return out
}

func sample(device, signal string) model.Record {
	return model.SampleRecord(model.Sample{
		Device: device, Signal: signal, TS: start,
		Value: model.IntValue(4242), Quality: model.Good, Priority: model.Critical,
	})
}

func line() config.Input {
	return config.Input{ID: "line", Devices: []config.Device{
		{Device: "press-07", Points: []config.Point{{Signal: "state", Type: "string"}, {Signal: "temp"}}},
		{Device: "oven-02", Points: []config.Point{{Signal: "temp", Unit: "°C"}}},
	}}
}

func TestRouterFansSamplesOutByRoute(t *testing.T) {
	tests := []struct {
		name    string
		include []string
		want    []string
	}{
		{name: "star routes everything", include: []string{"*"}, want: []string{"press-07/state", "press-07/temp", "oven-02/temp"}},
		{name: "a bare device means device/*", include: []string{"press-07"}, want: []string{"press-07/state", "press-07/temp"}},
		{name: "a device glob", include: []string{"press-*"}, want: []string{"press-07/state", "press-07/temp"}},
		{name: "a signal pattern across devices", include: []string{"*/temp"}, want: []string{"press-07/temp", "oven-02/temp"}},
		{name: "several patterns", include: []string{"oven-02/temp", "press-07/state"}, want: []string{"press-07/state", "oven-02/temp"}},
		{name: "nothing matches", include: []string{"mill-*"}, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.DiscardHandler))
			all, routed := &sink{}, &sink{}
			r.SetOutput("all", config.Route{Include: []string{"*"}}, all)
			r.SetOutput("routed", config.Route{Include: tt.include}, routed)

			for _, rec := range []model.Record{sample("press-07", "state"), sample("press-07", "temp"), sample("oven-02", "temp")} {
				r.Emit(rec)
			}

			if got := routed.got(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("routed output got %q, want %q", got, tt.want)
			}
			if got := all.got(); len(got) != 3 {
				t.Fatalf("the * output got %q, want all three samples", got)
			}
		})
	}
}

func TestRouterDeviceEventsFollowTheRoutedSignals(t *testing.T) {
	r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.DiscardHandler))
	all, temps, ovens, none := &sink{}, &sink{}, &sink{}, &sink{}
	r.SetOutput("all", config.Route{Include: []string{"*"}}, all)
	r.SetOutput("temps", config.Route{Include: []string{"*/temp"}}, temps)
	r.SetOutput("ovens", config.Route{Include: []string{"oven-*"}}, ovens)
	r.SetOutput("none", config.Route{Include: []string{"mill"}}, none)
	r.SetInput(line())

	r.Emit(model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "press-07", TS: start}))
	r.Emit(model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: "press-07", TS: start,
		Signals: []model.SignalInfo{{Signal: "state", DataType: model.DataTypeString}}}))

	want := map[string][]string{
		"all":   {"death press-07 []", "birth press-07 [state temp]"},
		"temps": {"death press-07 []", "birth press-07 [temp]"},
		"ovens": nil,
		"none":  nil,
	}
	for name, s := range map[string]*sink{"all": all, "temps": temps, "ovens": ovens, "none": none} {
		if got := s.got(); !reflect.DeepEqual(got, want[name]) {
			t.Errorf("%s got %q, want %q", name, got, want[name])
		}
	}
}

func TestRouterBirths(t *testing.T) {
	newRouter := func() (*pipeline.Router, map[string]*sink) {
		r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.DiscardHandler))
		sinks := map[string]*sink{"all": {}, "ovens": {}, "none": {}}
		r.SetOutput("all", config.Route{Include: []string{"*"}}, sinks["all"])
		r.SetOutput("ovens", config.Route{Include: []string{"oven-02/temp"}}, sinks["ovens"])
		r.SetOutput("none", config.Route{Include: []string{"mill"}}, sinks["none"])
		r.SetInput(line())
		r.SetInput(config.Input{ID: "mixer", Devices: []config.Device{{Device: "mixer-1", Points: []config.Point{{Signal: "rpm"}}}}})

		return r, sinks
	}

	tests := []struct {
		name string
		act  func(*pipeline.Router)
		want map[string][]string
	}{
		{
			name: "births of one input go to every output, filtered",
			act:  func(r *pipeline.Router) { r.BirthInput("line") },
			want: map[string][]string{
				"all":   {"birth press-07 [state temp]", "birth oven-02 [temp]"},
				"ovens": {"birth oven-02 [temp]"},
			},
		},
		{
			name: "births to one output cover every input routed to it",
			act:  func(r *pipeline.Router) { r.BirthTo("all") },
			want: map[string][]string{
				"all": {"birth press-07 [state temp]", "birth oven-02 [temp]", "birth mixer-1 [rpm]"},
			},
		},
		{
			name: "births to an output filtered by its route",
			act:  func(r *pipeline.Router) { r.BirthTo("ovens") },
			want: map[string][]string{"ovens": {"birth oven-02 [temp]"}},
		},
		{
			name: "a removed input has no births",
			act: func(r *pipeline.Router) {
				r.RemoveInput("line")
				r.BirthTo("all")
			},
			want: map[string][]string{"all": {"birth mixer-1 [rpm]"}},
		},
		{
			name: "a removed output receives nothing",
			act: func(r *pipeline.Router) {
				r.RemoveOutput("all", "removed")
				r.BirthInput("line")
				r.Emit(sample("press-07", "state"))
			},
			want: map[string][]string{"ovens": {"birth oven-02 [temp]"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, sinks := newRouter()
			tt.act(r)
			for name, s := range sinks {
				if got := s.got(); !reflect.DeepEqual(got, tt.want[name]) {
					t.Errorf("%s got %q, want %q", name, got, tt.want[name])
				}
			}
		})
	}
}

func TestRouterBirthCarriesTypesUnitsAndClockTime(t *testing.T) {
	clk := clock.NewFake(start)
	r := pipeline.NewRouter(clk, slog.New(slog.DiscardHandler))
	s := &sink{}
	r.SetOutput("all", config.Route{Include: []string{"*"}}, s)
	r.SetInput(line())
	clk.Advance(time.Minute)

	r.BirthTo("all")

	want := model.DeviceEvent{Kind: model.Birth, Device: "oven-02", TS: start.Add(time.Minute),
		Signals: []model.SignalInfo{{Signal: "temp", DataType: model.DataTypeNumber, Unit: "°C"}}}
	if got := s.recs[1].Event; got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("birth = %+v, want %+v", got, want)
	}
}

func TestRouterAppendErrorIsLoggedWithoutValueAndRoutingGoesOn(t *testing.T) {
	var logs bytes.Buffer
	r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.NewJSONHandler(&logs, nil)))
	failing := &sink{err: fmt.Errorf("buffer: %w", buffer.ErrDropped)}
	ok := &sink{}
	r.SetOutput("a-failing", config.Route{Include: []string{"*"}}, failing)
	r.SetOutput("b-ok", config.Route{Include: []string{"*"}}, ok)

	r.Emit(sample("press-07", "state"))

	if got := ok.got(); !slices.Equal(got, []string{"press-07/state"}) {
		t.Fatalf("second output got %q, want the sample", got)
	}
	out := logs.String()
	for _, want := range []string{`"level":"WARN"`, `"output":"a-failing"`, `"device":"press-07"`, `"signal":"state"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %s", out, want)
		}
	}
	if strings.Contains(out, "4242") {
		t.Errorf("log %q contains the sample value", out)
	}
}

func TestRouterSetOutputBirthsTheRunningDevicesFirst(t *testing.T) {
	r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.DiscardHandler))
	r.SetInput(line())
	s := &sink{}

	r.SetOutput("temps", config.Route{Include: []string{"*/temp"}}, s)
	r.Emit(sample("oven-02", "temp"))

	want := []string{"birth press-07 [temp]", "birth oven-02 [temp]", "oven-02/temp"}
	if got := s.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("new output got %q, want %q", got, want)
	}
}

func TestRouterHoldsARestartingOutput(t *testing.T) {
	r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.DiscardHandler))
	r.SetInput(line())
	old := &sink{}
	r.SetOutput("main", config.Route{Include: []string{"*"}}, old)
	before := len(old.got())

	r.Hold("main", config.Route{Include: []string{"*"}})
	r.Emit(sample("press-07", "state"))
	r.Emit(model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "oven-02", TS: start}))
	// A held birth is not flushed: SetOutput has just sent the current ones.
	r.Emit(model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: "oven-02", TS: start}))
	r.Emit(sample("oven-02", "temp"))
	if got := old.got(); len(got) != before {
		t.Fatalf("the previous buffer received %q while held", got[before:])
	}

	reopened := &sink{}
	r.SetOutput("main", config.Route{Include: []string{"press-07", "oven-02/temp"}}, reopened)

	want := []string{
		"birth press-07 [state temp]", "birth oven-02 [temp]",
		"press-07/state", "death oven-02 []", "oven-02/temp",
	}
	if got := reopened.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened output got %q, want births then the held records %q", got, want)
	}
	if len(reopened.losses) != 0 {
		t.Fatalf("losses = %+v, want none under the hold limit", reopened.losses)
	}
}

func TestRouterHoldOverflowBecomesLossMarkers(t *testing.T) {
	var logs bytes.Buffer
	r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.NewJSONHandler(&logs, nil)))
	r.SetInput(config.Input{ID: "press", Devices: []config.Device{{Device: "press-07", Points: []config.Point{{Signal: "state", Type: "string"}}}}})
	r.SetOutput("main", config.Route{Include: []string{"*"}}, &sink{})
	r.Hold("main", config.Route{Include: []string{"*"}})

	at := func(i int) time.Time { return start.Add(time.Duration(i) * time.Millisecond) }
	for range pipeline.HoldLimit {
		r.Emit(sample("press-07", "state"))
	}
	for i := 1; i <= 3; i++ {
		r.Emit(model.SampleRecord(model.Sample{Device: "press-07", Signal: "temp", TS: at(i), Value: model.IntValue(1), Quality: model.Good, Priority: model.Bulk}))
	}
	r.Emit(model.SampleRecord(model.Sample{Device: "press-07", Signal: "state", TS: at(9), Value: model.IntValue(1), Quality: model.Good, Priority: model.Critical}))
	// Device events past the limit are not samples: a birth is sent again
	// anyway, a lost death is logged with its device.
	r.Emit(model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: "press-07", TS: at(10),
		Signals: []model.SignalInfo{{Signal: "state", DataType: model.DataTypeString}}}))
	r.Emit(model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "press-07", TS: at(11)}))

	reopened := &sink{}
	r.SetOutput("main", config.Route{Include: []string{"*"}}, reopened)

	if n := len(reopened.recs) - 1; n != pipeline.HoldLimit {
		t.Fatalf("reopened output got %d records, want the %d held", n, pipeline.HoldLimit)
	}
	want := []model.Loss{
		{Priority: model.Critical, FromTS: at(9), ToTS: at(9), Samples: 1, Reason: model.LossBufferFull},
		{Priority: model.Bulk, FromTS: at(1), ToTS: at(3), Samples: 3, Reason: model.LossBufferFull},
	}
	if !reflect.DeepEqual(reopened.losses, want) {
		t.Fatalf("losses = %+v, want %+v", reopened.losses, want)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), `"device":"press-07"`) || !strings.Contains(logs.String(), "death") {
		t.Fatalf("no warning naming the device of the lost death in %s", logs.String())
	}
}

func TestRouterRemovingAHeldOutputDiscardsItsRecords(t *testing.T) {
	var logs bytes.Buffer
	r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.NewJSONHandler(&logs, nil)))
	r.SetOutput("main", config.Route{Include: []string{"*"}}, &sink{})
	r.Hold("main", config.Route{Include: []string{"*"}})
	r.Emit(sample("press-07", "state"))

	r.Emit(model.SampleRecord(model.Sample{Device: "press-07", Signal: "temp", TS: start.Add(time.Second), Value: model.IntValue(1), Quality: model.Good, Priority: model.Bulk}))
	r.Emit(model.SampleRecord(model.Sample{Device: "press-07", Signal: "temp", TS: start.Add(3 * time.Second), Value: model.IntValue(1), Quality: model.Good, Priority: model.Bulk}))

	r.RemoveOutput("main", "agent shutting down")
	again := &sink{}
	r.SetOutput("main", config.Route{Include: []string{"*"}}, again)

	if got := again.got(); len(got) != 0 {
		t.Fatalf("a new output got %q held for a removed one", got)
	}
	for _, want := range []string{
		`"reason":"agent shutting down","priority":"critical","records":1,"from_ts":"2026-10-19T08:00:00Z","to_ts":"2026-10-19T08:00:00Z"`,
		`"reason":"agent shutting down","priority":"bulk","records":2,"from_ts":"2026-10-19T08:00:01Z","to_ts":"2026-10-19T08:00:03Z"`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no warning with %s in %s", want, logs.String())
		}
	}
}

func TestRouterHoldingAHeldOutputKeepsItsRecords(t *testing.T) {
	r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.DiscardHandler))
	r.SetOutput("main", config.Route{Include: []string{"*"}}, &sink{})
	r.Hold("main", config.Route{Include: []string{"*"}})
	r.Emit(sample("press-07", "state"))

	r.Hold("main", config.Route{Include: []string{"*"}})
	reopened := &sink{}
	r.SetOutput("main", config.Route{Include: []string{"*"}}, reopened)

	if got := reopened.got(); !reflect.DeepEqual(got, []string{"press-07/state"}) {
		t.Fatalf("reopened output got %q, want the record held before the second Hold", got)
	}
}

func TestRouterHoldTakesTheGivenRoute(t *testing.T) {
	r := pipeline.NewRouter(clock.NewFake(start), slog.New(slog.DiscardHandler))
	r.SetOutput("main", config.Route{Include: []string{"*"}}, &sink{})

	// The hold routes with the configuration being restored.
	r.Hold("main", config.Route{Include: []string{"press-07"}})
	r.Emit(sample("oven-02", "temp"))
	r.Emit(sample("press-07", "state"))
	// Holding again changes the route and keeps the queue.
	r.Hold("main", config.Route{Include: []string{"oven-02"}})
	r.Emit(sample("press-07", "temp"))
	r.Emit(sample("oven-02", "temp"))

	reopened := &sink{}
	r.SetOutput("main", config.Route{Include: []string{"*"}}, reopened)

	if got, want := reopened.got(), []string{"press-07/state", "oven-02/temp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened output got %q, want %q", got, want)
	}
}
