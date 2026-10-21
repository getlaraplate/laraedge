package transform_test

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/transform"
)

// kinds is the kindOf of the sample file tests.
func kinds(device, signal string) (model.ValueKind, bool) {
	if device != "press-07" {
		return 0, false
	}

	switch signal {
	case "force":
		return model.KindFloat, true
	case "count":
		return model.KindInt, true
	case "door":
		return model.KindBool, true
	case "state":
		return model.KindString, true
	default:
		return 0, false
	}
}

func fileSample(signal string, ts time.Time, v model.Value, q model.Quality) model.Sample {
	return model.Sample{Device: "press-07", Signal: signal, TS: ts, Value: v, Quality: q, Priority: model.Critical}
}

func TestWriteSamples(t *testing.T) {
	ts := time.Date(2026, 10, 19, 8, 0, 1, 250_000_000, time.UTC)
	local := time.Date(2026, 10, 19, 10, 0, 1, 0, time.FixedZone("CEST", 2*3600))

	got := new(bytes.Buffer)
	err := transform.WriteSamples(got, []model.Sample{
		fileSample("force", ts, model.Value{Kind: model.KindFloat, Float: 1.5}, model.Good),
		fileSample("count", local, model.IntValue(-7), model.Uncertain),
		fileSample("door", ts, model.BoolValue(true), model.Good),
		fileSample("state", ts, model.StringValue("run \"hot\"\n"), model.Bad),
	})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:01.250Z","value":1.5,"quality":"good"}
{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:01.000Z","value":-7,"quality":"uncertain"}
{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":true,"quality":"good"}
{"device":"press-07","signal":"state","ts":"2026-10-19T08:00:01.250Z","value":"run \"hot\"\n","quality":"bad"}
`
	if got.String() != want {
		t.Errorf("written:\n%s\nwant:\n%s", got.String(), want)
	}
}

func TestWriteSamplesRefusesWhatItCannotWrite(t *testing.T) {
	ts := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		s    model.Sample
	}{
		{"a NaN", fileSample("force", ts, model.Value{Kind: model.KindFloat, Float: math.NaN()}, model.Good)},
		{"no value kind", fileSample("force", ts, model.Value{}, model.Good)},
		{"no quality", fileSample("force", ts, model.IntValue(1), 0)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			if err := transform.WriteSamples(&out, []model.Sample{tt.s}); err == nil {
				t.Fatal("WriteSamples succeeded")
			}

			if out.Len() != 0 {
				t.Errorf("wrote %q before failing", out.String())
			}
		})
	}
}

func TestReadSamples(t *testing.T) {
	ts := time.Date(2026, 10, 19, 8, 0, 1, 250_000_000, time.UTC)

	tests := []struct {
		name string
		in   string
		want []model.Sample
	}{
		{
			name: "every kind, blank lines skipped",
			in: `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:01.250Z","value":1.5,"quality":"good"}

{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:01.250Z","value":-7,"quality":"uncertain"}
{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":false,"quality":"good"}
{"device":"press-07","signal":"state","ts":"2026-10-19T08:00:01.250Z","value":"run","quality":"bad"}
`,
			want: []model.Sample{
				fileSample("force", ts, model.Value{Kind: model.KindFloat, Float: 1.5}, model.Good),
				fileSample("count", ts, model.IntValue(-7), model.Uncertain),
				fileSample("door", ts, model.BoolValue(false), model.Good),
				fileSample("state", ts, model.StringValue("run"), model.Bad),
			},
		},
		{
			name: "an integral number written as a float is an integer, an integer is a float",
			in: `{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:01.250Z","value":3.0,"quality":"good"}
{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:01.250Z","value":1e3,"quality":"good"}
{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:01.250Z","value":9007199254740993,"quality":"good"}
{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:01.250Z","value":2,"quality":"good"}
`,
			want: []model.Sample{
				fileSample("count", ts, model.IntValue(3), model.Good),
				fileSample("count", ts, model.IntValue(1000), model.Good),
				fileSample("count", ts, model.IntValue(9007199254740993), model.Good),
				fileSample("force", ts, model.Value{Kind: model.KindFloat, Float: 2}, model.Good),
			},
		},
		{
			name: "an offset timestamp is read as UTC",
			in:   `{"device":"press-07","signal":"door","ts":"2026-10-19T10:00:01.250+02:00","value":true,"quality":"good"}` + "\n",
			want: []model.Sample{fileSample("door", ts, model.BoolValue(true), model.Good)},
		},
		{
			name: "a last line without a newline",
			in:   `{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":true,"quality":"good"}`,
			want: []model.Sample{fileSample("door", ts, model.BoolValue(true), model.Good)},
		},
		{name: "an empty file", in: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := transform.ReadSamples(strings.NewReader(tt.in), kinds)
			if err != nil {
				t.Fatal(err)
			}

			if len(got) != len(tt.want) {
				t.Fatalf("got %d samples, want %d: %+v", len(got), len(tt.want), got)
			}

			for i := range got {
				if !sameSample(got[i], tt.want[i]) {
					t.Errorf("sample %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func sameSample(a, b model.Sample) bool {
	return a.Device == b.Device && a.Signal == b.Signal && a.TS.Equal(b.TS) &&
		a.Value == b.Value && a.Quality == b.Quality && a.Priority == b.Priority
}

func TestReadSamplesRoundTrip(t *testing.T) {
	ts := time.Date(2026, 10, 19, 8, 0, 1, 250_000_000, time.UTC)
	in := []model.Sample{
		fileSample("force", ts, model.Value{Kind: model.KindFloat, Float: 0.1 + 0.2}, model.Good),
		fileSample("force", ts, model.Value{Kind: model.KindFloat, Float: 3}, model.Good),
		fileSample("count", ts, model.IntValue(1<<62), model.Good),
		fileSample("door", ts, model.BoolValue(true), model.Uncertain),
		fileSample("state", ts, model.StringValue(""), model.Bad),
	}

	var buf bytes.Buffer
	if err := transform.WriteSamples(&buf, in); err != nil {
		t.Fatal(err)
	}

	got, err := transform.ReadSamples(&buf, kinds)
	if err != nil {
		t.Fatal(err)
	}

	for i := range in {
		if !sameSample(got[i], in[i]) {
			t.Errorf("sample %d = %+v, want %+v", i, got[i], in[i])
		}
	}
}

func TestReadSamplesErrorsNameTheLineNotTheValue(t *testing.T) {
	const secret = "s3cr3t-value"

	good := `{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":true,"quality":"good"}` + "\n"

	tests := []struct {
		name string
		line string
		want string
	}{
		{"not JSON", `not json ` + secret, "line 2"},
		{"not an object", `[1,2]`, "line 2"},
		{"unknown field", `{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":true,"quality":"good","x":"` + secret + `"}`, "line 2"},
		{"missing quality", `{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":true}`, "line 2"},
		{"missing value", `{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","quality":"good"}`, "line 2"},
		{"null value", `{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":null,"quality":"good"}`, "line 2"},
		{"unknown device", `{"device":"` + secret + `","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":true,"quality":"good"}`, "line 2"},
		{"unknown signal", `{"device":"press-07","signal":"` + secret + `","ts":"2026-10-19T08:00:01.250Z","value":true,"quality":"good"}`, "line 2"},
		{"bad timestamp", `{"device":"press-07","signal":"door","ts":"yesterday","value":true,"quality":"good"}`, "line 2"},
		{"missing timestamp", `{"device":"press-07","signal":"door","value":true,"quality":"good"}`, "line 2"},
		{"bad quality", `{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":true,"quality":"` + secret + `"}`, "line 2"},
		{"a bool point with a number", `{"device":"press-07","signal":"door","ts":"2026-10-19T08:00:01.250Z","value":1,"quality":"good"}`, "line 2"},
		{"an integer point with a fraction", `{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:01.250Z","value":1.5,"quality":"good"}`, "line 2"},
		{"an integer point beyond int64", `{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:01.250Z","value":9223372036854775808,"quality":"good"}`, "line 2"},
		{"an integer point with a string", `{"device":"press-07","signal":"count","ts":"2026-10-19T08:00:01.250Z","value":"` + secret + `","quality":"good"}`, "line 2"},
		{"a float point beyond float64", `{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:01.250Z","value":1e999,"quality":"good"}`, "line 2"},
		{"a string point with a number", `{"device":"press-07","signal":"state","ts":"2026-10-19T08:00:01.250Z","value":1,"quality":"good"}`, "line 2"},
		{"two values on one line", strings.TrimSpace(good) + " " + strings.TrimSpace(good), "line 2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := transform.ReadSamples(strings.NewReader(good+tt.line+"\n"), kinds)
			if err == nil {
				t.Fatalf("ReadSamples succeeded: %+v", got)
			}

			if got != nil {
				t.Errorf("returned samples %+v with an error", got)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %q", err, tt.want)
			}

			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q carries a value", err)
			}
		})
	}
}

func TestReadSamplesRefusesALineBeyondTheLimit(t *testing.T) {
	long := `{"device":"press-07","signal":"state","ts":"2026-10-19T08:00:01.250Z","value":"` +
		strings.Repeat("x", transform.MaxSampleLine) + `","quality":"good"}` + "\n"

	_, err := transform.ReadSamples(strings.NewReader(long), kinds)
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("err = %v, want a refusal naming line 1", err)
	}
}
