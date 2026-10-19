//go:build linux

package watchdog

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
)

// maxUsec is the largest WATCHDOG_USEC a time.Duration holds.
const maxUsec = uint64(math.MaxInt64 / int64(time.Microsecond))

// Notifier sends the sd_notify notifications. Ready, Stopping and Run are
// safe for concurrent use.
type Notifier struct {
	socket   string
	interval time.Duration
	clk      clock.Clock
	log      *slog.Logger

	ready    sync.Once
	stopping sync.Once
}

// New reads the environment systemd gives the service. A malformed
// WATCHDOG_USEC or WATCHDOG_PID disables the watchdog pings with a warning;
// READY=1 and STOPPING=1 are still sent.
func New(cfg Config) *Notifier {
	n := &Notifier{clk: cfg.Clock, log: cfg.Logger}
	if n.clk == nil {
		n.clk = clock.Real()
	}
	if n.log == nil {
		n.log = slog.Default()
	}
	lookup := cfg.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	pid := cfg.PID
	if pid == 0 {
		pid = os.Getpid()
	}

	n.socket, _ = lookup("NOTIFY_SOCKET")
	if n.socket == "" {
		return n
	}

	raw, ok := lookup("WATCHDOG_USEC")
	if !ok {
		return n
	}
	usec, err := strconv.ParseUint(raw, 10, 63)
	if err != nil || usec == 0 || usec > maxUsec {
		n.log.Warn("watchdog: WATCHDOG_USEC is not a positive number of microseconds; watchdog pings disabled")

		return n
	}
	if rawPID, ok := lookup("WATCHDOG_PID"); ok {
		owner, err := strconv.Atoi(rawPID)
		if err != nil {
			n.log.Warn("watchdog: WATCHDOG_PID is not a process id; watchdog pings disabled")

			return n
		}
		if owner != pid {
			return n
		}
	}
	n.interval = time.Duration(usec) * time.Microsecond / 2

	return n
}

// Enabled reports whether NOTIFY_SOCKET is set.
func (n *Notifier) Enabled() bool {
	return n.socket != ""
}

// Interval returns the period of the watchdog pings, half of WATCHDOG_USEC,
// or zero when there are none.
func (n *Notifier) Interval() time.Duration {
	return n.interval
}

// Ready sends READY=1, once.
func (n *Notifier) Ready() {
	n.ready.Do(func() { n.notify("READY=1") })
}

// Stopping sends STOPPING=1, once.
func (n *Notifier) Stopping() {
	n.stopping.Do(func() { n.notify("STOPPING=1") })
}

// Run sends WATCHDOG=1 every Interval until ctx is done; it returns nil, at
// once when there are no pings to send.
func (n *Notifier) Run(ctx context.Context) error {
	if n.socket == "" || n.interval <= 0 {
		return nil
	}

	for {
		timer := n.clk.NewTimer(n.interval)
		select {
		case <-ctx.Done():
			timer.Stop()

			return nil
		case <-timer.C():
			n.notify("WATCHDOG=1")
		}
	}
}

// notify sends one datagram to NOTIFY_SOCKET, a path or an abstract name
// starting with @. A failure is logged and changes nothing else: systemd,
// if it is waiting, acts on the missing notification.
func (n *Notifier) notify(state string) {
	if n.socket == "" {
		return
	}
	if err := send(n.socket, state); err != nil {
		n.log.Warn("watchdog: cannot notify systemd", "state", state, "error", err)
	}
}

func send(socket, state string) error {
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("dial NOTIFY_SOCKET: %w", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("write NOTIFY_SOCKET: %w", err)
	}

	return nil
}
