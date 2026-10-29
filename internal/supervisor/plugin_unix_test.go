//go:build unix

package supervisor

import (
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCloseStopsPluginsWithinTheStopDeadline: plugins that answer neither
// Shutdown nor SIGTERM (stopped with SIGSTOP) are killed once the stop
// deadline of spec section 6 has passed, 5 s then 5 s more, all at once:
// the second does not wait for the first.
func TestCloseStopsPluginsWithinTheStopDeadline(t *testing.T) {
	f, _ := newPluginFixture(t)
	demo := installPlugin(t, "demo", sampleScript)
	other := installPlugin(t, "other", sampleScript)
	f.mustApply(f.pluginDocument(f.document(1, nil, nil),
		[]any{demo.entry(nil), other.entry(nil)},
		pluginInput("p1", "demo", "d1", 0, "s1"), pluginInput("p2", "other", "d2", 0, "s1")))

	pids := []int{f.runningPluginPid("demo"), f.runningPluginPid("other")}
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
	}

	closed := make(chan error, 1)
	go func() { closed <- f.sup.Close() }()

	f.waitLogCount(`"msg":"plugin stopping"`, 2)
	pending := f.clk.PendingTimers()
	f.clk.Advance(5 * time.Second)

	f.waitLogCount("sending SIGTERM", 2)
	deadline := time.Now().Add(guard)
	for f.clk.PendingTimers() < pending {
		if time.Now().After(deadline) {
			t.Fatalf("the kill timers were not armed: %d pending, want %d", f.clk.PendingTimers(), pending)
		}
		runtime.Gosched()
	}
	f.clk.Advance(5 * time.Second)

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() = %v", err)
		}
	case <-time.After(guard):
		t.Fatal("Close did not return once the stop deadline passed")
	}
	if got := strings.Count(f.logs.String(), "killing it"); got != 2 {
		t.Fatalf("%d plugins killed, want 2", got)
	}
}
