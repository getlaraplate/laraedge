package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/transform"
)

const simulatedExample = "../../examples/simulated/config.yaml"

// probeLines are the lines the simulated example prints before the response
// time: device, signal, value, kind and quality, tab separated, in the order
// of the document.
var probeLines = []string{
	"press-07\tstate\t\"idle\"\tstring\tgood",
	"press-07\tgood_count\t0\tinteger\tgood",
	"press-07\tscrap_count\t0\tinteger\tgood",
	"press-07\tpress_force\t182\tfloat\tgood",
	"press-07\tdie_temperature\t78\tfloat\tgood",
	"press-08\tstate\t\"idle\"\tstring\tgood",
	"press-08\tgood_count\t0\tinteger\tgood",
	"press-08\tscrap_count\t0\tinteger\tgood",
	"press-08\tpress_force\t182\tfloat\tgood",
	"press-08\tdie_temperature\t78\tfloat\tgood",
}

func TestProbeTheSimulatedExample(t *testing.T) {
	for _, args := range [][]string{
		{"probe", "press-line", "--config", simulatedExample},
		{"probe", "--config", simulatedExample, "press-line"},
	} {
		var stdout, stderr bytes.Buffer

		if code := run(args, &stdout, &stderr); code != exitOK {
			t.Fatalf("%v: exit code = %d (stderr: %q)", args, code, stderr.String())
		}

		lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
		if len(lines) != len(probeLines)+1 {
			t.Fatalf("%v: %d lines, want %d: %q", args, len(lines), len(probeLines)+1, stdout.String())
		}

		for i, want := range probeLines {
			if lines[i] != want {
				t.Errorf("line %d = %q, want %q", i+1, lines[i], want)
			}
		}

		if last := lines[len(lines)-1]; !strings.HasPrefix(last, "response time: ") {
			t.Errorf("last line = %q, want the response time", last)
		}

		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty", stderr.String())
		}
	}
}

func TestProbeRecordsJSONLines(t *testing.T) {
	record := filepath.Join(t.TempDir(), "probe.jsonl")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"probe", "press-line", "--config", simulatedExample, "--record", record}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d (stderr: %q)", code, stderr.String())
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}

	doc := loadExample(t)
	samples, err := transform.ReadSamples(bytes.NewReader(data), pointKinds(doc))
	if err != nil {
		t.Fatalf("the recorded file does not read back: %v", err)
	}

	if len(samples) != len(probeLines) {
		t.Fatalf("recorded %d samples, want %d", len(samples), len(probeLines))
	}

	if first := strings.SplitN(string(data), "\n", 2)[0]; !strings.HasPrefix(first, `{"device":"press-07","signal":"state","ts":"`) ||
		!strings.HasSuffix(first, `"value":"idle","quality":"good"}`) {
		t.Errorf("first recorded line = %q", first)
	}

	entries, err := os.ReadDir(filepath.Dir(record))
	if err != nil || len(entries) != 1 {
		t.Errorf("the record directory holds %v (%v), want the record only", entries, err)
	}
}

// TestProbeThenConfigTest runs a recording through config test: the two
// subcommands agree on the file format.
func TestProbeThenConfigTest(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "probe.jsonl")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"probe", "press-line", "--config", simulatedExample, "--record", record}, &stdout, &stderr); code != exitOK {
		t.Fatalf("probe: exit code = %d (stderr: %q)", code, stderr.String())
	}

	stdout.Reset()
	if code := run([]string{"config", "test", simulatedExample, "--input", record}, &stdout, &stderr); code != exitOK {
		t.Fatalf("config test: exit code = %d (stderr: %q)", code, stderr.String())
	}

	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}

	if stdout.String() != string(recorded) {
		t.Errorf("a document without chains changed the recording:\n%s\nwant:\n%s", stdout.String(), recorded)
	}
}

func TestProbeFromTheLastValidDocument(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	absExample, err := filepath.Abs(simulatedExample)
	if err != nil {
		t.Fatal(err)
	}

	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, bootstrapFile, "agent_id: probe-01\nconfig: {provider: file, file: "+absExample+"}\ndata_dir: "+dataDir+"\n")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"probe", "press-line", "--bootstrap", bootstrapFile}, &stdout, &stderr); code != exitFailure {
		t.Fatalf("no document applied yet: exit code = %d, want 1 (stdout: %q)", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "no configuration") {
		t.Errorf("stderr = %q, want it to say no configuration was applied", stderr.String())
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(dataDir).SaveValid(loadExample(t)); err != nil {
		t.Fatal(err)
	}
	before := listTree(t, dataDir)

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"probe", "press-line", "--bootstrap", bootstrapFile}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d (stderr: %q)", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), probeLines[0]+"\n") {
		t.Errorf("stdout = %q, want the first sample", stdout.String())
	}

	if after := listTree(t, dataDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("probe changed data_dir: before %v, after %v", before, after)
	}
}

func TestProbeReportsAFailingStdout(t *testing.T) {
	var stderr bytes.Buffer

	if code := runProbe([]string{"press-line", "--config", simulatedExample}, failingWriter{}, &stderr); code != exitFailure {
		t.Fatalf("exit code = %d, want 1", code)
	}

	if !strings.Contains(stderr.String(), "write the samples") {
		t.Errorf("stderr = %q, want the write failure", stderr.String())
	}
}

func TestProbeFailures(t *testing.T) {
	dir := t.TempDir()
	badChain := filepath.Join(dir, "bad-chain.yaml")
	writeFile(t, badChain, chainDoc(`[{bit: 3}]`))
	broken := filepath.Join(dir, "broken.yaml")
	writeFile(t, broken, "schema: nope\n")

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr []string
	}{
		{"an unknown input", []string{"probe", "no-such-input", "--config", simulatedExample}, exitFailure, []string{"no-such-input", "press-line"}},
		{"a document with a chain type error", []string{"probe", "line", "--config", badChain}, exitFailure, []string{"bad-chain.yaml:", "step 1 (bit)"}},
		{"an invalid document", []string{"probe", "line", "--config", broken}, exitFailure, []string{"broken.yaml"}},
		{"a missing document", []string{"probe", "line", "--config", filepath.Join(dir, "missing.yaml")}, exitFailure, []string{"missing.yaml"}},
		{"a missing bootstrap", []string{"probe", "line", "--bootstrap", filepath.Join(dir, "missing.yaml")}, exitFailure, []string{"missing.yaml"}},
		{"an unwritable record", []string{"probe", "press-line", "--config", simulatedExample, "--record", filepath.Join(dir, "no-dir", "r.jsonl")}, exitFailure, []string{"--record", "r.jsonl"}},
		{"no input", []string{"probe", "--config", simulatedExample}, exitUsage, []string{"usage:"}},
		{"two inputs", []string{"probe", "a", "b", "--config", simulatedExample}, exitUsage, []string{"usage:"}},
		{"no document source", []string{"probe", "press-line"}, exitUsage, []string{"--config", "--bootstrap"}},
		{"an unknown flag", []string{"probe", "press-line", "--nope"}, exitUsage, []string{"nope"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(tt.args, &stdout, &stderr)

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %q)", code, tt.wantCode, stderr.String())
			}

			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty on failure", stdout.String())
			}

			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
		})
	}
}

func loadExample(t *testing.T) config.Document {
	t.Helper()

	data, err := os.ReadFile(simulatedExample)
	if err != nil {
		t.Fatal(err)
	}

	doc, msgs, err := config.Parse(data)
	if err != nil || len(msgs) > 0 {
		t.Fatalf("Parse: %v %v", err, msgs)
	}

	return doc
}

// listTree returns every path under dir with its size, sorted.
func listTree(t *testing.T, dir string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		out = append(out, fmt.Sprintf("%s %d %d", path, info.Size(), info.ModTime().UnixNano()))

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// opcuaProbeDoc is a document with one OPC UA input whose server is not
// there: nothing listens on the port.
func opcuaProbeDoc(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()

	return `schema: laraplate-edge-config/1
inputs:
  - id: press
    driver: opcua
    connection: {endpoint: 'opc.tcp://127.0.0.1:` + port + `', security_policy: None}
    devices:
      - device: press-07
        points:
          - {signal: speed, address: "ns=2;s=Speed", type: float64, read: {poll: 1s}}
outputs:
  - {id: main, type: laraplate-http, url: 'http://127.0.0.1:8000', token: secret://env/TOKEN}
`
}

func TestProbeOfAnOPCUAInputUsesTheBootstrapDataDir(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")
	writeFile(t, configFile, opcuaProbeDoc(t))
	dataDir := filepath.Join(dir, "data")
	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, bootstrapFile, bootstrapYAML("plant-a-edge-01", configFile, dataDir))

	var stdout, stderr bytes.Buffer

	code := run([]string{"probe", "press", "--config", configFile, "--bootstrap", bootstrapFile}, &stdout, &stderr)
	if code != exitFailure || !strings.Contains(stderr.String(), `opcua input "press": probe: `) {
		t.Fatalf("exit code = %d, stderr = %q; want a failed connection to the absent server", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "127.0.0.1") {
		t.Fatalf("stderr names the endpoint: %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "opcua", "pki", "own", "cert.der")); err != nil {
		t.Fatalf("the agent certificate was not created under the bootstrap's data_dir: %v", err)
	}

	stderr.Reset()
	if code := run([]string{"probe", "press", "--config", configFile}, &stdout, &stderr); code != exitFailure || !strings.Contains(stderr.String(), "data_dir") {
		t.Fatalf("without a bootstrap: exit code = %d, stderr = %q; want an error naming data_dir", code, stderr.String())
	}
}
