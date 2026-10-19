//go:build linux

package main

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/input"
)

// TestRunAgentNotifiesSystemd checks the wiring of the watchdog: READY=1
// once the configuration is applied, STOPPING=1 on shutdown.
func TestRunAgentNotifiesSystemd(t *testing.T) {
	sockDir, err := os.MkdirTemp("", "wd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sockPath := filepath.Join(sockDir, "notify")
	sock, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	next := func() string {
		t.Helper()
		if err := sock.SetReadDeadline(time.Now().Add(guard)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 256)
		n, _, err := sock.ReadFromUnix(buf)
		if err != nil {
			t.Fatalf("no notification received: %v", err)
		}

		return string(buf[:n])
	}

	dir := t.TempDir()
	be := newIngest(t)
	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, bootstrapFile, bootstrapYAML("plant-a-edge-01", quietConfig(t, dir, be.srv.URL), filepath.Join(dir, "data")))

	a := startAgent(t, bootstrapFile, agentDeps{
		lookupEnv: (&fakeEnv{vars: map[string]string{runEnv: runToken, "NOTIFY_SOCKET": sockPath}}).lookup,
		clock:     clock.NewFake(time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)),
		drivers:   map[string]input.Factory{config.DriverSimulated: quietFactory},
		logger:    slog.New(slog.DiscardHandler),
	})

	if got := next(); got != "READY=1" {
		t.Fatalf("notification = %q, want READY=1", got)
	}
	if v := a.sup.Status().Version; v != 1 {
		t.Fatalf("READY=1 sent with version %d applied, want 1", v)
	}

	a.stop(t)
	if got := next(); got != "STOPPING=1" {
		t.Fatalf("notification = %q, want STOPPING=1", got)
	}
}
