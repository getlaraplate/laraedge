package buffer

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/clock"
	"github.com/getlaraplate/laraplate-edge/internal/model"
)

// logLines is a JSON log sink safe for the sync goroutine and the test.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

// entries returns the decoded log lines.
func (l *logLines) entries(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
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

func (l *logLines) messages(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range l.entries(t) {
		out = append(out, e["msg"].(string))
	}

	return out
}

func death(device string, ts time.Time) model.Record {
	return model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: device, TS: ts})
}

func birth(device string, ts time.Time) model.Record {
	return model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: device, TS: ts, Signals: []model.SignalInfo{{Signal: "temp", DataType: model.DataTypeNumber}}})
}

// TestDropPathLogsAreRateLimited: on a disk that refuses every write the
// drop path logs the first failure of each kind, then at most one summary a
// minute with the counts, by the clock; a quiet minute ends the episode.
func TestDropPathLogsAreRateLimited(t *testing.T) {
	clk := clock.NewFake(epoch)
	armed := make(chan struct{}, 16)
	synced := make(chan int, 16)
	logs := &logLines{}
	opts := testOptions(t, clk)
	opts.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	writer := &failingWriter{failures: 1 << 30}
	b := openBuffer(t, t.TempDir(), opts, hooks{
		write:  writer.write,
		armed:  func() { armed <- struct{}{} },
		synced: func(n int) { synced <- n },
	})
	<-armed

	appendDropped := func(n int) {
		t.Helper()
		for i := range n {
			if err := b.Append(crit(i)); !errors.Is(err, ErrDropped) {
				t.Fatalf("Append() on a refusing disk = %v, want ErrDropped", err)
			}
		}
	}

	tick := func(d time.Duration) {
		t.Helper()
		clk.Advance(d)
		<-synced
		<-armed
	}

	appendDropped(50)
	first := logs.messages(t)
	if len(first) != 2 {
		t.Fatalf("logged %d lines for 50 refused records, want 2 (the first failure of each kind): %v", len(first), first)
	}

	tick(30 * time.Second)
	if got := len(logs.messages(t)); got != 2 {
		t.Fatalf("a summary came before a minute passed: %d lines", got)
	}

	tick(31 * time.Second)
	entries := logs.entries(t)
	if len(entries) != 3 {
		t.Fatalf("logged %d lines a minute later, want one summary more: %v", len(entries), logs.messages(t))
	}

	summary := entries[2]
	if summary["suppressed"] != float64(2*49) || summary["critical.write_retry"] != float64(49) || summary["critical.write_failed"] != float64(49) {
		t.Fatalf("summary = %v, want 49 retries and 49 drops of the critical queue", summary)
	}

	tick(61 * time.Second)
	if got := len(logs.messages(t)); got != 3 {
		t.Fatalf("a quiet minute logged %d lines, want none", got-3)
	}

	appendDropped(1)
	if got := len(logs.messages(t)); got != 5 {
		t.Fatalf("the first failure after a quiet minute logged %d lines, want 2 in full", got-3)
	}
}

// TestLostDeviceEventsAreNotSamples: loss markers and the dropped counters
// count samples only. A lost device event is not a sample; a lost death is
// logged with its device; a drop of events alone leaves no marker.
func TestLostDeviceEventsAreNotSamples(t *testing.T) {
	t.Run("an event dropped on write", func(t *testing.T) {
		logs := &logLines{}
		opts := testOptions(t, clock.NewFake(epoch))
		opts.SegmentSize = 1 << 16
		opts.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		writer := &failingWriter{failures: 2}
		b := openBuffer(t, t.TempDir(), opts, hooks{write: writer.write})

		if err := b.Append(death("press-07", epoch)); !errors.Is(err, ErrDropped) {
			t.Fatalf("Append() = %v, want ErrDropped", err)
		}

		if pending := b.PendingLosses(); len(pending) != 0 {
			t.Fatalf("a dropped death left markers %+v, want none", pending)
		}

		if got := b.Stats().Dropped[model.LossBufferFull]; got != 0 {
			t.Fatalf("Dropped = %d samples, want 0", got)
		}

		assertDeathLogged(t, logs, "press-07")
	})

	t.Run("events in an evicted segment", func(t *testing.T) {
		tests := []struct {
			name    string
			records []model.Record
			want    []model.Loss
		}{
			{
				name:    "samples and events",
				records: []model.Record{death("press-07", epoch), crit(0), birth("press-07", epoch), crit(1)},
				want:    []model.Loss{loss(model.Critical, crit(0), crit(1), 2)},
			},
			{
				name:    "events only",
				records: []model.Record{death("press-07", epoch), birth("press-07", epoch)},
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				logs := &logLines{}
				opts := testOptions(t, clock.NewFake(epoch))
				opts.SegmentSize = 1 << 16
				opts.Logger = slog.New(slog.NewJSONHandler(logs, nil))
				b := openBuffer(t, t.TempDir(), opts, hooks{})
				mustAppend(t, b, tc.records...)

				b.mu.Lock()
				opts.MaxSize = b.critical.bytes() + 1
				b.opts.MaxSize = opts.MaxSize
				b.mu.Unlock()

				mustAppend(t, b, crit(2))
				assertLosses(t, b, tc.want)

				var samples uint64
				for _, l := range tc.want {
					samples += uint64(l.Samples)
				}

				if got := b.Stats().Dropped[model.LossBufferFull]; got != samples {
					t.Fatalf("Dropped = %d samples, want %d", got, samples)
				}

				assertDeathLogged(t, logs, "press-07")
			})
		}
	})
}

func assertDeathLogged(t *testing.T, logs *logLines, device string) {
	t.Helper()
	for _, e := range logs.entries(t) {
		if e["level"] == "WARN" && e["device"] == device && strings.Contains(e["msg"].(string), "death") {
			return
		}
	}

	t.Fatalf("no WARN line names the lost death of %s: %v", device, logs.messages(t))
}

// TestRecordsCountsWithoutReadingFrames: Records is the cheap count the
// batcher polls on every append; it never reads a frame.
func TestRecordsCountsWithoutReadingFrames(t *testing.T) {
	dir := t.TempDir()
	b := openBuffer(t, dir, testOptions(t, clock.NewFake(epoch)), hooks{})
	if n := b.Records(); n != 0 {
		t.Fatalf("Records() of an empty buffer = %d", n)
	}

	mustAppend(t, b, crit(0), bulk(0), bulk(1))
	if err := b.Ack(model.Bulk, readAll(t, b, model.Bulk)[0].Next); err != nil {
		t.Fatal(err)
	}

	if n := b.Records(); n != 2 {
		t.Fatalf("Records() = %d, want 2", n)
	}

	mustClose(t, b)
	if n := b.Records(); n != 2 {
		t.Fatalf("Records() after Close = %d, want the last count 2", n)
	}
}
