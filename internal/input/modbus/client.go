package modbus

import (
	"errors"
	"io"
	"log"
	"net"
	"strconv"

	mbus "github.com/simonvetter/modbus"
)

// client is what the driver needs of a Modbus client: open and close the
// transport, address a unit and the four read functions. It has no write
// function, so the driver cannot send one (parent 9.1). *mbus.ModbusClient
// implements it. A client is used by one goroutine at a time (decision M5).
type client interface {
	Open() error
	Close() error
	SetUnitId(id uint8) error
	ReadRegisters(addr, quantity uint16, regType mbus.RegType) ([]uint16, error)
	ReadCoils(addr, quantity uint16) ([]bool, error)
	ReadDiscreteInputs(addr, quantity uint16) ([]bool, error)
}

// clientFactory builds the client of a connection without opening it.
type clientFactory func(conn Connection) (client, error)

// errClientConfig is the fixed text of a connection the library refuses: the
// library's own message carries the URL.
var errClientConfig = errors.New("the Modbus library refused the connection settings")

// parities maps the connection's parity to the library's.
var parities = map[string]uint{
	"none": mbus.PARITY_NONE,
	"even": mbus.PARITY_EVEN,
	"odd":  mbus.PARITY_ODD,
}

// newLibraryClient builds the library client of conn, without opening it.
func newLibraryClient(conn Connection) (client, error) {
	cfg, err := clientConfiguration(conn)
	if err != nil {
		return nil, err
	}

	c, err := mbus.NewClient(cfg)
	if err != nil {
		return nil, errClientConfig
	}

	return c, nil
}

// clientConfiguration maps conn to the library's configuration: the URL per
// transport, the request timeout and, for rtu, the serial settings. The stop
// bits are always set, so the library's own default (2 without parity) never
// applies. The logger discards everything: the library logs to stdout by
// default, and its lines carry the host, port or device.
func clientConfiguration(conn Connection) (*mbus.ClientConfiguration, error) {
	cfg := &mbus.ClientConfiguration{
		Timeout: conn.Timeout,
		Logger:  log.New(io.Discard, "", 0),
	}

	switch conn.Transport {
	case TransportTCP, TransportRTUOverTCP:
		cfg.URL = conn.Transport + "://" + net.JoinHostPort(conn.Host, strconv.Itoa(int(conn.Port)))
	case TransportRTU:
		parity, ok := parities[conn.Parity]
		if !ok {
			return nil, errClientConfig
		}
		cfg.URL = TransportRTU + "://" + conn.Device
		cfg.Speed = uint(conn.Baud)
		cfg.DataBits = uint(conn.DataBits)
		cfg.Parity = parity
		cfg.StopBits = uint(conn.StopBits)
	default:
		return nil, errClientConfig
	}

	return cfg, nil
}
