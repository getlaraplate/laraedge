// Package pipeline routes the records of the inputs to the buffers of the
// outputs (spec 4, router).
package pipeline

import (
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/input"
	"github.com/swolley/laraplate-edge/internal/model"
)

// HoldLimit bounds the records kept in memory for an output while it
// restarts; the records beyond it become buffer_full loss markers.
const HoldLimit = 10_000

// Appender is what the router uses of an output's buffer (*buffer.Buffer).
type Appender interface {
	Append(r model.Record) error
	AddLoss(l model.Loss) error
}

// Router fans every record out to the buffers of the outputs whose route
// matches it. A sample goes to each output whose route.include matches its
// device/signal. A device event goes to each output routing at least one of
// the device's configured signals, and a birth lists only the signals routed
// to that output. It is safe for concurrent use.
type Router struct {
	clk clock.Clock
	log *slog.Logger

	mu      sync.RWMutex
	outputs map[string]target
	order   []string // output ids, sorted
	inputs  map[string]registeredInput
	// lastToken is the token of the latest input registration.
	lastToken InputToken
}

// InputToken identifies one registration of an input with the router: the
// running instance that made it. Only that instance's RemoveInput removes it
// (ruling R50).
type InputToken uint64

type registeredInput struct {
	cfg   config.Input
	token InputToken
}

type target struct {
	route config.Route
	buf   Appender
	// hold, while the output restarts, keeps what would have been appended.
	hold *holdQueue
}

// holdQueue keeps the records of a restarting or failed output, at most
// HoldLimit; the samples beyond it are counted per priority with their ts
// range (ruling R46). Device events beyond it are not samples: a birth is
// sent again when the output starts, a death is remembered by device to be
// logged.
type holdQueue struct {
	mu         sync.Mutex
	recs       []model.Record
	lost       map[model.Priority]*model.Loss
	lostDeaths []string
}

func newHoldQueue() *holdQueue {
	return &holdQueue{lost: map[model.Priority]*model.Loss{}}
}

func (q *holdQueue) add(rec model.Record) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.recs) < HoldLimit {
		q.recs = append(q.recs, rec)

		return
	}

	if rec.Event != nil {
		if rec.Event.Kind == model.Death && !slices.Contains(q.lostDeaths, rec.Event.Device) {
			q.lostDeaths = append(q.lostDeaths, rec.Event.Device)
		}

		return
	}

	ts := recordTS(rec)
	l := q.lost[rec.Priority()]
	if l == nil {
		l = &model.Loss{Priority: rec.Priority(), FromTS: ts, ToTS: ts, Reason: model.LossBufferFull}
		q.lost[rec.Priority()] = l
	}
	l.Samples++
	if ts.Before(l.FromTS) {
		l.FromTS = ts
	}
	if ts.After(l.ToTS) {
		l.ToTS = ts
	}
}

func recordTS(rec model.Record) time.Time {
	if rec.Sample != nil {
		return rec.Sample.TS
	}

	return rec.Event.TS
}

// NewRouter returns a router without outputs or inputs. A nil log means
// slog.Default().
func NewRouter(clk clock.Clock, log *slog.Logger) *Router {
	if log == nil {
		log = slog.Default()
	}

	return &Router{clk: clk, log: log, outputs: map[string]target{}, inputs: map[string]registeredInput{}}
}

// SetOutput routes to buf the records matching route, from now on, after
// appending the births of the running inputs' devices it routes: no sample
// reaches the output before the birth of its device. When the output was
// held, the held records follow the births, routed again with route, and
// the records beyond HoldLimit become buffer_full markers on buf. The caller
// hands over a buffer ready for appends: its batcher already built.
func (r *Router) SetOutput(id string, route config.Route, buf Appender) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.outputs[id]
	if !ok {
		r.order = append(r.order, id)
		slices.Sort(r.order)
	}
	r.outputs[id] = target{route: route, buf: buf}
	r.birthTo(id)

	if ok && old.hold != nil {
		r.flush(id, old.hold)
	}
}

// flush appends the held records of output id and its overflow markers.
func (r *Router) flush(id string, q *holdQueue) {
	q.mu.Lock()
	defer q.mu.Unlock()

	t := r.outputs[id]
	for _, rec := range q.recs {
		switch {
		case rec.Sample != nil:
			if t.route.Match(rec.Sample.Device, rec.Sample.Signal) {
				r.append(id, t.buf, rec)
			}
		case rec.Event != nil && rec.Event.Kind != model.Birth:
			// Held births are dropped: SetOutput has just sent the current
			// ones, possibly with a wider signal list.
			r.event(id, *rec.Event)
		}
	}
	r.logLostDeaths(id, q)

	for _, p := range []model.Priority{model.Critical, model.Bulk} {
		l := q.lost[p]
		if l == nil {
			continue
		}
		r.log.Warn("router: records beyond the hold limit while the output restarted; recorded as lost", "output", id, "priority", p.String(), "samples", l.Samples)
		if err := t.buf.AddLoss(*l); err != nil {
			r.log.Warn("router: cannot record the loss marker of a restart", "output", id, "priority", p.String(), "error", err)
		}
	}
}

// logLostDeaths warns about each device whose death went past the hold
// limit of output id.
func (r *Router) logLostDeaths(id string, q *holdQueue) {
	for _, device := range q.lostDeaths {
		r.log.Warn("router: a death beyond the hold limit of a restarting output is lost", "output", id, "device", device)
	}
}

// Hold keeps, in memory, what route selects for output id until the next
// SetOutput of id, which flushes it, or RemoveOutput of id, which discards
// it: the output restarts, or failed to and waits for a later document
// (rulings R46, R48). route is that of the configuration being restored.
// Holding a held output only changes its route. When it returns no append
// to the previous buffer is running or will start.
func (r *Router) Hold(id string, route config.Route) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.outputs[id]
	if !ok {
		return
	}
	hold := t.hold
	if hold == nil {
		hold = newHoldQueue()
	}
	r.outputs[id] = target{route: route, hold: hold}
}

// RemoveOutput stops routing to output id. Records held for it are
// discarded with one warning per priority giving reason, the count and the
// ts range. When it returns no append to its buffer is running or will
// start.
func (r *Router) RemoveOutput(id, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if t, ok := r.outputs[id]; ok && t.hold != nil {
		r.discard(id, reason, t.hold)
	}
	delete(r.outputs, id)
	r.order = slices.DeleteFunc(r.order, func(o string) bool { return o == id })
}

// discard logs what a held queue loses: per priority, the held records and
// the overflow, with their ts range, and the lost deaths.
func (r *Router) discard(id, reason string, q *holdQueue) {
	q.mu.Lock()
	defer q.mu.Unlock()

	lost := map[model.Priority]*model.Loss{}
	for p, l := range q.lost {
		c := *l
		lost[p] = &c
	}
	for _, rec := range q.recs {
		p, ts := rec.Priority(), recordTS(rec)
		l := lost[p]
		if l == nil {
			l = &model.Loss{Priority: p, FromTS: ts, ToTS: ts}
			lost[p] = l
		}
		l.Samples++
		if ts.Before(l.FromTS) {
			l.FromTS = ts
		}
		if ts.After(l.ToTS) {
			l.ToTS = ts
		}
	}

	for _, p := range []model.Priority{model.Critical, model.Bulk} {
		if l := lost[p]; l != nil {
			r.log.Warn("router: held records of an output discarded", "output", id, "reason", reason,
				"priority", p.String(), "records", l.Samples, "from_ts", l.FromTS.UTC(), "to_ts", l.ToTS.UTC())
		}
	}
	r.logLostDeaths(id, q)
}

// SetInput registers the devices and signals of a running input: they make
// the births and decide where its device events go. It replaces any earlier
// registration of the same id and returns the token of this one, which its
// RemoveInput must give.
func (r *Router) SetInput(in config.Input) InputToken {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lastToken++
	r.inputs[in.ID] = registeredInput{cfg: in, token: r.lastToken}

	return r.lastToken
}

// RemoveInput forgets the devices of input id when token is that of its
// current registration. A token of an earlier registration removes nothing:
// an instance that ended late cannot unregister the one that replaced it.
func (r *Router) RemoveInput(id string, token InputToken) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if reg, ok := r.inputs[id]; ok && reg.token == token {
		delete(r.inputs, id)
	}
}

// Emit routes one record. A birth of a registered device lists the device's
// configured signals across the running inputs. Append errors, drops
// included, are logged at warn with the output and the device and signal,
// never the value; the other outputs still receive the record.
func (r *Router) Emit(rec model.Record) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	switch {
	case rec.Sample != nil:
		for _, id := range r.order {
			t := r.outputs[id]
			if t.route.Match(rec.Sample.Device, rec.Sample.Signal) {
				r.deliver(id, t, rec)
			}
		}
	case rec.Event != nil:
		ev := *rec.Event
		if signals, known := r.deviceSignals(ev.Device); known && ev.Kind == model.Birth {
			ev.Signals = signals
		}
		for _, id := range r.order {
			r.event(id, ev)
		}
	}
}

// BirthInput sends the birth of every device of input id to every output
// routing some of its signals: at the input's start.
func (r *Router) BirthInput(id string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	reg, ok := r.inputs[id]
	if !ok {
		return
	}

	for _, device := range devicesOf(reg.cfg) {
		ev := r.birth(device)
		for _, out := range r.order {
			r.event(out, ev)
		}
	}
}

// BirthTo sends to output id the birth of every device of the running inputs
// that it routes: when the output starts and when it reconnects (ruling R5).
func (r *Router) BirthTo(id string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	r.birthTo(id)
}

func (r *Router) birthTo(id string) {
	if _, ok := r.outputs[id]; !ok {
		return
	}

	seen := map[string]bool{}
	for _, inID := range r.inputIDs() {
		for _, device := range devicesOf(r.inputs[inID].cfg) {
			if seen[device] {
				continue
			}
			seen[device] = true
			r.event(id, r.birth(device))
		}
	}
}

// birth builds the birth of device at the clock time.
func (r *Router) birth(device string) model.DeviceEvent {
	signals, _ := r.deviceSignals(device)

	return model.DeviceEvent{Kind: model.Birth, Device: device, Signals: signals, TS: r.clk.Now().UTC()}
}

// event sends ev to output id when the output routes one of the device's
// signals; a birth keeps only the routed ones.
func (r *Router) event(id string, ev model.DeviceEvent) {
	t, ok := r.outputs[id]
	if !ok {
		return
	}

	signals, _ := r.deviceSignals(ev.Device)
	if ev.Kind == model.Birth {
		signals = ev.Signals
	}

	var routed []model.SignalInfo
	for _, si := range signals {
		if t.route.Match(ev.Device, si.Signal) {
			routed = append(routed, si)
		}
	}
	if len(routed) == 0 {
		return
	}

	if ev.Kind == model.Birth {
		ev.Signals = routed
	}
	r.deliver(id, t, model.EventRecord(ev))
}

// deliver appends rec to the output's buffer, or holds it while the output
// restarts.
func (r *Router) deliver(id string, t target, rec model.Record) {
	if t.hold != nil {
		t.hold.add(rec)

		return
	}
	r.append(id, t.buf, rec)
}

func (r *Router) append(id string, buf Appender, rec model.Record) {
	if err := buf.Append(rec); err != nil {
		attrs := []any{"output", id, "device", rec.Device(), "error", err}
		if rec.Sample != nil {
			attrs = append(attrs, "signal", rec.Sample.Signal)
		} else {
			attrs = append(attrs, "event", rec.Event.Kind.String())
		}
		r.log.Warn("router: cannot buffer a record", attrs...)
	}
}

// deviceSignals returns the configured signals of device across the running
// inputs, in input id order, and whether the device is configured.
func (r *Router) deviceSignals(device string) ([]model.SignalInfo, bool) {
	var out []model.SignalInfo
	known := false
	for _, id := range r.inputIDs() {
		for _, d := range r.inputs[id].cfg.Devices {
			if d.Device == device {
				known = true
				out = append(out, input.Signals(d)...)
			}
		}
	}

	return out, known
}

func (r *Router) inputIDs() []string {
	ids := make([]string, 0, len(r.inputs))
	for id := range r.inputs {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	return ids
}

// devicesOf returns the device names of in, in configuration order, once.
func devicesOf(in config.Input) []string {
	var out []string
	for _, d := range in.Devices {
		if !slices.Contains(out, d.Device) {
			out = append(out, d.Device)
		}
	}

	return out
}
