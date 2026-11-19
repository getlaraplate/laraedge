package serial

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
)

func routeInput(t *testing.T, devices ...config.Device) config.Input {
	t.Helper()
	return config.Input{ID: "scale", Driver: "serial", Connection: json.RawMessage(`{}`), Devices: devices}
}

func evPoint(signal, address, typ string) config.Point {
	return config.Point{Signal: signal, Address: address, Type: typ, Read: config.Read{Mode: config.ReadEvent}, Priority: model.Critical}
}

func mustRouter(t *testing.T, conn Connection, devices ...config.Device) *router {
	t.Helper()
	r, err := newRouter(routeInput(t, devices...), conn)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func rule(key, pattern string, group int) ContextRule {
	return ContextRule{Key: key, Match: regexp.MustCompile(pattern), Group: group}
}

var t0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func TestRouteFirstMatchWins(t *testing.T) {
	r := mustRouter(t, Connection{Encoding: "utf-8"},
		config.Device{Device: "a", Points: []config.Point{evPoint("w", `^01A(.+)$`, "float64")}},
		config.Device{Device: "b", Points: []config.Point{evPoint("w", `^0(.+)$`, "float64")}},
	)
	got := r.route([]byte("01A+1.250"), t0)
	if got.Sample == nil || got.Sample.Device != "a" || got.Sample.Value.Float != 1.25 || got.Sample.Quality != model.Good || !got.Sample.TS.Equal(t0) {
		t.Fatalf("got %+v", got)
	}
	got = r.route([]byte("02A+3"), t0)
	if got.Sample == nil || got.Sample.Device != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestRouteWholeFrameWithoutGroup(t *testing.T) {
	r := mustRouter(t, Connection{Encoding: "utf-8"}, config.Device{Device: "a", Points: []config.Point{evPoint("w", `^\d+\.\d+$`, "float64")}})
	got := r.route([]byte("12.5"), t0)
	if got.Sample == nil || got.Sample.Value.Float != 12.5 {
		t.Fatalf("got %+v", got)
	}
}

func TestRouteConversion(t *testing.T) {
	r := mustRouter(t, Connection{Encoding: "utf-8"}, config.Device{Device: "a", Points: []config.Point{evPoint("w", `^01A(.+)$`, "float64")}})
	got := r.route([]byte("01Aabc"), t0)
	if got.Kind != "conversion" || got.Sample == nil || got.Sample.Quality != model.Bad {
		t.Fatalf("got %+v", got)
	}
}

func TestRouteContext(t *testing.T) {
	conn := Connection{Encoding: "utf-8", Context: []ContextRule{rule("serial", `^SN(\w+)$`, 1), rule("order_ref", `^PO-(\d{4}-\d+)$`, 0)}}
	r := mustRouter(t, conn, config.Device{Device: "a", Points: []config.Point{evPoint("w", `^01A(.+)$`, "float64")}})

	check := func(frame string, want model.Context) {
		t.Helper()
		got := r.route([]byte(frame), t0)
		if got.Sample == nil {
			t.Fatalf("%s: no sample: %+v", frame, got)
		}
		if want.IsEmpty() {
			if got.Sample.Context != nil {
				t.Fatalf("%s: context %+v, want nil", frame, got.Sample.Context)
			}
			return
		}
		if got.Sample.Context == nil || *got.Sample.Context != want {
			t.Fatalf("%s: context %+v, want %+v", frame, got.Sample.Context, want)
		}
	}
	set := r.route([]byte("SN42"), t0)
	if set.Sample != nil || set.ContextKey != "serial" || set.ContextValue != "42" {
		t.Fatalf("set %+v", set)
	}
	check("01A1.0", model.Context{Serial: "42"})
	r.route([]byte("PO-2026-7"), t0)
	check("01A2.0", model.Context{Serial: "42", OrderRef: "PO-2026-7"})
	r.route([]byte("SN43"), t0)
	check("01A3.0", model.Context{Serial: "43", OrderRef: "PO-2026-7"})

	// An empty capture clears the key.
	r2 := mustRouter(t, Connection{Encoding: "utf-8", Context: []ContextRule{rule("serial", `^SN(\w*)$`, 1)}},
		config.Device{Device: "a", Points: []config.Point{evPoint("w", `^01A(.+)$`, "float64")}})
	r2.route([]byte("SN42"), t0)
	cleared := r2.route([]byte("SN"), t0)
	if cleared.ContextKey != "serial" || cleared.ContextValue != "" {
		t.Fatalf("cleared %+v", cleared)
	}
	if got := r2.route([]byte("01A1.0"), t0); got.Sample.Context != nil {
		t.Fatalf("context %+v, want nil", got.Sample.Context)
	}
}

func TestRouteContextFrozenPerSample(t *testing.T) {
	conn := Connection{Encoding: "utf-8", Context: []ContextRule{rule("serial", `^SN(\w+)$`, 1)}}
	r := mustRouter(t, conn, config.Device{Device: "a", Points: []config.Point{evPoint("w", `^01A(.+)$`, "float64")}})
	r.route([]byte("SN42"), t0)
	first := r.route([]byte("01A1.0"), t0)
	r.route([]byte("SN43"), t0)
	if first.Sample.Context.Serial != "42" {
		t.Fatalf("context %+v, want 42", first.Sample.Context)
	}
}

func TestRouteContextTTL(t *testing.T) {
	conn := Connection{Encoding: "utf-8", ContextTTL: 10 * time.Minute, Context: []ContextRule{rule("serial", `^SN(\w+)$`, 1)}}
	r := mustRouter(t, conn, config.Device{Device: "a", Points: []config.Point{evPoint("w", `^01A(.+)$`, "float64")}})
	r.route([]byte("SN42"), t0)
	if got := r.route([]byte("01A1.0"), t0.Add(9*time.Minute+59*time.Second)); got.Sample.Context == nil || got.Sample.Context.Serial != "42" {
		t.Fatalf("got %+v", got.Sample.Context)
	}
	if got := r.route([]byte("01A1.0"), t0.Add(10*time.Minute+time.Second)); got.Sample.Context != nil {
		t.Fatalf("got %+v", got.Sample.Context)
	}
}

func TestRouteDecode(t *testing.T) {
	dev := config.Device{Device: "a", Points: []config.Point{evPoint("w", `^(.+)$`, "string")}}
	r := mustRouter(t, Connection{Encoding: "utf-8"}, dev)
	if got := r.route([]byte("\xd8"), t0); got.Kind != "decode" || got.Sample != nil {
		t.Fatalf("got %+v", got)
	}
	if got := r.route([]byte("ok"), t0); got.Sample == nil || got.Sample.Value.Str != "ok" {
		t.Fatalf("got %+v", got)
	}
	for _, enc := range []string{"windows-1252", "iso-8859-1"} {
		r = mustRouter(t, Connection{Encoding: enc}, dev)
		if got := r.route([]byte("\xd8"), t0); got.Sample == nil || got.Sample.Value.Str != "Ø" {
			t.Fatalf("%s: got %+v", enc, got)
		}
	}
}

func TestRouteUnmatched(t *testing.T) {
	r := mustRouter(t, Connection{Encoding: "utf-8"}, config.Device{Device: "a", Points: []config.Point{evPoint("w", `^01A(.+)$`, "float64")}})
	got := r.route([]byte("zzz"), t0)
	if got.Kind != "unmatched" || got.Sample != nil || got.ContextKey != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestDuplicateAddresses(t *testing.T) {
	r := mustRouter(t, Connection{Encoding: "utf-8"},
		config.Device{Device: "a", Points: []config.Point{evPoint("w", `^A$`, "string"), evPoint("x", `^B$`, "string")}},
		config.Device{Device: "b", Points: []config.Point{evPoint("w", `^A$`, "string")}},
	)
	got := r.duplicateAddresses()
	if len(got) != 1 || got[0] != "devices.1.points.0" {
		t.Fatalf("got %v", got)
	}
}
