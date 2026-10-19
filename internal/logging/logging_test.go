package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "info", want: slog.LevelInfo},
		{in: "warn", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "", wantErr: true},
		{in: "INFO", wantErr: true},
		{in: "trace", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseLevel(tt.in)

			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "debug, info, warn or error") {
					t.Fatalf("ParseLevel(%q) = %v, %v; want an error listing the levels", tt.in, got, err)
				}

				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseLevel(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestNewWritesJSONAtTheLevel(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)

	log.Debug("hidden")
	log.Info("shown", "output", "main")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want only the info line: %q", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if rec["msg"] != "shown" || rec["level"] != "INFO" || rec["output"] != "main" {
		t.Fatalf("record = %v", rec)
	}
}

func TestNewRedactsSecretLookingAttributes(t *testing.T) {
	const secret = "s3cr3t-value-xyz"

	tests := []struct {
		key      string
		redacted bool
	}{
		{key: "token", redacted: true},
		{key: "Token", redacted: true},
		{key: "status_token", redacted: true},
		{key: "password", redacted: true},
		{key: "db_password", redacted: true},
		{key: "secret", redacted: true},
		{key: "client_secret", redacted: true},
		{key: "api_key", redacted: true},
		{key: "token_ref", redacted: false},
		{key: "output", redacted: false},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			var buf bytes.Buffer
			log := New(&buf, slog.LevelDebug)

			log.Info("m", tt.key, secret)
			log.Info("m", slog.Group("g", tt.key, secret))

			got := buf.String()
			if tt.redacted && strings.Contains(got, secret) {
				t.Fatalf("logs contain the value of %q: %s", tt.key, got)
			}
			if tt.redacted && strings.Count(got, Redacted) != 2 {
				t.Fatalf("logs = %s, want %q twice", got, Redacted)
			}
			if !tt.redacted && strings.Count(got, secret) != 2 {
				t.Fatalf("logs = %s, want the value of %q kept", got, tt.key)
			}
		})
	}
}
