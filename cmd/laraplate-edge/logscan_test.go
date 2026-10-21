package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/simulated"
	"github.com/getlaraplate/laraedge/internal/logging"
	"github.com/getlaraplate/laraedge/internal/model"
)

const (
	scanToken = "scan-tok-0c4d-SECRET-VALUE"
	scanEnv   = "EDGE_SCAN_TOKEN"
)

// sampleValues records the string forms of every sample value an input
// emits.
type sampleValues struct {
	mu      sync.Mutex
	floats  map[string]bool
	strs    map[string]bool
	samples int
}

func (v *sampleValues) add(rec model.Record) {
	if rec.Sample == nil {
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	v.samples++
	val := rec.Sample.Value
	switch val.Kind {
	case model.KindFloat:
		v.floats[strconv.FormatFloat(val.Float, 'f', -1, 64)] = true
		v.floats[strconv.FormatFloat(val.Float, 'g', -1, 64)] = true
	case model.KindString:
		v.strs[val.Str] = true
	}
}

func (v *sampleValues) counts() (samples, floats, strs int) {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.samples, len(v.floats), len(v.strs)
}

// forms returns every recorded string form.
func (v *sampleValues) forms() []string {
	v.mu.Lock()
	defer v.mu.Unlock()

	var out []string
	for s := range v.floats {
		out = append(out, s)
	}
	for s := range v.strs {
		out = append(out, s)
	}

	return out
}

// recordingInput passes every record of a real input on and records its
// sample values.
type recordingInput struct {
	input.Input
	values *sampleValues
}

func (r recordingInput) Start(ctx context.Context, emit func(model.Record)) error {
	return r.Input.Start(ctx, func(rec model.Record) {
		r.values.add(rec)
		emit(rec)
	})
}

// scriptedIngest answers 401, then 422, then 202 to every request.
type scriptedIngest struct {
	srv   *httptest.Server
	mu    sync.Mutex
	codes []int
}

func newScriptedIngest(t *testing.T) *scriptedIngest {
	t.Helper()

	in := &scriptedIngest{}
	in.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		in.mu.Lock()
		code := http.StatusAccepted
		switch len(in.codes) {
		case 0:
			code = http.StatusUnauthorized
		case 1:
			code = http.StatusUnprocessableEntity
		}
		in.codes = append(in.codes, code)
		in.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		switch code {
		case http.StatusUnauthorized:
			_, _ = io.WriteString(w, `{"message":"Unauthenticated."}`)
		case http.StatusUnprocessableEntity:
			_, _ = io.WriteString(w, `{"message":"invalid","errors":{"devices.0.samples.0.value":["bad"]}}`)
		default:
			_, _ = io.WriteString(w, `{"status":"accepted"}`)
		}
	}))
	in.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	in.srv.Start()
	t.Cleanup(in.srv.Close)

	return in
}

func (in *scriptedIngest) requests() int {
	in.mu.Lock()
	defer in.mu.Unlock()

	return len(in.codes)
}

// scanScenario produces a part at almost every step, with measurements whose
// string forms cannot appear in a log line by chance, and state names that
// are just as distinctive.
const scanScenario = `
        tick: 1s
        initial: zq-producing
        states:
          - name: zq-producing
            producing: true
            transitions: {zq-halted: 0.2}
          - name: zq-halted
            transitions: {zq-producing: 0.5}
        cycle_time: {mean: 1s, stddev: 0s}
        scrap_rate: 0.3
        measurements:
          - {signal: press_force, mean: 182.371, stddev: 3.5}
          - {signal: die_temperature, mean: 78.913, stddev: 1.2}
`

// TestLogsHoldNoTokenAndNoSampleValue is the log scanner of spec 10.5: the
// agent runs the simulated input against a backend that refuses the token
// (401), then refuses a batch (422), then accepts; every log line, at every
// level, is scanned for the token, and every line at info and above for the
// sample values the input emitted. Integer counters are left out of the
// value scan: small integers appear in ordinary log lines (versions,
// statuses, counts), so their absence proves nothing; the float and string
// values travel the same code paths.
func TestLogsHoldNoTokenAndNoSampleValue(t *testing.T) {
	dir := t.TempDir()
	be := newScriptedIngest(t)
	configFile := filepath.Join(dir, "config.yaml")
	writeFile(t, configFile, `schema: laraplate-edge-config/1
version: 1
inputs:
  - id: press-line
    driver: simulated
    connection:
      seed: 7
      scenario:`+scanScenario+`
    devices:
      - device: press-07
        points: &points
          - {signal: state, address: state, type: string, read: subscribe, priority: critical}
          - {signal: good_count, address: good_count, type: uint32, read: subscribe, priority: critical}
          - {signal: scrap_count, address: scrap_count, type: uint32, read: subscribe, priority: critical}
          - {signal: press_force, address: press_force, type: float64, read: subscribe, priority: bulk}
          - {signal: die_temperature, address: die_temperature, type: float64, read: subscribe, priority: bulk}
      - device: press-08
        points: *points
outputs:
  - id: main
    type: laraplate-http
    url: `+be.srv.URL+`
    token: secret://env/`+scanEnv+`
    batch: {max_samples: 5}
`)
	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, bootstrapFile, bootstrapYAML("plant-a-edge-01", configFile, filepath.Join(dir, "data")))

	values := &sampleValues{floats: map[string]bool{}, strs: map[string]bool{}}
	realSimulated := simulated.Factory(dir)
	recording := func(cfg config.Input, clk clock.Clock) (input.Input, error) {
		in, err := realSimulated(cfg, clk)
		if err != nil {
			return nil, err
		}

		return recordingInput{Input: in, values: values}, nil
	}

	logs := &lockedBuffer{}
	clk := clock.NewFake(time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC))
	a := startAgent(t, bootstrapFile, agentDeps{
		lookupEnv: (&fakeEnv{vars: map[string]string{scanEnv: scanToken}}).lookup,
		clock:     clk,
		drivers:   map[string]input.Factory{config.DriverSimulated: recording},
		logger:    logging.New(logs, slog.LevelDebug),
	})
	waitFor(t, "configuration applied", func() bool { return a.sup.Status().Version == 1 })

	// The 401 suspends the output for five minutes, then the 422 drops a
	// batch, then the backend accepts. The clock moves in steps the
	// simulator replays.
	deadline := time.Now().Add(guard)
	for {
		samples, floats, strs := values.counts()
		if be.requests() >= 6 && samples >= 50 && floats >= 20 && strs >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not enough traffic: %d requests, %d samples, %d float and %d string values", be.requests(), samples, floats, strs)
		}
		clk.Advance(5 * time.Second)
		runtime.Gosched()
	}
	a.stop(t)

	for _, want := range []string{`"status":401`, "rejected_invalid"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the logs do not show the %s path:\n%s", want, logs.String())
		}
	}

	forms := values.forms()
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lines := 0
	for sc.Scan() {
		line := sc.Text()
		lines++
		if strings.Contains(line, scanToken) {
			t.Errorf("a log line holds the token: %s", line)
		}

		var rec struct {
			Level string `json:"level"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not JSON: %s", line)
		}
		if rec.Level == "DEBUG" {
			continue
		}
		for _, form := range forms {
			if strings.Contains(line, form) {
				t.Errorf("a %s log line holds the sample value %s: %s", rec.Level, form, line)
			}
		}
	}
	if lines == 0 {
		t.Fatal("no log line captured")
	}
}
