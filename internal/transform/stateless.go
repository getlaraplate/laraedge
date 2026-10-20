package transform

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/model"
)

// Fixed runtime messages. None of them carries a sample value.
var (
	errNotFinite   = errors.New("result is not finite")
	errValueKind   = errors.New("unexpected value kind")
	errBCDDigit    = errors.New("invalid BCD digit")
	errBCDNegative = errors.New("negative value is not BCD")
	errNoMapping   = errors.New("no mapping for the value")
	errNoMatch     = errors.New("no match")
	errConversion  = errors.New("capture cannot be converted")
)

func init() {
	register("scale", definition{accepts: acceptNumber, build: buildScale})
	register("offset", definition{accepts: acceptNumber, build: buildOffset})
	register("round", definition{accepts: acceptNumber, build: buildRound})
	register("bit", definition{accepts: acceptInt, build: buildBit})
	register("bcd", definition{accepts: acceptInt, build: buildBCD})
	register("map", definition{accepts: acceptAny, build: buildMap})
	register("valid_range", definition{accepts: acceptNumber, build: buildValidRange})
	register("trim", definition{accepts: acceptString, build: buildTrim})
	register("regex_extract", definition{accepts: acceptString, build: buildRegexExtract})
}

// stateless is embedded by the steps that keep no state and are never
// time-driven.
type stateless struct{}

// Deadline reports no deadline.
func (stateless) Deadline() (time.Time, bool) { return time.Time{}, false }

// Fire emits nothing.
func (stateless) Fire(time.Time) []model.Sample { return nil }

// withValue returns s carrying v.
func withValue(s model.Sample, v model.Value) []model.Sample {
	s.Value = v

	return []model.Sample{s}
}

// asFloat reads a numeric value as a float64.
func asFloat(v model.Value) (float64, bool) {
	switch v.Kind {
	case model.KindFloat:
		return v.Float, true
	case model.KindInt:
		return float64(v.Int), true
	default:
		return 0, false
	}
}

// finiteSample returns s carrying the float f, or errNotFinite (P6).
func finiteSample(s model.Sample, f float64) ([]model.Sample, error) {
	v, err := model.FloatValue(f)
	if err != nil {
		return nil, errNotFinite
	}

	return withValue(s, v), nil
}

// --- scale, offset ---

// arithmetic is scale or offset: a number in, a float out.
type arithmetic struct {
	stateless

	operand float64
	combine func(x, operand float64) float64
}

func (arithmetic) OutKind() model.ValueKind { return model.KindFloat }

func (a arithmetic) Apply(s model.Sample) ([]model.Sample, error) {
	x, ok := asFloat(s.Value)
	if !ok {
		return nil, errValueKind
	}

	return finiteSample(s, a.combine(x, a.operand))
}

func buildScale(c buildContext) (Step, error) {
	f, err := numberParam(c.Params)
	if err != nil {
		return nil, err
	}

	return arithmetic{operand: f, combine: func(x, k float64) float64 { return x * k }}, nil
}

func buildOffset(c buildContext) (Step, error) {
	f, err := numberParam(c.Params)
	if err != nil {
		return nil, err
	}

	return arithmetic{operand: f, combine: func(x, k float64) float64 { return x + k }}, nil
}

// --- round ---

// roundStep rounds half away from zero. Its output kind follows its input
// kind: an integer passes unchanged and stays an integer, a float stays a
// float.
type roundStep struct {
	stateless

	in    model.ValueKind
	scale float64
}

func (r roundStep) OutKind() model.ValueKind { return r.in }

func (r roundStep) Apply(s model.Sample) ([]model.Sample, error) {
	switch s.Value.Kind {
	case model.KindInt:
		return []model.Sample{s}, nil
	case model.KindFloat:
		x := s.Value.Float
		// Beyond 2^52 every float is already an integer; multiplying could
		// overflow.
		if math.Abs(x) >= 1<<52 {
			return []model.Sample{s}, nil
		}

		return finiteSample(s, math.Round(x*r.scale)/r.scale)
	default:
		return nil, errValueKind
	}
}

func buildRound(c buildContext) (Step, error) {
	decimals, err := integerParam(c.Params, 0, 10)
	if err != nil {
		return nil, err
	}

	return roundStep{in: c.In, scale: math.Pow10(int(decimals))}, nil
}

// --- bit, bcd ---

type bitStep struct {
	stateless

	index uint
}

func (bitStep) OutKind() model.ValueKind { return model.KindBool }

func (b bitStep) Apply(s model.Sample) ([]model.Sample, error) {
	if s.Value.Kind != model.KindInt {
		return nil, errValueKind
	}

	return withValue(s, model.BoolValue((uint64(s.Value.Int)>>b.index)&1 == 1)), nil
}

func buildBit(c buildContext) (Step, error) {
	i, err := integerParam(c.Params, 0, 63)
	if err != nil {
		return nil, err
	}

	return bitStep{index: uint(i)}, nil
}

type bcdStep struct{ stateless }

func (bcdStep) OutKind() model.ValueKind { return model.KindInt }

func (bcdStep) Apply(s model.Sample) ([]model.Sample, error) {
	if s.Value.Kind != model.KindInt {
		return nil, errValueKind
	}

	if s.Value.Int < 0 {
		return nil, errBCDNegative
	}

	var (
		raw    = uint64(s.Value.Int)
		result int64
		weight int64 = 1
	)

	for ; raw != 0; raw >>= 4 {
		digit := int64(raw & 0xF)
		if digit > 9 {
			return nil, errBCDDigit
		}

		result += digit * weight
		weight *= 10
	}

	return withValue(s, model.IntValue(result)), nil
}

func buildBCD(c buildContext) (Step, error) {
	if err := trueParam(c.Params); err != nil {
		return nil, err
	}

	return bcdStep{}, nil
}

// --- map ---

type mapStep struct {
	stateless

	table map[string]model.Value
	def   *model.Value
	out   model.ValueKind
}

func (m mapStep) OutKind() model.ValueKind { return m.out }

func (m mapStep) Apply(s model.Sample) ([]model.Sample, error) {
	key, ok := mapKey(s.Value)
	if !ok {
		return nil, errValueKind
	}

	if v, found := m.table[key]; found {
		return withValue(s, v), nil
	}

	if m.def != nil {
		return withValue(s, *m.def), nil
	}

	return nil, errNoMapping
}

// mapKey renders a value as the text key of a map table (decision P4).
func mapKey(v model.Value) (string, bool) {
	switch v.Kind {
	case model.KindBool:
		return strconv.FormatBool(v.Bool), true
	case model.KindInt:
		return strconv.FormatInt(v.Int, 10), true
	case model.KindFloat:
		return strconv.FormatFloat(v.Float, 'g', -1, 64), true
	case model.KindString:
		return v.Str, true
	default:
		return "", false
	}
}

var (
	errMapKinds  = errors.New("table values and default must share one kind")
	errMapScalar = errors.New("table values and default must be booleans, numbers or strings")
	errMapEmpty  = errors.New("table has no value")
)

// mapValue converts a decoded table value. A number with a fraction or an
// exponent is a float; any other number is an integer.
func mapValue(v any) (model.Value, error) {
	switch x := v.(type) {
	case bool:
		return model.BoolValue(x), nil
	case string:
		return model.StringValue(x), nil
	case json.Number:
		lit := x.String()
		if !strings.ContainsAny(lit, ".eE") {
			i, err := strconv.ParseInt(lit, 10, 64)
			if err != nil {
				return model.Value{}, errors.New("table number is out of range")
			}

			return model.IntValue(i), nil
		}

		f, ok := finiteFloat(x)
		if !ok {
			return model.Value{}, errors.New("table number is out of range")
		}

		return model.FloatValue(f)
	default:
		return model.Value{}, errMapScalar
	}
}

func buildMap(c buildContext) (Step, error) {
	obj, err := objectParam(c.Params, "table", "default")
	if err != nil {
		return nil, err
	}

	rawTable, ok := obj["table"]
	if !ok {
		return nil, errors.New("parameter table is required")
	}

	table, ok := rawTable.(map[string]any)
	if !ok {
		return nil, errors.New("parameter table must be an object")
	}

	step := mapStep{table: make(map[string]model.Value, len(table))}

	var kind model.ValueKind

	note := func(v model.Value) error {
		if kind != 0 && kind != v.Kind {
			return errMapKinds
		}

		kind = v.Kind

		return nil
	}

	for key, raw := range table {
		v, err := mapValue(raw)
		if err != nil {
			return nil, err
		}

		if err := note(v); err != nil {
			return nil, err
		}

		step.table[key] = v
	}

	if raw, has := obj["default"]; has {
		v, err := mapValue(raw)
		if err != nil {
			return nil, err
		}

		if err := note(v); err != nil {
			return nil, err
		}

		step.def = &v
	}

	if kind == 0 {
		return nil, errMapEmpty
	}

	step.out = kind

	return step, nil
}

// --- valid_range ---

type rangeStep struct {
	stateless

	in       model.ValueKind
	min, max *float64
}

func (r rangeStep) OutKind() model.ValueKind { return r.in }

func (r rangeStep) Apply(s model.Sample) ([]model.Sample, error) {
	x, ok := asFloat(s.Value)
	if !ok {
		return nil, errValueKind
	}

	if (r.min != nil && x < *r.min) || (r.max != nil && x > *r.max) {
		s.Quality = model.Bad
	}

	return []model.Sample{s}, nil
}

func buildValidRange(c buildContext) (Step, error) {
	obj, err := objectParam(c.Params, "min", "max")
	if err != nil {
		return nil, err
	}

	step := rangeStep{in: c.In}

	for _, bound := range []struct {
		key string
		dst **float64
	}{{"min", &step.min}, {"max", &step.max}} {
		raw, has := obj[bound.key]
		if !has {
			continue
		}

		f, ok := finiteFloat(raw)
		if !ok {
			return nil, errors.New("parameter " + bound.key + " must be a number")
		}

		*bound.dst = &f
	}

	if step.min == nil && step.max == nil {
		return nil, errors.New("at least one of min and max is required")
	}

	if step.min != nil && step.max != nil && *step.min > *step.max {
		return nil, errors.New("min must not exceed max")
	}

	return step, nil
}

// --- trim ---

type trimStep struct{ stateless }

func (trimStep) OutKind() model.ValueKind { return model.KindString }

func (trimStep) Apply(s model.Sample) ([]model.Sample, error) {
	if s.Value.Kind != model.KindString {
		return nil, errValueKind
	}

	return withValue(s, model.StringValue(strings.TrimSpace(s.Value.Str))), nil
}

func buildTrim(c buildContext) (Step, error) {
	if err := trueParam(c.Params); err != nil {
		return nil, err
	}

	return trimStep{}, nil
}

// --- regex_extract ---

type regexStep struct {
	stateless

	re    *regexp.Regexp
	group int
	out   model.ValueKind
}

func (r regexStep) OutKind() model.ValueKind { return r.out }

func (r regexStep) Apply(s model.Sample) ([]model.Sample, error) {
	if s.Value.Kind != model.KindString {
		return nil, errValueKind
	}

	loc := r.re.FindStringSubmatchIndex(s.Value.Str)
	if loc == nil || loc[2*r.group] < 0 {
		return nil, errNoMatch
	}

	capture := s.Value.Str[loc[2*r.group]:loc[2*r.group+1]]

	switch r.out {
	case model.KindInt:
		i, err := strconv.ParseInt(capture, 10, 64)
		if err != nil {
			return nil, errConversion
		}

		return withValue(s, model.IntValue(i)), nil
	case model.KindFloat:
		f, err := strconv.ParseFloat(capture, 64)
		if err != nil {
			return nil, errConversion
		}

		v, err := model.FloatValue(f)
		if err != nil {
			return nil, errConversion
		}

		return withValue(s, v), nil
	default:
		return withValue(s, model.StringValue(capture)), nil
	}
}

func buildRegexExtract(c buildContext) (Step, error) {
	obj, err := objectParam(c.Params, "pattern", "group", "as")
	if err != nil {
		return nil, err
	}

	rawPattern, has := obj["pattern"]
	if !has {
		return nil, errors.New("parameter pattern is required")
	}

	pattern, ok := rawPattern.(string)
	if !ok {
		return nil, errors.New("parameter pattern must be a string")
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, errors.New("pattern is not a valid RE2 expression")
	}

	group := int64(1)

	if raw, has := obj["group"]; has {
		g, ok := integerIn(raw, 0, math.MaxInt32)
		if !ok {
			return nil, errors.New("parameter group must be a non-negative integer")
		}

		group = g
	}

	if group > int64(re.NumSubexp()) {
		return nil, errors.New("group does not exist in the pattern")
	}

	out := model.KindString

	if raw, has := obj["as"]; has {
		as, _ := raw.(string)

		switch as {
		case "string":
		case "integer":
			out = model.KindInt
		case "float":
			out = model.KindFloat
		default:
			return nil, errors.New("parameter as must be string, integer or float")
		}
	}

	return regexStep{re: re, group: int(group), out: out}, nil
}
