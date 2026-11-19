package serial

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	bug "go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// ReadTimeout is how long a Read waits for data before it returns 0, nil.
const ReadTimeout = 100 * time.Millisecond

// Errors OpenPort tells apart.
var (
	// ErrPortBusy is a port another process (or input) holds open.
	ErrPortBusy = errors.New("serial port is busy")
	// ErrPortMissing is a port that does not exist.
	ErrPortMissing = errors.New("serial port does not exist")
	// ErrPortPermission is a port the agent may not open.
	ErrPortPermission = errors.New("serial port permission denied")
)

// Mode is the line setting of a port. Parity is "none", "even" or "odd".
type Mode struct {
	Baud     int
	DataBits int
	StopBits int
	Parity   string
}

// Port is an open serial port. Read returns 0, nil when ReadTimeout passes
// without data; any error means the port is lost.
type Port interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// Opener opens a port; OpenPort is the production one.
type Opener func(device string, mode Mode) (Port, error)

// OpenPort opens device with mode, sets the read timeout and flushes what
// the port received before the open (the tail of a frame). Errors are
// wrapped into ErrPortBusy, ErrPortMissing or ErrPortPermission when the
// cause is known.
func OpenPort(device string, mode Mode) (Port, error) {
	parity, err := libraryParity(mode.Parity)
	if err != nil {
		return nil, err
	}
	stop := bug.OneStopBit
	switch mode.StopBits {
	case 1:
	case 2:
		stop = bug.TwoStopBits
	default:
		return nil, fmt.Errorf("serial stop bits %d not supported", mode.StopBits)
	}
	p, err := bug.Open(device, &bug.Mode{
		BaudRate: mode.Baud,
		DataBits: mode.DataBits,
		Parity:   parity,
		StopBits: stop,
	})
	if err != nil {
		return nil, classifyOpenError(err)
	}
	if err := p.SetReadTimeout(ReadTimeout); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("set serial read timeout: %w", err)
	}
	if err := p.ResetInputBuffer(); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("flush serial input: %w", err)
	}
	return p, nil
}

func libraryParity(parity string) (bug.Parity, error) {
	switch parity {
	case "none", "":
		return bug.NoParity, nil
	case "even":
		return bug.EvenParity, nil
	case "odd":
		return bug.OddParity, nil
	}
	return bug.NoParity, errors.New("serial parity not supported")
}

func classifyOpenError(err error) error {
	var pe *bug.PortError
	if errors.As(err, &pe) {
		switch pe.Code() {
		case bug.PortBusy:
			return fmt.Errorf("%w: %w", ErrPortBusy, err)
		case bug.PortNotFound:
			return fmt.Errorf("%w: %w", ErrPortMissing, err)
		case bug.PermissionDenied:
			return fmt.Errorf("%w: %w", ErrPortPermission, err)
		}
		return err
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: %w", ErrPortMissing, err)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("%w: %w", ErrPortPermission, err)
	}
	return err
}

// PortInfo describes a serial port of the host. StablePath is the
// /dev/serial/by-id link that resolves to Name (Linux), empty elsewhere or when
// there is none.
type PortInfo struct {
	Name         string
	StablePath   string
	VID          string
	PID          string
	SerialNumber string
	Product      string
	USB          bool
}

// ListPorts lists the serial ports of the host.
func ListPorts() ([]PortInfo, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, fmt.Errorf("list serial ports: %w", err)
	}
	stable := stablePaths()
	out := make([]PortInfo, 0, len(details))
	for _, d := range details {
		out = append(out, PortInfo{
			Name:         d.Name,
			StablePath:   stable[d.Name],
			VID:          d.VID,
			PID:          d.PID,
			SerialNumber: d.SerialNumber,
			Product:      d.Product,
			USB:          d.IsUSB,
		})
	}
	return out, nil
}

// stablePaths maps a device name to its /dev/serial/by-id link.
func stablePaths() map[string]string {
	out := map[string]string{}
	if runtime.GOOS != "linux" {
		return out
	}
	const dir = "/dev/serial/by-id"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		link := filepath.Join(dir, e.Name())
		target, err := filepath.EvalSymlinks(link)
		if err != nil {
			continue
		}
		out[target] = link
	}
	return out
}
