package modbus

import (
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
)

// blackHole returns the port of a loopback listener that never accepts and
// whose accept queue is full, so a new connection waits for a SYN-ACK that
// never comes: a dial to it runs until its own timeout, as a dial to a
// black-holed host does. Linux keeps backlog+1 connections queued; the
// listener has a backlog of 0 and is filled until a dial times out.
func blackHole(t *testing.T) uint16 {
	t.Helper()

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(sa.(*syscall.SockaddrInet4).Port)

	addr := (&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}).String()
	for range 10 {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return port
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	t.Fatal("the accept queue never filled")

	return 0
}

// TestCloseDuringADialToABlackHole (ruling R10) runs the library's real dial,
// whose fixed 5 s timeout nothing cancels: Close returns well before it.
func TestCloseDuringADialToABlackHole(t *testing.T) {
	port := blackHole(t)
	conn := `{"transport":"tcp","host":"127.0.0.1","port":` + strconv.Itoa(int(port)) + `,"timeout":"100ms"}`

	h := newHarness(t, modbusInput([]byte(conn), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}))
	opening := make(chan struct{}, 1)
	h.in.newClient = func(c Connection) (client, error) {
		lib, err := newLibraryClient(c)
		if err != nil {
			return nil, err
		}
		return signalOpen{lib, opening}, nil
	}
	if err := h.in.Start(t.Context(), h.emit); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opening:
	case <-time.After(guard):
		t.Fatal("the input never opened a client")
	}

	// A generous bound, far below the library's 5 s dial (ruling R3).
	const bound = 2 * time.Second
	start := time.Now()
	if err := h.in.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if took := time.Since(start); took > bound {
		t.Errorf("Close took %v during a dial, want under %v", took, bound)
	}
	h.closed.Store(true)
	if h.in.Health().Connected {
		t.Error("connected after Close")
	}
}

// signalOpen is a client that reports when its Open starts.
type signalOpen struct {
	client
	opening chan<- struct{}
}

func (s signalOpen) Open() error {
	select {
	case s.opening <- struct{}{}:
	default:
	}

	return s.client.Open()
}
