package transform

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/model"
)

// Steps that keep state or emit on a deadline. A step has no clock: its
// deadlines are computed from sample timestamps, and Fire receives the clock
// time from the chain's owner. Fire checks the step's own deadline, because
// Chain.Fire calls every step. A bad sample never reaches Apply (the chain
// passes it through), so it does not touch a step's state; deadband sees it
// through observeBad, because a quality change is a reason to send.

func init() {
	register("deadband", definition{accepts: acceptNumber, build: buildDeadband})
	register("heartbeat", definition{accepts: acceptAny, build: buildHeartbeat})
	register("min_interval", definition{accepts: acceptAny, build: buildMinInterval})
	register("debounce", definition{accepts: acceptAny, build: buildDebounce})
	register("derivative", definition{accepts: acceptNumber, build: buildDerivative})
	register("edge_counter", definition{accepts: acceptBool, build: buildEdgeCounter})
}

// untimed is embedded by the stateful steps that are never time-driven.
type untimed struct{}

// Deadline reports no deadline.
func (untimed) Deadline() (time.Time, bool) { return time.Time{}, false }

// Fire emits nothing.
func (untimed) Fire(time.Time) []model.Sample { return nil }

// --- parameters ---

// durationPattern is the duration pattern of the configuration schema.
var durationPattern = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`)

var errDuration = errors.New("parameter must be a positive duration such as 500ms, 2s or 1m30s")

// durationParam parses a parameter that is a positive duration string.
func durationParam(raw []byte) (time.Duration, error) {
	v, ok := decodeValue(raw)
	if !ok {
		return 0, errDuration
	}

	text, ok := v.(string)
	if !ok || !durationPattern.MatchString(text) {
		return 0, errDuration
	}

	d, err := time.ParseDuration(text)
	if err != nil || d <= 0 {
		return 0, errDuration
	}

	return d, nil
}

// --- deadband ---

// percentPattern is a non-negative percentage such as "5%" or "0.5%".
var percentPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?%$`)

var errDeadband = errors.New(`parameter must be a non-negative number or a percentage such as "5%"`)

// deadband sends a sample when its value moved from the last sent value by
// at least the threshold, or when its quality differs from the last sent
// quality (a bad sample passing through counts as sent). The first value
// always passes. The threshold is absolute, or a fraction of the magnitude of
// the last sent value; with a last sent value of 0 any change passes (P5).
type deadband struct {
	untimed

	kind      model.ValueKind
	threshold float64
	percent   bool

	sent     bool
	last     float64
	lastQual model.Quality
}

func (d *deadband) OutKind() model.ValueKind { return d.kind }

func (d *deadband) Apply(s model.Sample) ([]model.Sample, error) {
	v, ok := asFloat(s.Value)
	if !ok {
		return nil, errValueKind
	}

	if d.sent && s.Quality == d.lastQual && !d.moved(v) {
		return nil, nil
	}

	d.sent, d.last, d.lastQual = true, v, s.Quality

	return []model.Sample{s}, nil
}

// moved reports whether v is far enough from the last sent value.
func (d *deadband) moved(v float64) bool {
	threshold := d.threshold
	if d.percent {
		threshold = d.threshold / 100 * math.Abs(d.last)
	}

	diff := math.Abs(v - d.last)

	return diff > 0 && diff >= threshold
}

// observeBad records that a bad sample left the step: the next good sample is
// a quality change.
func (d *deadband) observeBad(model.Sample) {
	if d.sent {
		d.lastQual = model.Bad
	}
}

func buildDeadband(c buildContext) (Step, error) {
	v, ok := decodeValue(c.Params)
	if !ok {
		return nil, errDeadband
	}

	step := &deadband{kind: c.In}

	switch x := v.(type) {
	case string:
		if !percentPattern.MatchString(x) {
			return nil, errDeadband
		}

		f, err := strconv.ParseFloat(strings.TrimSuffix(x, "%"), 64)
		if err != nil || math.IsInf(f, 0) {
			return nil, errDeadband
		}

		step.threshold, step.percent = f, true
	default:
		f, ok := finiteFloat(x)
		if !ok || f < 0 {
			return nil, errDeadband
		}

		step.threshold = f
	}

	return step, nil
}

// --- heartbeat ---

// heartbeat passes every sample and, when nothing left the step for the
// period, resends the last value with the clock time and its quality. The
// deadline is the ts of the last sample that left plus the period.
type heartbeat struct {
	kind   model.ValueKind
	period time.Duration

	has      bool
	last     model.Sample
	deadline time.Time
}

func (h *heartbeat) OutKind() model.ValueKind { return h.kind }

func (h *heartbeat) Apply(s model.Sample) ([]model.Sample, error) {
	h.has, h.last, h.deadline = true, s, s.TS.Add(h.period)

	return []model.Sample{s}, nil
}

func (h *heartbeat) Deadline() (time.Time, bool) { return h.deadline, h.has }

func (h *heartbeat) Fire(now time.Time) []model.Sample {
	if !h.has || now.Before(h.deadline) {
		return nil
	}

	resent := h.last
	resent.TS = now
	h.deadline = now.Add(h.period)

	return []model.Sample{resent}
}

func buildHeartbeat(c buildContext) (Step, error) {
	d, err := durationParam(c.Params)
	if err != nil {
		return nil, err
	}

	return &heartbeat{kind: c.In, period: d}, nil
}

// --- min_interval ---

// minInterval lets at most one sample out per interval after the first
// (ruling R7). The first sample of a quiet period passes at once and opens a
// window [ts, ts+interval); samples inside the window are held, keeping only
// the latest; at the window end the held sample leaves with its own ts and
// the next window opens; a window that held nothing closes.
type minInterval struct {
	kind     model.ValueKind
	interval time.Duration

	open bool
	end  time.Time
	held *model.Sample
}

func (m *minInterval) OutKind() model.ValueKind { return m.kind }

func (m *minInterval) Apply(s model.Sample) ([]model.Sample, error) {
	out := m.advance(s.TS)

	if !m.open {
		m.open, m.end = true, s.TS.Add(m.interval)

		return append(out, s), nil
	}

	m.held = &s

	return out, nil
}

// Deadline is the end of the window while it holds a sample. A window
// holding nothing closes on the next sample or Fire; it needs no wake-up.
func (m *minInterval) Deadline() (time.Time, bool) { return m.end, m.open && m.held != nil }

func (m *minInterval) Fire(now time.Time) []model.Sample { return m.advance(now) }

// advance ends every window that ended at or before t, emitting what each
// held. A sample at or after the window end therefore leaves the held sample
// first, whether or not Fire ran in time.
func (m *minInterval) advance(t time.Time) []model.Sample {
	var out []model.Sample

	for m.open && !t.Before(m.end) {
		if m.held == nil {
			m.open = false

			break
		}

		out = append(out, *m.held)
		m.held = nil
		m.end = m.end.Add(m.interval)
	}

	return out
}

func buildMinInterval(c buildContext) (Step, error) {
	d, err := durationParam(c.Params)
	if err != nil {
		return nil, err
	}

	return &minInterval{kind: c.In, interval: d}, nil
}

// --- debounce ---

// debounce sends a value only after it stayed unchanged for the delay, with
// the ts of its change. A different value restarts the wait; a return to the
// last sent value cancels it; the last sent value is not resent.
type debounce struct {
	kind  model.ValueKind
	delay time.Duration

	pending *model.Sample
	sent    bool
	last    model.Value
}

func (d *debounce) OutKind() model.ValueKind { return d.kind }

func (d *debounce) Apply(s model.Sample) ([]model.Sample, error) {
	out := d.due(s.TS)

	switch {
	case d.pending != nil && s.Value == d.pending.Value:
	case d.sent && s.Value == d.last:
		d.pending = nil
	default:
		d.pending = &s
	}

	return out, nil
}

func (d *debounce) Deadline() (time.Time, bool) {
	if d.pending == nil {
		return time.Time{}, false
	}

	return d.pending.TS.Add(d.delay), true
}

func (d *debounce) Fire(now time.Time) []model.Sample { return d.due(now) }

// due emits the waiting value when its deadline is at or before t. A sample
// at or after the deadline therefore sends the waiting value first, whether
// or not Fire ran in time.
func (d *debounce) due(t time.Time) []model.Sample {
	if d.pending == nil || t.Before(d.pending.TS.Add(d.delay)) {
		return nil
	}

	s := *d.pending
	d.pending, d.sent, d.last = nil, true, s.Value

	return []model.Sample{s}
}

func buildDebounce(c buildContext) (Step, error) {
	d, err := durationParam(c.Params)
	if err != nil {
		return nil, err
	}

	return &debounce{kind: c.In, delay: d}, nil
}

// --- derivative ---

// derivative is the change per second between two consecutive samples, over
// their timestamps. The first sample, a sample not later than the previous
// one and a backward jump (a totaliser reset) produce nothing; each of them
// becomes the new baseline.
type derivative struct {
	untimed

	has  bool
	prev float64
	at   time.Time
}

func (*derivative) OutKind() model.ValueKind { return model.KindFloat }

func (d *derivative) Apply(s model.Sample) ([]model.Sample, error) {
	v, ok := asFloat(s.Value)
	if !ok {
		return nil, errValueKind
	}

	prev, at, had := d.prev, d.at, d.has
	d.has, d.prev, d.at = true, v, s.TS

	if !had || !s.TS.After(at) || v < prev {
		return nil, nil
	}

	return finiteSample(s, (v-prev)/s.TS.Sub(at).Seconds())
}

func buildDerivative(c buildContext) (Step, error) {
	if err := trueParam(c.Params); err != nil {
		return nil, err
	}

	return &derivative{}, nil
}

// --- edge_counter ---

// edgeMode selects the edges an edge_counter counts.
type edgeMode uint8

const (
	edgeRising edgeMode = 1 << iota
	edgeFalling

	edgeBoth = edgeRising | edgeFalling
)

var errEdgeMode = errors.New("parameter must be rising, falling or both")

// saveError is a failed Save of the count. Its text is fixed: the cause may
// name a path of the data directory.
type saveError struct{ cause error }

func (saveError) Error() string { return "count cannot be saved" }

func (e saveError) Unwrap() error { return e.cause }

// edgeCounter is the cumulative count of the counted edges of a boolean
// (ruling R8). The count starts from the Store; the first sample after start
// emits the count as a baseline and sets the level; each counted edge then
// increments the count, saves it and emits it; other samples emit nothing.
type edgeCounter struct {
	untimed

	mode  edgeMode
	store Store

	count   int64
	started bool
	level   bool
}

func (*edgeCounter) OutKind() model.ValueKind { return model.KindInt }

func (e *edgeCounter) Apply(s model.Sample) ([]model.Sample, error) {
	if s.Value.Kind != model.KindBool {
		return nil, errValueKind
	}

	v := s.Value.Bool

	if !e.started {
		e.started, e.level = true, v

		return withValue(s, model.IntValue(e.count)), nil
	}

	rising := v && !e.level
	falling := !v && e.level
	e.level = v

	if !(rising && e.mode&edgeRising != 0) && !(falling && e.mode&edgeFalling != 0) {
		return nil, nil
	}

	e.count++

	if e.store != nil {
		if err := e.store.Save(e.count); err != nil {
			return nil, saveError{cause: err}
		}
	}

	return withValue(s, model.IntValue(e.count)), nil
}

func buildEdgeCounter(c buildContext) (Step, error) {
	v, ok := decodeValue(c.Params)
	if !ok {
		return nil, errEdgeMode
	}

	text, _ := v.(string)

	var mode edgeMode

	switch text {
	case "rising":
		mode = edgeRising
	case "falling":
		mode = edgeFalling
	case "both":
		mode = edgeBoth
	default:
		return nil, errEdgeMode
	}

	step := &edgeCounter{mode: mode}

	if c.NewState != nil {
		step.store = c.NewState(c.Position, c.Canonical)
	}

	if step.store != nil {
		if count, found := step.store.Load(); found {
			step.count = count
		}
	}

	return step, nil
}
