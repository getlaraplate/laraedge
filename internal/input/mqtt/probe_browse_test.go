package mqtt

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

// newCommandInput builds the input of cfg on a fake clock with a trace that
// feeds the returned channel.
func newCommandInput(t *testing.T, cfg config.Input, opts Options) (*Input, *clock.Fake, chan string) {
	t.Helper()

	clk := clock.NewFake(driverEpoch)
	in, err := New(cfg, clk, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events := make(chan string, 1024)
	in.trace = func(event string) {
		select {
		case events <- event:
		default:
		}
	}
	t.Cleanup(func() { _ = in.Close() })

	return in, clk, events
}

func waitFor(t *testing.T, events <-chan string, event string, n int) {
	t.Helper()

	bound := time.NewTimer(waitTimeout)
	defer bound.Stop()

	for n > 0 {
		select {
		case got := <-events:
			if got == event {
				n--
			}
		case <-bound.C:
			t.Fatalf("waiting for %d more %q events", n, event)
		}
	}
}

type probeResult struct {
	samples []model.Sample
	err     error
}

func TestProbeReturnsWhenEveryPointHasASample(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	b.Publish("plant/press/state", []byte(`{"count": 42, "mode": "auto"}`), true, 1)
	b.Publish("plant/press/temp", []byte("21.5"), true, 1)

	cfg := mqttInput(t, "line", b, nil, device("press",
		subscribePoint("temp", "plant/press/temp", "float64"),
		subscribePoint("count", "plant/press/state|count", "int32"),
		subscribePoint("mode", "plant/press/state|mode", "string"),
	))
	in, _, _ := newCommandInput(t, cfg, Options{})

	// No timeout shorter than waitTimeout: the probe must end by itself.
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()

	samples, err := in.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Probe waited for the context instead of returning at the last sample")
	}

	if len(samples) != 3 || samples[0].Signal != "temp" || samples[1].Signal != "count" || samples[2].Signal != "mode" {
		t.Fatalf("samples = %+v, want temp, count, mode in configuration order", samples)
	}
	if samples[0].Value.Float != 21.5 || samples[1].Value.Int != 42 || samples[2].Value.Str != "auto" {
		t.Errorf("values = %+v", samples)
	}
}

func TestProbeKeepsTheFirstSampleAndReturnsWhatItHasWhenTheContextEnds(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	b.Publish("plant/press/temp", []byte("21.5"), true, 1)

	cfg := mqttInput(t, "line", b, nil, device("press",
		subscribePoint("temp", "plant/press/temp", "float64"),
		subscribePoint("silent", "plant/press/silent", "float64"),
	))
	in, _, events := newCommandInput(t, cfg, Options{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan probeResult, 1)
	go func() {
		samples, err := in.Probe(ctx)
		done <- probeResult{samples, err}
	}()

	waitFor(t, events, traceProbeMessage, 1)
	// A later message on the same topic must not replace the first sample.
	b.Publish("plant/press/temp", []byte("99"), false, 1)
	waitFor(t, events, traceProbeMessage, 1)
	cancel()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Probe error on context end = %v, want none", r.err)
		}
		if len(r.samples) != 1 || r.samples[0].Signal != "temp" || r.samples[0].Value.Float != 21.5 {
			t.Fatalf("samples = %+v, want the first temp sample only", r.samples)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Probe did not return after the context ended")
	}
}

func TestProbeConnectionFailureIsAnError(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{Users: map[string]string{"edge": "right"}})

	conn := map[string]any{"auth": map[string]any{"user": "edge", "password": "secret://x"}}
	cfg := mqttInput(t, "line", b, conn, device("press", subscribePoint("temp", "plant/press/temp", "float64")))
	in, _, _ := newCommandInput(t, cfg, Options{Resolver: &fakeResolver{value: "wrong"}})

	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()

	samples, err := in.Probe(ctx)
	if err == nil || samples != nil {
		t.Fatalf("Probe = %v, %v, want an error", samples, err)
	}
	if strings.Contains(err.Error(), "wrong") || strings.Contains(err.Error(), "edge") {
		t.Errorf("error leaks a credential: %v", err)
	}
}

func TestBrowseListsTheTopicsSorted(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	b.Publish("plant/b/temp", []byte("21.5"), true, 1)
	b.Publish("plant/a/state", []byte(`{"count": 42, "mode": "auto", "nested": {"ok": true, "list": [1, null]}, "dotted.key": 1}`), true, 1)
	b.Publish("plant/c/bin", []byte("a\x00b\xffc\n"), true, 1)

	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("temp", "plant/b/temp", "float64")))
	in, clk, events := newCommandInput(t, cfg, Options{})

	type result struct {
		entries   []BrowseEntry
		truncated bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		e, tr, err := in.Browse(t.Context(), DefaultBrowseFilter, DefaultBrowseDuration)
		done <- result{e, tr, err}
	}()

	waitFor(t, events, traceBrowseMessage, 3)
	// A live message after the retained one: the count goes up, retained goes down.
	b.Publish("plant/b/temp", []byte("22"), false, 1)
	waitFor(t, events, traceBrowseMessage, 1)

	clk.BlockUntil(t, 1)
	clk.Advance(DefaultBrowseDuration)

	r := <-done
	if r.err != nil || r.truncated {
		t.Fatalf("Browse = %v, truncated %v", r.err, r.truncated)
	}

	if len(r.entries) != 3 || r.entries[0].Topic != "plant/a/state" || r.entries[1].Topic != "plant/b/temp" || r.entries[2].Topic != "plant/c/bin" {
		t.Fatalf("entries = %+v, want the three topics sorted", r.entries)
	}

	a := r.entries[0]
	want := []PathPreview{
		{"plant/a/state|count", "42"},
		{"plant/a/state|mode", `"auto"`},
		{"plant/a/state|nested.list.0", "1"},
		{"plant/a/state|nested.list.1", "null"},
		{"plant/a/state|nested.ok", "true"},
	}
	if len(a.Paths) != len(want) || !a.Retained || a.Messages != 1 || a.Text != "" {
		t.Fatalf("a = %+v", a)
	}
	for i := range want {
		if a.Paths[i] != want[i] {
			t.Errorf("path %d = %+v, want %+v", i, a.Paths[i], want[i])
		}
	}

	temp := r.entries[1]
	if temp.Messages != 2 || temp.Retained || temp.Size != 2 || temp.Text != "22" || len(temp.Paths) != 0 {
		t.Errorf("temp = %+v", temp)
	}

	if bin := r.entries[2]; bin.Text != `a\x00b\xffc\n` || bin.Size != 6 {
		t.Errorf("bin = %+v, want the escaped preview", bin)
	}
}

func TestBrowseStopsAfterTheTopicLimit(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	for i := range BrowseMaxTopics + 1 {
		b.Publish("t/x"+strconv.Itoa(i), []byte("1"), true, 0)
	}

	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("temp", "t/x0", "float64")))
	in, _, _ := newCommandInput(t, cfg, Options{})

	// The early stop ends the call without the clock: the timer never fires.
	entries, truncated, err := in.Browse(t.Context(), "t/#", DefaultBrowseDuration)
	if err != nil || !truncated || len(entries) != BrowseMaxTopics {
		t.Fatalf("Browse = %d entries, truncated %v, err %v", len(entries), truncated, err)
	}
}

func TestBrowseRefusesBadArguments(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("temp", "t", "float64")))
	in, _, _ := newCommandInput(t, cfg, Options{})

	for _, tt := range []struct {
		filter   string
		duration time.Duration
	}{
		{"", time.Second}, {"a/#/b", time.Second}, {"a/b+", time.Second}, {"#", 999 * time.Millisecond}, {"#", 6 * time.Minute},
	} {
		if _, _, err := in.Browse(t.Context(), tt.filter, tt.duration); err == nil {
			t.Errorf("Browse(%q, %v) succeeded", tt.filter, tt.duration)
		}
	}
}

func TestPreviewAndEscape(t *testing.T) {
	long := strings.Repeat("abcdefghij", 5)
	for _, tt := range []struct {
		in    string
		quote bool
		want  string
	}{
		{"hello", false, "hello"},
		{"hello", true, `"hello"`},
		{long, false, long[:37] + "..."},
		{strings.Repeat("a", 40), false, strings.Repeat("a", 40)},
		{strings.Repeat("a", 38), true, `"` + strings.Repeat("a", 38) + `"`},
		{strings.Repeat("a", 39), true, `"` + strings.Repeat("a", 35) + `..."`},
		{"é\t\"\\", false, `é\t\"\\`},
		{strings.Repeat("\x00", 20), false, `\x00\x00\x00\x00\x00\x00\x00\x00\x00...`},
	} {
		got := preview(tt.in, tt.quote)
		if got != tt.want {
			t.Errorf("preview(%q, %v) = %q, want %q", tt.in, tt.quote, got, tt.want)
		}
		if n := len([]rune(got)); n > PreviewMax {
			t.Errorf("preview(%q) is %d characters", tt.in, n)
		}
	}

	if got := Escape("a\x1b[0m\xff"); got != `a\x1b[0m\xff` {
		t.Errorf("Escape = %q", got)
	}
}

// The commands connect with their own client ids, so a running input with
// the same configuration keeps its connection: it never sees Lost, never
// dies and counts no reconnection.
func TestProbeAndBrowseLeaveARunningInputAlone(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	b.Publish("plant/press/temp", []byte("21.5"), true, 1)

	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("temp", "plant/press/temp", "float64")))
	h := newHarness(t, cfg, Options{DataDir: t.TempDir(), AgentID: "agent"})
	h.start()
	h.serving()
	h.waitSamples("temp", 1)
	topicsBefore, err := readTopics(h.in.opts.DataDir, "line")
	if err != nil {
		t.Fatal(err)
	}

	probe, _, _ := newCommandInput(t, cfg, Options{DataDir: h.in.opts.DataDir, AgentID: "agent"})
	samples, err := probe.Probe(testContext(t))
	if err != nil || len(samples) != 1 {
		t.Fatalf("Probe = %v, %v", samples, err)
	}

	browse, clk, events := newCommandInput(t, cfg, Options{DataDir: h.in.opts.DataDir, AgentID: "agent"})
	done := make(chan error, 1)
	go func() {
		_, _, err := browse.Browse(t.Context(), "#", time.Second)
		done <- err
	}()
	waitFor(t, events, traceBrowseMessage, 1)
	clk.BlockUntil(t, 1)
	clk.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Browse: %v", err)
	}

	// The running input saw nothing: still connected, no down event, no
	// connection error counted, no death event, and its topics file is intact.
	select {
	case ev := <-h.events:
		t.Fatalf("the running input traced %q", ev)
	default:
	}
	health := h.in.Health()
	if !health.Connected || health.ReadErrors["connection"] != 0 {
		t.Errorf("health = %+v", health)
	}
	if n := countEvents(h.snapshot(), model.Death, "press"); n != 0 {
		t.Errorf("the running input emitted %d death events", n)
	}
	if after, _ := readTopics(h.in.opts.DataDir, "line"); strings.Join(after, ",") != strings.Join(topicsBefore, ",") {
		t.Errorf("topics file changed: %v -> %v", topicsBefore, after)
	}
}
