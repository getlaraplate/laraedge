package main

import (
	"bytes"
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
			name:       "run is a stub",
			args:       []string{"run", "--bootstrap", "bootstrap.yaml"},
			wantCode:   1,
			wantStderr: []string{"not implemented yet"},
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
			name:       "config validate is a stub",
			args:       []string{"config", "validate", "agent.yaml"},
			wantCode:   1,
			wantStderr: []string{"not implemented yet"},
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
			name:       "status is a stub with default listen",
			args:       []string{"status"},
			wantCode:   1,
			wantStderr: []string{"not implemented yet"},
		},
		{
			name:       "status accepts listen",
			args:       []string{"status", "--listen", "127.0.0.1:9999"},
			wantCode:   1,
			wantStderr: []string{"not implemented yet"},
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
