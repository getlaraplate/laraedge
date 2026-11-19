package serial

import (
	"context"
	"errors"
	"fmt"
	"time"

	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

// KindNoFrame is the kind of a probed point that no frame answered.
const KindNoFrame = "no frame"

// maxProbeUnmatched is how many unmatched frames a probe keeps for its notes.
const maxProbeUnmatched = 10

// probeContextKeys is the order the context lines of a probe come in.
var probeContextKeys = []string{"serial", "order_ref", "operation_ref", "lot"}

// PointRef names a point of an input: its device and signal.
type PointRef = edgeinput.PointRef

// Probe opens the port once and reads it like the running input would,
// without emitting, until every point has a sample or ctx ends; see
// ProbePoints.
func (in *Input) Probe(ctx context.Context) ([]model.Sample, error) {
	samples, _, err := in.ProbePoints(ctx)
	return samples, err
}

// ProbePoints opens the port once (no backoff), sends the configured requests
// on their schedule, routes the frames with a fresh router, framer and
// scheduler, and keeps the first sample of every point, in configuration
// order. It returns as soon as every point has one, or when ctx ends with
// what it has and no error. kinds says, for a point without a sample,
// "no frame", and for a bad sample its read error kind. It never emits and
// never touches the lifecycle; the port is closed on return. A busy port is
// an error telling that the input may be running.
func (in *Input) ProbePoints(ctx context.Context) ([]model.Sample, map[PointRef]string, error) {
	port, err := in.deps.open(in.conn.Device, in.conn.Mode)
	if err != nil {
		return nil, nil, in.probeOpenError(err)
	}
	defer func() { _ = port.Close() }()

	router, err := newRouter(in.cfg, in.conn)
	if err != nil {
		return nil, nil, err
	}
	framer := newFramer(in.conn.Framing, in.conn.MaxFrame)
	var sched *scheduler
	if len(in.conn.Requests) > 0 {
		sched = newScheduler(in.conn.Requests, in.conn.RequestTimeout, in.clk.Now())
	}

	var order []PointRef
	for _, dev := range in.cfg.Devices {
		for _, p := range dev.Points {
			order = append(order, PointRef{Device: dev.Device, Signal: p.Signal})
		}
	}
	want := len(distinct(order))
	got := make(map[PointRef]model.Sample, len(order))
	kinds := make(map[PointRef]string)
	contexts := map[string]string{}
	var unmatched []string

	buf := make([]byte, readBufferSize)
	var readErr error
	for ctx.Err() == nil && len(got) < want {
		n, err := port.Read(buf)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			readErr = err
			break
		}
		at := in.clk.Now()

		var frames [][]byte
		if n > 0 {
			frames, _ = framer.push(buf[:n], at)
		} else {
			frames, _ = framer.tick(at)
		}
		for _, frame := range frames {
			if q := probeRoute(router, frame, at, got, kinds, contexts); q != "" && len(unmatched) < maxProbeUnmatched {
				unmatched = append(unmatched, q)
			}
		}

		if sched == nil {
			continue
		}
		for range frames {
			sched.frame(at)
		}
		sched.expired(at)
		if cmd, ok := sched.next(at); ok {
			if _, err := port.Write(cmd); err != nil {
				readErr = err
				break
			}
		}
	}

	in.setNotes(contexts, unmatched)
	if readErr != nil {
		return nil, nil, fmt.Errorf("serial input %q: probe: the port failed while reading", in.cfg.ID)
	}

	var samples []model.Sample
	seen := map[PointRef]bool{}
	for _, ref := range order {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if s, ok := got[ref]; ok {
			samples = append(samples, s)
		} else {
			kinds[ref] = KindNoFrame
		}
	}
	return samples, kinds, nil
}

// probeRoute routes one frame into the probe state and returns the quoted frame
// when it matched nothing.
func probeRoute(r *router, frame []byte, at time.Time, got map[PointRef]model.Sample, kinds map[PointRef]string, contexts map[string]string) string {
	res := r.route(frame, at)
	switch {
	case res.Sample != nil:
		ref := PointRef{Device: res.Sample.Device, Signal: res.Sample.Signal}
		if _, ok := got[ref]; !ok {
			got[ref] = *res.Sample
			if res.Kind != "" {
				kinds[ref] = res.Kind
			}
		}
	case res.ContextKey != "":
		contexts[res.ContextKey] = res.ContextValue
	case res.Kind == KindUnmatched:
		return quoteFrame(frame)
	}
	return ""
}

// distinct returns the points of order without repeats.
func distinct(order []PointRef) map[PointRef]struct{} {
	out := make(map[PointRef]struct{}, len(order))
	for _, ref := range order {
		out[ref] = struct{}{}
	}
	return out
}

// probeOpenError names the cause class of a failed open, never the error text.
func (in *Input) probeOpenError(err error) error {
	var what string
	switch {
	case errors.Is(err, ErrPortBusy):
		what = "port busy: the input may be running; stop it before probing"
	case errors.Is(err, ErrPortMissing):
		what = "the port does not exist"
	case errors.Is(err, ErrPortPermission):
		what = "permission denied opening the port"
	default:
		what = "the port cannot be opened"
	}
	return fmt.Errorf("serial input %q: probe: %s", in.cfg.ID, what)
}

// setNotes keeps the notes of the probe: the context seen, then the unmatched
// frames (already quoted and truncated).
func (in *Input) setNotes(contexts map[string]string, unmatched []string) {
	var notes []string
	for _, key := range probeContextKeys {
		if v, ok := contexts[key]; ok {
			notes = append(notes, "context: "+key+"="+v)
		}
	}
	for _, q := range unmatched {
		notes = append(notes, "unmatched: "+q)
	}
	in.mu.Lock()
	in.notes = notes
	in.mu.Unlock()
}

// ProbeNotes returns the lines the last probe saw besides the samples:
// "context: <key>=<value>" for each context key set, in the order serial,
// order_ref, operation_ref, lot, then up to ten "unmatched: <frame>" lines,
// the frame quoted and cut at 80 bytes. Probe is an operator tool: values are
// printed on purpose.
func (in *Input) ProbeNotes() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]string(nil), in.notes...)
}
