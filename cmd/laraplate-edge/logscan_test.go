package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
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
	ints    map[string]bool
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
	case model.KindInt:
		// Only an integer of three digits or more can stand for a value: a
		// small one appears in ordinary log lines (versions, statuses,
		// counts), so its absence proves nothing.
		if str := strconv.FormatInt(val.Int, 10); len(strings.TrimPrefix(str, "-")) >= minIntDigits {
			v.ints[str] = true
		}
	case model.KindString:
		v.strs[val.Str] = true
	}
}

// minIntDigits is the shortest integer sample value the log scan looks for.
const minIntDigits = 3

func (v *sampleValues) counts() (samples, floats, ints, strs int) {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.samples, len(v.floats), len(v.ints), len(v.strs)
}

// intForms returns the recorded integer values.
func (v *sampleValues) intForms() []string {
	v.mu.Lock()
	defer v.mu.Unlock()

	return slices.Collect(maps.Keys(v.ints))
}

// holdsNumber reports whether line holds the integer form as a number of its
// own, not as the digits of a longer one.
func holdsNumber(line, form string) bool {
	for i := 0; ; {
		at := strings.Index(line[i:], form)
		if at < 0 {
			return false
		}
		at += i
		end := at + len(form)
		before := at == 0 || !isNumberByte(line[at-1])
		after := end == len(line) || !isNumberByte(line[end])
		if before && after {
			return true
		}
		i = at + 1
	}
}

// countingAttributes are the numeric log attributes the agent writes: counts,
// sequence numbers, HTTP statuses and versions. They say how many or which,
// never what a machine measured, and they grow through the same range as a
// counter sample (a batch seq of 401, a status of 401), so scanning them would
// fail the test by coincidence, as a timestamp did in the router test. The
// scan drops them from the line, and every other numeric attribute is scanned:
// a new one that collides fails the test and has to be named here on purpose.
var countingAttributes = []string{
	"time", "level", "records", "seq", "started", "stopped", "unchanged", "version", "status",
	"suppressed", "window_seconds", "attempt", "attempts",
}

// withoutCountingAttributes returns the JSON line without the countingAttributes.
func withoutCountingAttributes(t *testing.T, line string) string {
	t.Helper()

	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("a log line is not a JSON object: %s", line)
	}
	for _, key := range countingAttributes {
		if _, isNumber := fields[key].(float64); isNumber || key == "time" || key == "level" {
			delete(fields, key)
		}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	return string(out)
}

func isNumberByte(b byte) bool { return b >= '0' && b <= '9' || b == '.' }

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
// sample values the input emitted. Integers are not scanned as substrings;
// those of at least minIntDigits digits are scanned as whole
// numbers, because small ones appear in ordinary log lines (versions,
// statuses, counts) and their absence would prove nothing.
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

	values := &sampleValues{floats: map[string]bool{}, ints: map[string]bool{}, strs: map[string]bool{}}
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
		samples, floats, ints, strs := values.counts()
		if be.requests() >= 6 && samples >= 50 && floats >= 20 && ints >= 5 && strs >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not enough traffic: %d requests, %d samples, %d float, %d integer and %d string values", be.requests(), samples, floats, ints, strs)
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
	ints := values.intForms()
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
		scanned := withoutCountingAttributes(t, line)
		for _, form := range ints {
			if holdsNumber(scanned, form) {
				t.Errorf("a %s log line holds the integer sample value %s: %s", rec.Level, form, line)
			}
		}
	}
	if lines == 0 {
		t.Fatal("no log line captured")
	}
}

func TestHoldsNumberMatchesWholeNumbersOnly(t *testing.T) {
	for _, tt := range []struct {
		line string
		want bool
	}{
		{`{"value":182}`, true},
		{`value=182`, true},
		{`182`, true},
		{`{"samples":1182}`, false},
		{`{"samples":1821}`, false},
		{`{"offset":0.182}`, false},
		{`{"offset":182.5}`, false},
		{`{"a":1821,"b":182}`, true},
		{`no digits`, false},
	} {
		if got := holdsNumber(tt.line, "182"); got != tt.want {
			t.Errorf("holdsNumber(%q, 182) = %v, want %v", tt.line, got, tt.want)
		}
	}
}
