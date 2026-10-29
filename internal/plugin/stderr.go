package plugin

import (
	"bytes"
	"log/slog"
	"sync"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
)

const (
	// maxOutputLine is the longest plugin output line logged; the rest of a
	// longer line is dropped.
	maxOutputLine = 4096
	// outputRate and outputBurst are the token bucket of the output lines a
	// plugin may log (decision H7): 50 lines per second, bursts of 200.
	outputRate  = 50
	outputBurst = 200
	// dropReportEvery is the period of the WARN that counts dropped lines.
	dropReportEvery = time.Minute
)

// outputLimiter is the H7 rate limit of the output lines of one plugin,
// shared by its stdout and stderr and kept across its restarts. Lines over
// the limit are dropped and counted in one WARN per minute. It is safe for
// concurrent use.
type outputLimiter struct {
	clk clock.Clock
	log *slog.Logger

	mu         sync.Mutex
	tokens     float64
	refilled   time.Time
	dropped    uint64
	reportedAt time.Time
}

func newOutputLimiter(clk clock.Clock, log *slog.Logger) *outputLimiter {
	now := clk.Now()

	return &outputLimiter{clk: clk, log: log, tokens: outputBurst, refilled: now, reportedAt: now}
}

// allow reports whether one more line may be logged now; a refused line is
// counted as dropped.
func (l *outputLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clk.Now()
	if elapsed := now.Sub(l.refilled); elapsed > 0 {
		l.tokens = min(outputBurst, l.tokens+elapsed.Seconds()*outputRate)
		l.refilled = now
	}
	l.reportLocked(now, false)
	if l.tokens >= 1 {
		l.tokens--

		return true
	}
	l.dropped++

	return false
}

// report logs the dropped lines once a minute has passed since the last
// report, or at once when force is set (the plugin stops).
func (l *outputLimiter) report(force bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.reportLocked(l.clk.Now(), force)
}

func (l *outputLimiter) reportLocked(now time.Time, force bool) {
	if !force && now.Sub(l.reportedAt) < dropReportEvery {
		return
	}
	if l.dropped > 0 {
		l.log.Warn("plugin output lines dropped by the rate limit", "dropped", l.dropped)
		l.dropped = 0
	}
	l.reportedAt = now
}

// lineWriter splits one output stream of a plugin process into lines and
// logs them at INFO under the rate limit. Lines are cut at maxOutputLine
// bytes. Write never blocks on the limit and never fails, so the plugin
// never waits on its pipe.
type lineWriter struct {
	stream  string
	limiter *outputLimiter
	log     *slog.Logger

	mu       sync.Mutex
	partial  []byte
	overflow bool
}

func newLineWriter(stream string, limiter *outputLimiter, log *slog.Logger) *lineWriter {
	return &lineWriter{stream: stream, limiter: limiter, log: log}
}

// Write logs every complete line of p and keeps an unterminated rest.
func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			w.appendPartial(p)

			break
		}
		w.appendPartial(p[:end])
		w.emit()
		p = p[end+1:]
	}

	return n, nil
}

// flush logs an unterminated last line, once the process output has ended.
func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.partial) > 0 || w.overflow {
		w.emit()
	}
}

func (w *lineWriter) appendPartial(b []byte) {
	room := maxOutputLine - len(w.partial)
	if len(b) > room {
		b = b[:room]
		w.overflow = true
	}
	w.partial = append(w.partial, b...)
}

func (w *lineWriter) emit() {
	line := string(bytes.TrimSuffix(w.partial, []byte("\r")))
	truncated := w.overflow
	w.partial = w.partial[:0]
	w.overflow = false

	if !w.limiter.allow() {
		return
	}
	if truncated {
		w.log.Info("plugin output", "stream", w.stream, "line", line, "truncated", true)

		return
	}
	w.log.Info("plugin output", "stream", w.stream, "line", line)
}
