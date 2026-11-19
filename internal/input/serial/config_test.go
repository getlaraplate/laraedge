package serial

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
)

func TestParseConnection(t *testing.T) {
	const base = `"device":"/dev/ttyUSB0","baud":9600`
	refusals := []struct {
		name string
		json string
		want string
	}{
		{"not an object", `[]`, "connection: must be an object"},
		{"device missing", `{"baud":9600}`, "connection.device: required"},
		{"device not a string", `{"device":3,"baud":9600}`, "connection.device: must be a string"},
		{"baud missing", `{"device":"x"}`, "connection.baud: want a positive integer"},
		{"baud zero", `{"device":"x","baud":0}`, "connection.baud: want a positive integer"},
		{"baud string", `{"device":"x","baud":"9600"}`, "connection.baud: want a positive integer"},
		{"data_bits", `{` + base + `,"data_bits":9}`, "connection.data_bits: want 5, 6, 7 or 8"},
		{"parity", `{` + base + `,"parity":"mark"}`, "connection.parity: want none, even or odd"},
		{"stop_bits", `{` + base + `,"stop_bits":3}`, "connection.stop_bits: want 1 or 2"},
		{"framing empty", `{` + base + `,"framing":{}}`, "connection.framing: want exactly one of terminator, length, gap"},
		{"framing two", `{` + base + `,"framing":{"terminator":"\r","length":4}}`, "connection.framing: want exactly one of terminator, length, gap"},
		{"framing only start", `{` + base + `,"framing":{"start":"\u0002"}}`, "connection.framing: want exactly one of terminator, length, gap"},
		{"terminator empty", `{` + base + `,"framing":{"terminator":""}}`, "connection.framing.terminator: want 1 to 4 bytes"},
		{"terminator long", `{` + base + `,"framing":{"terminator":"abcde"}}`, "connection.framing.terminator: want 1 to 4 bytes"},
		{"length zero", `{` + base + `,"framing":{"length":0}}`, "connection.framing.length: want 1 to max_frame"},
		{"length above default max", `{` + base + `,"framing":{"length":1025}}`, "connection.framing.length: want 1 to max_frame"},
		{"length above explicit max, any order", `{"framing":{"length":65},` + base + `,"max_frame":64}`, "connection.framing.length: want 1 to max_frame"},
		{"gap", `{` + base + `,"framing":{"gap":"10ms"}}`, "connection.framing.gap: at least 20ms"},
		{"gap not a duration", `{` + base + `,"framing":{"gap":"soon"}}`, "connection.framing.gap: at least 20ms"},
		{"start two chars", `{` + base + `,"framing":{"terminator":"\r","start":"ab"}}`, "connection.framing.start: want one character"},
		{"start empty", `{` + base + `,"framing":{"terminator":"\r","start":""}}`, "connection.framing.start: want one character"},
		{"framing unknown", `{` + base + `,"framing":{"terminator":"\r","x":1}}`, "connection.framing.x: unknown field"},
		{"max_frame zero", `{` + base + `,"max_frame":0}`, "connection.max_frame: want 1 to 65536"},
		{"max_frame huge", `{` + base + `,"max_frame":65537}`, "connection.max_frame: want 1 to 65536"},
		{"encoding", `{` + base + `,"encoding":"utf-16"}`, "connection.encoding: want utf-8, windows-1252 or iso-8859-1"},
		{"context key", `{` + base + `,"context":[{"key":"colour","match":"a"}]}`, "connection.context.0.key: want serial, order_ref, operation_ref or lot"},
		{"context key duplicate", `{` + base + `,"context":[{"key":"lot","match":"(a)"},{"key":"lot","match":"(b)"}]}`, "connection.context.1.key: duplicate"},
		{"context match", `{` + base + `,"context":[{"key":"lot","match":"(a"}]}`, "connection.context.0.match: invalid expression"},
		{"context group", `{` + base + `,"context":[{"key":"lot","match":"^(a)$","group":2}]}`, "connection.context.0.group: the expression has no such group"},
		{"context unknown", `{` + base + `,"context":[{"key":"lot","match":"a","x":1}]}`, "connection.context.0.x: unknown field"},
		{"context_ttl", `{` + base + `,"context_ttl":"0s"}`, "connection.context_ttl: want a positive duration"},
		{"every", `{` + base + `,"requests":[{"command":"?","every":"50ms"}]}`, "connection.requests.0.every: at least 100ms"},
		{"every missing", `{` + base + `,"requests":[{"command":"?"}]}`, "connection.requests.0.every: at least 100ms"},
		{"command empty", `{` + base + `,"requests":[{"command":"","every":"1s"}]}`, "connection.requests.0.command: want 1 to 64 bytes"},
		{"command long", `{` + base + `,"requests":[{"command":"` + strings.Repeat("a", 65) + `","every":"1s"}]}`, "connection.requests.0.command: want 1 to 64 bytes"},
		{"request unknown", `{` + base + `,"requests":[{"command":"?","every":"1s","x":1}]}`, "connection.requests.0.x: unknown field"},
		{"request_timeout", `{` + base + `,"request_timeout":"0s"}`, "connection.request_timeout: want a positive duration"},
		{"unknown field", `{` + base + `,"colour":"red"}`, "connection.colour: unknown field"},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConnection(json.RawMessage(tt.json))
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}

	t.Run("defaults", func(t *testing.T) {
		got, err := ParseConnection(json.RawMessage(`{"device":"COM4","baud":19200}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Device != "COM4" || got.Mode != (Mode{Baud: 19200, DataBits: 8, StopBits: 1, Parity: "none"}) {
			t.Errorf("port = %q %+v", got.Device, got.Mode)
		}
		if string(got.Framing.Terminator) != "\r" || got.Framing.Length != 0 || got.Framing.Gap != 0 || got.Framing.HasStart {
			t.Errorf("framing = %+v", got.Framing)
		}
		if got.MaxFrame != 1024 || got.Encoding != "utf-8" || got.RequestTimeout != time.Second || got.ContextTTL != 0 {
			t.Errorf("defaults = %+v", got)
		}
		if len(got.Context) != 0 || len(got.Requests) != 0 {
			t.Errorf("context/requests = %+v %+v", got.Context, got.Requests)
		}
	})

	t.Run("full", func(t *testing.T) {
		got, err := ParseConnection(json.RawMessage(`{"device":"x","baud":300,"data_bits":7,"parity":"even","stop_bits":2,
			"framing":{"length":12,"start":"\u0002"},"max_frame":64,"encoding":"windows-1252",
			"context":[{"key":"serial","match":"^SN(\\w+)$"},{"key":"order_ref","match":"^PO-\\d+$","group":0}],
			"context_ttl":"10m","requests":[{"command":"01\r","every":"5s"}],"request_timeout":"2s"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != (Mode{Baud: 300, DataBits: 7, StopBits: 2, Parity: "even"}) {
			t.Errorf("mode = %+v", got.Mode)
		}
		if got.Framing.Length != 12 || len(got.Framing.Terminator) != 0 || !got.Framing.HasStart || got.Framing.Start != 2 {
			t.Errorf("framing = %+v", got.Framing)
		}
		if got.MaxFrame != 64 || got.Encoding != "windows-1252" || got.ContextTTL != 10*time.Minute || got.RequestTimeout != 2*time.Second {
			t.Errorf("conn = %+v", got)
		}
		if len(got.Context) != 2 || got.Context[0].Key != "serial" || got.Context[0].Group != 1 || got.Context[1].Group != 0 || got.Context[1].Match == nil {
			t.Errorf("context = %+v", got.Context)
		}
		if len(got.Requests) != 1 || string(got.Requests[0].Command) != "01\r" || got.Requests[0].Every != 5*time.Second {
			t.Errorf("requests = %+v", got.Requests)
		}
	})

	t.Run("gap and decoded multi-byte terminator", func(t *testing.T) {
		got, err := ParseConnection(json.RawMessage(`{"device":"x","baud":9600,"framing":{"gap":"20ms"}}`))
		if err != nil || got.Framing.Gap != 20*time.Millisecond || len(got.Framing.Terminator) != 0 {
			t.Errorf("gap: %+v, %v", got.Framing, err)
		}
		got, err = ParseConnection(json.RawMessage(`{"device":"x","baud":9600,"framing":{"terminator":"\r\n"}}`))
		if err != nil || string(got.Framing.Terminator) != "\r\n" {
			t.Errorf("terminator: %+v, %v", got.Framing, err)
		}
	})
}

func TestCheckPoint(t *testing.T) {
	tests := []struct {
		name string
		p    config.Point
		want string
	}{
		{"poll", config.Point{Address: "^a$", Type: "float64", Read: config.Read{Mode: config.ReadPoll}}, "read: want event"},
		{"subscribe", config.Point{Address: "^a$", Type: "float64", Read: config.Read{Mode: config.ReadSubscribe}}, "read: want event"},
		{"no type", config.Point{Address: "^a$", Read: config.Read{Mode: config.ReadEvent}}, "type: required"},
		{"bad type", config.Point{Address: "^a$", Type: "decimal", Read: config.Read{Mode: config.ReadEvent}}, "type: want bool, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string"},
		{"bad address", config.Point{Address: "(a", Type: "float64", Read: config.Read{Mode: config.ReadEvent}}, "address: invalid expression"},
		{"empty address", config.Point{Type: "float64", Read: config.Read{Mode: config.ReadEvent}}, "address: required"},
		{"ok", config.Point{Address: `^01A(.+)$`, Type: "float64", Read: config.Read{Mode: config.ReadEvent}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re, err := CheckPoint(tt.p)
			if tt.want == "" {
				if err != nil || re == nil {
					t.Fatalf("CheckPoint = %v, %v", re, err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
