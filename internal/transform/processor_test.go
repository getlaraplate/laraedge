package transform_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/atomicfile"
	"github.com/getlaraplate/laraplate-edge/internal/clock"
	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/transform"
)

const inputID = "line1"

// point describes one configured point of the test input.
type point struct {
	device, signal, typ string
	steps               []config.Transform
}

// inputOf builds an input whose devices hold the points, in order.
func inputOf(points ...point) config.Input {
	in := config.Input{ID: inputID, Driver: "simulated"}

	for _, p := range points {
		idx := -1
		for i, d := range in.Devices {
			if d.Device == p.device {
				idx = i
			}
		}

		if idx < 0 {
			in.Devices = append(in.Devices, config.Device{Device: p.device})
			idx = len(in.Devices) - 1
		}

		in.Devices[idx].Points = append(in.Devices[idx].Points, config.Point{
			Signal: p.signal, Type: p.typ, Priority: model.Critical, Transforms: p.steps,
		})
	}

	return in
}

// recorder is a downstream emit. It flags any record that arrives after the
// test marked the processor closed.
type recorder struct {
	mu      sync.Mutex
	records []model.Record
	closed  atomic.Bool
	late    atomic.Int32
}

func (r *recorder) emit(rec model.Record) {
	if r.closed.Load() {
		r.late.Add(1)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.records = append(r.records, rec)
}

func (r *recorder) all() []model.Record {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]model.Record(nil), r.records...)
}

func (r *recorder) samples(t *testing.T) []model.Sample {
	t.Helper()

	var out []model.Sample

	for _, rec := range r.all() {
		if rec.Sample == nil {
			t.Fatalf("record %+v is not a sample", rec)
		}

		out = append(out, *rec.Sample)
	}

	return out
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.records)
}

// waitUntil yields until cond holds. The wall-clock guard only stops a
// broken test; no assertion depends on timing.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()

	guard := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(guard) {
			t.Fatalf("%s not reached", what)
		}

		runtime.Gosched()
	}
}

// receive waits for ch to be closed, with the same guard as waitUntil.
func receive(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s not reached", what)
	}
}

// waitArmed waits until the processor's goroutine waits for the deadline at.
func waitArmed(t *testing.T, p *transform.Processor, at time.Time) {
	t.Helper()

	waitUntil(t, "deadline armed at +"+at.Sub(t0).String(), func() bool {
		d, ok := transform.ArmedDeadline(p)
		return ok && d.Equal(at)
	})
}

// logSink captures the processor's log as JSON lines.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

func (l *logSink) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (l *logSink) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.String()
}

func (l *logSink) entries(t *testing.T) []map[string]any {
	t.Helper()

	var out []map[string]any

	for _, line := range strings.Split(strings.TrimSpace(l.text()), "\n") {
		if line == "" {
			continue
		}

		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}

		out = append(out, m)
	}

	return out
}

func newProcessor(t *testing.T, in config.Input, dir string, clk clock.Clock, log *slog.Logger) *transform.Processor {
	t.Helper()

	p, err := transform.NewProcessor(in, dir, clk, log)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	t.Cleanup(p.Close)

	return p
}

func sampleOf(device, signal string, at time.Duration, v model.Value) model.Record {
	return model.SampleRecord(model.Sample{
		Device: device, Signal: signal, TS: t0.Add(at),
		Value: v, Quality: model.Good, Priority: model.Critical,
	})
}

func TestProcessorWrap(t *testing.T) {
	in := inputOf(
		point{device: "press1", signal: "temp", typ: "int16", steps: []config.Transform{step("scale", "0.1")}},
		point{device: "press1", signal: "raw", typ: "int16"},
	)

	birth := model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: "press1", TS: t0,
		Signals: []model.SignalInfo{{Signal: "temp", DataType: model.DataTypeNumber}}})

	tests := []struct {
		name string
		in   model.Record
		want model.Record
	}{
		{
			name: "a chain is applied",
			in:   sampleOf("press1", "temp", 0, model.IntValue(215)),
			want: sampleOf("press1", "temp", 0, model.Value{Kind: model.KindFloat, Float: 21.5}),
		},
		{
			name: "a point without a chain passes through",
			in:   sampleOf("press1", "raw", 0, model.IntValue(215)),
			want: sampleOf("press1", "raw", 0, model.IntValue(215)),
		},
		{
			name: "a sample of an unknown point passes through",
			in:   sampleOf("press2", "temp", 0, model.IntValue(215)),
			want: sampleOf("press2", "temp", 0, model.IntValue(215)),
		},
		{name: "a device event passes through", in: birth, want: birth},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newProcessor(t, in, t.TempDir(), clock.NewFake(t0), nil)
			rec := &recorder{}

			p.Wrap(rec.emit)(tc.in)

			got := rec.all()
			if len(got) != 1 {
				t.Fatalf("emitted %d records, want 1", len(got))
			}

			if tc.want.Event != nil {
				if got[0].Event == nil || got[0].Event.Device != tc.want.Event.Device || got[0].Event.Kind != tc.want.Event.Kind {
					t.Fatalf("emitted %+v, want the event %+v", got[0], tc.want)
				}

				return
			}

			if got[0].Sample == nil {
				t.Fatalf("emitted %+v, want a sample", got[0])
			}

			g, w := *got[0].Sample, *tc.want.Sample
			if g.Device != w.Device || g.Signal != w.Signal || !g.TS.Equal(w.TS) || g.Quality != w.Quality ||
				g.Priority != w.Priority || !sameValue(g.Value, w.Value) {
				t.Errorf("emitted %+v, want %+v", g, w)
			}

			if p.Errors() != 0 {
				t.Errorf("Errors() = %d, want 0", p.Errors())
			}
		})
	}
}

func TestProcessorRuntimeErrorIsABadSample(t *testing.T) {
	in := inputOf(point{device: "press1", signal: "code", typ: "uint16", steps: []config.Transform{step("bcd", "true")}})
	log := &logSink{}
	p := newProcessor(t, in, t.TempDir(), clock.NewFake(t0), log.logger())
	rec := &recorder{}

	// 0x7A7A has nibbles above nine: an invalid BCD value.
	p.Wrap(rec.emit)(sampleOf("press1", "code", 0, model.IntValue(0x7A7A)))

	got := rec.samples(t)
	if len(got) != 1 {
		t.Fatalf("emitted %d samples, want 1", len(got))
	}

	if got[0].Quality != model.Bad || got[0].Value != (model.Value{Kind: model.KindInt}) {
		t.Errorf("emitted %+v, want a bad sample with the integer zero value", got[0])
	}

	if p.Errors() != 1 {
		t.Errorf("Errors() = %d, want 1", p.Errors())
	}

	entries := log.entries(t)
	if len(entries) != 1 {
		t.Fatalf("log entries = %v, want one", entries)
	}

	e := entries[0]
	if e["level"] != "WARN" || e["input"] != inputID || e["device"] != "press1" || e["signal"] != "code" ||
		e["step"] != float64(1) || e["transform"] != "bcd" {
		t.Errorf("log entry %v lacks the input, device, signal, step or transform", e)
	}

	if text := log.text(); strings.Contains(text, "31354") || strings.Contains(text, "7a7a") {
		t.Errorf("log %q carries the sample value", text)
	}
}

func TestProcessorRuntimeErrorLogIsRateLimited(t *testing.T) {
	in := inputOf(
		point{device: "press1", signal: "code", typ: "uint16", steps: []config.Transform{step("bcd", "true")}},
		point{device: "press1", signal: "other", typ: "uint16", steps: []config.Transform{step("bcd", "true")}},
	)
	log := &logSink{}
	clk := clock.NewFake(t0)
	p, err := transform.NewProcessor(in, t.TempDir(), clk, log.logger())
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	rec := &recorder{}
	emit := p.Wrap(rec.emit)
	bad := func(signal string) { emit(sampleOf("press1", signal, 0, model.IntValue(0x1A))) }

	bad("code")
	bad("code")
	bad("code")
	bad("other")

	if n := len(log.entries(t)); n != 2 {
		t.Fatalf("log entries after the first errors = %d, want 2 (one per signal):\n%s", n, log.text())
	}

	// Errors keep coming: one summary once the minute has passed.
	clk.Advance(30 * time.Second)
	bad("code")

	if n := len(log.entries(t)); n != 2 {
		t.Fatalf("log entries within the minute = %d, want 2:\n%s", n, log.text())
	}

	clk.Advance(30 * time.Second)
	bad("code")

	entries := log.entries(t)
	if len(entries) != 3 {
		t.Fatalf("log entries after a minute = %d, want 3:\n%s", len(entries), log.text())
	}

	if s := entries[2]; s["level"] != "WARN" || s["input"] != inputID || s["suppressed"] != float64(3) {
		t.Errorf("summary %v, want WARN with the input and suppressed 3", s)
	}

	// Two quiet minutes end the episode: the error counted with the summary
	// is reported, and the next error is logged in full again.
	clk.Advance(2 * time.Minute)
	bad("code")

	entries = log.entries(t)
	if len(entries) != 5 {
		t.Fatalf("log entries after a quiet window = %d, want 5:\n%s", len(entries), log.text())
	}

	if s := entries[3]; s["suppressed"] != float64(1) {
		t.Errorf("summary %v, want suppressed 1", s)
	}

	if e := entries[4]; e["step"] != float64(1) || e["signal"] != "code" || e["suppressed"] != nil {
		t.Errorf("entry %v, want the error logged in full", e)
	}

	bad("code")
	p.Close()

	entries = log.entries(t)
	if len(entries) != 6 || entries[5]["suppressed"] != float64(1) {
		t.Errorf("log after Close:\n%s\nwant a last summary with suppressed 1", log.text())
	}

	if p.Errors() != 8 {
		t.Errorf("Errors() = %d, want 8", p.Errors())
	}
}

func TestProcessorHeartbeatFiresOnTheClock(t *testing.T) {
	in := inputOf(point{device: "press1", signal: "state", typ: "int16", steps: []config.Transform{step("heartbeat", `"10s"`)}})
	clk := clock.NewFake(t0)
	p := newProcessor(t, in, t.TempDir(), clk, nil)
	rec := &recorder{}
	emit := p.Wrap(rec.emit)

	p.Start(t.Context())

	emit(sampleOf("press1", "state", 0, model.IntValue(3)))
	waitArmed(t, p, t0.Add(10*time.Second))

	emit(sampleOf("press1", "state", 5*time.Second, model.IntValue(4)))
	waitArmed(t, p, t0.Add(15*time.Second))

	clk.Advance(10 * time.Second)
	clk.Advance(5 * time.Second)
	waitUntil(t, "the heartbeat", func() bool { return rec.count() >= 3 })

	got := rec.samples(t)
	if len(got) != 3 {
		t.Fatalf("emitted %+v, want the two samples and one heartbeat", got)
	}

	if !got[2].TS.Equal(t0.Add(15*time.Second)) || got[2].Value != model.IntValue(4) || got[2].Quality != model.Good {
		t.Errorf("heartbeat = %+v, want 4 resent at +15s", got[2])
	}

	waitArmed(t, p, t0.Add(25*time.Second))
}

func TestProcessorFireErrorIsCounted(t *testing.T) {
	in := inputOf(point{device: "press1", signal: "code", typ: "uint16",
		steps: []config.Transform{step("heartbeat", `"10s"`), step("bcd", "true")}})
	clk := clock.NewFake(t0)
	p := newProcessor(t, in, t.TempDir(), clk, (&logSink{}).logger())
	rec := &recorder{}
	emit := p.Wrap(rec.emit)

	p.Start(t.Context())

	emit(sampleOf("press1", "code", 0, model.IntValue(0x1A)))
	waitArmed(t, p, t0.Add(10*time.Second))

	clk.Advance(10 * time.Second)
	waitUntil(t, "the heartbeat", func() bool { return rec.count() == 2 })

	for i, s := range rec.samples(t) {
		if s.Quality != model.Bad || s.Value != (model.Value{Kind: model.KindInt}) {
			t.Errorf("sample %d = %+v, want bad with the integer zero value", i, s)
		}
	}

	if p.Errors() != 2 {
		t.Errorf("Errors() = %d, want 2", p.Errors())
	}
}

// TestProcessorEmitsNothingAfterClose is the Review Focus case: a heartbeat
// or min_interval deadline is due while the input closes.
func TestProcessorEmitsNothingAfterClose(t *testing.T) {
	tests := []struct {
		name  string
		steps []config.Transform
		feed  []time.Duration
		armed time.Duration
	}{
		{name: "heartbeat", steps: []config.Transform{step("heartbeat", `"10s"`)}, feed: []time.Duration{0}, armed: 10 * time.Second},
		{name: "min_interval", steps: []config.Transform{step("min_interval", `"10s"`)}, feed: []time.Duration{0, time.Second}, armed: 10 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := inputOf(point{device: "press1", signal: "state", typ: "int16", steps: tc.steps})
			clk := clock.NewFake(t0)
			p, err := transform.NewProcessor(in, t.TempDir(), clk, nil)
			if err != nil {
				t.Fatalf("NewProcessor: %v", err)
			}

			rec := &recorder{}
			emit := p.Wrap(rec.emit)

			p.Start(t.Context())

			for i, at := range tc.feed {
				emit(sampleOf("press1", "state", at, model.IntValue(int64(i))))
			}

			waitArmed(t, p, t0.Add(tc.armed))

			clk.Advance(tc.armed)
			p.Close()
			rec.closed.Store(true)

			emit(sampleOf("press1", "state", tc.armed, model.IntValue(9)))
			clk.Advance(time.Hour)

			if n := rec.late.Load(); n != 0 {
				t.Fatalf("%d records emitted after Close returned", n)
			}
		})
	}
}

// TestProcessorCloseWaitsForAnEmitInProgress closes the processor while the
// deadline goroutine is inside the downstream emit.
func TestProcessorCloseWaitsForAnEmitInProgress(t *testing.T) {
	in := inputOf(point{device: "press1", signal: "state", typ: "int16", steps: []config.Transform{step("heartbeat", `"10s"`)}})
	clk := clock.NewFake(t0)
	p, err := transform.NewProcessor(in, t.TempDir(), clk, nil)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	rec := &recorder{}
	entered := make(chan struct{})
	release := make(chan struct{})

	var calls atomic.Int32

	emit := p.Wrap(func(r model.Record) {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}

		rec.emit(r)
	})

	p.Start(t.Context())

	emit(sampleOf("press1", "state", 0, model.IntValue(1)))
	waitArmed(t, p, t0.Add(10*time.Second))
	clk.Advance(10 * time.Second)
	receive(t, "the heartbeat emit", entered)

	closed := make(chan struct{})

	go func() {
		p.Close()
		rec.closed.Store(true)
		close(closed)
	}()

	// The emit stays blocked until release: Close must not return meanwhile.
	for range 10000 {
		select {
		case <-closed:
			t.Fatal("Close returned while an emit was in progress")
		default:
		}

		runtime.Gosched()
	}

	close(release)
	receive(t, "Close", closed)

	clk.Advance(time.Hour)

	if n := rec.late.Load(); n != 0 {
		t.Fatalf("%d records emitted after Close returned", n)
	}

	if rec.count() != 2 {
		t.Errorf("emitted %d records, want the sample and the heartbeat", rec.count())
	}
}

func counterKey(device, signal string, position int, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%s/%s/%d/%s", device, signal, position, hex.EncodeToString(sum[:]))
}

func stateFilePath(dir string) string {
	return filepath.Join(dir, "inputs", inputID, "transform-state.json")
}

func readState(t *testing.T, dir string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(stateFilePath(dir))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("state %q: %v", data, err)
	}

	return doc
}

// pulse feeds edges to a boolean point and returns the counts emitted.
func pulse(t *testing.T, p *transform.Processor, signal string, levels ...bool) []int64 {
	t.Helper()

	rec := &recorder{}
	emit := p.Wrap(rec.emit)

	for i, v := range levels {
		emit(sampleOf("press1", signal, time.Duration(i)*time.Second, model.BoolValue(v)))
	}

	var counts []int64

	for _, s := range rec.samples(t) {
		if s.Quality != model.Good || s.Value.Kind != model.KindInt {
			t.Fatalf("edge_counter emitted %+v", s)
		}

		counts = append(counts, s.Value.Int)
	}

	return counts
}

func edgeCounter(signal, mode string) point {
	return point{device: "press1", signal: signal, typ: "bool", steps: []config.Transform{step("edge_counter", `"`+mode+`"`)}}
}

func TestProcessorEdgeCounterSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	in := inputOf(edgeCounter("door", "rising"))

	first := newProcessor(t, in, dir, clock.NewFake(t0), nil)
	if got := pulse(t, first, "door", false, true, false, true); fmt.Sprint(got) != "[0 1 2]" {
		t.Fatalf("first run counts = %v, want [0 1 2]", got)
	}

	first.Close()

	doc := readState(t, dir)
	key := counterKey("press1", "door", 1, `{"edge_counter":"rising"}`)
	if doc["version"] != float64(1) || fmt.Sprint(doc["counters"]) != fmt.Sprint(map[string]any{key: float64(2)}) {
		t.Fatalf("state = %v, want version 1 and %s: 2", doc, key)
	}

	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{
			filepath.Dir(stateFilePath(dir)): 0o700,
			stateFilePath(dir):               0o600,
		} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			if got := info.Mode().Perm(); got != want {
				t.Errorf("%s mode = %o, want %o", path, got, want)
			}
		}
	}

	second := newProcessor(t, in, dir, clock.NewFake(t0), nil)
	if got := pulse(t, second, "door", false, true); fmt.Sprint(got) != "[2 3]" {
		t.Fatalf("after restart counts = %v, want [2 3]", got)
	}
}

// TestProcessorChangedChainKeepsOtherPointsState is the Review Focus case
// for P2.
func TestProcessorChangedChainKeepsOtherPointsState(t *testing.T) {
	dir := t.TempDir()

	first := newProcessor(t, inputOf(edgeCounter("door", "rising"), edgeCounter("gate", "rising")), dir, clock.NewFake(t0), nil)
	pulse(t, first, "door", false, true, false, true)
	pulse(t, first, "gate", false, true)
	first.Close()

	second := newProcessor(t, inputOf(edgeCounter("door", "rising"), edgeCounter("gate", "both")), dir, clock.NewFake(t0), nil)

	if got := pulse(t, second, "door", true); fmt.Sprint(got) != "[2]" {
		t.Errorf("unchanged point baseline = %v, want [2]", got)
	}

	if got := pulse(t, second, "gate", true, false); fmt.Sprint(got) != "[0 1]" {
		t.Errorf("changed point counts = %v, want [0 1] (a clean start)", got)
	}
}

// TestProcessorCorruptStateFile is the Review Focus case for P3.
func TestProcessorCorruptStateFile(t *testing.T) {
	key := counterKey("press1", "door", 1, `{"edge_counter":"rising"}`)

	tests := []struct {
		name    string
		content string
	}{
		{name: "truncated", content: `{"version":1,"counters":{"` + key + `":4`},
		{name: "empty", content: ""},
		{name: "not JSON", content: "SECRETCONTENT"},
		{name: "unknown version", content: `{"version":2,"counters":{"` + key + `":4}}`},
		{name: "no version", content: `{"counters":{"` + key + `":4}}`},
		{name: "counter not an integer", content: `{"version":1,"counters":{"` + key + `":4.5}}`},
		{name: "negative counter", content: `{"version":1,"counters":{"` + key + `":-4}}`},
		{name: "trailing data", content: `{"version":1,"counters":{"` + key + `":4}} {}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := stateFilePath(dir)

			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			log := &logSink{}
			clk := clock.NewFake(t0)
			p := newProcessor(t, inputOf(edgeCounter("door", "rising")), dir, clk, log.logger())

			aside := fmt.Sprintf("%s.corrupt-%d", path, t0.Unix())

			moved, err := os.ReadFile(aside)
			if err != nil || string(moved) != tc.content {
				t.Fatalf("renamed file = %q, %v; want the original content", moved, err)
			}

			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("state file still in place: %v", err)
			}

			entries := log.entries(t)
			if len(entries) != 1 || entries[0]["level"] != "ERROR" || entries[0]["input"] != inputID {
				t.Errorf("log = %s, want one ERROR with the input id", log.text())
			}

			if strings.Contains(log.text(), "SECRETCONTENT") {
				t.Errorf("log %q carries the file content", log.text())
			}

			if got := pulse(t, p, "door", false, true); fmt.Sprint(got) != "[0 1]" {
				t.Errorf("counts = %v, want [0 1] (counters from 0)", got)
			}
		})
	}
}

func TestProcessorStateEntryIsWrittenOnTheFirstSave(t *testing.T) {
	dir := t.TempDir()

	_, err := transform.NewProcessor(inputOf(point{device: "press1", signal: "door", typ: "bool",
		steps: []config.Transform{step("edge_counter", `"rising"`), step("trim", "true")}}), dir, clock.NewFake(t0), nil)
	if err == nil {
		t.Fatal("NewProcessor accepted a chain that does not type-check")
	}

	p := newProcessor(t, inputOf(edgeCounter("door", "rising"), edgeCounter("gate", "falling")), dir, clock.NewFake(t0), nil)

	pulse(t, p, "door", false)

	if _, err := os.Stat(stateFilePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("state file written before any count: %v", err)
	}

	pulse(t, p, "door", true)

	counters, _ := readState(t, dir)["counters"].(map[string]any)
	if len(counters) != 1 || counters[counterKey("press1", "door", 1, `{"edge_counter":"rising"}`)] != float64(1) {
		t.Errorf("counters = %v, want only the door's count", counters)
	}
}

func TestProcessorSaveErrorIsARuntimeError(t *testing.T) {
	dir := t.TempDir()

	// A directory where the temporary file of the atomic write goes makes
	// every Save fail.
	if err := os.MkdirAll(stateFilePath(dir)+atomicfile.TmpSuffix, 0o700); err != nil {
		t.Fatal(err)
	}

	log := &logSink{}
	p := newProcessor(t, inputOf(edgeCounter("door", "rising")), dir, clock.NewFake(t0), log.logger())
	rec := &recorder{}
	emit := p.Wrap(rec.emit)

	emit(sampleOf("press1", "door", 0, model.BoolValue(false)))
	emit(sampleOf("press1", "door", time.Second, model.BoolValue(true)))

	got := rec.samples(t)
	if len(got) != 2 || got[1].Quality != model.Bad || got[1].Value != (model.Value{Kind: model.KindInt}) {
		t.Fatalf("emitted %+v, want the baseline then a bad integer zero", got)
	}

	if p.Errors() != 1 {
		t.Errorf("Errors() = %d, want 1", p.Errors())
	}

	if strings.Contains(log.text(), dir) {
		t.Errorf("log %q names a path of the data directory", log.text())
	}
}

func TestProcessorWithoutDataDirDoesNotPersist(t *testing.T) {
	wd := t.TempDir()
	t.Chdir(wd)

	p := newProcessor(t, inputOf(edgeCounter("door", "rising")), "", clock.NewFake(t0), nil)
	if got := pulse(t, p, "door", false, true); fmt.Sprint(got) != "[0 1]" {
		t.Fatalf("counts = %v, want [0 1]", got)
	}

	if entries, err := os.ReadDir(wd); err != nil || len(entries) != 0 {
		t.Errorf("working directory holds %v, %v; want nothing", entries, err)
	}
}

func TestProcessorConcurrentUse(t *testing.T) {
	in := inputOf(point{device: "press1", signal: "state", typ: "int16", steps: []config.Transform{step("heartbeat", `"1s"`)}})
	clk := clock.NewFake(t0)
	p := newProcessor(t, in, t.TempDir(), clk, nil)
	rec := &recorder{}
	emit := p.Wrap(rec.emit)

	p.Start(t.Context())

	const perWriter = 50

	var wg sync.WaitGroup

	for w := range 4 {
		wg.Go(func() {
			for i := range perWriter {
				emit(sampleOf("press1", "state", time.Duration(i)*ms, model.IntValue(int64(w*perWriter+i))))
			}
		})
	}

	wg.Go(func() {
		for range 20 {
			clk.Advance(500 * ms)
		}
	})

	wg.Wait()
	p.Close()

	if n := rec.count(); n < 4*perWriter {
		t.Errorf("emitted %d records, want at least %d", n, 4*perWriter)
	}
}

// startedProcessor builds and starts a processor, as the supervisor does.
func startedProcessor(t *testing.T, in config.Input, dir string) *transform.Processor {
	t.Helper()

	p := newProcessor(t, in, dir, clock.NewFake(t0), nil)
	p.Start(t.Context())

	return p
}

// TestProcessorRevertedStepStartsClean is ruling R11: a key no chain asks
// for is dropped, so a changed then reverted step never resumes an older,
// higher count.
func TestProcessorRevertedStepStartsClean(t *testing.T) {
	tests := []struct {
		name string
		// middle is the point list of the intermediate document.
		middle []point
		// countInMiddle pulses the intermediate document's door before Close.
		countInMiddle bool
	}{
		{name: "rising, both, rising", middle: []point{edgeCounter("door", "both")}, countInMiddle: true},
		{name: "rising, both without a count, rising across a restart", middle: []point{edgeCounter("door", "both")}},
		{name: "point removed and re-added", middle: []point{edgeCounter("gate", "rising")}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			rising := inputOf(edgeCounter("door", "rising"))

			first := startedProcessor(t, rising, dir)
			if got := pulse(t, first, "door", false, true, false, true, false, true); fmt.Sprint(got) != "[0 1 2 3]" {
				t.Fatalf("first counts = %v", got)
			}

			first.Close()

			middle := startedProcessor(t, inputOf(tc.middle...), dir)
			if tc.countInMiddle {
				if got := pulse(t, middle, "door", false, true); fmt.Sprint(got) != "[0 1]" {
					t.Fatalf("changed step counts = %v, want [0 1]", got)
				}
			}

			middle.Close()

			last := startedProcessor(t, rising, dir)
			if got := pulse(t, last, "door", true); fmt.Sprint(got) != "[0]" {
				t.Errorf("reverted step baseline = %v, want [0]", got)
			}
		})
	}
}

func TestProcessorStartWritesNothingWithoutPrunedKeys(t *testing.T) {
	dir := t.TempDir()

	startedProcessor(t, inputOf(edgeCounter("door", "rising")), dir).Close()

	if _, err := os.Stat(stateFilePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("Start wrote a state file with nothing to prune: %v", err)
	}
}

// deathOf is the death of device at t0+at.
func deathOf(device string, at time.Duration) model.Record {
	return model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: device, TS: t0.Add(at)})
}

// describe renders records as "<offset>=<value>" for samples and "death" for
// a death, the offset from t0.
func describe(records []model.Record) string {
	var parts []string

	for _, r := range records {
		if r.Event != nil {
			parts = append(parts, r.Event.Kind.String())

			continue
		}

		parts = append(parts, fmt.Sprintf("%s=%d", r.Sample.TS.Sub(t0), r.Sample.Value.Int))
	}

	return strings.Join(parts, ",")
}

// TestProcessorHeartbeatStopsAtTheDeviceDeath is ruling R14: after the
// death of its device a heartbeat resends nothing, so the backend sees the
// device unreachable, until a new sample leaves the step.
func TestProcessorHeartbeatStopsAtTheDeviceDeath(t *testing.T) {
	in := inputOf(
		point{device: "press1", signal: "state", typ: "int16", steps: []config.Transform{step("heartbeat", `"10s"`)}},
		point{device: "press2", signal: "state", typ: "int16", steps: []config.Transform{step("heartbeat", `"60s"`)}},
	)
	clk := clock.NewFake(t0)
	p := newProcessor(t, in, t.TempDir(), clk, nil)
	rec := &recorder{}
	emit := p.Wrap(rec.emit)

	p.Start(t.Context())

	emit(sampleOf("press1", "state", 0, model.IntValue(3)))
	emit(sampleOf("press2", "state", 0, model.IntValue(7)))
	waitArmed(t, p, t0.Add(10*time.Second))

	// The death of press1 disarms its heartbeat only: the goroutine now waits
	// on the heartbeat of press2.
	emit(deathOf("press1", time.Second))
	waitArmed(t, p, t0.Add(60*time.Second))

	clk.Advance(30 * time.Second)

	emit(sampleOf("press1", "state", 40*time.Second, model.IntValue(4)))
	waitArmed(t, p, t0.Add(50*time.Second))

	if got, want := describe(rec.all()), "0s=3,0s=7,death,40s=4"; got != want {
		t.Fatalf("emitted %s, want %s: no heartbeat of press1 between its death and its next sample", got, want)
	}
}

// TestProcessorDeviceDeathReleasesAndCancels is ruling R14 for the steps
// that hold a sample: min_interval sends its held sample before the death,
// debounce drops its waiting value.
func TestProcessorDeviceDeathReleasesAndCancels(t *testing.T) {
	tests := []struct {
		name  string
		steps []config.Transform
		feed  []model.Record
		want  string
	}{
		{
			name:  "min_interval sends the held sample before the death",
			steps: []config.Transform{step("min_interval", `"10s"`)},
			feed: []model.Record{
				sampleOf("press1", "state", 0, model.IntValue(1)),
				sampleOf("press1", "state", 2*time.Second, model.IntValue(2)),
				deathOf("press1", 3*time.Second),
				sampleOf("press1", "state", 4*time.Second, model.IntValue(3)),
			},
			// The window closed with the death: the first sample after it passes at once.
			want: "0s=1,2s=2,death,4s=3",
		},
		{
			name:  "debounce drops the waiting value",
			steps: []config.Transform{step("debounce", `"10s"`)},
			feed: []model.Record{
				sampleOf("press1", "state", 0, model.IntValue(1)),
				deathOf("press1", time.Second),
				sampleOf("press1", "state", 15*time.Second, model.IntValue(1)),
			},
			// Without the death, the sample at 15s would send 1 waiting since 0s.
			want: "death",
		},
		{
			name:  "the death of another device changes nothing",
			steps: []config.Transform{step("min_interval", `"10s"`)},
			feed: []model.Record{
				sampleOf("press1", "state", 0, model.IntValue(1)),
				sampleOf("press1", "state", 2*time.Second, model.IntValue(2)),
				deathOf("press2", 3*time.Second),
			},
			want: "0s=1,death",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := inputOf(point{device: "press1", signal: "state", typ: "int16", steps: tc.steps})
			p := newProcessor(t, in, t.TempDir(), clock.NewFake(t0), nil)
			rec := &recorder{}
			emit := p.Wrap(rec.emit)

			for _, r := range tc.feed {
				emit(r)
			}

			if got := describe(rec.all()); got != tc.want {
				t.Errorf("emitted %s, want %s", got, tc.want)
			}
		})
	}
}

// TestProcessorUnchangedValueIsSentAfterTheDeviceDeath is ruling R16: after
// a death the backend shows the device Offline, and a state sample equal to
// the state it had is a no-op there. A deadband or debounce before a
// heartbeat therefore forgets its last sent value at the death: the first
// value after it is sent even when unchanged, and the heartbeat re-arms.
func TestProcessorUnchangedValueIsSentAfterTheDeviceDeath(t *testing.T) {
	start := func(t *testing.T, first config.Transform) (*transform.Processor, *clock.Fake, *recorder, func(model.Record)) {
		t.Helper()

		in := inputOf(point{device: "press1", signal: "state", typ: "int16", steps: []config.Transform{first, step("heartbeat", `"10s"`)}})
		clk := clock.NewFake(t0)
		p := newProcessor(t, in, t.TempDir(), clk, nil)
		rec := &recorder{}
		emit := p.Wrap(rec.emit)

		p.Start(t.Context())

		return p, clk, rec, emit
	}

	t.Run("deadband sends it at once", func(t *testing.T) {
		p, clk, rec, emit := start(t, step("deadband", "1"))

		emit(sampleOf("press1", "state", 0, model.IntValue(3)))
		waitArmed(t, p, t0.Add(10*time.Second))

		emit(deathOf("press1", 3*time.Second))
		emit(sampleOf("press1", "state", 5*time.Second, model.IntValue(3)))

		// The heartbeat re-armed on the value sent at 5s.
		waitArmed(t, p, t0.Add(15*time.Second))
		clk.Advance(15 * time.Second)
		waitUntil(t, "the heartbeat", func() bool { return rec.count() >= 4 })

		if got, want := describe(rec.all()), "0s=3,death,5s=3,15s=3"; got != want {
			t.Errorf("emitted %s, want %s", got, want)
		}
	})

	t.Run("debounce sends it once stable for the delay", func(t *testing.T) {
		p, clk, rec, emit := start(t, step("debounce", `"2s"`))

		emit(sampleOf("press1", "state", 0, model.IntValue(3)))
		waitArmed(t, p, t0.Add(2*time.Second))
		clk.Advance(2 * time.Second)
		waitArmed(t, p, t0.Add(12*time.Second))

		emit(deathOf("press1", 3*time.Second))
		emit(sampleOf("press1", "state", 5*time.Second, model.IntValue(3)))

		// The unchanged value waits for the delay again, then leaves with
		// the ts of its change; the heartbeat re-arms from the clock time.
		waitArmed(t, p, t0.Add(7*time.Second))
		clk.Advance(5 * time.Second)
		waitArmed(t, p, t0.Add(17*time.Second))
		clk.Advance(10 * time.Second)
		waitUntil(t, "the heartbeat", func() bool { return rec.count() >= 4 })

		if got, want := describe(rec.all()), "0s=3,death,5s=3,17s=3"; got != want {
			t.Errorf("emitted %s, want %s", got, want)
		}
	})
}

// TestProcessorErrorSummaryClosesTheEpisodeOnTheClock: the summary of the
// errors only counted is logged a quiet errorLogWindow after the last error,
// by the deadline goroutine, without waiting for another error or Close.
func TestProcessorErrorSummaryClosesTheEpisodeOnTheClock(t *testing.T) {
	in := inputOf(point{device: "press1", signal: "code", typ: "uint16", steps: []config.Transform{step("bcd", "true")}})
	log := &logSink{}
	clk := clock.NewFake(t0)
	p := newProcessor(t, in, t.TempDir(), clk, log.logger())
	emit := p.Wrap((&recorder{}).emit)

	p.Start(t.Context())

	emit(sampleOf("press1", "code", 0, model.IntValue(0x1A)))
	clk.Advance(10 * time.Second)
	emit(sampleOf("press1", "code", 10*time.Second, model.IntValue(0x1A)))
	emit(sampleOf("press1", "code", 10*time.Second, model.IntValue(0x1A)))

	if n := len(log.entries(t)); n != 1 {
		t.Fatalf("log entries = %d, want the first error only:\n%s", n, log.text())
	}

	waitArmed(t, p, t0.Add(70*time.Second))
	clk.Advance(time.Minute)
	waitUntil(t, "the summary", func() bool { return len(log.entries(t)) == 2 })

	if s := log.entries(t)[1]; s["suppressed"] != float64(2) {
		t.Fatalf("summary %v, want suppressed 2", s)
	}

	// The episode ended: the next error is logged in full, and no deadline is
	// armed while nothing is counted.
	waitUntil(t, "no deadline", func() bool { _, ok := transform.ArmedDeadline(p); return !ok })
	emit(sampleOf("press1", "code", 80*time.Second, model.IntValue(0x1A)))

	entries := log.entries(t)
	if len(entries) != 3 || entries[2]["step"] != float64(1) || entries[2]["suppressed"] != nil {
		t.Errorf("log:\n%s\nwant the error after the summary logged in full", log.text())
	}
}

// TestRemoveStaleInputStates is ruling R15's helper: it removes the
// directories of data_dir/inputs that keep refuses, and touches nothing
// else, not even what a symbolic link there points to.
func TestRemoveStaleInputStates(t *testing.T) {
	if removed, err := transform.RemoveStaleInputStates(t.TempDir(), func(string) bool { return false }); err != nil || removed != nil {
		t.Fatalf("RemoveStaleInputStates() without data_dir/inputs = %v, %v, want nothing", removed, err)
	}

	dataDir := t.TempDir()
	root := filepath.Join(dataDir, "inputs")
	outside := t.TempDir()

	for _, dir := range []string{filepath.Join(root, "kept"), filepath.Join(root, "old", "nested"), filepath.Join(root, "older")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}

	removed, err := transform.RemoveStaleInputStates(dataDir, func(id string) bool { return id == "kept" })
	if err != nil || fmt.Sprint(removed) != "[old older]" {
		t.Fatalf("RemoveStaleInputStates() = %v, %v, want [old older]", removed, err)
	}

	for _, name := range []string{"kept", "notes.txt", "link"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s: %v, want it kept", name, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the link's target: %v, want it kept", err)
	}
}
