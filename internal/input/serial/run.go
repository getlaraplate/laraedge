package serial

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strconv"
	"time"

	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Read error kinds of the port and of the requests (spec section 9).
const (
	// KindOpen is a failed attempt to open the port.
	KindOpen = "open"
	// KindPortLost is an open port that failed a read or a write.
	KindPortLost = "port_lost"
	// KindNoResponse is a request command without a frame in time.
	KindNoResponse = "no_response"
)

// Log messages of the input.
const (
	msgOpenMissing      = "serial: the port does not exist; retrying with backoff"
	msgOpenBusy         = "serial: the port is busy, held by another program or input; retrying with backoff"
	msgOpenPermission   = "serial: permission denied opening the port; retrying with backoff"
	msgOpenFailed       = "serial: the port cannot be opened; retrying with backoff"
	msgPortLost         = "serial: the port was lost; reopening with backoff"
	msgContextSet       = "serial: context set"
	msgContextDropped   = "serial: the input stops; its context is dropped"
	msgUnmatched        = "serial: a frame matches no context rule and no point"
	msgUnmatchedSummary = "serial: frames matching no context rule and no point"
	msgDuplicateAddress = "serial: the address repeats an earlier point's; this point never receives a frame"
)

// dialoutHint names the group a Linux user needs to open serial ports.
const dialoutHint = "add the agent's user to the dialout group"

// unmatchedWindow is the window of the rate-limited unmatched frame log.
const unmatchedWindow = time.Minute

// maxLoggedFrame is the longest part of a frame put in a log line, in bytes.
const maxLoggedFrame = 80

// readBufferSize is the size of one read from the port.
const readBufferSize = 4096

// reader is the state of the goroutine owning the port; only that goroutine
// uses it.
type reader struct {
	in      *Input
	emit    func(model.Record)
	life    *edgeinput.Lifecycle
	framer  *framer
	backoff *driverkit.Backoff
	// warned holds the open failure causes logged since the port last held.
	warned map[string]bool
	// lostWarned is set once a loss of the port was logged, until the port
	// holds (a frame read or BackoffMax open): a port that flaps is warned
	// once per episode.
	lostWarned bool
	buf        []byte
}

// run opens the port with the backoff, reads it until it is lost, and opens
// it again, until ctx is done (spec section 8).
func (in *Input) run(ctx context.Context, emit func(model.Record), life *edgeinput.Lifecycle) {
	r := &reader{
		in:      in,
		emit:    emit,
		life:    life,
		framer:  newFramer(in.conn.Framing, in.conn.MaxFrame),
		backoff: driverkit.NewBackoff(in.clk, in.deps.jitter),
		warned:  map[string]bool{},
		buf:     make([]byte, readBufferSize),
	}
	backoff := r.backoff

	for ctx.Err() == nil {
		port, err := in.deps.open(in.conn.Device, in.conn.Mode)
		if err != nil {
			in.health.CountError(KindOpen)
			r.warnOpen(err)
			if backoff.Wait(ctx) != nil {
				return
			}
			continue
		}

		in.health.SetConnected(true)
		err = r.serve(ctx, port)
		_ = port.Close()
		in.health.SetConnected(false)
		r.framer.reset()
		if err == nil {
			return
		}

		in.health.CountError(KindPortLost)
		if !r.lostWarned {
			r.lostWarned = true
			in.log.Warn(msgPortLost, "input", in.cfg.ID, "error", err.Error())
		}
		if backoff.Wait(ctx) != nil {
			return
		}
	}
}

// warnOpen logs an open failure at warn once per cause until the port opens.
func (r *reader) warnOpen(err error) {
	cause, msg := "other", msgOpenFailed
	var attrs []any
	switch {
	case errors.Is(err, ErrPortMissing):
		cause, msg = "missing", msgOpenMissing
	case errors.Is(err, ErrPortBusy):
		cause, msg = "busy", msgOpenBusy
	case errors.Is(err, ErrPortPermission):
		cause, msg = "permission", msgOpenPermission
		if runtime.GOOS == "linux" {
			attrs = append(attrs, "hint", dialoutHint)
		}
	}
	if r.warned[cause] {
		return
	}
	r.warned[cause] = true
	in := r.in
	in.log.Warn(msg, append([]any{"input", in.cfg.ID, "device", in.conn.Device, "error", err.Error()}, attrs...)...)
}

// serve reads the open port one cycle at a time until ctx is done (nil) or a
// read or a write fails (the error). Requests are scheduled from the open
// on and dropped with the port. The port holds once it delivers a frame or
// stays open for BackoffMax: only then are the backoff and the logged open
// causes reset, so a port that opens and fails at once still backs off.
func (r *reader) serve(ctx context.Context, port Port) error {
	in := r.in
	opened := in.clk.Now()
	held := false
	var sched *scheduler
	if len(in.conn.Requests) > 0 {
		sched = newScheduler(in.conn.Requests, in.conn.RequestTimeout, in.clk.Now())
	}

	for {
		n, err := port.Read(r.buf)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		at := in.clk.Now()

		var frames []frame
		var kinds []string
		if n > 0 {
			frames, kinds = r.framer.push(r.buf[:n], at)
		} else {
			frames, kinds = r.framer.tick(at)
		}
		for _, kind := range kinds {
			in.health.CountError(kind)
		}
		if len(frames) > 0 {
			r.lostWarned = false
		}
		if !held && (len(frames) > 0 || at.Sub(opened) >= driverkit.BackoffMax) {
			held = true
			r.backoff.Reset()
			clear(r.warned)
			r.lostWarned = false
		}

		if sched == nil || len(frames) > 0 {
			r.renew()
		}
		for _, f := range frames {
			r.handle(f.data, f.at)
		}

		if sched == nil {
			continue
		}
		for range frames {
			sched.frame(at)
		}
		if sched.expired(at) {
			in.health.CountError(KindNoResponse)
		}
		if cmd, ok := sched.next(at); ok {
			if _, err := port.Write(cmd); err != nil {
				return err
			}
			r.framer.commandSent()
		}
	}
}

// renew records a successful read of every device of the input (spec S9).
func (r *reader) renew() {
	for _, device := range r.in.devices {
		r.life.Read(device)
	}
}

// handle routes one frame read at at: it emits the sample, logs a context
// change (a frame repeating the value in force is not logged), and counts the kind of a frame dropped or converted badly.
func (r *reader) handle(frame []byte, at time.Time) {
	in := r.in
	got := in.router.route(frame, at)
	if got.Kind != "" {
		in.health.CountError(got.Kind)
	}
	switch {
	case got.Sample != nil:
		r.emit(model.SampleRecord(*got.Sample))
	case got.ContextChanged:
		in.log.Info(msgContextSet, "input", in.cfg.ID, "key", got.ContextKey, "value", got.ContextValue)
	case got.Kind == KindUnmatched:
		in.unmatched.Report(at, KindUnmatched, slog.LevelDebug, msgUnmatched, "input", in.cfg.ID, "frame", quoteFrame(frame))
	}
}

// quoteFrame quotes the first maxLoggedFrame bytes of a frame for a log line.
func quoteFrame(frame []byte) string {
	if len(frame) > maxLoggedFrame {
		frame = frame[:maxLoggedFrame]
	}
	return strconv.Quote(string(frame))
}
