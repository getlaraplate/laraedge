//go:build e2e && linux

package e2e

import (
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/serial/ptytest"
)

// serialPoint is the single point of the serial documents: a bore diameter
// read from frames such as 01A+1.250 or from a bare number.
func serialPoint(address string) map[string]any {
	return map[string]any{"signal": "bore_d", "address": address, "type": "float64", "unit": "mm", "read": "event"}
}

// writeSerialSetup writes the file provider setup of a serial input on port
// into dir and returns the bootstrap path.
func writeSerialSetup(t *testing.T, dir, url, port string, connection map[string]any, point map[string]any) string {
	t.Helper()

	connection["device"] = port
	connection["baud"] = 9600
	doc := marshal(t, map[string]any{
		"schema":  "laraplate-edge-config/1",
		"version": 1,
		"inputs": []any{map[string]any{
			"id":              "station",
			"driver":          "serial",
			"death_tolerance": "10s",
			"connection":      connection,
			"devices":         []any{map[string]any{"device": "gauge-1", "points": []any{point}}},
		}},
		"outputs": []any{map[string]any{
			"id": "laraplate", "type": "laraplate-http", "url": url, "token": "secret://env/" + tokenEnv,
			"batch": map[string]any{"max_samples": 40, "max_wait": "200ms"},
		}},
	})
	writeFile(t, filepath.Join(dir, "config.json"), string(doc))

	boot := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf("agent_id: e2e-serial\nconfig:\n  provider: file\n  file: %s\ndata_dir: %s\nstatus_listen: 127.0.0.1:0\n",
		filepath.Join(dir, "config.json"), filepath.Join(dir, "data")))

	return boot
}

// TestSerialInput: the built binary reads a pseudo-terminal as a serial port.
// A scanned serial followed by two measurements reaches the backend as two
// samples of bore_d with the serial as their context; with a request the
// agent writes the command to the port and the answer becomes a sample.
func TestSerialInput(t *testing.T) {
	t.Run("scan then measurements", func(t *testing.T) {
		b := newBackend(t)
		master, slave := ptytest.Open(t)

		dir := t.TempDir()
		boot := writeSerialSetup(t, dir, b.url(), slave, map[string]any{
			"context": []any{map[string]any{"key": "serial", "match": `^SN(\w+)$`}},
		}, serialPoint(`^01A([+-]?\d+\.\d+)$`))
		a := startAgent(t, boot)

		// The input is open when the gauge is born.
		b.waitFor(t, a, "the birth of the gauge", func() bool {
			return countEvents(b.events(), "gauge-1", "birth") > 0
		})

		if _, err := master.WriteString("SN42\r01A+1.250\r01A+1.300\r"); err != nil {
			t.Fatal(err)
		}
		b.waitFor(t, a, "two samples of bore_d", func() bool {
			return countSignal(b.events(), "gauge-1", "bore_d") >= 2
		})

		a.stop(t)

		var values []string
		for _, e := range b.events() {
			if e.kind != "sample" {
				continue
			}
			if e.quality != "good" {
				t.Errorf("%s: quality %q, want good", e.signal, e.quality)
			}
			if want := map[string]string{"serial": "42"}; !maps.Equal(e.context, want) {
				t.Errorf("%s: context %v, want %v", e.signal, e.context, want)
			}
			values = append(values, string(e.value))
		}
		if strings.Join(values, ",") != "1.25,1.3" {
			t.Errorf("values = %v, want 1.25 and 1.3", values)
		}

		b.noViolations(t)
		a.checkOutput(t, false)
	})

	t.Run("request and answer", func(t *testing.T) {
		b := newBackend(t)
		master, slave := ptytest.Open(t)

		dir := t.TempDir()
		boot := writeSerialSetup(t, dir, b.url(), slave, map[string]any{
			"requests":        []any{map[string]any{"command": "?\r", "every": "1s"}},
			"request_timeout": "500ms",
		}, serialPoint(`^([+-]?\d+\.\d+)$`))
		a := startAgent(t, boot)

		// The instrument: the first "?\r" it sees is answered.
		seen := make(chan string, 1)
		go func() {
			buf := make([]byte, 64)
			var got strings.Builder
			for {
				n, err := master.Read(buf)
				if err != nil {
					return
				}
				got.Write(buf[:n])
				if strings.Contains(got.String(), "?\r") {
					seen <- got.String()
					_, _ = master.WriteString("5.0\r")

					return
				}
			}
		}()

		select {
		case <-seen:
		case <-time.After(20 * time.Second):
			t.Fatalf("the agent never wrote the request\nstderr:\n%s", a.stderr.String())
		}
		b.waitFor(t, a, "the sample of the answer", func() bool {
			return countSignal(b.events(), "gauge-1", "bore_d") >= 1
		})

		a.stop(t)

		for _, e := range b.events() {
			if e.kind == "sample" && (string(e.value) != "5" || e.quality != "good") {
				t.Errorf("%s: value %s quality %q, want 5 good", e.signal, e.value, e.quality)
			}
		}

		b.noViolations(t)
		a.checkOutput(t, false)
	})
}
