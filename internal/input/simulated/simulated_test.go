package simulated

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/input"
	"github.com/swolley/laraplate-edge/internal/model"
)

// guard bounds every wait for a goroutine; it is a deadlock guard, never a
// timing assertion.
const guard = 10 * time.Second

// The simulator is an input.Input and its factory an input.Factory.
var (
	_ input.Input   = (*Input)(nil)
	_ input.Factory = Factory("")
)

var t0 = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

// certain is a scenario without randomness: idle becomes running at the
// first tick, a part completes every 3 ticks, none is scrap and every
// measurement is exactly 10.
const certain = `{"tick":"1s","initial":"idle","states":[
	{"name":"idle","transitions":{"running":1}},
	{"name":"running","producing":true}],
	"cycle_time":{"mean":"3s","stddev":"0s"},"scrap_rate":0,
	"measurements":[{"signal":"temperature","mean":10,"stddev":0,"unit":"C"}]}`

// collector gathers emitted records; safe for concurrent use.
type collector struct {
	mu   sync.Mutex
	recs []model.Record
}

func (c *collector) emit(rec model.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.recs = append(c.recs, rec)
}

func (c *collector) records() []model.Record {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.recs)
}

// samples returns the samples of device (all devices when empty) as
// "signal=value@seconds-since-t0" strings, in emission order.
func (c *collector) samples(device string) []string {
	var out []string
	for _, rec := range c.records() {
		if rec.Sample == nil || (device != "" && rec.Sample.Device != device) {
			continue
		}
		out = append(out, describe(*rec.Sample))
	}

	return out
}

func describe(s model.Sample) string {
	return fmt.Sprintf("%s=%s@%d", s.Signal, valueText(s.Value), int(s.TS.Sub(t0)/time.Second))
}

func valueText(v model.Value) string {
	switch v.Kind {
	case model.KindString:
		return v.Str
	case model.KindInt:
		return fmt.Sprint(v.Int)
	case model.KindFloat:
		return fmt.Sprintf("%g", v.Float)
	default:
		return fmt.Sprintf("kind%d", v.Kind)
	}
}

func inline(scenario string, seed ...int64) json.RawMessage {
	conn := `{"scenario":` + scenario
	if len(seed) > 0 {
		conn += fmt.Sprintf(`,"seed":%d`, seed[0])
	}

	return json.RawMessage(conn + `}`)
}

// press returns a device with every kind of point.
func press(name string) config.Device {
	return config.Device{Device: name, Points: []config.Point{
		{Signal: "state", Address: "state", Type: "string", Priority: model.Critical},
		{Signal: "good", Address: "good_count", Type: "uint32", Unit: "pcs", Priority: model.Critical},
		{Signal: "scrap", Address: "scrap_count", Type: "uint32", Unit: "pcs", Priority: model.Critical},
		{Signal: "temp", Address: "temperature", Type: "float64", Unit: "C", Priority: model.Bulk},
	}}
}

func inputOf(conn json.RawMessage, devices ...config.Device) config.Input {
	return config.Input{ID: "line", Driver: config.DriverSimulated, Connection: conn, Devices: devices}
}

// waitArmed waits until the input's goroutine has registered its timer.
func waitArmed(t *testing.T, clk *clock.Fake) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for clk.PendingTimers() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the input did not arm its timer")
		}
		runtime.Gosched()
	}
}

// drive starts in on a fake clock, advances it by step n times, closes the
// input and returns what it emitted.
func drive(t *testing.T, in input.Input, clk *clock.Fake, n int, step time.Duration) *collector {
	t.Helper()

	c := &collector{}
	if err := in.Start(t.Context(), c.emit); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range n {
		waitArmed(t, clk)
		clk.Advance(step)
	}
	waitArmed(t, clk)
	if err := in.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	return c
}

func mustNew(t *testing.T, cfg config.Input, seed int64) (*Input, *clock.Fake) {
	t.Helper()

	clk := clock.NewFake(t0)
	in, err := New(cfg, clk, seed)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return in, clk
}

func TestScenarioDrivesStatesCountersAndMeasurements(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	got := drive(t, in, clk, 7, time.Second).samples("")

	want := []string{
		"state=idle@0", "good=0@0", "scrap=0@0",
		"state=running@1",
		"good=1@4", "temp=10@4",
		"good=2@7", "temp=10@7",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("emitted\n%v\nwant\n%v", got, want)
	}
}

func TestEveryStateChangeAbandonsThePartInProgress(t *testing.T) {
	// a and b both produce; a becomes b at the first tick. The part of b
	// starts then, so with a cycle of 3 ticks it completes at tick 4. Had the
	// progress made in a been kept, it would complete at tick 3.
	twoProducing := `{"tick":"1s","initial":"a","states":[
		{"name":"a","producing":true,"transitions":{"b":1}},
		{"name":"b","producing":true}],
		"cycle_time":{"mean":"3s","stddev":"0s"},"scrap_rate":0,
		"measurements":[{"signal":"temperature","mean":10,"stddev":0}]}`
	in, clk := mustNew(t, inputOf(inline(twoProducing), press("press-07")), 1)
	got := drive(t, in, clk, 5, time.Second).samples("")

	want := []string{"state=a@0", "good=0@0", "scrap=0@0", "state=b@1", "good=1@4", "temp=10@4"}
	if !slices.Equal(got, want) {
		t.Fatalf("emitted\n%v\nwant\n%v", got, want)
	}
}

func TestScrapRateDecidesWhichCounterMoves(t *testing.T) {
	scrapOnly := strings.Replace(certain, `"scrap_rate":0`, `"scrap_rate":1`, 1)
	in, clk := mustNew(t, inputOf(inline(scrapOnly), press("press-07")), 1)
	got := drive(t, in, clk, 7, time.Second).samples("")

	want := []string{
		"state=idle@0", "good=0@0", "scrap=0@0",
		"state=running@1",
		"scrap=1@4", "temp=10@4",
		"scrap=2@7", "temp=10@7",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("emitted\n%v\nwant\n%v", got, want)
	}
}

func TestRecordsCarryDeviceTimestampQualityAndPointPriority(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	recs := drive(t, in, clk, 4, time.Second).records()

	if len(recs) == 0 {
		t.Fatal("nothing emitted")
	}
	for _, rec := range recs {
		if rec.Event != nil {
			t.Fatalf("the simulator emitted a device event: %+v", rec.Event)
		}
		s := rec.Sample
		if s == nil {
			t.Fatal("empty record")
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("invalid sample %+v: %v", s, err)
		}
		if s.Device != "press-07" || s.Quality != model.Good {
			t.Errorf("sample %+v: want device press-07 and quality good", s)
		}
		if s.TS.Location() != time.UTC {
			t.Errorf("sample %s: timestamp not UTC", s.Signal)
		}
		wantPriority := model.Critical
		if s.Signal == "temp" {
			wantPriority = model.Bulk
		}
		if s.Priority != wantPriority {
			t.Errorf("sample %s: priority %v, want %v", s.Signal, s.Priority, wantPriority)
		}
	}
}

func TestValueKindsMatchTheSignals(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	kinds := map[string]model.ValueKind{}
	for _, rec := range drive(t, in, clk, 4, time.Second).records() {
		kinds[rec.Sample.Signal] = rec.Sample.Value.Kind
	}

	want := map[string]model.ValueKind{"state": model.KindString, "good": model.KindInt, "scrap": model.KindInt, "temp": model.KindFloat}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
}

func TestOnlyConfiguredPointsAreEmitted(t *testing.T) {
	dev := config.Device{Device: "press-07", Points: []config.Point{
		{Signal: "temp", Address: "temperature", Type: "float64", Priority: model.Bulk},
	}}
	in, clk := mustNew(t, inputOf(inline(certain), dev), 1)
	got := drive(t, in, clk, 7, time.Second).samples("")

	if want := []string{"temp=10@4", "temp=10@7"}; !slices.Equal(got, want) {
		t.Fatalf("emitted %v, want %v", got, want)
	}
}

func TestTwoPointsMayAddressTheSameSignal(t *testing.T) {
	dev := config.Device{Device: "press-07", Points: []config.Point{
		{Signal: "state", Address: "state", Type: "string", Priority: model.Critical},
		{Signal: "mode", Address: "state", Type: "string", Priority: model.Bulk},
	}}
	in, clk := mustNew(t, inputOf(inline(certain), dev), 1)
	got := drive(t, in, clk, 1, time.Second).samples("")

	if want := []string{"state=idle@0", "mode=idle@0", "state=running@1", "mode=running@1"}; !slices.Equal(got, want) {
		t.Fatalf("emitted %v, want %v", got, want)
	}
}

func TestSameSeedGivesTheSameSequenceAndAnotherSeedDoesNot(t *testing.T) {
	run := func(seed int64) []model.Record {
		in, clk := mustNew(t, inputOf(inline(validScenario), press("press-07"), press("press-08")), seed)

		return drive(t, in, clk, 600, time.Second).records()
	}

	a, b, other := run(7), run(7), run(8)
	if len(a) < 100 {
		t.Fatalf("only %d records in 600 ticks", len(a))
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two runs with the same seed emitted different sequences")
	}
	if reflect.DeepEqual(a, other) {
		t.Fatal("two seeds emitted the same sequence")
	}
}

func TestStochasticRunFollowsTheScenario(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(validScenario), press("press-07")), 3)
	recs := drive(t, in, clk, 3000, time.Second).records()

	transitions := map[string][]string{"idle": {"running", "setup"}, "running": {"idle", "fault"}, "setup": {"idle"}, "fault": {"idle"}}
	states := map[string]bool{}
	var current string
	// The counters start with a sample of 0, the baseline the backend takes
	// its first delta from.
	good, scrap := int64(-1), int64(-1)
	var temps, parts int64
	for _, rec := range recs {
		s := rec.Sample
		switch s.Signal {
		case "state":
			next := s.Value.Str
			states[next] = true
			if current != "" && !slices.Contains(transitions[current], next) {
				t.Fatalf("state went %s -> %s, which the scenario does not allow", current, next)
			}
			current = next
		case "good":
			good++
			if s.Value.Int != good {
				t.Fatalf("good_count = %d, want %d (the previous value plus one)", s.Value.Int, good)
			}
		case "scrap":
			scrap++
			if s.Value.Int != scrap {
				t.Fatalf("scrap_count = %d, want %d (the previous value plus one)", s.Value.Int, scrap)
			}
		case "temp":
			temps++
			if s.Value.Float < 60 || s.Value.Float > 100 {
				t.Fatalf("temperature %v is more than 10 standard deviations from its mean", s.Value.Float)
			}
		}
	}
	parts = good + scrap // both counters are one sample ahead of their parts, the baseline

	if len(states) != 4 {
		t.Errorf("visited states %v, want all four", states)
	}
	if good < 1 || scrap < 1 {
		t.Errorf("good = %d, scrap = %d: want both to move", good, scrap)
	}
	if temps != parts {
		t.Errorf("%d measurements for %d completed parts", temps, parts)
	}
	if ratio := float64(scrap) / float64(parts); ratio < 0.1 || ratio > 0.3 {
		t.Errorf("scrap ratio %.2f, scenario says 0.2", ratio)
	}
}

func TestADeviceSequenceDoesNotDependOnItsNeighbours(t *testing.T) {
	alone, clkA := mustNew(t, inputOf(inline(validScenario), press("press-07")), 5)
	both, clkB := mustNew(t, inputOf(inline(validScenario), press("press-06"), press("press-07")), 5)

	a := drive(t, alone, clkA, 300, time.Second).samples("press-07")
	b := drive(t, both, clkB, 300, time.Second).samples("press-07")
	if !slices.Equal(a, b) {
		t.Fatal("press-07 emitted a different sequence when another device was configured")
	}
}

func TestDevicesWithDifferentNamesDiverge(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(validScenario), press("press-06"), press("press-07")), 5)
	c := drive(t, in, clk, 600, time.Second)

	if slices.Equal(c.samples("press-06"), c.samples("press-07")) {
		t.Fatal("two devices emitted the same sequence")
	}
}

func TestACoarseAdvanceCatchesUpTickByTick(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	c := &collector{}
	if err := in.Start(t.Context(), c.emit); err != nil {
		t.Fatal(err)
	}
	waitArmed(t, clk)
	clk.Advance(7 * time.Second)
	waitArmed(t, clk)
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}

	want := []string{"state=idle@0", "good=0@0", "scrap=0@0", "state=running@1", "good=1@4", "temp=10@4", "good=2@7", "temp=10@7"}
	if got := c.samples(""); !slices.Equal(got, want) {
		t.Fatalf("emitted\n%v\nwant\n%v", got, want)
	}
}

func TestProbeReturnsOneSamplePerPointInConfigOrder(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(certain), press("press-07"), press("press-08")), 1)
	c := &collector{}
	if err := in.Start(t.Context(), c.emit); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		waitArmed(t, clk)
		clk.Advance(time.Second)
	}
	waitArmed(t, clk)

	before := len(c.records())
	samples, err := in.Probe(t.Context())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if len(c.records()) != before {
		t.Fatal("Probe emitted records")
	}

	var got []string
	for _, s := range samples {
		if s.Quality != model.Good {
			t.Errorf("probe sample %s/%s has quality %v", s.Device, s.Signal, s.Quality)
		}
		if !s.TS.Equal(t0.Add(4 * time.Second)) {
			t.Errorf("probe sample %s/%s at %v, want the clock time", s.Device, s.Signal, s.TS)
		}
		if err := s.Validate(); err != nil {
			t.Errorf("probe sample %+v: %v", s, err)
		}
		got = append(got, s.Device+"/"+s.Signal+"="+valueText(s.Value))
	}
	want := []string{
		"press-07/state=running", "press-07/good=1", "press-07/scrap=0", "press-07/temp=10",
		"press-08/state=running", "press-08/good=1", "press-08/scrap=0", "press-08/temp=10",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Probe = %v, want %v", got, want)
	}
}

func TestProbeOfAnInputNeverStartedReadsTheInitialValues(t *testing.T) {
	in, _ := mustNew(t, inputOf(inline(certain), press("press-07")), 1)

	samples, err := in.Probe(t.Context())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	var got []string
	for _, s := range samples {
		got = append(got, s.Signal+"="+valueText(s.Value))
	}
	// A measurement not yet drawn reads as its mean.
	if want := []string{"state=idle", "good=0", "scrap=0", "temp=10"}; !slices.Equal(got, want) {
		t.Fatalf("Probe = %v, want %v", got, want)
	}
}

func TestProbeHonoursACancelledContext(t *testing.T) {
	in, _ := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := in.Probe(ctx); err == nil {
		t.Fatal("Probe ignored a cancelled context")
	}
}

func TestCloseStopsTheGoroutineAndIsSafeEverywhere(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	c := &collector{}
	if err := in.Start(t.Context(), c.emit); err != nil {
		t.Fatal(err)
	}
	waitArmed(t, clk)
	if err := in.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := clk.PendingTimers(); n != 0 {
		t.Fatalf("%d timers pending after Close", n)
	}

	count := len(c.records())
	clk.Advance(10 * time.Second)
	if len(c.records()) != count {
		t.Fatal("the input emitted after Close")
	}
	if err := in.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	unstarted, _ := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	if err := unstarted.Close(); err != nil {
		t.Fatalf("Close of an input never started: %v", err)
	}
	if err := unstarted.Start(t.Context(), c.emit); err == nil {
		t.Fatal("Start after Close succeeded")
	}
}

func TestCancellingTheStartContextStopsTheGoroutine(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	ctx, cancel := context.WithCancel(t.Context())
	c := &collector{}
	if err := in.Start(ctx, c.emit); err != nil {
		t.Fatal(err)
	}
	waitArmed(t, clk)
	cancel()

	deadline := time.Now().Add(guard)
	for clk.PendingTimers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the goroutine kept its timer after the context was cancelled")
		}
		runtime.Gosched()
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartRefusesASecondStartAndACancelledContext(t *testing.T) {
	in, clk := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	c := &collector{}
	if err := in.Start(t.Context(), c.emit); err != nil {
		t.Fatal(err)
	}
	waitArmed(t, clk)
	if err := in.Start(t.Context(), c.emit); err == nil {
		t.Error("a second Start succeeded")
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}

	again, _ := mustNew(t, inputOf(inline(certain), press("press-07")), 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := again.Start(ctx, c.emit); err == nil {
		t.Error("Start with a cancelled context succeeded")
	}
}

func TestNewRefusesWithTheFieldNamed(t *testing.T) {
	dev := press("press-07")
	badAddress := config.Device{Device: "press-09", Points: []config.Point{
		{Signal: "state", Address: "state", Type: "string"},
		{Signal: "humidity", Address: "humidity", Type: "float64"},
	}}
	stateAsNumber := config.Device{Device: "press-07", Points: []config.Point{{Signal: "state", Address: "state", Type: "uint16"}}}
	stateUntyped := config.Device{Device: "press-07", Points: []config.Point{{Signal: "state", Address: "state"}}}
	counterAsString := config.Device{Device: "press-07", Points: []config.Point{{Signal: "good", Address: "good_count", Type: "string"}}}
	measureAsBool := config.Device{Device: "press-07", Points: []config.Point{{Signal: "temp", Address: "temperature", Type: "bool"}}}

	tests := []struct {
		name string
		cfg  config.Input
		want []string
	}{
		{"no scenario", inputOf(json.RawMessage(`{}`), dev), []string{"scenario", "scenario_file"}},
		{"no connection", inputOf(nil, dev), []string{"scenario", "scenario_file"}},
		{"both scenarios", inputOf(json.RawMessage(`{"scenario":`+certain+`,"scenario_file":"s.yaml"}`), dev), []string{"scenario", "scenario_file", "exactly one"}},
		{"null scenario", inputOf(json.RawMessage(`{"scenario":null}`), dev), []string{"scenario"}},
		{"unknown key", inputOf(json.RawMessage(`{"scenario":`+certain+`,"speed":3}`), dev), []string{"connection", "unknown key speed"}},
		{"seed not an integer", inputOf(json.RawMessage(`{"scenario":`+certain+`,"seed":1.5}`), dev), []string{"connection.seed"}},
		{"seed as text", inputOf(json.RawMessage(`{"scenario":`+certain+`,"seed":"one"}`), dev), []string{"connection.seed"}},
		{"scenario file not text", inputOf(json.RawMessage(`{"scenario_file":3}`), dev), []string{"connection.scenario_file"}},
		{"empty scenario file", inputOf(json.RawMessage(`{"scenario_file":""}`), dev), []string{"connection.scenario_file"}},
		{"relative file without a base directory", inputOf(json.RawMessage(`{"scenario_file":"scenario.yaml"}`), dev), []string{"connection.scenario_file", "relative", "base directory"}},
		{"invalid scenario", inputOf(json.RawMessage(`{"scenario":{"initial":"idle","states":[]}}`), dev), []string{"connection.scenario", "states"}},
		{"address not offered", inputOf(inline(certain), badAddress), []string{"press-09", "humidity", "scenario"}},
		{"state as a number", inputOf(inline(certain), stateAsNumber), []string{"press-07", "state", "string"}},
		{"state without a type", inputOf(inline(certain), stateUntyped), []string{"press-07", "state", "string"}},
		{"counter as a string", inputOf(inline(certain), counterAsString), []string{"press-07", "good", "number"}},
		{"measurement as a bool", inputOf(inline(certain), measureAsBool), []string{"press-07", "temp", "number"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := New(tt.cfg, clock.NewFake(t0), 1)
			if err == nil {
				t.Fatalf("New accepted the input: %+v", in)
			}
			if in != nil {
				t.Error("New returned an input with its error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func writeScenario(t *testing.T, dir, name, content string) string {
	t.Helper()

	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

func TestFactoryReadsTheSeedAndResolvesTheScenarioFileAgainstTheBaseDir(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "scenario.json", validScenario)
	abs := writeScenario(t, t.TempDir(), "elsewhere.json", validScenario)

	fileInput := func(file string, seed int) config.Input {
		return inputOf(json.RawMessage(fmt.Sprintf(`{"scenario_file":%q,"seed":%d}`, file, seed)), press("press-07"))
	}
	run := func(factory input.Factory, cfg config.Input) []string {
		clk := clock.NewFake(t0)
		in, err := factory(cfg, clk)
		if err != nil {
			t.Fatalf("factory: %v", err)
		}

		return drive(t, in, clk, 300, time.Second).samples("")
	}

	reference, clk := mustNew(t, inputOf(inline(validScenario), press("press-07")), 11)
	want := drive(t, reference, clk, 300, time.Second).samples("")

	relative := run(Factory(dir), fileInput("scenario.json", 11))
	absolute := run(Factory(dir), fileInput(abs, 11))
	if !slices.Equal(relative, want) || !slices.Equal(absolute, want) {
		t.Fatal("a scenario file and the same inline scenario with the seed 11 emitted different sequences")
	}
	if slices.Equal(run(Factory(dir), fileInput("scenario.json", 12)), want) {
		t.Fatal("the seed of the connection is ignored")
	}
}

func TestFactoryDefaultsTheSeedToOne(t *testing.T) {
	cfg := inputOf(inline(validScenario), press("press-07"))

	clk := clock.NewFake(t0)
	in, err := Factory("")(cfg, clk)
	if err != nil {
		t.Fatal(err)
	}
	got := drive(t, in, clk, 200, time.Second).samples("")

	explicit, clkE := mustNew(t, cfg, 1)
	if want := drive(t, explicit, clkE, 200, time.Second).samples(""); !slices.Equal(got, want) {
		t.Fatal("the default seed is not 1")
	}
}

func TestFactoryRefusesARelativeFileWithoutABaseDir(t *testing.T) {
	cfg := inputOf(json.RawMessage(`{"scenario_file":"scenario.yaml"}`), press("press-07"))

	if _, err := Factory("")(cfg, clock.NewFake(t0)); err == nil || !strings.Contains(err.Error(), "scenario_file") {
		t.Fatalf("err = %v, want a refusal naming scenario_file", err)
	}
}

func TestAnUnreadableScenarioFileFailsStartNotTheFactory(t *testing.T) {
	dir := t.TempDir()
	cfg := inputOf(json.RawMessage(`{"scenario_file":"missing.yaml"}`), press("press-07"))

	clk := clock.NewFake(t0)
	in, err := Factory(dir)(cfg, clk)
	if err != nil {
		t.Fatalf("the factory read the scenario file: %v", err)
	}
	c := &collector{}
	startErr := in.Start(t.Context(), c.emit)
	if startErr == nil || !strings.Contains(startErr.Error(), "missing.yaml") {
		t.Fatalf("Start = %v, want an error naming the file", startErr)
	}
	if err := in.Close(); err != nil {
		t.Fatalf("Close after a failed Start: %v", err)
	}

	// The file appears: a new instance starts.
	writeScenario(t, dir, "missing.yaml", certain)
	again, err := Factory(dir)(cfg, clk)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Start(t.Context(), c.emit); err != nil {
		t.Fatalf("Start with the file present: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAnInvalidScenarioFileFailsTheFactory(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "bad.yaml", "initial: idle\nstates: []\n")
	cfg := inputOf(json.RawMessage(`{"scenario_file":"bad.yaml"}`), press("press-07"))

	if _, err := Factory(dir)(cfg, clock.NewFake(t0)); err == nil || !strings.Contains(err.Error(), "states") {
		t.Fatalf("err = %v, want the scenario problem", err)
	}
}

func TestAnUnreadableFileWithAnAddressProblemFailsStartWithTheAddress(t *testing.T) {
	dir := t.TempDir()
	cfg := inputOf(json.RawMessage(`{"scenario_file":"late.yaml"}`), config.Device{Device: "press-07", Points: []config.Point{
		{Signal: "humidity", Address: "humidity", Type: "float64"},
	}})

	in, err := Factory(dir)(cfg, clock.NewFake(t0))
	if err != nil {
		t.Fatal(err)
	}
	writeScenario(t, dir, "late.yaml", certain)
	err = in.Start(t.Context(), func(model.Record) {})
	if err == nil || !strings.Contains(err.Error(), "humidity") {
		t.Fatalf("Start = %v, want the address problem", err)
	}
	_ = in.Close()
}

func TestScenarioFileLargerThanTheLimitIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "big.yaml", "# "+strings.Repeat("x", maxScenarioSize)+"\n")
	cfg := inputOf(json.RawMessage(`{"scenario_file":"big.yaml"}`), press("press-07"))

	in, err := Factory(dir)(cfg, clock.NewFake(t0))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Start(t.Context(), func(model.Record) {}); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("Start = %v, want a size refusal", err)
	}
	_ = in.Close()
}
