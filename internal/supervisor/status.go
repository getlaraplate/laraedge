package supervisor

import (
	"slices"
	"strings"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/buffer"
)

// Status is a snapshot of the supervisor (spec 6.8, 10.3).
type Status struct {
	// Applied reports whether a document was applied.
	Applied bool
	// Version is the applied document's version, 0 when it has none.
	Version int64
	// Inputs are the inputs of the applied document, by id.
	Inputs []InputStatus
	// Outputs are the outputs of the applied document, by id.
	Outputs []OutputStatus
}

// InputStatus describes one input.
type InputStatus struct {
	// ID is the input id.
	ID string
	// Driver is the input driver.
	Driver string
	// State is InputRunning or StateFailed.
	State string
	// LastError is why a failed input is not running.
	LastError string
}

// OutputStatus describes one output.
type OutputStatus struct {
	// ID is the output id.
	ID string
	// Type is the output type.
	Type string
	// State is the sending state (sending, backoff, suspended_auth) or
	// StateFailed.
	State string
	// Buffer holds the figures of the output's buffer, zero when failed.
	Buffer buffer.Stats
	// ClockOffset is the backend clock minus the agent clock.
	ClockOffset time.Duration
	// LastConfirmation is when a batch was last confirmed, zero before any.
	LastConfirmation time.Time
	// LastError is the last error, without secrets, and LastErrorAt when it
	// happened.
	LastError   string
	LastErrorAt time.Time
}

// Status returns the current state of the inputs and outputs.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := Status{Applied: s.applied, Version: s.version}

	for id, ri := range s.inputs {
		st.Inputs = append(st.Inputs, InputStatus{ID: id, Driver: ri.cfg.Driver, State: InputRunning})
	}
	for id, f := range s.failedIn {
		st.Inputs = append(st.Inputs, InputStatus{ID: id, Driver: f.kind, State: StateFailed, LastError: f.err})
	}

	for id, ro := range s.outputs {
		lastErr, lastErrAt := ro.out.LastError()
		st.Outputs = append(st.Outputs, OutputStatus{
			ID:               id,
			Type:             ro.cfg.Type,
			State:            ro.out.State().String(),
			Buffer:           ro.buf.Stats(),
			ClockOffset:      ro.out.ClockOffset(),
			LastConfirmation: ro.out.LastConfirmation(),
			LastError:        lastErr,
			LastErrorAt:      lastErrAt,
		})
	}
	for id, f := range s.failedOut {
		st.Outputs = append(st.Outputs, OutputStatus{ID: id, Type: f.kind, State: StateFailed, LastError: f.err})
	}

	slices.SortFunc(st.Inputs, func(a, b InputStatus) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(st.Outputs, func(a, b OutputStatus) int { return strings.Compare(a.ID, b.ID) })

	return st
}
