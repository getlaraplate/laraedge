package envelope_test

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/getlaraplate/laraedge/internal/envelope"
	"github.com/getlaraplate/laraedge/internal/model"
)

const schemaPath = "testdata/laraplate-machine-1.schema.json"

var (
	t0     = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	sentAt = time.Date(2026, 10, 19, 8, 0, 5, 0, time.UTC)
)

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()

	if err := compiler.AddResource("laraplate-machine-1.schema.json", doc); err != nil {
		t.Fatalf("add schema: %v", err)
	}

	schema, err := compiler.Compile("laraplate-machine-1.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	return schema
}

func validate(t *testing.T, schema *jsonschema.Schema, body []byte) error {
	t.Helper()

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}

	return schema.Validate(inst)
}

func mustFloat(t *testing.T, f float64) model.Value {
	t.Helper()

	v, err := model.FloatValue(f)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

func sample(device, signal string, at time.Time, v model.Value, q model.Quality, p model.Priority) model.Record {
	return model.SampleRecord(model.Sample{Device: device, Signal: signal, TS: at, Value: v, Quality: q, Priority: p})
}

func birth(device string, signals ...model.SignalInfo) model.Record {
	return model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: device, Signals: signals, TS: t0})
}

func death(device string) model.Record {
	return model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: device, TS: t0})
}

type wireDevice struct {
	Device  string            `json:"device"`
	Type    string            `json:"type"`
	Samples []json.RawMessage `json:"samples"`
	Signals []json.RawMessage `json:"signals"`
}

type wireEnvelope struct {
	Protocol  string          `json:"protocol"`
	MessageID string          `json:"message_id"`
	SourceSeq uint64          `json:"source_seq"`
	SentAt    string          `json:"sent_at"`
	Devices   []wireDevice    `json:"devices"`
	Losses    json.RawMessage `json:"losses"`
}

func decode(t *testing.T, body []byte) wireEnvelope {
	t.Helper()

	var env wireEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	return env
}

func TestBuildValidatesAgainstBackendSchema(t *testing.T) {
	schema := compileSchema(t)

	numberSignal := model.SignalInfo{Signal: "temp", DataType: model.DataTypeNumber, Unit: "C"}
	stringSignal := model.SignalInfo{Signal: "state", DataType: model.DataTypeString}

	tests := []struct {
		name    string
		records []model.Record
		losses  []model.Loss
	}{
		{
			name: "data with all four value kinds",
			records: []model.Record{
				sample("press-07", "temp", t0, mustFloat(t, 21.5), model.Good, model.Bulk),
				sample("press-07", "good_count", t0, model.IntValue(1234), model.Good, model.Critical),
				sample("press-07", "running", t0, model.BoolValue(true), model.Uncertain, model.Critical),
				sample("press-07", "state", t0, model.StringValue("RUN"), model.Bad, model.Critical),
			},
		},
		{
			name:    "birth",
			records: []model.Record{birth("press-07", numberSignal, stringSignal)},
		},
		{
			name:    "death",
			records: []model.Record{death("press-07")},
		},
		{
			name: "mixed devices and events",
			records: []model.Record{
				birth("press-07", numberSignal),
				sample("press-07", "temp", t0, mustFloat(t, 20), model.Good, model.Bulk),
				sample("press-08", "temp", t0, mustFloat(t, 30), model.Good, model.Bulk),
				death("press-09"),
				sample("press-07", "temp", t0.Add(time.Second), mustFloat(t, 21), model.Good, model.Bulk),
			},
		},
		{
			name: "data with losses",
			records: []model.Record{
				sample("press-07", "temp", t0, mustFloat(t, 20), model.Good, model.Bulk),
			},
			losses: []model.Loss{
				{Priority: model.Bulk, FromTS: t0, ToTS: t0.Add(time.Minute), Samples: 42, Reason: model.LossBufferFull},
				{Priority: model.Critical, FromTS: t0, ToTS: t0, Samples: 1, Reason: model.LossRejectedInvalid},
			},
		},
		{
			name: "strings needing JSON escapes",
			records: []model.Record{
				sample("press-07", "note", t0, model.StringValue("a \"quoted\" <tag> & \n newline é"), model.Good, model.Critical),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := envelope.Build("0192f5a0-0000-7000-8000-000000000001", 7, sentAt, tc.records, tc.losses)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			if err := validate(t, schema, body); err != nil {
				t.Fatalf("schema validation failed: %v\n%s", err, body)
			}
		})
	}
}

func TestSchemaGuardActuallyRejects(t *testing.T) {
	schema := compileSchema(t)

	bad := map[string]string{
		"bad date-time":  `{"protocol":"laraplate-machine/1","message_id":"m","source_seq":0,"sent_at":"2026-13-45T99:00:00Z","devices":[{"device":"d","type":"death"}]}`,
		"no devices":     `{"protocol":"laraplate-machine/1","message_id":"m","source_seq":0,"sent_at":"2026-10-19T08:00:00.000Z","devices":[]}`,
		"wrong protocol": `{"protocol":"x/1","message_id":"m","source_seq":0,"sent_at":"2026-10-19T08:00:00.000Z","devices":[{"device":"d","type":"death"}]}`,
	}

	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if err := validate(t, schema, []byte(body)); err == nil {
				t.Fatal("schema accepted an invalid document: the validation in this package would be vacuous")
			}
		})
	}
}

func TestBuildGroupsDevicesInRecordOrder(t *testing.T) {
	sig := model.SignalInfo{Signal: "temp", DataType: model.DataTypeNumber}
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	records := []model.Record{
		sample("A", "x", at(1), model.IntValue(1), model.Good, model.Bulk),
		sample("B", "x", at(2), model.IntValue(2), model.Good, model.Bulk),
		sample("A", "y", at(3), model.IntValue(3), model.Good, model.Bulk),
		birth("A", sig),
		sample("A", "x", at(4), model.IntValue(4), model.Good, model.Bulk),
		sample("B", "x", at(5), model.IntValue(5), model.Good, model.Bulk),
		death("B"),
		sample("B", "x", at(6), model.IntValue(6), model.Good, model.Bulk),
		sample("A", "x", at(7), model.IntValue(7), model.Good, model.Bulk),
	}

	body, err := envelope.Build("id", 1, sentAt, records, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	type entry struct {
		device, kind string
		count        int
	}

	want := []entry{
		{"A", "data", 2},  // samples 1 and 3, opened at A's first appearance
		{"B", "data", 2},  // samples 2 and 5
		{"A", "birth", 1}, // closes A's open data entry
		{"A", "data", 2},  // samples 4 and 7, opened after the birth
		{"B", "death", 0}, // closes B's open data entry
		{"B", "data", 1},  // sample 6
	}

	var got []entry
	for _, d := range decode(t, body).Devices {
		count := len(d.Samples)
		if d.Type == "birth" {
			count = len(d.Signals)
		}
		got = append(got, entry{d.Device, d.Type, count})
	}

	if len(got) != len(want) {
		t.Fatalf("devices = %+v, want %+v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("device entry %d = %+v, want %+v\nbody: %s", i, got[i], want[i], body)
		}
	}
}

func TestBuildSampleOrderWithinEntryIsRecordOrder(t *testing.T) {
	records := []model.Record{
		sample("A", "x", t0, model.IntValue(1), model.Good, model.Bulk),
		sample("B", "x", t0, model.IntValue(9), model.Good, model.Bulk),
		sample("A", "x", t0, model.IntValue(2), model.Good, model.Bulk),
		sample("A", "x", t0, model.IntValue(3), model.Good, model.Bulk),
	}

	body, err := envelope.Build("id", 1, sentAt, records, nil)
	if err != nil {
		t.Fatal(err)
	}

	var values []int
	for _, raw := range decode(t, body).Devices[0].Samples {
		var s struct{ Value int }
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		values = append(values, s.Value)
	}

	if len(values) != 3 || values[0] != 1 || values[1] != 2 || values[2] != 3 {
		t.Fatalf("values = %v, want [1 2 3]", values)
	}
}

func TestBuildEnvelopeFields(t *testing.T) {
	records := []model.Record{sample("A", "x", t0, model.IntValue(1), model.Good, model.Bulk)}

	body, err := envelope.Build("msg-1", 18446744073709551615, sentAt, records, nil)
	if err != nil {
		t.Fatal(err)
	}

	env := decode(t, body)

	if env.Protocol != "laraplate-machine/1" {
		t.Fatalf("protocol = %q", env.Protocol)
	}

	if env.MessageID != "msg-1" {
		t.Fatalf("message_id = %q", env.MessageID)
	}

	if env.SourceSeq != math.MaxUint64 {
		t.Fatalf("source_seq = %d, want max uint64 preserved", env.SourceSeq)
	}

	if env.Losses != nil {
		t.Fatalf("losses present without losses: %s", env.Losses)
	}
}

func TestBuildTimestampsAreUTCWithMillisecondPrecision(t *testing.T) {
	rome := time.FixedZone("CEST", 2*3600)
	local := time.Date(2026, 10, 19, 10, 30, 15, 123_987_654, rome)

	records := []model.Record{
		sample("A", "x", local, model.IntValue(1), model.Good, model.Bulk),
		sample("A", "y", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), model.IntValue(1), model.Good, model.Bulk),
	}
	losses := []model.Loss{{
		Priority: model.Bulk,
		FromTS:   time.Date(2026, 10, 19, 10, 0, 0, 5_000_000, rome),
		ToTS:     time.Date(2026, 10, 19, 8, 1, 0, 999_999_999, time.UTC),
		Samples:  3,
		Reason:   model.LossBufferFull,
	}}

	body, err := envelope.Build("id", 1, time.Date(2026, 10, 19, 11, 0, 0, 1_000_000, rome), records, losses)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		SentAt  string `json:"sent_at"`
		Devices []struct {
			Samples []struct {
				TS string `json:"ts"`
			} `json:"samples"`
		} `json:"devices"`
		Losses []struct {
			FromTS string `json:"from_ts"`
			ToTS   string `json:"to_ts"`
		} `json:"losses"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}

	checks := []struct{ name, got, want string }{
		{"sent_at", doc.SentAt, "2026-10-19T09:00:00.001Z"},
		{"sample ts converted to UTC and truncated to ms", doc.Devices[0].Samples[0].TS, "2026-10-19T08:30:15.123Z"},
		{"sample ts zero millis padded", doc.Devices[0].Samples[1].TS, "2026-01-02T03:04:05.000Z"},
		{"loss from_ts", doc.Losses[0].FromTS, "2026-10-19T08:00:00.005Z"},
		{"loss to_ts", doc.Losses[0].ToTS, "2026-10-19T08:01:00.999Z"},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestBuildSampleDetails(t *testing.T) {
	records := []model.Record{
		sample("A", "i", t0, model.IntValue(9007199254740993), model.Good, model.Bulk),
		sample("A", "f", t0, mustFloat(t, 0.1), model.Uncertain, model.Bulk),
		sample("A", "b", t0, model.BoolValue(false), model.Bad, model.Bulk),
		sample("A", "s", t0, model.StringValue("x"), model.Good, model.Bulk),
	}

	body, err := envelope.Build("id", 1, sentAt, records, nil)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Devices []struct {
			Samples []map[string]json.RawMessage `json:"samples"`
		} `json:"devices"`
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}

	wantValue := []string{"9007199254740993", "0.1", "false", `"x"`}
	wantQuality := []string{`"good"`, `"uncertain"`, `"bad"`, `"good"`}

	for i, s := range doc.Devices[0].Samples {
		if got := string(s["value"]); got != wantValue[i] {
			t.Errorf("sample %d value = %s, want %s", i, got, wantValue[i])
		}

		if got := string(s["quality"]); got != wantQuality[i] {
			t.Errorf("sample %d quality = %s, want %s", i, got, wantQuality[i])
		}

		if _, ok := s["priority"]; ok {
			t.Errorf("sample %d leaks internal priority to the wire", i)
		}
	}
}

func TestBuildBirthUnitOmittedWhenEmpty(t *testing.T) {
	records := []model.Record{birth("A",
		model.SignalInfo{Signal: "temp", DataType: model.DataTypeNumber, Unit: "C"},
		model.SignalInfo{Signal: "state", DataType: model.DataTypeString},
	)}

	body, err := envelope.Build("id", 1, sentAt, records, nil)
	if err != nil {
		t.Fatal(err)
	}

	env := decode(t, body)

	var withUnit, withoutUnit map[string]json.RawMessage
	if err := json.Unmarshal(env.Devices[0].Signals[0], &withUnit); err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(env.Devices[0].Signals[1], &withoutUnit); err != nil {
		t.Fatal(err)
	}

	if string(withUnit["unit"]) != `"C"` {
		t.Errorf("unit = %s", withUnit["unit"])
	}

	if _, ok := withoutUnit["unit"]; ok {
		t.Errorf("empty unit written: %s", withoutUnit["unit"])
	}

	if string(withoutUnit["data_type"]) != `"string"` {
		t.Errorf("data_type = %s", withoutUnit["data_type"])
	}
}

func TestBuildDoesNotEscapeHTML(t *testing.T) {
	records := []model.Record{sample("A", "x", t0, model.StringValue("a<b>&c"), model.Good, model.Bulk)}

	body, err := envelope.Build("id", 1, sentAt, records, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), `"a<b>&c"`) {
		t.Fatalf("value was HTML-escaped: %s", body)
	}

	if bytes.HasSuffix(body, []byte("\n")) {
		t.Fatal("body ends with a newline")
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	records := []model.Record{
		sample("A", "x", t0, model.IntValue(1), model.Good, model.Bulk),
		birth("B", model.SignalInfo{Signal: "s", DataType: model.DataTypeBoolean}),
	}
	losses := []model.Loss{{Priority: model.Bulk, FromTS: t0, ToTS: t0, Samples: 1, Reason: model.LossBufferFull}}

	first, err := envelope.Build("id", 1, sentAt, records, losses)
	if err != nil {
		t.Fatal(err)
	}

	second, err := envelope.Build("id", 1, sentAt, records, losses)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, second) {
		t.Fatalf("same input, different bytes:\n%s\n%s", first, second)
	}
}

func TestBuildRejectsInvalidInput(t *testing.T) {
	good := sample("A", "x", t0, model.IntValue(1), model.Good, model.Bulk)
	goodLoss := model.Loss{Priority: model.Bulk, FromTS: t0, ToTS: t0, Samples: 1, Reason: model.LossBufferFull}

	withSample := func(mutate func(*model.Sample)) model.Record {
		s := *good.Sample
		mutate(&s)

		return model.Record{Sample: &s}
	}
	withLoss := func(mutate func(*model.Loss)) model.Loss {
		l := goodLoss
		mutate(&l)

		return l
	}

	tests := []struct {
		name      string
		messageID string
		records   []model.Record
		losses    []model.Loss
	}{
		{name: "no records", messageID: "id", records: nil},
		{name: "no records but losses", messageID: "id", records: nil, losses: []model.Loss{goodLoss}},
		{name: "empty message id", messageID: "", records: []model.Record{good}},
		{name: "message id too long", messageID: strings.Repeat("a", 129), records: []model.Record{good}},
		{name: "empty record", messageID: "id", records: []model.Record{{}}},
		{name: "record with sample and event", messageID: "id", records: []model.Record{{Sample: good.Sample, Event: &model.DeviceEvent{Kind: model.Death, Device: "A"}}}},
		{name: "empty device", messageID: "id", records: []model.Record{withSample(func(s *model.Sample) { s.Device = "" })}},
		{name: "empty signal", messageID: "id", records: []model.Record{withSample(func(s *model.Sample) { s.Signal = "" })}},
		{name: "zero timestamp", messageID: "id", records: []model.Record{withSample(func(s *model.Sample) { s.TS = time.Time{} })}},
		{name: "zero value", messageID: "id", records: []model.Record{withSample(func(s *model.Sample) { s.Value = model.Value{} })}},
		{name: "hand built NaN", messageID: "id", records: []model.Record{withSample(func(s *model.Sample) { s.Value = model.Value{Kind: model.KindFloat, Float: math.NaN()} })}},
		{name: "hand built Inf", messageID: "id", records: []model.Record{withSample(func(s *model.Sample) { s.Value = model.Value{Kind: model.KindFloat, Float: math.Inf(-1)} })}},
		{name: "invalid quality", messageID: "id", records: []model.Record{withSample(func(s *model.Sample) { s.Quality = 0 })}},
		{name: "invalid priority", messageID: "id", records: []model.Record{withSample(func(s *model.Sample) { s.Priority = 0 })}},
		{name: "birth without signals", messageID: "id", records: []model.Record{birth("A")}},
		{name: "birth with bad data type", messageID: "id", records: []model.Record{birth("A", model.SignalInfo{Signal: "s", DataType: "int"})}},
		{name: "death with signals", messageID: "id", records: []model.Record{model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "A", Signals: []model.SignalInfo{{Signal: "s", DataType: model.DataTypeNumber}}})}},
		{name: "event without device", messageID: "id", records: []model.Record{death("")}},
		{name: "unknown event kind", messageID: "id", records: []model.Record{model.EventRecord(model.DeviceEvent{Kind: 0, Device: "A"})}},
		{name: "loss unknown reason", messageID: "id", records: []model.Record{good}, losses: []model.Loss{withLoss(func(l *model.Loss) { l.Reason = "other" })}},
		{name: "loss zero samples", messageID: "id", records: []model.Record{good}, losses: []model.Loss{withLoss(func(l *model.Loss) { l.Samples = 0 })}},
		{name: "loss invalid priority", messageID: "id", records: []model.Record{good}, losses: []model.Loss{withLoss(func(l *model.Loss) { l.Priority = 0 })}},
		{name: "loss zero from_ts", messageID: "id", records: []model.Record{good}, losses: []model.Loss{withLoss(func(l *model.Loss) { l.FromTS = time.Time{} })}},
		{name: "loss to_ts before from_ts", messageID: "id", records: []model.Record{good}, losses: []model.Loss{withLoss(func(l *model.Loss) { l.ToTS = l.FromTS.Add(-time.Second) })}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := envelope.Build(tc.messageID, 1, sentAt, tc.records, tc.losses)
			if err == nil {
				t.Fatalf("Build accepted invalid input: %s", body)
			}

			if body != nil {
				t.Fatalf("Build returned a body together with an error: %s", body)
			}
		})
	}
}
