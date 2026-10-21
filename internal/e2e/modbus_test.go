//go:build e2e && unix

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mbus "github.com/simonvetter/modbus"
)

// plcUnit2 is the unit the second device answers on (address override @2).
const plcUnit2 = 2

// modbusDevice is the register map behind the library's Modbus TCP server:
// unit 1 holds the first device (input registers 0-1 a float32 21.5 in byte
// order CDAB, holding register 0 an int16 -5, holding registers 10-13 the
// string "PRESS-7", coil 0 true) and unit 2 the second (holding register 0 a
// uint16 7). Any other address answers exception 2. A write function code is
// counted and refused, so the test can prove the agent sends none.
type modbusDevice struct {
	writes atomic.Int64
}

// cdab is 21.5 as float32 (0x41AC0000) in byte order CDAB: low word first.
var cdab = [2]uint16{0x0000, 0x41AC}

func (d *modbusDevice) HandleCoils(req *mbus.CoilsRequest) ([]bool, error) {
	if req.IsWrite {
		d.writes.Add(1)

		return nil, mbus.ErrIllegalFunction
	}
	if req.UnitId != 1 || req.Addr != 0 || req.Quantity != 1 {
		return nil, mbus.ErrIllegalDataAddress
	}

	return []bool{true}, nil
}

func (d *modbusDevice) HandleDiscreteInputs(*mbus.DiscreteInputsRequest) ([]bool, error) {
	return nil, mbus.ErrIllegalDataAddress
}

func (d *modbusDevice) HandleHoldingRegisters(req *mbus.HoldingRegistersRequest) ([]uint16, error) {
	if req.IsWrite {
		d.writes.Add(1)

		return nil, mbus.ErrIllegalFunction
	}

	// Holding registers 0 and 10-13 of unit 1 are two blocks: a gap is
	// never read.
	switch {
	case req.UnitId == 1 && req.Addr == 0 && req.Quantity == 1:
		return []uint16{uint16(0xFFFF - 4)}, nil // int16 -5
	case req.UnitId == 1 && req.Addr == 10 && req.Quantity == 4:
		return []uint16{'P'<<8 | 'R', 'E'<<8 | 'S', 'S'<<8 | '-', '7' << 8}, nil
	case req.UnitId == plcUnit2 && req.Addr == 0 && req.Quantity == 1:
		return []uint16{7}, nil
	}

	return nil, mbus.ErrIllegalDataAddress
}

func (d *modbusDevice) HandleInputRegisters(req *mbus.InputRegistersRequest) ([]uint16, error) {
	if req.UnitId != 1 || req.Addr != 0 || req.Quantity != 2 {
		return nil, mbus.ErrIllegalDataAddress
	}

	return []uint16{cdab[0], cdab[1]}, nil
}

// modbusServer is the library's Modbus TCP server on a loopback port that
// can be stopped and started again on the same port.
type modbusServer struct {
	t    *testing.T
	dev  *modbusDevice
	port int

	mu  sync.Mutex
	srv *mbus.ModbusServer
}

func startModbusServer(t *testing.T, dev *modbusDevice) *modbusServer {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	s := &modbusServer{t: t, dev: dev, port: port}
	s.start()
	t.Cleanup(s.stop)

	return s
}

func (s *modbusServer) start() {
	s.t.Helper()

	srv, err := mbus.NewServer(&mbus.ServerConfiguration{
		URL:        "tcp://127.0.0.1:" + strconv.Itoa(s.port),
		MaxClients: 10,
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
func (s *modbusServer) stop() {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.mu.Unlock()

	if srv != nil {
		_ = srv.Stop()
	}
}

// modbusPoints are the points of the two devices: plc-1 on the input's unit 1
// and plc-2 on unit 2 through the address override. temp_x10 reads the same
// registers as temp, with a transform chain.
var modbusPoints = map[string][]map[string]any{
	"plc-1": {
		{"signal": "temp", "address": "ir:0", "type": "float32", "byte_order": "CDAB", "unit": "C", "read": map[string]any{"poll": "100ms"}},
		{"signal": "temp_x10", "address": "ir:0", "type": "float32", "byte_order": "CDAB", "read": map[string]any{"poll": "100ms"},
			"transforms": []any{map[string]any{"scale": 10}}},
		{"signal": "delta", "address": "hr:0", "type": "int16", "read": map[string]any{"poll": "100ms"}},
		{"signal": "label", "address": "hr:10#4", "type": "string", "read": map[string]any{"poll": "100ms"}},
		{"signal": "running", "address": "co:0", "type": "bool", "read": map[string]any{"poll": "100ms"}},
	},
	"plc-2": {
		{"signal": "counter", "address": "hr:0@" + strconv.Itoa(plcUnit2), "type": "uint16", "read": map[string]any{"poll": "100ms"}},
	},
}

// modbusDevices lists the devices in a fixed order.
var modbusDevices = []string{"plc-1", "plc-2"}

// writeModbusSetup writes the file provider setup of a modbus input reaching
// port, with a death tolerance of 3 seconds, into dir, and returns the
// bootstrap path.
func writeModbusSetup(t *testing.T, dir, url string, port int) string {
	t.Helper()

	var devices []any
	for _, d := range modbusDevices {
		devices = append(devices, map[string]any{"device": d, "points": modbusPoints[d]})
	}
	doc := marshal(t, map[string]any{
		"schema":  "laraplate-edge-config/1",
		"version": 1,
		"inputs": []any{map[string]any{
			"id":              "plc",
			"driver":          "modbus",
			"death_tolerance": "3s",
			"connection": map[string]any{
				"transport": "tcp", "host": "127.0.0.1", "port": port, "timeout": "500ms",
			},
			"devices": devices,
		}},
		"outputs": []any{map[string]any{
			"id": "laraplate", "type": "laraplate-http", "url": url, "token": "secret://env/" + tokenEnv,
			"batch": map[string]any{"max_samples": 40, "max_wait": "200ms"},
		}},
	})
	// JSON is YAML for the file provider.
	writeFile(t, filepath.Join(dir, "config.json"), string(doc))

	boot := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf("agent_id: e2e-modbus\nconfig:\n  provider: file\n  file: %s\ndata_dir: %s\nstatus_listen: 127.0.0.1:0\n",
		filepath.Join(dir, "config.json"), filepath.Join(dir, "data")))

	return boot
}

// busEvent is one thing the backend accepted, in delivery order.
type busEvent struct {
	device string
	// kind is "birth", "death" or "sample".
	kind    string
	signal  string
	value   json.RawMessage
	quality string
	// signals are the announced signals of a birth.
	signals []birthSignal
}

// events returns what the backend accepted, in source_seq order and in the
// order of the devices entries of each envelope, up to the first gap.
func (b *backend) events() []busEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []busEvent
	for seq := uint64(0); ; seq++ {
		env, ok := b.accepted[seq]
		if !ok {
			break
		}
		for _, dev := range env.Devices {
			switch dev.Type {
			case "birth", "death":
				out = append(out, busEvent{device: dev.Device, kind: dev.Type, signals: dev.Signals})
			}
			for _, s := range dev.Samples {
				out = append(out, busEvent{device: dev.Device, kind: "sample", signal: s.Signal, value: s.Value, quality: s.Quality})
			}
		}
	}

	return out
}

// countEvents returns how many events of kind the device has.
func countEvents(events []busEvent, device, kind string) int {
	n := 0
	for _, e := range events {
		if e.device == device && e.kind == kind {
			n++
		}
	}

	return n
}

// noViolations fails the test for every violation the backend recorded: an
// envelope that fails the backend schema above all.
func (b *backend) noViolations(t *testing.T) {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	for _, v := range b.violations {
		t.Error(v)
	}
	if len(b.accepted) == 0 {
		t.Error("the backend accepted nothing")
	}
}

// modbusInputStatus is the modbus input as /status shows it.
type modbusInputStatus struct {
	Connected  *bool             `json:"connected"`
	ReadErrors map[string]uint64 `json:"read_errors"`
}

// inputStatus reads the input "plc" from /status.
func inputStatus(t *testing.T, addr string) modbusInputStatus {
	t.Helper()

	code, body := getStatus(t, addr, "/status")
	var st struct {
		Inputs []struct {
			ID string `json:"id"`
			modbusInputStatus
		} `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
		t.Fatalf("/status = %d %q (%v), want 200 JSON", code, body, err)
	}
	for _, in := range st.Inputs {
		if in.ID == "plc" {
			return in.modbusInputStatus
		}
	}
	t.Fatalf("/status lacks the input plc: %s", body)

	return modbusInputStatus{}
}

// waitConnected waits until /status reports the input connected or not.
func waitConnected(t *testing.T, b *backend, a *agent, addr string, want bool) modbusInputStatus {
	t.Helper()

	var st modbusInputStatus
	b.waitFor(t, a, fmt.Sprintf("/status connected %v", want), func() bool {
		st = inputStatus(t, addr)

		return st.Connected != nil && *st.Connected == want
	})

	return st
}

// TestModbus: the built binary polls the library's Modbus TCP server. The
// backend receives the decoded values after the births of both devices, with
// envelopes valid against the backend schema; the server sends no write. When
// the server stops, the input reports its devices dead after the death
// tolerance, /status and the metrics show the connection down, and no sample
// arrives; when the server starts again the devices are born again and the
// data resumes.
func TestModbus(t *testing.T) {
	b := newBackend(t)
	dev := &modbusDevice{}
	srv := startModbusServer(t, dev)
	dir := t.TempDir()
	boot := writeModbusSetup(t, dir, b.url(), srv.port)

	a := startAgent(t, boot)

	// Every signal of both devices arrives, after the birth.
	b.waitFor(t, a, "a good sample of every signal", func() bool {
		events := b.events()
		for _, d := range modbusDevices {
			for _, p := range modbusPoints[d] {
				if !slices.ContainsFunc(events, func(e busEvent) bool {
					return e.device == d && e.kind == "sample" && e.signal == p["signal"]
				}) {
					return false
				}
			}
		}

		return true
	})
	addr := a.statusAddr(t)
	waitConnected(t, b, a, addr, true)

	events := b.events()
	checkModbusBirths(t, events)
	checkModbusValues(t, events)
	for _, d := range modbusDevices {
		if n := countEvents(events, d, "death"); n != 0 {
			t.Errorf("%s: %d deaths before the server stopped, want 0", d, n)
		}
	}

	// The server stops: death after the tolerance, connection down.
	srv.stop()
	b.waitFor(t, a, "both devices dead", func() bool {
		events := b.events()
		for _, d := range modbusDevices {
			if countEvents(events, d, "death") == 0 {
				return false
			}
		}

		return true
	})
	down := waitConnected(t, b, a, addr, false)
	if down.ReadErrors["connection"] == 0 {
		t.Errorf("/status read_errors = %v, want a connection error", down.ReadErrors)
	}
	code, body := getStatus(t, addr, "/metrics")
	for _, want := range []string{
		`laraplate_edge_input_connected{input="plc"} 0` + "\n",
		`laraplate_edge_input_read_errors_total{input="plc",kind="timeout"} `,
		`laraplate_edge_input_read_errors_total{input="plc",kind="exception"} 0` + "\n",
		`laraplate_edge_input_read_errors_total{input="plc",kind="decode"} 0` + "\n",
	} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("/metrics = %d, lacks %q:\n%s", code, want, body)
		}
	}
	if code != http.StatusOK || !strings.Contains(body, `laraplate_edge_input_read_errors_total{input="plc",kind="connection"} `) ||
		strings.Contains(body, `laraplate_edge_input_read_errors_total{input="plc",kind="connection"} 0`+"\n") {
		t.Errorf("/metrics = %d, want a connection error count above 0:\n%s", code, body)
	}

	// The server starts again on the same port: birth and data again.
	dead := b.events()
	srv.start()
	b.waitFor(t, a, "both devices born again with data", func() bool {
		events := b.events()
		for _, d := range modbusDevices {
			if countEvents(events, d, "birth") < 2 || countEvents(events, d, "sample") <= countEvents(dead, d, "sample") {
				return false
			}
		}

		return true
	})
	waitConnected(t, b, a, addr, true)
	code, body = getStatus(t, addr, "/metrics")
	if want := `laraplate_edge_input_connected{input="plc"} 1` + "\n"; code != http.StatusOK || !strings.Contains(body, want) {
		t.Errorf("/metrics = %d, lacks %q:\n%s", code, want, body)
	}

	a.stop(t)

	events = b.events()
	checkModbusOutage(t, events)
	checkModbusValues(t, events)
	b.noViolations(t)
	if n := dev.writes.Load(); n != 0 {
		t.Errorf("the device received %d write requests, want 0 (the driver is read-only)", n)
	}
	a.checkOutput(t, false)
	if out := a.stdout.String() + a.stderr.String(); strings.Contains(out, "127.0.0.1:"+strconv.Itoa(srv.port)) || strings.Contains(out, "PRESS-7") {
		t.Errorf("the agent output holds a connection setting or a sample value:\n%s", out)
	}
}

// checkModbusBirths: each device has a birth, before any of its samples,
// announcing every signal with its data type and unit.
func checkModbusBirths(t *testing.T, events []busEvent) {
	t.Helper()

	want := map[string]map[string]string{
		"plc-1": {"temp": "number", "temp_x10": "number", "delta": "number", "label": "string", "running": "boolean"},
		"plc-2": {"counter": "number"},
	}
	for _, d := range modbusDevices {
		births := 0
		for _, e := range events {
			if e.device != d {
				continue
			}
			if e.kind == "sample" && births == 0 {
				t.Errorf("%s: a sample of %s before the birth", d, e.signal)

				break
			}
			if e.kind != "birth" {
				continue
			}
			births++
			got := map[string]string{}
			for _, s := range e.signals {
				got[s.Signal] = s.DataType
				if s.Signal == "temp" && (s.Unit == nil || *s.Unit != "C") {
					t.Errorf("%s: birth announces temp with unit %v, want C", d, s.Unit)
				}
			}
			if !slices.Equal(sortedPairs(got), sortedPairs(want[d])) {
				t.Errorf("%s: birth announces %v, want %v", d, got, want[d])
			}
		}
		if births == 0 {
			t.Errorf("%s: no birth", d)
		}
	}
}

func sortedPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)

	return out
}

// checkModbusValues: every sample is good and has the value the server holds.
func checkModbusValues(t *testing.T, events []busEvent) {
	t.Helper()

	number := func(want float64) func(json.RawMessage) bool {
		return func(v json.RawMessage) bool {
			f, err := strconv.ParseFloat(string(v), 64)

			return err == nil && math.Abs(f-want) < 1e-9
		}
	}
	exact := func(want string) func(json.RawMessage) bool {
		return func(v json.RawMessage) bool { return string(v) == want }
	}
	want := map[string]func(json.RawMessage) bool{
		"plc-1/temp":     number(21.5),
		"plc-1/temp_x10": number(215),
		"plc-1/delta":    number(-5),
		"plc-1/label":    exact(`"PRESS-7"`),
		"plc-1/running":  exact("true"),
		"plc-2/counter":  number(7),
	}
	for _, e := range events {
		if e.kind != "sample" {
			continue
		}
		check, ok := want[e.device+"/"+e.signal]
		if !ok {
			t.Errorf("unexpected sample %s/%s", e.device, e.signal)

			continue
		}
		if e.quality != "good" || !check(e.value) {
			t.Errorf("%s/%s = %s (%s), want the server's value (good)", e.device, e.signal, e.value, e.quality)
		}
	}
}

// checkModbusOutage: between a death of a device and its next birth no
// sample of it arrives, a device dies only after a birth, and each death is
// followed by a birth.
func checkModbusOutage(t *testing.T, events []busEvent) {
	t.Helper()

	for _, d := range modbusDevices {
		alive := false
		deaths := 0
		for _, e := range events {
			if e.device != d {
				continue
			}
			switch e.kind {
			case "birth":
				if alive {
					t.Errorf("%s: a birth while alive", d)
				}
				alive = true
			case "death":
				if !alive {
					t.Errorf("%s: a death while dead", d)
				}
				alive = false
				deaths++
			case "sample":
				if !alive {
					t.Errorf("%s: sample of %s while dead", d, e.signal)
				}
			}
		}
		if deaths == 0 || !alive {
			t.Errorf("%s: %d deaths and alive %v at the end, want a death and a birth after it", d, deaths, alive)
		}
	}
}
