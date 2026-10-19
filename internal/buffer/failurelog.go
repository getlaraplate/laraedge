package buffer

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"
)

// failureLogWindow is the shortest interval between two summaries of the
// write and drop path's failures.
const failureLogWindow = time.Minute

// failureLog keeps the write and drop path from logging once per record on
// a disk that refuses every write. The first failure of each kind (queue and
// reason, such as "critical.write_failed") is logged in full; later ones are
// only counted, and the counts are logged in one summary at most once per
// failureLogWindow, measured on the buffer's clock. A window without any
// failure ends the episode: the next failure is logged in full again. The
// owner holds the buffer's lock around every call.
type failureLog struct {
	log *slog.Logger
	// start is when the current window began.
	start time.Time
	// counts holds, per kind logged in full in this episode, the failures
	// counted but not logged since the last summary.
	counts map[string]uint64
	levels map[string]slog.Level
}

func newFailureLog(log *slog.Logger) *failureLog {
	return &failureLog{log: log, counts: map[string]uint64{}, levels: map[string]slog.Level{}}
}

// report logs the failure in full when it is the first of its kind in this
// episode, and otherwise counts it.
func (f *failureLog) report(now time.Time, kind string, level slog.Level, msg string, attrs ...any) {
	f.flush(now)

	if _, seen := f.counts[kind]; seen {
		f.counts[kind]++

		return
	}

	if len(f.counts) == 0 {
		f.start = now
	}

	f.counts[kind] = 0
	f.levels[kind] = level
	f.log.Log(context.Background(), level, msg, attrs...)
}

// flush logs the summary of the counted failures once the window has passed,
// or ends the episode when nothing was counted in it.
func (f *failureLog) flush(now time.Time) {
	if len(f.counts) == 0 || now.Sub(f.start) < failureLogWindow {
		return
	}

	var total uint64
	level := slog.LevelWarn
	attrs := []any{"window_seconds", int(now.Sub(f.start).Seconds())}
	for _, kind := range slices.Sorted(maps.Keys(f.counts)) {
		n := f.counts[kind]
		if n == 0 {
			continue
		}

		total += n
		level = max(level, f.levels[kind])
		attrs = append(attrs, kind, n)
	}

	if total == 0 {
		clear(f.counts)
		clear(f.levels)

		return
	}

	f.log.Log(context.Background(), level, "buffer: write and drop failures repeated since the last report", append([]any{"suppressed", total}, attrs...)...)
	for kind := range f.counts {
		f.counts[kind] = 0
	}

	f.start = now
}
