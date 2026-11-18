package file

import (
	"errors"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Q-DAS separators: between the characteristics of a value line, and between
// the fields of one characteristic.
const (
	qs = "\x0f"
	qf = "\x14"
)

var qdasModTime = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func qdasConnection() Connection {
	return Connection{Format: FormatQDAS, Encoding: DefaultEncoding, Location: time.UTC}
}

func qdasPlan(conn Connection, points ...config.Point) *plan {
	return newPlan(config.Input{Devices: []config.Device{{Device: "cmm", Points: points}}}, conn)
}

// qdasLines joins lines with LF and ends the text with one.
func qdasLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestReadQDAS(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		check func(t *testing.T, f *qdasFile)
	}{
		{
			name: "description lines set the characteristics",
			text: qdasLines("K0100 2", "K2001/1 1.1", "K2002/1 Diameter", "K2101/1 12.5", "K2110/1 12.4",
				"K2111/1 12.6", "K2142/1 mm", "K2004/2 1", "K2003/1 unused"),
			check: func(t *testing.T, f *qdasFile) {
				want := []qdasCharacteristic{
					{Number: "1.1", Description: "Diameter", Nominal: "12.5", Lower: "12.4", Upper: "12.6", Unit: "mm"},
					{Attributive: true},
				}
				got := []qdasCharacteristic{f.Characteristic(1), f.Characteristic(2)}
				if f.Count != 2 || !reflect.DeepEqual(got, want) || len(f.Records) != 0 {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "K0100 sets the count",
			text: qdasLines("K0100 3", "K2001/2 B"),
			check: func(t *testing.T, f *qdasFile) {
				if f.Count != 3 || len(f.Characteristics) != 1 || f.Characteristic(2).Number != "B" || f.Characteristic(3) != (qdasCharacteristic{}) {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "without K0100 the highest K2 index is the count",
			text: qdasLines("K2001/1 A", "K2002/4 D"),
			check: func(t *testing.T, f *qdasFile) {
				if f.Count != 4 || len(f.Characteristics) != 2 || f.Characteristic(4).Description != "D" {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "value line gives the positional fields",
			text: qdasLines("K0100 2",
				"12.51"+qf+"0"+qf+"18.11.2026/14:05:09"+qf+"E1"+qf+"B7"+qf+"2"+qf+"Ins"+qf+"M1"+qf+"P9"+qf+"G3"+qf+"extra"+qs+"3"),
			check: func(t *testing.T, f *qdasFile) {
				if len(f.Records) != 1 || f.Records[0].Malformed {
					t.Fatalf("records = %+v", f.Records)
				}
				want := map[string]string{
					"K0001": "12.51", "K0002": "0", "K0004": "18.11.2026/14:05:09", "K0005": "E1", "K0006": "B7",
					"K0007": "2", "K0008": "Ins", "K0010": "M1", "K0011": "P9", "K0012": "G3",
				}
				for key, value := range want {
					if got := f.Field(0, 1, key); got != value {
						t.Fatalf("field 1 %s = %q", key, got)
					}
				}
				if f.Field(0, 1, "K0013") != "" || f.Field(0, 2, "K0001") != "3" || f.Field(0, 2, "K0004") != "" || f.Field(0, 3, "K0001") != "" {
					t.Fatalf("records = %+v", f.Records)
				}
			},
		},
		{
			name: "coded fields after a value line",
			text: qdasLines("K0100 2", "1"+qs+"2", "K0014/0 SN1", "K0053/2 A7"),
			check: func(t *testing.T, f *qdasFile) {
				if len(f.Records) != 1 || f.Field(0, 0, "K0014") != "SN1" || f.Field(0, 2, "K0053") != "A7" || f.Field(0, 1, "K0053") != "" {
					t.Fatalf("records = %+v", f.Records)
				}
			},
		},
		{
			name: "coded values group into records",
			text: qdasLines("K0100 2", "K0001/1 12.5", "K0004/1 18.11.2026/14:05:09", "K0001/2 3", "K0001/1 12.6"),
			check: func(t *testing.T, f *qdasFile) {
				if len(f.Records) != 2 {
					t.Fatalf("records = %+v", f.Records)
				}
				if f.Field(0, 1, "K0001") != "12.5" || f.Field(0, 1, "K0004") == "" || f.Field(0, 2, "K0001") != "3" {
					t.Fatalf("first = %+v", f.Records[0])
				}
				if f.Field(1, 1, "K0001") != "12.6" || f.Field(1, 2, "K0001") != "" || f.Records[0].Malformed || f.Records[1].Malformed {
					t.Fatalf("second = %+v", f.Records[1])
				}
			},
		},
		{
			name: "a coded value after a value line starts a record",
			text: qdasLines("K0100 2", "1", "K0001/2 5"),
			check: func(t *testing.T, f *qdasFile) {
				if len(f.Records) != 2 || !f.Records[0].Malformed || f.Records[1].Malformed || f.Field(1, 2, "K0001") != "5" {
					t.Fatalf("records = %+v", f.Records)
				}
			},
		},
		{
			name: "a key without index is index 1",
			text: qdasLines("K2001 A", "K0001 7"),
			check: func(t *testing.T, f *qdasFile) {
				if f.Count != 1 || f.Characteristic(1).Number != "A" || f.Field(0, 1, "K0001") != "7" {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "fields before any record are ignored",
			text: qdasLines("K0100 1", "K0014/0 SN0", "K0053/1 A0", "5"),
			check: func(t *testing.T, f *qdasFile) {
				if len(f.Records) != 1 || f.Field(0, 0, "K0014") != "" || f.Field(0, 1, "K0053") != "" {
					t.Fatalf("records = %+v", f.Records)
				}
			},
		},
		{
			name: "a value line with too few blocks is malformed",
			text: qdasLines("K0100 3", "1"+qs+"2"),
			check: func(t *testing.T, f *qdasFile) {
				if len(f.Records) != 1 || !f.Records[0].Malformed || f.Field(0, 2, "K0001") != "2" || f.Field(0, 3, "K0001") != "" {
					t.Fatalf("records = %+v", f.Records)
				}
			},
		},
		{
			name: "a value line with too many blocks is malformed",
			text: qdasLines("K0100 1", "1"+qs+"2"),
			check: func(t *testing.T, f *qdasFile) {
				if len(f.Records) != 1 || !f.Records[0].Malformed || f.Field(0, 1, "K0001") != "1" || f.Field(0, 2, "K0001") != "" {
					t.Fatalf("records = %+v", f.Records)
				}
			},
		},
		{
			name: "blank lines and CRLF are skipped",
			text: "K0100 1\r\n\r\nK2001/1 A \r\n   \n5\r\n\r\n",
			check: func(t *testing.T, f *qdasFile) {
				if f.Count != 1 || f.Characteristic(1).Number != "A" || len(f.Records) != 1 || f.Field(0, 1, "K0001") != "5" {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "a key with no value",
			text: qdasLines("K2001/1", "K2002/1 "),
			check: func(t *testing.T, f *qdasFile) {
				if f.Count != 1 || f.Characteristic(1) != (qdasCharacteristic{}) {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "K2142/0 before an own value",
			text: qdasLines("K0100 3", "K2142/0 mm", "K2142/2 in"),
			check: func(t *testing.T, f *qdasFile) {
				if f.Characteristic(1).Unit != "mm" || f.Characteristic(2).Unit != "in" || f.Characteristic(3).Unit != "mm" {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "K2142/0 after an own value",
			text: qdasLines("K0100 2", "K2142/2 in", "K2142/0 mm"),
			check: func(t *testing.T, f *qdasFile) {
				if f.Characteristic(1).Unit != "mm" || f.Characteristic(2).Unit != "in" {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "K2004/0 makes every characteristic attributive unless its own says 0",
			text: qdasLines("K0100 3", "K2004/2 0", "K2004/0 1", "K2001/3 C"),
			check: func(t *testing.T, f *qdasFile) {
				if !f.Characteristic(1).Attributive || f.Characteristic(2).Attributive || !f.Characteristic(3).Attributive || f.Characteristic(3).Number != "C" {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "K2xxx/0 does not raise the count",
			text: qdasLines("K2001/2 B", "K2002/0 D"),
			check: func(t *testing.T, f *qdasFile) {
				if f.Count != 2 || f.Characteristic(1).Description != "D" || f.Characteristic(2).Description != "D" {
					t.Fatalf("file = %+v", f)
				}
			},
		},
		{
			name: "index 0 on a part key is ignored",
			text: qdasLines("K1001/0 P", "K1001 P", "K2001 A"),
			check: func(t *testing.T, f *qdasFile) {
				if f.Count != 1 {
					t.Fatalf("file = %+v", f)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := readQDAS(tc.text)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			tc.check(t, f)
		})
	}
}

func TestReadQDASRefused(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"short key", qdasLines("K12 x")},
		{"letter index", qdasLines("K2001/1 A", "K0001/a 1")},
		{"empty index", qdasLines("K2001/ A")},
		{"no space after the key", qdasLines("K2001/1A")},
		{"tab after the key", qdasLines("K2001\tA")},
		{"overflowing index", qdasLines("K2001/99999999999999999999 A")},
		{"index above the limit", qdasLines("K2001/1000001 A")},
		{"second part type", qdasLines("K1001/2 P", "K2001 A")},
		{"K2 index above K0100", qdasLines("K0100 3", "K2001/4 x")},
		{"K0 index above K0100", qdasLines("K0100 3", "K0001/1 1", "K0006/4 x")},
		{"K0 index above the K2 count", qdasLines("K2001/1 A", "K0001/2 1")},
		{"K0100 after higher K2", qdasLines("K2001/4 x", "K0100 3")},
		{"unreadable K0100", qdasLines("K0100 three")},
		{"negative K0100", qdasLines("K0100 -1")},
		{"value line before any characteristic", qdasLines("5", "K2001/1 A")},
		{"coded value before any characteristic", qdasLines("K0001/1 5", "K2001/1 A")},
		{"field index above K0100 before any record", qdasLines("K0100 1", "K0014/2 x", "5")},
		{"coded value on index 0", qdasLines("K2001/1 A", "K0001/0 5")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := readQDAS(tc.text)
			if !errors.Is(err, errMalformedFile) || f != nil {
				t.Fatalf("got %+v %v", f, err)
			}
		})
	}
}

func TestQDASAddress(t *testing.T) {
	if got := (qdasCharacteristic{Number: " 1.1 "}).Address(3); got != "1.1" {
		t.Fatalf("address = %q", got)
	}
	if got := (qdasCharacteristic{}).Address(3); got != "#3" {
		t.Fatalf("address = %q", got)
	}
	if got := (qdasCharacteristic{Number: "  "}).Address(2); got != "#2" {
		t.Fatalf("address = %q", got)
	}
}

func TestQDASSamplesByAddress(t *testing.T) {
	p := qdasPlan(qdasConnection(),
		pt("bore", "1.1", "float64", model.Critical),
		pt("count", "#2", "int32", model.Bulk),
		pt("bore_text", " 1.1 ", "string", model.Bulk),
	)
	data := qdasLines("K0100 2", "K2001/1 1.1",
		"12.5"+qf+"0"+qf+"18.11.2026/14:05:09"+qs+"3"+qf+"0"+qf+"18.11.2026/14:05:10")
	samples, kinds, err := parseQDAS(p, []byte(data), qdasModTime)
	if err != nil || len(kinds) != 0 {
		t.Fatalf("err=%v kinds=%v", err, kinds)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %+v", samples)
	}
	bore, text, count := samples[0], samples[1], samples[2]
	if bore.Device != "cmm" || bore.Signal != "bore" || bore.Priority != model.Critical || bore.Quality != model.Good || floatOf(t, bore) != 12.5 {
		t.Fatalf("bore = %+v", bore)
	}
	if !bore.TS.Equal(time.Date(2026, 11, 18, 14, 5, 9, 0, time.UTC)) || bore.Context != nil {
		t.Fatalf("bore ts/context = %v %+v", bore.TS, bore.Context)
	}
	if text.Signal != "bore_text" || text.Value.Str != "12.5" {
		t.Fatalf("text = %+v", text)
	}
	if count.Signal != "count" || count.Priority != model.Bulk || count.Value.Int != 3 ||
		!count.TS.Equal(time.Date(2026, 11, 18, 14, 5, 10, 0, time.UTC)) {
		t.Fatalf("count = %+v", count)
	}
}

func TestQDASOutcomes(t *testing.T) {
	const ts1 = "18.11.2026/14:05:09"
	at := func(sec int) time.Time { return time.Date(2026, 11, 18, 14, 5, sec, 0, time.UTC) }
	tests := []struct {
		name      string
		data      string
		points    []config.Point
		wantKinds []string
		check     func(t *testing.T, s []model.Sample)
	}{
		{
			name: "attributes 255 and 256 give no sample",
			data: qdasLines("K0100 3", "1"+qf+"255"+qf+ts1+qs+"2"+qf+"256"+qf+ts1+qs+"3"+qf+"0"+qf+ts1),
			points: []config.Point{
				pt("a", "#1", "float64", model.Bulk), pt("b", "#2", "float64", model.Bulk), pt("c", "#3", "float64", model.Bulk),
			},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || s[0].Signal != "c" || floatOf(t, s[0]) != 3 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name:   "an empty value gives no sample",
			data:   qdasLines("K0100 2", " "+qf+"0"+qf+ts1+qs+"2"+qf+"0"+qf+ts1),
			points: []config.Point{pt("a", "#1", "float64", model.Bulk), pt("b", "#2", "float64", model.Bulk)},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || s[0].Signal != "b" {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name:      "a decimal comma is a conversion error",
			data:      qdasLines("K0100 1", "12,5"+qf+"0"+qf+ts1),
			points:    []config.Point{pt("a", "#1", "float64", model.Bulk)},
			wantKinds: []string{KindConversion},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || s[0].Quality != model.Bad || s[0].Value.Kind != model.KindFloat || s[0].Value.Float != 0 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "own K0004 beats the shared one",
			data: qdasLines("K0100 2", "1"+qf+"0"+qf+"18.11.2026/14:05:01"+qs+"2"+qf+"0"+qf+"18.11.2026/14:05:02",
				"K0004/0 18.11.2026/14:05:03"),
			points: []config.Point{pt("b", "#2", "float64", model.Bulk)},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || !s[0].TS.Equal(at(2)) {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "shared K0004 beats the lowest index",
			data: qdasLines("K0100 2", "1"+qf+"0"+qf+"18.11.2026/14:05:01"+qs+"2",
				"K0004/0 18.11.2026/14:05:03"),
			points: []config.Point{pt("b", "#2", "float64", model.Bulk)},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || !s[0].TS.Equal(at(3)) {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name:   "the lowest index K0004 is the fallback",
			data:   qdasLines("K0100 3", "1"+qs+"2"+qf+"0"+qf+"18.11.2026/14:05:04"+qs+"3"+qf+"0"+qf+"18.11.2026/14:05:05"),
			points: []config.Point{pt("a", "#1", "float64", model.Bulk)},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || !s[0].TS.Equal(at(4)) {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "an unreadable K0004 makes the record bad",
			data: qdasLines("K0100 2", "1"+qf+"0"+qf+"18.11.2026 14:05"+qs+"2",
				"3"+qf+"0"+qf+ts1+qs+"4"),
			points:    []config.Point{pt("a", "#1", "float64", model.Bulk), pt("b", "#2", "float64", model.Bulk)},
			wantKinds: []string{KindBadTimestamp},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 4 || s[0].Quality != model.Bad || s[1].Quality != model.Bad || !s[0].TS.Equal(qdasModTime) {
					t.Fatalf("samples = %+v", s)
				}
				if s[2].Quality != model.Good || s[3].Quality != model.Good || !s[3].TS.Equal(at(9)) {
					t.Fatalf("second record = %+v", s[2:])
				}
			},
		},
		{
			name:      "a K0004 before 1970 is unreadable",
			data:      qdasLines("K0100 1", "1"+qf+"0"+qf+"18.11.1969/14:05:09"),
			points:    []config.Point{pt("a", "#1", "float64", model.Bulk)},
			wantKinds: []string{KindBadTimestamp},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || s[0].Quality != model.Bad {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name:   "one record without K0004 takes the modification time",
			data:   qdasLines("K0100 1", "1"),
			points: []config.Point{pt("a", "#1", "float64", model.Bulk)},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || s[0].Quality != model.Good || !s[0].TS.Equal(qdasModTime) {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name:      "a malformed record is bad",
			data:      qdasLines("K0100 2", "1"+qf+"0"+qf+ts1, "K0014/0 SN", "3"+qf+"0"+qf+ts1+qs+"4"),
			points:    []config.Point{pt("a", "#1", "float64", model.Bulk), pt("b", "#2", "float64", model.Bulk)},
			wantKinds: []string{KindMalformedRow},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 4 || s[0].Quality != model.Bad || s[1].Quality != model.Bad || s[0].Context != nil {
					t.Fatalf("samples = %+v", s)
				}
				if !s[0].TS.Equal(at(9)) || !s[1].TS.Equal(at(9)) || s[2].Quality != model.Good {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name:      "a point whose address no characteristic has",
			data:      qdasLines("K0100 1", "K2001/1 A", "1"),
			points:    []config.Point{pt("a", "A", "float64", model.Bulk), pt("x", "X", "float64", model.Bulk), pt("y", "#1", "float64", model.Bulk)},
			wantKinds: []string{KindMissingField},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || s[0].Signal != "a" {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name:      "a point on an attributive characteristic",
			data:      qdasLines("K0100 2", "K2004/2 1", "1"+qs+"0", "K0004/0 "+ts1, "2"+qs+"1", "K0004/0 "+ts1),
			points:    []config.Point{pt("a", "#1", "float64", model.Bulk), pt("b", "#2", "float64", model.Bulk)},
			wantKinds: []string{KindMissingField},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 2 || s[0].Signal != "a" || s[1].Signal != "a" {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "a colliding K2001 nobody addresses",
			data: qdasLines("K0100 2", "K2001/1 A", "K2001/2 A", "1"+qs+"2"),
			points: []config.Point{
				pt("b", "B", "float64", model.Bulk),
			},
			wantKinds: []string{KindMissingField},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 0 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name:   "a description-only file",
			data:   qdasLines("K0100 2", "K2001/1 A", "K2001/2 A", "K2004/1 1"),
			points: []config.Point{pt("a", "A", "float64", model.Bulk), pt("x", "X", "float64", model.Bulk)},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 0 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := qdasPlan(qdasConnection(), tc.points...)
			samples, kinds, err := parseQDAS(p, []byte(tc.data), qdasModTime)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(kinds, tc.wantKinds) {
				t.Fatalf("kinds = %v, want %v", kinds, tc.wantKinds)
			}
			tc.check(t, samples)
		})
	}
}

func TestQDASTimeInLocation(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skip("no tz database")
	}
	conn := qdasConnection()
	conn.Location = rome
	p := qdasPlan(conn, pt("a", "#1", "float64", model.Bulk))
	for _, text := range []string{"18.11.2026/14:05", "18.11.2026/14:05:00"} {
		samples, kinds, err := parseQDAS(p, []byte(qdasLines("K0100 1", "1"+qf+"0"+qf+text)), qdasModTime)
		if err != nil || len(kinds) != 0 || len(samples) != 1 {
			t.Fatalf("%s: %+v %v %v", text, samples, kinds, err)
		}
		if want := time.Date(2026, 11, 18, 13, 5, 0, 0, time.UTC); !samples[0].TS.Equal(want) || samples[0].TS.Location() != time.UTC {
			t.Fatalf("%s: ts = %v", text, samples[0].TS)
		}
	}
}

func TestQDASContext(t *testing.T) {
	conn := qdasConnection()
	conn.Context = ContextMap{Serial: "K0014", OrderRef: "K0053", OperationRef: "K0054", Lot: "K0006"}
	p := qdasPlan(conn, pt("a", "#1", "float64", model.Bulk))
	const ts1 = "18.11.2026/14:05:09"
	data := qdasLines("K0100 3",
		"1"+qf+"0"+qf+ts1+qs+"2"+qf+"0"+qf+ts1+qf+""+qf+"L2"+qs+"3"+qf+"0"+qf+ts1+qf+""+qf+"L3",
		"K0014/0 SN1", "K0014/2 SN2", "K0053/3 O3", "K0053/2 O2",
		"4"+qf+"0"+qf+ts1+qs+"5"+qs+"6",
	)
	samples, kinds, err := parseQDAS(p, []byte(data), qdasModTime)
	if err != nil || len(kinds) != 0 || len(samples) != 2 {
		t.Fatalf("%+v %v %v", samples, kinds, err)
	}
	if want := (&model.Context{Serial: "SN1", OrderRef: "O2", Lot: "L2"}); !reflect.DeepEqual(samples[0].Context, want) {
		t.Fatalf("context = %+v", samples[0].Context)
	}
	if samples[1].Context != nil {
		t.Fatalf("second context = %+v, want nil", samples[1].Context)
	}
}

func TestQDASRefusedFiles(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"invalid utf-8", []byte("K0100 1\n\xff\xfe\n")},
		{"two records one without K0004", []byte(qdasLines("K0100 1", "1"+qf+"0"+qf+"18.11.2026/14:05:09", "2"))},
		{"addressed K2001 twice", []byte(qdasLines("K0100 2", "K2001/1 A", "K2001/2 A", "1"+qs+"2"))},
		{"reader refusal", []byte(qdasLines("K12 x"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := qdasPlan(qdasConnection(), pt("a", "A", "float64", model.Bulk))
			samples, kinds, err := parseQDAS(p, tc.data, qdasModTime)
			if !errors.Is(err, errMalformedFile) || samples != nil || kinds != nil {
				t.Fatalf("got %+v %v %v", samples, kinds, err)
			}
		})
	}
}

func TestQDASRefusalNeverCarriesValues(t *testing.T) {
	data := qdasLines("K0100 2", "K2001/1 Secret", "K2001/2 Secret", "4711"+qs+"4712")
	_, _, err := parseQDAS(qdasPlan(qdasConnection(), pt("a", "Secret", "float64", model.Bulk)), []byte(data), qdasModTime)
	if err == nil || strings.Contains(err.Error(), "Secret") || strings.Contains(err.Error(), "4711") {
		t.Fatalf("err = %v", err)
	}
}

// TestQDASGermanCMMReport is a .dfq as a CMM on a German Windows PC writes it:
// windows-1252, CRLF, a description with an umlaut sign, a trailing empty line.
func TestQDASGermanCMMReport(t *testing.T) {
	text := "K0100 2\r\nK1001 Welle\r\nK2001/1 1.1\r\nK2002/1 Ø Bohrung\r\nK2142/1 mm\r\n" +
		"K2001/2 1.2\r\nK2002/2 Länge\r\n" +
		"12.503" + qf + "0" + qf + "03.05.2021/14:12:50" + qs + "40.01" + qf + "0" + qf + "03.05.2021/14:12:50\r\n" +
		"K0014/0 SN-1\r\n\r\n"
	data, err := charmap.Windows1252.NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	conn := qdasConnection()
	conn.Encoding = "windows-1252"
	conn.Context = ContextMap{Serial: "K0014"}
	p := qdasPlan(conn, pt("bore", "1.1", "float64", model.Critical), pt("length", "1.2", "float64", model.Bulk))
	samples, kinds, err := parseQDAS(p, data, qdasModTime)
	if err != nil || len(kinds) != 0 || len(samples) != 2 {
		t.Fatalf("%+v %v %v", samples, kinds, err)
	}
	for _, s := range samples {
		if s.Quality != model.Good || s.Context == nil || s.Context.Serial != "SN-1" {
			t.Fatalf("sample = %+v", s)
		}
	}
	if floatOf(t, samples[0]) != 12.503 || floatOf(t, samples[1]) != 40.01 {
		t.Fatalf("samples = %+v", samples)
	}
	decoded, err := decodeText(data, conn.Encoding)
	if err != nil {
		t.Fatal(err)
	}
	f, err := readQDAS(decoded)
	if err != nil || f.Characteristic(1).Description != "Ø Bohrung" || f.Characteristic(2).Description != "Länge" {
		t.Fatalf("file = %+v %v", f, err)
	}
}

// TestQDASPartsKeepTheirOwnContext is a report of several parts in one file,
// each value line followed by its own K0014/0 and K0053/0.
func TestQDASPartsKeepTheirOwnContext(t *testing.T) {
	conn := qdasConnection()
	conn.Context = ContextMap{Serial: "K0014", OrderRef: "K0053"}
	p := qdasPlan(conn, pt("a", "#1", "float64", model.Bulk))
	data := qdasLines("K0100 1",
		"1"+qf+"0"+qf+"18.11.2026/14:05:01", "K0014/0 SN1", "K0053/0 O1",
		"2"+qf+"0"+qf+"18.11.2026/14:05:02", "K0014/0 SN2", "K0053/0 O2",
		"3"+qf+"0"+qf+"18.11.2026/14:05:03",
	)
	samples, kinds, err := parseQDAS(p, []byte(data), qdasModTime)
	if err != nil || len(kinds) != 0 || len(samples) != 3 {
		t.Fatalf("%+v %v %v", samples, kinds, err)
	}
	want := []*model.Context{{Serial: "SN1", OrderRef: "O1"}, {Serial: "SN2", OrderRef: "O2"}, nil}
	for i, s := range samples {
		if !reflect.DeepEqual(s.Context, want[i]) {
			t.Fatalf("part %d context = %+v", i+1, s.Context)
		}
	}
}

// TestQDASExcludedInFirstPartOnly is a characteristic excluded (255) in the
// first part and valid in the second.
func TestQDASExcludedInFirstPartOnly(t *testing.T) {
	p := qdasPlan(qdasConnection(), pt("a", "#1", "float64", model.Bulk))
	data := qdasLines("K0100 1",
		"1"+qf+"255"+qf+"18.11.2026/14:05:01",
		"2"+qf+"0"+qf+"18.11.2026/14:05:02",
	)
	samples, kinds, err := parseQDAS(p, []byte(data), qdasModTime)
	if err != nil || len(kinds) != 0 || len(samples) != 1 || floatOf(t, samples[0]) != 2 {
		t.Fatalf("%+v %v %v", samples, kinds, err)
	}
}

func FuzzParseQDAS(f *testing.F) {
	f.Add([]byte(qdasLines("K0100 2", "K2001/1 A", "1"+qf+"0"+qf+"18.11.2026/14:05:09"+qs+"2", "K0014/0 S")))
	f.Add([]byte(qdasLines("K0001/1 1", "K0001/1 2")))
	f.Fuzz(func(t *testing.T, data []byte) {
		conn := qdasConnection()
		conn.Context = ContextMap{Serial: "K0014"}
		p := qdasPlan(conn, pt("a", "A", "float64", model.Bulk), pt("b", "#2", "string", model.Bulk))
		samples, kinds, err := parseQDAS(p, data, qdasModTime)
		if err != nil && (samples != nil || kinds != nil || !errors.Is(err, errMalformedFile)) {
			t.Fatalf("refusal with output: %v", err)
		}
	})
}

// TestQDASMemoryGrowsWithTheInput bounds the bytes allocated to read a file by
// a small multiple of its size: a large K0100 or a value line with many
// blocks must not multiply the memory.
func TestQDASMemoryGrowsWithTheInput(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"large K0100 and many coded records", "K0100 100000\n" + strings.Repeat("K0001 1\nK0004 18.11.2026/14:05:09\n", 2000)},
		{"many blocks past the count", "K0100 1\n1" + strings.Repeat(qs, 20000) + "\n"},
		{"large K0100 and one value line", "K0100 100000\n" + strings.Repeat("1"+qs, 99999) + "1\n"},
		{"description lines on high indices", "K0100 100000\n" + strings.Repeat("K2001/99999 A\nK2002/0 D\n", 500) + "K0001/99999 1\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := qdasPlan(qdasConnection(), pt("a", "#1", "float64", model.Bulk), pt("b", "A", "string", model.Bulk))
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, _, err := parseQDAS(p, []byte(tc.data), qdasModTime)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			got, limit := after.TotalAlloc-before.TotalAlloc, uint64(64*len(tc.data))
			t.Logf("allocated %d bytes for %d bytes of input", got, len(tc.data))
			if got > limit {
				t.Fatalf("allocated %d bytes for %d bytes of input, limit %d", got, len(tc.data), limit)
			}
		})
	}
}

func TestQDASContextKeys(t *testing.T) {
	text := qdasLines("K0100 2",
		"1"+qf+qf+qf+"EV"+qf+"B1"+qs+"2",
		"K0053/0 O", "K0014/0 P", "K0006/2 B2", "K0008/1 ", "K0099/2 Z")
	f, err := readQDAS(text)
	if err != nil {
		t.Fatal(err)
	}
	got := f.ContextKeys(0)
	want := []string{"K0014", "K0053", "K0005", "K0006", "K0099"}
	if !slices.Equal(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	if v := f.ContextValue(0, "K0006"); v != "B1" {
		t.Errorf("K0006 = %q, want the lowest characteristic's", v)
	}
}

// TestQDASBrowseAndContextWorkStaysLinear: a report with many characteristics
// and as many coded fields must not rebuild a per-record index for every
// characteristic. The bound is on allocated bytes, proportional to the input.
func TestQDASBrowseAndContextWorkStaysLinear(t *testing.T) {
	const n = 20000
	var b strings.Builder
	b.WriteString("K0100 20000\n" + strings.Repeat("1"+qs, n-1) + "1\n")
	for i := 1; i <= n; i++ {
		b.WriteString("K0008/" + strconv.Itoa(i) + " x\n")
	}
	data := b.String()
	limit := uint64(256 * len(data))

	measure := func(name string, run func()) {
		t.Helper()
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		run()
		runtime.ReadMemStats(&after)
		if got := after.TotalAlloc - before.TotalAlloc; got > limit {
			t.Errorf("%s allocated %d bytes for %d bytes of input, limit %d", name, got, len(data), limit)
		}
	}

	in := &Input{conn: qdasConnection()}
	measure("browse addresses", func() {
		addrs, ctx, ok := in.qdasAddressesOf([]byte(data))
		if !ok || len(addrs) != n || len(ctx) != 1 || ctx[0].Address != "K0008" {
			t.Errorf("ok = %v, addresses = %d, context = %+v", ok, len(addrs), ctx)
		}
	})
	p := qdasPlan(qdasConnection(), pt("a", "#1", "float64", model.Bulk), pt("b", "#20000", "float64", model.Bulk))
	measure("parseQDAS", func() {
		if _, _, err := parseQDAS(p, []byte(data), qdasModTime); err != nil {
			t.Errorf("err = %v", err)
		}
	})
}
