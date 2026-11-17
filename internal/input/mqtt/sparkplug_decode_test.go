package mqtt

import (
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

func ptr[T any](v T) *T { return &v }

func TestDecodePayloadRoundTrip(t *testing.T) {
	p := mqtttest.SparkplugPayload{
		Timestamp: ptr(uint64(1700000000000)),
		Seq:       ptr(uint64(7)),
		Metrics: []mqtttest.SparkplugMetric{
			{Name: ptr("a"), Alias: ptr(uint64(5)), Timestamp: ptr(uint64(42)), Datatype: ptr(uint32(3)), Historical: true, Int: ptr(uint32(9)), Quality: ptr(int64(192))},
			{Name: ptr("b"), Datatype: ptr(uint32(4)), Long: ptr(uint64(1 << 63)), Null: true},
			{Alias: ptr(uint64(6)), Datatype: ptr(uint32(9)), Float: ptr(float32(1.5))},
			{Name: ptr("d"), Datatype: ptr(uint32(10)), Double: ptr(2.25)},
			{Name: ptr("e"), Datatype: ptr(uint32(11)), Bool: ptr(true)},
			{Name: ptr("f"), Datatype: ptr(uint32(12)), String: ptr("hi")},
			{Name: ptr("g"), Datatype: ptr(uint32(17)), Bytes: []byte{1, 2}},
			mqtttest.BdSeq(3),
		},
	}
	raw := p.Encode()
	// Unknown fields of every wire type are skipped: uuid 4, body 5, and
	// numbers nothing defines.
	raw = protowire.AppendTag(raw, 4, protowire.BytesType)
	raw = protowire.AppendString(raw, "uuid")
	raw = protowire.AppendTag(raw, 99, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 1)
	raw = protowire.AppendTag(raw, 98, protowire.Fixed64Type)
	raw = protowire.AppendFixed64(raw, 1)
	raw = protowire.AppendTag(raw, 97, protowire.Fixed32Type)
	raw = protowire.AppendFixed32(raw, 1)

	got, err := decodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasTimestamp || got.Timestamp != 1700000000000 || !got.HasSeq || got.Seq != 7 || len(got.Metrics) != 8 {
		t.Fatalf("payload = %+v", got)
	}
	m := got.Metrics[0]
	if m.Name != "a" || !m.HasName || m.Alias != 5 || !m.HasAlias || m.Timestamp != 42 || !m.HasTimestamp ||
		m.Datatype != 3 || !m.Historical || m.Null || !m.HasQuality || m.Quality != 192 ||
		m.Value.Field != 10 || m.Value.Uint != 9 {
		t.Fatalf("metric 0 = %+v", m)
	}
	if m := got.Metrics[1]; !m.Null || m.Value.Field != 11 || m.Value.Uint != 1<<63 || m.HasAlias || m.HasTimestamp {
		t.Fatalf("metric 1 = %+v", m)
	}
	if m := got.Metrics[2]; m.HasName || m.Alias != 6 || m.Value.Field != 12 || m.Value.Float64 != 1.5 {
		t.Fatalf("metric 2 = %+v", m)
	}
	if m := got.Metrics[3]; m.Value.Field != 13 || m.Value.Float64 != 2.25 {
		t.Fatalf("metric 3 = %+v", m)
	}
	if m := got.Metrics[4]; m.Value.Field != 14 || !m.Value.Bool {
		t.Fatalf("metric 4 = %+v", m)
	}
	if m := got.Metrics[5]; m.Value.Field != 15 || m.Value.Text != "hi" {
		t.Fatalf("metric 5 = %+v", m)
	}
	if m := got.Metrics[6]; m.Value.Field != 16 {
		t.Fatalf("metric 6 = %+v", m)
	}
	if m := got.Metrics[7]; m.Name != "bdSeq" || m.Datatype != 4 || m.Value.Uint != 3 {
		t.Fatalf("bdSeq = %+v", m)
	}
}

func TestDecodePayloadErrors(t *testing.T) {
	cases := map[string][]byte{
		"truncated varint":          {0x08, 0x80},
		"truncated tag":             {0x80},
		"length beyond buffer":      {0x12, 0x05, 0x0a},
		"group wire type":           {0x1b, 0x1c},
		"unknown group":             protowire.AppendTag(nil, 50, protowire.StartGroupType),
		"timestamp wrong wire type": {0x0a, 0x01, 0x00},
		"metric truncated":          {0x12, 0x02, 0x08, 0x80},
		"metric name wire type":     {0x12, 0x02, 0x08, 0x01},
		"metric group":              {0x12, 0x01, 0x0b},
		"fixed32 truncated":         {0x12, 0x03, 0x65, 0x00, 0x00},
		"properties truncated":      {0x12, 0x03, 0x4a, 0x01, 0x0a},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodePayload(b); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	if p, err := decodePayload(nil); err != nil || len(p.Metrics) != 0 {
		t.Fatalf("empty payload = %+v, %v", p, err)
	}
}

func TestMetricValue(t *testing.T) {
	i := func(f protowire.Number, u uint64) spValue { return spValue{Field: f, Uint: u} }
	f := func(field protowire.Number, v float64) spValue { return spValue{Field: field, Float64: v} }
	cases := []struct {
		name  string
		dt    uint32
		value spValue
		typ   string
		want  model.Value
		kind  string
	}{
		{"int8 sign extended", 1, i(10, 0xFFFFFF80), "int16", model.IntValue(-128), ""},
		{"int8 narrow", 1, i(10, 0x80), "int16", model.IntValue(-128), ""},
		{"int8 positive", 1, i(10, 127), "int32", model.IntValue(127), ""},
		{"int16 sign extended", 2, i(10, 0xFFFF8000), "int16", model.IntValue(-32768), ""},
		{"int32 negative", 3, i(10, 0xFFFFFFFF), "int32", model.IntValue(-1), ""},
		{"int32 negative into uint", 3, i(10, 0xFFFFFFFF), "uint32", model.Value{}, KindConversion},
		{"uint8", 5, i(10, 200), "uint16", model.IntValue(200), ""},
		{"uint16 into int16 out of range", 6, i(10, 40000), "int16", model.Value{}, KindConversion},
		{"uint32 into int32 out of range", 7, i(10, 0xFFFFFFFF), "int32", model.Value{}, KindConversion},
		{"uint32 into int64", 7, i(10, 0xFFFFFFFF), "int64", model.IntValue(4294967295), ""},
		{"int64 min", 4, i(11, 1<<63), "int64", model.IntValue(math.MinInt64), ""},
		{"int64 into int32 out of range", 4, i(11, 1<<40), "int32", model.Value{}, KindConversion},
		{"uint64 above int64", 8, i(11, 1<<63), "uint64", model.Value{}, KindConversion},
		{"uint64 fits", 8, i(11, 5), "uint64", model.IntValue(5), ""},
		{"float into float64", 9, f(12, 1.5), "float64", model.Value{Kind: model.KindFloat, Float: 1.5}, ""},
		{"float fraction into int32", 9, f(12, 1.5), "int32", model.Value{}, KindConversion},
		{"float whole into int32", 9, f(12, 3.0), "int32", model.IntValue(3), ""},
		{"double into float32", 10, f(13, 2.5), "float32", model.Value{Kind: model.KindFloat, Float: 2.5}, ""},
		{"double too large for float32", 10, f(13, 1e300), "float32", model.Value{}, KindConversion},
		{"double NaN", 10, f(13, math.NaN()), "float64", model.Value{}, KindConversion},
		{"double Inf", 10, f(13, math.Inf(1)), "float64", model.Value{}, KindConversion},
		{"boolean", 11, spValue{Field: 14, Bool: true}, "bool", model.BoolValue(true), ""},
		{"boolean into string", 11, spValue{Field: 14, Bool: true}, "string", model.Value{}, KindConversion},
		{"boolean into int", 11, spValue{Field: 14, Bool: true}, "int16", model.Value{}, KindConversion},
		{"string", 12, spValue{Field: 15, Text: "x"}, "string", model.StringValue("x"), ""},
		{"text", 14, spValue{Field: 15, Text: "x"}, "string", model.StringValue("x"), ""},
		{"uuid", 15, spValue{Field: 15, Text: "x"}, "string", model.StringValue("x"), ""},
		{"string into int", 12, spValue{Field: 15, Text: "1"}, "int16", model.Value{}, KindConversion},
		{"string invalid utf8", 12, spValue{Field: 15, Text: "\xff"}, "string", model.Value{}, KindConversion},
		{"datetime", 13, i(11, 1700000000000), "int64", model.IntValue(1700000000000), ""},
		{"bytes", 17, i(16, 0), "string", model.Value{}, KindConversion},
		{"dataset", 16, spValue{Field: 17}, "string", model.Value{}, KindConversion},
		{"template", 19, spValue{Field: 18}, "string", model.Value{}, KindConversion},
		{"int8 array", 22, spValue{Field: 16}, "int16", model.Value{}, KindConversion},
		{"datatype 0", 0, i(10, 1), "int16", model.Value{}, KindConversion},
		{"datatype 99", 99, i(10, 1), "int16", model.Value{}, KindConversion},
		{"float carrying string", 9, spValue{Field: 15, Text: "1"}, "float32", model.Value{}, KindConversion},
		{"no value", 3, spValue{}, "int32", model.Value{}, KindConversion},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, kind := metricValue(spMetric{Value: c.value}, c.dt, c.typ)
			if kind != c.kind || got != c.want {
				t.Fatalf("got %+v kind %q, want %+v kind %q", got, kind, c.want, c.kind)
			}
		})
	}
}

func TestMetricValueOwnDatatypeWins(t *testing.T) {
	m := spMetric{Datatype: 1, Value: spValue{Field: 10, Uint: 0xFF}}
	if v, kind := metricValue(m, 7, "int16"); kind != "" || v != model.IntValue(-1) {
		t.Fatalf("got %+v %q", v, kind)
	}
}

func TestMetricQuality(t *testing.T) {
	cases := []struct {
		name string
		m    spMetric
		want model.Quality
	}{
		{"absent", spMetric{}, model.Good},
		{"192", spMetric{HasQuality: true, Quality: 192}, model.Good},
		{"255", spMetric{HasQuality: true, Quality: 255}, model.Good},
		{"191", spMetric{HasQuality: true, Quality: 191}, model.Uncertain},
		{"64", spMetric{HasQuality: true, Quality: 64}, model.Uncertain},
		{"63", spMetric{HasQuality: true, Quality: 63}, model.Bad},
		{"0", spMetric{HasQuality: true, Quality: 0}, model.Bad},
		{"negative", spMetric{HasQuality: true, Quality: -1}, model.Bad},
	}
	for _, c := range cases {
		if got := metricQuality(c.m); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestQualityPropertyFromWire(t *testing.T) {
	raw := mqtttest.SparkplugPayload{Metrics: []mqtttest.SparkplugMetric{
		{Name: ptr("n"), Quality: ptr(int64(100))},
		{Name: ptr("s"), QualityText: ptr("0")},
		{Name: ptr("l"), Quality: ptr(int64(1) << 40)},
	}}.Encode()
	p, err := decodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if q := metricQuality(p.Metrics[0]); q != model.Uncertain {
		t.Errorf("int quality = %v", q)
	}
	if q := metricQuality(p.Metrics[1]); q != model.Good || p.Metrics[1].HasQuality {
		t.Errorf("string quality must be ignored, got %v %+v", q, p.Metrics[1])
	}
	if q := metricQuality(p.Metrics[2]); q != model.Good {
		t.Errorf("long quality = %v", q)
	}
}

func TestMetricTime(t *testing.T) {
	received := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	payload := spPayload{HasTimestamp: true, Timestamp: 2000}
	cases := []struct {
		name   string
		m      spMetric
		p      spPayload
		want   time.Time
		wantOK bool
	}{
		{"metric", spMetric{HasTimestamp: true, Timestamp: 1000}, payload, time.UnixMilli(1000).UTC(), true},
		{"payload", spMetric{}, payload, time.UnixMilli(2000).UTC(), true},
		{"reception", spMetric{}, spPayload{}, received, true},
		{"year 10000", spMetric{HasTimestamp: true, Timestamp: 253402300800000}, payload, time.Time{}, false},
		{"huge", spMetric{HasTimestamp: true, Timestamp: math.MaxUint64}, payload, time.Time{}, false},
		{"payload year 10000", spMetric{}, spPayload{HasTimestamp: true, Timestamp: 253402300800000}, time.Time{}, false},
		{"last plausible", spMetric{HasTimestamp: true, Timestamp: 253402300799999}, payload, time.UnixMilli(253402300799999).UTC(), true},
	}
	for _, c := range cases {
		got, ok := metricTime(c.m, c.p, received)
		if ok != c.wantOK || !got.Equal(c.want) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func TestEncodeRebirth(t *testing.T) {
	now := time.UnixMilli(1700000000123)
	p, err := decodePayload(encodeRebirth(now))
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasTimestamp || p.Timestamp != 1700000000123 || len(p.Metrics) != 1 {
		t.Fatalf("payload = %+v", p)
	}
	m := p.Metrics[0]
	if m.Name != "Node Control/Rebirth" || m.Datatype != 11 || m.Value.Field != 14 || !m.Value.Bool {
		t.Fatalf("metric = %+v", m)
	}
}

func TestDatatypeNames(t *testing.T) {
	cases := []struct {
		dt        uint32
		name, typ string
	}{
		{1, "Int8", "int16"}, {2, "Int16", "int16"}, {3, "Int32", "int32"}, {4, "Int64", "int64"},
		{5, "UInt8", "uint16"}, {6, "UInt16", "uint16"}, {7, "UInt32", "uint32"}, {8, "UInt64", "uint64"},
		{9, "Float", "float32"}, {10, "Double", "float64"}, {11, "Boolean", "bool"}, {12, "String", "string"},
		{13, "DateTime", "int64"}, {14, "Text", "string"}, {15, "UUID", "string"},
		{16, "DataSet", "unsupported:DataSet"}, {17, "Bytes", "unsupported:Bytes"},
		{19, "Template", "unsupported:Template"}, {22, "Int8Array", "unsupported:Int8Array"},
		{34, "DateTimeArray", "unsupported:DateTimeArray"},
		{0, "Unknown", "unsupported:Unknown"}, {99, "Unknown", "unsupported:Unknown"},
	}
	for _, c := range cases {
		if got := DatatypeName(c.dt); got != c.name {
			t.Errorf("DatatypeName(%d) = %q", c.dt, got)
		}
		if got := SuggestedType(c.dt); got != c.typ {
			t.Errorf("SuggestedType(%d) = %q", c.dt, got)
		}
	}
}
