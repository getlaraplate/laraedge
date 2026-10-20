// Package transform compiles the transform chain of a point and applies it to
// samples. A chain is type-checked when it is compiled, starting from the
// value kind of the point's decoding type, so a document with a chain that
// cannot work is refused before it is applied.
//
// Messages never carry a parameter value, a sample value or a name taken from
// the document that does not have the shape of a catalogue key: a secret
// written in the wrong place must not leak through an error.
package transform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
)

// KindOf returns the value kind a point with the given decoding type
// produces: bool gives a boolean, the integer types an integer, float32 and
// float64 a float, string a string. A point without a type (or with a type
// the schema does not define) is a number and gives a float.
func KindOf(pointType string) model.ValueKind {
	switch pointType {
	case "bool":
		return model.KindBool
	case "int16", "uint16", "int32", "uint32", "int64", "uint64":
		return model.KindInt
	case "string":
		return model.KindString
	default:
		return model.KindFloat
	}
}

// Store keeps the persisted state of one step: a single integer. A step that
// needs persistence (edge_counter) asks the StateFactory for one.
type Store interface {
	// Load returns the saved value and whether there was one.
	Load() (int64, bool)
	// Save replaces the saved value.
	Save(int64) error
}

// StateFactory returns the Store of the step at position (1-based) whose
// canonical JSON is canonical. A nil StateFactory means no persistence: unit
// tests and `config test`.
type StateFactory func(position int, canonical json.RawMessage) Store

// Step is one compiled transform of a chain. A step belongs to one chain
// position and is not safe for concurrent use: the processor drives a chain
// from one goroutine.
type Step interface {
	// OutKind is the kind of the values the step produces.
	OutKind() model.ValueKind
	// Apply transforms one sample. A stateless step returns zero or one
	// sample; a step that holds samples back returns none and emits later.
	// A time-driven step whose deadline the sample's ts reached first returns
	// what was due, so it may return more than one sample.
	// An error is a runtime error of the step: the chain turns it into a bad
	// sample. The error text must not contain the sample value.
	Apply(s model.Sample) (out []model.Sample, err error)
	// Deadline returns when the step wants Fire to be called, if it does.
	Deadline() (time.Time, bool)
	// Fire emits what is due at now. Stateless steps emit nothing.
	Fire(now time.Time) []model.Sample
}

// StepError is the error of one step: a refusal at compile time or a runtime
// error. Its text names the position and the transform, never a value.
type StepError struct {
	// Position is the 1-based position of the step in the chain.
	Position int
	// Name is the transform name; empty when it is not safe to show.
	Name string
	// Err is the cause; its text is fixed.
	Err error
}

// Error implements error.
func (e *StepError) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("step %d: %s", e.Position, e.Err)
	}

	return fmt.Sprintf("step %d (%s): %s", e.Position, e.Name, e.Err)
}

// Unwrap returns the cause.
func (e *StepError) Unwrap() error { return e.Err }

// badObserver is implemented by a step that must know a bad sample passed
// through it uncomputed: deadband (a quality change is a reason to send),
// heartbeat (the bad sample is the last value), debounce and min_interval
// (the bad sample cancels what they hold). It returns what was due at the
// bad sample's ts, which leaves the step before it.
type badObserver interface {
	observeBad(s model.Sample) []model.Sample
}

// clockObserver is implemented by a step that must know the clock time of a
// Fire before samples emitted by earlier steps in that Fire reach it.
type clockObserver interface {
	observeNow(now time.Time)
}

// deviceObserver is implemented by a step that must know its device died
// (ruling R14): heartbeat stops resending, min_interval releases its held
// sample, debounce drops its waiting value. It returns what leaves the step
// before the death.
type deviceObserver interface {
	deviceDown() []model.Sample
}

type stage struct {
	name string
	step Step
}

// Chain is a compiled transform chain. It is not safe for concurrent use.
type Chain struct {
	stages []stage
	out    model.ValueKind
}

func newChain(names []string, steps []Step, in model.ValueKind) *Chain {
	c := &Chain{out: in, stages: make([]stage, len(steps))}

	for i, s := range steps {
		c.stages[i] = stage{name: names[i], step: s}
		c.out = s.OutKind()
	}

	return c
}

// OutKind is the kind of the values the chain produces, which is the input
// kind for an empty chain.
func (c *Chain) OutKind() model.ValueKind { return c.out }

// Apply runs a sample through the chain. A sample produced by step k flows
// through steps k+1 to n. A bad sample is not computed: it passes through
// unchanged. A runtime error of a step yields a bad sample of that step's
// output kind with the zero value, and the error in the second result. Every
// sample leaving the chain has a value of kind OutKind (ruling R5): a bad
// sample of another kind gets the zero value of OutKind, a good integer
// leaving a float chain becomes a float (see exit).
func (c *Chain) Apply(s model.Sample) ([]model.Sample, []error) {
	var errs []error

	out := c.exit(c.run(0, []model.Sample{s}, &errs), &errs)

	return out, errs
}

// Deadline returns the earliest deadline of the steps.
func (c *Chain) Deadline() (time.Time, bool) {
	var (
		earliest time.Time
		found    bool
	)

	for _, st := range c.stages {
		d, ok := st.step.Deadline()
		if ok && (!found || d.Before(earliest)) {
			earliest, found = d, true
		}
	}

	return earliest, found
}

// Fire lets every step emit what is due at now; what a step emits flows
// through the steps after it. Every step learns now before any sample of
// this Fire reaches it.
func (c *Chain) Fire(now time.Time) ([]model.Sample, []error) {
	var (
		out  []model.Sample
		errs []error
	)

	for _, st := range c.stages {
		if o, ok := st.step.(clockObserver); ok {
			o.observeNow(now)
		}
	}

	for i, st := range c.stages {
		if emitted := st.step.Fire(now); len(emitted) > 0 {
			out = append(out, c.run(i+1, emitted, &errs)...)
		}
	}

	return c.exit(out, &errs), errs
}

// deviceDown tells every step, in chain order, that the chain's device died;
// what a step releases flows through the steps after it before they are
// told, so a sample min_interval releases re-arms a later heartbeat, which
// its own notice then disarms.
func (c *Chain) deviceDown() ([]model.Sample, []error) {
	var (
		out  []model.Sample
		errs []error
	)

	for i, st := range c.stages {
		o, ok := st.step.(deviceObserver)
		if !ok {
			continue
		}

		if released := o.deviceDown(); len(released) > 0 {
			out = append(out, c.run(i+1, released, &errs)...)
		}
	}

	return c.exit(out, &errs), errs
}

// exit enforces the chain-exit invariant: every sample leaving the chain has a
// value of kind OutKind. A bad sample of another kind gets the zero value of
// OutKind (its other fields are kept); a good integer leaving a float chain,
// for example an untyped point whose driver emits integers, becomes a float.
// Any other mismatch is a defect upstream: the sample becomes bad with the
// zero value and the error is reported.
func (c *Chain) exit(samples []model.Sample, errs *[]error) []model.Sample {
	for i := range samples {
		s := &samples[i]
		if s.Value.Kind == c.out {
			continue
		}

		switch {
		case s.Quality == model.Bad:
		case c.out == model.KindFloat && s.Value.Kind == model.KindInt:
			s.Value = model.Value{Kind: model.KindFloat, Float: float64(s.Value.Int)}

			continue
		default:
			s.Quality = model.Bad

			*errs = append(*errs, errValueKind)
		}

		s.Value = model.Value{Kind: c.out}
	}

	return samples
}

// run flows samples through the stages from index from.
func (c *Chain) run(from int, in []model.Sample, errs *[]error) []model.Sample {
	cur := in

	for i := from; i < len(c.stages) && len(cur) > 0; i++ {
		st := c.stages[i]

		var next []model.Sample

		for _, s := range cur {
			if s.Quality == model.Bad {
				if o, ok := st.step.(badObserver); ok {
					next = append(next, o.observeBad(s)...)
				}

				next = append(next, s)

				continue
			}

			out, err := st.step.Apply(s)
			if err != nil {
				*errs = append(*errs, &StepError{Position: i + 1, Name: st.name, Err: err})

				s.Value = model.Value{Kind: st.step.OutKind()}
				s.Quality = model.Bad
				next = append(next, s)

				continue
			}

			next = append(next, out...)
		}

		cur = next
	}

	return cur
}

// Compile type-checks and builds a chain for a point whose values have kind
// in. The error names the position and the transform of the first step that
// cannot be built, and never a parameter value.
func Compile(steps []config.Transform, in model.ValueKind, newState StateFactory) (*Chain, error) {
	built := make([]Step, 0, len(steps))
	names := make([]string, 0, len(steps))
	kind := in

	for i, t := range steps {
		position := i + 1
		fail := func(err error) error {
			return &StepError{Position: position, Name: shownName(t.Name), Err: err}
		}

		def, ok := catalogue[t.Name]
		if !ok {
			return nil, fail(errors.New("unknown transform"))
		}

		if !def.accepts.has(kind) {
			return nil, fail(fmt.Errorf("accepts %s, got %s", def.accepts, kindName(kind)))
		}

		// A parameter that is not valid JSON cannot be marshalled; the
		// builder then refuses it with its own fixed message.
		canonical, marshalErr := json.Marshal(map[string]json.RawMessage{t.Name: t.Params})

		step, err := def.build(buildContext{
			In:        kind,
			Position:  position,
			Params:    t.Params,
			Canonical: canonical,
			NewState:  newState,
		})
		if err != nil {
			return nil, fail(err)
		}

		if marshalErr != nil {
			return nil, fail(errors.New("parameter is not valid JSON"))
		}

		built = append(built, step)
		names = append(names, t.Name)
		kind = step.OutKind()
	}

	return newChain(names, built, in), nil
}

// buildContext is what a builder gets to build one step.
type buildContext struct {
	// In is the kind of the values reaching the step.
	In model.ValueKind
	// Position is the 1-based position of the step in the chain.
	Position int
	// Params are the step's parameters as JSON.
	Params json.RawMessage
	// Canonical is the compact JSON of the whole step, the key of its state.
	Canonical json.RawMessage
	// NewState is the state factory of Compile; nil means no persistence.
	NewState StateFactory
}

// definition describes one transform of the catalogue.
type definition struct {
	// accepts is the set of input kinds the transform takes.
	accepts accept
	// build parses the parameters and returns the step. Its errors are fixed
	// messages that never contain a parameter value.
	build func(buildContext) (Step, error)
}

// catalogue maps a transform name to its definition. It is filled by register
// calls in init functions and only read afterwards, so a new transform is one
// register call and Compile does not change.
var catalogue = map[string]definition{}

// register adds a transform to the catalogue. A duplicate name is a
// programming error caught by every test run.
func register(name string, d definition) {
	if _, dup := catalogue[name]; dup {
		panic("transform: duplicate catalogue entry " + name)
	}

	catalogue[name] = d
}

// accept is a set of input kinds.
type accept uint8

const (
	acceptFloat accept = 1 << iota
	acceptInt
	acceptBool
	acceptString

	acceptNumber = acceptFloat | acceptInt
	acceptAny    = acceptFloat | acceptInt | acceptBool | acceptString
)

func (a accept) has(k model.ValueKind) bool {
	switch k {
	case model.KindFloat:
		return a&acceptFloat != 0
	case model.KindInt:
		return a&acceptInt != 0
	case model.KindBool:
		return a&acceptBool != 0
	case model.KindString:
		return a&acceptString != 0
	default:
		return false
	}
}

// String names the set the way the messages do.
func (a accept) String() string {
	switch a {
	case acceptNumber:
		return "number"
	case acceptAny:
		return "any value"
	case acceptFloat:
		return "float"
	case acceptInt:
		return "integer"
	case acceptBool:
		return "boolean"
	case acceptString:
		return "string"
	default:
		return "other kinds"
	}
}

func kindName(k model.ValueKind) string {
	switch k {
	case model.KindFloat:
		return "float"
	case model.KindInt:
		return "integer"
	case model.KindBool:
		return "boolean"
	case model.KindString:
		return "string"
	default:
		return "unknown kind"
	}
}

// safeName has the shape of a catalogue key. core's docrules holds the same
// rule for document keys; the package does not import it (ruling R3).
var safeName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// shownName returns name when it may be named in a message, else "".
func shownName(name string) string {
	if safeName.MatchString(name) {
		return name
	}

	return ""
}

// --- parameter parsing ---
//
// Every parser returns a fixed-message error: the document value is never
// echoed, and neither is the text of a JSON decoding error.

// decodeValue decodes exactly one JSON value, numbers as json.Number.
func decodeValue(raw json.RawMessage) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}

	return v, true
}

// finiteFloat converts a decoded JSON number to a finite float.
func finiteFloat(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}

	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}

	return f, true
}

// integerIn converts a decoded JSON number to an integer within [lo, hi].
func integerIn(v any, lo, hi int64) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}

	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(n.String(), 64)
		if ferr != nil || f != math.Trunc(f) || f < float64(lo) || f > float64(hi) {
			return 0, false
		}

		i = int64(f)
	}

	return i, i >= lo && i <= hi
}

var (
	errNumber  = errors.New("parameter must be a number")
	errTrue    = errors.New("parameter must be true")
	errObject  = errors.New("parameter must be an object")
	errUnknown = errors.New("unknown parameter")
)

// numberParam parses a parameter that is one finite number.
func numberParam(raw json.RawMessage) (float64, error) {
	v, ok := decodeValue(raw)
	if !ok {
		return 0, errNumber
	}

	f, ok := finiteFloat(v)
	if !ok {
		return 0, errNumber
	}

	return f, nil
}

// integerParam parses a parameter that is one integer in [lo, hi].
func integerParam(raw json.RawMessage, lo, hi int64) (int64, error) {
	v, ok := decodeValue(raw)
	if ok {
		if i, ok := integerIn(v, lo, hi); ok {
			return i, nil
		}
	}

	return 0, fmt.Errorf("parameter must be an integer from %d to %d", lo, hi)
}

// trueParam checks that the parameter is the JSON value true.
func trueParam(raw json.RawMessage) error {
	if v, ok := decodeValue(raw); ok {
		if b, isBool := v.(bool); isBool && b {
			return nil
		}
	}

	return errTrue
}

// objectParam decodes an object parameter and refuses keys outside allowed.
func objectParam(raw json.RawMessage, allowed ...string) (map[string]any, error) {
	v, ok := decodeValue(raw)
	if !ok {
		return nil, errObject
	}

	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errObject
	}

	for key := range obj {
		known := false

		for _, a := range allowed {
			if key == a {
				known = true

				break
			}
		}

		if !known {
			return nil, errUnknown
		}
	}

	return obj, nil
}
