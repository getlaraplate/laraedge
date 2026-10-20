// Package watchdog tells systemd about the agent through sd_notify (spec
// 10.1): READY=1 once the agent is up, WATCHDOG=1 at half of WATCHDOG_USEC
// while it runs, and STOPPING=1 on shutdown. It does nothing when
// NOTIFY_SOCKET is not set, and nothing at all outside Linux, where the
// service manager has its own recovery actions. No systemd library is used:
// a notification is one datagram on a unix socket.
package watchdog

import (
	"log/slog"

	"github.com/getlaraplate/laraplate-edge/internal/clock"
)

// Config configures a Notifier.
type Config struct {
	// LookupEnv reads NOTIFY_SOCKET, WATCHDOG_USEC and WATCHDOG_PID; nil
	// means os.LookupEnv.
	LookupEnv func(name string) (string, bool)
	// Clock times the watchdog pings; nil means clock.Real().
	Clock clock.Clock
	// Logger receives the logs; nil means slog.Default().
	Logger *slog.Logger
	// PID is the process id WATCHDOG_PID must name; zero means os.Getpid().
	PID int
}
