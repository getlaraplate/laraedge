//go:build unix

package plugin_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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

// failHealthChecks lets n health checks of a stopped plugin time out.
func (h *harness) failHealthChecks(n int) {
	h.t.Helper()

	for range n {
		h.waitFor("the health timer", func() bool { return h.clk.PendingTimers() == 1 })
		h.clk.Advance(5 * time.Second)
		h.waitFor("the health check deadline", func() bool { return h.clk.PendingTimers() == 1 })
		h.clk.Advance(2 * time.Second)
	}
}

func TestHostHealthFailuresResetOnSuccess(t *testing.T) {
	h := newHarness(t, newSpec(t, ""))
	h.start()
	pid := h.host.Status().Pid

	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	h.failHealthChecks(2)
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the health timer", func() bool { return h.clk.PendingTimers() == 1 })
	h.clk.Advance(5 * time.Second)
	h.waitFor("a passed health check", func() bool {
		return strings.Count(h.log.String(), "plugin health check passed") == 1 && h.clk.PendingTimers() == 1
	})
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	h.failHealthChecks(2)
	h.waitFor("the fourth failure", func() bool {
		return strings.Count(h.log.String(), "plugin health check failed") == 4 && h.clk.PendingTimers() == 1
	})

	if status := h.host.Status(); status.State != plugin.StateUp || status.Restarts != 0 || status.Pid != pid {
		t.Fatalf("Status() = %+v, want the same plugin up without restarts", status)
	}
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
}

// alive reports whether process pid exists (a reaped process does not).
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestHostStopDuringHandshake(t *testing.T) {
	h := newHarness(t, newSpec(t, `{"block_before_handshake":true,"pid_file":"pid.txt"}`))

	started := make(chan error, 1)
	go func() { started <- h.host.Start(context.Background()) }()
	pidFile := filepath.Join(h.spec.Dir, "pid.txt")
	var pid int
	h.waitFor("the plugin process", func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil || len(data) == 0 {
			return false
		}
		pid, err = strconv.Atoi(string(data))

		return err == nil
	})

	if err := h.host.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if alive(pid) {
		t.Fatal("the plugin process is alive after Stop returned")
	}
	select {
	case err := <-started:
		if err == nil || err.Error() != "the plugin host is stopped" {
			t.Fatalf("Start() = %v, want the host is stopped", err)
		}
	default:
		t.Fatal("Start had not returned when Stop returned")
	}
	if status := h.host.Status(); status.State != plugin.StateStopped || status.Pid != 0 {
		t.Fatalf("Status() = %+v, want stopped", status)
	}
}

func TestHostConcurrentStopsWaitForTheReap(t *testing.T) {
	h := newHarness(t, newSpec(t, `{"ignore_shutdown":true}`))
	h.start()
	pid := h.host.Status().Pid

	type result struct {
		err   error
		alive bool
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			err := h.host.Stop(context.Background())
			results <- result{err: err, alive: alive(pid)}
		}()
	}
	h.waitFor("Shutdown sent", func() bool { return strings.Contains(h.log.String(), "plugin stopping") })
	h.waitFor("the Shutdown deadline", func() bool { return h.clk.PendingTimers() == 1 })
	h.clk.Advance(5 * time.Second)

	for range 2 {
		select {
		case r := <-results:
			if r.err != nil || r.alive {
				t.Fatalf("Stop() = %v with the plugin alive %v; want nil after the reap", r.err, r.alive)
			}
		case <-time.After(guard):
			t.Fatal("Stop did not return")
		}
	}
	if got := strings.Count(h.log.String(), "plugin stopping"); got != 1 {
		t.Fatalf("the stop sequence ran %d times, want 1", got)
	}
}
