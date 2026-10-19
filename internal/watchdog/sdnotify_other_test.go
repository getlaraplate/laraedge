//go:build !linux

package watchdog

import (
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
)

func TestNotifierIsANoOpOutsideLinux(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	n := New(Config{
		LookupEnv: func(string) (string, bool) { return "/run/systemd/notify", true },
		Clock:     clk,
	})

	if n.Enabled() || n.Interval() != 0 {
		t.Fatalf("Enabled() = %v, Interval() = %v; want a disabled notifier", n.Enabled(), n.Interval())
	}
	if err := n.Run(t.Context()); err != nil {
		t.Fatalf("Run() = %v, want nil at once", err)
	}
	n.Ready()
	n.Stopping()
	if got := clk.PendingTimers(); got != 0 {
		t.Fatalf("PendingTimers() = %d, want 0", got)
	}
}
