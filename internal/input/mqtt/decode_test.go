package mqtt

import (
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
)

var receivedAt = time.Date(2026, 11, 6, 10, 0, 0, 0, time.FixedZone("x", 3600))

func pt(signal, address, typ string, prio model.Priority) PlannedPoint {
	addr, err := ParseAddress(address)
	if err != nil {
		panic(err)
	}
	return PlannedPoint{Device: "d", Signal: signal, Point: config.Point{Signal: signal, Address: address, Type: typ, Priority: prio}, Path: addr.Path}
}

func single(address, typ string) *TopicPlan {
	return &TopicPlan{Topic: "t", Points: []PlannedPoint{pt("s", address, typ, model.Critical)}}
}

func TestDecodeJSONObjectThreePaths(t *testing.T) {
	plan := &TopicPlan{Topic: "t", Points: []PlannedPoint{
		pt("a", "t|a", "int32", model.Critical),
		pt("b", "t|b.c", "float64", model.Bulk),
		pt("c", "t|c.1.x", "string", model.Critical),
	}}
	payload := `{"a": 5, "b": {"c": 1.5}, "c": [{"x":"no"},{"x":"yes"}]}`
	samples, errs := Decode(plan, []byte(payload), receivedAt)
	if len(errs) != 0 || len(samples) != 3 {
		t.Fatalf("samples=%v errs=%v", samples, errs)
	}
	want := []model.Value{model.IntValue(5), {Kind: model.KindFloat, Float: 1.5}, model.StringValue("yes")}
	prios := []model.Priority{model.Critical, model.Bulk, model.Critical}
	for i, s := range samples {
		if s.Value != want[i] || s.Quality != model.Good || s.Priority != prios[i] || !s.TS.Equal(receivedAt) || s.TS.Location() != time.UTC {
			t.Errorf("sample %d = %+v", i, s)
		}
		if !s.TS.Equal(samples[0].TS) {
			t.Errorf("timestamps differ")
		}
	}
}

func TestDecodeMissingPath(t *testing.T) {
	for _, payload := range []string{`{}`, `{"a":{"b":1}}`, `{"a":[1]}`, `{"a":5}`, `[1]`, `7`} {
		samples, errs := Decode(single("t|a.b.c", "int32"), []byte(payload), receivedAt)
		if len(samples) != 0 || len(errs) != 0 {
			t.Errorf("%s: samples=%v errs=%v", payload, samples, errs)
		}
	}
}

func TestDecodeIndexOutOfRange(t *testing.T) {
	samples, errs := Decode(single("t|a.5", "int32"), []byte(`{"a":[1]}`), receivedAt)
	if len(samples) != 0 || len(errs) != 0 {
		t.Fatal(samples, errs)
	}
}

func TestDecodeConversions(t *testing.T) {
	big := "18446744073709551615"
	tests := []struct {
		name    string
		address string
		typ     string
		payload string
		want    model.Value
		bad     bool
	}{
		// JSON values
		{"int ok", "t|v", "int32", `{"v":12}`, model.IntValue(12), false},
		{"int16 max", "t|v", "int16", `{"v":32767}`, model.IntValue(32767), false},
		{"int16 overflow", "t|v", "int16", `{"v":70000}`, model.IntValue(0), true},
		{"uint16 negative", "t|v", "uint16", `{"v":-1}`, model.IntValue(0), true},
		{"uint16 max", "t|v", "uint16", `{"v":65535}`, model.IntValue(65535), false},
		{"uint32 max", "t|v", "uint32", `{"v":4294967295}`, model.IntValue(4294967295), false},
		{"uint32 over", "t|v", "uint32", `{"v":4294967296}`, model.IntValue(0), true},
		{"int32 min", "t|v", "int32", `{"v":-2147483648}`, model.IntValue(-2147483648), false},
		{"int64 exact", "t|v", "int64", `{"v":9223372036854775807}`, model.IntValue(9223372036854775807), false},
		{"uint64 within int64", "t|v", "uint64", `{"v":9223372036854775807}`, model.IntValue(9223372036854775807), false},
		{"uint64 above int64", "t|v", "uint64", `{"v":9223372036854775808}`, model.IntValue(0), true},
		{"twenty digits", "t|v", "int64", `{"v":` + big + `}`, model.IntValue(0), true},
		{"fraction into int", "t|v", "int32", `{"v":3.5}`, model.IntValue(0), true},
		{"3.0 into int", "t|v", "int32", `{"v":3.0}`, model.IntValue(0), true},
		{"1e3 into int", "t|v", "int32", `{"v":1e3}`, model.IntValue(0), true},
		{"float", "t|v", "float64", `{"v":3.5}`, model.Value{Kind: model.KindFloat, Float: 3.5}, false},
		{"int into float", "t|v", "float32", `{"v":3}`, model.Value{Kind: model.KindFloat, Float: 3}, false},
		{"1e400 into float64", "t|v", "float64", `{"v":1e400}`, model.Value{Kind: model.KindFloat}, true},
		{"float32 overflow", "t|v", "float32", `{"v":1e39}`, model.Value{Kind: model.KindFloat}, true},
		{"bool true", "t|v", "bool", `{"v":true}`, model.BoolValue(true), false},
		{"bool into int", "t|v", "int32", `{"v":true}`, model.IntValue(0), true},
		{"number into bool", "t|v", "bool", `{"v":1}`, model.BoolValue(false), true},
		{"string", "t|v", "string", `{"v":"RUN"}`, model.StringValue("RUN"), false},
		{"string into int", "t|v", "int32", `{"v":"12"}`, model.IntValue(0), true},
		{"number into string", "t|v", "string", `{"v":12}`, model.StringValue(""), true},
		{"null", "t|v", "int32", `{"v":null}`, model.IntValue(0), true},
		{"object at path", "t|v", "int32", `{"v":{"a":1}}`, model.IntValue(0), true},
		{"list at path", "t|v", "string", `{"v":[1]}`, model.StringValue(""), true},
		// plain payloads
		{"plain bool TRUE", "t", "bool", "TRUE", model.BoolValue(true), false},
		{"plain bool on", "t", "bool", "On", model.BoolValue(true), false},
		{"plain bool 1", "t", "bool", "1", model.BoolValue(true), false},
		{"plain bool off", "t", "bool", "OFF", model.BoolValue(false), false},
		{"plain bool 0", "t", "bool", "0", model.BoolValue(false), false},
		{"plain bool false", "t", "bool", "False", model.BoolValue(false), false},
		{"plain bool yes", "t", "bool", "yes", model.BoolValue(false), true},
		{"plain string", "t", "string", "RUN", model.StringValue("RUN"), false},
		{"plain string untrimmed", "t", "string", " RUN\n", model.StringValue(" RUN\n"), false},
		{"plain string json-looking", "t", "string", `{"a":1}`, model.StringValue(`{"a":1}`), false},
		{"plain int32", "t", "int32", "12", model.IntValue(12), false},
		{"plain int trimmed", "t", "int32", " 12\n", model.IntValue(12), false},
		{"plain int fraction", "t", "int32", "3.0", model.IntValue(0), true},
		{"plain int16 overflow", "t", "int16", "70000", model.IntValue(0), true},
		{"plain float", "t", "float64", "2.5", model.Value{Kind: model.KindFloat, Float: 2.5}, false},
		{"plain NaN", "t", "float64", "NaN", model.Value{Kind: model.KindFloat}, true},
		{"plain Inf", "t", "float64", "Inf", model.Value{Kind: model.KindFloat}, true},
		{"plain text into int", "t", "int32", "abc", model.IntValue(0), true},
		{"plain empty into int", "t", "int32", "", model.IntValue(0), true},
		{"plain invalid utf8 string", "t", "string", "\xff", model.StringValue(""), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			samples, errs := Decode(single(tc.address, tc.typ), []byte(tc.payload), receivedAt)
			if len(samples) != 1 {
				t.Fatalf("samples=%v errs=%v", samples, errs)
			}
			s := samples[0]
			if s.Value != tc.want {
				t.Errorf("value = %+v want %+v", s.Value, tc.want)
			}
			if tc.bad {
				if s.Quality != model.Bad || len(errs) != 1 || errs[0] != KindConversion {
					t.Errorf("quality=%v errs=%v", s.Quality, errs)
				}
			} else if s.Quality != model.Good || len(errs) != 0 {
				t.Errorf("quality=%v errs=%v", s.Quality, errs)
			}
		})
	}
}

func TestDecodeMalformedJSON(t *testing.T) {
	plan := &TopicPlan{Topic: "t", Points: []PlannedPoint{
		pt("a", "t|a", "int32", model.Critical),
		pt("b", "t|b", "string", model.Bulk),
		pt("raw", "t", "string", model.Critical),
	}}
	for _, payload := range []string{`{"a":`, `RUN`, ``, `{"a":1} x`, `{"a":1}{"a":2}`} {
		samples, errs := Decode(plan, []byte(payload), receivedAt)
		if len(samples) != 3 {
			t.Fatalf("%q: samples=%v", payload, samples)
		}
		if samples[0].Quality != model.Bad || samples[0].Value != model.IntValue(0) ||
			samples[1].Quality != model.Bad || samples[1].Value != model.StringValue("") || samples[1].Priority != model.Bulk {
			t.Errorf("%q: path samples %+v %+v", payload, samples[0], samples[1])
		}
		if samples[2].Quality != model.Good || samples[2].Value != model.StringValue(payload) {
			t.Errorf("%q: plain sample %+v", payload, samples[2])
		}
		if len(errs) != 1 || errs[0] != KindMalformedJSON {
			t.Errorf("%q: errs=%v", payload, errs)
		}
	}
}

func TestDecodePlainOnlyTopicNeverParsesJSON(t *testing.T) {
	samples, errs := Decode(single("t", "string"), []byte(`{"broken`), receivedAt)
	if len(samples) != 1 || len(errs) != 0 || samples[0].Quality != model.Good {
		t.Fatal(samples, errs)
	}
}

func TestDecodeTimestamp(t *testing.T) {
	want := time.Date(2026, 11, 6, 9, 30, 15, 0, time.UTC)
	tests := []struct {
		name    string
		spec    TimestampSpec
		payload string
		want    time.Time
		ok      bool
	}{
		{"rfc3339 utc", TimestampSpec{"ts", TimestampRFC3339}, `{"ts":"2026-11-06T09:30:15Z","v":1}`, want, true},
		{"rfc3339 offset", TimestampSpec{"ts", TimestampRFC3339}, `{"ts":"2026-11-06T10:30:15+01:00","v":1}`, want, true},
		{"rfc3339 fraction", TimestampSpec{"ts", TimestampRFC3339}, `{"ts":"2026-11-06T09:30:15.5Z","v":1}`, want.Add(500 * time.Millisecond), true},
		{"rfc3339 bad text", TimestampSpec{"ts", TimestampRFC3339}, `{"ts":"yesterday","v":1}`, time.Time{}, false},
		{"rfc3339 number", TimestampSpec{"ts", TimestampRFC3339}, `{"ts":5,"v":1}`, time.Time{}, false},
		{"unix_s int", TimestampSpec{"ts", TimestampUnixSeconds}, `{"ts":1793957415,"v":1}`, want, true},
		{"unix_s fractional", TimestampSpec{"ts", TimestampUnixSeconds}, `{"ts":1793957415.25,"v":1}`, want.Add(250 * time.Millisecond), true},
		{"unix_s text", TimestampSpec{"ts", TimestampUnixSeconds}, `{"ts":"1793957415","v":1}`, time.Time{}, false},
		{"unix_s huge", TimestampSpec{"ts", TimestampUnixSeconds}, `{"ts":1e30,"v":1}`, time.Time{}, false},
		{"unix_s huge int", TimestampSpec{"ts", TimestampUnixSeconds}, `{"ts":9223372036854775807,"v":1}`, time.Time{}, false},
		{"unix_ms int", TimestampSpec{"ts", TimestampUnixMillis}, `{"ts":1793957415000,"v":1}`, want, true},
		{"unix_ms int millis", TimestampSpec{"ts", TimestampUnixMillis}, `{"ts":1793957415123,"v":1}`, want.Add(123 * time.Millisecond), true},
		{"unix_ms fractional", TimestampSpec{"ts", TimestampUnixMillis}, `{"ts":1793957415000.5,"v":1}`, want.Add(500 * time.Microsecond), true},
		{"nested path", TimestampSpec{"meta.ts", TimestampUnixSeconds}, `{"meta":{"ts":1793957415},"v":1}`, want, true},
		{"missing field", TimestampSpec{"ts", TimestampUnixSeconds}, `{"v":1}`, time.Time{}, false},
		{"malformed json", TimestampSpec{"ts", TimestampUnixSeconds}, `{"v":`, time.Time{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			plan := &TopicPlan{Topic: "t", Timestamp: &spec, Points: []PlannedPoint{pt("v", "t|v", "int32", model.Critical)}}
			samples, errs := Decode(plan, []byte(tc.payload), receivedAt)
			if len(samples) != 1 {
				t.Fatalf("samples=%v errs=%v", samples, errs)
			}
			s := samples[0]
			if tc.ok {
				if !s.TS.Equal(tc.want) || s.TS.Location() != time.UTC || s.Quality != model.Good || len(errs) != 0 {
					t.Errorf("sample=%+v errs=%v want ts %v", s, errs, tc.want)
				}
				return
			}
			if s.Quality != model.Bad || s.Value != model.IntValue(0) || !s.TS.Equal(receivedAt) || s.TS.Location() != time.UTC {
				t.Errorf("sample=%+v", s)
			}
			found := false
			for _, e := range errs {
				if e == KindBadTimestamp {
					found = true
				}
			}
			if !found {
				t.Errorf("errs=%v", errs)
			}
		})
	}
}

func TestDecodeBadTimestampMakesEveryPointBad(t *testing.T) {
	spec := &TimestampSpec{"ts", TimestampUnixSeconds}
	plan := &TopicPlan{Topic: "t", Timestamp: spec, Points: []PlannedPoint{
		pt("a", "t|a", "int32", model.Critical),
		pt("raw", "t", "string", model.Bulk),
	}}
	// Valid JSON, no timestamp field: both points bad, kind bad_timestamp only.
	samples, errs := Decode(plan, []byte(`{"a":1}`), receivedAt)
	if len(samples) != 2 || samples[0].Quality != model.Bad || samples[1].Quality != model.Bad || samples[1].Priority != model.Bulk {
		t.Fatalf("samples=%+v", samples)
	}
	if len(errs) != 1 || errs[0] != KindBadTimestamp {
		t.Errorf("errs=%v", errs)
	}
	// Not JSON: bad_timestamp plus malformed_json (the topic has a path point).
	samples, errs = Decode(plan, []byte(`RUN`), receivedAt)
	if len(samples) != 2 || samples[0].Quality != model.Bad || samples[1].Quality != model.Bad {
		t.Fatalf("samples=%+v", samples)
	}
	if strings.Join(errs, ",") != KindMalformedJSON+","+KindBadTimestamp {
		t.Errorf("errs=%v", errs)
	}
	// Only plain points: bad_timestamp alone.
	plain := &TopicPlan{Topic: "t", Timestamp: spec, Points: []PlannedPoint{pt("raw", "t", "string", model.Critical)}}
	_, errs = Decode(plain, []byte(`RUN`), receivedAt)
	if len(errs) != 1 || errs[0] != KindBadTimestamp {
		t.Errorf("errs=%v", errs)
	}
}

func TestDecodeNoTimestampSpecUsesReception(t *testing.T) {
	samples, _ := Decode(single("t|v", "int32"), []byte(`{"v":1}`), receivedAt)
	if !samples[0].TS.Equal(receivedAt) || samples[0].TS.Location() != time.UTC {
		t.Errorf("ts=%v", samples[0].TS)
	}
}

func TestDecodeTimestampWithPlainPointOnJSONPayload(t *testing.T) {
	spec := &TimestampSpec{"ts", TimestampUnixSeconds}
	plan := &TopicPlan{Topic: "t", Timestamp: spec, Points: []PlannedPoint{pt("raw", "t", "string", model.Critical)}}
	samples, errs := Decode(plan, []byte(`{"ts":1793957415}`), receivedAt)
	if len(errs) != 0 || len(samples) != 1 || samples[0].Value != model.StringValue(`{"ts":1793957415}`) {
		t.Fatal(samples, errs)
	}
}

func TestDecodeSeveralFailuresOneKindEach(t *testing.T) {
	plan := &TopicPlan{Topic: "t", Points: []PlannedPoint{
		pt("a", "t|a", "int16", model.Critical),
		pt("b", "t|b", "int16", model.Critical),
		pt("c", "t|c", "int16", model.Critical),
	}}
	samples, errs := Decode(plan, []byte(`{"a":70000,"b":1.5,"c":1}`), receivedAt)
	if len(samples) != 3 || len(errs) != 2 || errs[0] != KindConversion || errs[1] != KindConversion {
		t.Fatalf("samples=%+v errs=%v", samples, errs)
	}
}

func input(devices ...config.Device) config.Input { return config.Input{ID: "in", Devices: devices} }

func TestPlanGroupsByTopicAcrossDevices(t *testing.T) {
	cfg := input(
		config.Device{Device: "m1", Points: []config.Point{
			{Signal: "a", Address: "plant/m1|a", Type: "int32", Priority: model.Critical},
			{Signal: "b", Address: "plant/m1|b.0", Type: "float64", Priority: model.Bulk},
			{Signal: "state", Address: "plant/state", Type: "string", Priority: model.Critical},
		}},
		config.Device{Device: "m2", Points: []config.Point{
			{Signal: "a", Address: "plant/m1|c", Type: "bool", Priority: model.Critical},
		}},
	)
	conn := Connection{Timestamp: &TimestampSpec{"ts", TimestampUnixSeconds}}
	plans, err := Plan(cfg, conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans=%v", plans)
	}
	p := plans["plant/m1"]
	if p.Topic != "plant/m1" || len(p.Points) != 3 || p.Timestamp != conn.Timestamp {
		t.Fatalf("plan=%+v", p)
	}
	if p.Points[0].Device != "m1" || p.Points[2].Device != "m2" || p.Points[1].Path[0] != "b" || p.Points[1].Path[1] != "0" {
		t.Errorf("points=%+v", p.Points)
	}
	if st := plans["plant/state"]; len(st.Points) != 1 || st.Points[0].Path != nil {
		t.Errorf("state=%+v", st)
	}
}

func TestPlanTopicOverrideTimestamp(t *testing.T) {
	cfg := input(config.Device{Device: "m", Points: []config.Point{
		{Signal: "a", Address: "t1|a", Type: "int32"},
		{Signal: "b", Address: "t2|a", Type: "int32"},
		{Signal: "c", Address: "t3|a", Type: "int32"},
	}})
	global := &TimestampSpec{"g", TimestampRFC3339}
	override := &TimestampSpec{"o", TimestampUnixMillis}
	conn := Connection{Timestamp: global, Topics: map[string]TopicOverride{"t1": {Timestamp: override}, "t3": {}}}
	plans, err := Plan(cfg, conn)
	if err != nil {
		t.Fatal(err)
	}
	if plans["t1"].Timestamp != override || plans["t2"].Timestamp != global || plans["t3"].Timestamp != global {
		t.Errorf("t1=%v t2=%v t3=%v", plans["t1"].Timestamp, plans["t2"].Timestamp, plans["t3"].Timestamp)
	}
	plans, _ = Plan(cfg, Connection{})
	if plans["t1"].Timestamp != nil {
		t.Error("timestamp without spec must be nil")
	}
}

func TestPlanBadAddress(t *testing.T) {
	cfg := input(config.Device{Device: "m", Points: []config.Point{
		{Signal: "ok", Address: "t|a", Type: "int32"},
		{Signal: "bad", Address: "secret/+/topic|a", Type: "int32"},
	}})
	_, err := Plan(cfg, Connection{})
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "devices[0].points[1]") || strings.Contains(err.Error(), "secret") {
		t.Errorf("err=%v", err)
	}
}
