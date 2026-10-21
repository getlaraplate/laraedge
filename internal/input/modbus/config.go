// Package modbus is the Modbus input driver: the parsing and validation of the
// connection and of the point addresses (this file), the block planner, the
// register decoder and the poll loop. The driver is read-only (parent 9.1).
//
// Every error of this file is a fixed message: it names the field, never the
// value, so a host name, a device path or a rejected setting cannot reach a log
// (global constraint on connection settings). The factory wraps the errors with
// the input and the point position.
package modbus

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
)

// Transports of the connection (spec section 5).
const (
	// TransportTCP is Modbus TCP.
	TransportTCP = "tcp"
	// TransportRTU is RTU framing over a serial port.
	TransportRTU = "rtu"
	// TransportRTUOverTCP is RTU framing over TCP, for serial-to-Ethernet
	// gateways and for testing RTU without a port.
	TransportRTUOverTCP = "rtuovertcp"
)

// Defaults of the connection (spec section 5).
const (
	// DefaultPort is the Modbus TCP port.
	DefaultPort uint16 = 502
	// DefaultTimeout is the request timeout.
	DefaultTimeout = time.Second
	// DefaultUnitID is the unit of the input's points.
	DefaultUnitID uint8 = 1
	// DefaultRateLimit is the number of requests per second.
	DefaultRateLimit = 20
	// DefaultDataBits is the serial data bits.
	DefaultDataBits = 8
	// DefaultParity is the serial parity.
	DefaultParity = "none"
	// DefaultStopBits is the serial stop bits.
	DefaultStopBits = 1
)

// Limits of a request and of the address space (spec section 5).
const (
	// MaxOffset is the last register or bit offset.
	MaxOffset = 65535
	// MaxBlockRegisters is the largest register block of one request.
	MaxBlockRegisters = 125
	// MaxBlockBits is the largest coil or discrete input block of one request.
	MaxBlockBits = 2000
)

// Connection is the parsed `connection` of a Modbus input. The fields of the
// other transport family are zero: Host and Port for rtu, and the serial
// settings for tcp and rtuovertcp.
type Connection struct {
	// Transport is TransportTCP, TransportRTU or TransportRTUOverTCP.
	Transport string
	// Host and Port locate the device for tcp and rtuovertcp.
	Host string
	Port uint16
	// Device is the serial port path for rtu.
	Device string
	// Baud, DataBits, Parity ("none", "even", "odd") and StopBits are the
	// serial settings for rtu.
	Baud     int
	DataBits int
	Parity   string
	StopBits int
	// Timeout bounds one request.
	Timeout time.Duration
	// UnitID is the default unit of the input's points.
	UnitID uint8
	// RateLimit is the requests per second.
	RateLimit int
}

var connectionFields = []string{
	"transport", "host", "port", "device", "baud", "data_bits", "parity",
	"stop_bits", "timeout", "unit_id", "rate_limit",
}

// serialFields are accepted by rtu only, networkFields by tcp and rtuovertcp
// only. Baud has no default: the spec gives none, so rtu requires it.
var (
	serialFields  = []string{"device", "baud", "data_bits", "parity", "stop_bits"}
	networkFields = []string{"host", "port"}
)

// ParseConnection validates the `connection` object of a Modbus input and
// applies the defaults of spec section 5.
func ParseConnection(raw json.RawMessage) (Connection, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Connection{}, errors.New("connection: must be an object")
	}
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(connectionFields, key) {
			return Connection{}, errors.New("connection." + key + ": unknown field")
		}
	}

	var conn Connection
	if err := stringField(fields, "transport", &conn.Transport); err != nil {
		return Connection{}, err
	}
	switch conn.Transport {
	case "":
		return Connection{}, errors.New("connection.transport: required")
	case TransportTCP, TransportRTU, TransportRTUOverTCP:
	default:
		return Connection{}, errors.New("connection.transport: want tcp, rtu or rtuovertcp")
	}

	if conn.Transport == TransportRTU {
		for _, key := range networkFields {
			if _, ok := fields[key]; ok {
				return Connection{}, errors.New("connection." + key + ": not for rtu")
			}
		}
		if err := parseSerial(fields, &conn); err != nil {
			return Connection{}, err
		}
	} else {
		for _, key := range serialFields {
			if _, ok := fields[key]; ok {
				return Connection{}, errors.New("connection." + key + ": only for rtu")
			}
		}
		if err := parseNetwork(fields, &conn); err != nil {
			return Connection{}, err
		}
	}

	conn.Timeout = DefaultTimeout
	if v, ok := fields["timeout"]; ok {
		var text string
		if err := json.Unmarshal(v, &text); err != nil {
			return Connection{}, errors.New("connection.timeout: want a duration such as 1s")
		}
		d, err := time.ParseDuration(text)
		if err != nil {
			return Connection{}, errors.New("connection.timeout: want a duration such as 1s")
		}
		if d <= 0 {
			return Connection{}, errors.New("connection.timeout: must be positive")
		}
		conn.Timeout = d
	}

	conn.UnitID = DefaultUnitID
	if v, ok := fields["unit_id"]; ok {
		n, err := integer(v, 0, 255)
		if err != nil {
			return Connection{}, errors.New("connection.unit_id: want an integer from 0 to 255")
		}
		conn.UnitID = uint8(n)
	}

	conn.RateLimit = DefaultRateLimit
	if v, ok := fields["rate_limit"]; ok {
		n, err := integer(v, 1, 1<<31-1)
		if err != nil {
			return Connection{}, errors.New("connection.rate_limit: want a positive integer")
		}
		conn.RateLimit = int(n)
	}
	return conn, nil
}

func parseNetwork(fields map[string]json.RawMessage, conn *Connection) error {
	if err := stringField(fields, "host", &conn.Host); err != nil {
		return err
	}
	if conn.Host == "" {
		return errors.New("connection.host: required for " + conn.Transport)
	}
	conn.Port = DefaultPort
	if v, ok := fields["port"]; ok {
		n, err := integer(v, 1, 65535)
		if err != nil {
			return errors.New("connection.port: want an integer from 1 to 65535")
		}
		conn.Port = uint16(n)
	}
	return nil
}

func parseSerial(fields map[string]json.RawMessage, conn *Connection) error {
	if err := stringField(fields, "device", &conn.Device); err != nil {
		return err
	}
	if conn.Device == "" {
		return errors.New("connection.device: required for rtu")
	}

	v, ok := fields["baud"]
	if !ok {
		return errors.New("connection.baud: required for rtu")
	}
	baud, err := integer(v, 1, 1<<31-1)
	if err != nil {
		return errors.New("connection.baud: want a positive integer")
	}
	conn.Baud = int(baud)

	conn.DataBits = DefaultDataBits
	if v, ok := fields["data_bits"]; ok {
		n, err := integer(v, 5, 8)
		if err != nil {
			return errors.New("connection.data_bits: want 5, 6, 7 or 8")
		}
		conn.DataBits = int(n)
	}

	conn.Parity = DefaultParity
	if _, ok := fields["parity"]; ok {
		if err := stringField(fields, "parity", &conn.Parity); err != nil {
			return err
		}
		if !slices.Contains([]string{"none", "even", "odd"}, conn.Parity) {
			return errors.New("connection.parity: want none, even or odd")
		}
	}

	conn.StopBits = DefaultStopBits
	if v, ok := fields["stop_bits"]; ok {
		n, err := integer(v, 1, 2)
		if err != nil {
			return errors.New("connection.stop_bits: want 1 or 2")
		}
		conn.StopBits = int(n)
	}
	return nil
}

// stringField decodes the JSON string at key into dst when the key is present.
func stringField(fields map[string]json.RawMessage, key string, dst *string) error {
	v, ok := fields[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(v, dst); err != nil {
		return errors.New("connection." + key + ": must be a string")
	}
	return nil
}

// integer parses a JSON integer literal within [lo, hi]. A string, a fraction
// or an exponent is refused.
func integer(v json.RawMessage, lo, hi int64) (int64, error) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, errors.New("out of range")
	}
	return n, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Area is the Modbus data area of an address.
type Area string

// Areas of spec section 5.
const (
	// AreaHoldingRegister is `hr`, holding registers (function 3).
	AreaHoldingRegister Area = "hr"
	// AreaInputRegister is `ir`, input registers (function 4).
	AreaInputRegister Area = "ir"
	// AreaCoil is `co`, coils (function 1).
	AreaCoil Area = "co"
	// AreaDiscreteInput is `di`, discrete inputs (function 2).
	AreaDiscreteInput Area = "di"
)

// IsRegister reports whether the area holds 16-bit registers (hr, ir) rather
// than bits (co, di).
func (a Area) IsRegister() bool {
	return a == AreaHoldingRegister || a == AreaInputRegister
}

// Address is a parsed point address `<area>:<offset>[#<registers>][@<unit>]`.
type Address struct {
	// Area is the data area.
	Area Area
	// Offset is the first register or bit, 0 to 65535.
	Offset uint16
	// Registers is the `#n` length in registers, 0 when absent.
	Registers uint16
	// Unit is the `@u` override of the input's unit id, nil when absent.
	Unit *uint8
}

const addressShape = "address: want <area>:<offset>[#<registers>][@<unit>]"

// ParseAddress parses a point address. It checks the syntax and ranges only;
// CheckPoint relates the address to the point's type.
func ParseAddress(s string) (Address, error) {
	rest, unitText, hasUnit := strings.Cut(s, "@")
	rest, regsText, hasRegs := strings.Cut(rest, "#")
	areaText, offsetText, ok := strings.Cut(rest, ":")
	if !ok {
		return Address{}, errors.New(addressShape)
	}

	var a Address
	switch area := Area(areaText); area {
	case AreaHoldingRegister, AreaInputRegister, AreaCoil, AreaDiscreteInput:
		a.Area = area
	default:
		return Address{}, errors.New("address: area: want hr, ir, co or di")
	}

	offset, ok := digits(offsetText, MaxOffset)
	if !ok {
		return Address{}, errors.New("address: offset: want an integer from 0 to 65535")
	}
	a.Offset = uint16(offset)

	if hasRegs {
		n, ok := digits(regsText, 65535)
		if !ok || n < 1 {
			return Address{}, errors.New("address: registers: want an integer from 1 to 65535")
		}
		a.Registers = uint16(n)
	}
	if hasUnit {
		n, ok := digits(unitText, 255)
		if !ok {
			return Address{}, errors.New("address: unit: want an integer from 0 to 255")
		}
		unit := uint8(n)
		a.Unit = &unit
	}
	return a, nil
}

// digits parses a non-empty run of ASCII digits no greater than limit. A sign,
// a space or a prefix is refused.
func digits(s string, limit uint64) (uint64, bool) {
	if s == "" || len(s) > 10 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n > limit {
		return 0, false
	}
	return n, true
}

// Point types of spec section 5.
var pointTypes = []string{
	"bool", "int16", "uint16", "int32", "uint32", "float32", "int64", "uint64", "float64", "string",
}

// Size returns the number of registers the point occupies (one bit for bool,
// which sits on a bit area): 1 for bool, int16 and uint16, 2 for int32, uint32
// and float32, 4 for int64, uint64 and float64, the `#n` count for string.
func Size(p config.Point, a Address) (uint16, error) {
	switch p.Type {
	case "bool", "int16", "uint16":
		return 1, nil
	case "int32", "uint32", "float32":
		return 2, nil
	case "int64", "uint64", "float64":
		return 4, nil
	case "string":
		return a.Registers, nil
	}
	return 0, errors.New("type: unknown")
}

// CheckPoint validates a point against its parsed address: the read mode, the
// type against the area, `#n` (string only), the end of the address space and
// the byte order. The Modbus driver reads by polling only.
func CheckPoint(p config.Point, a Address) error {
	if p.Read.Mode != config.ReadPoll {
		return errors.New("read: only poll is supported")
	}
	if !slices.Contains(pointTypes, p.Type) {
		return errors.New("type: want bool, int16, uint16, int32, uint32, float32, int64, uint64, float64 or string")
	}

	switch {
	case p.Type == "bool" && a.Area.IsRegister():
		return errors.New("type bool: only on co or di")
	case p.Type != "bool" && !a.Area.IsRegister():
		return errors.New("type " + p.Type + ": only on hr or ir")
	}

	if p.Type == "string" {
		if a.Registers == 0 {
			return errors.New("address: string needs #<registers>")
		}
		if a.Registers > MaxBlockRegisters {
			return errors.New("address: #<registers> must not exceed 125")
		}
	} else if a.Registers != 0 {
		return errors.New("address: #<registers> is only for string")
	}

	size, err := Size(p, a)
	if err != nil {
		return err
	}
	if int(a.Offset)+int(size)-1 > MaxOffset {
		return errors.New("address: offset and size go beyond 65535")
	}

	switch p.ByteOrder {
	case "", "ABCD", "CDAB", "BADC", "DCBA":
	default:
		return errors.New("byte_order: want ABCD, CDAB, BADC or DCBA")
	}
	return nil
}
