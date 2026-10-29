package plugin_test

import (
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/plugin"
)

// TestHostRestartsAboveMaxMemory sets max_memory to one byte, which every
// process exceeds, and expects a restart with reason memory at the 10 s check.
func TestHostRestartsAboveMaxMemory(t *testing.T) {
	spec := newSpec(t, "")
	spec.MaxMemory = 1
	h := newHarness(t, spec)
	h.start()

	h.waitFor("the health and memory timers", func() bool { return h.clk.PendingTimers() == 2 })
	h.clk.Advance(10 * time.Second)
	h.waitFor("a memory restart", func() bool {
		return h.host.Status().RestartsByReason[plugin.ReasonMemory] == 1
	})
	if !strings.Contains(h.log.String(), "level=WARN msg=\"plugin memory above max_memory\"") {
		t.Fatalf("no WARN about max_memory:\n%s", h.log.String())
	}
	if got := h.host.Status().LastError; !strings.Contains(got, "max_memory") {
		t.Fatalf("LastError = %q, want max_memory", got)
	}
}

// TestHostMemoryUnderTheLimitKeepsRunning: the memory check passes and is
// armed again 10 s later.
func TestHostMemoryUnderTheLimitKeepsRunning(t *testing.T) {
	spec := newSpec(t, "")
	spec.MaxMemory = 1 << 40
	h := newHarness(t, spec)
	h.start()
	pid := h.host.Status().Pid

	h.waitFor("the health and memory timers", func() bool { return h.clk.PendingTimers() == 2 })
	h.clk.Advance(10 * time.Second)
	h.waitFor("one memory and one health check, both timers armed again", func() bool {
		log := h.log.String()

		return strings.Count(log, "plugin memory checked") == 1 &&
			strings.Count(log, "plugin health check passed") == 1 && h.clk.PendingTimers() == 2
	})
	h.clk.Advance(10 * time.Second)
	h.waitFor("a second memory check", func() bool {
		return strings.Count(h.log.String(), "plugin memory checked") == 2
	})

	if status := h.host.Status(); status.State != plugin.StateUp || status.Restarts != 0 || status.Pid != pid {
		t.Fatalf("Status() = %+v, want the same plugin up without restarts", status)
	}
}
