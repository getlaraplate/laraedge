//go:build linux

// Package ptytest opens pseudo-terminal pairs for tests of code that talks to
// a serial port. It is a regular package, not a _test file, so that other
// packages (internal/e2e) can use it.
package ptytest

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// Open opens a pseudo-terminal and returns its master and the path of its
// slave, which a test opens as the serial port. The master is closed on
// cleanup.
func Open(t testing.TB) (master *os.File, slavePath string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlock pty: %v", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("pty number: %v", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", n)
}
