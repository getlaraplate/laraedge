package modbus

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
)

func TestParseConnection(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Connection
	}{
		{
			name: "tcp defaults",
			raw:  `{"transport":"tcp","host":"10.0.0.5"}`,
			want: Connection{Transport: "tcp", Host: "10.0.0.5", Port: 502, Timeout: time.Second, UnitID: 1, RateLimit: 20},
		},
		{
			name: "tcp explicit",
			raw:  `{"transport":"tcp","host":"plc","port":1502,"timeout":"250ms","unit_id":0,"rate_limit":5}`,
			want: Connection{Transport: "tcp", Host: "plc", Port: 1502, Timeout: 250 * time.Millisecond, UnitID: 0, RateLimit: 5},
		},
		{
			name: "rtuovertcp defaults",
			raw:  `{"transport":"rtuovertcp","host":"gw"}`,
			want: Connection{Transport: "rtuovertcp", Host: "gw", Port: 502, Timeout: time.Second, UnitID: 1, RateLimit: 20},
		},
		{
			name: "rtu defaults",
			raw:  `{"transport":"rtu","device":"/dev/ttyUSB0","baud":9600}`,
			want: Connection{Transport: "rtu", Device: "/dev/ttyUSB0", Baud: 9600, DataBits: 8, Parity: "none", StopBits: 1, Timeout: time.Second, UnitID: 1, RateLimit: 20},
		},
		{
			name: "rtu explicit",
			raw:  `{"transport":"rtu","device":"COM3","baud":19200,"data_bits":7,"parity":"even","stop_bits":2,"unit_id":255}`,
			want: Connection{Transport: "rtu", Device: "COM3", Baud: 19200, DataBits: 7, Parity: "even", StopBits: 2, Timeout: time.Second, UnitID: 255, RateLimit: 20},
		},
		{
			name: "rtu odd parity",
			raw:  `{"transport":"rtu","device":"/dev/ttyS1","baud":4800,"parity":"odd"}`,
			want: Connection{Transport: "rtu", Device: "/dev/ttyS1", Baud: 4800, DataBits: 8, Parity: "odd", StopBits: 1, Timeout: time.Second, UnitID: 1, RateLimit: 20},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseConnection(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("ParseConnection: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseConnectionRefusals(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", ``, "connection: must be an object"},
		{"null", `null`, "connection: must be an object"},
		{"array", `[]`, "connection: must be an object"},
		{"missing transport", `{}`, "connection.transport: required"},
		{"bad transport", `{"transport":"udp","host":"SECRET-HOST"}`, "connection.transport: want tcp, rtu or rtuovertcp"},
		{"transport not string", `{"transport":1}`, "connection.transport: must be a string"},
		{"tcp without host", `{"transport":"tcp"}`, "connection.host: required for tcp"},
		{"rtuovertcp without host", `{"transport":"rtuovertcp"}`, "connection.host: required for rtuovertcp"},
		{"empty host", `{"transport":"tcp","host":""}`, "connection.host: required for tcp"},
		{"host not string", `{"transport":"tcp","host":7}`, "connection.host: must be a string"},
		{"tcp refuses device", `{"transport":"tcp","host":"h","device":"/dev/x"}`, "connection.device: only for rtu"},
		{"tcp refuses baud", `{"transport":"tcp","host":"h","baud":9600}`, "connection.baud: only for rtu"},
		{"tcp refuses parity", `{"transport":"tcp","host":"h","parity":"even"}`, "connection.parity: only for rtu"},
		{"tcp refuses data_bits", `{"transport":"tcp","host":"h","data_bits":8}`, "connection.data_bits: only for rtu"},
		{"tcp refuses stop_bits", `{"transport":"tcp","host":"h","stop_bits":1}`, "connection.stop_bits: only for rtu"},
		{"port zero", `{"transport":"tcp","host":"h","port":0}`, "connection.port: want an integer from 1 to 65535"},
		{"port too big", `{"transport":"tcp","host":"h","port":65536}`, "connection.port: want an integer from 1 to 65535"},
		{"port negative", `{"transport":"tcp","host":"h","port":-1}`, "connection.port: want an integer from 1 to 65535"},
		{"port fraction", `{"transport":"tcp","host":"h","port":1.5}`, "connection.port: want an integer from 1 to 65535"},
		{"port string", `{"transport":"tcp","host":"h","port":"502"}`, "connection.port: want an integer from 1 to 65535"},
		{"rtu without device", `{"transport":"rtu","baud":9600}`, "connection.device: required for rtu"},
		{"rtu refuses host", `{"transport":"rtu","device":"/dev/x","baud":9600,"host":"h"}`, "connection.host: not for rtu"},
		{"rtu refuses port", `{"transport":"rtu","device":"/dev/x","baud":9600,"port":502}`, "connection.port: not for rtu"},
		{"rtu without baud", `{"transport":"rtu","device":"/dev/x"}`, "connection.baud: required for rtu"},
		{"baud zero", `{"transport":"rtu","device":"/dev/x","baud":0}`, "connection.baud: want a positive integer"},
		{"baud negative", `{"transport":"rtu","device":"/dev/x","baud":-9600}`, "connection.baud: want a positive integer"},
		{"baud string", `{"transport":"rtu","device":"/dev/x","baud":"9600"}`, "connection.baud: want a positive integer"},
		{"data bits 9", `{"transport":"rtu","device":"/dev/x","baud":9600,"data_bits":9}`, "connection.data_bits: want 5, 6, 7 or 8"},
		{"data bits 4", `{"transport":"rtu","device":"/dev/x","baud":9600,"data_bits":4}`, "connection.data_bits: want 5, 6, 7 or 8"},
		{"parity mark", `{"transport":"rtu","device":"/dev/x","baud":9600,"parity":"mark"}`, "connection.parity: want none, even or odd"},
		{"parity uppercase", `{"transport":"rtu","device":"/dev/x","baud":9600,"parity":"EVEN"}`, "connection.parity: want none, even or odd"},
		{"stop bits 3", `{"transport":"rtu","device":"/dev/x","baud":9600,"stop_bits":3}`, "connection.stop_bits: want 1 or 2"},
		{"stop bits 0", `{"transport":"rtu","device":"/dev/x","baud":9600,"stop_bits":0}`, "connection.stop_bits: want 1 or 2"},
		{"timeout not a duration", `{"transport":"tcp","host":"h","timeout":"soon"}`, "connection.timeout: want a duration such as 1s"},
		{"timeout number", `{"transport":"tcp","host":"h","timeout":1}`, "connection.timeout: want a duration such as 1s"},
		{"timeout zero", `{"transport":"tcp","host":"h","timeout":"0s"}`, "connection.timeout: must be positive"},
		{"timeout negative", `{"transport":"tcp","host":"h","timeout":"-1s"}`, "connection.timeout: must be positive"},
		{"unit id 256", `{"transport":"tcp","host":"h","unit_id":256}`, "connection.unit_id: want an integer from 0 to 255"},
		{"unit id negative", `{"transport":"tcp","host":"h","unit_id":-1}`, "connection.unit_id: want an integer from 0 to 255"},
		{"rate limit zero", `{"transport":"tcp","host":"h","rate_limit":0}`, "connection.rate_limit: want a positive integer"},
		{"rate limit negative", `{"transport":"tcp","host":"h","rate_limit":-3}`, "connection.rate_limit: want a positive integer"},
		{"rate limit fraction", `{"transport":"tcp","host":"h","rate_limit":2.5}`, "connection.rate_limit: want a positive integer"},
		{"unknown field", `{"transport":"tcp","host":"h","retries":3}`, "connection.retries: unknown field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConnection(json.RawMessage(tc.raw))
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tc.want {
				t.Fatalf("got %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseConnectionErrorsNeverEchoValues(t *testing.T) {
	secrets := []string{"SECRET-HOST", "SECRET-DEV", "SECRET-PARITY", "SECRET-TIMEOUT"}
	raws := []string{
		`{"transport":"SECRET-HOST"}`,
		`{"transport":"tcp","host":"SECRET-HOST","timeout":"SECRET-TIMEOUT"}`,
		`{"transport":"tcp","host":"SECRET-HOST","port":"SECRET-HOST"}`,
		`{"transport":"rtu","device":"SECRET-DEV","baud":9600,"parity":"SECRET-PARITY"}`,
		`{"transport":"rtu","device":"SECRET-DEV","baud":9600,"host":"SECRET-HOST"}`,
		`{"transport":"tcp","host":"SECRET-HOST","device":"SECRET-DEV"}`,
		`{"transport":"tcp","host":"SECRET-HOST","unit_id":"SECRET-HOST"}`,
	}
	for _, raw := range raws {
		_, err := ParseConnection(json.RawMessage(raw))
		if err == nil {
			t.Fatalf("%s: expected an error", raw)
		}
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Fatalf("%s: error %q echoes a value", raw, err.Error())
			}
		}
	}
}

func TestParseAddress(t *testing.T) {
	u3, u0, u255 := uint8(3), uint8(0), uint8(255)
	tests := []struct {
		in   string
		want Address
	}{
		{"hr:10", Address{Area: AreaHoldingRegister, Offset: 10}},
		{"ir:0#8", Address{Area: AreaInputRegister, Offset: 0, Registers: 8}},
		{"co:5@3", Address{Area: AreaCoil, Offset: 5, Unit: &u3}},
		{"di:65535", Address{Area: AreaDiscreteInput, Offset: 65535}},
		{"hr:100#4@255", Address{Area: AreaHoldingRegister, Offset: 100, Registers: 4, Unit: &u255}},
		{"hr:0@0", Address{Area: AreaHoldingRegister, Offset: 0, Unit: &u0}},
		{"hr:007", Address{Area: AreaHoldingRegister, Offset: 7}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseAddress(tc.in)
			if err != nil {
				t.Fatalf("ParseAddress: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseAddressRefusals(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "address: want <area>:<offset>[#<registers>][@<unit>]"},
		{"hr", "address: want <area>:<offset>[#<registers>][@<unit>]"},
		{"hr:", "address: offset: want an integer from 0 to 65535"},
		{"xx:1", "address: area: want hr, ir, co or di"},
		{"HR:1", "address: area: want hr, ir, co or di"},
		{"40011", "address: want <area>:<offset>[#<registers>][@<unit>]"},
		{"hr:65536", "address: offset: want an integer from 0 to 65535"},
		{"hr:-1", "address: offset: want an integer from 0 to 65535"},
		{"hr:+1", "address: offset: want an integer from 0 to 65535"},
		{"hr:1.5", "address: offset: want an integer from 0 to 65535"},
		{"hr:0x10", "address: offset: want an integer from 0 to 65535"},
		{"hr:99999999999999999999", "address: offset: want an integer from 0 to 65535"},
		{"hr:1#0", "address: registers: want an integer from 1 to 65535"},
		{"hr:1#", "address: registers: want an integer from 1 to 65535"},
		{"hr:1#-2", "address: registers: want an integer from 1 to 65535"},
		{"hr:1#65536", "address: registers: want an integer from 1 to 65535"},
		{"hr:1@256", "address: unit: want an integer from 0 to 255"},
		{"hr:1@", "address: unit: want an integer from 0 to 255"},
		{"hr:1@-1", "address: unit: want an integer from 0 to 255"},
		{"hr:1@3#4", "address: unit: want an integer from 0 to 255"},
		{"hr:1@3@4", "address: unit: want an integer from 0 to 255"},
		{"hr:1#2#3", "address: registers: want an integer from 1 to 65535"},
		{"hr:1 ", "address: offset: want an integer from 0 to 65535"},
		{"hr:1:2", "address: offset: want an integer from 0 to 65535"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			_, err := ParseAddress(tc.in)
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tc.want {
				t.Fatalf("got %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAddressRegisterCountAndAreaKind(t *testing.T) {
	if !AreaHoldingRegister.IsRegister() || !AreaInputRegister.IsRegister() {
		t.Fatal("hr and ir are register areas")
	}
	if AreaCoil.IsRegister() || AreaDiscreteInput.IsRegister() {
		t.Fatal("co and di are bit areas")
	}
}

func point(typ, byteOrder, mode string) config.Point {
	return config.Point{Signal: "s", Type: typ, ByteOrder: byteOrder, Read: config.Read{Mode: mode, Interval: time.Second}}
}

func TestCheckPointAccepts(t *testing.T) {
	tests := []struct {
		name string
		p    config.Point
		addr string
	}{
		{"bool on co", point("bool", "", config.ReadPoll), "co:0"},
		{"bool on di", point("bool", "", config.ReadPoll), "di:65535"},
		{"int16 on hr", point("int16", "", config.ReadPoll), "hr:0"},
		{"uint16 on ir", point("uint16", "", config.ReadPoll), "ir:65535"},
		{"int32 ABCD", point("int32", "ABCD", config.ReadPoll), "hr:10"},
		{"uint32 CDAB", point("uint32", "CDAB", config.ReadPoll), "hr:10"},
		{"float32 BADC", point("float32", "BADC", config.ReadPoll), "ir:65534"},
		{"float64 DCBA", point("float64", "DCBA", config.ReadPoll), "hr:65532"},
		{"int64", point("int64", "", config.ReadPoll), "hr:0"},
		{"uint64", point("uint64", "", config.ReadPoll), "hr:0"},
		{"string with count", point("string", "", config.ReadPoll), "hr:10#8"},
		{"string last register", point("string", "", config.ReadPoll), "ir:65535#1"},
		{"string 125 registers", point("string", "", config.ReadPoll), "hr:0#125"},
		{"unit override", point("int16", "", config.ReadPoll), "hr:1@7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddress(tc.addr)
			if err != nil {
				t.Fatalf("ParseAddress: %v", err)
			}
			if err := CheckPoint(tc.p, a); err != nil {
				t.Fatalf("CheckPoint: %v", err)
			}
		})
	}
}

func TestCheckPointRefusals(t *testing.T) {
	tests := []struct {
		name string
		p    config.Point
		addr string
		want string
	}{
		{"bool on hr", point("bool", "", config.ReadPoll), "hr:0", "type bool: only on co or di"},
		{"bool on ir", point("bool", "", config.ReadPoll), "ir:0", "type bool: only on co or di"},
		{"float32 on co", point("float32", "", config.ReadPoll), "co:0", "type float32: only on hr or ir"},
		{"int16 on di", point("int16", "", config.ReadPoll), "di:0", "type int16: only on hr or ir"},
		{"string on co", point("string", "", config.ReadPoll), "co:0#4", "type string: only on hr or ir"},
		{"count on int16", point("int16", "", config.ReadPoll), "hr:0#4", "address: #<registers> is only for string"},
		{"count on bool", point("bool", "", config.ReadPoll), "co:0#1", "address: #<registers> is only for string"},
		{"count on float64", point("float64", "", config.ReadPoll), "hr:0#4", "address: #<registers> is only for string"},
		{"string without count", point("string", "", config.ReadPoll), "hr:0", "address: string needs #<registers>"},
		{"string over one block", point("string", "", config.ReadPoll), "hr:0#126", "address: #<registers> must not exceed 125"},
		{"no type", point("", "", config.ReadPoll), "hr:0", "type: want bool, int16, uint16, int32, uint32, float32, int64, uint64, float64 or string"},
		{"unknown type", point("decimal", "", config.ReadPoll), "hr:0", "type: want bool, int16, uint16, int32, uint32, float32, int64, uint64, float64 or string"},
		{"subscribe", point("int16", "", config.ReadSubscribe), "hr:0", "read: only poll is supported"},
		{"event", point("int16", "", config.ReadEvent), "hr:0", "read: only poll is supported"},
		{"no read mode", point("int16", "", ""), "hr:0", "read: only poll is supported"},
		{"bad byte order", point("int32", "ABDC", config.ReadPoll), "hr:0", "byte_order: want ABCD, CDAB, BADC or DCBA"},
		{"lowercase byte order", point("int32", "abcd", config.ReadPoll), "hr:0", "byte_order: want ABCD, CDAB, BADC or DCBA"},
		{"int32 beyond end", point("int32", "", config.ReadPoll), "hr:65535", "address: offset and size go beyond 65535"},
		{"float64 beyond end", point("float64", "", config.ReadPoll), "ir:65533", "address: offset and size go beyond 65535"},
		{"string beyond end", point("string", "", config.ReadPoll), "hr:65530#7", "address: offset and size go beyond 65535"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAddress(tc.addr)
			if err != nil {
				t.Fatalf("ParseAddress: %v", err)
			}
			err = CheckPoint(tc.p, a)
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tc.want {
				t.Fatalf("got %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestPointSize(t *testing.T) {
	tests := []struct {
		typ  string
		regs uint16
		want uint16
	}{
		{"bool", 0, 1}, {"int16", 0, 1}, {"uint16", 0, 1},
		{"int32", 0, 2}, {"uint32", 0, 2}, {"float32", 0, 2},
		{"int64", 0, 4}, {"uint64", 0, 4}, {"float64", 0, 4},
		{"string", 9, 9},
	}
	for _, tc := range tests {
		t.Run(tc.typ, func(t *testing.T) {
			p := point(tc.typ, "", config.ReadPoll)
			got, err := Size(p, Address{Area: AreaHoldingRegister, Registers: tc.regs})
			if err != nil {
				t.Fatalf("Size: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
	if _, err := Size(point("nope", "", config.ReadPoll), Address{}); err == nil {
		t.Fatal("expected an error for an unknown type")
	}
}
