package modbus

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	mbus "github.com/simonvetter/modbus"
)

// rtuServer serves a testDevice with RTU framing (unit, function, data,
// CRC-16/Modbus) over TCP on a loopback port, as a serial-to-Ethernet gateway
// does (decision M6): the library's server speaks Modbus TCP only. It serves
// the four read functions; any other function code counts as a write and
// ends the connection, since the server cannot frame it. Held requests of
// the device are not supported: stop waits for the connections.
type rtuServer struct {
	t    *testing.T
	dev  *testDevice
	l    net.Listener
	host string
	port uint16

	// badCRC corrupts the CRC of every answer while it is set.
	badCRC atomic.Bool
	// accepted counts the connections the server accepted.
	accepted atomic.Int32

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// startRTUServer starts an RTU over TCP server for dev on a free loopback
// port; it is stopped when the test ends.
func startRTUServer(t *testing.T, dev *testDevice) *rtuServer {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &rtuServer{
		t:     t,
		dev:   dev,
		l:     l,
		host:  "127.0.0.1",
		port:  uint16(l.Addr().(*net.TCPAddr).Port),
		conns: map[net.Conn]struct{}{},
	}
	s.wg.Go(s.accept)
	t.Cleanup(s.stop)

	return s
}

func (s *rtuServer) accept() {
	for {
		conn, err := s.l.Accept()
		if err != nil {
			return
		}
		s.accepted.Add(1)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.wg.Go(func() { s.serve(conn) })
		s.mu.Unlock()
	}
}

// serve answers the requests of one connection until it is closed.
func (s *rtuServer) serve(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	// A read request is unit, function, address, quantity and CRC.
	frame := make([]byte, 8)
	for {
		if _, err := io.ReadFull(conn, frame); err != nil {
			return
		}
		if crc16(frame[:6]) != binary.LittleEndian.Uint16(frame[6:]) {
			// A device stays silent on a corrupted request.
			continue
		}

		unit, function := frame[0], frame[1]
		addr, quantity := binary.BigEndian.Uint16(frame[2:]), binary.BigEndian.Uint16(frame[4:])
		data, err := s.read(unit, function, addr, quantity)
		if errors.Is(err, errNotARead) {
			s.dev.set(func(d *testDevice) { d.writes++ })
			return
		}

		answer := []byte{unit, function}
		if err != nil {
			answer = []byte{unit, function | 0x80, exceptionCode(err)}
		} else {
			answer = append(append(answer, byte(len(data))), data...)
		}
		answer = binary.LittleEndian.AppendUint16(answer, crc16(answer))
		if s.badCRC.Load() {
			answer[len(answer)-1] ^= 0xFF
		}
		if _, err := conn.Write(answer); err != nil {
			return
		}
	}
}

var errNotARead = errors.New("not a read function")

// read serves a read request through the device's handlers and returns the
// data bytes of the answer: big-endian registers, or bits packed from the
// least significant bit of the first byte.
func (s *rtuServer) read(unit, function uint8, addr, quantity uint16) ([]byte, error) {
	switch function {
	case 0x01, 0x02:
		var bits []bool
		var err error
		if function == 0x01 {
			bits, err = s.dev.HandleCoils(&mbus.CoilsRequest{UnitId: unit, Addr: addr, Quantity: quantity})
		} else {
			bits, err = s.dev.HandleDiscreteInputs(&mbus.DiscreteInputsRequest{UnitId: unit, Addr: addr, Quantity: quantity})
		}
		if err != nil {
			return nil, err
		}
		data := make([]byte, (len(bits)+7)/8)
		for i, bit := range bits {
			if bit {
				data[i/8] |= 1 << (i % 8)
			}
		}
		return data, nil
	case 0x03, 0x04:
		var regs []uint16
		var err error
		if function == 0x03 {
			regs, err = s.dev.HandleHoldingRegisters(&mbus.HoldingRegistersRequest{UnitId: unit, Addr: addr, Quantity: quantity})
		} else {
			regs, err = s.dev.HandleInputRegisters(&mbus.InputRegistersRequest{UnitId: unit, Addr: addr, Quantity: quantity})
		}
		if err != nil {
			return nil, err
		}
		var data []byte
		for _, r := range regs {
			data = binary.BigEndian.AppendUint16(data, r)
		}
		return data, nil
	default:
		return nil, errNotARead
	}
}

// exceptionCode is the Modbus exception code of a library error.
func exceptionCode(err error) byte {
	codes := map[error]byte{
		mbus.ErrIllegalFunction:         1,
		mbus.ErrIllegalDataAddress:      2,
		mbus.ErrIllegalDataValue:        3,
		mbus.ErrServerDeviceFailure:     4,
		mbus.ErrAcknowledge:             5,
		mbus.ErrServerDeviceBusy:        6,
		mbus.ErrGWPathUnavailable:       10,
		mbus.ErrGWTargetFailedToRespond: 11,
	}
	if code, ok := codes[err]; ok {
		return code
	}

	return 4
}

// crc16 is the CRC-16/Modbus of b (polynomial 0xA001 reflected, initial
// value 0xFFFF), sent low byte first.
func crc16(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, c := range b {
		crc ^= uint16(c)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}

	return crc
}

// stop closes the listener and every connection and waits for them.
func (s *rtuServer) stop() {
	_ = s.l.Close()
	s.mu.Lock()
	s.closed = true
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// connection is the JSON connection of an rtuovertcp input reaching the
// server.
func (s *rtuServer) connection(extra string) []byte {
	conn := `{"transport":"rtuovertcp","host":"` + s.host + `","port":` + strconv.Itoa(int(s.port))
	if extra != "" {
		conn += "," + extra
	}

	return []byte(conn + "}")
}

// TestCRC16 checks the test server's CRC against the reference frame of the
// Modbus over serial line specification (read 10 holding registers of unit
// 1: CRC bytes C5 CD on the wire).
func TestCRC16(t *testing.T) {
	if got := crc16([]byte{0x01, 0x03, 0x00, 0x00, 0x00, 0x0A}); got != 0xCDC5 {
		t.Errorf("crc16 = %#04x, want 0xcdc5", got)
	}
}
