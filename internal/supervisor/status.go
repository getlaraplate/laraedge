package supervisor

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/buffer"
	"github.com/getlaraplate/laraedge/internal/input"
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
	// TransformErrors counts the runtime errors of the input's transform
	// chains since the agent started, across the restarts of the input.
	TransformErrors uint64
	// Connected reports whether the input's connection to the machine is
	// up; nil when the input does not report its health (input.HealthReporter)
	// or failed.
	Connected *bool
	// ReadErrors counts the input's read errors by kind; nil when Connected
	// is nil.
	ReadErrors map[string]uint64
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
		is := InputStatus{
			ID: id, Driver: ri.cfg.Driver, State: InputRunning,
			TransformErrors: s.transformErrors[id] + ri.proc.Errors(),
		}
		if hr, ok := ri.in.(input.HealthReporter); ok {
			h := hr.Health()
			is.Connected = &h.Connected
			is.ReadErrors = maps.Clone(h.ReadErrors)
			if is.ReadErrors == nil {
				is.ReadErrors = map[string]uint64{}
			}
		}
		st.Inputs = append(st.Inputs, is)
	}
	for id, f := range s.failedIn {
		st.Inputs = append(st.Inputs, InputStatus{
			ID: id, Driver: f.kind, State: StateFailed, LastError: f.err,
			TransformErrors: s.transformErrors[id],
		})
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
