//go:build e2e && unix

package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
)

// mqttMarker is the value of the state signal while the agent runs: no log
// may hold it.
const mqttMarker = "MARKER-5521-state"

// mqttDevices lists the devices of the mqtt input in a fixed order: press has
// a JSON payload and an availability topic, meter a plain payload.
var mqttDevices = []string{"press", "meter"}

// mqttSignals are the signals of the two devices.
var mqttSignals = map[string][]string{
	"press": {"state", "count", "temp"},
	"meter": {"power"},
}

// writeMQTTSetup writes the file provider setup of an mqtt input reaching
// the TLS broker with a death tolerance of 3 seconds, into dir, and returns
// the bootstrap path.
func writeMQTTSetup(t *testing.T, dir, url string, broker *mqtttest.Broker) string {
	t.Helper()

	doc := marshal(t, map[string]any{
		"schema":  "laraplate-edge-config/1",
		"version": 1,
		"inputs": []any{map[string]any{
			"id":              "plc",
			"driver":          "mqtt",
			"death_tolerance": "3s",
			"connection": map[string]any{
				"broker": "mqtts://" + broker.Addr(),
				"tls":    map[string]any{"ca_file": broker.CA()},
				"availability": []any{map[string]any{
					"device": "press", "topic": "plant/press/status", "online": "online", "offline": "offline",
				}},
			},
			"devices": []any{
				map[string]any{"device": "press", "points": []any{
					map[string]any{"signal": "state", "address": "plant/press|state", "type": "string", "read": "subscribe"},
					map[string]any{"signal": "count", "address": "plant/press|count", "type": "int32", "read": "subscribe"},
					map[string]any{"signal": "temp", "address": "plant/press|temp", "type": "float64", "unit": "C", "read": "subscribe"},
				}},
				map[string]any{"device": "meter", "points": []any{
					map[string]any{"signal": "power", "address": "plant/meter/power", "type": "float64", "unit": "kW", "read": "subscribe"},
				}},
			},
		}},
		"outputs": []any{map[string]any{
			"id": "laraplate", "type": "laraplate-http", "url": url, "token": "secret://env/" + tokenEnv,
			"batch": map[string]any{"max_samples": 40, "max_wait": "200ms"},
		}},
	})
	// JSON is YAML for the file provider.
	writeFile(t, filepath.Join(dir, "config.json"), string(doc))

	boot := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf("agent_id: e2e-mqtt\nconfig:\n  provider: file\n  file: %s\ndata_dir: %s\nstatus_listen: 127.0.0.1:0\n",
		filepath.Join(dir, "config.json"), filepath.Join(dir, "data")))

	return boot
}

// pressMessage is a JSON payload of the press.
func pressMessage(count int, temp float64) []byte {
	// Marshalling a map of numbers and a string cannot fail.
	data, _ := json.Marshal(map[string]any{"state": mqttMarker, "count": count, "temp": temp})

	return data
}

// mqttReadErrors returns the read errors of the input "plc" from /status.
func mqttReadErrors(t *testing.T, addr string) map[string]uint64 {
	t.Helper()

	return inputStatus(t, addr).ReadErrors
}

// TestMQTT: the built binary subscribes over TLS to the broker of the test.
// The retained messages give the births and the first samples at once, typed
// as the points say; a message published later reaches the backend; a payload
// that is not JSON is counted as a read error. When the broker stops the
// devices die after the death tolerance and /status and the metrics show the
// connection down; when it starts again they are born again and the data
// resumes. An offline availability message is the death of its device alone,
// and the online one its birth. Every envelope is valid against the backend
// schema and no log holds a sample value or the broker address.
func TestMQTT(t *testing.T) {
	b := newBackend(t)
	broker := mqtttest.Start(t, mqtttest.Options{TLS: true})

	// Retained: delivered on every subscription, so the test needs no
	// ordering with the agent's connection.
	broker.Publish("plant/press", pressMessage(42, 21.5), true, 1)
	broker.Publish("plant/meter/power", []byte("3.5"), true, 1)
	broker.Publish("plant/press/status", []byte("online"), true, 1)

	dir := t.TempDir()
	boot := writeMQTTSetup(t, dir, b.url(), broker)

	a := startAgent(t, boot)

	b.waitFor(t, a, "a good sample of every signal", func() bool {
		events := b.events()
		for _, d := range mqttDevices {
			for _, s := range mqttSignals[d] {
				if countSignal(events, d, s) == 0 {
					return false
				}
			}
		}

		return true
	})
	addr := a.statusAddr(t)
	waitConnected(t, b, a, addr, true)

	events := b.events()
	checkMQTTBirths(t, events)
	for _, d := range mqttDevices {
		if n := countEvents(events, d, "death"); n != 0 {
			t.Errorf("%s: %d deaths before the broker stopped, want 0", d, n)
		}
	}

	// A message published now reaches the backend through the live
	// subscription.
	broker.Publish("plant/press", pressMessage(43, 22), false, 1)
	b.waitFor(t, a, "the changed count", func() bool {
		return slices.ContainsFunc(b.events(), func(e busEvent) bool {
			return e.device == "press" && e.signal == "count" && string(e.value) == "43"
		})
	})

	// A payload that is not JSON gives a bad sample and a read error.
	broker.Publish("plant/press", []byte("not json"), false, 1)
	b.waitFor(t, a, "a malformed_json read error", func() bool {
		return mqttReadErrors(t, addr)["malformed_json"] > 0
	})

	// The broker stops: death after the tolerance, connection down.
	broker.Stop()
	b.waitFor(t, a, "both devices dead", func() bool {
		events := b.events()
		for _, d := range mqttDevices {
			if countEvents(events, d, "death") == 0 {
				return false
			}
		}

		return true
	})
	down := waitConnected(t, b, a, addr, false)
	if down.ReadErrors["connection"] == 0 {
		t.Errorf("/status read_errors = %v, want a connection error", down.ReadErrors)
	}
	code, body := getStatus(t, addr, "/metrics")
	if want := `laraplate_edge_input_connected{input="plc"} 0` + "\n"; code != http.StatusOK || !strings.Contains(body, want) {
		t.Errorf("/metrics = %d, lacks %q:\n%s", code, want, body)
	}

	// The broker is back: birth and data again, the retained messages
	// delivered anew by the subscriptions.
	dead := b.events()
	broker.Restart()
	b.waitFor(t, a, "both devices born again with data", func() bool {
		events := b.events()
		for _, d := range mqttDevices {
			if countEvents(events, d, "birth") < 2 || countEvents(events, d, "sample") <= countEvents(dead, d, "sample") {
				return false
			}
		}

		return true
	})
	waitConnected(t, b, a, addr, true)

	// An offline availability message is the death of the press alone.
	broker.Publish("plant/press/status", []byte("offline"), true, 1)
	b.waitFor(t, a, "the press dead by its availability topic", func() bool {
		return countEvents(b.events(), "press", "death") >= 2
	})
	if n := countEvents(b.events(), "meter", "death"); n != 1 {
		t.Errorf("meter: %d deaths, want 1 (the outage only)", n)
	}

	// The online message is its birth.
	broker.Publish("plant/press/status", []byte("online"), true, 1)
	b.waitFor(t, a, "the press born again by its availability topic", func() bool {
		return countEvents(b.events(), "press", "birth") >= 3
	})

	a.stop(t)

	events = b.events()
	checkMQTTLifecycle(t, events, map[string]int{"press": 2, "meter": 1})
	checkMQTTValues(t, events)
	b.noViolations(t)
	a.checkOutput(t, false)

	// The agent runs at its default level, info: values and connection
	// settings never appear there.
	out := a.stdout.String() + a.stderr.String()
	for _, secret := range []string{broker.Addr(), mqttMarker} {
		if strings.Contains(out, secret) {
			t.Errorf("the agent output holds %q: a connection setting or a sample value", secret)
		}
	}
}

// checkMQTTBirths: each device has a birth before any of its samples,
// announcing every signal with its data type and units.
func checkMQTTBirths(t *testing.T, events []busEvent) {
	t.Helper()

	want := map[string]map[string]string{
		"press": {"state": "string", "count": "number", "temp": "number"},
		"meter": {"power": "number"},
	}
	units := map[string]string{"temp": "C", "power": "kW"}
	for _, d := range mqttDevices {
		births := 0
		for _, e := range events {
			if e.device != d {
				continue
			}
			if e.kind == "sample" && births == 0 {
				t.Errorf("%s: a sample of %s before the birth", d, e.signal)

				break
			}
			if e.kind != "birth" {
				continue
			}
			births++
			got := map[string]string{}
			for _, s := range e.signals {
				got[s.Signal] = s.DataType
				if u, ok := units[s.Signal]; ok && (s.Unit == nil || *s.Unit != u) {
					t.Errorf("%s: birth announces %s with unit %v, want %s", d, s.Signal, s.Unit, u)
				}
			}
			if !slices.Equal(sortedPairs(got), sortedPairs(want[d])) {
				t.Errorf("%s: birth announces %v, want %v", d, got, want[d])
			}
		}
		if births == 0 {
			t.Errorf("%s: no birth", d)
		}
	}
}

// checkMQTTLifecycle: a device dies only after a birth, is born only when
// dead, has no sample while dead, and ends alive with the given number of
// deaths.
func checkMQTTLifecycle(t *testing.T, events []busEvent, wantDeaths map[string]int) {
	t.Helper()

	for _, d := range mqttDevices {
		alive := false
		deaths := 0
		for _, e := range events {
			if e.device != d {
				continue
			}
			switch e.kind {
			case "birth":
				if alive {
					t.Errorf("%s: a birth while alive", d)
				}
				alive = true
			case "death":
				if !alive {
					t.Errorf("%s: a death while dead", d)
				}
				alive = false
				deaths++
			case "sample":
				if !alive {
					t.Errorf("%s: sample of %s while dead", d, e.signal)
				}
			}
		}
		if deaths != wantDeaths[d] || !alive {
			t.Errorf("%s: %d deaths and alive %v at the end, want %d deaths and alive", d, deaths, alive, wantDeaths[d])
		}
	}
}

// checkMQTTValues: every sample has a value the test published, typed as the
// point says; the only bad ones are the press samples of the message that
// is not JSON.
func checkMQTTValues(t *testing.T, events []busEvent) {
	t.Helper()

	number := func(want ...float64) func(json.RawMessage) bool {
		return func(v json.RawMessage) bool {
			f, err := strconv.ParseFloat(string(v), 64)
			if err != nil {
				return false
			}

			return slices.ContainsFunc(want, func(w float64) bool { return math.Abs(f-w) < 1e-9 })
		}
	}
	want := map[string]func(json.RawMessage) bool{
		"press/state": func(v json.RawMessage) bool { return string(v) == strconv.Quote(mqttMarker) },
		"press/count": number(42, 43),
		"press/temp":  number(21.5, 22),
		"meter/power": number(3.5),
	}
	bad := 0
	for _, e := range events {
		if e.kind != "sample" {
			continue
		}
		check, ok := want[e.device+"/"+e.signal]
		if !ok {
			t.Errorf("unexpected sample %s/%s", e.device, e.signal)

			continue
		}
		if e.quality == "bad" && e.device == "press" {
			bad++

			continue
		}
		if e.quality != "good" || !check(e.value) {
			t.Errorf("%s/%s = %s (%s), want a published value (good)", e.device, e.signal, e.value, e.quality)
		}
	}
	if bad != len(mqttSignals["press"]) {
		t.Errorf("%d bad press samples, want %d (the message that is not JSON)", bad, len(mqttSignals["press"]))
	}
}
