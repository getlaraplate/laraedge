//go:build unix

package plugintest

import (
	"syscall"
	"testing"
	"time"
)

// stopBound bounds the wait for a stopped process; a stop takes
// milliseconds, so it is a deadlock guard, never a timing assertion.
const stopBound = 10 * time.Second

// StopProcess stops process pid with SIGSTOP and waits until it is stopped:
// kill returns before every thread of the process has stopped, and a thread
// still running answers the next request (C61, C64).
func StopProcess(t testing.TB, pid int) {
	t.Helper()

	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(stopBound)
	for !ProcessStopped(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d not stopped %s after SIGSTOP", pid, stopBound)
		}
		time.Sleep(time.Millisecond)
	}
}
