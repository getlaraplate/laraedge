package modbus

import (
	"io"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	mbus "github.com/simonvetter/modbus"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

func TestRTUOverTCPReadsRegistersAndBits(t *testing.T) {
	dev := newTestDevice()
	level := math.Float32bits(-2.25)
	dev.set(func(d *testDevice) {
		d.holding[0] = 0xFFFE // -2 as int16
		d.holding[1] = 7
		d.inputs[10] = uint16(level >> 16)
		d.inputs[11] = uint16(level)
		d.coils[5] = true
		d.coils[6] = false
		d.coils[12] = true
		d.discrete[3] = true
	})
	srv := startRTUServer(t, dev)

	h := newHarness(t, modbusInput(srv.connection(`"timeout":"2s"`), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{
			pollPoint("temp", "hr:0", "int16", time.Second),
			pollPoint("count", "hr:1", "uint16", time.Second),
			pollPoint("level", "ir:10@2", "float32", time.Second),
			pollPoint("door", "co:5", "bool", time.Second),
			pollPoint("lock", "co:6", "bool", time.Second),
			pollPoint("alarm", "co:12", "bool", time.Second),
			pollPoint("open", "di:3", "bool", time.Second),
		},
	}))
	h.start()

	sortReadings := func(rs []reading) []reading {
		slices.SortFunc(rs, func(a, b reading) int { return strings.Compare(a.signal, b.signal) })
		return rs
	}
	want := sortReadings([]reading{
		{"press-07", "temp", model.IntValue(-2), model.Good, epoch},
		{"press-07", "count", model.IntValue(7), model.Good, epoch},
		{"press-07", "level", floatOf(t, -2.25), model.Good, epoch},
		{"press-07", "door", model.BoolValue(true), model.Good, epoch},
		{"press-07", "lock", model.BoolValue(false), model.Good, epoch},
		{"press-07", "alarm", model.BoolValue(true), model.Good, epoch},
		{"press-07", "open", model.BoolValue(true), model.Good, epoch},
	})
	if got := sortReadings(readings(t, h.take())); !slices.Equal(got, want) {
		t.Errorf("samples\n got %+v\nwant %+v", got, want)
	}

	h.step(time.Second)
	if got := readings(t, h.take()); len(got) != len(want) {
		t.Errorf("second poll: %d samples, want %d", len(got), len(want))
	}

	served := dev.served()
	wantFirst := []request{
		{1, AreaCoil, 5, 2}, {1, AreaCoil, 12, 1}, {1, AreaDiscreteInput, 3, 1},
		{1, AreaHoldingRegister, 0, 2}, {2, AreaInputRegister, 10, 2},
	}
	if len(served) != 2*len(wantFirst) || !slices.Equal(served[:len(wantFirst)], wantFirst) {
		t.Errorf("requests\n got %+v\nwant twice %+v", served, wantFirst)
	}
	if n := dev.writeCount(); n != 0 {
		t.Errorf("the driver sent %d writes", n)
	}
	if n := srv.accepted.Load(); n != 1 {
		t.Errorf("connections = %d, want 1", n)
	}
	if health := h.in.Health(); !health.Connected || len(health.ReadErrors) != 0 {
		t.Errorf("Health = %+v, want connected without errors", health)
	}
}

func TestRTUOverTCPExceptionIsADeviceAnswer(t *testing.T) {
	dev := newTestDevice()
	dev.set(func(d *testDevice) {
		d.holding[0] = 1
		// hr:1 is not mapped: exception 2 for the block.
		d.holding[10] = 9
	})
	srv := startRTUServer(t, dev)

	h := newHarness(t, modbusInput(srv.connection(""), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{
			pollPoint("total", "hr:0", "uint32", time.Second),
			pollPoint("speed", "hr:10", "int16", time.Second),
		},
	}))
	h.start()

	want := []reading{
		{"press-07", "total", model.IntValue(0), model.Bad, epoch},
		{"press-07", "speed", model.IntValue(9), model.Good, epoch},
	}
	if got := readings(t, h.take()); !slices.Equal(got, want) {
		t.Errorf("samples\n got %+v\nwant %+v", got, want)
	}
	health := h.in.Health()
	if !health.Connected || health.ReadErrors[input.ReadErrorException] != 1 || len(health.ReadErrors) != 1 {
		t.Errorf("Health = %+v, want connected with one exception", health)
	}
}

// TestRTUOverTCPBadCRCIsNeverData: an answer whose CRC does not match is a
// connection error (ruling R8): no sample, the connection is closed and
// reopened after the backoff, and the data comes back once the CRC is good.
func TestRTUOverTCPBadCRCIsNeverData(t *testing.T) {
	dev := newTestDevice()
	dev.set(func(d *testDevice) { d.holding[0] = 11 })
	srv := startRTUServer(t, dev)
	srv.badCRC.Store(true)

	h := newHarness(t, modbusInput(srv.connection(`"timeout":"2s"`), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}))
	h.start()

	if got := h.take(); len(got) != 0 {
		t.Fatalf("records for an answer with a bad CRC: %+v", got)
	}
	health := h.in.Health()
	if health.Connected {
		t.Error("connected after a bad CRC")
	}
	if health.ReadErrors[input.ReadErrorConnection] != 1 || len(health.ReadErrors) != 1 {
		t.Errorf("ReadErrors = %v, want one connection error", health.ReadErrors)
	}

	srv.badCRC.Store(false)
	var records []model.Record
	for range 10 {
		h.step(time.Second)
		if records = h.take(); len(records) > 0 {
			break
		}
	}
	if got := readings(t, records); len(got) != 1 || got[0].value != model.IntValue(11) || got[0].quality != model.Good {
		t.Fatalf("samples after the CRC is good again = %+v", got)
	}
	if n := srv.accepted.Load(); n != 2 {
		t.Errorf("connections = %d, want 2 (closed and reopened)", n)
	}
	if !h.in.Health().Connected {
		t.Error("not connected after the reconnect")
	}
}

// TestRTUMissingSerialPortFailsStart (ruling R2, core R47): a serial port
// that cannot be opened is a local resource error, so Start fails, with a
// fixed text that does not name the device.
func TestRTUMissingSerialPortFailsStart(t *testing.T) {
	device := filepath.Join(t.TempDir(), "ttyMISSING9")
	conn := `{"transport":"rtu","device":"` + filepath.ToSlash(device) + `","baud":9600}`

	in, err := New(modbusInput([]byte(conn), 30*time.Second, config.Device{
		Device: "press-07",
		Points: []config.Point{pollPoint("speed", "hr:0", "int16", time.Second)},
	}), clock.NewFake(epoch))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = in.Start(t.Context(), func(model.Record) { t.Error("a failed Start emitted") })
	const want = `modbus input "line": start: cannot open the serial port`
	if err == nil || err.Error() != want {
		t.Fatalf("Start error = %v, want %q", err, want)
	}
	if strings.Contains(err.Error(), "ttyMISSING9") {
		t.Errorf("Start error %q names the device", err)
	}
	if in.Health().Connected {
		t.Error("connected after a failed Start")
	}
	if err := in.Close(); err != nil {
		t.Errorf("Close after a failed Start: %v", err)
	}
}

// TestClientConfiguration checks the mapping of a connection to the library
// without opening anything: the URL per transport and, for rtu, the serial
// settings. The stop bits are always explicit, so the library's own default
// (2 stop bits without parity) never applies.
func TestClientConfiguration(t *testing.T) {
	tests := []struct {
		name       string
		connection string
		want       mbus.ClientConfiguration
	}{
		{"tcp", `{"transport":"tcp","host":"plc"}`,
			mbus.ClientConfiguration{URL: "tcp://plc:502", Timeout: time.Second}},
		{"rtu over tcp, IPv6", `{"transport":"rtuovertcp","host":"fd00::7","port":4001,"timeout":"250ms"}`,
			mbus.ClientConfiguration{URL: "rtuovertcp://[fd00::7]:4001", Timeout: 250 * time.Millisecond}},
		{"rtu defaults", `{"transport":"rtu","device":"/dev/ttyUSB0","baud":19200}`,
			mbus.ClientConfiguration{URL: "rtu:///dev/ttyUSB0", Speed: 19200, DataBits: 8, Parity: mbus.PARITY_NONE, StopBits: 1, Timeout: time.Second}},
		{"rtu even parity", `{"transport":"rtu","device":"COM3","baud":9600,"data_bits":7,"parity":"even","stop_bits":2,"timeout":"300ms"}`,
			mbus.ClientConfiguration{URL: "rtu://COM3", Speed: 9600, DataBits: 7, Parity: mbus.PARITY_EVEN, StopBits: 2, Timeout: 300 * time.Millisecond}},
		{"rtu odd parity", `{"transport":"rtu","device":"/dev/ttyS1","baud":115200,"parity":"odd"}`,
			mbus.ClientConfiguration{URL: "rtu:///dev/ttyS1", Speed: 115200, DataBits: 8, Parity: mbus.PARITY_ODD, StopBits: 1, Timeout: time.Second}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := ParseConnection([]byte(tt.connection))
			if err != nil {
				t.Fatalf("ParseConnection: %v", err)
			}
			got, err := clientConfiguration(conn)
			if err != nil {
				t.Fatalf("clientConfiguration: %v", err)
			}
			if got.Logger == nil || got.Logger.Writer() != io.Discard {
				t.Error("the library logger is not silenced")
			}
			got.Logger = nil
			if *got != tt.want {
				t.Errorf("configuration\n got %+v\nwant %+v", *got, tt.want)
			}
		})
	}
}
