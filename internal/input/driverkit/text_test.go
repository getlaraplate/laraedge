package driverkit

import (
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

func TestConvertText(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		text string
		want model.Value
		ok   bool
	}{
		{"string as is", "string", " a b ", model.StringValue(" a b "), true},
		{"string invalid utf8", "string", "\xff", model.Value{}, false},
		{"bool true", "bool", " TRUE ", model.BoolValue(true), true},
		{"bool on", "bool", "on", model.BoolValue(true), true},
		{"bool 0", "bool", "0", model.BoolValue(false), true},
		{"bool junk", "bool", "yes", model.Value{}, false},
		{"int16 trimmed", "int16", " 12 ", model.IntValue(12), true},
		{"int16 plus", "int16", "+12", model.IntValue(12), true},
		{"int16 range", "int16", "32768", model.Value{}, false},
		{"uint16 negative", "uint16", "-1", model.Value{}, false},
		{"int32 float form", "int32", "3.0", model.Value{}, false},
		{"int32 exponent", "int32", "1e3", model.Value{}, false},
		{"uint64 above int64", "uint64", "9223372036854775808", model.Value{}, false},
		{"hex refused", "float64", "0x1p3", model.Value{}, false},
		{"inf refused", "float64", "inf", model.Value{}, false},
		{"underscore refused", "int32", "1_0", model.Value{}, false},
		{"leading zero refused", "int32", "012", model.Value{}, false},
		{"bare dot refused", "float64", "1.", model.Value{}, false},
		{"float64 exponent", "float64", "1.5e2", mustFloat(t, 150), true},
		{"float32 overflow", "float32", "1e39", model.Value{}, false},
		{"float32 underflow", "float32", "1e-50", model.Value{}, false},
		{"float32 zero", "float32", "0", mustFloat(t, 0), true},
		{"unknown type", "blob", "1", model.Value{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ConvertText(tt.typ, tt.text)
			if ok != tt.ok || (ok && got != tt.want) {
				t.Fatalf("ConvertText(%q, %q) = %v, %v; want %v, %v", tt.typ, tt.text, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func mustFloat(t *testing.T, f float64) model.Value {
	t.Helper()
	v, err := model.FloatValue(f)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestConvertNumber(t *testing.T) {
	if v, ok := ConvertNumber("int64", "-5"); !ok || v != model.IntValue(-5) {
		t.Fatalf("got %v, %v", v, ok)
	}
	if _, ok := ConvertNumber("string", "1"); ok {
		t.Fatal("string is not numeric")
	}
}

func TestZeroValue(t *testing.T) {
	if v := ZeroValue("bool"); v != model.BoolValue(false) {
		t.Fatalf("bool: %v", v)
	}
	for _, typ := range []string{"int16", "uint16", "int32", "uint32", "int64", "uint64"} {
		if v := ZeroValue(typ); v != model.IntValue(0) {
			t.Fatalf("%s: %v", typ, v)
		}
	}
	for _, typ := range []string{"float32", "float64"} {
		if v := ZeroValue(typ); v.Kind != model.KindFloat {
			t.Fatalf("%s: %v", typ, v)
		}
	}
	if v := ZeroValue("string"); v != model.StringValue("") {
		t.Fatalf("string: %v", v)
	}
}

func TestParseUnix(t *testing.T) {
	got, ok := ParseUnix("1700000000", false)
	if !ok || !got.Equal(time.Unix(1700000000, 0)) || got.Location() != time.UTC {
		t.Fatalf("seconds: %v, %v", got, ok)
	}
	got, ok = ParseUnix("1700000000123", true)
	if !ok || !got.Equal(time.Unix(1700000000, 123e6)) {
		t.Fatalf("millis: %v, %v", got, ok)
	}
	got, ok = ParseUnix("1700000000.5", false)
	if !ok || !got.Equal(time.Unix(1700000000, 5e8)) {
		t.Fatalf("fraction: %v, %v", got, ok)
	}
	if _, ok = ParseUnix("253402300800", false); ok {
		t.Fatal("above year 9999 accepted")
	}
	if _, ok = ParseUnix("abc", false); ok {
		t.Fatal("junk accepted")
	}
	if _, ok = ParseUnix("NaN", false); ok {
		t.Fatal("NaN accepted")
	}
}
