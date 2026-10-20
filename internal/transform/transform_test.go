package transform_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/transform"
)

var t0 = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

func step(name, params string) config.Transform {
	return config.Transform{Name: name, Params: json.RawMessage(params)}
}

func sample(v model.Value) model.Sample {
	return model.Sample{
		Device: "press1", Signal: "x", TS: t0,
		Value: v, Quality: model.Good, Priority: model.Bulk,
	}
}

func TestKindOf(t *testing.T) {
	tests := []struct {
		pointType string
		want      model.ValueKind
	}{
		{"bool", model.KindBool},
		{"int16", model.KindInt},
		{"uint16", model.KindInt},
		{"int32", model.KindInt},
		{"uint32", model.KindInt},
		{"int64", model.KindInt},
		{"uint64", model.KindInt},
		{"float32", model.KindFloat},
		{"float64", model.KindFloat},
		{"string", model.KindString},
		{"", model.KindFloat},
	}

	for _, tc := range tests {
		t.Run("type "+tc.pointType, func(t *testing.T) {
			if got := transform.KindOf(tc.pointType); got != tc.want {
				t.Errorf("KindOf(%q) = %v, want %v", tc.pointType, got, tc.want)
			}
		})
	}
}

func TestCompileTypeCheck(t *testing.T) {
	tests := []struct {
		name    string
		in      model.ValueKind
		steps   []config.Transform
		wantErr string
		wantOut model.ValueKind
	}{
		{"bit after float", model.KindFloat, []config.Transform{step("bit", "3")}, "step 1 (bit): accepts integer, got float", 0},
		{"bit after int", model.KindInt, []config.Transform{step("bit", "3")}, "", model.KindBool},
		{"scale then bit", model.KindInt, []config.Transform{step("scale", "0.1"), step("bit", "3")}, "step 2 (bit): accepts integer, got float", 0},
		{"scale on string", model.KindString, []config.Transform{step("scale", "2")}, "step 1 (scale): accepts number, got string", 0},
		{"trim on int", model.KindInt, []config.Transform{step("trim", "true")}, "step 1 (trim): accepts string, got integer", 0},
		{"bcd on bool", model.KindBool, []config.Transform{step("bcd", "true")}, "step 1 (bcd): accepts integer, got boolean", 0},
		{"scale yields float", model.KindInt, []config.Transform{step("scale", "2")}, "", model.KindFloat},
		{"round keeps int", model.KindInt, []config.Transform{step("round", "1")}, "", model.KindInt},
		{"round keeps float", model.KindFloat, []config.Transform{step("round", "1")}, "", model.KindFloat},
		{"map string values", model.KindInt, []config.Transform{step("map", `{"table":{"1":"run"}}`)}, "", model.KindString},
		{"map int values from bool", model.KindBool, []config.Transform{step("map", `{"table":{"true":1,"false":0}}`)}, "", model.KindInt},
		{"map float values", model.KindString, []config.Transform{step("map", `{"table":{"a":1.5},"default":2.5}`)}, "", model.KindFloat},
		{"map bool values", model.KindInt, []config.Transform{step("map", `{"table":{"1":true},"default":false}`)}, "", model.KindBool},
		{"map mixed kinds", model.KindInt, []config.Transform{step("map", `{"table":{"1":"run","2":3}}`)}, "step 1 (map): table values and default must share one kind", 0},
		{"map default of another kind", model.KindInt, []config.Transform{step("map", `{"table":{"1":"run"},"default":0}`)}, "step 1 (map): table values and default must share one kind", 0},
		{"map int and float unify to float", model.KindInt, []config.Transform{step("map", `{"table":{"1":1,"2":2.5}}`)}, "", model.KindFloat},
		{"map whole float literal and fraction", model.KindInt, []config.Transform{step("map", `{"table":{"1":1.0,"2":1.5}}`)}, "", model.KindFloat},
		{"map all integers", model.KindInt, []config.Transform{step("map", `{"table":{"1":1,"2":2},"default":3}`)}, "", model.KindInt},
		{"map number and string", model.KindInt, []config.Transform{step("map", `{"table":{"1":1,"2":"x"}}`)}, "step 1 (map): table values and default must share one kind", 0},
		{"map bool and number", model.KindInt, []config.Transform{step("map", `{"table":{"1":true},"default":0}`)}, "step 1 (map): table values and default must share one kind", 0},
		{"map without values", model.KindInt, []config.Transform{step("map", `{"table":{}}`)}, "step 1 (map): table has no value", 0},
		{"valid_range keeps kind", model.KindInt, []config.Transform{step("valid_range", `{"max":100}`)}, "", model.KindInt},
		{"regex default string", model.KindString, []config.Transform{step("regex_extract", `{"pattern":"W=(.*)"}`)}, "", model.KindString},
		{"regex integer", model.KindString, []config.Transform{step("regex_extract", `{"pattern":"W=(.*)","as":"integer"}`)}, "", model.KindInt},
		{"regex float", model.KindString, []config.Transform{step("regex_extract", `{"pattern":"W=(.*)","as":"float"}`)}, "", model.KindFloat},
		{"empty chain", model.KindBool, nil, "", model.KindBool},
		{"chain after chain", model.KindString, []config.Transform{
			step("trim", "true"), step("regex_extract", `{"pattern":"([0-9]+)","as":"integer"}`), step("bcd", "true"), step("bit", "0"),
		}, "", model.KindBool},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain, err := transform.Compile(tc.steps, tc.in, nil)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Compile succeeded, want error %q", tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Errorf("Compile error = %q, want %q", err.Error(), tc.wantErr)
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

func TestCompileRefusesBadParameters(t *testing.T) {
	tests := []struct {
		name    string
		in      model.ValueKind
		step    config.Transform
		wantErr string
	}{
		{"unknown transform", model.KindFloat, step("frobnicate", "1"), "step 1 (frobnicate): unknown transform"},
		{"scale text", model.KindFloat, step("scale", `"abc"`), "step 1 (scale): parameter must be a number"},
		{"scale numeric text", model.KindFloat, step("scale", `"2"`), "step 1 (scale): parameter must be a number"},
		{"scale null", model.KindFloat, step("scale", `null`), "step 1 (scale): parameter must be a number"},
		{"scale empty", model.KindFloat, step("scale", ``), "step 1 (scale): parameter must be a number"},
		{"scale trailing data", model.KindFloat, step("scale", `2 3`), "step 1 (scale): parameter must be a number"},
		{"offset object", model.KindFloat, step("offset", `{}`), "step 1 (offset): parameter must be a number"},
		{"round too many", model.KindFloat, step("round", "11"), "step 1 (round): parameter must be an integer from 0 to 10"},
		{"round negative", model.KindFloat, step("round", "-1"), "step 1 (round): parameter must be an integer from 0 to 10"},
		{"round fraction", model.KindFloat, step("round", "1.5"), "step 1 (round): parameter must be an integer from 0 to 10"},
		{"bit too high", model.KindInt, step("bit", "64"), "step 1 (bit): parameter must be an integer from 0 to 63"},
		{"bcd false", model.KindInt, step("bcd", "false"), "step 1 (bcd): parameter must be true"},
		{"trim number", model.KindString, step("trim", "1"), "step 1 (trim): parameter must be true"},
		{"map not object", model.KindInt, step("map", `[1]`), "step 1 (map): parameter must be an object"},
		{"map unknown key", model.KindInt, step("map", `{"table":{"1":"a"},"zzz":1}`), "step 1 (map): unknown parameter"},
		{"map without table", model.KindInt, step("map", `{"default":"a"}`), "step 1 (map): parameter table is required"},
		{"map table not object", model.KindInt, step("map", `{"table":[1]}`), "step 1 (map): parameter table must be an object"},
		{"map null value", model.KindInt, step("map", `{"table":{"1":null}}`), "step 1 (map): table values and default must be booleans, numbers or strings"},
		{"map object value", model.KindInt, step("map", `{"table":{"1":{}}}`), "step 1 (map): table values and default must be booleans, numbers or strings"},
		{"valid_range empty", model.KindInt, step("valid_range", `{}`), "step 1 (valid_range): at least one of min and max is required"},
		{"valid_range inverted", model.KindInt, step("valid_range", `{"min":5,"max":1}`), "step 1 (valid_range): min must not exceed max"},
		{"valid_range text bound", model.KindInt, step("valid_range", `{"max":"x"}`), "step 1 (valid_range): parameter max must be a number"},
		{"valid_range unknown key", model.KindInt, step("valid_range", `{"max":1,"top":2}`), "step 1 (valid_range): unknown parameter"},
		{"regex without pattern", model.KindString, step("regex_extract", `{}`), "step 1 (regex_extract): parameter pattern is required"},
		{"regex pattern number", model.KindString, step("regex_extract", `{"pattern":3}`), "step 1 (regex_extract): parameter pattern must be a string"},
		{"regex bad pattern", model.KindString, step("regex_extract", `{"pattern":"(SECRET"}`), "step 1 (regex_extract): pattern is not a valid RE2 expression"},
		{"regex group out of range", model.KindString, step("regex_extract", `{"pattern":"(a)","group":2}`), "step 1 (regex_extract): group does not exist in the pattern"},
		{"regex group negative", model.KindString, step("regex_extract", `{"pattern":"(a)","group":-1}`), "step 1 (regex_extract): parameter group must be a non-negative integer"},
		{"regex bad as", model.KindString, step("regex_extract", `{"pattern":"(a)","as":"SECRET"}`), "step 1 (regex_extract): parameter as must be string, integer or float"},
		{"regex unknown key", model.KindString, step("regex_extract", `{"pattern":"(a)","SECRET":1}`), "step 1 (regex_extract): unknown parameter"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := transform.Compile([]config.Transform{tc.step}, tc.in, nil)
			if err == nil {
				t.Fatalf("Compile succeeded, want error %q", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("Compile error = %q, want %q", err.Error(), tc.wantErr)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error %q echoes a document value", err)
			}
		})
	}
}

func TestCompileDoesNotEchoUnsafeTransformName(t *testing.T) {
	_, err := transform.Compile([]config.Transform{step("sk-live-SECRET value!", "1")}, model.KindFloat, nil)
	if err == nil {
		t.Fatal("Compile succeeded for an unknown transform")
	}
	if err.Error() != "step 1: unknown transform" {
		t.Errorf("error = %q", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error %q echoes the transform name", err)
	}
}

func TestCompileErrorIsStepError(t *testing.T) {
	_, err := transform.Compile([]config.Transform{step("trim", "true"), step("scale", "x")}, model.KindString, nil)

	var se *transform.StepError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a *StepError", err)
	}
	if se.Position != 2 || se.Name != "scale" {
		t.Errorf("StepError = step %d (%s), want step 2 (scale)", se.Position, se.Name)
	}
}

// fakeStep is a fake step: it emits one sample per Apply, recording what it
// saw, and a time-driven sample on Fire.
type fakeStep struct {
	out      model.ValueKind
	apply    func(model.Sample) ([]model.Sample, error)
	deadline time.Time
	fire     []model.Sample
	fired    int
}

func (f *fakeStep) OutKind() model.ValueKind { return f.out }

func (f *fakeStep) Apply(s model.Sample) ([]model.Sample, error) { return f.apply(s) }

func (f *fakeStep) Deadline() (time.Time, bool) { return f.deadline, !f.deadline.IsZero() }

func (f *fakeStep) Fire(time.Time) []model.Sample {
	f.fired++
	return f.fire
}

func intSample(i int64) model.Sample { return sample(model.IntValue(i)) }

func TestChainFlowsThroughSteps(t *testing.T) {
	double := &fakeStep{out: model.KindInt, apply: func(s model.Sample) ([]model.Sample, error) {
		s.Value = model.IntValue(s.Value.Int * 2)
		return []model.Sample{s}, nil
	}}
	plusOne := &fakeStep{out: model.KindInt, apply: func(s model.Sample) ([]model.Sample, error) {
		s.Value = model.IntValue(s.Value.Int + 1)
		return []model.Sample{s}, nil
	}}

	chain := transform.NewChain([]string{"double", "plus_one"}, []transform.Step{double, plusOne}, model.KindInt)

	out, errs := chain.Apply(intSample(5))
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(out) != 1 || out[0].Value.Int != 11 {
		t.Fatalf("Apply = %+v, want one sample of 11", out)
	}
}

func TestChainStepProducingNothingStopsTheSample(t *testing.T) {
	drop := &fakeStep{out: model.KindInt, apply: func(model.Sample) ([]model.Sample, error) { return nil, nil }}
	next := &fakeStep{out: model.KindInt, apply: func(s model.Sample) ([]model.Sample, error) {
		t.Error("the step after a dropping step ran")
		return []model.Sample{s}, nil
	}}

	chain := transform.NewChain([]string{"drop", "next"}, []transform.Step{drop, next}, model.KindInt)

	out, errs := chain.Apply(intSample(1))
	if len(out) != 0 || len(errs) != 0 {
		t.Fatalf("Apply = %v, %v; want nothing", out, errs)
	}
}

func TestChainFireFlowsDownstream(t *testing.T) {
	deadline := t0.Add(10 * time.Second)
	timed := &fakeStep{
		out:      model.KindInt,
		apply:    func(s model.Sample) ([]model.Sample, error) { return []model.Sample{s}, nil },
		deadline: deadline,
		fire:     []model.Sample{intSample(7)},
	}
	plusOne := &fakeStep{out: model.KindInt, apply: func(s model.Sample) ([]model.Sample, error) {
		s.Value = model.IntValue(s.Value.Int + 1)
		return []model.Sample{s}, nil
	}}

	chain := transform.NewChain([]string{"timed", "plus_one"}, []transform.Step{timed, plusOne}, model.KindInt)

	got, ok := chain.Deadline()
	if !ok || !got.Equal(deadline) {
		t.Fatalf("Deadline = %v, %v; want %v, true", got, ok, deadline)
	}

	out, errs := chain.Fire(deadline)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(out) != 1 || out[0].Value.Int != 8 {
		t.Fatalf("Fire = %+v, want one sample of 8", out)
	}
}

func TestChainDeadlineIsEarliest(t *testing.T) {
	early, late := t0.Add(time.Second), t0.Add(time.Minute)
	mk := func(d time.Time) *fakeStep {
		return &fakeStep{out: model.KindInt, deadline: d, apply: func(s model.Sample) ([]model.Sample, error) { return []model.Sample{s}, nil }}
	}

	tests := []struct {
		name   string
		steps  []transform.Step
		want   time.Time
		wantOK bool
	}{
		{"none", []transform.Step{mk(time.Time{}), mk(time.Time{})}, time.Time{}, false},
		{"one", []transform.Step{mk(time.Time{}), mk(late)}, late, true},
		{"earliest", []transform.Step{mk(late), mk(early)}, early, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			names := make([]string, len(tc.steps))
			chain := transform.NewChain(names, tc.steps, model.KindInt)

			got, ok := chain.Deadline()
			if ok != tc.wantOK || !got.Equal(tc.want) {
				t.Errorf("Deadline = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestChainRuntimeErrorBecomesBadSampleOfStepKind(t *testing.T) {
	failing := &fakeStep{out: model.KindString, apply: func(model.Sample) ([]model.Sample, error) {
		return nil, errors.New("boom")
	}}
	after := &fakeStep{out: model.KindString, apply: func(s model.Sample) ([]model.Sample, error) {
		t.Error("a step ran on a bad sample")
		return []model.Sample{s}, nil
	}}

	chain := transform.NewChain([]string{"failing", "after"}, []transform.Step{failing, after}, model.KindInt)

	in := intSample(5)
	out, errs := chain.Apply(in)

	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one", errs)
	}
	var se *transform.StepError
	if !errors.As(errs[0], &se) || se.Position != 1 || se.Name != "failing" {
		t.Errorf("error = %v, want a StepError at step 1 (failing)", errs[0])
	}
	if len(out) != 1 {
		t.Fatalf("Apply produced %d samples, want 1", len(out))
	}
	got := out[0]
	if got.Quality != model.Bad {
		t.Errorf("Quality = %v, want bad", got.Quality)
	}
	if got.Value != (model.Value{Kind: model.KindString}) {
		t.Errorf("Value = %+v, want the zero string", got.Value)
	}
	if got.Device != in.Device || got.Signal != in.Signal || !got.TS.Equal(in.TS) || got.Priority != in.Priority {
		t.Errorf("identity fields changed: %+v", got)
	}
}

func TestChainBadSampleIsNotComputed(t *testing.T) {
	chain, err := transform.Compile([]config.Transform{step("scale", "2"), step("round", "1")}, model.KindInt, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	in := intSample(123)
	in.Quality = model.Bad

	out, errs := chain.Apply(in)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	// scale and round yield a float chain: the exit invariant gives the bad
	// integer the zero float and keeps every other field.
	want := in
	want.Value = model.Value{Kind: model.KindFloat}

	if len(out) != 1 || out[0] != want {
		t.Fatalf("Apply = %+v, want %+v", out, want)
	}
}

func TestChainExitKind(t *testing.T) {
	badInt := intSample(5)
	badInt.Quality = model.Bad

	badString := sample(model.StringValue("junk"))
	badString.Quality = model.Bad

	tests := []struct {
		name     string
		steps    []config.Transform
		in       model.ValueKind
		sample   model.Sample
		wantKind model.ValueKind
		wantQ    model.Quality
		wantVal  model.Value
		wantErrs int
	}{
		{
			name:     "valid_range then bit: out of range bad int leaves as bool false",
			steps:    []config.Transform{step("valid_range", `{"max":1}`), step("bit", "0")},
			in:       model.KindInt,
			sample:   intSample(5),
			wantKind: model.KindBool, wantQ: model.Bad, wantVal: model.Value{Kind: model.KindBool},
		},
		{
			name:     "bcd error then map to string: leaves as empty string",
			steps:    []config.Transform{step("bcd", "true"), step("map", `{"table":{"1":"one"}}`)},
			in:       model.KindInt,
			sample:   intSample(0x1A),
			wantKind: model.KindString, wantQ: model.Bad, wantVal: model.Value{Kind: model.KindString},
			wantErrs: 1,
		},
		{
			name:     "bad string into regex_extract as float: leaves as 0.0",
			steps:    []config.Transform{step("regex_extract", `{"pattern":"([0-9.]+)","as":"float"}`)},
			in:       model.KindString,
			sample:   badString,
			wantKind: model.KindFloat, wantQ: model.Bad, wantVal: model.Value{Kind: model.KindFloat},
		},
		{
			name:     "untyped point with round and an integer sample leaves as float",
			steps:    []config.Transform{step("round", "1")},
			in:       transform.KindOf(""),
			sample:   intSample(7),
			wantKind: model.KindFloat, wantQ: model.Good, wantVal: model.Value{Kind: model.KindFloat, Float: 7},
		},
		{
			name:     "empty chain of an untyped point converts a good integer",
			steps:    nil,
			in:       transform.KindOf(""),
			sample:   intSample(3),
			wantKind: model.KindFloat, wantQ: model.Good, wantVal: model.Value{Kind: model.KindFloat, Float: 3},
		},
		{
			name:     "good sample of an impossible kind becomes bad and is reported",
			steps:    nil,
			in:       model.KindBool,
			sample:   sample(model.StringValue("x")),
			wantKind: model.KindBool, wantQ: model.Bad, wantVal: model.Value{Kind: model.KindBool},
			wantErrs: 1,
		},
		{
			name:     "bad sample of the right kind is untouched",
			steps:    []config.Transform{step("valid_range", `{"max":1}`)},
			in:       model.KindInt,
			sample:   badInt,
			wantKind: model.KindInt, wantQ: model.Bad, wantVal: model.IntValue(5),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain, err := transform.Compile(tc.steps, tc.in, nil)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			out, errs := chain.Apply(tc.sample)
			if len(errs) != tc.wantErrs {
				t.Errorf("errors = %v, want %d", errs, tc.wantErrs)
			}

			if len(out) != 1 {
				t.Fatalf("Apply produced %d samples, want 1", len(out))
			}

			if out[0].Value.Kind != chain.OutKind() || chain.OutKind() != tc.wantKind {
				t.Errorf("Value.Kind = %v, OutKind = %v, want %v", out[0].Value.Kind, chain.OutKind(), tc.wantKind)
			}

			if out[0].Quality != tc.wantQ || out[0].Value != tc.wantVal {
				t.Errorf("sample = %+v, want quality %v value %+v", out[0], tc.wantQ, tc.wantVal)
			}

			if out[0].Device != tc.sample.Device || out[0].Signal != tc.sample.Signal || !out[0].TS.Equal(tc.sample.TS) || out[0].Priority != tc.sample.Priority {
				t.Errorf("identity fields changed: %+v", out[0])
			}
		})
	}
}

func TestChainFireExitKind(t *testing.T) {
	deadline := t0.Add(time.Second)
	emitted := sample(model.StringValue("late"))
	emitted.Quality = model.Bad

	// A timed step that is the chain's only step keeps its own kind; what it
	// emits upstream of a kind-changing step still leaves with the chain kind.
	timed := &fakeStep{
		out:      model.KindInt,
		apply:    func(s model.Sample) ([]model.Sample, error) { return []model.Sample{s}, nil },
		deadline: deadline,
		fire:     []model.Sample{emitted},
	}

	chain := transform.NewChain([]string{"timed"}, []transform.Step{timed}, model.KindInt)

	out, errs := chain.Fire(deadline)
	if len(errs) != 0 || len(out) != 1 {
		t.Fatalf("Fire = %v, %v", out, errs)
	}

	if out[0].Value != (model.Value{Kind: model.KindInt}) || out[0].Quality != model.Bad {
		t.Errorf("sample = %+v, want a bad zero integer", out[0])
	}

	if timed.fire[0].Value.Kind != model.KindString {
		t.Error("the step's own slice was mutated")
	}
}

func TestChainDoesNotMutateInput(t *testing.T) {
	chain, err := transform.Compile([]config.Transform{step("scale", "2")}, model.KindInt, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	in := intSample(4)
	snapshot := in
	chain.Apply(in)

	if in != snapshot {
		t.Errorf("input changed: %+v", in)
	}
}

func TestEmptyChainPassesThrough(t *testing.T) {
	chain, err := transform.Compile(nil, model.KindInt, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	in := intSample(9)
	out, errs := chain.Apply(in)
	if len(errs) != 0 || len(out) != 1 || out[0] != in {
		t.Fatalf("Apply = %v, %v; want the input", out, errs)
	}
	if _, ok := chain.Deadline(); ok {
		t.Error("an empty chain has a deadline")
	}
	if out, errs := chain.Fire(t0); len(out) != 0 || len(errs) != 0 {
		t.Errorf("Fire = %v, %v; want nothing", out, errs)
	}
}
