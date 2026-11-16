package lograte_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/lograte"
)

var epoch = time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)

const (
	window  = time.Minute
	summary = "repeated since the last report"
)

// lockedBuffer is a bytes.Buffer safe for the concurrent test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}

func newReporter(logs *lockedBuffer) *lograte.Reporter {
	h := slog.NewJSONHandler(logs, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}

		return a
	}})

	return lograte.New(slog.New(h), window, "x: "+summary)
}

func TestFirstFailureOfAKindIsLoggedInFullAndTheRestCounted(t *testing.T) {
	logs := &lockedBuffer{}
	r := newReporter(logs)

	for range 100 {
		r.Report(epoch, "a.write", slog.LevelWarn, "x: failed", "device", "press-07")
	}

	out := logs.String()
	if got := strings.Count(out, "x: failed"); got != 1 {
		t.Fatalf("%d full lines for 100 failures, want 1:\n%s", got, out)
	}
	if strings.Contains(out, summary) {
		t.Fatalf("a summary before the window ended:\n%s", out)
	}
}

func TestAnotherKindInTheSameEpisodeIsLoggedInFull(t *testing.T) {
	logs := &lockedBuffer{}
	r := newReporter(logs)

	r.Report(epoch, "a.write", slog.LevelWarn, "x: first")
	r.Report(epoch, "b.drop", slog.LevelWarn, "x: second")
	r.Report(epoch, "a.write", slog.LevelWarn, "x: first")

	out := logs.String()
	if strings.Count(out, "x: first") != 1 || strings.Count(out, "x: second") != 1 {
		t.Fatalf("want each kind in full once:\n%s", out)
	}
}

func TestFlushAfterTheWindowLogsOneSummaryWithTheCounts(t *testing.T) {
	logs := &lockedBuffer{}
	r := newReporter(logs)
	for range 4 {
		r.Report(epoch, "a.write", slog.LevelWarn, "x: failed")
	}
	r.Report(epoch, "b.drop", slog.LevelError, "x: dropped")
	r.Report(epoch, "b.drop", slog.LevelError, "x: dropped")

	r.Flush(epoch.Add(window - time.Second))
	if strings.Contains(logs.String(), summary) {
		t.Fatalf("a summary before the window:\n%s", logs.String())
	}

	r.Flush(epoch.Add(window))
	r.Flush(epoch.Add(window))

	out := logs.String()
	if got := strings.Count(out, summary); got != 1 {
		t.Fatalf("%d summaries, want 1:\n%s", got, out)
	}
	for _, want := range []string{`"level":"ERROR"`, `"suppressed":4`, `"a.write":3`, `"b.drop":1`, `"window_seconds":60`} {
		if !strings.Contains(out, want) {
			t.Errorf("summary %q does not contain %s", out, want)
		}
	}
}

func TestAQuietWindowEndsTheEpisode(t *testing.T) {
	logs := &lockedBuffer{}
	r := newReporter(logs)
	r.Report(epoch, "a.write", slog.LevelWarn, "x: failed")

	r.Flush(epoch.Add(window))
	r.Report(epoch.Add(window+time.Second), "a.write", slog.LevelWarn, "x: failed")

	out := logs.String()
	if strings.Contains(out, summary) {
		t.Fatalf("a summary for an episode with nothing counted:\n%s", out)
	}
	if got := strings.Count(out, "x: failed"); got != 2 {
		t.Fatalf("%d full lines, want 2 (the episode ended in between):\n%s", got, out)
	}
}

func TestConcurrentReports(t *testing.T) {
	logs := &lockedBuffer{}
	r := newReporter(logs)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 200 {
				r.Report(epoch.Add(time.Duration(i)*time.Millisecond), "a.write", slog.LevelWarn, "x: failed")
				r.Flush(epoch.Add(time.Duration(i) * time.Millisecond))
			}
		})
	}
	wg.Wait()
	r.Flush(epoch.Add(window))

	out := logs.String()
	if got := strings.Count(out, "x: failed"); got != 1 {
		t.Fatalf("%d full lines under concurrency, want 1", got)
	}
	if !strings.Contains(out, `"suppressed":1599`) {
		t.Fatalf("the summary does not count the 1599 suppressed failures:\n%s", out)
	}
}
