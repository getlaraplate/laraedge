package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge-sdk/manifest"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/plugin"
	"github.com/getlaraplate/laraedge/internal/transform"
)

const (
	secretValue = "s3cr3t-value-0042"
	tolerance   = 10 * time.Second
	ts1         = "2026-10-29T11:59:00Z"
)

// fakeResolver resolves references from a map and counts the calls.
type fakeResolver struct {
	mu     sync.Mutex
	values map[string]string
	calls  int
}

func (r *fakeResolver) Resolve(_ context.Context, ref string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls++
	if v, ok := r.values[ref]; ok {
		return v, nil
	}

	return "", errors.New("unknown reference with the value " + secretValue)
}

func (r *fakeResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.calls
}

// collector receives what the input emits.
type collector struct {
	mu   sync.Mutex
	recs []model.Record
}

func (c *collector) emit(r model.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.recs = append(c.recs, r)
}

func (c *collector) all() []model.Record {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]model.Record(nil), c.recs...)
}

func (c *collector) samples(quality model.Quality) []model.Sample {
	var out []model.Sample
	for _, r := range c.all() {
		if r.Sample != nil && r.Sample.Quality == quality {
			out = append(out, *r.Sample)
		}
	}

	return out
}

func (c *collector) events(kind model.EventKind) []model.DeviceEvent {
	var out []model.DeviceEvent
	for _, r := range c.all() {
		if r.Event != nil && r.Event.Kind == kind {
			out = append(out, *r.Event)
		}
	}

	return out
}

// newInputSpec installs the test plugin with behaviour and the plugin.json
// that matches it (a behaviour can change the manifest).
func newInputSpec(t *testing.T, behaviour string) plugin.Spec {
	t.Helper()

	spec := newSpec(t, behaviour)
	out, err := exec.Command(spec.Path, "--manifest").Output()
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(strings.TrimSuffix(string(out), "\n"))
	writeFile(t, filepath.Join(spec.Dir, plugin.ManifestFile), raw, 0o600)
	m, raw, err := manifest.Load(filepath.Join(spec.Dir, plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	spec.Manifest, spec.ManifestRaw, spec.ManifestSHA256 = m, raw, plugin.SumHex(raw)

	return spec
}

// testInput is input in1 with devices d1 (s1, s2) and d2 (s1), all int32.
func testInput(connection string, transforms ...config.Transform) config.Input {
	point := func(signal string) config.Point {
		return config.Point{Signal: signal, Address: "a", Type: "int32", Priority: model.Critical, Read: config.Read{Mode: config.ReadPoll, Interval: time.Second}, Transforms: transforms}
	}
	raw := fmt.Sprintf(`{"id":"in1","driver":"plugin:demo","connection":%s,"devices":[`+
		`{"device":"d1","points":[{"signal":"s1","address":"a","type":"int32","read":{"poll":"1s"}},{"signal":"s2","address":"a","type":"int32","read":{"poll":"1s"}}]},`+
		`{"device":"d2","points":[{"signal":"s1","address":"a","type":"int32","read":{"poll":"1s"}}]}]}`, connection)

	return config.Input{
		ID: "in1", Driver: "plugin:demo", Connection: json.RawMessage(connection), DeathTolerance: tolerance,
		Devices: []config.Device{
			{Device: "d1", Points: []config.Point{point("s1"), point("s2")}},
			{Device: "d2", Points: []config.Point{point("s1")}},
		},
		Raw: json.RawMessage(raw),
	}
}

// inputHarness runs the input on a clock of its own, apart from the host's:
// the host tests rely on counting the timers of the host clock.
type inputHarness struct {
	*harness
	inClk    *clock.Fake
	resolver *fakeResolver
	out      *collector
}

func newInputHarness(t *testing.T, behaviour string) *inputHarness {
	t.Helper()

	h := &inputHarness{
		harness:  newHarness(t, newInputSpec(t, behaviour)),
		inClk:    clock.NewFake(epoch),
		resolver: &fakeResolver{values: map[string]string{"secret://env/TOKEN": secretValue}},
		out:      &collector{},
	}
	h.start()

	return h
}

func (h *inputHarness) build(t *testing.T, cfg config.Input) input.Input {
	t.Helper()

	in, err := plugin.InputFactory(h.host, h.resolver)(cfg, h.inClk)
	if err != nil {
		t.Fatalf("factory = %v", err)
	}
	t.Cleanup(func() { _ = in.Close() })

	return in
}

func (h *inputHarness) startInput(t *testing.T, cfg config.Input) input.Input {
	t.Helper()

	in := h.build(t, cfg)
	if err := in.Start(context.Background(), h.out.emit); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	return in
}

func (h *inputHarness) read(name string) string {
	data, _ := os.ReadFile(filepath.Join(h.spec.Dir, name))

	return string(data)
}

func (h *inputHarness) waitTrace(want string) {
	h.t.Helper()

	h.waitFor("trace "+want, func() bool { return strings.Contains(h.read("trace"), want) })
}

// setBehaviourFor replaces the behaviour read by the next start.
func (h *inputHarness) setBehaviour(behaviour string) { setBehaviour(h.t, h.spec.Path, behaviour) }

// restartHost fires the backoff of the host after a crash and waits for the
// plugin to be up again.
func (h *inputHarness) restartHost() {
	h.t.Helper()

	h.waitFor("the host backoff", func() bool {
		return h.host.Status().State == plugin.StateDown && h.clk.PendingTimers() == 1
	})
	h.clk.Advance(time.Minute)
	h.waitUp()
}

func scriptBehaviour(script string, extra string) string {
	return fmt.Sprintf(`{"trace_file":"trace","script":%s%s}`, script, extra)
}

func sampleEvent(device, signal string, value int, quality string) string {
	return fmt.Sprintf(`{"sample":{"device":%q,"signal":%q,"value":%d,"ts":%q,"quality":%q}}`, device, signal, value, ts1, quality)
}

func TestInputStartConfiguresWithResolvedSecretsAndReturnsAtOnce(t *testing.T) {
	h := newInputHarness(t, `{"config_file":"config.json","trace_file":"trace"}`)

	h.startInput(t, testInput(`{"host":"h","token":"secret://env/TOKEN"}`))

	got := h.read("config.json")
	if !strings.Contains(got, `"token":"`+secretValue+`"`) || strings.Contains(got, "secret://") {
		t.Fatalf("the plugin received %s, want the token resolved", got)
	}
	var section struct {
		ID      string `json:"id"`
		Devices []struct {
			Device string `json:"device"`
		} `json:"devices"`
	}
	if err := json.Unmarshal([]byte(got), &section); err != nil || section.ID != "in1" || len(section.Devices) != 2 {
		t.Fatalf("the plugin received %s (%v), want the section of input in1", got, err)
	}
	if n := h.resolver.count(); n != 1 {
		t.Fatalf("resolver calls = %d, want 1", n)
	}
	if strings.Contains(h.log.String(), secretValue) {
		t.Fatalf("the log holds the secret value:\n%s", h.log.String())
	}
}

func TestInputEmitsSamplesWithPluginTimestampAndQualityThroughTransforms(t *testing.T) {
	script := "[" + sampleEvent("d1", "s1", 5, "good") + "," + sampleEvent("d1", "s2", 7, "bad") + "]"
	h := newInputHarness(t, scriptBehaviour(script, ""))

	cfg := testInput(`{"host":"h"}`, config.Transform{Name: "scale", Params: json.RawMessage("10")})
	proc, err := transform.NewProcessor(cfg, "", h.inClk, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	proc.Start(context.Background())
	t.Cleanup(proc.Close)

	in := h.build(t, cfg)
	if err := in.Start(context.Background(), proc.Wrap(h.out.emit)); err != nil {
		t.Fatal(err)
	}

	h.waitFor("two samples", func() bool { return len(h.out.samples(model.Good))+len(h.out.samples(model.Bad)) == 2 })
	good, bad := h.out.samples(model.Good), h.out.samples(model.Bad)
	want := time.Date(2026, 10, 29, 11, 59, 0, 0, time.UTC)
	if len(good) != 1 || good[0].Device != "d1" || good[0].Signal != "s1" || !good[0].TS.Equal(want) {
		t.Fatalf("good samples = %+v, want d1/s1 at %v", good, want)
	}
	if good[0].Value.Float != 50 && good[0].Value.Int != 50 {
		t.Fatalf("good value = %+v, want 5 scaled by 10", good[0].Value)
	}
	if len(bad) != 1 || bad[0].Signal != "s2" || bad[0].Quality != model.Bad {
		t.Fatalf("bad samples = %+v, want d1/s2 with the plugin's bad quality", bad)
	}
}

func TestInputAnsweredAndHealthFeedTheDevicesAndTheStatus(t *testing.T) {
	script := `[{"health":{"connected":true,"read_error":"timeout"}},{"health":{"connected":true,"read_error":"bogus"}},{"answered":"d1"}]`
	h := newInputHarness(t, scriptBehaviour(script, ""))
	in := h.startInput(t, testInput(`{"host":"h"}`))

	reporter := in.(input.HealthReporter)
	h.waitFor("health", func() bool { return reporter.Health().ReadErrors[input.ReadErrorTimeout] == 1 })
	if got := reporter.Health(); !got.Connected || got.ReadErrors[input.ReadErrorTimeout] != 1 || len(got.ReadErrors) != 1 {
		t.Fatalf("Health() = %+v, want connected with one timeout and the unknown kind ignored", got)
	}
}

func TestInputSamplesOfUnknownPointsAreDropped(t *testing.T) {
	script := "[" + sampleEvent("d9", "s1", 1, "good") + "," + sampleEvent("d1", "s1", 2, "good") + "]"
	h := newInputHarness(t, scriptBehaviour(script, ""))
	h.startInput(t, testInput(`{"host":"h"}`))

	h.waitFor("the known sample", func() bool { return len(h.out.samples(model.Good)) >= 1 })
	for _, s := range h.out.samples(model.Good) {
		if s.Device == "d9" {
			t.Fatalf("the sample of an unknown device was emitted: %+v", s)
		}
	}
}

func TestInputConfigRefusalFailsStartWithPathsAndNoValues(t *testing.T) {
	tests := []struct {
		name      string
		behaviour string
		want      string
	}{
		{"plain", `{"refuse_config":true}`, "inputs.2.connection.port: must be an integer"},
		{"echoing plugin", `{"refuse_config":true,"refuse_echo":true}`, "inputs.2.connection.port: bad value ***"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newInputHarness(t, tc.behaviour)
			in := h.build(t, testInput(`{"host":"h","token":"secret://env/TOKEN"}`))

			err := in.Start(context.Background(), h.out.emit)
			var refused *plugin.ConfigRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Start() = %v, want a ConfigRefusedError", err)
			}
			if got := refused.Messages("inputs.2"); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("Messages = %q, want %q", got, tc.want)
			}
			if strings.Contains(err.Error(), secretValue) || strings.Contains(h.log.String(), secretValue) {
				t.Fatalf("the secret leaked: %v\n%s", err, h.log.String())
			}
			if err := in.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInputStartFailsWhenASecretCannotBeResolved(t *testing.T) {
	h := newInputHarness(t, `{"trace_file":"trace"}`)
	in := h.build(t, testInput(`{"host":"h","auth":{"password":"secret://env/MISSING"}}`))

	err := in.Start(context.Background(), h.out.emit)
	var refused *plugin.ConfigRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Start() = %v, want a ConfigRefusedError", err)
	}
	if got := refused.Messages("inputs.0"); len(got) != 1 || got[0] != "inputs.0.connection.auth.password: the secret cannot be resolved" {
		t.Fatalf("Messages = %q", got)
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("the resolver's words leaked: %v", err)
	}
	if strings.Contains(h.read("trace"), "configure") {
		t.Fatal("the plugin was configured with an unresolved secret")
	}
}

// TestInputPluginCrashRestartsTheInstance is the H6 scenario: a crash gives
// one bad sample per point, the devices die after the tolerance, and the
// restart configures and subscribes again, then the first answer is a birth.
func TestInputPluginCrashRestartsTheInstance(t *testing.T) {
	script := `[{"answered":"d1"}]`
	h := newInputHarness(t, scriptBehaviour(script, ""))
	h.startInput(t, testInput(`{"host":"h"}`))
	h.waitTrace("run in1")

	h.setBehaviour(`{"crash_at_start":true}`)
	h.kill()
	h.waitFor("one bad sample per point", func() bool { return len(h.out.samples(model.Bad)) == 3 })
	bad := h.out.samples(model.Bad)
	for i, want := range [][2]string{{"d1", "s1"}, {"d1", "s2"}, {"d2", "s1"}} {
		s := bad[i]
		if s.Device != want[0] || s.Signal != want[1] || s.Value.Kind != model.KindInt || s.Value.Int != 0 || !s.TS.Equal(epoch) || s.Priority != model.Critical {
			t.Fatalf("bad sample %d = %+v, want %v with the zero int at the clock time", i, s, want)
		}
	}
	if len(h.out.events(model.Death)) != 0 {
		t.Fatal("a device died before the tolerance")
	}

	h.inClk.Advance(tolerance)
	h.waitFor("death of both devices", func() bool { return len(h.out.events(model.Death)) == 2 })
	if got := len(h.out.samples(model.Bad)); got != 3 {
		t.Fatalf("bad samples = %d, want exactly one per point", got)
	}

	h.setBehaviour(scriptBehaviour(script, ""))
	h.restartHost()
	h.waitFor("birth of d1", func() bool { return len(h.out.events(model.Birth)) == 1 })
	birth := h.out.events(model.Birth)[0]
	if birth.Device != "d1" || len(birth.Signals) != 2 {
		t.Fatalf("birth = %+v, want d1 with its two signals", birth)
	}
	if got := h.read("trace"); got != "configure in1\nrun in1\nconfigure in1\nrun in1\n" {
		t.Fatalf("trace = %q, want Configure and Subscribe once per run of the plugin", got)
	}
}

// TestInputRefusedAfterRestartStaysDown is decision H6: the document was
// valid when applied, so a refusal after a restart is logged and the
// devices die; nothing is rolled back.
func TestInputRefusedAfterRestartStaysDown(t *testing.T) {
	h := newInputHarness(t, scriptBehaviour(`[{"answered":"d1"}]`, ""))
	h.startInput(t, testInput(`{"host":"h","token":"secret://env/TOKEN"}`))
	h.waitTrace("run in1")

	h.setBehaviour(scriptBehaviour(`[{"answered":"d1"}]`, `,"refuse_config":true,"refuse_echo":true`))
	h.kill()
	h.restartHost()
	h.waitFor("the refusal in the log", func() bool {
		return strings.Contains(h.log.String(), "refused the configuration after a restart")
	})
	h.inClk.Advance(tolerance)
	h.waitFor("death of both devices", func() bool { return len(h.out.events(model.Death)) == 2 })

	if got := h.read("trace"); got != "configure in1\nrun in1\nconfigure in1\n" {
		t.Fatalf("trace = %q, want a second Configure and no second Subscribe", got)
	}
	if strings.Contains(h.log.String(), secretValue) {
		t.Fatalf("the log holds the secret value:\n%s", h.log.String())
	}
	if !strings.Contains(h.log.String(), "input.connection.port: bad value ***") {
		t.Fatalf("the log does not name the path:\n%s", h.log.String())
	}
	if n := h.resolver.count(); n != 2 {
		t.Fatalf("resolver calls = %d, want one per Configure", n)
	}
}

func TestInputStartWithThePluginDownReportsBadSamples(t *testing.T) {
	h := &inputHarness{
		harness:  &harness{t: t, clk: clock.NewFake(epoch), log: &lockedBuffer{}},
		inClk:    clock.NewFake(epoch),
		resolver: &fakeResolver{},
		out:      &collector{},
	}
	spec := newInputSpec(t, "")
	h.host = plugin.NewHost(spec, plugin.HostOptions{DataDir: shortTempDir(t), Clock: h.clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	in := h.build(t, testInput(`{"host":"h"}`))
	if err := in.Start(context.Background(), h.out.emit); err != nil {
		t.Fatalf("Start() = %v, want no error with the plugin down", err)
	}
	h.waitFor("one bad sample per point", func() bool { return len(h.out.samples(model.Bad)) == 3 })
	if in.(input.HealthReporter).Health().Connected {
		t.Fatal("a down plugin reports connected")
	}
}

func TestInputCloseEndsTheStreamAndEmitsNothingAfter(t *testing.T) {
	h := newInputHarness(t, scriptBehaviour(`[`+sampleEvent("d1", "s1", 1, "good")+`]`, ""))
	in := h.startInput(t, testInput(`{"host":"h"}`))
	h.waitFor("the sample", func() bool { return len(h.out.samples(model.Good)) == 1 })

	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	h.waitTrace("stop in1")
	before := len(h.out.all())
	h.inClk.Advance(time.Hour)
	if got := len(h.out.all()); got != before {
		t.Fatalf("%d records after Close", got-before)
	}
	if err := in.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	if err := in.Start(context.Background(), h.out.emit); err == nil {
		t.Fatal("Start after Close succeeded")
	}
}

func TestInputCloseOfAnInputNeverStarted(t *testing.T) {
	h := newInputHarness(t, "")
	if err := h.build(t, testInput(`{"host":"h"}`)).Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

func TestInputProbeReturnsThePluginSamples(t *testing.T) {
	probe := fmt.Sprintf(`[{"device":"d1","signal":"s1","value":3,"ts":%q},{"device":"d2","signal":"s1","value":4,"ts":%q,"quality":"bad"}]`, ts1, ts1)
	h := newInputHarness(t, fmt.Sprintf(`{"probe":%s}`, probe))
	in := h.build(t, testInput(`{"host":"h"}`))

	samples, err := in.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe() = %v", err)
	}
	if len(samples) != 2 || samples[0].Value.Int != 3 || samples[0].Quality != model.Good || samples[1].Quality != model.Bad {
		t.Fatalf("Probe() = %+v", samples)
	}
	if len(h.out.all()) != 0 {
		t.Fatal("Probe emitted records")
	}
}

func TestInputProbeGivesUpAfter30Seconds(t *testing.T) {
	h := newInputHarness(t, `{"probe_block":true}`)
	in := h.build(t, testInput(`{"host":"h"}`))

	result := make(chan error, 1)
	go func() {
		_, err := in.Probe(context.Background())
		result <- err
	}()

	deadline := time.Now().Add(guard)
	for {
		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), "did not answer in time") {
				t.Fatalf("Probe() = %v, want the timeout", err)
			}

			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Probe did not give up")
		}
		if h.clk.PendingTimers() > 0 {
			h.clk.Advance(30 * time.Second)
		}
	}
}

func TestInputProbeWithThePluginDownFails(t *testing.T) {
	h := newInputHarness(t, "")
	in := h.build(t, testInput(`{"host":"h"}`))
	h.stop()

	if _, err := in.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Probe() = %v, want the plugin is not running", err)
	}
}

func TestInputBrowseIsOfferedOnlyWhenTheManifestDeclaresIt(t *testing.T) {
	tests := []struct {
		name      string
		behaviour string
		browse    bool
	}{
		{"declared", `{"browse":true}`, true},
		{"not declared", `{}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newInputHarness(t, tc.behaviour)
			in := h.build(t, testInput(`{"host":"h"}`))

			browser, ok := in.(plugin.Browser)
			if ok != tc.browse {
				t.Fatalf("implements Browser = %v, want %v", ok, tc.browse)
			}
			if !ok {
				return
			}
			nodes, err := browser.Browse(context.Background(), "root", 2)
			if err != nil {
				t.Fatalf("Browse() = %v", err)
			}
			if len(nodes) != 1 || nodes[0] != (plugin.Node{Address: "root/2", Name: "n1", Type: "int32", Access: "read"}) {
				t.Fatalf("Browse() = %+v", nodes)
			}
			if _, err := browser.Browse(context.Background(), "", -1); err == nil {
				t.Fatal("Browse with a negative depth succeeded")
			}
		})
	}
}

func TestInputFactoryRefusesAPluginWithoutTheInputKind(t *testing.T) {
	spec := newSpecWithManifest(t, "", []byte(`{"kinds":{"output":{"config_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}}},"name":"demo","protocol_versions":["v1"],"version":"0.1.0"}`))
	host := plugin.NewHost(spec, plugin.HostOptions{DataDir: shortTempDir(t)})

	if _, err := plugin.InputFactory(host, nil)(testInput(`{}`), clock.NewFake(epoch)); err == nil {
		t.Fatal("the factory accepted a plugin without the input kind")
	}
}

func TestHostWatchDeliversEveryRestartToEverySubscriber(t *testing.T) {
	h := newHarness(t, newSpec(t, ""))
	first, cancelFirst := h.host.Watch()
	second, cancelSecond := h.host.Watch()
	defer cancelSecond()
	h.start()

	for name, ch := range map[string]<-chan uint64{"first": first, "second": second} {
		if got := <-ch; got != 1 {
			t.Fatalf("%s watcher got generation %d, want 1", name, got)
		}
	}

	cancelFirst()
	h.kill()
	h.waitFor("the host backoff", func() bool {
		return h.host.Status().State == plugin.StateDown && h.clk.PendingTimers() == 1
	})
	h.clk.Advance(time.Minute)
	h.waitUp()

	if got := <-second; got != 2 {
		t.Fatalf("second watcher got generation %d, want 2", got)
	}
	select {
	case got := <-first:
		t.Fatalf("the cancelled watcher got generation %d", got)
	default:
	}
}
