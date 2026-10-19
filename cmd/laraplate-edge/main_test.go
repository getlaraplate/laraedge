package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swolley/laraplate-edge/internal/version"
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

	invalid := filepath.Join(t.TempDir(), "invalid.yaml")
	doc := "schema: laraplate-edge-config/1\ninputs: []\noutputs:\n  - {id: main, type: laraplate-http, url: 'http://erp.example.com', token: plain-token-value}\n"
	if err := os.WriteFile(invalid, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		file       string
		wantCode   int
		wantStdout string
		wantStderr []string
	}{
		{
			name:       "a document this build applies",
			file:       fixtures + "build.yaml",
			wantCode:   0,
			wantStdout: fixtures + "build.yaml: valid\n",
		},
		{
			name:     "the same document as JSON",
			file:     fixtures + "build.json",
			wantCode: 0,
		},
		{
			name:     "the spec example refused by the build check",
			file:     fixtures + "spec-5.3.yaml",
			wantCode: 1,
			wantStderr: []string{
				fixtures + "spec-5.3.yaml: inputs.0.driver: not available in this build\n",
				fixtures + "spec-5.3.yaml: plugins.0: not available in this build\n",
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
			if strings.Contains(stderr.String(), "plain-token-value") {
				t.Errorf("stderr echoes a value: %q", stderr.String())
			}
		})
	}
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
