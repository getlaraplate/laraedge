package file

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

var csvModTime = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func csvConn() Connection {
	return Connection{
		Encoding: DefaultEncoding,
		Location: time.UTC,
		CSV:      CSVSettings{Delimiter: ',', Decimal: '.', HeaderRow: 1, Layout: LayoutWide},
	}
}

func pt(signal, addr, typ string, prio model.Priority) config.Point {
	return config.Point{Signal: signal, Address: addr, Type: typ, Priority: prio}
}

func csvPlan(conn Connection, points ...config.Point) *plan {
	return newPlan(config.Input{Devices: []config.Device{{Device: "cmm", Points: points}}}, conn)
}

func isoSpec(col string) *TimestampSpec {
	return &TimestampSpec{Column: col, Format: TimestampRFC3339}
}

func floatOf(t *testing.T, s model.Sample) float64 {
	t.Helper()
	if s.Value.Kind != model.KindFloat {
		t.Fatalf("value kind = %v, want float", s.Value.Kind)
	}
	return s.Value.Float
}

func TestWideOneSamplePerPointPerRow(t *testing.T) {
	conn := csvConn()
	conn.Timestamp = isoSpec("Time")
	conn.Context = ContextMap{Serial: "Serial", Lot: "Lot"}
	p := csvPlan(conn,
		pt("bore", "Bore", "float64", model.Critical),
		pt("count", "Count", "int32", model.Bulk),
		pt("shared", "Bore", "string", model.Bulk),
	)
	data := "Time,Serial,Lot,Bore,Count,Extra\n" +
		"2026-11-18T10:00:00Z,S1,L1,12.5,3,x\n" +
		"2026-11-18T10:01:00Z,S2,,12.6,4,x\n" +
		"2026-11-18T10:02:00Z,,,12.7,5,x\n"
	samples, kinds, err := parseCSV(p, []byte(data), csvModTime)
	if err != nil || len(kinds) != 0 {
		t.Fatalf("err=%v kinds=%v", err, kinds)
	}
	if len(samples) != 9 {
		t.Fatalf("samples = %d, want 9", len(samples))
	}
	first := samples[0]
	if first.Device != "cmm" || first.Signal != "bore" || first.Priority != model.Critical || first.Quality != model.Good {
		t.Fatalf("first = %+v", first)
	}
	if !first.TS.Equal(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)) || floatOf(t, first) != 12.5 {
		t.Fatalf("first ts/value = %v %v", first.TS, first.Value)
	}
	if want := (&model.Context{Serial: "S1", Lot: "L1"}); !reflect.DeepEqual(first.Context, want) {
		t.Fatalf("context = %+v", first.Context)
	}
	if samples[1].Signal != "shared" || samples[1].Value.Str != "12.5" {
		t.Fatalf("shared = %+v", samples[1])
	}
	if samples[2].Signal != "count" || samples[2].Priority != model.Bulk || samples[2].Value.Int != 3 {
		t.Fatalf("count = %+v", samples[2])
	}
	if want := (&model.Context{Serial: "S2"}); !reflect.DeepEqual(samples[3].Context, want) {
		t.Fatalf("row 2 context = %+v", samples[3].Context)
	}
	if samples[6].Context != nil {
		t.Fatalf("row 3 context = %+v, want nil", samples[6].Context)
	}
	if !samples[6].TS.Equal(time.Date(2026, 11, 18, 10, 2, 0, 0, time.UTC)) {
		t.Fatalf("row 3 ts = %v", samples[6].TS)
	}
}

func TestWideOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		spec      *TimestampSpec
		wantKinds []string
		check     func(t *testing.T, s []model.Sample)
	}{
		{
			name: "unaddressed column ignored", data: "A,Bore,Z\n1,2,3\n",
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || floatOf(t, s[0]) != 2 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "missing column once per file", data: "A\n1\n",
			wantKinds: []string{KindMissingField},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 0 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "empty cell gives no sample", data: "A,Bore\n1,\n",
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 0 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "blank cell between delimiters", data: "A,Bore\n1, \n",
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 0 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "text into float is bad", data: "Bore\nabc\n",
			wantKinds: []string{KindConversion},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || s[0].Quality != model.Bad || s[0].Value.Kind != model.KindFloat || s[0].Value.Float != 0 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "bad timestamp makes the row bad", data: "Time,Bore\nnope,1\n2026-11-18T10:00:00Z,2\n", spec: isoSpec("Time"),
			wantKinds: []string{KindBadTimestamp},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 2 || s[0].Quality != model.Bad || s[1].Quality != model.Good {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "short row is bad", data: "Time,Bore\n2026-11-18T10:00:00Z,1\n2026-11-18T10:01:00Z\n2026-11-18T10:02:00Z,3,9\n", spec: isoSpec("Time"),
			wantKinds: []string{KindMalformedRow, KindMalformedRow},
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 3 || s[0].Quality != model.Good || s[1].Quality != model.Bad || s[2].Quality != model.Bad {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "rows before header_row are skipped", data: "junk,line\nBore\n5\n",
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || floatOf(t, s[0]) != 5 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
		{
			name: "header names are trimmed", data: " Bore  \n5\n",
			check: func(t *testing.T, s []model.Sample) {
				if len(s) != 1 || floatOf(t, s[0]) != 5 {
					t.Fatalf("samples = %+v", s)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := csvConn()
			conn.Timestamp = tc.spec
			if tc.name == "rows before header_row are skipped" {
				conn.CSV.HeaderRow = 2
			}
			p := csvPlan(conn, pt("bore", "Bore", "float64", model.Critical))
			samples, kinds, err := parseCSV(p, []byte(tc.data), csvModTime)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(kinds, tc.wantKinds) {
				t.Fatalf("kinds = %v, want %v", kinds, tc.wantKinds)
			}
			tc.check(t, samples)
		})
	}
}

func TestWideBareQuoteIsOneRow(t *testing.T) {
	conn := csvConn()
	conn.Timestamp = isoSpec("Time")
	p := csvPlan(conn, pt("bore", "Bore", "float64", model.Critical))
	data := "Time,Bore\n2026-11-18T10:00:00Z,1\n2026-11-18T10:01:00Z,1\"2\n2026-11-18T10:02:00Z,3\n"
	samples, kinds, err := parseCSV(p, []byte(data), csvModTime)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(kinds, []string{KindMalformedRow}) {
		t.Fatalf("kinds = %v", kinds)
	}
	var good int
	for _, s := range samples {
		if s.Quality == model.Good {
			good++
		}
	}
	if good != 2 {
		t.Fatalf("good samples = %d of %+v", good, samples)
	}
}

func TestWideTimestampRules(t *testing.T) {
	p := csvPlan(csvConn(), pt("bore", "Bore", "float64", model.Critical))
	samples, kinds, err := parseCSV(p, []byte("Bore\n1\n"), csvModTime)
	if err != nil || len(kinds) != 0 || len(samples) != 1 || !samples[0].TS.Equal(csvModTime) {
		t.Fatalf("one row: %+v %v %v", samples, kinds, err)
	}
	samples, kinds, err = parseCSV(p, []byte("Bore\n1\n2\n"), csvModTime)
	if !errors.Is(err, errMalformedFile) || samples != nil || kinds != nil {
		t.Fatalf("two rows: %+v %v %v", samples, kinds, err)
	}
}

func TestLong(t *testing.T) {
	conn := csvConn()
	conn.CSV.Layout = LayoutLong
	conn.CSV.KeyColumn = "Name"
	conn.CSV.ValueColumn = "Value"
	conn.Timestamp = isoSpec("Time")
	conn.Context = ContextMap{Serial: "Serial", OrderRef: "Order"}
	p := csvPlan(conn,
		pt("bore", "Bore", "float64", model.Critical),
		pt("len", "Length", "float64", model.Bulk),
		pt("absent", "Absent", "float64", model.Bulk),
	)
	data := "Name,Value,Time,Serial,Order\n" +
		" Bore ,12.5,,,\n" +
		"Length,30,2026-11-18T10:00:00Z,S9,\n" +
		"Other,1,2026-11-18T11:00:00Z,S10,O1\n"
	samples, kinds, err := parseCSV(p, []byte(data), csvModTime)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(kinds, []string{KindMissingField}) {
		t.Fatalf("kinds = %v", kinds)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %+v", samples)
	}
	want := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	for _, s := range samples {
		if !s.TS.Equal(want) {
			t.Fatalf("ts = %v", s.TS)
		}
		if c := (&model.Context{Serial: "S9", OrderRef: "O1"}); !reflect.DeepEqual(s.Context, c) {
			t.Fatalf("context = %+v", s.Context)
		}
	}
	if samples[0].Signal != "bore" || floatOf(t, samples[0]) != 12.5 || samples[1].Signal != "len" {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestLongWithoutTimestampTakesModTime(t *testing.T) {
	conn := csvConn()
	conn.CSV.Layout = LayoutLong
	conn.CSV.KeyColumn = "Name"
	conn.CSV.ValueColumn = "Value"
	p := csvPlan(conn, pt("bore", "Bore", "float64", model.Critical), pt("len", "Length", "float64", model.Critical))
	samples, kinds, err := parseCSV(p, []byte("Name,Value\nBore,1\nLength,2\n"), csvModTime)
	if err != nil || len(kinds) != 0 || len(samples) != 2 || !samples[0].TS.Equal(csvModTime) || samples[0].Context != nil {
		t.Fatalf("%+v %v %v", samples, kinds, err)
	}
}

func TestLongBadTimestampAndConversion(t *testing.T) {
	conn := csvConn()
	conn.CSV.Layout = LayoutLong
	conn.CSV.KeyColumn = "Name"
	conn.CSV.ValueColumn = "Value"
	conn.Timestamp = isoSpec("Time")
	p := csvPlan(conn, pt("bore", "Bore", "float64", model.Critical))
	samples, kinds, err := parseCSV(p, []byte("Name,Value,Time\nBore,abc,nope\n"), csvModTime)
	if err != nil {
		t.Fatal(err)
	}
	// The row is bad, so its value is not converted and counts no conversion.
	if !reflect.DeepEqual(kinds, []string{KindBadTimestamp}) {
		t.Fatalf("kinds = %v", kinds)
	}
	if len(samples) != 1 || samples[0].Quality != model.Bad {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestRefusedFiles(t *testing.T) {
	long := csvConn()
	long.CSV.Layout = LayoutLong
	long.CSV.KeyColumn = "Name"
	long.CSV.ValueColumn = "Value"
	withCtx := csvConn()
	withCtx.Context = ContextMap{Serial: "Serial"}
	hdr3 := csvConn()
	hdr3.CSV.HeaderRow = 3
	tests := []struct {
		name string
		conn Connection
		data []byte
	}{
		{"invalid utf-8", csvConn(), []byte("Bore\n\xff\xfe\n")},
		{"empty file", csvConn(), nil},
		{"shorter than header_row", hdr3, []byte("a\nb\n")},
		{"context column missing", withCtx, []byte("Bore\n1\n")},
		{"duplicate addressed column", csvConn(), []byte("Bore,Bore\n1,2\n")},
		{"repeated key in long", long, []byte("Name,Value\nBore,1\nBore,2\n")},
		{"key column missing in long", long, []byte("Other,Value\nBore,1\n")},
		{"value column missing in long", long, []byte("Name,Other\nBore,1\n")},
		{"timestamp column missing", tsConn(csvConn()), []byte("Bore\n1\n")},
		{"timestamp column missing in long", tsConn(long), []byte("Name,Value\nBore,1\n")},
		{"wide two rows no timestamp", csvConn(), []byte("Bore\n1\n2\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := csvPlan(tc.conn, pt("bore", "Bore", "float64", model.Critical))
			samples, kinds, err := parseCSV(p, tc.data, csvModTime)
			if !errors.Is(err, errMalformedFile) || samples != nil || kinds != nil {
				t.Fatalf("got %+v %v %v", samples, kinds, err)
			}
		})
	}
}

func TestRefusalNeverCarriesValues(t *testing.T) {
	p := csvPlan(csvConn(), pt("bore", "Bore", "float64", model.Critical))
	_, _, err := parseCSV(p, []byte("Bore,Bore\nSECRET,2\n"), csvModTime)
	if err == nil || strings.Contains(err.Error(), "Bore") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecimalComma(t *testing.T) {
	conn := csvConn()
	conn.CSV.Decimal = ','
	conn.CSV.Delimiter = ';'
	p := csvPlan(conn,
		pt("f", "F", "float64", model.Critical),
		pt("i", "I", "int32", model.Critical),
		pt("s", "S", "string", model.Critical),
		pt("b", "B", "bool", model.Critical),
		pt("g", "G", "float64", model.Critical),
	)
	samples, kinds, err := parseCSV(p, []byte("F;I;S;B;G\n12,5;7;\"3,5\";1;\"1.234,5\"\n"), csvModTime)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(kinds, []string{KindConversion}) {
		t.Fatalf("kinds = %v", kinds)
	}
	if floatOf(t, samples[0]) != 12.5 || samples[1].Value.Int != 7 {
		t.Fatalf("samples = %+v", samples)
	}
	if samples[2].Value.Str != "3,5" {
		t.Fatalf("string = %q", samples[2].Value.Str)
	}
	if samples[3].Value.Kind != model.KindBool || !samples[3].Value.Bool {
		t.Fatalf("bool = %+v", samples[3])
	}
	if samples[4].Quality != model.Bad {
		t.Fatalf("thousands separator = %+v", samples[4])
	}
	// Integers take a decimal comma too: "7,0" reads as "7.0" does.
	i2 := csvPlan(conn, pt("i", "I", "int32", model.Critical))
	s2, _, err := parseCSV(i2, []byte("I\n7,0\n"), csvModTime)
	want, ok := driverkit.ConvertText("int32", "7.0")
	if err != nil || len(s2) != 1 || (s2[0].Quality == model.Good) != ok || (ok && s2[0].Value != want) {
		t.Fatalf("%+v %v", s2, err)
	}
}

func TestExcelExportEveryValueGood(t *testing.T) {
	conn := csvConn()
	conn.Encoding = "windows-1252"
	conn.CSV.Delimiter = ';'
	conn.CSV.Decimal = ','
	conn.Timestamp = &TimestampSpec{Column: "Zeit", Format: "DD.MM.YYYY hh:mm:ss", Layout: mustLayout(t, "DD.MM.YYYY hh:mm:ss")}
	p := csvPlan(conn,
		pt("bore", "Ø Bohrung", "float64", model.Critical),
		pt("count", "Anzahl", "int32", model.Critical),
	)
	// 0xD8 is the capital O with stroke in windows-1252.
	data := "Zeit;\xd8 Bohrung;Anzahl\r\n" +
		"18.11.2026 10:00:00;\"12,5\";3\r\n" +
		"18.11.2026 10:01:00;\"12,6\";4\r\n" +
		";;\r\n" +
		"\r\n"
	samples, kinds, err := parseCSV(p, []byte(data), csvModTime)
	if err != nil || len(kinds) != 0 {
		t.Fatalf("err=%v kinds=%v", err, kinds)
	}
	if len(samples) != 4 {
		t.Fatalf("samples = %+v", samples)
	}
	for _, s := range samples {
		if s.Quality != model.Good {
			t.Fatalf("not good: %+v", s)
		}
	}
	if floatOf(t, samples[0]) != 12.5 || floatOf(t, samples[2]) != 12.6 {
		t.Fatalf("samples = %+v", samples)
	}
}

func mustLayout(t *testing.T, s string) TimeLayout {
	t.Helper()
	l, err := ParseTimeLayout(s)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestUTF8BOMIsSkipped(t *testing.T) {
	conn := csvConn()
	conn.CSV.Delimiter = ';'
	conn.CSV.Decimal = ','
	p := csvPlan(conn, pt("bore", "Ø Bohrung", "float64", model.Critical))
	data := "\xef\xbb\xbfØ Bohrung\r\n\"12,5\"\r\n"
	samples, kinds, err := parseCSV(p, []byte(data), csvModTime)
	if err != nil || len(kinds) != 0 || len(samples) != 1 || samples[0].Quality != model.Good || floatOf(t, samples[0]) != 12.5 {
		t.Fatalf("%+v %v %v", samples, kinds, err)
	}
}

func TestISO88591(t *testing.T) {
	conn := csvConn()
	conn.Encoding = "iso-8859-1"
	p := csvPlan(conn, pt("s", "Größe", "string", model.Critical))
	samples, _, err := parseCSV(p, []byte("Gr\xf6\xdfe\n\xe4\n"), csvModTime)
	if err != nil || len(samples) != 1 || samples[0].Value.Str != "ä" {
		t.Fatalf("%+v %v", samples, err)
	}
}

func TestSamplesAreUTC(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatal(err)
	}
	conn := csvConn()
	conn.Location = rome
	conn.Timestamp = &TimestampSpec{Column: "T", Format: "YYYY-MM-DD hh:mm:ss", Layout: mustLayout(t, "YYYY-MM-DD hh:mm:ss")}
	p := csvPlan(conn, pt("bore", "Bore", "float64", model.Critical))
	samples, _, err := parseCSV(p, []byte("T,Bore\n2026-11-18 11:00:00,1\n"), csvModTime)
	if err != nil || len(samples) != 1 {
		t.Fatalf("%+v %v", samples, err)
	}
	if samples[0].TS.Location() != time.UTC || !samples[0].TS.Equal(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("ts = %v", samples[0].TS)
	}
}

func tsConn(c Connection) Connection {
	c.Timestamp = isoSpec("Time")
	return c
}

func TestHeaderRowCountsLines(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"blank line lf", "Report\n\nBore\n1\n"},
		{"blank line crlf", "Report\r\n\r\nBore\r\n1\r\n"},
		{"unbalanced quote", "say \"hi\nx\nBore\n1\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := csvConn()
			conn.CSV.HeaderRow = 3
			p := csvPlan(conn, pt("bore", "Bore", "float64", model.Critical))
			samples, kinds, err := parseCSV(p, []byte(tc.data), csvModTime)
			if err != nil || len(kinds) != 0 || len(samples) != 1 || samples[0].Quality != model.Good {
				t.Fatalf("%+v %v %v", samples, kinds, err)
			}
		})
	}
}

func TestThousandsDotUnderDecimalComma(t *testing.T) {
	conn := csvConn()
	conn.CSV.Decimal = ','
	p := csvPlan(conn, pt("f", "F", "float64", model.Critical))
	samples, kinds, err := parseCSV(p, []byte("F\n1.234\n"), csvModTime)
	if err != nil || len(samples) != 1 || samples[0].Quality != model.Bad || !reflect.DeepEqual(kinds, []string{KindConversion}) {
		t.Fatalf("%+v %v %v", samples, kinds, err)
	}
}

// TestBlankRowsAreDropped: Excel writes rows of empty fields after the data;
// they are neither malformed rows nor bad timestamps, in both layouts.
func TestBlankRowsAreDropped(t *testing.T) {
	wide := csvConn()
	wide.CSV.Delimiter = ';'
	long := wide
	long.CSV.Layout = "long"
	long.CSV.KeyColumn = "K"
	long.CSV.ValueColumn = "V"

	cases := []struct {
		name string
		conn Connection
		data string
		want int
	}{
		{"wide without timestamp", wide, "Bore;Count\n12.5;3\n;;\n ; \n", 2},
		{"wide with timestamp", withTimestamp(wide), "TS;Bore;Count\n2026-11-18T10:00:00Z;12.5;3\n;;\n", 2},
		{"long without timestamp", long, "K;V\nbore;12.5\n;\n", 1},
		{"long with timestamp", withTimestamp(long), "TS;K;V\n2026-11-18T10:00:00Z;bore;12.5\n;;\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := csvPlan(tc.conn, pt("bore", "Bore", "float64", model.Critical), pt("count", "Count", "int32", model.Critical))
			if tc.conn.CSV.Layout == "long" {
				p = csvPlan(tc.conn, pt("bore", "bore", "float64", model.Critical))
			}
			samples, kinds, err := parseCSV(p, []byte(tc.data), csvModTime)
			if err != nil || len(kinds) != 0 {
				t.Fatalf("err=%v kinds=%v", err, kinds)
			}
			if len(samples) != tc.want {
				t.Fatalf("samples = %d, want %d: %+v", len(samples), tc.want, samples)
			}
		})
	}
}

func withTimestamp(c Connection) Connection {
	c.Timestamp = isoSpec("TS")

	return c
}
