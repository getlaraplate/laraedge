// Package pipeline routes the records of the inputs to the buffers of the
// outputs (spec 4, router).
package pipeline

import (
	"log/slog"
	"slices"
	"sync"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/input"
	"github.com/swolley/laraplate-edge/internal/model"
)

// Appender is what the router uses of an output's buffer (*buffer.Buffer).
type Appender interface {
	Append(r model.Record) error
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
	inputs  map[string]config.Input
}

type target struct {
	route config.Route
	buf   Appender
}

// NewRouter returns a router without outputs or inputs. A nil log means
// slog.Default().
func NewRouter(clk clock.Clock, log *slog.Logger) *Router {
	if log == nil {
		log = slog.Default()
	}

	return &Router{clk: clk, log: log, outputs: map[string]target{}, inputs: map[string]config.Input{}}
}

// SetOutput routes to buf the records matching route, from now on, after
// appending the births of the running inputs' devices it routes: no sample
// reaches the output before the birth of its device. The caller hands over a
// buffer ready for appends: its batcher already built.
func (r *Router) SetOutput(id string, route config.Route, buf Appender) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.outputs[id]; !ok {
		r.order = append(r.order, id)
		slices.Sort(r.order)
	}
	r.outputs[id] = target{route: route, buf: buf}
	r.birthTo(id)
}

// RemoveOutput stops routing to output id. When it returns no append to its
// buffer is running or will start.
func (r *Router) RemoveOutput(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.outputs, id)
	r.order = slices.DeleteFunc(r.order, func(o string) bool { return o == id })
}

// SetInput registers the devices and signals of a running input: they make
// the births and decide where its device events go.
func (r *Router) SetInput(in config.Input) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.inputs[in.ID] = in
}

// RemoveInput forgets the devices of input id.
func (r *Router) RemoveInput(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.inputs, id)
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
				r.append(id, t.buf, rec)
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

	in, ok := r.inputs[id]
	if !ok {
		return
	}

	for _, device := range devicesOf(in) {
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
		for _, device := range devicesOf(r.inputs[inID]) {
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
	r.append(id, t.buf, model.EventRecord(ev))
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
		for _, d := range r.inputs[id].Devices {
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
