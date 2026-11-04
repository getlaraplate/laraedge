// Package lograte keeps a path that can fail once per record from logging
// once per record.
package lograte

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
)

// Reporter logs the first failure of each kind (such as
// "critical.write_failed") of an episode in full, counts the later ones, and
// logs the counts in one summary at most once per window, measured on the
// clock of the times passed in. A window without any failure ends the
// episode: the next failure is logged in full again. It is safe for
// concurrent use.
type Reporter struct {
	log     *slog.Logger
	window  time.Duration
	summary string

	mu sync.Mutex
	// start is when the current window began.
	start time.Time
	// counts holds, per kind logged in full in this episode, the failures
	// counted but not logged since the last summary.
	counts map[string]uint64
	levels map[string]slog.Level
}

// New returns a Reporter that logs to log and writes its summary with the
// message summary at most once per window.
func New(log *slog.Logger, window time.Duration, summary string) *Reporter {
	return &Reporter{log: log, window: window, summary: summary, counts: map[string]uint64{}, levels: map[string]slog.Level{}}
}

// Report logs the failure in full when it is the first of its kind in this
// episode, and otherwise counts it.
func (r *Reporter) Report(now time.Time, kind string, level slog.Level, msg string, attrs ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.flush(now)

	if _, seen := r.counts[kind]; seen {
		r.counts[kind]++

		return
	}

	if len(r.counts) == 0 {
		r.start = now
	}

	r.counts[kind] = 0
	r.levels[kind] = level
	r.log.Log(context.Background(), level, msg, attrs...)
}

// Flush logs the summary of the counted failures once the window has passed,
// or ends the episode when nothing was counted in it.
func (r *Reporter) Flush(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.flush(now)
}

func (r *Reporter) flush(now time.Time) {
	if len(r.counts) == 0 || now.Sub(r.start) < r.window {
		return
	}

	var total uint64
	level := slog.LevelWarn
	attrs := []any{"window_seconds", int(now.Sub(r.start).Seconds())}
	for _, kind := range slices.Sorted(maps.Keys(r.counts)) {
		n := r.counts[kind]
		if n == 0 {
			continue
		}

		total += n
		level = max(level, r.levels[kind])
		attrs = append(attrs, kind, n)
	}

	if total == 0 {
		clear(r.counts)
		clear(r.levels)

		return
	}

	r.log.Log(context.Background(), level, r.summary, append([]any{"suppressed", total}, attrs...)...)
	for kind := range r.counts {
		r.counts[kind] = 0
	}

	r.start = now
}
