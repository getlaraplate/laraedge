//go:build linux

package serial_test

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/serial"
	"github.com/getlaraplate/laraedge/internal/input/serial/ptytest"
)

var testMode = serial.Mode{Baud: 9600, DataBits: 8, StopBits: 1, Parity: "none"}

func readFull(t *testing.T, r io.Reader, want int) []byte {
	t.Helper()
	buf := make([]byte, 0, want)
	tmp := make([]byte, 64)
	deadline := time.Now().Add(5 * time.Second)
	for len(buf) < want && time.Now().Before(deadline) {
		n, err := r.Read(tmp)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		buf = append(buf, tmp[:n]...)
	}
	return buf
}

func TestOpenPortReadsAndWritesAPseudoTerminal(t *testing.T) {
	master, slave := ptytest.Open(t)
	port, err := serial.OpenPort(slave, testMode)
	if err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	t.Cleanup(func() { _ = port.Close() })

	// Nothing pending: the read times out with 0, nil.
	buf := make([]byte, 16)
	if n, err := port.Read(buf); n != 0 || err != nil {
		t.Fatalf("idle Read = %d, %v; want 0, nil", n, err)
	}

	if _, err := master.Write([]byte("12.5\r")); err != nil {
		t.Fatalf("master write: %v", err)
	}
	if got := string(readFull(t, port, 5)); got != "12.5\r" {
		t.Errorf("port read %q, want %q", got, "12.5\r")
	}

	if _, err := port.Write([]byte("PING\r")); err != nil {
		t.Fatalf("port write: %v", err)
	}
	if got := string(readFull(t, master, 5)); got != "PING\r" {
		t.Errorf("master read %q, want %q", got, "PING\r")
	}
}

func TestOpenPortTwiceIsBusy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores TIOCEXCL")
	}
	_, slave := ptytest.Open(t)
	first, err := serial.OpenPort(slave, testMode)
	if err != nil {
		t.Fatalf("first OpenPort: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := serial.OpenPort(slave, testMode); !errors.Is(err, serial.ErrPortBusy) {
		t.Fatalf("second OpenPort error = %v, want ErrPortBusy", err)
	}
}

func TestOpenPortMissing(t *testing.T) {
	_, err := serial.OpenPort("/dev/ttylaraedge-none", testMode)
	if !errors.Is(err, serial.ErrPortMissing) {
		t.Fatalf("OpenPort error = %v, want ErrPortMissing", err)
	}
}

// TestReadAfterMasterCloses covers the other end going away (a pulled cable).
// Observed with go.bug.st/serial v1.8.0: the port becomes readable with zero
// bytes and the library returns a PortError with code PortClosed ("Port has
// been closed"), so the wrapper needs no detection of its own. The contract is
// that Read then returns an error, never 0, nil forever.
func TestReadAfterMasterCloses(t *testing.T) {
	master, slave := ptytest.Open(t)
	port, err := serial.OpenPort(slave, testMode)
	if err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	t.Cleanup(func() { _ = port.Close() })
	if err := master.Close(); err != nil {
		t.Fatalf("close master: %v", err)
	}
	buf := make([]byte, 16)
	for i := 0; i < 20; i++ {
		n, err := port.Read(buf)
		if err != nil {
			return
		}
		if n != 0 {
			t.Fatalf("Read returned %d bytes after the master closed", n)
		}
	}
	t.Fatal("Read kept returning 0, nil after the master closed")
}
