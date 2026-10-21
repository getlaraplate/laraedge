//go:build e2e && unix

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/input/opcua/opcuatest"
)

// opcuaMarker is the value of the state variable while the agent runs: no
// log may hold it.
const opcuaMarker = "MARKER-7731-state"

// opcuaDevices lists the devices of the opcua input in a fixed order.
var opcuaDevices = []string{"line-1", "line-2"}

// opcuaPoints are the points of the two devices: speed and state are
// subscribed (speed with the long form), running and temp polled, and temp is
// addressed by its namespace URI. count is polled on the second device.
func opcuaPoints(ns uint16) map[string][]map[string]any {
	node := func(name string) string { return "ns=" + strconv.Itoa(int(ns)) + ";s=" + name }

	return map[string][]map[string]any{
		"line-1": {
			{"signal": "speed", "address": node("speed"), "type": "float64", "unit": "m/s",
				"read": map[string]any{"subscribe": map[string]any{"sampling": "100ms", "queue": 5}}},
			{"signal": "state", "address": node("state"), "type": "string", "read": "subscribe"},
			{"signal": "running", "address": node("deep"), "type": "bool", "read": map[string]any{"poll": "100ms"}},
			{"signal": "temp", "address": "nsu=" + opcuatest.Namespace + ";s=temp", "type": "float64", "read": map[string]any{"poll": "100ms"}},
		},
		"line-2": {
			{"signal": "count", "address": node("count"), "type": "int32", "read": map[string]any{"poll": "100ms"}},
		},
	}
}

// writeOPCUASetup writes the file provider setup of an opcua input reaching
// srv with Basic256Sha256 SignAndEncrypt and a death tolerance of 3 seconds,
// into dir, and returns the bootstrap path.
func writeOPCUASetup(t *testing.T, dir, url string, srv *opcuatest.Server) string {
	t.Helper()

	points := opcuaPoints(srv.NamespaceIndex)

	var devices []any
	for _, d := range opcuaDevices {
		devices = append(devices, map[string]any{"device": d, "points": points[d]})
	}

	doc := marshal(t, map[string]any{
		"schema":  "laraplate-edge-config/1",
		"version": 1,
		"inputs": []any{map[string]any{
			"id":              "plc",
			"driver":          "opcua",
			"death_tolerance": "3s",
			"connection": map[string]any{
				"endpoint":            srv.URL,
				"security_policy":     "Basic256Sha256",
				"security_mode":       "SignAndEncrypt",
				"publishing_interval": "100ms",
			},
			"devices": devices,
		}},
		"outputs": []any{map[string]any{
			"id": "laraplate", "type": "laraplate-http", "url": url, "token": "secret://env/" + tokenEnv,
			"batch": map[string]any{"max_samples": 40, "max_wait": "200ms"},
		}},
	})
	// JSON is YAML for the file provider.
	writeFile(t, filepath.Join(dir, "config.json"), string(doc))

	boot := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf("agent_id: e2e-opcua\nconfig:\n  provider: file\n  file: %s\ndata_dir: %s\nstatus_listen: 127.0.0.1:0\n",
		filepath.Join(dir, "config.json"), filepath.Join(dir, "data")))

	return boot
}

// runAgentCommand runs the built binary with args and returns its exit code,
// stdout and stderr.
func runAgentCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	cmd := exec.Command(agentBinary, args...)
	cmd.Env = agentEnv()
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	code := 0

	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v", args, err)
		}

		code = exit.ExitCode()
	}

	return code, stdout.String(), stderr.String()
}

// TestOPCUA: the built binary trusts the server's certificate with
// "opcua trust", then reads the library's OPC UA server through a proxy over
// Basic256Sha256 SignAndEncrypt. The backend receives the births, the
// subscribed and the polled values and a value changed on the server, with
// envelopes valid against the backend schema. When the proxy drops the
// connection and refuses the next ones the devices die after the death
// tolerance and no sample arrives; when it accepts again they are born again
// and the data resumes, the subscription created anew.
func TestOPCUA(t *testing.T) {
	opcuatest.SilenceLibraryLogs()

	b := newBackend(t)
	srv := opcuatest.Start(t, opcuatest.Options{Proxied: true})
	srv.SetValue(t, "state", opcuaMarker)

	dir := t.TempDir()
	boot := writeOPCUASetup(t, dir, b.url(), srv)
	// No document has been applied yet: the command reads the file, and
	// takes data_dir and agent_id from the bootstrap.
	conf := filepath.Join(dir, "config.json")
	trusted := filepath.Join(dir, "data", "opcua", "pki", "trusted", srv.Thumbprint+".der")

	// A thumbprint that is not the server's trusts nothing.
	wrong := strings.Repeat("0", len(srv.Thumbprint))
	if code, _, stderr := runAgentCommand(t, "opcua", "trust", "plc", "--config", conf, "--bootstrap", boot, "--thumbprint", wrong); code == 0 || !strings.Contains(stderr, "thumbprint mismatch") {
		t.Fatalf("opcua trust with a wrong thumbprint = %d %q, want a failure naming the mismatch", code, stderr)
	}
	if _, err := os.Stat(trusted); err == nil {
		t.Fatalf("the certificate was trusted after a thumbprint mismatch")
	}

	// The right one trusts it.
	code, stdout, stderr := runAgentCommand(t, "opcua", "trust", "plc", "--config", conf, "--bootstrap", boot, "--thumbprint", srv.Thumbprint)
	if code != 0 || !strings.Contains(stdout, "Trusted "+srv.Thumbprint) {
		t.Fatalf("opcua trust = %d\nstdout:\n%s\nstderr:\n%s\nwant exit 0 and Trusted <thumbprint>", code, stdout, stderr)
	}
	if got, err := os.ReadFile(trusted); err != nil || !bytes.Equal(got, srv.Cert) {
		t.Fatalf("trusted/%s.der = %d bytes (%v), want the server certificate", srv.Thumbprint, len(got), err)
	}
	if info, err := os.Stat(filepath.Join(dir, "data", "opcua", "pki", "own", "key.pem")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("own/key.pem = %v (%v), want mode 0600", info, err)
	}

	a := startAgent(t, boot)

	b.waitFor(t, a, "a good sample of every signal", func() bool {
		events := b.events()
		for _, d := range opcuaDevices {
			for _, p := range opcuaPoints(srv.NamespaceIndex)[d] {
				if !slices.ContainsFunc(events, func(e busEvent) bool {
					return e.device == d && e.kind == "sample" && e.signal == p["signal"]
				}) {
					return false
				}
			}
		}

		return true
	})
	addr := a.statusAddr(t)
	waitConnected(t, b, a, addr, true)

	events := b.events()
	checkOPCUABirths(t, events)
	for _, d := range opcuaDevices {
		if n := countEvents(events, d, "death"); n != 0 {
			t.Errorf("%s: %d deaths before the connection dropped, want 0", d, n)
		}
	}

	// A change on the server reaches the backend through the subscription.
	srv.SetValue(t, "speed", 2.5)
	b.waitFor(t, a, "the changed speed", func() bool {
		return slices.ContainsFunc(b.events(), func(e busEvent) bool {
			return e.device == "line-1" && e.signal == "speed" && string(e.value) == "2.5"
		})
	})

	// The network is lost: death after the tolerance, connection down.
	srv.Down(t)
	b.waitFor(t, a, "both devices dead", func() bool {
		events := b.events()
		for _, d := range opcuaDevices {
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
	code2, body := getStatus(t, addr, "/metrics")
	if want := `laraplate_edge_input_connected{input="plc"} 0` + "\n"; code2 != http.StatusOK || !strings.Contains(body, want) {
		t.Errorf("/metrics = %d, lacks %q:\n%s", code2, want, body)
	}

	// The network is back: birth and data again, the subscription anew.
	dead := b.events()
	srv.Up(t)
	b.waitFor(t, a, "both devices born again with data, the subscription included", func() bool {
		events := b.events()
		for _, d := range opcuaDevices {
			if countEvents(events, d, "birth") < 2 || countEvents(events, d, "sample") <= countEvents(dead, d, "sample") {
				return false
			}
		}

		return countSignal(events, "line-1", "speed") > countSignal(dead, "line-1", "speed")
	})
	waitConnected(t, b, a, addr, true)

	a.stop(t)

	events = b.events()
	checkOPCUAOutage(t, events)
	checkOPCUAValues(t, events)
	b.noViolations(t)
	a.checkOutput(t, false)

	out := a.stdout.String() + a.stderr.String()
	for _, secret := range []string{srv.URL, strings.TrimPrefix(srv.URL, "opc.tcp://"), opcuaMarker} {
		if strings.Contains(out, secret) {
			t.Errorf("the agent output holds %q: a connection setting or a sample value", secret)
		}
	}
}

// countSignal returns how many samples the device has of signal.
func countSignal(events []busEvent, device, signal string) int {
	n := 0
	for _, e := range events {
		if e.device == device && e.kind == "sample" && e.signal == signal {
			n++
		}
	}

	return n
}

// checkOPCUABirths: each device has a birth, before any of its samples,
// announcing every signal with its data type and the unit of speed.
func checkOPCUABirths(t *testing.T, events []busEvent) {
	t.Helper()

	want := map[string]map[string]string{
		"line-1": {"speed": "number", "state": "string", "running": "boolean", "temp": "number"},
		"line-2": {"count": "number"},
	}
	for _, d := range opcuaDevices {
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
				if s.Signal == "speed" && (s.Unit == nil || *s.Unit != "m/s") {
					t.Errorf("%s: birth announces speed with unit %v, want m/s", d, s.Unit)
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

// checkOPCUAValues: every sample is good and has a value the server held:
// speed 1.5 and, after the change, 2.5.
func checkOPCUAValues(t *testing.T, events []busEvent) {
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
		"line-1/speed":   number(1.5, 2.5),
		"line-1/state":   func(v json.RawMessage) bool { return string(v) == strconv.Quote(opcuaMarker) },
		"line-1/running": func(v json.RawMessage) bool { return string(v) == "true" },
		"line-1/temp":    number(20),
		"line-2/count":   number(1),
	}
	for _, e := range events {
		if e.kind != "sample" {
			continue
		}
		check, ok := want[e.device+"/"+e.signal]
		if !ok {
			t.Errorf("unexpected sample %s/%s", e.device, e.signal)

			continue
		}
		if e.quality != "good" || !check(e.value) {
			t.Errorf("%s/%s = %s (%s), want the server's value (good)", e.device, e.signal, e.value, e.quality)
		}
	}
}

// checkOPCUAOutage: between a death of a device and its next birth no sample
// of it arrives, a device dies only after a birth, and each device ends born
// again after a death.
func checkOPCUAOutage(t *testing.T, events []busEvent) {
	t.Helper()

	for _, d := range opcuaDevices {
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
		if deaths == 0 || !alive {
			t.Errorf("%s: %d deaths and alive %v at the end, want a death and a birth after it", d, deaths, alive)
		}
	}
}
