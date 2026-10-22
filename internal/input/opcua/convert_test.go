package opcua

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"

	"github.com/getlaraplate/laraedge/internal/model"
)

func goodDV(v interface{}) *ua.DataValue {
	return &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(v)}
}

func mustFloat(t *testing.T, f float64) model.Value {
	t.Helper()

	v, err := model.FloatValue(f)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

func TestConvertTable(t *testing.T) {
	const (
		maxInt64  = int64(math.MaxInt64)
		pow53     = int64(1) << 53
		pow24     = int64(1) << 24
		bigUint64 = uint64(math.MaxInt64) + 1
	)

	tests := []struct {
		name    string
		in      interface{}
		typ     string
		want    model.Value
		wantErr error
	}{
		// integer to integer
		{"int32 -5 to int16", int32(-5), "int16", model.IntValue(-5), nil},
		{"int32 32767 to int16", int32(32767), "int16", model.IntValue(32767), nil},
		{"int32 32768 to int16", int32(32768), "int16", model.IntValue(0), errConvertRange},
		{"int32 -32769 to int16", int32(-32769), "int16", model.IntValue(0), errConvertRange},
		{"uint32 70000 to uint16", uint32(70000), "uint16", model.IntValue(0), errConvertRange},
		{"uint32 65535 to uint16", uint32(65535), "uint16", model.IntValue(65535), nil},
		{"int32 -1 to uint16", int32(-1), "uint16", model.IntValue(0), errConvertRange},
		{"int32 -1 to uint64", int32(-1), "uint64", model.IntValue(0), errConvertRange},
		{"int64 max to int64", maxInt64, "int64", model.IntValue(maxInt64), nil},
		{"int64 to int32 too big", int64(1) << 31, "int32", model.IntValue(0), errConvertRange},
		{"uint32 max to int32", uint32(math.MaxUint32), "int32", model.IntValue(0), errConvertRange},
		{"uint32 max to uint32", uint32(math.MaxUint32), "uint32", model.IntValue(math.MaxUint32), nil},
		{"int8 to int16", int8(-128), "int16", model.IntValue(-128), nil},
		{"uint8 to uint16", uint8(255), "uint16", model.IntValue(255), nil},
		{"uint64 max int64 to uint64", uint64(math.MaxInt64), "uint64", model.IntValue(maxInt64), nil},
		{"uint64 above int64 to uint64", bigUint64, "uint64", model.IntValue(0), errConvertUint64},
		{"uint64 above int64 to int64", bigUint64, "int64", model.IntValue(0), errConvertUint64},
		{"uint64 max to float64", uint64(math.MaxUint64), "float64", mustFloat(t, 0), errConvertUint64},
		{"uint64 small to int32", uint64(7), "int32", model.IntValue(7), nil},
		// integer to float
		{"int64 2^53 to float64", pow53, "float64", mustFloat(t, 9007199254740992), nil},
		{"int64 -2^53 to float64", -pow53, "float64", mustFloat(t, -9007199254740992), nil},
		{"int64 2^53+1 to float64", pow53 + 1, "float64", mustFloat(t, 0), errConvertPrecision},
		{"int64 -2^53-1 to float64", -pow53 - 1, "float64", mustFloat(t, 0), errConvertPrecision},
		{"int32 to float64", int32(-5), "float64", mustFloat(t, -5), nil},
		{"int32 2^24 to float32", int32(pow24), "float32", mustFloat(t, 16777216), nil},
		{"int32 2^24+1 to float32", int32(pow24 + 1), "float32", mustFloat(t, 0), errConvertPrecision},
		{"int32 -2^24-1 to float32", int32(-pow24 - 1), "float32", mustFloat(t, 0), errConvertPrecision},
		// float to float
		{"float32 1.5 to float64", float32(1.5), "float64", mustFloat(t, 1.5), nil},
		{"float64 to float64", 2.25, "float64", mustFloat(t, 2.25), nil},
		{"float32 to float32", float32(0.5), "float32", mustFloat(t, 0.5), nil},
		{"float64 exact to float32", 1.5, "float32", mustFloat(t, 1.5), nil},
		{"float64 lossy to float32", 0.1, "float32", mustFloat(t, 0), errConvertPrecision},
		{"float64 overflow to float32", 1e300, "float32", mustFloat(t, 0), errConvertPrecision},
		{"float32 0.1 to float64 keeps float32 value", float32(0.1), "float64", mustFloat(t, float64(float32(0.1))), nil},
		{"NaN to float64", math.NaN(), "float64", mustFloat(t, 0), errConvertNotFinite},
		{"+Inf to float64", math.Inf(1), "float64", mustFloat(t, 0), errConvertNotFinite},
		{"-Inf to float32", math.Inf(-1), "float32", mustFloat(t, 0), errConvertNotFinite},
		{"float32 NaN to float64", float32(math.NaN()), "float64", mustFloat(t, 0), errConvertNotFinite},
		// float to integer never
		{"double to int32", 5.0, "int32", model.IntValue(0), errConvertType},
		{"float to uint16", float32(1), "uint16", model.IntValue(0), errConvertType},
		// bool and string
		{"bool to bool", true, "bool", model.BoolValue(true), nil},
		{"bool false to bool", false, "bool", model.BoolValue(false), nil},
		{"bool to int16", true, "int16", model.IntValue(0), errConvertType},
		{"bool to float64", true, "float64", mustFloat(t, 0), errConvertType},
		{"bool to string", true, "string", model.StringValue(""), errConvertType},
		{"int to bool", int32(1), "bool", model.BoolValue(false), errConvertType},
		{"float to bool", 1.0, "bool", model.BoolValue(false), errConvertType},
		{"string to string", "hello", "string", model.StringValue("hello"), nil},
		{"empty string to string", "", "string", model.StringValue(""), nil},
		{"string with UTF-8", "caffè", "string", model.StringValue("caffè"), nil},
		{"string invalid UTF-8", "a\xffb", "string", model.StringValue(""), errConvertString},
		{"string to int32", "12", "int32", model.IntValue(0), errConvertType},
		{"string to bool", "true", "bool", model.BoolValue(false), errConvertType},
		{"string to float64", "1.5", "float64", mustFloat(t, 0), errConvertType},
		{"int to string", int32(1), "string", model.StringValue(""), errConvertType},
		// arrays and other types
		{"[]int32 to int32", []int32{1, 2}, "int32", model.IntValue(0), errConvertType},
		{"[]int32 to string", []int32{1}, "string", model.StringValue(""), errConvertType},
		{"[]float64 to float64", []float64{1}, "float64", mustFloat(t, 0), errConvertType},
		{"[]string to string", []string{"a"}, "string", model.StringValue(""), errConvertType},
		{"empty []int32 to int32", []int32{}, "int32", model.IntValue(0), errConvertType},
		{"DateTime to int64", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), "int64", model.IntValue(0), errConvertType},
		{"DateTime to string", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), "string", model.StringValue(""), errConvertType},
		{"ByteString to string", []byte{1, 2}, "string", model.StringValue(""), errConvertType},
		{"StatusCode to uint32", ua.StatusBadTimeout, "uint32", model.IntValue(0), errConvertType},
		{"NodeId to string", ua.NewNumericNodeID(0, 85), "string", model.StringValue(""), errConvertType},
		// unknown point type
		{"unknown type", int32(1), "decimal", model.Value{}, errConvertPointType},
		{"empty type", int32(1), "", model.Value{}, errConvertPointType},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, q, err := Convert(goodDV(tc.in), tc.typ)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if q != model.Bad {
					t.Fatalf("quality = %v, want bad", q)
				}
				if got != tc.want {
					t.Fatalf("value = %+v, want zero of the kind %+v", got, tc.want)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if q != model.Good {
				t.Fatalf("quality = %v, want good", q)
			}
			if got != tc.want {
				t.Fatalf("value = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestConvertAcceptsEveryPointType(t *testing.T) {
	inputs := map[string]interface{}{
		"bool": true, "int16": int16(1), "uint16": uint16(1), "int32": int32(1), "uint32": uint32(1),
		"int64": int64(1), "uint64": uint64(1), "float32": float32(1), "float64": 1.0, "string": "x",
	}
	if len(inputs) != len(pointTypes) {
		t.Fatalf("test covers %d types, config knows %d", len(inputs), len(pointTypes))
	}

	for _, typ := range pointTypes {
		in, ok := inputs[typ]
		if !ok {
			t.Fatalf("no input for type %q", typ)
		}
		v, q, err := Convert(goodDV(in), typ)
		if err != nil || q != model.Good {
			t.Fatalf("%s: value %+v quality %v err %v", typ, v, q, err)
		}
		if err := v.Validate(); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
	}
}

func TestConvertMissingValue(t *testing.T) {
	statusOnly := func(sc ua.StatusCode) *ua.DataValue {
		return &ua.DataValue{EncodingMask: ua.DataValueStatusCode, Value: new(ua.Variant), Status: sc}
	}

	tests := []struct {
		name    string
		dv      *ua.DataValue
		typ     string
		want    model.Value
		wantQ   model.Quality
		wantErr error
	}{
		{"null variant, good", &ua.DataValue{EncodingMask: ua.DataValueValue, Value: new(ua.Variant)}, "int32", model.IntValue(0), model.Bad, errConvertMissing},
		{"null variant built, good", goodDV(nil), "float64", mustFloat(t, 0), model.Bad, errConvertMissing},
		{"no value flag, good", &ua.DataValue{Value: new(ua.Variant)}, "bool", model.BoolValue(false), model.Bad, errConvertMissing},
		{"nil variant, good", &ua.DataValue{EncodingMask: ua.DataValueValue}, "string", model.StringValue(""), model.Bad, errConvertMissing},
		{"value present but flag missing", &ua.DataValue{Value: ua.MustVariant(int32(5))}, "int32", model.IntValue(0), model.Bad, errConvertMissing},
		{"uncertain status, no value", statusOnly(ua.StatusUncertainInitialValue), "int32", model.IntValue(0), model.Bad, errConvertMissing},
		{"good status flag, no value", statusOnly(ua.StatusOK), "uint16", model.IntValue(0), model.Bad, errConvertMissing},
		{"bad status, no value: no error", statusOnly(ua.StatusBadNodeIDUnknown), "int32", model.IntValue(0), model.Bad, nil},
		{"bad status, no value, float", statusOnly(ua.StatusBadNodeIDUnknown), "float64", mustFloat(t, 0), model.Bad, nil},
		{"bad status, no value, bool", statusOnly(ua.StatusBadNodeIDUnknown), "bool", model.BoolValue(false), model.Bad, nil},
		{"bad status, no value, string", statusOnly(ua.StatusBadNodeIDUnknown), "string", model.StringValue(""), model.Bad, nil},
		{"bad status with a good-looking value: server quality wins", &ua.DataValue{
			EncodingMask: ua.DataValueValue | ua.DataValueStatusCode, Value: ua.MustVariant(int32(5)), Status: ua.StatusBadSensorFailure,
		}, "int32", model.IntValue(0), model.Bad, nil},
		{"bad status with a non-convertible value: still no error", &ua.DataValue{
			EncodingMask: ua.DataValueValue | ua.DataValueStatusCode, Value: ua.MustVariant("text"), Status: ua.StatusBadSensorFailure,
		}, "int32", model.IntValue(0), model.Bad, nil},
		{"reserved class 3 is bad", statusOnly(ua.StatusCode(0xC0000000)), "int32", model.IntValue(0), model.Bad, nil},
		{"nil data value", nil, "int32", model.IntValue(0), model.Bad, errConvertMissing},
		{"bad status, unknown type is still an error", statusOnly(ua.StatusBadNodeIDUnknown), "decimal", model.Value{}, model.Bad, errConvertPointType},
		{"uncertain with value converts", &ua.DataValue{
			EncodingMask: ua.DataValueValue | ua.DataValueStatusCode, Value: ua.MustVariant(int32(5)), Status: ua.StatusUncertainInitialValue,
		}, "int32", model.IntValue(5), model.Uncertain, nil},
		{"uncertain with bad conversion is bad with error", &ua.DataValue{
			EncodingMask: ua.DataValueValue | ua.DataValueStatusCode, Value: ua.MustVariant("x"), Status: ua.StatusUncertainInitialValue,
		}, "int32", model.IntValue(0), model.Bad, errConvertType},
		{"good local override keeps good", &ua.DataValue{
			EncodingMask: ua.DataValueValue | ua.DataValueStatusCode, Value: ua.MustVariant(int32(5)), Status: ua.StatusCode(0x00960000),
		}, "int32", model.IntValue(5), model.Good, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, q, err := Convert(tc.dv, tc.typ)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if q != tc.wantQ {
				t.Fatalf("quality = %v, want %v", q, tc.wantQ)
			}
			if got != tc.want {
				t.Fatalf("value = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestConvertErrorsNeverCarryTheValue(t *testing.T) {
	secret := "s3cr3t-value-90817"
	tests := []struct {
		in  interface{}
		typ string
	}{
		{secret, "int32"},
		{int64(90817) << 40, "int16"},
		{math.NaN(), "float64"},
		{"a\xff" + secret, "string"},
		{uint64(math.MaxUint64), "uint64"},
		{0.1, "float32"},
		{int64(1<<53) + 12345, "float64"},
	}

	for _, tc := range tests {
		_, _, err := Convert(goodDV(tc.in), tc.typ)
		if err == nil {
			t.Fatalf("%v to %s: want an error", tc.in, tc.typ)
		}
		for _, bad := range []string{secret, "90817", "12345", "0.1", "255"} {
			if strings.Contains(err.Error(), bad) {
				t.Fatalf("error %q contains %q", err, bad)
			}
		}
		if !strings.HasPrefix(err.Error(), "convert: ") {
			t.Fatalf("error %q lacks the fixed prefix", err)
		}
	}
}

func TestQuality(t *testing.T) {
	tests := []struct {
		sc   ua.StatusCode
		want model.Quality
	}{
		{0, model.Good},
		{0x00960000, model.Good},
		{0x40000000, model.Uncertain},
		{0x40920000, model.Uncertain},
		{0x80000000, model.Bad},
		{0x80340000, model.Bad},
		{0xC0000000, model.Bad},
		{0xFFFFFFFF, model.Bad},
	}

	for _, tc := range tests {
		if got := Quality(tc.sc); got != tc.want {
			t.Errorf("Quality(%#x) = %v, want %v", uint32(tc.sc), got, tc.want)
		}
	}
}

func TestTimestamp(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	src := time.Date(2026, 10, 21, 9, 29, 0, 0, time.UTC)
	srv := time.Date(2026, 10, 21, 9, 29, 30, 0, time.UTC)
	zone := time.FixedZone("x", 2*3600)

	tests := []struct {
		name string
		dv   *ua.DataValue
		now  time.Time
		want time.Time
	}{
		{"source present", &ua.DataValue{EncodingMask: ua.DataValueSourceTimestamp | ua.DataValueServerTimestamp, SourceTimestamp: src, ServerTimestamp: srv}, now, src},
		{"only source", &ua.DataValue{EncodingMask: ua.DataValueSourceTimestamp, SourceTimestamp: src}, now, src},
		{"only server", &ua.DataValue{EncodingMask: ua.DataValueServerTimestamp, ServerTimestamp: srv}, now, srv},
		{"source flag, zero source, server present", &ua.DataValue{EncodingMask: ua.DataValueSourceTimestamp | ua.DataValueServerTimestamp, ServerTimestamp: srv}, now, srv},
		{"source set but flag missing is ignored", &ua.DataValue{EncodingMask: ua.DataValueServerTimestamp, SourceTimestamp: src, ServerTimestamp: srv}, now, srv},
		{"neither", &ua.DataValue{}, now, now},
		{"flags but zero times", &ua.DataValue{EncodingMask: ua.DataValueSourceTimestamp | ua.DataValueServerTimestamp}, now, now},
		{"times without flags", &ua.DataValue{SourceTimestamp: src, ServerTimestamp: srv}, now, now},
		{"nil data value", nil, now, now},
		{"now in another zone is returned in UTC", &ua.DataValue{}, now.In(zone), now},
		{"source in another zone is returned in UTC", &ua.DataValue{EncodingMask: ua.DataValueSourceTimestamp, SourceTimestamp: src.In(zone)}, now, src},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Timestamp(tc.dv, tc.now)
			if !got.Equal(tc.want) {
				t.Fatalf("Timestamp = %v, want %v", got, tc.want)
			}
			if got.Location() != time.UTC {
				t.Fatalf("location = %v, want UTC", got.Location())
			}
		})
	}
}
