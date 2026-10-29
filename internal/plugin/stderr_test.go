package plugin

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
)

var stderrEpoch = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func newTestWriter(t *testing.T) (*lineWriter, *outputLimiter, *clock.Fake, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	clk := clock.NewFake(stderrEpoch)
	limiter := newOutputLimiter(clk, log)

	return newLineWriter("stderr", limiter, log), limiter, clk, &buf
}

func writeLines(t *testing.T, w *lineWriter, n int) {
	t.Helper()

	for range n {
		if _, err := w.Write([]byte("x\n")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLineWriterSplitsAndTruncates(t *testing.T) {
	long := strings.Repeat("a", maxOutputLine+100)
	tests := []struct {
		name   string
		writes []string
		flush  bool
		want   []string
	}{
		{name: "lines split across writes", writes: []string{"one\ntw", "o\r\nthr", "ee\n"}, want: []string{"line=one", "line=two", "line=three"}},
		{name: "unterminated line waits for flush", writes: []string{"tail"}, want: nil},
		{name: "flush logs the unterminated line", writes: []string{"tail"}, flush: true, want: []string{"line=tail"}},
		{name: "long line cut at 4 KiB", writes: []string{long + "\nnext\n"}, want: []string{
			"line=" + strings.Repeat("a", maxOutputLine) + " truncated=true", "line=next",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, _, _, buf := newTestWriter(t)
			for _, s := range tt.writes {
				n, err := w.Write([]byte(s))
				if n != len(s) || err != nil {
					t.Fatalf("Write() = %d, %v; want %d, nil", n, err, len(s))
				}
			}
			if tt.flush {
				w.flush()
			}

			var got []string
			for line := range strings.Lines(buf.String()) {
				_, attrs, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "stream=stderr ")
				got = append(got, attrs)
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("logged %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutputLimiterBurstAndRate(t *testing.T) {
	w, _, clk, buf := newTestWriter(t)

	writeLines(t, w, 250)
	if got := strings.Count(buf.String(), "plugin output"); got != outputBurst {
		t.Fatalf("logged %d lines of 250 at once, want the burst %d", got, outputBurst)
	}

	clk.Advance(time.Second)
	writeLines(t, w, 100)
	if got := strings.Count(buf.String(), "plugin output"); got != outputBurst+outputRate {
		t.Fatalf("logged %d lines after one more second, want %d", got, outputBurst+outputRate)
	}
	if strings.Contains(buf.String(), "dropped") {
		t.Fatal("dropped lines reported before a minute passed")
	}
}

func TestOutputLimiterReportsDropsOncePerMinute(t *testing.T) {
	w, limiter, clk, buf := newTestWriter(t)

	writeLines(t, w, outputBurst+30)
	clk.Advance(30 * time.Second)
	limiter.report(false)
	if strings.Contains(buf.String(), "dropped") {
		t.Fatal("dropped lines reported after 30 s")
	}

	clk.Advance(30 * time.Second)
	limiter.report(false)
	if got := strings.Count(buf.String(), "level=WARN msg=\"plugin output lines dropped by the rate limit\" dropped=30"); got != 1 {
		t.Fatalf("want one WARN with dropped=30, log:\n%s", buf.String())
	}

	clk.Advance(time.Minute)
	limiter.report(false)
	if got := strings.Count(buf.String(), "lines dropped"); got != 1 {
		t.Fatalf("a minute without drops logged %d WARNs, want still 1", got)
	}

	for range outputBurst + 5 {
		limiter.allow()
	}
	limiter.report(true)
	if !strings.Contains(buf.String(), "dropped=5") {
		t.Fatalf("the forced report did not count the last drops:\n%s", buf.String())
	}
}
