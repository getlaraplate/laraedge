package transform_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/clock"
	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/transform"
)

// out is one sample expected to leave the chain: its timestamp as an offset
// from t0, its value and its quality (zero means good).
type out struct {
	at    time.Duration
	value model.Value
	q     model.Quality
}

// deadlineWant checks the chain's deadline after an event; the zero value
// does not check.
type deadlineWant struct {
	check bool
	has   bool
	at    time.Duration
}

var noDeadline = deadlineWant{check: true}

func deadlineAt(d time.Duration) deadlineWant { return deadlineWant{check: true, has: true, at: d} }

// event is a sample applied with ts t0+at, or, when fire is set, a Fire of the
// chain with the fake clock at t0+at.
type event struct {
	at       time.Duration
	fire     bool
	value    model.Value
	q        model.Quality // zero means good
	want     []out
	wantErr  bool
	deadline deadlineWant
}

func apply(at time.Duration, v model.Value, want ...out) event {
	return event{at: at, value: v, want: want}
}

func fire(at time.Duration, want ...out) event {
	return event{at: at, fire: true, want: want}
}

func (e event) withQuality(q model.Quality) event {
	e.q = q
	return e
}

func (e event) failing() event {
	e.wantErr = true
	return e
}

func (e event) expectDeadline(d deadlineWant) event {
	e.deadline = d
	return e
}

func f64(f float64) model.Value { return model.Value{Kind: model.KindFloat, Float: f} }

func i64(i int64) model.Value { return model.IntValue(i) }

func boolean(b bool) model.Value { return model.BoolValue(b) }

func good(at time.Duration, v model.Value) out { return out{at: at, value: v} }

type statefulCase struct {
	name   string
	steps  []config.Transform
	in     model.ValueKind
	state  transform.StateFactory
	events []event
}

// runEvents drives a chain through events on a fake clock and checks every
// sample that leaves it.
func runEvents(t *testing.T, tc statefulCase) {
	t.Helper()

	chain, err := transform.Compile(tc.steps, tc.in, tc.state)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	fc := clock.NewFake(t0)

	for i, ev := range tc.events {
		label := fmt.Sprintf("event %d (%s)", i, ev.at)

		if now := t0.Add(ev.at); now.After(fc.Now()) {
			fc.Advance(now.Sub(fc.Now()))
		}

		var (
			got  []model.Sample
			errs []error
		)

		if ev.fire {
			got, errs = chain.Fire(fc.Now())
		} else {
			in := sample(ev.value)
			in.TS = t0.Add(ev.at)
			if ev.q != 0 {
				in.Quality = ev.q
			}
			got, errs = chain.Apply(in)
		}

		if (len(errs) != 0) != ev.wantErr {
			t.Fatalf("%s: errors = %v, wantErr %v", label, errs, ev.wantErr)
		}

		if len(got) != len(ev.want) {
			t.Fatalf("%s: got %d samples %+v, want %d %+v", label, len(got), got, len(ev.want), ev.want)
		}

		for j, w := range ev.want {
			wq := w.q
			if wq == 0 {
				wq = model.Good
			}

			g := got[j]
			if !g.TS.Equal(t0.Add(w.at)) {
				t.Errorf("%s sample %d: ts = +%s, want +%s", label, j, g.TS.Sub(t0), w.at)
			}
			if !sameValue(g.Value, w.value) {
				t.Errorf("%s sample %d: value = %+v, want %+v", label, j, g.Value, w.value)
			}
			if g.Quality != wq {
				t.Errorf("%s sample %d: quality = %v, want %v", label, j, g.Quality, wq)
			}
			if g.Device != "press1" || g.Signal != "x" || g.Priority != model.Bulk {
				t.Errorf("%s sample %d: identity fields changed: %+v", label, j, g)
			}
		}

		if ev.deadline.check {
			d, ok := chain.Deadline()
			if ok != ev.deadline.has {
				t.Fatalf("%s: deadline present = %v, want %v", label, ok, ev.deadline.has)
			}
			if ok && !d.Equal(t0.Add(ev.deadline.at)) {
				t.Errorf("%s: deadline = +%s, want +%s", label, d.Sub(t0), ev.deadline.at)
			}
		}
	}
}

func runStateful(t *testing.T, tests []statefulCase) {
	t.Helper()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { runEvents(t, tc) })
	}
}

const ms = time.Millisecond

func TestDeadband(t *testing.T) {
	runStateful(t, []statefulCase{
		{
			name: "absolute", steps: []config.Transform{step("deadband", "0.5")}, in: model.KindFloat,
			events: []event{
				apply(0, f64(10.0), good(0, f64(10.0))),
				apply(time.Second, f64(10.3)),
				apply(2*time.Second, f64(10.6), good(2*time.Second, f64(10.6))),
				apply(3*time.Second, f64(10.6), out{3 * time.Second, f64(10.6), model.Uncertain}).withQuality(model.Uncertain),
				apply(4*time.Second, f64(10.6)).withQuality(model.Uncertain),
				apply(5*time.Second, f64(10.6), good(5*time.Second, f64(10.6))),
				apply(6*time.Second, f64(10.1), good(6*time.Second, f64(10.1))),
			},
		},
		{
			name: "percent of the last sent value", steps: []config.Transform{step("deadband", `"5%"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(100), good(0, f64(100))),
				apply(time.Second, f64(104)),
				apply(2*time.Second, f64(106), good(2*time.Second, f64(106))),
				apply(3*time.Second, f64(111)),
				apply(4*time.Second, f64(111.4), good(4*time.Second, f64(111.4))),
			},
		},
		{
			name: "percent of a negative value", steps: []config.Transform{step("deadband", `"5%"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(-100), good(0, f64(-100))),
				apply(time.Second, f64(-104)),
				apply(2*time.Second, f64(-106), good(2*time.Second, f64(-106))),
			},
		},
		{
			// Review Focus, P5: after a sent 0 any change passes.
			name: "percent after a sent zero", steps: []config.Transform{step("deadband", `"5%"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(0), good(0, f64(0))),
				apply(time.Second, f64(0)),
				apply(2*time.Second, f64(0.001), good(2*time.Second, f64(0.001))),
			},
		},
		{
			name: "integers keep their kind", steps: []config.Transform{step("deadband", "2")}, in: model.KindInt,
			events: []event{
				apply(0, i64(10), good(0, i64(10))),
				apply(time.Second, i64(11)),
				apply(2*time.Second, i64(12), good(2*time.Second, i64(12))),
			},
		},
		{
			// 0.3 - 0.2 is 0.09999999999999998 in float64: an exact step of
			// the threshold still passes.
			name: "exact step despite float subtraction", steps: []config.Transform{step("deadband", "0.1")}, in: model.KindFloat,
			events: []event{
				apply(0, f64(0.2), good(0, f64(0.2))),
				apply(time.Second, f64(0.29)),
				apply(2*time.Second, f64(0.3), good(2*time.Second, f64(0.3))),
				apply(3*time.Second, f64(0.2), good(3*time.Second, f64(0.2))),
			},
		},
		{
			name: "exact percent step despite float subtraction", steps: []config.Transform{step("deadband", `"50%"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(0.2), good(0, f64(0.2))),
				apply(time.Second, f64(0.3), good(time.Second, f64(0.3))),
			},
		},
		{
			name: "zero threshold sends every change", steps: []config.Transform{step("deadband", "0")}, in: model.KindFloat,
			events: []event{
				apply(0, f64(1), good(0, f64(1))),
				apply(time.Second, f64(1)),
				apply(2*time.Second, f64(1.5), good(2*time.Second, f64(1.5))),
			},
		},
		{
			// A bad sample passes through and a good one after it is a
			// quality change, so it is sent although its value did not move.
			name: "good after bad is sent", steps: []config.Transform{step("deadband", "0.5")}, in: model.KindFloat,
			events: []event{
				apply(0, f64(10), good(0, f64(10))),
				apply(time.Second, f64(99), out{time.Second, f64(99), model.Bad}).withQuality(model.Bad),
				apply(2*time.Second, f64(10), good(2*time.Second, f64(10))),
				apply(3*time.Second, f64(10.2)),
			},
		},
		{
			name: "no deadline", steps: []config.Transform{step("deadband", "1")}, in: model.KindFloat,
			events: []event{
				apply(0, f64(1), good(0, f64(1))).expectDeadline(noDeadline),
				fire(time.Hour),
			},
		},
	})
}

func TestHeartbeat(t *testing.T) {
	runStateful(t, []statefulCase{
		{
			name: "resends the last value with the clock time", steps: []config.Transform{step("heartbeat", `"10s"`)}, in: model.KindFloat,
			events: []event{
				fire(0).expectDeadline(noDeadline),
				apply(0, f64(5), good(0, f64(5))).expectDeadline(deadlineAt(10 * time.Second)),
				fire(10*time.Second - ms).expectDeadline(deadlineAt(10 * time.Second)),
				fire(10*time.Second, good(10*time.Second, f64(5))).expectDeadline(deadlineAt(20 * time.Second)),
				fire(20*time.Second, good(20*time.Second, f64(5))).expectDeadline(deadlineAt(30 * time.Second)),
				// A new sample resets the deadline.
				apply(25*time.Second, f64(6), good(25*time.Second, f64(6))).expectDeadline(deadlineAt(35 * time.Second)),
				fire(30 * time.Second),
				fire(35*time.Second, good(35*time.Second, f64(6))),
				// A late Fire resends once and re-arms from the clock time.
				fire(47*time.Second, good(47*time.Second, f64(6))).expectDeadline(deadlineAt(57 * time.Second)),
			},
		},
		{
			name: "keeps the quality and the kind", steps: []config.Transform{step("heartbeat", `"1m"`)}, in: model.KindString,
			events: []event{
				apply(0, model.StringValue("run"), out{0, model.StringValue("run"), model.Uncertain}).withQuality(model.Uncertain),
				fire(time.Minute, out{time.Minute, model.StringValue("run"), model.Uncertain}),
			},
		},
	})
}

func TestMinInterval(t *testing.T) {
	runStateful(t, []statefulCase{
		{
			// R7: the first sample of a quiet period passes at once; the
			// latest held sample leaves at the window end with its own ts.
			name: "three samples in one window", steps: []config.Transform{step("min_interval", `"1s"`)}, in: model.KindInt,
			events: []event{
				apply(0, i64(1), good(0, i64(1))).expectDeadline(noDeadline),
				apply(200*ms, i64(2)).expectDeadline(deadlineAt(time.Second)),
				apply(500*ms, i64(3)).expectDeadline(deadlineAt(time.Second)),
				fire(999 * ms),
				fire(time.Second, good(500*ms, i64(3))).expectDeadline(noDeadline),
				// The next window opened at the end of the last one.
				apply(1500*ms, i64(4)).expectDeadline(deadlineAt(2 * time.Second)),
				fire(2*time.Second, good(1500*ms, i64(4))).expectDeadline(noDeadline),
				// Nothing held in [2s, 3s): the window closed, 5 passes at once.
				apply(3500*ms, i64(5), good(3500*ms, i64(5))),
			},
		},
		{
			// A sample after the window end emits what the window held first,
			// whether or not Fire ran in time.
			name: "window end reached by a sample", steps: []config.Transform{step("min_interval", `"1s"`)}, in: model.KindInt,
			events: []event{
				apply(0, i64(1), good(0, i64(1))),
				apply(500*ms, i64(2)),
				apply(1200*ms, i64(3), good(500*ms, i64(2))).expectDeadline(deadlineAt(2 * time.Second)),
				apply(2500*ms, i64(4), good(1200*ms, i64(3))).expectDeadline(deadlineAt(3 * time.Second)),
				apply(5*time.Second, i64(5), good(2500*ms, i64(4)), good(5*time.Second, i64(5))),
			},
		},
	})
}

func TestDebounce(t *testing.T) {
	runStateful(t, []statefulCase{
		{
			name: "sends a value that stayed", steps: []config.Transform{step("debounce", `"2s"`)}, in: model.KindInt,
			events: []event{
				apply(0, i64(1)).expectDeadline(deadlineAt(2 * time.Second)),
				apply(500*ms, i64(0)).expectDeadline(deadlineAt(2500 * ms)),
				apply(time.Second, i64(1)).expectDeadline(deadlineAt(3 * time.Second)),
				fire(2999 * ms),
				fire(3*time.Second, good(time.Second, i64(1))).expectDeadline(noDeadline),
				// The value already sent is not resent.
				apply(4*time.Second, i64(1)).expectDeadline(noDeadline),
				// A change that returns to the sent value before the
				// deadline sends nothing.
				apply(5*time.Second, i64(0)).expectDeadline(deadlineAt(7 * time.Second)),
				apply(6*time.Second, i64(1)).expectDeadline(noDeadline),
				fire(7 * time.Second),
			},
		},
		{
			name: "a repeated value keeps the time of its change", steps: []config.Transform{step("debounce", `"2s"`)}, in: model.KindBool,
			events: []event{
				apply(0, boolean(true)),
				apply(time.Second, boolean(true)).expectDeadline(deadlineAt(2 * time.Second)),
				fire(2*time.Second, good(0, boolean(true))),
			},
		},
		{
			// A sample at or after the deadline sends the waiting value first,
			// whether or not Fire ran in time.
			name: "deadline reached by a sample", steps: []config.Transform{step("debounce", `"2s"`)}, in: model.KindInt,
			events: []event{
				apply(0, i64(1)),
				apply(2500*ms, i64(0), good(0, i64(1))).expectDeadline(deadlineAt(4500 * ms)),
			},
		},
	})
}

func TestDerivative(t *testing.T) {
	runStateful(t, []statefulCase{
		{
			name: "change per second", steps: []config.Transform{step("derivative", "true")}, in: model.KindFloat,
			events: []event{
				apply(0, f64(100)).expectDeadline(noDeadline),
				apply(5*time.Second, f64(110), good(5*time.Second, f64(2.0))),
				// A backward jump is a totaliser reset: nothing, new baseline.
				apply(10*time.Second, f64(50)),
				apply(15*time.Second, f64(60), good(15*time.Second, f64(2.0))),
				// No time between the samples: nothing, new baseline.
				apply(15*time.Second, f64(70)),
				apply(20*time.Second, f64(80), good(20*time.Second, f64(2.0))),
				apply(30*time.Second, f64(80), good(30*time.Second, f64(0))),
				// A timestamp before the previous one: nothing, new baseline.
				apply(25*time.Second, f64(90)),
				apply(30*time.Second, f64(100), good(30*time.Second, f64(2.0))),
			},
		},
		{
			name: "integers give a float", steps: []config.Transform{step("derivative", "true")}, in: model.KindInt,
			events: []event{
				apply(0, i64(100)),
				apply(4*time.Second, i64(102), good(4*time.Second, f64(0.5))),
			},
		},
		{
			// P6: a non-finite result is a runtime error.
			name: "non-finite result", steps: []config.Transform{step("derivative", "true")}, in: model.KindFloat,
			events: []event{
				apply(0, f64(-1.7e308)),
				apply(time.Second, f64(1.7e308), out{time.Second, f64(0), model.Bad}).failing(),
			},
		},
	})
}

// fakeStore is an in-memory Store recording every Save.
type fakeStore struct {
	value int64
	has   bool
	saves []int64
	err   error
}

func (f *fakeStore) Load() (int64, bool) { return f.value, f.has }

func (f *fakeStore) Save(v int64) error {
	f.saves = append(f.saves, v)
	if f.err != nil {
		return f.err
	}

	f.value, f.has = v, true

	return nil
}

func storeFactory(s *fakeStore) transform.StateFactory {
	return func(int, json.RawMessage) transform.Store { return s }
}

func TestEdgeCounter(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		store     *fakeStore
		events    []event
		wantSaves []int64
	}{
		{
			// R8: the first sample emits the count as a baseline.
			name: "rising", mode: `"rising"`, store: &fakeStore{},
			events: []event{
				apply(0, boolean(false), good(0, i64(0))),
				apply(time.Second, boolean(true), good(time.Second, i64(1))),
				apply(2*time.Second, boolean(false)),
				apply(3*time.Second, boolean(true), good(3*time.Second, i64(2))),
				apply(4*time.Second, boolean(true)).expectDeadline(noDeadline),
			},
			wantSaves: []int64{1, 2},
		},
		{
			name: "falling", mode: `"falling"`, store: &fakeStore{},
			events: []event{
				apply(0, boolean(true), good(0, i64(0))),
				apply(time.Second, boolean(false), good(time.Second, i64(1))),
				apply(2*time.Second, boolean(true)),
				apply(3*time.Second, boolean(false), good(3*time.Second, i64(2))),
			},
			wantSaves: []int64{1, 2},
		},
		{
			name: "both", mode: `"both"`, store: &fakeStore{},
			events: []event{
				apply(0, boolean(false), good(0, i64(0))),
				apply(time.Second, boolean(true), good(time.Second, i64(1))),
				apply(2*time.Second, boolean(false), good(2*time.Second, i64(2))),
				apply(3*time.Second, boolean(true), good(3*time.Second, i64(3))),
			},
			wantSaves: []int64{1, 2, 3},
		},
		{
			// The initial level is not an edge.
			name: "starts from the stored count", mode: `"rising"`, store: &fakeStore{value: 41, has: true},
			events: []event{
				apply(0, boolean(true), good(0, i64(41))),
				apply(time.Second, boolean(false)),
				apply(2*time.Second, boolean(true), good(2*time.Second, i64(42))),
			},
			wantSaves: []int64{42},
		},
		{
			name: "keeps the quality", mode: `"rising"`, store: &fakeStore{},
			events: []event{
				apply(0, boolean(false), out{0, i64(0), model.Uncertain}).withQuality(model.Uncertain),
				apply(time.Second, boolean(true), out{time.Second, i64(1), model.Uncertain}).withQuality(model.Uncertain),
			},
			wantSaves: []int64{1},
		},
		{
			// A failed Save is a runtime error for that sample; the count
			// stays incremented in memory.
			name: "save error", mode: `"rising"`, store: &fakeStore{err: errors.New("disk full")},
			events: []event{
				apply(0, boolean(false), good(0, i64(0))),
				apply(time.Second, boolean(true), out{time.Second, i64(0), model.Bad}).failing(),
				apply(2*time.Second, boolean(false)),
				apply(3*time.Second, boolean(true), out{3 * time.Second, i64(0), model.Bad}).failing(),
			},
			wantSaves: []int64{1, 2},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runEvents(t, statefulCase{
				steps: []config.Transform{step("edge_counter", tc.mode)}, in: model.KindBool,
				state: storeFactory(tc.store), events: tc.events,
			})

			if fmt.Sprint(tc.store.saves) != fmt.Sprint(tc.wantSaves) {
				t.Errorf("saves = %v, want %v", tc.store.saves, tc.wantSaves)
			}
		})
	}
}

func TestEdgeCounterWithoutPersistence(t *testing.T) {
	runEvents(t, statefulCase{
		steps: []config.Transform{step("edge_counter", `"rising"`)}, in: model.KindBool,
		events: []event{
			apply(0, boolean(false), good(0, i64(0))),
			apply(time.Second, boolean(true), good(time.Second, i64(1))),
			apply(2*time.Second, boolean(false)),
			apply(3*time.Second, boolean(true), good(3*time.Second, i64(2))),
		},
	})
}

func TestEdgeCounterSaveErrorDoesNotEchoTheCause(t *testing.T) {
	chain, err := transform.Compile([]config.Transform{step("edge_counter", `"rising"`)}, model.KindBool,
		storeFactory(&fakeStore{err: errors.New("/SECRET/path")}))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	chain.Apply(sample(boolean(false)))

	_, errs := chain.Apply(sample(boolean(true)))
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one", errs)
	}

	if strings.Contains(errs[0].Error(), "SECRET") {
		t.Errorf("error %q echoes the cause", errs[0])
	}

	if errs[0].Error() != "step 1 (edge_counter): count cannot be saved" {
		t.Errorf("error = %q", errs[0])
	}
}

func TestEdgeCounterAsksTheFactoryForItsStore(t *testing.T) {
	var (
		gotPosition  int
		gotCanonical string
	)

	factory := func(position int, canonical json.RawMessage) transform.Store {
		gotPosition, gotCanonical = position, string(canonical)

		return &fakeStore{}
	}

	_, err := transform.Compile([]config.Transform{
		step("map", `{"table":{"1":true},"default":false}`),
		step("edge_counter", ` "both" `),
	}, model.KindInt, factory)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if gotPosition != 2 || gotCanonical != `{"edge_counter":"both"}` {
		t.Errorf("factory called with (%d, %s), want (2, {\"edge_counter\":\"both\"})", gotPosition, gotCanonical)
	}
}

func TestBadInputThroughStatefulTransforms(t *testing.T) {
	bad := func(at time.Duration, v model.Value, kind model.ValueKind) event {
		zero := model.Value{Kind: kind}
		if v.Kind == kind {
			zero = v
		}

		return apply(at, v, out{at, zero, model.Bad}).withQuality(model.Bad)
	}

	runStateful(t, []statefulCase{
		{
			// R9: a bad sample leaving the step is the last value; the
			// heartbeat resends it bad, with the clock time.
			name: "heartbeat resends a bad sample", steps: []config.Transform{step("heartbeat", `"10s"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(5), good(0, f64(5))),
				bad(5*time.Second, f64(7), model.KindFloat).expectDeadline(deadlineAt(15 * time.Second)),
				fire(10 * time.Second),
				fire(15*time.Second, out{15 * time.Second, f64(7), model.Bad}),
				apply(17*time.Second, f64(6), good(17*time.Second, f64(6))).expectDeadline(deadlineAt(27 * time.Second)),
				fire(27*time.Second, good(27*time.Second, f64(6))),
			},
		},
		{
			// R10: a bad sample passes at once and drops the held sample.
			name: "min_interval drops the held sample", steps: []config.Transform{step("min_interval", `"1s"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(1), good(0, f64(1))),
				apply(500*ms, f64(2)).expectDeadline(deadlineAt(time.Second)),
				bad(700*ms, f64(9), model.KindFloat).expectDeadline(noDeadline),
				fire(time.Second),
				// The window held nothing and closed: 3 passes at once.
				apply(1500*ms, f64(3), good(1500*ms, f64(3))),
			},
		},
		{
			// A held sample whose window ended before the bad sample leaves
			// first, whether or not Fire ran in time.
			name: "min_interval window ended before the bad sample", steps: []config.Transform{step("min_interval", `"1s"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(1), good(0, f64(1))),
				apply(500*ms, f64(2)),
				apply(1200*ms, f64(9), good(500*ms, f64(2)), out{1200 * ms, f64(9), model.Bad}).withQuality(model.Bad).expectDeadline(noDeadline),
			},
		},
		{
			// R10: a bad sample cancels the waiting value, which was not seen
			// unchanged for the delay.
			name: "debounce cancels the waiting value", steps: []config.Transform{step("debounce", `"2s"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(1)),
				bad(time.Second, f64(2), model.KindFloat).expectDeadline(noDeadline),
				fire(2 * time.Second),
			},
		},
		{
			name: "debounce deadline reached before the bad sample", steps: []config.Transform{step("debounce", `"2s"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(1)),
				apply(2500*ms, f64(2), good(0, f64(1)), out{2500 * ms, f64(2), model.Bad}).withQuality(model.Bad).expectDeadline(noDeadline),
			},
		},
		{
			// After a bad sample the last value that left is bad, so the
			// value sent before it is sent again once it stays.
			name: "debounce resends after bad", steps: []config.Transform{step("debounce", `"2s"`)}, in: model.KindFloat,
			events: []event{
				apply(0, f64(1)),
				fire(2*time.Second, good(0, f64(1))),
				bad(3*time.Second, f64(0), model.KindFloat),
				apply(4*time.Second, f64(1)).expectDeadline(deadlineAt(6 * time.Second)),
				fire(6*time.Second, good(4*time.Second, f64(1))),
			},
		},
		{
			name: "derivative", steps: []config.Transform{step("derivative", "true")}, in: model.KindFloat,
			events: []event{
				apply(0, f64(100)),
				bad(time.Second, f64(1000), model.KindFloat),
				apply(5*time.Second, f64(110), good(5*time.Second, f64(2))),
			},
		},
		{
			name: "edge_counter", steps: []config.Transform{step("edge_counter", `"rising"`)}, in: model.KindBool,
			events: []event{
				apply(0, boolean(false), good(0, i64(0))),
				bad(time.Second, boolean(true), model.KindInt),
				apply(2*time.Second, boolean(true), good(2*time.Second, i64(1))),
			},
		},
	})
}

func TestStatefulCompile(t *testing.T) {
	tests := []struct {
		name    string
		in      model.ValueKind
		step    config.Transform
		wantErr string
		wantOut model.ValueKind
	}{
		{"deadband keeps int", model.KindInt, step("deadband", "1"), "", model.KindInt},
		{"deadband keeps float", model.KindFloat, step("deadband", `"5%"`), "", model.KindFloat},
		{"deadband fractional percent", model.KindFloat, step("deadband", `"0.5%"`), "", model.KindFloat},
		{"deadband on bool", model.KindBool, step("deadband", "1"), "step 1 (deadband): accepts number, got boolean", 0},
		{"deadband negative", model.KindFloat, step("deadband", "-1"), "step 1 (deadband): parameter must be a non-negative number or a percentage such as \"5%\"", 0},
		{"deadband text", model.KindFloat, step("deadband", `"SECRET"`), "step 1 (deadband): parameter must be a non-negative number or a percentage such as \"5%\"", 0},
		{"deadband percent without number", model.KindFloat, step("deadband", `"%"`), "step 1 (deadband): parameter must be a non-negative number or a percentage such as \"5%\"", 0},
		{"deadband negative percent", model.KindFloat, step("deadband", `"-5%"`), "step 1 (deadband): parameter must be a non-negative number or a percentage such as \"5%\"", 0},
		{"deadband numeric text", model.KindFloat, step("deadband", `"5"`), "step 1 (deadband): parameter must be a non-negative number or a percentage such as \"5%\"", 0},
		{"heartbeat keeps string", model.KindString, step("heartbeat", `"10s"`), "", model.KindString},
		{"heartbeat keeps bool", model.KindBool, step("heartbeat", `"1m30s"`), "", model.KindBool},
		{"heartbeat number", model.KindFloat, step("heartbeat", "10"), "step 1 (heartbeat): parameter must be a positive duration such as 500ms, 2s or 1m30s", 0},
		{"heartbeat zero", model.KindFloat, step("heartbeat", `"0s"`), "step 1 (heartbeat): parameter must be a positive duration such as 500ms, 2s or 1m30s", 0},
		{"heartbeat negative", model.KindFloat, step("heartbeat", `"-1s"`), "step 1 (heartbeat): parameter must be a positive duration such as 500ms, 2s or 1m30s", 0},
		{"heartbeat unit outside the schema", model.KindFloat, step("heartbeat", `"10us"`), "step 1 (heartbeat): parameter must be a positive duration such as 500ms, 2s or 1m30s", 0},
		{"heartbeat overflow", model.KindFloat, step("heartbeat", `"9999999999h"`), "step 1 (heartbeat): parameter must be a positive duration such as 500ms, 2s or 1m30s", 0},
		{"min_interval keeps int", model.KindInt, step("min_interval", `"500ms"`), "", model.KindInt},
		{"min_interval text", model.KindInt, step("min_interval", `"SECRET"`), "step 1 (min_interval): parameter must be a positive duration such as 500ms, 2s or 1m30s", 0},
		{"debounce keeps bool", model.KindBool, step("debounce", `"2s"`), "", model.KindBool},
		{"debounce null", model.KindBool, step("debounce", `null`), "step 1 (debounce): parameter must be a positive duration such as 500ms, 2s or 1m30s", 0},
		{"derivative gives float", model.KindInt, step("derivative", "true"), "", model.KindFloat},
		{"derivative on string", model.KindString, step("derivative", "true"), "step 1 (derivative): accepts number, got string", 0},
		{"derivative false", model.KindFloat, step("derivative", "false"), "step 1 (derivative): parameter must be true", 0},
		{"edge_counter gives int", model.KindBool, step("edge_counter", `"falling"`), "", model.KindInt},
		{"edge_counter on int", model.KindInt, step("edge_counter", `"rising"`), "step 1 (edge_counter): accepts boolean, got integer", 0},
		{"edge_counter bad mode", model.KindBool, step("edge_counter", `"SECRET"`), "step 1 (edge_counter): parameter must be rising, falling or both", 0},
		{"edge_counter true", model.KindBool, step("edge_counter", "true"), "step 1 (edge_counter): parameter must be rising, falling or both", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain, err := transform.Compile([]config.Transform{tc.step}, tc.in, nil)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Compile succeeded, want error %q", tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Errorf("Compile error = %q, want %q", err.Error(), tc.wantErr)
				}
				if strings.Contains(err.Error(), "SECRET") {
					t.Errorf("error %q echoes a document value", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if got := chain.OutKind(); got != tc.wantOut {
				t.Errorf("OutKind = %v, want %v", got, tc.wantOut)
			}
		})
	}
}

// TestTimeDrivenStepFeedsTheNextStep checks that what a step emits on Fire
// flows through the steps after it.
func TestTimeDrivenStepFeedsTheNextStep(t *testing.T) {
	runEvents(t, statefulCase{
		steps: []config.Transform{step("debounce", `"2s"`), step("edge_counter", `"rising"`)}, in: model.KindBool,
		events: []event{
			apply(0, boolean(false)),
			fire(2*time.Second, good(0, i64(0))),
			apply(3*time.Second, boolean(true)),
			fire(5*time.Second, good(3*time.Second, i64(1))),
		},
	})
}

// TestHeartbeatAfterAHoldBackStep checks that a sample a hold-back step emits
// on Fire, with an old ts, arms the heartbeat from the clock time instead of
// being resent at once.
func TestHeartbeatAfterAHoldBackStep(t *testing.T) {
	runEvents(t, statefulCase{
		steps: []config.Transform{step("debounce", `"2s"`), step("heartbeat", `"1s"`)}, in: model.KindInt,
		events: []event{
			apply(0, i64(1)),
			fire(2*time.Second, good(0, i64(1))).expectDeadline(deadlineAt(3 * time.Second)),
			fire(3*time.Second, good(3*time.Second, i64(1))).expectDeadline(deadlineAt(4 * time.Second)),
		},
	})
}
