package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/version"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr []string
	}{
		{
			name:       "version prints the version",
			args:       []string{"version"},
			wantCode:   0,
			wantStdout: version.String() + "\n",
		},
		{
			name:       "no subcommand prints usage",
			args:       nil,
			wantCode:   2,
			wantStderr: []string{"usage:"},
		},
		{
			name:       "unknown subcommand prints usage",
			args:       []string{"frobnicate"},
			wantCode:   2,
			wantStderr: []string{"unknown command", "frobnicate", "usage:"},
		},
		{
			name:       "run with a missing bootstrap file",
			args:       []string{"run", "--bootstrap", "no-such-bootstrap.yaml"},
			wantCode:   1,
			wantStderr: []string{"laraplate-edge run:", "no-such-bootstrap.yaml"},
		},
		{
			name:       "run requires bootstrap",
			args:       []string{"run"},
			wantCode:   2,
			wantStderr: []string{"--bootstrap"},
		},
		{
			name:       "run rejects an unknown flag",
			args:       []string{"run", "--nope"},
			wantCode:   2,
			wantStderr: []string{"nope"},
		},
		{
			name:       "config validate on a missing file",
			args:       []string{"config", "validate", "no-such-agent.yaml"},
			wantCode:   1,
			wantStderr: []string{"no-such-agent.yaml"},
		},
		{
			name:       "config validate rejects extra arguments",
			args:       []string{"config", "validate", "a.yaml", "b.yaml"},
			wantCode:   2,
			wantStderr: []string{"usage:"},
		},
		{
			name:       "config validate requires a file",
			args:       []string{"config", "validate"},
			wantCode:   2,
			wantStderr: []string{"usage:"},
		},
		{
			name:       "config without validate prints usage",
			args:       []string{"config"},
			wantCode:   2,
			wantStderr: []string{"usage:"},
		},
		{
			name:       "status rejects extra arguments",
			args:       []string{"status", "extra"},
			wantCode:   2,
			wantStderr: []string{"usage:"},
		},
		{
			name:       "run rejects an unknown log level",
			args:       []string{"run", "--bootstrap", "b.yaml", "--log-level", "trace"},
			wantCode:   2,
			wantStderr: []string{"--log-level", "debug, info, warn or error"},
		},
		{
			name:       "version rejects extra arguments",
			args:       []string{"version", "extra"},
			wantCode:   2,
			wantStderr: []string{"usage:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(tt.args, &stdout, &stderr)

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantCode != 0 && stdout.Len() != 0 {
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

func TestConfigValidate(t *testing.T) {
	const fixtures = "../../internal/config/testdata/"

	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.yaml")
	doc := "schema: laraplate-edge-config/1\ninputs: []\noutputs:\n  - {id: main, type: laraplate-http, url: 'http://erp.example.com', token: plain-token-value}\n"
	if err := os.WriteFile(invalid, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(dir, "valid.yaml")
	writeFile(t, valid, driversDoc("hr:10"))
	validJSON := filepath.Join(dir, "valid.json")
	writeFile(t, validJSON, driversJSONDoc("hr:10"))
	badAddress := filepath.Join(dir, "bad-address.yaml")
	writeFile(t, badAddress, driversDoc("hr:70000"))
	opcuaOffline := filepath.Join(dir, "opcua.yaml")
	writeFile(t, opcuaOffline, opcuaDoc)

	tests := []struct {
		name       string
		file       string
		wantCode   int
		wantStdout string
		wantStderr []string
	}{
		{
			name:       "a document this build applies",
			file:       valid,
			wantCode:   0,
			wantStdout: valid + ": valid\n",
		},
		{
			name:       "the same document as JSON",
			file:       validJSON,
			wantCode:   0,
			wantStdout: validJSON + ": valid\n",
		},
		{
			name:     "a driver refusing its configuration (decision M1)",
			file:     fixtures + "build.yaml",
			wantCode: 1,
			wantStderr: []string{
				fixtures + "build.yaml: inputs.0: simulated input \"sim-line\": connection: unknown key auth",
			},
		},
		{
			name:     "a driver refusing its configuration, as JSON",
			file:     fixtures + "build.json",
			wantCode: 1,
			wantStderr: []string{
				fixtures + "build.json: inputs.0: simulated input \"sim-line\": connection: unknown key auth",
			},
		},
		{
			name:     "a Modbus address, offline",
			file:     badAddress,
			wantCode: 1,
			wantStderr: []string{
				badAddress + ": inputs.1: modbus input \"plc\": devices.0.points.0: address: offset: want an integer from 0 to 65535\n",
			},
		},
		{
			name:     "OPC UA addresses and authentication, offline",
			file:     opcuaOffline,
			wantCode: 1,
			wantStderr: []string{
				opcuaOffline + ": inputs.0: opcua input \"press\": devices.0.points.1: address: namespace index: want an integer from 0 to 65535\n",
				opcuaOffline + ": inputs.1: opcua input \"oven\": connection.auth: user and password need a security_policy other than None\n",
			},
		},
		{
			name:     "the spec example refused by the build check",
			file:     fixtures + "spec-5.3.yaml",
			wantCode: 1,
			wantStderr: []string{
				fixtures + "spec-5.3.yaml: inputs.0.connection.auth.password: not available in this build\n",
				fixtures + "spec-5.3.yaml: inputs.0: opcua input \"press-line\": devices.0.points.0: type: required\n",
			},
		},
		{
			name:     "every schema and semantic problem",
			file:     invalid,
			wantCode: 1,
			wantStderr: []string{
				invalid + ": outputs.0.token: must be a secret:// reference",
				invalid + ": outputs.0.url: must use https",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run([]string{"config", "validate", tt.file}, &stdout, &stderr)

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if code == 0 && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
			if code != 0 && stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty on failure", stdout.String())
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
			for _, value := range []string{"plain-token-value", "plc.example", "1502", "70000"} {
				if strings.Contains(stderr.String(), value) {
					t.Errorf("stderr echoes a value: %q", stderr.String())
				}
			}
		})
	}
}

// opcuaDoc is a document with two OPC UA inputs the driver refuses offline:
// a NodeId with a namespace index out of syntax, and a user name over an
// unprotected connection. The endpoint host never resolves.
const opcuaDoc = `schema: laraplate-edge-config/1
inputs:
  - id: press
    driver: opcua
    connection: {endpoint: 'opc.tcp://plc.example:4840'}
    devices:
      - device: press-07
        points:
          - {signal: state, address: "ns=3;s=Machine.State", type: string, read: subscribe}
          - {signal: speed, address: "ns=x;s=Speed", type: float64, read: {poll: 1s}}
  - id: oven
    driver: opcua
    connection:
      endpoint: 'opc.tcp://plc.example:4840'
      security_policy: None
      auth: {user: mes, password: secret://env/OPCUA_PASSWORD}
    devices:
      - device: oven-02
        points:
          - {signal: temp, address: "ns=2;i=7", type: float64, read: subscribe}
outputs:
  - {id: main, type: laraplate-http, url: 'http://127.0.0.1:8000', token: secret://env/TOKEN}
`

// driversDoc is a document with a simulated and a Modbus input, the Modbus
// point at address. The Modbus factory never connects, so the host need not
// exist.
func driversDoc(address string) string {
	return `schema: laraplate-edge-config/1
inputs:
  - id: sim
    driver: simulated
    connection:
      scenario: ` + demoScenario + `
    devices:
      - device: oven-02
        points:
          - {signal: state, address: state, type: string, read: subscribe}
  - id: plc
    driver: modbus
    connection: {transport: tcp, host: plc.example, port: 1502}
    devices:
      - device: press-07
        points:
          - {signal: speed, address: "` + address + `", type: int16, read: {poll: 1s}}
outputs:
  - {id: main, type: laraplate-http, url: 'http://127.0.0.1:8000', token: secret://env/TOKEN}
`
}

// driversJSONDoc is the document of driversDoc written as JSON, the form
// the laraplate provider delivers.
func driversJSONDoc(address string) string {
	return `{
  "schema": "laraplate-edge-config/1",
  "inputs": [
    {
      "id": "sim", "driver": "simulated",
      "connection": {"scenario": ` + demoScenario + `},
      "devices": [{"device": "oven-02", "points": [
        {"signal": "state", "address": "state", "type": "string", "read": "subscribe"}
      ]}]
    },
    {
      "id": "plc", "driver": "modbus",
      "connection": {"transport": "tcp", "host": "plc.example", "port": 1502},
      "devices": [{"device": "press-07", "points": [
        {"signal": "speed", "address": "` + address + `", "type": "int16", "read": {"poll": "1s"}}
      ]}]
    }
  ],
  "outputs": [{"id": "main", "type": "laraplate-http", "url": "http://127.0.0.1:8000", "token": "secret://env/TOKEN"}]
}
`
}

// TestRunLogsToStdout checks that run writes its JSON logs to stdout (spec
// 10.5) and only its own failure message to stderr. The status port is taken,
// so the agent stops at start after logging.
func TestRunLogsToStdout(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	dir := t.TempDir()
	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	boot := "agent_id: logs-01\n" +
		"config: {provider: file, file: " + filepath.Join(dir, "agent.yaml") + "}\n" +
		"data_dir: " + filepath.Join(dir, "data") + "\n" +
		"status_listen: " + taken.Addr().String() + "\n"
	if err := os.WriteFile(bootstrapFile, []byte(boot), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--bootstrap", bootstrapFile}, &stdout, &stderr)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "laraplate-edge run:") || !strings.Contains(stderr.String(), "status_listen") {
		t.Errorf("stderr = %q, want the failure naming status_listen", stderr.String())
	}
	if strings.Contains(stderr.String(), `"level"`) {
		t.Errorf("stderr = %q, want no log line", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"msg":"agent: stopped"`) {
		t.Fatalf("stdout = %q, want the JSON log of the stop", stdout.String())
	}
	for line := range strings.Lines(stdout.String()) {
		if !json.Valid([]byte(line)) {
			t.Errorf("stdout line %q is not JSON", line)
		}
	}
}

const validateManifest = `{"kinds":{"input":{"address_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string"},"browse":false,"connection_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"},"read_modes":["poll"],"types":["int16"]}},"name":"demo","protocol_versions":["v1"],"version":"0.1.0"}`

func TestConfigValidateChecksPlugins(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins", "demo")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pluginDir, "plugin.json"), validateManifest)
	writeFile(t, filepath.Join(pluginDir, "demo"), "not really a binary")

	docText := func(sha string) string {
		return `{"schema":"laraplate-edge-config/1","version":1,
"plugins":[{"id":"demo","path":"plugins/demo/demo","sha256":"` + sha + `","manifest_sha256":"` + hashHex([]byte(validateManifest)) + `"}],
"outputs":[{"id":"o0","type":"laraplate-http","url":"https://example.com","token":"secret://env/T","route":{"include":["*"]}}],
"inputs":[{"id":"i0","driver":"plugin:demo","devices":[{"device":"d","points":[{"signal":"s","type":"int16","address":"x","read":{"poll":"1s"}}]}]}]}`
	}
	doc := filepath.Join(dir, "config.json")

	// A wrong binary hash is reported, relative to the config file's directory.
	writeFile(t, doc, docText(strings.Repeat("0", 64)))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "validate", doc}, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit code = %d, want 1 (stderr: %q)", code, stderr.String())
	}
	if want := doc + ": plugins.0.sha256: does not match the plugin binary"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}

	// The right hash passes.
	writeFile(t, doc, docText(hashHex([]byte("not really a binary"))))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"config", "validate", doc}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}

	// A missing manifest is a problem.
	if err := os.Remove(filepath.Join(pluginDir, "plugin.json")); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := run([]string{"config", "validate", doc}, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if want := "plugins.0: plugin.json not found next to the binary"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestConfigValidatePluginsDir(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins", "demo")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pluginDir, "demo"), "binary")
	zeros := strings.Repeat("0", 64)
	doc := filepath.Join(dir, "config.json")
	writeFile(t, doc, `{"schema":"laraplate-edge-config/1","version":1,
"plugins":[{"id":"demo","path":"plugins/demo/demo","sha256":"`+zeros+`","manifest_sha256":"`+zeros+`"}],
"outputs":[{"id":"o0","type":"laraplate-http","url":"https://example.com","token":"secret://env/T","route":{"include":["*"]}}],
"inputs":[{"id":"i0","driver":"plugin:demo","devices":[{"device":"d","points":[{"signal":"s","address":"a","type":"int16","read":{"poll":"1s"}}]}]}]}`)

	const outsideMsg = "plugins.0.path: must be inside plugins_dir"
	tests := []struct {
		name        string
		args        []string
		wantCode    int
		wantOutside bool
	}{
		{name: "dir containing the binary", args: []string{"config", "validate", doc, "--plugins-dir", filepath.Join(dir, "plugins")}, wantCode: exitFailure},
		{name: "flag before the file", args: []string{"config", "validate", "--plugins-dir", t.TempDir(), doc}, wantCode: exitFailure, wantOutside: true},
		{name: "relative dir refused", args: []string{"config", "validate", doc, "--plugins-dir", "plugins"}, wantCode: exitUsage},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			if code := run(tc.args, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %q)", code, tc.wantCode, stderr.String())
			}
			if got := strings.Contains(stderr.String(), outsideMsg); got != tc.wantOutside {
				t.Errorf("stderr = %q, outside message present = %v, want %v", stderr.String(), got, tc.wantOutside)
			}
		})
	}
}
