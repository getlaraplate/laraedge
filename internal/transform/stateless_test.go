package transform_test

import (
	"math"
	"strings"
	"testing"

	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/transform"
)

type statelessCase struct {
	name      string
	transform string
	params    string
	in        model.Value
	inQuality model.Quality // zero means good
	want      model.Value
	wantQ     model.Quality // zero means good
	wantErr   bool
}

func mustFloat(t *testing.T, f float64) model.Value {
	t.Helper()

	v, err := model.FloatValue(f)
	if err != nil {
		t.Fatalf("FloatValue(%v): %v", f, err)
	}

	return v
}

func sameValue(got, want model.Value) bool {
	if got.Kind != want.Kind {
		return false
	}

	if got.Kind == model.KindFloat {
		return math.Abs(got.Float-want.Float) < 1e-9*math.Max(1, math.Abs(want.Float))
	}

	return got == want
}

func runStateless(t *testing.T, tests []statelessCase) {
	t.Helper()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain, err := transform.Compile(
				[]config.Transform{step(tc.transform, tc.params)}, tc.in.Kind, nil)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			in := sample(tc.in)
			if tc.inQuality != 0 {
				in.Quality = tc.inQuality
			}

			out, errs := chain.Apply(in)

			if (len(errs) != 0) != tc.wantErr {
				t.Fatalf("errors = %v, wantErr %v", errs, tc.wantErr)
			}

			if len(out) != 1 {
				t.Fatalf("Apply produced %d samples, want 1: %+v", len(out), out)
			}

			wantQ := tc.wantQ
			if wantQ == 0 {
				wantQ = model.Good
			}

			got := out[0]
			if got.Quality != wantQ {
				t.Errorf("Quality = %v, want %v", got.Quality, wantQ)
			}

			if !sameValue(got.Value, tc.want) {
				t.Errorf("Value = %+v, want %+v", got.Value, tc.want)
			}

			if got.Device != in.Device || got.Signal != in.Signal || !got.TS.Equal(in.TS) || got.Priority != in.Priority {
				t.Errorf("identity fields changed: %+v", got)
			}
		})
	}
}

func TestScale(t *testing.T) {
	runStateless(t, []statelessCase{
		{name: "int to float", transform: "scale", params: "0.1", in: model.IntValue(123), want: model.Value{Kind: model.KindFloat, Float: 12.3}},
		{name: "float", transform: "scale", params: "2", in: model.Value{Kind: model.KindFloat, Float: 1.5}, want: model.Value{Kind: model.KindFloat, Float: 3}},
		{name: "negative factor on int", transform: "scale", params: "-1", in: model.IntValue(5), want: model.Value{Kind: model.KindFloat, Float: -5}},
		{name: "uncertain stays uncertain", transform: "scale", params: "2", in: model.IntValue(2), inQuality: model.Uncertain, wantQ: model.Uncertain, want: model.Value{Kind: model.KindFloat, Float: 4}},
		{name: "float overflow is an error", transform: "scale", params: "1e308", in: model.Value{Kind: model.KindFloat, Float: 1e308}, wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindFloat}},
		{name: "large int overflow is an error", transform: "scale", params: "1e308", in: model.IntValue(math.MaxInt64), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindFloat}},
		{name: "negative overflow is an error", transform: "scale", params: "1e308", in: model.IntValue(math.MinInt64), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindFloat}},
	})
}

func TestOffset(t *testing.T) {
	runStateless(t, []statelessCase{
		{name: "int to float", transform: "offset", params: "0.5", in: model.IntValue(2), want: model.Value{Kind: model.KindFloat, Float: 2.5}},
		{name: "negative", transform: "offset", params: "-3", in: model.Value{Kind: model.KindFloat, Float: 1}, want: model.Value{Kind: model.KindFloat, Float: -2}},
		{name: "overflow is an error", transform: "offset", params: "1e308", in: model.Value{Kind: model.KindFloat, Float: 1.7e308}, wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindFloat}},
	})
}

func TestRound(t *testing.T) {
	runStateless(t, []statelessCase{
		{name: "down", transform: "round", params: "1", in: model.Value{Kind: model.KindFloat, Float: 12.345}, want: model.Value{Kind: model.KindFloat, Float: 12.3}},
		{name: "negative half away from zero", transform: "round", params: "1", in: model.Value{Kind: model.KindFloat, Float: -12.35}, want: model.Value{Kind: model.KindFloat, Float: -12.4}},
		{name: "zero decimals positive half", transform: "round", params: "0", in: model.Value{Kind: model.KindFloat, Float: 2.5}, want: model.Value{Kind: model.KindFloat, Float: 3}},
		{name: "zero decimals negative half", transform: "round", params: "0", in: model.Value{Kind: model.KindFloat, Float: -2.5}, want: model.Value{Kind: model.KindFloat, Float: -3}},
		{name: "ten decimals", transform: "round", params: "10", in: model.Value{Kind: model.KindFloat, Float: 1.23}, want: model.Value{Kind: model.KindFloat, Float: 1.23}},
		{name: "huge float unchanged", transform: "round", params: "10", in: model.Value{Kind: model.KindFloat, Float: 1e300}, want: model.Value{Kind: model.KindFloat, Float: 1e300}},
		{name: "integer passes unchanged", transform: "round", params: "1", in: model.IntValue(7), want: model.IntValue(7)},
		{name: "decimal tie 1.005 goes up", transform: "round", params: "2", in: model.Value{Kind: model.KindFloat, Float: 1.005}, want: model.Value{Kind: model.KindFloat, Float: 1.01}},
		{name: "decimal tie 0.285 goes up", transform: "round", params: "2", in: model.Value{Kind: model.KindFloat, Float: 0.285}, want: model.Value{Kind: model.KindFloat, Float: 0.29}},
		{name: "decimal tie 2.675 goes up", transform: "round", params: "2", in: model.Value{Kind: model.KindFloat, Float: 2.675}, want: model.Value{Kind: model.KindFloat, Float: 2.68}},
		{name: "negative decimal tie 1.005", transform: "round", params: "2", in: model.Value{Kind: model.KindFloat, Float: -1.005}, want: model.Value{Kind: model.KindFloat, Float: -1.01}},
		{name: "negative decimal tie 0.285", transform: "round", params: "2", in: model.Value{Kind: model.KindFloat, Float: -0.285}, want: model.Value{Kind: model.KindFloat, Float: -0.29}},
		{name: "below the tie stays down", transform: "round", params: "2", in: model.Value{Kind: model.KindFloat, Float: 1.0049}, want: model.Value{Kind: model.KindFloat, Float: 1}},
		{name: "carry into the integer part", transform: "round", params: "2", in: model.Value{Kind: model.KindFloat, Float: 9.995}, want: model.Value{Kind: model.KindFloat, Float: 10}},
		{name: "carry through zero decimals", transform: "round", params: "0", in: model.Value{Kind: model.KindFloat, Float: 999.5}, want: model.Value{Kind: model.KindFloat, Float: 1000}},
		{name: "half to one from zero point five", transform: "round", params: "0", in: model.Value{Kind: model.KindFloat, Float: 0.5}, want: model.Value{Kind: model.KindFloat, Float: 1}},
		{name: "fewer decimals than asked", transform: "round", params: "3", in: model.Value{Kind: model.KindFloat, Float: 1.5}, want: model.Value{Kind: model.KindFloat, Float: 1.5}},
		{name: "tiny value rounds to zero", transform: "round", params: "2", in: model.Value{Kind: model.KindFloat, Float: 1e-300}, want: model.Value{Kind: model.KindFloat, Float: 0}},
	})
}

func TestBit(t *testing.T) {
	runStateless(t, []statelessCase{
		{name: "set", transform: "bit", params: "3", in: model.IntValue(8), want: model.BoolValue(true)},
		{name: "clear", transform: "bit", params: "3", in: model.IntValue(7), want: model.BoolValue(false)},
		{name: "bit 0", transform: "bit", params: "0", in: model.IntValue(1), want: model.BoolValue(true)},
		{name: "bit 63 of a negative", transform: "bit", params: "63", in: model.IntValue(-1), want: model.BoolValue(true)},
		{name: "bit 63 of a positive", transform: "bit", params: "63", in: model.IntValue(math.MaxInt64), want: model.BoolValue(false)},
		{name: "zero", transform: "bit", params: "5", in: model.IntValue(0), want: model.BoolValue(false)},
	})
}

func TestBCD(t *testing.T) {
	runStateless(t, []statelessCase{
		{name: "four digits", transform: "bcd", params: "true", in: model.IntValue(0x1234), want: model.IntValue(1234)},
		{name: "zero", transform: "bcd", params: "true", in: model.IntValue(0), want: model.IntValue(0)},
		{name: "two digits", transform: "bcd", params: "true", in: model.IntValue(0x99), want: model.IntValue(99)},
		{name: "sixteen digits", transform: "bcd", params: "true", in: model.IntValue(0x1234567890123456), want: model.IntValue(1234567890123456)},
		{name: "nibble above nine", transform: "bcd", params: "true", in: model.IntValue(0x1A), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindInt}},
		{name: "top nibble above nine", transform: "bcd", params: "true", in: model.IntValue(0x0F00), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindInt}},
		{name: "negative", transform: "bcd", params: "true", in: model.IntValue(-1), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindInt}},
	})
}

func TestMap(t *testing.T) {
	runStateless(t, []statelessCase{
		{name: "int key", transform: "map", params: `{"table":{"1":"run"}}`, in: model.IntValue(1), want: model.StringValue("run")},
		{name: "missing key without default", transform: "map", params: `{"table":{"1":"run"}}`, in: model.IntValue(2), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindString}},
		{name: "missing key with default", transform: "map", params: `{"table":{"1":"run"},"default":"idle"}`, in: model.IntValue(2), want: model.StringValue("idle")},
		{name: "negative int key", transform: "map", params: `{"table":{"-4":"rev"}}`, in: model.IntValue(-4), want: model.StringValue("rev")},
		{name: "bool true key", transform: "map", params: `{"table":{"true":1,"false":0}}`, in: model.BoolValue(true), want: model.IntValue(1)},
		{name: "bool false key", transform: "map", params: `{"table":{"true":1,"false":0}}`, in: model.BoolValue(false), want: model.IntValue(0)},
		{name: "float key shortest form", transform: "map", params: `{"table":{"1.5":"half"}}`, in: model.Value{Kind: model.KindFloat, Float: 1.5}, want: model.StringValue("half")},
		{name: "whole float key has no fraction", transform: "map", params: `{"table":{"1":"one"}}`, in: model.Value{Kind: model.KindFloat, Float: 1}, want: model.StringValue("one")},
		{name: "string key", transform: "map", params: `{"table":{"OK":true}}`, in: model.StringValue("OK"), want: model.BoolValue(true)},
		{name: "string key is case sensitive", transform: "map", params: `{"table":{"OK":true},"default":false}`, in: model.StringValue("ok"), want: model.BoolValue(false)},
		{name: "float values", transform: "map", params: `{"table":{"a":1.5}}`, in: model.StringValue("a"), want: model.Value{Kind: model.KindFloat, Float: 1.5}},
		{name: "whole and fractional values make a float table", transform: "map", params: `{"table":{"a":1,"b":1.5}}`, in: model.StringValue("a"), want: model.Value{Kind: model.KindFloat, Float: 1}},
		{name: "fractional value of a float table", transform: "map", params: `{"table":{"a":1,"b":1.5}}`, in: model.StringValue("b"), want: model.Value{Kind: model.KindFloat, Float: 1.5}},
		{name: "float default promotes integer values", transform: "map", params: `{"table":{"a":1},"default":2.5}`, in: model.StringValue("a"), want: model.Value{Kind: model.KindFloat, Float: 1}},
		{name: "integer default promoted by float values", transform: "map", params: `{"table":{"a":1.5},"default":2}`, in: model.StringValue("z"), want: model.Value{Kind: model.KindFloat, Float: 2}},
		{name: "all integers stay integer", transform: "map", params: `{"table":{"a":1,"b":2},"default":3}`, in: model.StringValue("b"), want: model.IntValue(2)},
		{name: "default only kind", transform: "map", params: `{"table":{},"default":7}`, in: model.IntValue(1), want: model.IntValue(7)},
	})
}

func TestMapRuntimeErrorDoesNotEchoTheValue(t *testing.T) {
	chain, err := transform.Compile([]config.Transform{step("map", `{"table":{"a":1}}`)}, model.KindString, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	_, errs := chain.Apply(sample(model.StringValue("SECRETVALUE")))
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one", errs)
	}

	if strings.Contains(errs[0].Error(), "SECRETVALUE") {
		t.Errorf("error %q echoes the sample value", errs[0])
	}
}

func TestValidRange(t *testing.T) {
	runStateless(t, []statelessCase{
		{name: "above max", transform: "valid_range", params: `{"max":100}`, in: model.IntValue(150), wantQ: model.Bad, want: model.IntValue(150)},
		{name: "within", transform: "valid_range", params: `{"max":100}`, in: model.IntValue(50), want: model.IntValue(50)},
		{name: "max is inclusive", transform: "valid_range", params: `{"max":100}`, in: model.IntValue(100), want: model.IntValue(100)},
		{name: "below min", transform: "valid_range", params: `{"min":10}`, in: model.IntValue(5), wantQ: model.Bad, want: model.IntValue(5)},
		{name: "min is inclusive", transform: "valid_range", params: `{"min":10}`, in: model.IntValue(10), want: model.IntValue(10)},
		{name: "float above max", transform: "valid_range", params: `{"min":0,"max":100}`, in: model.Value{Kind: model.KindFloat, Float: 100.5}, wantQ: model.Bad, want: model.Value{Kind: model.KindFloat, Float: 100.5}},
		{name: "float within both", transform: "valid_range", params: `{"min":0,"max":100}`, in: model.Value{Kind: model.KindFloat, Float: 12.5}, want: model.Value{Kind: model.KindFloat, Float: 12.5}},
		{name: "uncertain within keeps uncertain", transform: "valid_range", params: `{"max":100}`, in: model.IntValue(1), inQuality: model.Uncertain, wantQ: model.Uncertain, want: model.IntValue(1)},
		{name: "uncertain out of range becomes bad", transform: "valid_range", params: `{"max":100}`, in: model.IntValue(101), inQuality: model.Uncertain, wantQ: model.Bad, want: model.IntValue(101)},
	})
}

func TestTrim(t *testing.T) {
	runStateless(t, []statelessCase{
		{name: "surrounding white space", transform: "trim", params: "true", in: model.StringValue(" a b \n"), want: model.StringValue("a b")},
		{name: "tabs and carriage returns", transform: "trim", params: "true", in: model.StringValue("\t12.5\r\n"), want: model.StringValue("12.5")},
		{name: "already clean", transform: "trim", params: "true", in: model.StringValue("ok"), want: model.StringValue("ok")},
		{name: "empty", transform: "trim", params: "true", in: model.StringValue(""), want: model.StringValue("")},
		{name: "only white space", transform: "trim", params: "true", in: model.StringValue("  "), want: model.StringValue("")},
	})
}

func TestRegexExtract(t *testing.T) {
	runStateless(t, []statelessCase{
		{name: "float", transform: "regex_extract", params: `{"pattern":"W=([0-9.]+)","as":"float"}`, in: model.StringValue("W=12.5g"), want: model.Value{Kind: model.KindFloat, Float: 12.5}},
		{name: "integer", transform: "regex_extract", params: `{"pattern":"n=([0-9]+)","as":"integer"}`, in: model.StringValue("n=42"), want: model.IntValue(42)},
		{name: "negative integer", transform: "regex_extract", params: `{"pattern":"n=(-?[0-9]+)","as":"integer"}`, in: model.StringValue("n=-42"), want: model.IntValue(-42)},
		{name: "string by default", transform: "regex_extract", params: `{"pattern":"id:(\\w+)"}`, in: model.StringValue("id:ab12 end"), want: model.StringValue("ab12")},
		{name: "explicit string", transform: "regex_extract", params: `{"pattern":"id:(\\w+)","as":"string"}`, in: model.StringValue("id:ab12"), want: model.StringValue("ab12")},
		{name: "group zero is the whole match", transform: "regex_extract", params: `{"pattern":"[0-9]+","group":0}`, in: model.StringValue("ab 17 cd"), want: model.StringValue("17")},
		{name: "second group", transform: "regex_extract", params: `{"pattern":"(\\w+)=(\\w+)","group":2}`, in: model.StringValue("k=v"), want: model.StringValue("v")},
		{name: "first match wins", transform: "regex_extract", params: `{"pattern":"([0-9]+)","as":"integer"}`, in: model.StringValue("1 2 3"), want: model.IntValue(1)},
		{name: "no match", transform: "regex_extract", params: `{"pattern":"W=([0-9.]+)","as":"float"}`, in: model.StringValue("nothing"), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindFloat}},
		{name: "no match as string", transform: "regex_extract", params: `{"pattern":"W=(.+)"}`, in: model.StringValue("nothing"), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindString}},
		{name: "failed integer conversion", transform: "regex_extract", params: `{"pattern":"n=(.*)","as":"integer"}`, in: model.StringValue("n=4.2"), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindInt}},
		{name: "failed float conversion", transform: "regex_extract", params: `{"pattern":"n=(.*)","as":"float"}`, in: model.StringValue("n=abc"), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindFloat}},
		{name: "float conversion refuses NaN", transform: "regex_extract", params: `{"pattern":"n=(.*)","as":"float"}`, in: model.StringValue("n=NaN"), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindFloat}},
		{name: "float conversion refuses infinity", transform: "regex_extract", params: `{"pattern":"n=(.*)","as":"float"}`, in: model.StringValue("n=1e999"), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindFloat}},
		{name: "group that did not take part", transform: "regex_extract", params: `{"pattern":"(a)|(b)","group":2}`, in: model.StringValue("a"), wantErr: true, wantQ: model.Bad, want: model.Value{Kind: model.KindString}},
	})
}

func TestBadInputIsNotComputedByAnyStatelessTransform(t *testing.T) {
	tests := []struct {
		transform string
		params    string
		in        model.Value
	}{
		{"scale", "2", model.IntValue(1)},
		{"offset", "2", model.IntValue(1)},
		{"round", "1", model.Value{Kind: model.KindFloat, Float: 1.234}},
		{"bit", "1", model.IntValue(2)},
		{"bcd", "true", model.IntValue(0x1A)},
		{"map", `{"table":{"1":"a"}}`, model.IntValue(2)},
		{"valid_range", `{"max":1}`, model.IntValue(2)},
		{"trim", "true", model.StringValue(" a ")},
		{"regex_extract", `{"pattern":"(x)"}`, model.StringValue("no match")},
	}

	for _, tc := range tests {
		t.Run(tc.transform, func(t *testing.T) {
			chain, err := transform.Compile([]config.Transform{step(tc.transform, tc.params)}, tc.in.Kind, nil)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			in := sample(tc.in)
			in.Quality = model.Bad

			out, errs := chain.Apply(in)
			if len(errs) != 0 {
				t.Errorf("errors: %v", errs)
			}

			// Not computed: only the chain-exit invariant may touch it, giving
			// the zero value of the chain's kind when the kind differs.
			want := in
			if want.Value.Kind != chain.OutKind() {
				want.Value = model.Value{Kind: chain.OutKind()}
			}

			if len(out) != 1 || out[0] != want {
				t.Errorf("Apply = %+v, want %+v", out, want)
			}
		})
	}
}

func TestStatelessTransformsHaveNoDeadline(t *testing.T) {
	chain, err := transform.Compile([]config.Transform{step("scale", "2"), step("round", "1")}, model.KindInt, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if _, ok := chain.Deadline(); ok {
		t.Error("a stateless chain reports a deadline")
	}

	if out, errs := chain.Fire(t0); len(out) != 0 || len(errs) != 0 {
		t.Errorf("Fire = %v, %v; want nothing", out, errs)
	}
}

func TestNegativeZeroIsNormalised(t *testing.T) {
	negZero := math.Copysign(0, -1)

	tests := []struct {
		name      string
		transform string
		params    string
		in        model.Value
	}{
		{"scale", "scale", "-1", model.Value{Kind: model.KindFloat, Float: 0}},
		{"scale of an int", "scale", "-1", model.IntValue(0)},
		{"offset", "offset", "0", model.Value{Kind: model.KindFloat, Float: negZero}},
		{"round towards zero", "round", "0", model.Value{Kind: model.KindFloat, Float: -0.4}},
		{"round a negative tiny value", "round", "2", model.Value{Kind: model.KindFloat, Float: -1e-9}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain, err := transform.Compile([]config.Transform{step(tc.transform, tc.params)}, tc.in.Kind, nil)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			out, errs := chain.Apply(sample(tc.in))
			if len(errs) != 0 || len(out) != 1 {
				t.Fatalf("Apply = %v, %v", out, errs)
			}

			if out[0].Value.Float != 0 || math.Signbit(out[0].Value.Float) {
				t.Errorf("Value = %v (sign bit %v), want +0", out[0].Value.Float, math.Signbit(out[0].Value.Float))
			}
		})
	}
}

func TestMapRefusalIsDeterministic(t *testing.T) {
	// Two bad values: the sorted first key decides which message is reported.
	for range 20 {
		_, err := transform.Compile([]config.Transform{step("map", `{"table":{"b":null,"a":99999999999999999999}}`)}, model.KindString, nil)
		if err == nil || err.Error() != "step 1 (map): table number is out of range" {
			t.Fatalf("Compile error = %v", err)
		}
	}
}
