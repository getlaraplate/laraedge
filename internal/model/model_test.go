package model_test

import (
	"math"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

func TestFloatValueRefusesNonFinite(t *testing.T) {
	tests := []struct {
		name    string
		in      float64
		wantErr bool
	}{
		{"finite", 12.5, false},
		{"zero", 0, false},
		{"negative", -3.25, false},
		{"nan", math.NaN(), true},
		{"positive infinity", math.Inf(1), true},
		{"negative infinity", math.Inf(-1), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := model.FloatValue(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("FloatValue(%v) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}

			if err != nil {
				return
			}

			if v.Kind != model.KindFloat || v.Float != tc.in {
				t.Fatalf("FloatValue(%v) = %+v", tc.in, v)
			}

			if err := v.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestValueConstructorsHoldExactlyOneKind(t *testing.T) {
	f, _ := model.FloatValue(1.5)
	tests := []struct {
		name  string
		value model.Value
		kind  model.ValueKind
	}{
		{"float", f, model.KindFloat},
		{"int", model.IntValue(-7), model.KindInt},
		{"bool", model.BoolValue(true), model.KindBool},
		{"string", model.StringValue("RUN"), model.KindString},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value.Kind != tc.kind {
				t.Fatalf("kind = %v, want %v", tc.value.Kind, tc.kind)
			}

			if err := tc.value.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestValueValidateRejectsInvalid(t *testing.T) {
	tests := []struct {
		name  string
		value model.Value
	}{
		{"zero value", model.Value{}},
		{"unknown kind", model.Value{Kind: model.ValueKind(99)}},
		{"hand built nan", model.Value{Kind: model.KindFloat, Float: math.NaN()}},
		{"hand built inf", model.Value{Kind: model.KindFloat, Float: math.Inf(1)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.value.Validate(); err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}

func TestQualityStringAndParse(t *testing.T) {
	tests := []struct {
		quality model.Quality
		wire    string
	}{
		{model.Good, "good"},
		{model.Uncertain, "uncertain"},
		{model.Bad, "bad"},
	}

	for _, tc := range tests {
		t.Run(tc.wire, func(t *testing.T) {
			if got := tc.quality.String(); got != tc.wire {
				t.Fatalf("String() = %q, want %q", got, tc.wire)
			}

			got, err := model.ParseQuality(tc.wire)
			if err != nil || got != tc.quality {
				t.Fatalf("ParseQuality(%q) = %v, %v", tc.wire, got, err)
			}
		})
	}

	for _, in := range []string{"", "GOOD", "fine"} {
		if _, err := model.ParseQuality(in); err == nil {
			t.Fatalf("ParseQuality(%q) = nil error", in)
		}
	}
}

func TestPriorityStringAndParse(t *testing.T) {
	tests := []struct {
		priority model.Priority
		wire     string
	}{
		{model.Critical, "critical"},
		{model.Bulk, "bulk"},
	}

	for _, tc := range tests {
		t.Run(tc.wire, func(t *testing.T) {
			if got := tc.priority.String(); got != tc.wire {
				t.Fatalf("String() = %q, want %q", got, tc.wire)
			}

			got, err := model.ParsePriority(tc.wire)
			if err != nil || got != tc.priority {
				t.Fatalf("ParsePriority(%q) = %v, %v", tc.wire, got, err)
			}
		})
	}

	for _, in := range []string{"", "Critical", "low"} {
		if _, err := model.ParsePriority(in); err == nil {
			t.Fatalf("ParsePriority(%q) = nil error", in)
		}
	}
}

func TestInvalidEnumsHaveNoWireString(t *testing.T) {
	if got := model.Quality(0).String(); got == "good" || got == "uncertain" || got == "bad" {
		t.Fatalf("zero Quality renders as a valid wire string %q", got)
	}

	if got := model.Priority(0).String(); got == "critical" || got == "bulk" {
		t.Fatalf("zero Priority renders as a valid wire string %q", got)
	}
}

func TestRecordAccessors(t *testing.T) {
	ts := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	bulkSample := model.Sample{
		Device: "press-07", Signal: "temp", TS: ts,
		Value: model.IntValue(1), Quality: model.Good, Priority: model.Bulk,
	}
	critSample := bulkSample
	critSample.Priority = model.Critical

	tests := []struct {
		name         string
		record       model.Record
		wantDevice   string
		wantPriority model.Priority
		wantValid    bool
	}{
		{"bulk sample", model.SampleRecord(bulkSample), "press-07", model.Bulk, true},
		{"critical sample", model.SampleRecord(critSample), "press-07", model.Critical, true},
		{"birth", model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: "press-08", TS: ts, Signals: []model.SignalInfo{{Signal: "s", DataType: model.DataTypeString}}}), "press-08", model.Critical, true},
		{"death", model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "press-09", TS: ts}), "press-09", model.Critical, true},
		{"empty", model.Record{}, "", 0, false},
		{"both set", model.Record{Sample: &bulkSample, Event: &model.DeviceEvent{Kind: model.Death, Device: "x"}}, "", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.record.Validate()
			if (err == nil) != tc.wantValid {
				t.Fatalf("Validate() = %v, wantValid %v", err, tc.wantValid)
			}

			if !tc.wantValid {
				return
			}

			if got := tc.record.Device(); got != tc.wantDevice {
				t.Fatalf("Device() = %q, want %q", got, tc.wantDevice)
			}

			if got := tc.record.Priority(); got != tc.wantPriority {
				t.Fatalf("Priority() = %v, want %v", got, tc.wantPriority)
			}
		})
	}
}

func TestSampleRecordCopiesItsInput(t *testing.T) {
	s := model.Sample{Device: "a", Signal: "s", Value: model.IntValue(1), Quality: model.Good, Priority: model.Bulk}
	r := model.SampleRecord(s)
	s.Device = "changed"

	if r.Sample.Device != "a" {
		t.Fatalf("record aliases the caller's sample: %q", r.Sample.Device)
	}
}

func TestEventKindString(t *testing.T) {
	if model.Birth.String() != "birth" || model.Death.String() != "death" {
		t.Fatalf("got %q, %q", model.Birth, model.Death)
	}
}

func TestSignalDataTypeValid(t *testing.T) {
	for _, dt := range []string{model.DataTypeNumber, model.DataTypeBoolean, model.DataTypeString} {
		if err := (model.SignalInfo{Signal: "s", DataType: dt}).Validate(); err != nil {
			t.Fatalf("data type %q refused: %v", dt, err)
		}
	}

	bad := []model.SignalInfo{
		{Signal: "s", DataType: "int"},
		{Signal: "s"},
		{DataType: model.DataTypeNumber},
	}
	for _, si := range bad {
		if err := si.Validate(); err == nil {
			t.Fatalf("%+v accepted", si)
		}
	}
}
