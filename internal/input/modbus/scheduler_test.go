package modbus

import (
	"fmt"
	"io"
	"net"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	mbus "github.com/simonvetter/modbus"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

// framings are the two ways the raw test server frames an exchange: Modbus
// TCP (MBAP) and RTU over TCP.
var framings = []struct {
	name string
	mbap bool
}{
	{"tcp", true},
	{"rtuovertcp", false},
}

// waitAccepted yields until the server accepted n connections; guard bounds
// the wait. The client's Open returns once the handshake is done, possibly
// before the server's accept loop counted the connection.
func waitAccepted(t *testing.T, srv *rtuServer, n int32) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for srv.accepted.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("connections accepted = %d, want %d", srv.accepted.Load(), n)
		}
		runtime.Gosched()
	}
}

// TestSilentUnitDoesNotStarveTheOthers (ruling R13): on one connection, a unit
// that never answers only times out its own blocks. The other unit keeps
// being read at its interval and stays alive, the silent one dies after the
// death tolerance, and the connection is kept: the other unit answers on it.
func TestSilentUnitDoesNotStarveTheOthers(t *testing.T) {
	const interval = 500 * time.Millisecond

	for _, f := range framings {
		t.Run(f.name, func(t *testing.T) {
			dev := newTestDevice()
			dev.set(func(d *testDevice) { d.holding[0] = 1; d.holding[5] = 55 })
			srv := startRawServer(t, dev, f.mbap)
			srv.silence(1, true)

			h := newHarness(t, modbusInput(srv.connection(`"timeout":"50ms"`), 2*time.Second,
				config.Device{Device: "dead", Points: []config.Point{pollPoint("a", "hr:0@1", "int16", interval)}},
				config.Device{Device: "alive", Points: []config.Point{pollPoint("b", "hr:5@2", "int16", interval)}},
			))
			h.start()

			var evs []string
			polls := 0
			for step := 0; step <= 8; step++ {
				if step > 0 {
					h.step(interval)
				}
				polls++
				records := h.take()
				evs = append(evs, events(records)...)
				want := []reading{{"alive", "b", model.IntValue(55), model.Good, h.clk.Now()}}
				if got := readings(t, records); !slices.Equal(got, want) {
					t.Fatalf("at %v: samples %+v, want %+v", h.clk.Now().Sub(epoch), got, want)
				}
				if !h.in.Health().Connected {
					t.Fatalf("at %v: not connected while a unit answers", h.clk.Now().Sub(epoch))
				}
			}

			if !slices.Equal(evs, []string{"death dead"}) {
				t.Errorf("events = %v, want only the death of the silent unit", evs)
			}
			health := h.in.Health()
			if health.ReadErrors[input.ReadErrorTimeout] != uint64(polls) || health.ReadErrors[input.ReadErrorConnection] != 0 {
				t.Errorf("ReadErrors = %v, want %d timeouts and no connection error", health.ReadErrors, polls)
			}
			if n := srv.accepted.Load(); n != 1 {
				t.Errorf("connections = %d, want 1 (kept)", n)
			}
			if n := len(dev.served()); n != polls {
				t.Errorf("requests answered = %d, want %d (one per interval for the live unit)", n, polls)
			}
		})
	}
}

// TestAllUnitsSilentReconnectsAfterTheDeathTolerance (ruling R13): when no
// unit has answered on the connection for the death tolerance (a half-open
// socket, a dead gateway), the connection is closed and reopened after the
// backoff; the data comes back on the new connection.
func TestAllUnitsSilentReconnectsAfterTheDeathTolerance(t *testing.T) {
	const interval = 500 * time.Millisecond

	for _, f := range framings {
		t.Run(f.name, func(t *testing.T) {
			dev := newTestDevice()
			dev.set(func(d *testDevice) { d.holding[0] = 11 })
			srv := startRawServer(t, dev, f.mbap)
			srv.silence(1, true)

			h := newHarness(t, modbusInput(srv.connection(`"timeout":"50ms"`), 2*time.Second, config.Device{
				Device: "press-07",
				Points: []config.Point{pollPoint("speed", "hr:0", "int16", interval)},
			}))
			h.start()
			waitAccepted(t, srv, 1)

			// Timeouts at 0, 0.5, 1 and 1.5 s keep the connection.
			for step := 0; step <= 3; step++ {
				if step > 0 {
					h.step(interval)
				}
				if got := h.take(); len(got) != 0 {
					t.Fatalf("at %v: records %+v", h.clk.Now().Sub(epoch), got)
				}
				if !h.in.Health().Connected {
					t.Fatalf("at %v: a timeout closed the connection before the death tolerance", h.clk.Now().Sub(epoch))
				}
			}
			if n := srv.accepted.Load(); n != 1 {
				t.Fatalf("connections before the death tolerance = %d, want 1", n)
			}

			// At 2 s nothing answered for the tolerance: death, connection closed.
			h.step(interval)
			if got := events(h.take()); !slices.Equal(got, []string{"death press-07"}) {
				t.Fatalf("events at the death tolerance = %v, want one death", got)
			}
			if h.in.Health().Connected {
				t.Fatal("connected after the death tolerance without any answer")
			}

			// The backoff (0.5 s with no jitter) then reopens the connection.
			h.step(interval)
			waitAccepted(t, srv, 2)
			if !h.in.Health().Connected {
				t.Fatal("not connected after the backoff")
			}

			srv.silence(1, false)
			h.step(interval)
			records := h.take()
			if got := events(records); !slices.Equal(got, []string{"birth press-07"}) {
				t.Fatalf("events once the unit answers = %v, want a birth", got)
			}
			if got := readings(t, records); len(got) != 1 || got[0].value != model.IntValue(11) {
				t.Fatalf("samples once the unit answers = %+v", got)
			}
			health := h.in.Health()
			if !health.Connected || health.ReadErrors[input.ReadErrorTimeout] != 6 || health.ReadErrors[input.ReadErrorConnection] != 0 {
				t.Errorf("Health = %+v, want connected after 6 timeouts and no connection error", health)
			}
			if n := srv.accepted.Load(); n != 2 {
				t.Errorf("connections = %d, want 2 (one reopen)", n)
			}
		})
	}
}

// TestTimingOutBlockDoesNotStarveItsUnit (ruling R13): a block of a unit that
// times out at every poll does not keep another block of the same unit from
// being read at its interval.
func TestTimingOutBlockDoesNotStarveItsUnit(t *testing.T) {
	const interval = 500 * time.Millisecond

	link := newFakeLink()
	link.holding[10] = 7
	link.errsAt[0] = mbus.ErrRequestTimedOut
	h := newHarness(t, modbusInput([]byte(`{"transport":"tcp","host":"plc"}`), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{
			pollPoint("stuck", "hr:0", "int16", interval),
			pollPoint("speed", "hr:10", "int16", interval),
		},
	}))
	h.in.newClient = link.newClient
	h.start()

	polls := 0
	for step := 0; step <= 6; step++ {
		if step > 0 {
			h.step(interval)
		}
		polls++
		want := []reading{{"press-07", "speed", model.IntValue(7), model.Good, h.clk.Now()}}
		if got := readings(t, h.take()); !slices.Equal(got, want) {
			t.Fatalf("at %v: samples %+v, want %+v", h.clk.Now().Sub(epoch), got, want)
		}
	}

	var wantServed []uint16
	for range polls {
		wantServed = append(wantServed, 0, 10)
	}
	if got := link.servedAddrs(); !slices.Equal(got, wantServed) {
		t.Errorf("reads %v, want %v", got, wantServed)
	}
	health := h.in.Health()
	if !health.Connected || health.ReadErrors[input.ReadErrorTimeout] != uint64(polls) || len(health.ReadErrors) != 1 {
		t.Errorf("Health = %+v, want connected with %d timeouts only", health, polls)
	}
	if n := len(link.built()); n != 1 {
		t.Errorf("clients built = %d, want 1 (the connection is kept)", n)
	}
}

// TestBrokenBlockDoesNotStarveTheOthers: a block whose request breaks the
// connection at every poll is read again only after the blocks that waited
// longer, so after each reconnection the other block is read first. The
// backoff, reset by each answer, spaces those reads.
func TestBrokenBlockDoesNotStarveTheOthers(t *testing.T) {
	const interval = 500 * time.Millisecond

	link := newFakeLink()
	link.holding[10] = 7
	link.errsAt[0] = io.ErrUnexpectedEOF
	h := newHarness(t, modbusInput([]byte(`{"transport":"tcp","host":"plc"}`), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{
			pollPoint("broken", "hr:0", "int16", interval),
			pollPoint("speed", "hr:10", "int16", interval),
		},
	}))
	h.in.newClient = link.newClient
	h.start()

	var got []time.Duration
	for range 8 {
		h.step(interval)
		for _, r := range readings(t, h.take()) {
			got = append(got, r.ts.Sub(epoch))
		}
	}
	// Broken at 0, speed at 0.5 s after a 0.5 s backoff, broken again, a
	// 1 s backoff, then speed first at 2 s, and so on.
	want := []time.Duration{500 * time.Millisecond, 2 * time.Second, 3500 * time.Millisecond}
	if !slices.Equal(got, want) {
		t.Errorf("speed read at %v, want %v (reads %v)", got, want, link.servedAddrs())
	}
}

// TestUnreadableFrameReopensOnlyATCPConnection (ruling R19): on tcp a
// protocol error or a short frame can leave the MBAP stream out of step, and
// the library does not resynchronise it, so the connection is closed and
// reopened after the backoff. On rtu and rtuovertcp the library flushes the
// line after these errors and the connection is kept (ruling R13), as it is
// on tcp for an answer of another unit, read whole.
func TestUnreadableFrameReopensOnlyATCPConnection(t *testing.T) {
	tests := []struct {
		name       string
		connection string
		err        error
		reopened   bool
	}{
		{"tcp protocol error", `{"transport":"tcp","host":"plc"}`, mbus.ErrProtocolError, true},
		{"tcp short frame", `{"transport":"tcp","host":"plc"}`, mbus.ErrShortFrame, true},
		{"tcp another unit", `{"transport":"tcp","host":"plc"}`, mbus.ErrBadUnitId, false},
		{"rtuovertcp protocol error", `{"transport":"rtuovertcp","host":"gw"}`, mbus.ErrProtocolError, false},
		{"rtuovertcp short frame", `{"transport":"rtuovertcp","host":"gw"}`, mbus.ErrShortFrame, false},
		{"rtu protocol error", `{"transport":"rtu","device":"/dev/ttyUSB0","baud":9600}`, mbus.ErrProtocolError, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link := newFakeLink()
			link.holding[0] = 11
			link.readErrs = []error{tt.err}
			h := newHarness(t, modbusInput([]byte(tt.connection), 30*time.Second, config.Device{
				Device: "press-07",
				Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
			}))
			h.in.newClient = link.newClient
			h.start()

			// The first read fails: no record, one connection error. A
			// reopened connection waits the 0.5 s backoff, so it is down.
			if got := h.take(); len(got) != 0 {
				t.Fatalf("records after the unreadable frame: %+v", got)
			}
			health := h.in.Health()
			if health.Connected == tt.reopened || health.ReadErrors[input.ReadErrorConnection] != 1 || len(health.ReadErrors) != 1 {
				t.Fatalf("Health = %+v, want connected %v with one connection error", health, !tt.reopened)
			}
			clients := link.built()
			if _, closes := clients[0].counts(); (closes == 1) != tt.reopened {
				t.Errorf("first client closed %d times, reopened %v", closes, tt.reopened)
			}

			// The next poll reads the value, on a new connection or the same.
			h.step(time.Second)
			want := []reading{{"press-07", "speed", model.IntValue(11), model.Good, h.clk.Now()}}
			if got := readings(t, h.take()); !slices.Equal(got, want) {
				t.Fatalf("samples at the next poll = %+v, want %+v", got, want)
			}
			wantBuilt := 1
			if tt.reopened {
				wantBuilt = 2
			}
			if n := len(link.built()); n != wantBuilt {
				t.Errorf("clients built = %d, want %d", n, wantBuilt)
			}
			if !h.in.Health().Connected {
				t.Error("not connected at the next poll")
			}
		})
	}
}

// TestUnknownExceptionCodeIsADeviceAnswer (ruling R15): an exception code the
// library does not know is a device answer like exceptions 5 and 6: no
// sample, the device is read (never reported dead), kind exception, and the
// connection is kept.
func TestUnknownExceptionCodeIsADeviceAnswer(t *testing.T) {
	link := newFakeLink()
	// The library's own text for an exception code it does not map
	// (simonvetter/modbus v1.6.4, modbus.go:88).
	link.errsAt[0] = fmt.Errorf("unknown exception code (%v)", 7)
	h := newHarness(t, modbusInput([]byte(`{"transport":"tcp","host":"plc"}`), 3*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}))
	h.in.newClient = link.newClient
	h.start()

	var records []model.Record
	records = append(records, h.take()...)
	for range 5 {
		h.step(time.Second)
		records = append(records, h.take()...)
	}
	if len(records) != 0 {
		t.Errorf("records = %+v, want none (no sample, never dead)", records)
	}
	health := h.in.Health()
	if !health.Connected || health.ReadErrors[input.ReadErrorException] != 6 || len(health.ReadErrors) != 1 {
		t.Errorf("Health = %+v, want connected with 6 exceptions only", health)
	}
	if n := len(link.built()); n != 1 {
		t.Errorf("clients built = %d, want 1 (the connection is kept)", n)
	}
}

// TestClassifyKeepsTheConnectionUnlessItFailed (rulings R13, R15, R19): only
// an I/O error, or on tcp a protocol error or a short frame, is a broken
// connection; a timeout or another answer the library rejects keeps it, and
// an exception code the library does not know is a device answer.
func TestClassifyKeepsTheConnectionUnlessItFailed(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transport string
		want      outcome
	}{
		{"timeout", mbus.ErrRequestTimedOut, TransportRTUOverTCP, outcomeTimeout},
		{"timeout on tcp", mbus.ErrRequestTimedOut, TransportTCP, outcomeTimeout},
		{"bad CRC", mbus.ErrBadCRC, TransportRTU, outcomeGarbled},
		{"short frame", mbus.ErrShortFrame, TransportRTUOverTCP, outcomeGarbled},
		{"short frame on rtu", mbus.ErrShortFrame, TransportRTU, outcomeGarbled},
		{"short frame on tcp", mbus.ErrShortFrame, TransportTCP, outcomeOutOfStep},
		{"protocol error", mbus.ErrProtocolError, TransportRTUOverTCP, outcomeGarbled},
		{"protocol error on rtu", mbus.ErrProtocolError, TransportRTU, outcomeGarbled},
		{"protocol error on tcp", mbus.ErrProtocolError, TransportTCP, outcomeOutOfStep},
		{"another unit", mbus.ErrBadUnitId, TransportRTUOverTCP, outcomeGarbled},
		{"another unit on tcp", mbus.ErrBadUnitId, TransportTCP, outcomeGarbled},
		{"another transaction", mbus.ErrBadTransactionId, TransportTCP, outcomeGarbled},
		{"unknown protocol", mbus.ErrUnknownProtocolId, TransportTCP, outcomeGarbled},
		{"unknown exception code", fmt.Errorf("unknown exception code (%v)", 9), TransportTCP, outcomeUnknownException},
		{"end of stream", io.EOF, TransportTCP, outcomeConnection},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, TransportRTUOverTCP, outcomeConnection},
		{"closed", net.ErrClosed, TransportRTU, outcomeConnection},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.err, tt.transport); got != tt.want {
				t.Errorf("classify(%v, %s) = %d, want %d", tt.err, tt.transport, got, tt.want)
			}
		})
	}
}
