//go:build linux

package watchdog

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/clock"
)

// guard bounds every wait; it is a deadlock guard, never a timing assertion.
const guard = 10 * time.Second

const testPID = 4242

// fakeSystemd is a NOTIFY_SOCKET receiving the notifications.
type fakeSystemd struct {
	conn *net.UnixConn
	addr string
}

func newFakeSystemd(t *testing.T, abstract bool) *fakeSystemd {
	t.Helper()

	var name string
	if abstract {
		name = fmt.Sprintf("@laraplate-edge-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	} else {
		// A short directory: a socket path is limited to 108 bytes.
		dir, err := os.MkdirTemp("", "wd")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		name = filepath.Join(dir, "notify")
	}

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen %s: %v", name, err)
	}
	t.Cleanup(func() { conn.Close() })

	return &fakeSystemd{conn: conn, addr: name}
}

// next returns the next notification.
func (f *fakeSystemd) next(t *testing.T) string {
	t.Helper()

	if err := f.conn.SetReadDeadline(time.Now().Add(guard)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	n, _, err := f.conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("no notification received: %v", err)
	}

	return string(buf[:n])
}

// none checks that no notification is waiting.
func (f *fakeSystemd) none(t *testing.T) {
	t.Helper()

	if err := f.conn.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	if n, _, err := f.conn.ReadFromUnix(buf); err == nil {
		t.Fatalf("unexpected notification %q", buf[:n])
	}
}

func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]

		return v, ok
	}
}

func newNotifier(vars map[string]string, clk clock.Clock, logs *bytes.Buffer) *Notifier {
	return New(Config{
		LookupEnv: env(vars),
		Clock:     clk,
		Logger:    slog.New(slog.NewJSONHandler(logs, nil)),
		PID:       testPID,
	})
}

func TestReadyAndStoppingAreSentOnce(t *testing.T) {
	for _, abstract := range []bool{false, true} {
		t.Run("abstract="+strconv.FormatBool(abstract), func(t *testing.T) {
			sd := newFakeSystemd(t, abstract)
			n := newNotifier(map[string]string{"NOTIFY_SOCKET": sd.addr}, clock.NewFake(time.Unix(0, 0)), &bytes.Buffer{})

			n.Ready()
			n.Ready()
			if got := sd.next(t); got != "READY=1" {
				t.Fatalf("notification = %q, want READY=1", got)
			}
			n.Stopping()
			n.Stopping()
			if got := sd.next(t); got != "STOPPING=1" {
				t.Fatalf("notification = %q, want STOPPING=1", got)
			}
			sd.none(t)
		})
	}
}

func TestWatchdogPingsAtHalfTheInterval(t *testing.T) {
	sd := newFakeSystemd(t, false)
	clk := clock.NewFake(time.Unix(0, 0))
	n := newNotifier(map[string]string{
		"NOTIFY_SOCKET":  sd.addr,
		"WATCHDOG_USEC":  "20000000",
		"WATCHDOG_PID":   strconv.Itoa(testPID),
		"UNRELATED_NAME": "x",
	}, clk, &bytes.Buffer{})

	if got := n.Interval(); got != 10*time.Second {
		t.Fatalf("Interval() = %v, want half of WATCHDOG_USEC", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()

	for i := range 3 {
		waitTimer(t, clk)
		clk.Advance(9 * time.Second)
		sd.none(t)
		clk.Advance(time.Second)
		if got := sd.next(t); got != "WATCHDOG=1" {
			t.Fatalf("ping %d = %q, want WATCHDOG=1", i, got)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(guard):
		t.Fatal("Run did not return after cancel")
	}
	sd.none(t)
}

func TestWatchdogDisabled(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]string
		wantLog string
	}{
		{name: "no WATCHDOG_USEC", vars: map[string]string{}},
		{name: "a watchdog for another process", vars: map[string]string{"WATCHDOG_USEC": "2000000", "WATCHDOG_PID": "1"}},
		{name: "a malformed WATCHDOG_USEC", vars: map[string]string{"WATCHDOG_USEC": "soon"}, wantLog: "WATCHDOG_USEC"},
		{name: "a zero WATCHDOG_USEC", vars: map[string]string{"WATCHDOG_USEC": "0"}, wantLog: "WATCHDOG_USEC"},
		{name: "a WATCHDOG_USEC beyond a duration", vars: map[string]string{"WATCHDOG_USEC": "9223372036854775807"}, wantLog: "WATCHDOG_USEC"},
		{name: "a malformed WATCHDOG_PID", vars: map[string]string{"WATCHDOG_USEC": "2000000", "WATCHDOG_PID": "me"}, wantLog: "WATCHDOG_PID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sd := newFakeSystemd(t, false)
			tt.vars["NOTIFY_SOCKET"] = sd.addr
			var logs bytes.Buffer
			n := newNotifier(tt.vars, clock.NewFake(time.Unix(0, 0)), &logs)

			if got := n.Interval(); got != 0 {
				t.Fatalf("Interval() = %v, want 0", got)
			}
			if err := n.Run(t.Context()); err != nil {
				t.Fatalf("Run() = %v, want nil at once", err)
			}
			if tt.wantLog != "" && !bytes.Contains(logs.Bytes(), []byte(tt.wantLog)) {
				t.Fatalf("logs = %s, want a warning naming %s", logs.String(), tt.wantLog)
			}

			// Ready still goes to systemd.
			n.Ready()
			if got := sd.next(t); got != "READY=1" {
				t.Fatalf("notification = %q, want READY=1", got)
			}
		})
	}
}

func TestWithoutNotifySocketNothingIsSent(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	n := newNotifier(map[string]string{"WATCHDOG_USEC": "2000000"}, clk, &bytes.Buffer{})

	if n.Enabled() {
		t.Fatal("Enabled() = true without NOTIFY_SOCKET")
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

func TestASocketThatIsGoneIsLogged(t *testing.T) {
	dir, err := os.MkdirTemp("", "wd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	var logs bytes.Buffer
	n := newNotifier(map[string]string{"NOTIFY_SOCKET": filepath.Join(dir, "gone")}, clock.NewFake(time.Unix(0, 0)), &logs)

	n.Ready()

	if !bytes.Contains(logs.Bytes(), []byte(`"level":"WARN"`)) || !bytes.Contains(logs.Bytes(), []byte("READY=1")) {
		t.Fatalf("logs = %s, want a warning about READY=1", logs.String())
	}
}

func waitTimer(t *testing.T, clk *clock.Fake) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for clk.PendingTimers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watchdog did not arm its timer")
		}
		runtime.Gosched()
	}
}
