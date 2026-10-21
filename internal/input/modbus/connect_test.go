package modbus

import (
	"errors"
	"sync"
	"testing"
	"time"

	mbus "github.com/simonvetter/modbus"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

// fakeLink stands for the transport where a real server cannot produce the
// case: a serial port, or an Open the test holds. Every client the input
// builds through newClient is a fakeClient on the same link, recorded in
// order. Only holding registers are served.
type fakeLink struct {
	mu      sync.Mutex
	holding map[uint16]uint16
	openErr error
	// readErrs are returned by the next reads, one each, before the values.
	readErrs []error
	// errsAt makes every read covering an address return its error.
	errsAt map[uint16]error
	// served lists the start address of every read that reached the link.
	served []uint16
	// gate, when set, makes Open wait until it is closed.
	gate    chan struct{}
	clients []*fakeClient

	// opening receives a value when an Open starts.
	opening chan struct{}
}

func newFakeLink() *fakeLink {
	return &fakeLink{holding: map[uint16]uint16{}, errsAt: map[uint16]error{}, opening: make(chan struct{}, 64)}
}

func (l *fakeLink) newClient(Connection) (client, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	c := &fakeClient{link: l, closedCh: make(chan struct{})}
	l.clients = append(l.clients, c)

	return c, nil
}

// servedAddrs returns the start addresses of the reads served so far.
func (l *fakeLink) servedAddrs() []uint16 {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]uint16(nil), l.served...)
}

// built returns the clients built so far.
func (l *fakeLink) built() []*fakeClient {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]*fakeClient(nil), l.clients...)
}

type fakeClient struct {
	link *fakeLink

	mu            sync.Mutex
	opens, closes int
	// closedCh is closed by the first Close.
	closedCh   chan struct{}
	closedOnce sync.Once
}

func (c *fakeClient) Open() error {
	c.link.opening <- struct{}{}

	c.link.mu.Lock()
	gate, err := c.link.gate, c.link.openErr
	c.link.mu.Unlock()
	if gate != nil {
		<-gate
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.opens++

	return err
}

func (c *fakeClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closes++
	c.closedOnce.Do(func() { close(c.closedCh) })

	return nil
}

// counts returns how many times the client was opened and closed.
func (c *fakeClient) counts() (opens, closes int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.opens, c.closes
}

func (c *fakeClient) SetUnitId(uint8) error { return nil }

func (c *fakeClient) ReadRegisters(addr, quantity uint16, regType mbus.RegType) ([]uint16, error) {
	c.link.mu.Lock()
	defer c.link.mu.Unlock()

	if len(c.link.readErrs) > 0 {
		err := c.link.readErrs[0]
		c.link.readErrs = c.link.readErrs[1:]
		return nil, err
	}
	c.link.served = append(c.link.served, addr)
	if regType != mbus.HOLDING_REGISTER {
		return nil, mbus.ErrIllegalFunction
	}
	out := make([]uint16, 0, quantity)
	for a := int(addr); a < int(addr)+int(quantity); a++ {
		if err, ok := c.link.errsAt[uint16(a)]; ok {
			return nil, err
		}
		v, ok := c.link.holding[uint16(a)]
		if !ok {
			return nil, mbus.ErrIllegalDataAddress
		}
		out = append(out, v)
	}

	return out, nil
}

func (c *fakeClient) ReadCoils(uint16, uint16) ([]bool, error) {
	return nil, mbus.ErrIllegalFunction
}

func (c *fakeClient) ReadDiscreteInputs(uint16, uint16) ([]bool, error) {
	return nil, mbus.ErrIllegalFunction
}

// TestCloseDoesNotWaitForAConnectionAttempt (ruling R10): the library's Open
// cannot be cancelled, so Close returns while it is still running, and the
// client it opens late is closed by the input, never used.
func TestCloseDoesNotWaitForAConnectionAttempt(t *testing.T) {
	link := newFakeLink()
	link.holding[0] = 11
	gate := make(chan struct{})
	link.gate = gate

	h := newHarness(t, modbusInput([]byte(`{"transport":"tcp","host":"plc"}`), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}))
	h.in.newClient = link.newClient
	if err := h.in.Start(t.Context(), h.emit); err != nil {
		t.Fatal(err)
	}
	select {
	case <-link.opening:
	case <-time.After(guard):
		t.Fatal("the input never opened a client")
	}

	done := make(chan error, 1)
	go func() { done <- h.in.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(guard):
		close(gate)
		t.Fatal("Close waited for the connection attempt")
	}
	h.closed.Store(true)

	// The attempt succeeds after Close: the client is closed, nothing read.
	close(gate)
	clients := link.built()
	if len(clients) != 1 {
		t.Fatalf("clients built = %d, want 1", len(clients))
	}
	select {
	case <-clients[0].closedCh:
	case <-time.After(guard):
		t.Fatal("the client opened after Close was left open")
	}
	if opens, closes := clients[0].counts(); opens != 1 || closes != 1 {
		t.Errorf("opens, closes = %d, %d, want 1, 1", opens, closes)
	}
	if h.in.Health().Connected {
		t.Error("connected after Close")
	}
	if got := h.take(); len(got) != 0 {
		t.Errorf("records emitted: %+v", got)
	}
	if n := h.late.Load(); n != 0 {
		t.Errorf("%d records emitted after Close returned", n)
	}
}

// TestRTUStartOpensTheSerialPort (ruling R2): for rtu, Start opens the port
// itself and the poll goroutine reads through that client; a later failure
// of the port is a connection error, reopened in the background with the
// backoff like a TCP connection.
func TestRTUStartOpensTheSerialPort(t *testing.T) {
	link := newFakeLink()
	link.holding[0] = 11
	h := newHarness(t, modbusInput([]byte(`{"transport":"rtu","device":"/dev/ttyUSB0","baud":9600}`), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}))
	h.in.newClient = link.newClient

	if err := h.in.Start(t.Context(), h.emit); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.close)
	// Synchronous: the port is open when Start returns.
	clients := link.built()
	if len(clients) != 1 {
		t.Fatalf("clients built by Start = %d, want 1", len(clients))
	}
	if opens, _ := clients[0].counts(); opens != 1 {
		t.Fatalf("Start opened the port %d times, want 1", opens)
	}
	if !h.in.Health().Connected {
		t.Error("not connected once Start opened the port")
	}

	h.idle()
	if got := readings(t, h.take()); len(got) != 1 || got[0].value != model.IntValue(11) {
		t.Fatalf("samples = %+v, want speed 11", got)
	}
	if n := len(link.built()); n != 1 {
		t.Fatalf("clients built = %d, want the one Start opened", n)
	}

	// The port fails: closed, counted, reopened after the backoff.
	link.mu.Lock()
	link.readErrs = []error{errors.New("input/output error")}
	link.holding[0] = 12
	link.mu.Unlock()
	h.step(time.Second)
	if got := h.take(); len(got) != 0 {
		t.Fatalf("records after the port failed: %+v", got)
	}
	health := h.in.Health()
	if health.Connected || health.ReadErrors[input.ReadErrorConnection] != 1 {
		t.Fatalf("Health = %+v, want disconnected after one connection error", health)
	}
	if _, closes := clients[0].counts(); closes != 1 {
		t.Errorf("the failed port was closed %d times, want 1", closes)
	}

	var records []model.Record
	for range 10 {
		h.step(time.Second)
		if records = h.take(); len(records) > 0 {
			break
		}
	}
	if got := readings(t, records); len(got) != 1 || got[0].value != model.IntValue(12) {
		t.Fatalf("samples after the port reopened = %+v, want speed 12", got)
	}
	if n := len(link.built()); n != 2 {
		t.Errorf("clients built = %d, want 2 (one reopen)", n)
	}
	if !h.in.Health().Connected {
		t.Error("not connected after the port reopened")
	}
}
