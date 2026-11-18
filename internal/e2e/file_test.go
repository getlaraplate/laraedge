//go:build e2e && unix

package e2e

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileContextMarker is a context value of the report: no log may hold it.
const fileContextMarker = "SN-4471-marker"

// fileReport is a long CSV report of one part: a measured value per row, the
// serial and the order of the part on every row.
const fileReport = "Characteristic,Value,Serial,Order\n" +
	"bore,12.5," + fileContextMarker + ",ORD-9\n" +
	"length,100," + fileContextMarker + ",ORD-9\n"

// writeFileInputSetup writes the file provider setup of a file input on
// watched into dir and returns the bootstrap path.
func writeFileInputSetup(t *testing.T, dir, url, watched string) string {
	t.Helper()

	doc := marshal(t, map[string]any{
		"schema":  "laraplate-edge-config/1",
		"version": 1,
		"inputs": []any{map[string]any{
			"id":              "cmm",
			"driver":          "file",
			"death_tolerance": "3s",
			"connection": map[string]any{
				"directory":  watched,
				"pattern":    "*.csv",
				"format":     "csv",
				"interval":   "500ms",
				"stable_for": "500ms",
				"csv": map[string]any{
					"layout": "long", "key_column": "Characteristic", "value_column": "Value",
				},
				"context": map[string]any{"serial": "Serial", "order_ref": "Order"},
			},
			"devices": []any{map[string]any{"device": "cmm-1", "points": []any{
				map[string]any{"signal": "bore", "address": "bore", "type": "float64", "unit": "mm", "read": "event"},
				map[string]any{"signal": "length", "address": "length", "type": "int32", "unit": "mm", "read": "event"},
			}}},
		}},
		"outputs": []any{map[string]any{
			"id": "laraplate", "type": "laraplate-http", "url": url, "token": "secret://env/" + tokenEnv,
			"batch": map[string]any{"max_samples": 40, "max_wait": "200ms"},
		}},
	})
	// JSON is YAML for the file provider.
	writeFile(t, filepath.Join(dir, "config.json"), string(doc))

	boot := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf("agent_id: e2e-file\nconfig:\n  provider: file\n  file: %s\ndata_dir: %s\nstatus_listen: 127.0.0.1:0\n",
		filepath.Join(dir, "config.json"), filepath.Join(dir, "data")))

	return boot
}

// TestFileInput: the built binary watches a directory. A long CSV report
// dropped there is read once it has been stable, its samples reach the
// backend typed as the points say with the serial and the order of the part
// as their context, the envelope is valid against the backend schema, the
// report ends in done/ and no log holds a context value.
func TestFileInput(t *testing.T) {
	b := newBackend(t)

	dir := t.TempDir()
	watched := filepath.Join(dir, "reports")
	if err := os.Mkdir(watched, 0o755); err != nil {
		t.Fatal(err)
	}
	boot := writeFileInputSetup(t, dir, b.url(), watched)
	a := startAgent(t, boot)

	// The exporter writes a temporary file and renames it, as exporters do.
	writeFile(t, filepath.Join(watched, "part.tmp"), fileReport)
	if err := os.Rename(filepath.Join(watched, "part.tmp"), filepath.Join(watched, "part.csv")); err != nil {
		t.Fatal(err)
	}

	b.waitFor(t, a, "the samples of the report", func() bool {
		events := b.events()

		return countSignal(events, "cmm-1", "bore") > 0 && countSignal(events, "cmm-1", "length") > 0
	})

	// The commit comes at a later scan: the report moves to done/.
	done := filepath.Join(watched, "done", "part.csv")
	b.waitFor(t, a, "the report in done/", func() bool {
		_, err := os.Stat(done)

		return err == nil
	})
	if _, err := os.Stat(filepath.Join(watched, "part.csv")); !os.IsNotExist(err) {
		t.Errorf("part.csv is still in the watched directory (stat error %v)", err)
	}

	a.stop(t)

	events := b.events()
	if n := countEvents(events, "cmm-1", "birth"); n != 1 {
		t.Errorf("cmm-1: %d births, want 1", n)
	}
	values := map[string]string{}
	for _, e := range events {
		if e.kind != "sample" {
			continue
		}
		if e.quality != "good" {
			t.Errorf("%s: quality %q, want good", e.signal, e.quality)
		}
		want := map[string]string{"serial": fileContextMarker, "order_ref": "ORD-9"}
		if !maps.Equal(e.context, want) {
			t.Errorf("%s: context %v, want %v", e.signal, e.context, want)
		}
		if _, dup := values[e.signal]; dup {
			t.Errorf("%s: delivered more than once", e.signal)
		}
		values[e.signal] = string(e.value)
	}
	if values["bore"] != "12.5" || values["length"] != "100" {
		t.Errorf("values = %v, want bore 12.5 and length 100", values)
	}
	if entries, err := os.ReadDir(filepath.Join(watched, "error")); err == nil && len(entries) > 0 {
		t.Errorf("error/ holds %d files, want none", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "file", "seen", "cmm.json")); err != nil {
		t.Errorf("the ledger of read files is missing: %v", err)
	}

	b.noViolations(t)
	a.checkOutput(t, false)

	// The agent runs at its default level, info: a context value never
	// appears there, though the file name does.
	out := a.stdout.String() + a.stderr.String()
	if strings.Contains(out, fileContextMarker) {
		t.Errorf("the agent output holds a context value")
	}
	if !strings.Contains(out, "part.csv") {
		t.Errorf("the agent output does not name the file it read")
	}
}

// qdasReport is a Q-DAS report of two parts written as coded lines: the
// serial (K0014) and the order (K0053) follow the values of each part.
const qdasReport = "K0100 2\n" +
	"K2001/1 bore\n" +
	"K2001/2 length\n" +
	"K0001/1 12.5\n" +
	"K0001/2 100\n" +
	"K0004/0 03.05.2021/14:12:50\n" +
	"K0014/0 " + fileContextMarker + "-A\n" +
	"K0053/0 ORD-1\n" +
	"K0001/1 12.7\n" +
	"K0001/2 101\n" +
	"K0004/0 03.05.2021/14:15:10\n" +
	"K0014/0 " + fileContextMarker + "-B\n" +
	"K0053/0 ORD-2\n"

// writeQDASInputSetup writes the file provider setup of a qdas input on
// watched into dir and returns the bootstrap path.
func writeQDASInputSetup(t *testing.T, dir, url, watched string) string {
	t.Helper()

	doc := marshal(t, map[string]any{
		"schema":  "laraplate-edge-config/1",
		"version": 1,
		"inputs": []any{map[string]any{
			"id":              "cmm",
			"driver":          "file",
			"death_tolerance": "3s",
			"connection": map[string]any{
				"directory":  watched,
				"pattern":    "*.dfq",
				"format":     "qdas",
				"interval":   "500ms",
				"stable_for": "500ms",
				"timezone":   "UTC",
				"context":    map[string]any{"serial": "K0014", "order_ref": "K0053"},
			},
			"devices": []any{map[string]any{"device": "cmm-1", "points": []any{
				map[string]any{"signal": "bore", "address": "bore", "type": "float64", "unit": "mm", "read": "event"},
				map[string]any{"signal": "length", "address": "length", "type": "int32", "unit": "mm", "read": "event"},
			}}},
		}},
		"outputs": []any{map[string]any{
			"id": "laraplate", "type": "laraplate-http", "url": url, "token": "secret://env/" + tokenEnv,
			"batch": map[string]any{"max_samples": 40, "max_wait": "200ms"},
		}},
	})
	writeFile(t, filepath.Join(dir, "config.json"), string(doc))

	boot := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf("agent_id: e2e-qdas\nconfig:\n  provider: file\n  file: %s\ndata_dir: %s\nstatus_listen: 127.0.0.1:0\n",
		filepath.Join(dir, "config.json"), filepath.Join(dir, "data")))

	return boot
}

// TestFileInputQDAS: the built binary reads a Q-DAS report of two parts.
// Every sample carries the serial and the order of its own part, the
// envelope is valid against the backend schema, the report ends in done/.
func TestFileInputQDAS(t *testing.T) {
	b := newBackend(t)

	dir := t.TempDir()
	watched := filepath.Join(dir, "reports")
	if err := os.Mkdir(watched, 0o755); err != nil {
		t.Fatal(err)
	}
	boot := writeQDASInputSetup(t, dir, b.url(), watched)
	a := startAgent(t, boot)

	writeFile(t, filepath.Join(watched, "part.tmp"), qdasReport)
	if err := os.Rename(filepath.Join(watched, "part.tmp"), filepath.Join(watched, "part.dfq")); err != nil {
		t.Fatal(err)
	}

	b.waitFor(t, a, "the samples of both parts", func() bool {
		events := b.events()

		return countSignal(events, "cmm-1", "bore") >= 2 && countSignal(events, "cmm-1", "length") >= 2
	})

	done := filepath.Join(watched, "done", "part.dfq")
	b.waitFor(t, a, "the report in done/", func() bool {
		_, err := os.Stat(done)

		return err == nil
	})

	a.stop(t)

	events := b.events()
	// The first value of each part tells the parts apart.
	wantContext := map[string]map[string]string{
		"12.5": {"serial": fileContextMarker + "-A", "order_ref": "ORD-1"},
		"12.7": {"serial": fileContextMarker + "-B", "order_ref": "ORD-2"},
		"100":  {"serial": fileContextMarker + "-A", "order_ref": "ORD-1"},
		"101":  {"serial": fileContextMarker + "-B", "order_ref": "ORD-2"},
	}
	seen := map[string]bool{}
	for _, e := range events {
		if e.kind != "sample" {
			continue
		}
		if e.quality != "good" {
			t.Errorf("%s: quality %q, want good", e.signal, e.quality)
		}
		v := string(e.value)
		want, ok := wantContext[v]
		if !ok {
			t.Errorf("%s: unexpected value %s", e.signal, v)

			continue
		}
		if !maps.Equal(e.context, want) {
			t.Errorf("%s=%s: context %v, want %v", e.signal, v, e.context, want)
		}
		if seen[v] {
			t.Errorf("%s=%s: delivered more than once", e.signal, v)
		}
		seen[v] = true
	}
	if len(seen) != len(wantContext) {
		t.Errorf("values seen %v, want %d of them", seen, len(wantContext))
	}
	if entries, err := os.ReadDir(filepath.Join(watched, "error")); err == nil && len(entries) > 0 {
		t.Errorf("error/ holds %d files, want none", len(entries))
	}

	b.noViolations(t)
	a.checkOutput(t, false)

	out := a.stdout.String() + a.stderr.String()
	if strings.Contains(out, fileContextMarker) {
		t.Errorf("the agent output holds a context value")
	}
}
