package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
)

// DefaultFileInterval is how often the file provider looks at the file.
const DefaultFileInterval = 2 * time.Second

// FileConfig configures a File provider.
type FileConfig struct {
	// Path is the configuration document (bootstrap config.file).
	Path string
	// Applier applies the documents.
	Applier Applier
	// Reload asks for a reload whatever the file looks like (SIGHUP); nil
	// means never.
	Reload <-chan struct{}
	// Clock drives the polling; nil means clock.Real().
	Clock clock.Clock
	// Logger receives the logs; nil means slog.Default().
	Logger *slog.Logger
	// Interval is the polling period; zero means DefaultFileInterval.
	Interval time.Duration
	// Attempted, when set, is called once, from Run, when the first load
	// ended, whatever its outcome (systemd readiness).
	Attempted func()
}

// File applies the document of a local file: at start, whenever its
// modification time or size changes, and on a reload request. A document
// that does not parse or that the supervisor refuses is logged with its
// messages and the running configuration stays; an equal version applies
// (hand edits, ruling R9).
type File struct {
	path      string
	applier   Applier
	reload    <-chan struct{}
	clk       clock.Clock
	log       *slog.Logger
	interval  time.Duration
	attempted func()

	// seen is the file as last loaded; owned by Run.
	seen stamp
}

// stamp is what the polling compares.
type stamp struct {
	exists bool
	mod    time.Time
	size   int64
}

func (s stamp) same(o stamp) bool {
	return s.exists == o.exists && s.mod.Equal(o.mod) && s.size == o.size
}

// NewFile returns the provider of cfg.Path.
func NewFile(cfg FileConfig) (*File, error) {
	if cfg.Path == "" {
		return nil, errors.New("provider: file provider without a path")
	}
	if cfg.Applier == nil {
		return nil, errors.New("provider: file provider without an applier")
	}

	f := &File{path: cfg.Path, applier: cfg.Applier, reload: cfg.Reload, clk: cfg.Clock, log: cfg.Logger, interval: cfg.Interval, attempted: cfg.Attempted}
	if f.clk == nil {
		f.clk = clock.Real()
	}
	if f.log == nil {
		f.log = slog.Default()
	}
	if f.interval <= 0 {
		f.interval = DefaultFileInterval
	}

	return f, nil
}

// Run loads the file, then watches it until ctx is done; it returns nil.
// When ctx is already done (a shutdown during ApplyLastValid) it loads
// nothing.
func (f *File) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}

	f.load(ctx)
	if f.attempted != nil {
		f.attempted()
	}

	for {
		timer := f.clk.NewTimer(f.interval)

		select {
		case <-ctx.Done():
			timer.Stop()

			return nil
		case <-timer.C():
			if !f.stat().same(f.seen) {
				f.load(ctx)
			}
		case <-f.reload:
			timer.Stop()
			f.log.Info("provider: reloading the configuration file", "file", f.path)
			f.load(ctx)
		}
	}
}

func (f *File) stat() stamp {
	fi, err := os.Stat(f.path)
	if err != nil {
		return stamp{}
	}

	return stamp{exists: true, mod: fi.ModTime(), size: fi.Size()}
}

// load reads, parses and applies the file. The file is marked seen first, so
// a broken file is reported once per change, not at every poll.
func (f *File) load(ctx context.Context) {
	f.seen = f.stat()

	data, err := readDocument(f.path)
	if err != nil {
		f.log.Warn("provider: cannot read the configuration file", "file", f.path, "error", err)

		return
	}

	doc, msgs, err := config.Parse(data)
	if err != nil {
		f.log.Error("provider: cannot parse the configuration file", "file", f.path, "error", err)

		return
	}
	if len(msgs) > 0 {
		f.log.Warn("provider: configuration file refused", "file", f.path, "errors", msgs)

		return
	}

	out, err := f.applier.Apply(ctx, doc)
	logOutcome(f.log, f.path, doc.Version, out, err)
}

// readDocument reads at most one byte more than config.MaxDocumentSize, so
// Parse reports an oversized file without reading all of it.
func readDocument(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, config.MaxDocumentSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return data, nil
}
