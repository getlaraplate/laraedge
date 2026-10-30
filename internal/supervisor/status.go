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
	// Plugins are the plugins the applied document uses, by id.
	Plugins []PluginStatus
	// Processors are the processors of the applied document, by id.
	Processors []ProcessorStatus
}

// States of a processor besides StateFailed.
const (
	// ProcessorUp is a processor whose Process stream is open.
	ProcessorUp = "up"
	// ProcessorDown is a running processor whose stream is not open: its
	// plugin is down or restarting; its derived signals are bad.
	ProcessorDown = "down"
)

// ProcessorStatus describes one processor.
type ProcessorStatus struct {
	// ID is the processor id.
	ID string
	// Plugin is the id of the plugin providing it.
	Plugin string
	// State is ProcessorUp, ProcessorDown or StateFailed.
	State string
	// Refused counts what the plugin sent outside the declared derived
	// devices and signals, or with another value kind; Dropped counts the
	// copies of observed records the queue had no room for. Both count
	// since the processor instance started.
	Refused uint64
	Dropped uint64
	// LastError is why a failed processor is not running.
	LastError string
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

// PluginStatus describes one plugin the applied document uses.
type PluginStatus struct {
	// ID is the plugin id.
	ID string
	// State is the state of the plugin host (plugin.State: starting, up,
	// down) or StateFailed for a plugin a rollback could not start again.
	State string
	// Pid is the process id of the plugin while it is up, else 0.
	Pid int
	// Restarts counts the restarts of the plugin process since its host
	// started; RestartsByReason splits it by reason (exit, health, memory,
	// hash).
	Restarts         uint64
	RestartsByReason map[string]uint64
	// LastError is the last reason the plugin went down or failed to start.
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
	// ClockOffset is the backend clock minus the agent clock; always zero
	// for an output of type plugin:<id>, which has no clock offset.
	ClockOffset time.Duration
	// LastConfirmation is when a batch was last confirmed, zero before any.
	LastConfirmation time.Time
	// LastError is the last error, without secrets, and LastErrorAt when it
	// happened.
	LastError   string
	LastErrorAt time.Time
}

// Status returns the current state of the inputs, outputs, plugins and
// processors.
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
		out := OutputStatus{
			ID:               id,
			Type:             ro.cfg.Type,
			State:            ro.out.State().String(),
			Buffer:           ro.buf.Stats(),
			LastConfirmation: ro.out.LastConfirmation(),
			LastError:        lastErr,
			LastErrorAt:      lastErrAt,
		}
		if co, ok := ro.out.(clockOffsetter); ok {
			out.ClockOffset = co.ClockOffset()
		}
		st.Outputs = append(st.Outputs, out)
	}
	for id, f := range s.failedOut {
		st.Outputs = append(st.Outputs, OutputStatus{ID: id, Type: f.kind, State: StateFailed, LastError: f.err})
	}

	for id, rp := range s.plugins {
		hs := rp.host.Status()
		st.Plugins = append(st.Plugins, PluginStatus{
			ID: id, State: string(hs.State), Pid: hs.Pid,
			Restarts: hs.Restarts, RestartsByReason: hs.RestartsByReason, LastError: hs.LastError,
		})
	}
	for id, f := range s.failedPlugins {
		st.Plugins = append(st.Plugins, PluginStatus{ID: id, State: StateFailed, LastError: f.err})
	}

	for id, rp := range s.processors {
		ps := rp.proc.Stats()
		state := ProcessorDown
		if ps.Up {
			state = ProcessorUp
		}
		st.Processors = append(st.Processors, ProcessorStatus{
			ID: id, Plugin: rp.cfg.Plugin, State: state, Refused: ps.Refused, Dropped: ps.Dropped,
		})
	}
	for id, f := range s.failedProc {
		st.Processors = append(st.Processors, ProcessorStatus{ID: id, Plugin: f.kind, State: StateFailed, LastError: f.err})
	}

	slices.SortFunc(st.Inputs, func(a, b InputStatus) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(st.Outputs, func(a, b OutputStatus) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(st.Plugins, func(a, b PluginStatus) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(st.Processors, func(a, b ProcessorStatus) int { return strings.Compare(a.ID, b.ID) })

	return st
}
