package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"
)

func TestStdlogBridge(t *testing.T) {
	tests := []struct {
		name  string
		level slog.Level
		write func(l *log.Logger)
		want  []string
	}{
		{
			name:  "one line per record at debug",
			level: slog.LevelDebug,
			write: func(l *log.Logger) { l.Printf("error verifying session signature: %s", "bad") },
			want:  []string{"error verifying session signature: bad"},
		},
		{
			name:  "several lines in one write",
			level: slog.LevelDebug,
			write: func(l *log.Logger) { l.Print("first\nsecond") },
			want:  []string{"first", "second"},
		},
		{
			name:  "blank lines are dropped",
			level: slog.LevelDebug,
			write: func(l *log.Logger) { l.Print("\n\nonly\n") },
			want:  []string{"only"},
		},
		{
			name:  "nothing above debug",
			level: slog.LevelInfo,
			write: func(l *log.Logger) { l.Print("hidden") },
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			std := log.New(StdlogBridge(New(&buf, tt.level)), "", 0)
			tt.write(std)

			var got []string

			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if line == "" {
					continue
				}

				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("line is not JSON: %v: %q", err, line)
				}
				if rec["level"] != "DEBUG" || rec["source"] != "gopcua" {
					t.Fatalf("record = %v, want level DEBUG and source gopcua", rec)
				}

				got = append(got, rec["msg"].(string))
			}

			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("messages = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStdlogBridgeWithoutLoggerDiscards(t *testing.T) {
	n, err := StdlogBridge(nil).Write([]byte("dropped\n"))
	if n != len("dropped\n") || err != nil {
		t.Fatalf("Write = %d, %v; want the whole line accepted", n, err)
	}
}
