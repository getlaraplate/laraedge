package modbus

import (
	"io"
	"log"
	"net"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	mbus "github.com/simonvetter/modbus"

	"github.com/getlaraplate/laraedge/internal/clock"
)

// guard bounds a wait for a goroutine or a network exchange; it is a deadlock
// guard, never a timing assertion (ruling R3).
const guard = 10 * time.Second

var epoch = time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)

// request is one read request the test device served.
type request struct {
	unit     uint8
	area     Area
	addr     uint16
	quantity uint16
}

// testDevice is the register map behind the library's Modbus TCP server
// (decision M6). An address missing from its area answers exception 2, as an
// unmapped address of a real device does; errs makes an address answer
// another error; hold makes a request covering an address wait until the
// channel is closed, so a test can keep a request in flight.
type testDevice struct {
	mu       sync.Mutex
	holding  map[uint16]uint16
	inputs   map[uint16]uint16
	coils    map[uint16]bool
	discrete map[uint16]bool
	errs     map[uint16]error
	hold     map[uint16]chan struct{}
	requests []request
	writes   int

	// arrived receives the start address of each held request.
	arrived chan uint16
}

func newTestDevice() *testDevice {
	return &testDevice{
		holding:  map[uint16]uint16{},
		inputs:   map[uint16]uint16{},
		coils:    map[uint16]bool{},
		discrete: map[uint16]bool{},
		errs:     map[uint16]error{},
		hold:     map[uint16]chan struct{}{},
		arrived:  make(chan uint16, 64),
	}
}

// set runs f with the device locked, to change its registers or errors.
func (d *testDevice) set(f func(d *testDevice)) {
	d.mu.Lock()
	defer d.mu.Unlock()

	f(d)
}

// holdAt makes the requests covering addr wait until release is called.
func (d *testDevice) holdAt(addr uint16) (release func()) {
	ch := make(chan struct{})
	d.set(func(d *testDevice) { d.hold[addr] = ch })

	var once sync.Once

	return func() {
		once.Do(func() {
			d.set(func(d *testDevice) { delete(d.hold, addr) })
			close(ch)
		})
	}
}

// served returns a copy of the requests served so far.
func (d *testDevice) served() []request {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]request(nil), d.requests...)
}

func (d *testDevice) writeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.writes
}

func (d *testDevice) HandleCoils(req *mbus.CoilsRequest) ([]bool, error) {
	if req.IsWrite {
		d.set(func(d *testDevice) { d.writes++ })
		return nil, mbus.ErrIllegalFunction
	}

	return serve(d, AreaCoil, req.UnitId, req.Addr, req.Quantity, func(d *testDevice) map[uint16]bool { return d.coils })
}

func (d *testDevice) HandleDiscreteInputs(req *mbus.DiscreteInputsRequest) ([]bool, error) {
	return serve(d, AreaDiscreteInput, req.UnitId, req.Addr, req.Quantity, func(d *testDevice) map[uint16]bool { return d.discrete })
}

func (d *testDevice) HandleHoldingRegisters(req *mbus.HoldingRegistersRequest) ([]uint16, error) {
	if req.IsWrite {
		d.set(func(d *testDevice) { d.writes++ })
		return nil, mbus.ErrIllegalFunction
	}

	return serve(d, AreaHoldingRegister, req.UnitId, req.Addr, req.Quantity, func(d *testDevice) map[uint16]uint16 { return d.holding })
}

func (d *testDevice) HandleInputRegisters(req *mbus.InputRegistersRequest) ([]uint16, error) {
	return serve(d, AreaInputRegister, req.UnitId, req.Addr, req.Quantity, func(d *testDevice) map[uint16]uint16 { return d.inputs })
}

// serve records the request, waits while it is held, then answers the values
// of the addresses, or the first error or unmapped address it covers.
func serve[T any](d *testDevice, area Area, unit uint8, addr, quantity uint16, values func(*testDevice) map[uint16]T) ([]T, error) {
	d.mu.Lock()
	d.requests = append(d.requests, request{unit: unit, area: area, addr: addr, quantity: quantity})
	var held chan struct{}
	for a := int(addr); a < int(addr)+int(quantity); a++ {
		if ch, ok := d.hold[uint16(a)]; ok {
			held = ch
			break
		}
	}
	d.mu.Unlock()

	if held != nil {
		select {
		case d.arrived <- addr:
		default:
		}
		<-held
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]T, 0, quantity)
	for a := int(addr); a < int(addr)+int(quantity); a++ {
		if err, ok := d.errs[uint16(a)]; ok {
			return nil, err
		}
		v, ok := values(d)[uint16(a)]
		if !ok {
			return nil, mbus.ErrIllegalDataAddress
		}
		out = append(out, v)
	}

	return out, nil
}

// testServer is the library's Modbus TCP server on a loopback port, serving
// a testDevice. It can be stopped and started again on the same port.
type testServer struct {
	t    *testing.T
	dev  *testDevice
	host string
	port uint16

	mu  sync.Mutex
	srv *mbus.ModbusServer
}

// startServer starts a server for dev on a free loopback port; it is stopped
// when the test ends.
func startServer(t *testing.T, dev *testDevice) *testServer {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	s := &testServer{t: t, dev: dev, host: "127.0.0.1", port: port}
	s.start()
	t.Cleanup(s.stop)

	return s
}

func (s *testServer) start() {
	s.t.Helper()

	srv, err := mbus.NewServer(&mbus.ServerConfiguration{
		URL:        "tcp://" + net.JoinHostPort(s.host, strconv.Itoa(int(s.port))),
		MaxClients: 100,
		Logger:     log.New(io.Discard, "", 0),
	}, s.dev)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		s.t.Fatal(err)
	}

	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()
}

// stop closes the listener and every client connection.
func (s *testServer) stop() {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.mu.Unlock()

	if srv != nil {
		_ = srv.Stop()
	}
}

// connection is the JSON connection of a tcp input reaching the server.
func (s *testServer) connection(extra string) []byte {
	conn := `{"transport":"tcp","host":"` + s.host + `","port":` + strconv.Itoa(int(s.port))
	if extra != "" {
		conn += "," + extra
	}

	return []byte(conn + "}")
}

// waitPending yields until at least n timers wait on clk, so a test knows the
// goroutines of an input are blocked on the clock; guard bounds the wait.
func waitPending(t *testing.T, clk *clock.Fake, n int) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for clk.PendingTimers() < n {
		if time.Now().After(deadline) {
			t.Fatalf("PendingTimers() = %d, want %d", clk.PendingTimers(), n)
		}
		runtime.Gosched()
	}
}
