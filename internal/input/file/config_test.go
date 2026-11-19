package file

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
)

// conn builds a connection document with an absolute directory of the running
// OS plus the given extra members, written as raw JSON members.
func conn(t *testing.T, extra string) json.RawMessage {
	t.Helper()
	dir, err := json.Marshal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"directory":` + string(dir) + `,"format":"csv"`
	if extra != "" {
		doc += "," + extra
	}
	return json.RawMessage(doc + "}")
}

func TestParseConnectionDefaults(t *testing.T) {
	dir := t.TempDir()
	d, _ := json.Marshal(dir)
	got, err := ParseConnection(json.RawMessage(`{"directory":` + string(d) + `,"format":"csv"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Connection{
		Directory: dir, Pattern: "*", Format: FormatCSV, AfterRead: AfterReadMove, Encoding: "utf-8",
		Interval: 2 * time.Second, StableFor: 5 * time.Second, SeenRetention: 720 * time.Hour,
		MaxFileSize: 16 << 20, MaxFilesPerScan: 100, Location: time.Local,
		CSV: CSVSettings{Delimiter: ',', Decimal: '.', HeaderRow: 1, Layout: LayoutWide},
	}
	if got != want {
		t.Fatalf("defaults:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseConnectionAccepted(t *testing.T) {
	tests := []struct {
		name  string
		extra string
		check func(t *testing.T, c Connection)
	}{
		{"retention days", `"seen_retention":"30d"`, func(t *testing.T, c Connection) {
			if c.SeenRetention != 720*time.Hour {
				t.Fatal(c.SeenRetention)
			}
		}},
		{"retention days cap", `"seen_retention":"36500d"`, func(t *testing.T, c Connection) {
			if c.SeenRetention != 36500*24*time.Hour {
				t.Fatal(c.SeenRetention)
			}
		}},
		{"retention hours", `"seen_retention":"48h"`, func(t *testing.T, c Connection) {
			if c.SeenRetention != 48*time.Hour {
				t.Fatal(c.SeenRetention)
			}
		}},
		{"tab delimiter", `"csv":{"delimiter":"\t"}`, func(t *testing.T, c Connection) {
			if c.CSV.Delimiter != '\t' {
				t.Fatal(c.CSV.Delimiter)
			}
		}},
		{"semicolon and comma decimal", `"csv":{"delimiter":";","decimal":","}`, func(t *testing.T, c Connection) {
			if c.CSV.Delimiter != ';' || c.CSV.Decimal != ',' {
				t.Fatal(c.CSV)
			}
		}},
		{"timezone", `"timezone":"Europe/Rome"`, func(t *testing.T, c Connection) {
			if c.Location.String() != "Europe/Rome" {
				t.Fatal(c.Location)
			}
		}},
		{"interval and stable", `"interval":"500ms","stable_for":"500ms"`, func(t *testing.T, c Connection) {
			if c.Interval != 500*time.Millisecond || c.StableFor != 500*time.Millisecond {
				t.Fatal(c.Interval, c.StableFor)
			}
		}},
		{"long layout", `"csv":{"layout":"long","key_column":"K","value_column":"V"}`, func(t *testing.T, c Connection) {
			if c.CSV.Layout != LayoutLong || c.CSV.KeyColumn != "K" || c.CSV.ValueColumn != "V" {
				t.Fatal(c.CSV)
			}
		}},
		{"limits", `"max_file_size":"16MiB","max_files_per_scan":10000,"after_read":"keep","encoding":"windows-1252","pattern":"*.csv"`, func(t *testing.T, c Connection) {
			if c.MaxFileSize != 16<<20 || c.MaxFilesPerScan != 10000 || c.AfterRead != AfterReadKeep || c.Encoding != "windows-1252" || c.Pattern != "*.csv" {
				t.Fatal(c)
			}
		}},
		{"context", `"context":{"serial":" Serial No ","order_ref":"Order","operation_ref":"Op","lot":"Lot"}`, func(t *testing.T, c Connection) {
			if c.Context != (ContextMap{Serial: "Serial No", OrderRef: "Order", OperationRef: "Op", Lot: "Lot"}) {
				t.Fatal(c.Context)
			}
		}},
		{"timestamp token layout", `"timestamp":{"column":"Time","format":"DD.MM.YYYY hh:mm:ss"}`, func(t *testing.T, c Connection) {
			if c.Timestamp == nil || c.Timestamp.Column != "Time" || c.Timestamp.Format != "DD.MM.YYYY hh:mm:ss" {
				t.Fatal(c.Timestamp)
			}
			if _, ok := ParseTimestamp(c.Timestamp, "18.11.2026 14:05:09", time.UTC); !ok {
				t.Fatal("layout not usable")
			}
		}},
		{"timestamp unix", `"timestamp":{"column":"T","format":"unix_ms"}`, func(t *testing.T, c Connection) {
			if c.Timestamp == nil || c.Timestamp.Format != TimestampUnixMillis {
				t.Fatal(c.Timestamp)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConnection(conn(t, tc.extra))
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, c)
		})
	}
}

func TestParseConnectionRefused(t *testing.T) {
	tests := []struct {
		name string
		raw  func(t *testing.T) json.RawMessage
		want string
	}{
		{"not an object", func(*testing.T) json.RawMessage { return json.RawMessage(`[]`) }, "connection: must be an object"},
		{"relative directory", func(*testing.T) json.RawMessage {
			return json.RawMessage(`{"directory":"reports/in","format":"csv"}`)
		}, "connection.directory: must be an absolute path"},
		{"missing directory", func(*testing.T) json.RawMessage { return json.RawMessage(`{"format":"csv"}`) }, "connection.directory: required"},
		{"directory not a string", func(*testing.T) json.RawMessage {
			return json.RawMessage(`{"directory":4,"format":"csv"}`)
		}, "connection.directory: must be a string"},
		{"missing format", func(t *testing.T) json.RawMessage {
			d, _ := json.Marshal(t.TempDir())
			return json.RawMessage(`{"directory":` + string(d) + `}`)
		}, "connection.format: required"},
		{"other format", func(t *testing.T) json.RawMessage {
			d, _ := json.Marshal(t.TempDir())
			return json.RawMessage(`{"directory":` + string(d) + `,"format":"xml"}`)
		}, "connection.format: want csv or qdas"},
		{"bad pattern", func(t *testing.T) json.RawMessage { return conn(t, `"pattern":"["`) }, "connection.pattern: not a valid glob"},
		{"pattern bad only once lowercased", func(t *testing.T) json.RawMessage { return conn(t, `"pattern":"[Z-a]"`) }, "connection.pattern: not a valid glob"},
		{"interval too small", func(t *testing.T) json.RawMessage { return conn(t, `"interval":"400ms"`) }, "connection.interval: want a duration of at least 500ms, such as 2s"},
		{"interval not duration", func(t *testing.T) json.RawMessage { return conn(t, `"interval":"soon"`) }, "connection.interval: want a duration of at least 500ms, such as 2s"},
		{"stable below interval", func(t *testing.T) json.RawMessage { return conn(t, `"interval":"3s","stable_for":"2s"`) }, "connection.stable_for: want a duration of at least interval, such as 5s"},
		{"size zero", func(t *testing.T) json.RawMessage { return conn(t, `"max_file_size":0`) }, "connection.max_file_size: want 1 byte to 16MiB, as bytes or a size such as 1MiB"},
		{"size above limit", func(t *testing.T) json.RawMessage { return conn(t, `"max_file_size":"17MiB"`) }, "connection.max_file_size: want 1 byte to 16MiB, as bytes or a size such as 1MiB"},
		{"files zero", func(t *testing.T) json.RawMessage { return conn(t, `"max_files_per_scan":0`) }, "connection.max_files_per_scan: want a whole number from 1 to 10000"},
		{"files above limit", func(t *testing.T) json.RawMessage { return conn(t, `"max_files_per_scan":10001`) }, "connection.max_files_per_scan: want a whole number from 1 to 10000"},
		{"files fraction", func(t *testing.T) json.RawMessage { return conn(t, `"max_files_per_scan":1.5`) }, "connection.max_files_per_scan: want a whole number from 1 to 10000"},
		{"after_read delete", func(t *testing.T) json.RawMessage { return conn(t, `"after_read":"delete"`) }, "connection.after_read: want move or keep"},
		{"retention 30m", func(t *testing.T) json.RawMessage { return conn(t, `"seen_retention":"30m"`) }, "connection.seen_retention: want a duration of at least 1h, or days such as 30d"},
		{"retention abc", func(t *testing.T) json.RawMessage { return conn(t, `"seen_retention":"abc"`) }, "connection.seen_retention: want a duration of at least 1h, or days such as 30d"},
		{"retention 0d", func(t *testing.T) json.RawMessage { return conn(t, `"seen_retention":"0d"`) }, "connection.seen_retention: want a duration of at least 1h, or days such as 30d"},
		{"retention huge days", func(t *testing.T) json.RawMessage { return conn(t, `"seen_retention":"99999999d"`) }, "connection.seen_retention: want a duration of at least 1h, or days such as 30d"},
		{"retention days above cap", func(t *testing.T) json.RawMessage { return conn(t, `"seen_retention":"36501d"`) }, "connection.seen_retention: want a duration of at least 1h, or days such as 30d"},
		{"encoding", func(t *testing.T) json.RawMessage { return conn(t, `"encoding":"utf-16"`) }, "connection.encoding: want utf-8, windows-1252 or iso-8859-1"},
		{"timezone", func(t *testing.T) json.RawMessage { return conn(t, `"timezone":"Mars/Base"`) }, "connection.timezone: not an IANA time zone name"},
		{"delimiter two chars", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"delimiter":";;"}`) }, "connection.csv.delimiter: want one character that is not a quote or a line break"},
		{"delimiter quote", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"delimiter":"\""}`) }, "connection.csv.delimiter: want one character that is not a quote or a line break"},
		{"delimiter newline", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"delimiter":"\n"}`) }, "connection.csv.delimiter: want one character that is not a quote or a line break"},
		{"delimiter empty", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"delimiter":""}`) }, "connection.csv.delimiter: want one character that is not a quote or a line break"},
		{"delimiter digit", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"delimiter":"5"}`) }, "connection.csv.delimiter: must not be a digit"},
		{"delimiter is the default decimal", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"delimiter":"."}`) }, "connection.csv.decimal: must not be . when the delimiter is ."},
		{"decimal bad", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"decimal":";"}`) }, "connection.csv.decimal: want . or ,"},
		{"decimal comma with comma delimiter", func(t *testing.T) json.RawMessage {
			return conn(t, `"csv":{"decimal":","}`)
		}, "connection.csv.decimal: must not be , when the delimiter is ,"},
		{"header_row 0", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"header_row":0}`) }, "connection.csv.header_row: want a whole number from 1"},
		{"layout tall", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"layout":"tall"}`) }, "connection.csv.layout: want wide or long"},
		{"long without key", func(t *testing.T) json.RawMessage {
			return conn(t, `"csv":{"layout":"long","value_column":"V"}`)
		}, "connection.csv.key_column: required with layout long"},
		{"long without value", func(t *testing.T) json.RawMessage {
			return conn(t, `"csv":{"layout":"long","key_column":"K"}`)
		}, "connection.csv.value_column: required with layout long"},
		{"key with wide", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"key_column":"K"}`) }, "connection.csv.key_column: only with layout long"},
		{"value with wide", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"value_column":"V"}`) }, "connection.csv.value_column: only with layout long"},
		{"context unknown key", func(t *testing.T) json.RawMessage { return conn(t, `"context":{"part":"P"}`) }, "connection.context.part: unknown field"},
		{"context empty column", func(t *testing.T) json.RawMessage { return conn(t, `"context":{"serial":" "}`) }, "connection.context.serial: required"},
		{"context not string", func(t *testing.T) json.RawMessage { return conn(t, `"context":{"lot":3}`) }, "connection.context.lot: must be a string"},
		{"timestamp no column", func(t *testing.T) json.RawMessage { return conn(t, `"timestamp":{"format":"unix_s"}`) }, "connection.timestamp.column: required"},
		{"timestamp no format", func(t *testing.T) json.RawMessage { return conn(t, `"timestamp":{"column":"T"}`) }, "connection.timestamp.format: required"},
		{"timestamp unknown format", func(t *testing.T) json.RawMessage {
			return conn(t, `"timestamp":{"column":"T","format":"excel"}`)
		}, "connection.timestamp.format: want rfc3339, unix_s, unix_ms or a layout of YYYY, MM, DD, hh, mm, ss"},
		{"timestamp bad layout", func(t *testing.T) json.RawMessage {
			return conn(t, `"timestamp":{"column":"T","format":"DD.MM.YY"}`)
		}, "connection.timestamp.format: want rfc3339, unix_s, unix_ms or a layout of YYYY, MM, DD, hh, mm, ss"},
		{"unknown connection field", func(t *testing.T) json.RawMessage { return conn(t, `"bogus":1`) }, "connection.bogus: unknown field"},
		{"unknown csv field", func(t *testing.T) json.RawMessage { return conn(t, `"csv":{"bogus":1}`) }, "connection.csv.bogus: unknown field"},
		{"unknown timestamp field", func(t *testing.T) json.RawMessage {
			return conn(t, `"timestamp":{"column":"T","format":"unix_s","bogus":1}`)
		}, "connection.timestamp.bogus: unknown field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConnection(tc.raw(t))
			if err == nil {
				t.Fatal("want error")
			}
			if err.Error() != tc.want {
				t.Fatalf("got %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// qdasConn builds a qdas connection document with the given extra members.
func qdasConn(t *testing.T, extra string) json.RawMessage {
	t.Helper()
	dir, err := json.Marshal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"directory":` + string(dir) + `,"format":"qdas"`
	if extra != "" {
		doc += "," + extra
	}
	return json.RawMessage(doc + "}")
}

func TestParseConnectionQDASDefaults(t *testing.T) {
	raw := qdasConn(t, "")
	var doc struct{ Directory string }
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got, err := ParseConnection(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := Connection{
		Directory: doc.Directory, Pattern: "*", Format: FormatQDAS, AfterRead: AfterReadMove, Encoding: "utf-8",
		Interval: 2 * time.Second, StableFor: 5 * time.Second, SeenRetention: 720 * time.Hour,
		MaxFileSize: 16 << 20, MaxFilesPerScan: 100, Location: time.Local,
		CSV: CSVSettings{Delimiter: ',', Decimal: '.', HeaderRow: 1, Layout: LayoutWide},
	}
	if got != want {
		t.Fatalf("defaults:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseConnectionQDASContext(t *testing.T) {
	c, err := ParseConnection(qdasConn(t, `"context":{"serial":"K0014","order_ref":"K0053","lot":"K0006"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Context.Serial != "K0014" || c.Context.OrderRef != "K0053" || c.Context.Lot != "K0006" || c.Context.OperationRef != "" {
		t.Fatalf("context = %+v", c.Context)
	}

	c, err = ParseConnection(conn(t, `"context":{"serial":"K0014"}`))
	if err != nil || c.Context.Serial != "K0014" {
		t.Fatalf("csv keeps K0014 as a column name: %+v, %v", c.Context, err)
	}
}

func TestParseConnectionQDASRefused(t *testing.T) {
	const keyShape = ": want a K-field key from K0005 to K0099, such as K0014"
	tests := []struct {
		name, extra, want string
	}{
		{"csv block", `"csv":{}`, "connection.csv: not allowed with format qdas"},
		{"csv block with content", `"csv":{"delimiter":";"}`, "connection.csv: not allowed with format qdas"},
		{"timestamp", `"timestamp":{"column":"T","format":"rfc3339"}`, "connection.timestamp: not allowed with format qdas, the time is K0004"},
		{"column name", `"context":{"serial":"Serial No"}`, "connection.context.serial" + keyShape},
		{"lower case", `"context":{"serial":"k0014"}`, "connection.context.serial" + keyShape},
		{"short", `"context":{"order_ref":"K14"}`, "connection.context.order_ref" + keyShape},
		{"K0001", `"context":{"lot":"K0001"}`, "connection.context.lot" + keyShape},
		{"K0002", `"context":{"serial":"K0002"}`, "connection.context.serial" + keyShape},
		{"K0004", `"context":{"serial":"K0004"}`, "connection.context.serial" + keyShape},
		{"K0100", `"context":{"operation_ref":"K0100"}`, "connection.context.operation_ref" + keyShape},
		{"K1001", `"context":{"serial":"K1001"}`, "connection.context.serial" + keyShape},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConnection(qdasConn(t, tc.extra))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestIsKField(t *testing.T) {
	for s, want := range map[string]bool{
		"K0014": true, "K0000": true, "K9999": true,
		"": false, "K": false, "K14": false, "k0014": false, "K00140": false, "K00a4": false, "K0014 ": false, "X0014": false, "K+014": false,
	} {
		if got := IsKField(s); got != want {
			t.Errorf("IsKField(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestParseConnectionNeverEchoesValues checks the fixed messages: a secret-like
// value placed in a refused field does not reach the error text.
func TestParseConnectionNeverEchoesValues(t *testing.T) {
	const marker = "s3cr3t-marker"
	docs := []string{
		`{"directory":"` + marker + `","format":"csv"}`,
		`{"directory":"/x","format":"` + marker + `"}`,
		`{"directory":"/x","format":"csv","encoding":"` + marker + `"}`,
		`{"directory":"/x","format":"csv","timezone":"` + marker + `"}`,
		`{"directory":"/x","format":"csv","after_read":"` + marker + `"}`,
		`{"directory":"/x","format":"csv","csv":{"layout":"` + marker + `"}}`,
		`{"directory":"/x","format":"csv","` + marker + `":1}`,
	}
	for i, doc := range docs {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			_, err := ParseConnection(json.RawMessage(doc))
			if err == nil {
				t.Skip("accepted on this OS")
			}
			if contains(err.Error(), marker) {
				t.Fatalf("error echoes the value: %q", err)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRelativeDirectoryRefusedOnEveryOS(t *testing.T) {
	abs, err := filepath.Abs("x")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := json.Marshal(abs)
	if _, err := ParseConnection(json.RawMessage(`{"directory":` + string(d) + `,"format":"csv"}`)); err != nil {
		t.Fatalf("absolute path refused: %v", err)
	}
	for _, rel := range []string{".", "..", "a/b", "./a"} {
		d, _ := json.Marshal(rel)
		if _, err := ParseConnection(json.RawMessage(`{"directory":` + string(d) + `,"format":"csv"}`)); err == nil {
			t.Fatalf("%q accepted", rel)
		}
	}
}

func TestCheckPoint(t *testing.T) {
	ok := config.Point{Signal: "s", Address: "Col", Type: "float64", Read: config.Read{Mode: config.ReadEvent}}
	with := func(f func(p *config.Point)) config.Point {
		p := ok
		f(&p)
		return p
	}
	tests := []struct {
		name string
		p    config.Point
		want string
	}{
		{"event", ok, ""},
		{"poll", with(func(p *config.Point) { p.Read.Mode = config.ReadPoll }), "read: want event"},
		{"subscribe", with(func(p *config.Point) { p.Read.Mode = config.ReadSubscribe }), "read: want event"},
		{"long form", with(func(p *config.Point) {
			p.Read = config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{}}
		}), "read: want event"},
		{"no type", with(func(p *config.Point) { p.Type = "" }), "type: required"},
		{"unknown type", with(func(p *config.Point) { p.Type = "decimal" }), "type: want bool, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string"},
		{"byte order", with(func(p *config.Point) { p.ByteOrder = "ABCD" }), "byte_order: not for file"},
		{"empty address", with(func(p *config.Point) { p.Address = "" }), "address: required"},
		{"blank address", with(func(p *config.Point) { p.Address = "  \t" }), "address: required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPoint(tc.p)
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInvertedRange(t *testing.T) {
	for pattern, want := range map[string]bool{
		"*":              false,
		"r[a-c].csv":     false,
		"[^a-c]*":        false,
		"[a-cx-z]*":      false,
		"[c-a]*":         true,
		"[Z-a]*":         true,
		"x[\\a-c]*":      false,
		"[a-c\\-]*":      false,
		"a-z[a-c]-[c-a]": true,
	} {
		if got := invertedRange(strings.ToLower(pattern)); got != want {
			t.Errorf("invertedRange(%q) = %v, want %v", pattern, got, want)
		}
	}
}
