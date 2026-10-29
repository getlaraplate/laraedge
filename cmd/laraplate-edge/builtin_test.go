package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge-sdk/conformance"
	"github.com/getlaraplate/laraedge-sdk/manifest"
	"github.com/getlaraplate/laraedge-sdk/sdk"

	"github.com/getlaraplate/laraedge/internal/clock"
)

// longGuard bounds the waits on a process: a deadlock guard, never a timing
// assertion.
const longGuard = 60 * time.Second

const (
	builtinDir        = "testdata/builtin-simulated"
	builtinSimulated  = "laraedge-builtin-simulated"
	builtinPluginName = "laraedge-builtin-simulated"
)

// The tools the tests run as processes, built once into builtTools and
// removed by TestMain: the agent under its busybox name, and the test plugin
// of internal/plugin/testdata/plugin.
var (
	toolsOnce  sync.Once
	toolsDir   string
	toolsErr   error
	builtTools = map[string]string{}
)

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}

	return name
}

func buildTools() {
	toolsDir, toolsErr = os.MkdirTemp("", "lpt")
	if toolsErr != nil {
		return
	}

	for name, pkg := range map[string]string{
		builtinSimulated: ".",
		"testplugin":     "../../internal/plugin/testdata/plugin",
	} {
		out := filepath.Join(toolsDir, exe(name))
		if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			toolsErr = fmt.Errorf("build %s: %w\n%s", pkg, err, b)

			return
		}
		builtTools[name] = out
	}
}

// removeBuiltTools is called by TestMain.
func removeBuiltTools() {
	if toolsDir != "" {
		_ = os.RemoveAll(toolsDir)
	}
}

// tool returns the path of a built tool.
func tool(t *testing.T, name string) string {
	t.Helper()

	toolsOnce.Do(buildTools)
	if toolsErr != nil {
		t.Fatal(toolsErr)
	}

	return builtTools[name]
}

// shortTempDir returns a temporary directory with a short path: the plugin
// socket path must fit the Unix limit.
func shortTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "lps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

func copyFile(t *testing.T, from, to string, mode os.FileMode) {
	t.Helper()

	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, mode); err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinDriver(t *testing.T) {
	tests := []struct {
		exe        string
		wantDriver string
		wantOK     bool
	}{
		{"laraedge-builtin-simulated", "simulated", true},
		{"/usr/local/bin/laraedge-builtin-simulated", "simulated", true},
		{`C:\agent\laraedge-builtin-simulated.exe`, "simulated", runtime.GOOS == "windows"},
		{"laraedge-builtin-simulated.EXE", "simulated", true},
		{"laraedge-builtin-modbus", "modbus", true},
		{"laraplate-edge", "", false},
		{"laraplate-edge.exe", "", false},
		{"laraedge-builtin", "", false},
		{"my-laraedge-builtin-simulated", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.exe, func(t *testing.T) {
			driver, ok := builtinDriver(tc.exe)
			if ok != tc.wantOK || (ok && driver != tc.wantDriver) {
				t.Fatalf("builtinDriver(%q) = %q, %v; want %q, %v", tc.exe, driver, ok, tc.wantDriver, tc.wantOK)
			}
		})
	}
}

func TestServeBuiltinRefusesTheDriversItDoesNotServe(t *testing.T) {
	for _, driver := range []string{"modbus", "opcua", "nonesuch"} {
		var stderr bytes.Buffer
		if code := serveBuiltin("laraedge-builtin-"+driver, driver, &stderr); code != exitUsage {
			t.Errorf("%s: exit code = %d, want %d", driver, code, exitUsage)
		}
		if want := "cannot be served as a plugin"; !strings.Contains(stderr.String(), want) {
			t.Errorf("%s: stderr = %q, want it to contain %q", driver, stderr.String(), want)
		}
	}
}

// installBuiltin copies the agent built as the busybox plugin, with the
// committed plugin.json and conformance.json, into a fresh directory.
func installBuiltin(t *testing.T) string {
	t.Helper()

	dir := shortTempDir(t)
	copyFile(t, tool(t, builtinSimulated), filepath.Join(dir, exe(builtinSimulated)), 0o700)
	for _, f := range []string{"plugin.json", "conformance.json"} {
		copyFile(t, filepath.Join(builtinDir, f), filepath.Join(dir, f), 0o600)
	}

	return dir
}

func TestBuiltinSimulatedManifestIsTheCommittedOne(t *testing.T) {
	dir := installBuiltin(t)

	out, err := exec.Command(filepath.Join(dir, exe(builtinSimulated)), "--manifest").Output()
	if err != nil {
		t.Fatalf("--manifest: %v", err)
	}

	committed, err := os.ReadFile(filepath.Join(builtinDir, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.TrimSuffix(out, []byte("\n")); !bytes.Equal(got, committed) {
		t.Errorf("--manifest differs from the committed plugin.json:\n got: %s\nwant: %s", got, committed)
	}

	canonical, err := manifest.Canonical(committed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, committed) {
		t.Errorf("the committed plugin.json is not in canonical form:\n got: %s\nwant: %s", committed, canonical)
	}

	var m manifest.Manifest
	if err := json.Unmarshal(committed, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != builtinPluginName || m.Kinds.Input == nil || m.Kinds.Input.Browse {
		t.Errorf("manifest = %+v, want the input plugin %s without browse", m, builtinPluginName)
	}
}

func TestBuiltinSimulatedPassesTheConformanceSuite(t *testing.T) {
	dir := installBuiltin(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*longGuard)
	defer cancel()

	report := conformance.Run(ctx, dir, conformance.Options{Timeout: longGuard})
	for _, c := range report.Checks {
		if !c.Passed {
			t.Errorf("check %s failed: %s", c.Name, c.Detail)
		}
	}
	if !report.Passed {
		t.Fatalf("the conformance suite did not pass (%d checks)", len(report.Checks))
	}
}

// recorder is an sdk.InputEmitter that keeps what it is given.
type recorder struct {
	mu       sync.Mutex
	samples  []sdk.Sample
	answered []string
	// failAfter makes Sample fail once that many samples were taken; <0 never.
	failAfter int
	got       chan struct{}
}

func newRecorder(failAfter int) *recorder {
	return &recorder{failAfter: failAfter, got: make(chan struct{}, 64)}
}

func (r *recorder) Sample(s sdk.Sample) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.failAfter >= 0 && len(r.samples) >= r.failAfter {
		return errors.New("the stream ended")
	}
	r.samples = append(r.samples, s)
	r.got <- struct{}{}

	return nil
}

func (r *recorder) Answered(device string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answered = append(r.answered, device)

	return nil
}

func (r *recorder) Health(bool, sdk.ReadError) error { return nil }

func (r *recorder) count() (samples, answered int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.samples), len(r.answered)
}

const serviceScenario = `{"tick":"1s","initial":"idle",
"states":[{"name":"idle","transitions":{"running":0.5}},{"name":"running","producing":true,"transitions":{"idle":0.1}}],
"cycle_time":{"mean":"5s","stddev":"1s"},"scrap_rate":0.02,
"measurements":[{"signal":"press_force","mean":182,"stddev":3.5,"unit":"kN"}]}`

func serviceSection(connection, points string) json.RawMessage {
	return json.RawMessage(`{"id":"sim","driver":"plugin:laraedge-builtin-simulated","connection":` + connection +
		`,"devices":[{"device":"press-1","points":[` + points + `]}]}`)
}

const (
	serviceConnection = `{"seed":3,"scenario":` + serviceScenario + `}`
	servicePoints     = `{"signal":"state","address":"state","type":"string","read":"subscribe"},` +
		`{"signal":"force","address":"press_force","type":"float64","read":"subscribe","priority":"bulk"}`
)

func TestSimulatedServiceConfigure(t *testing.T) {
	tests := []struct {
		name    string
		section json.RawMessage
		want    string // a path-and-message fragment of the first refusal; empty: accepted
	}{
		{"valid", serviceSection(serviceConnection, servicePoints), ""},
		{"no scenario", serviceSection(`{"seed":3}`, servicePoints), "exactly one of scenario and scenario_file"},
		{"unknown connection key", serviceSection(`{"scenario":`+serviceScenario+`,"color":"red"}`, servicePoints), "unknown key color"},
		{"a relative scenario file has no base directory", serviceSection(`{"scenario_file":"scenario.yaml"}`, servicePoints), "absolute path"},
		{"unknown address", serviceSection(serviceConnection, `{"signal":"x","address":"nope","type":"float64","read":"subscribe"}`), "offers no signal"},
		{"state needs a string", serviceSection(serviceConnection, `{"signal":"state","address":"state","type":"float64","read":"subscribe"}`), "must be string"},
		{"bad priority", serviceSection(serviceConnection, `{"signal":"state","address":"state","type":"string","read":"subscribe","priority":"urgent"}`), "devices[0].points[0].priority: must be critical or bulk"},
		{"not an object", json.RawMessage(`[]`), "object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newSimulatedService(clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

			errs := svc.Configure(context.Background(), "sim", tc.section)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("Configure refused: %v", errs)
				}

				return
			}
			if len(errs) == 0 {
				t.Fatal("Configure accepted a bad section")
			}
			got := errs[0].Path + ": " + errs[0].Message
			if !strings.Contains(got, tc.want) {
				t.Errorf("first refusal = %q, want it to contain %q", got, tc.want)
			}
			if _, err := svc.Probe(context.Background(), "sim"); err == nil {
				t.Error("an instance that was refused is configured")
			}
		})
	}
}

func TestSimulatedServiceProbeAndRun(t *testing.T) {
	start := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	svc := newSimulatedService(clock.NewFake(start))
	ctx, cancel := context.WithTimeout(context.Background(), longGuard)
	defer cancel()

	if errs := svc.Configure(ctx, "sim", serviceSection(serviceConnection, servicePoints)); len(errs) != 0 {
		t.Fatalf("Configure: %v", errs)
	}

	probed, err := svc.Probe(ctx, "sim")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(probed) != 2 {
		t.Fatalf("Probe returned %d samples, want 2", len(probed))
	}
	if s := probed[0]; s.Signal != "state" || s.Value != sdk.StringValue("idle") || s.Quality != sdk.QualityGood || s.Priority != sdk.PriorityCritical || !s.TS.Equal(start) {
		t.Errorf("probed state = %+v", s)
	}
	if s := probed[1]; s.Signal != "force" || s.Value.Kind != sdk.ValueKindFloat || s.Value.Float != 182 || s.Priority != sdk.PriorityBulk {
		t.Errorf("probed force = %+v", s)
	}

	// Run delivers the initial state at once, each sample followed by the
	// answer of its device, and ends cleanly when its context does.
	rec := newRecorder(-1)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- svc.Run(runCtx, "sim", rec) }()

	// The initial state is the one sample a simulation emits before its first
	// tick: a measurement comes with the first completed part.
	select {
	case <-rec.got:
	case err := <-done:
		t.Fatalf("Run ended early: %v", err)
	case <-ctx.Done():
		t.Fatal("Run emitted no initial samples")
	}
	stop()
	if err := <-done; err != nil {
		t.Errorf("Run after cancel = %v, want nil", err)
	}
	if samples, answered := rec.count(); samples < 1 || answered != samples {
		t.Errorf("Run emitted %d samples and %d answers, want one answer per sample", samples, answered)
	}
	if got := rec.samples[0]; got.Signal != "state" || got.Value != sdk.StringValue("idle") {
		t.Errorf("first sample = %+v, want the initial state", got)
	}

	// A stream that ends under the plugin ends the run with its error.
	failing := newRecorder(0)
	if err := svc.Run(ctx, "sim", failing); err == nil || !strings.Contains(err.Error(), "the stream ended") {
		t.Errorf("Run with a failing emitter = %v, want the emitter's error", err)
	}

	// Run and Probe on an unknown instance fail without panicking.
	if err := svc.Run(ctx, "other", rec); err == nil {
		t.Error("Run of an unknown instance succeeded")
	}
	if err := svc.Stop(ctx, "other"); err != nil {
		t.Errorf("Stop of an unknown instance = %v, want nil", err)
	}
}
