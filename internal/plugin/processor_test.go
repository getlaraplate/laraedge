package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

// processorHarness runs a processor on a clock of its own, apart from the
// host's.
type processorHarness struct {
	*harness
	procClk  *clock.Fake
	resolver *fakeResolver
	out      *collector
}

func newProcessorHarness(t *testing.T, behaviour string) *processorHarness {
	t.Helper()

	h := &processorHarness{
		harness:  newHarness(t, newInputSpec(t, behaviour)),
		procClk:  clock.NewFake(epoch),
		resolver: &fakeResolver{values: map[string]string{"secret://env/TOKEN": secretValue}},
		out:      &collector{},
	}
	h.start()

	return h
}

func (h *processorHarness) processor(t *testing.T, cfg config.Processor) *plugin.Processor {
	t.Helper()

	p := plugin.NewProcessor(h.host, cfg, h.resolver, h.procClk, slog.New(slog.NewTextHandler(h.log, nil)))
	t.Cleanup(func() { _ = p.Close() })

	return p
}

// startProcessor starts p1 and waits for its stream.
func (h *processorHarness) startProcessor(t *testing.T, cfg config.Processor) *plugin.Processor {
	t.Helper()

	p := h.processor(t, cfg)
	if err := p.Start(context.Background(), h.out.emit); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	h.waitFor("the stream up", func() bool { return p.Stats().Up })

	return p
}

func (h *processorHarness) helper() *inputHarness { return &inputHarness{harness: h.harness} }

func (h *processorHarness) read(name string) string { return h.helper().read(name) }

func (h *processorHarness) waitTrace(want string) {
	h.t.Helper()

	h.waitFor("trace "+want, func() bool { return strings.Contains(h.read("trace"), want) })
}

func processorBehaviour(extra string) string {
	return `{"processor":true,"trace_file":"trace","records_file":"records","config_file":"config.json"` + extra + `}`
}

// testProcessor is processor p1 observing d1, deriving line-1 (count int,
// label string bulk) and line-2 (on bool, speed float bulk).
func testProcessor(configJSON string) config.Processor {
	raw := fmt.Sprintf(`{"id":"p1","plugin":"demo","observe":["d1"],"devices":[`+
		`{"device":"line-1","signals":[{"signal":"count","type":"int","priority":"critical"},{"signal":"label","type":"string","priority":"bulk","unit":"u"}]},`+
		`{"device":"line-2","signals":[{"signal":"on","type":"bool","priority":"critical"},{"signal":"speed","type":"float","priority":"bulk","unit":"rpm"}]}],`+
		`"death_tolerance":"10s","config":%s}`, configJSON)

	return config.Processor{
		ID: "p1", Plugin: "demo", Observe: []string{"d1"},
		Devices: []config.DerivedDevice{
			{Device: "line-1", Signals: []config.DerivedSignal{
				{Signal: "count", Type: "int", Priority: "critical"},
				{Signal: "label", Type: "string", Priority: "bulk", Unit: "u"},
			}},
			{Device: "line-2", Signals: []config.DerivedSignal{
				{Signal: "on", Type: "bool", Priority: "critical"},
				{Signal: "speed", Type: "float", Priority: "bulk", Unit: "rpm"},
			}},
		},
		DeathTolerance: tolerance,
		Config:         json.RawMessage(configJSON),
		Raw:            json.RawMessage(raw),
	}
}

func observed(device string, value int64, quality model.Quality) model.Record {
	ts, _ := time.Parse(time.RFC3339, ts1)

	return model.SampleRecord(model.Sample{Device: device, Signal: "s1", TS: ts, Value: model.IntValue(value), Quality: quality, Priority: model.Critical})
}

func processSample(device, signal string, value int, kind string) string {
	return fmt.Sprintf(`{"device":%q,"signal":%q,"value":%d,"ts":%q,"kind":%q}`, device, signal, value, ts1, kind)
}

func TestProcessorEchoesObservedRecordsWithThePluginTsAndQuality(t *testing.T) {
	h := newProcessorHarness(t, processorBehaviour(""))
	p := h.startProcessor(t, testProcessor(`{"mode":"fast","token":"secret://env/TOKEN"}`))

	p.Offer(observed("d1", 7, model.Uncertain))
	p.Offer(model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "d1", TS: epoch}))
	p.Offer(observed("d9", 8, model.Good))
	p.Offer(observed("d1", 9, model.Good))

	h.waitFor("two derived samples", func() bool { return len(h.out.all()) == 2 })
	want := []struct {
		value   int64
		quality model.Quality
	}{{7, model.Uncertain}, {9, model.Good}}
	for i, rec := range h.out.all() {
		s := rec.Sample
		if s == nil || s.Device != "line-1" || s.Signal != "count" || s.Value != model.IntValue(want[i].value) ||
			s.TS.Format(time.RFC3339) != ts1 || s.Quality != want[i].quality || s.Priority != model.Critical {
			t.Fatalf("derived record %d = %+v, want line-1/count %d at the plugin ts, quality %v, the declared priority", i, rec, want[i].value, want[i].quality)
		}
	}

	h.waitFor("three records in the plugin", func() bool { return strings.Count(h.read("records"), "\n") == 3 })
	records := h.read("records")
	if strings.Contains(records, "d9") || !strings.Contains(records, `"Kind":2`) {
		t.Fatalf("the plugin received\n%s\nwant the samples and the death of d1 only", records)
	}

	cfg := h.read("config.json")
	if !strings.Contains(cfg, `"token":"`+secretValue+`"`) || strings.Contains(cfg, "secret://") || !strings.Contains(cfg, `"id":"p1"`) {
		t.Fatalf("the plugin was configured with %s, want the section of p1 with the token resolved", cfg)
	}
	if got := h.read("trace"); got != "configure p1\nprocess p1\n" {
		t.Fatalf("trace = %q, want one Configure then one Process", got)
	}
	if got := p.Stats(); !got.Up || got.Refused != 0 || got.Dropped != 0 {
		t.Fatalf("Stats() = %+v, want up, nothing refused or dropped", got)
	}
	if strings.Contains(h.log.String(), secretValue) {
		t.Fatalf("the log holds the secret value:\n%s", h.log.String())
	}
}

func TestProcessorRefusesWhatItDidNotDeclare(t *testing.T) {
	tests := []struct {
		name   string
		sample string
	}{
		{"undeclared device", processSample("ghost", "count", 1, "int")},
		{"real device", processSample("d1", "s1", 1, "int")},
		{"undeclared signal", processSample("line-1", "nope", 1, "int")},
		{"string for an int", processSample("line-1", "count", 1, "string")},
		{"int for a bool", processSample("line-2", "on", 1, "int")},
		{"int for a float", processSample("line-2", "speed", 1, "int")},
		{"float for an int", processSample("line-1", "count", 1, "float")},
		{"bool for a string", processSample("line-1", "label", 1, "bool")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := `[` + tc.sample + `,` + processSample("line-1", "count", 5, "int") + `]`
			h := newProcessorHarness(t, processorBehaviour(`,"process_script":`+script))
			p := h.startProcessor(t, testProcessor(`{}`))

			h.waitFor("the declared sample", func() bool { return len(h.out.all()) == 1 })
			if s := h.out.all()[0].Sample; s == nil || s.Device != "line-1" || s.Signal != "count" || s.Value != model.IntValue(5) {
				t.Fatalf("emitted %+v, want only the declared sample", h.out.all()[0])
			}
			if got := p.Stats(); got.Refused != 1 || got.Dropped != 0 {
				t.Fatalf("Stats() = %+v, want one refusal", got)
			}
			if !strings.Contains(h.log.String(), "refused") {
				t.Fatalf("the refusal is not logged:\n%s", h.log.String())
			}
		})
	}
}

func TestProcessorOfferDropsBeyondTheQueue(t *testing.T) {
	h := newProcessorHarness(t, processorBehaviour(""))
	p := h.processor(t, testProcessor(`{}`))

	for i := range 1500 {
		p.Offer(observed("d1", int64(i), model.Good))
	}
	for range 10 {
		p.Offer(observed("d9", 1, model.Good))
	}
	if got := p.Stats(); got.Dropped != 500 || got.Up {
		t.Fatalf("Stats() = %+v, want 500 copies dropped beyond the 1000 queued, not up", got)
	}
}

// TestProcessorHungPluginNeverBlocksOffer is the review focus: a processor
// that stops reading its stream never blocks the input offering copies; the
// copies beyond the queue are dropped and counted.
func TestProcessorHungPluginNeverBlocksOffer(t *testing.T) {
	h := newProcessorHarness(t, processorBehaviour(`,"stop_reading":true`))
	p := h.startProcessor(t, testProcessor(`{}`))

	const burst = 200_000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range burst {
			p.Offer(observed("d1", int64(i), model.Good))
		}
	}()
	select {
	case <-done:
	case <-time.After(guard):
		t.Fatal("Offer blocked on a hung processor")
	}

	got := p.Stats()
	t.Logf("%d of %d copies dropped", got.Dropped, burst)
	if got.Dropped == 0 || got.Dropped > burst || !got.Up {
		t.Fatalf("Stats() = %+v, want copies dropped and the stream still up", got)
	}
	if records := h.read("records"); records != "" {
		t.Fatalf("a hung plugin read records:\n%s", records)
	}
}

// TestProcessorCrashGoesBadThenDeathThenBirthAfterRestart is decision Q5.
func TestProcessorCrashGoesBadThenDeathThenBirthAfterRestart(t *testing.T) {
	h := newProcessorHarness(t, processorBehaviour(""))
	p := h.startProcessor(t, testProcessor(`{}`))
	h.waitTrace("process p1")

	h.helper().setBehaviour(`{"crash_at_start":true}`)
	h.kill()
	h.waitFor("one bad sample per derived signal", func() bool { return len(h.out.samples(model.Bad)) == 4 })
	want := []struct {
		device, signal string
		value          model.Value
		priority       model.Priority
	}{
		{"line-1", "count", model.IntValue(0), model.Critical},
		{"line-1", "label", model.StringValue(""), model.Bulk},
		{"line-2", "on", model.BoolValue(false), model.Critical},
		{"line-2", "speed", model.Value{Kind: model.KindFloat}, model.Bulk},
	}
	for i, s := range h.out.samples(model.Bad) {
		w := want[i]
		if s.Device != w.device || s.Signal != w.signal || s.Value != w.value || s.Priority != w.priority || !s.TS.Equal(epoch) {
			t.Fatalf("bad sample %d = %+v, want %+v at the clock time", i, s, w)
		}
	}
	if p.Stats().Up {
		t.Fatal("Stats().Up after the crash")
	}

	// The tick stopped with the stream; the lifecycle waits on the clock.
	h.waitFor("the lifecycle timer", func() bool { return h.procClk.PendingTimers() >= 1 })
	h.procClk.Advance(tolerance - time.Millisecond)
	if n := len(h.out.events(model.Death)); n != 0 {
		t.Fatalf("%d deaths before the tolerance", n)
	}
	h.procClk.Advance(time.Millisecond)
	h.waitFor("death of both derived devices", func() bool { return len(h.out.events(model.Death)) == 2 })

	h.helper().setBehaviour(processorBehaviour(""))
	h.helper().restartHost()
	h.waitFor("the stream up again", func() bool { return p.Stats().Up })
	h.waitFor("the tick and the lifecycle timers", func() bool { return h.procClk.PendingTimers() == 2 })
	if n := len(h.out.events(model.Birth)); n != 0 {
		t.Fatalf("%d births before the first tick", n)
	}
	h.procClk.Advance(time.Second)
	h.waitFor("birth of both derived devices", func() bool { return len(h.out.events(model.Birth)) == 2 })
	for _, birth := range h.out.events(model.Birth) {
		if len(birth.Signals) != 2 {
			t.Fatalf("birth = %+v, want the two declared signals", birth)
		}
	}

	p.Offer(observed("d1", 3, model.Good))
	h.waitFor("a derived sample after the restart", func() bool { return len(h.out.samples(model.Good)) == 1 })
	if got := h.read("trace"); got != "configure p1\nprocess p1\nconfigure p1\nprocess p1\n" {
		t.Fatalf("trace = %q, want Configure and Process once per run of the plugin", got)
	}
	if got := len(h.out.samples(model.Bad)); got != 4 {
		t.Fatalf("bad samples = %d, want exactly one per derived signal", got)
	}
}

// TestProcessorTickKeepsTheDerivedDevicesAlive: no per-sample liveness, a
// derived device lives while the stream is up.
func TestProcessorTickKeepsTheDerivedDevicesAlive(t *testing.T) {
	h := newProcessorHarness(t, processorBehaviour(""))
	h.startProcessor(t, testProcessor(`{}`))

	for range 3 * int(tolerance/time.Second) {
		h.waitFor("the tick and the lifecycle timers", func() bool { return h.procClk.PendingTimers() == 2 })
		h.procClk.Advance(time.Second)
	}
	if n := len(h.out.all()); n != 0 {
		t.Fatalf("emitted %v, want nothing while the stream is up", h.out.all())
	}
}

func TestProcessorConfigRefusalFailsStartWithPathsAndNoValues(t *testing.T) {
	h := newProcessorHarness(t, processorBehaviour(`,"refuse_config":true,"refuse_echo":true`))
	p := h.processor(t, testProcessor(`{"token":"secret://env/TOKEN"}`))

	err := p.Start(context.Background(), h.out.emit)
	var refused *plugin.ConfigRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Start() = %v, want a ConfigRefusedError", err)
	}
	if !strings.Contains(err.Error(), "processors.p1.config.port: bad value ***") || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("Start() = %q, want the path and no secret value", err)
	}
	if strings.Contains(h.read("trace"), "process p1") {
		t.Fatal("a refused processor opened its stream")
	}
}

func TestProcessorStartFailsWhenASecretCannotBeResolved(t *testing.T) {
	h := newProcessorHarness(t, processorBehaviour(""))
	p := h.processor(t, testProcessor(`{"token":"secret://env/MISSING"}`))

	err := p.Start(context.Background(), h.out.emit)
	var refused *plugin.ConfigRefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "processors.p1.config.token") || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("Start() = %v, want the path of the secret and no value", err)
	}
	if got := h.read("trace"); got != "" {
		t.Fatalf("trace = %q, want the plugin never contacted", got)
	}
}

func TestProcessorStartRefusesAPluginWithoutTheProcessorKind(t *testing.T) {
	h := newProcessorHarness(t, `{"trace_file":"trace"}`)
	p := h.processor(t, testProcessor(`{}`))

	if err := p.Start(context.Background(), h.out.emit); err == nil || !strings.Contains(err.Error(), "processor kind") {
		t.Fatalf("Start() = %v, want the missing processor kind", err)
	}
}

func TestProcessorCloseEndsTheStreamAndEmitsNothingAfter(t *testing.T) {
	h := newProcessorHarness(t, processorBehaviour(""))
	p := h.startProcessor(t, testProcessor(`{}`))
	p.Offer(observed("d1", 1, model.Good))
	h.waitFor("the derived sample", func() bool { return len(h.out.all()) == 1 })

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	h.waitTrace("process end p1")
	p.Offer(observed("d1", 2, model.Good))
	h.procClk.Advance(time.Hour)
	if got := len(h.out.all()); got != 1 {
		t.Fatalf("emitted %d records, want nothing after Close", got)
	}
	if got := p.Stats(); got.Up || got.Dropped != 0 {
		t.Fatalf("Stats() = %+v after Close, want down and nothing counted", got)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
}

func TestProcessorInputDescribesTheDerivedDevices(t *testing.T) {
	h := newProcessorHarness(t, processorBehaviour(""))
	in := h.processor(t, testProcessor(`{}`)).Input()

	if in.ID != "p1" || in.DeathTolerance != tolerance || len(in.Devices) != 2 {
		t.Fatalf("Input() = %+v", in)
	}
	want := map[string][]model.SignalInfo{
		"line-1": {{Signal: "count", DataType: model.DataTypeNumber}, {Signal: "label", DataType: model.DataTypeString, Unit: "u"}},
		"line-2": {{Signal: "on", DataType: model.DataTypeBoolean}, {Signal: "speed", DataType: model.DataTypeNumber, Unit: "rpm"}},
	}
	priorities := map[string]model.Priority{"count": model.Critical, "label": model.Bulk, "on": model.Critical, "speed": model.Bulk}
	for _, d := range in.Devices {
		got := input.Signals(d)
		if fmt.Sprint(got) != fmt.Sprint(want[d.Device]) {
			t.Fatalf("signals of %s = %+v, want %+v", d.Device, got, want[d.Device])
		}
		for _, pt := range d.Points {
			if pt.Priority != priorities[pt.Signal] {
				t.Fatalf("priority of %s = %v", pt.Signal, pt.Priority)
			}
		}
	}

	var deaths []string
	life := input.NewLifecycle(in, h.procClk, func(r model.Record) { deaths = append(deaths, r.Event.Device) })
	h.procClk.Advance(tolerance)
	life.Check()
	if fmt.Sprint(deaths) != "[line-1 line-2]" {
		t.Fatalf("deaths = %v, want both derived devices", deaths)
	}
}
