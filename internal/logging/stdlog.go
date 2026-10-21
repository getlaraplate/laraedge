package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
)

// StdlogBridge returns a writer for the standard log package that turns each
// line it receives into one record of l at debug level with the attribute
// source=gopcua (decision O6): the OPC UA library logs only through the
// standard default logger, and its lines may name an endpoint, so they never
// reach info. Blank lines are dropped. The caller sets log.SetFlags(0) so no
// date prefix enters the message, then log.SetOutput with this writer. A nil
// l discards everything.
//
// The standard logger writes one entry per Write call, so no partial line is
// kept between calls: a Write without a trailing newline is a whole line.
func StdlogBridge(l *slog.Logger) io.Writer {
	return stdlogBridge{log: l}
}

type stdlogBridge struct {
	log *slog.Logger
}

func (b stdlogBridge) Write(p []byte) (int, error) {
	if b.log == nil || !b.log.Enabled(context.Background(), slog.LevelDebug) {
		return len(p), nil
	}

	for line := range bytes.SplitSeq(p, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		b.log.LogAttrs(context.Background(), slog.LevelDebug, string(line), slog.String("source", "gopcua"))
	}

	return len(p), nil
}
