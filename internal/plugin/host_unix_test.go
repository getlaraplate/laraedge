//go:build unix

package plugin_test

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/plugin"
)

// TestHostRestartsAfterThreeFailedHealthChecks stops the plugin process
// (SIGSTOP) so that its health checks get no answer within 2 s.
func TestHostRestartsAfterThreeFailedHealthChecks(t *testing.T) {
	h := newHarness(t, newSpec(t, ""))
	h.start()
	pid := h.host.Status().Pid
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		h.waitFor("the health timer", func() bool { return h.clk.PendingTimers() == 1 })
		h.clk.Advance(5 * time.Second)
		h.waitFor("the health check deadline", func() bool { return h.clk.PendingTimers() == 1 })
		h.clk.Advance(2 * time.Second)
	}
	h.waitRestarts(1)
	if got := h.host.Status().RestartsByReason[plugin.ReasonHealth]; got != 1 {
		t.Fatalf("health restarts = %d, want 1", got)
	}
	if got := strings.Count(h.log.String(), "plugin health check failed"); got != 3 {
		t.Fatalf("logged %d failed health checks, want 3", got)
	}

	h.expectBackoff(time.Second)
	h.waitUp()
	if h.host.Status().Pid == pid {
		t.Fatal("the restarted plugin kept the old pid")
	}
	if _, generation := h.host.Conn(); generation != 2 {
		t.Fatalf("generation after the restart = %d, want 2", generation)
	}
}

// TestHostStopSequence: a plugin that answers Shutdown but keeps running
// gets SIGTERM after 5 s; one that also ignores SIGTERM is killed 5 s later.
func TestHostStopSequence(t *testing.T) {
	tests := []struct {
		name      string
		behaviour string
		wantKill  bool
	}{
		{name: "SIGTERM ends it", behaviour: `{"ignore_shutdown":true}`},
		{name: "kill ends it", behaviour: `{"ignore_shutdown":true,"ignore_sigterm":true}`, wantKill: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, newSpec(t, tt.behaviour))
			h.start()

			done := make(chan error, 1)
			go func() { done <- h.host.Stop(context.Background()) }()

			h.waitFor("Shutdown sent", func() bool { return strings.Contains(h.log.String(), "plugin stopping") })
			h.waitFor("the Shutdown deadline", func() bool { return h.clk.PendingTimers() == 1 })
			h.clk.Advance(5 * time.Second)
			h.waitFor("SIGTERM sent", func() bool { return strings.Contains(h.log.String(), "sending SIGTERM") })
			if tt.wantKill {
				h.waitFor("the kill deadline", func() bool { return h.clk.PendingTimers() == 1 })
				h.clk.Advance(5 * time.Second)
			}

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Stop() = %v", err)
				}
			case <-time.After(guard):
				t.Fatal("Stop did not return")
			}
			if killed := strings.Contains(h.log.String(), "killing it"); killed != tt.wantKill {
				t.Fatalf("killed = %v, want %v; log:\n%s", killed, tt.wantKill, h.log.String())
			}
			if got := h.host.Status().State; got != plugin.StateStopped {
				t.Fatalf("state = %s, want stopped", got)
			}
		})
	}
}
