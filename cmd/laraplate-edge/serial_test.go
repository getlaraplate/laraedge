package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowseASerialInputRefusesTheOtherDriversFlags(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	writeFile(t, cfg, `schema: laraplate-edge-config/1
inputs:
  - id: gauge
    driver: serial
    connection: {device: /dev/ttyUSB0, baud: 9600}
    devices:
      - device: g
        points:
          - {signal: w, address: '^(.+)$', type: float64, read: event}
outputs:
  - {id: main, type: laraplate-http, url: 'http://127.0.0.1:8000', token: secret://env/TOKEN}
`)
	for _, flag := range [][]string{{"--limit", "5"}, {"--node", "i=85"}, {"--depth", "2"}, {"--filter", "#"}, {"--duration", "2s"}} {
		t.Run(flag[0], func(t *testing.T) {
			r := runCLI(t, "", append([]string{"browse", "gauge", "--config", cfg}, flag...)...)
			if r.code != exitUsage || !strings.Contains(r.stderr, errSerialBrowseFlags) {
				t.Errorf("code = %d, stderr = %q", r.code, r.stderr)
			}
		})
	}
}
