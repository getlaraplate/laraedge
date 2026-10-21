package driverkit

import (
	"sync"

	"github.com/getlaraplate/laraedge/internal/input"
)

// Health holds the connection state and the read error counters of one
// input; a driver returns its Snapshot from input.HealthReporter. It is
// safe for concurrent use; the zero value is a disconnected input without
// errors.
type Health struct {
	mu        sync.Mutex
	connected bool
	errors    map[string]uint64
}

// SetConnected records whether the connection to the machine is up.
func (h *Health) SetConnected(connected bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.connected = connected
}

// CountError counts one read error of kind (see input.ReadErrorKinds).
func (h *Health) CountError(kind string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.errors == nil {
		h.errors = make(map[string]uint64)
	}
	h.errors[kind]++
}

// Snapshot returns a copy of the current state; its ReadErrors is never nil.
func (h *Health) Snapshot() input.Health {
	h.mu.Lock()
	defer h.mu.Unlock()

	errs := make(map[string]uint64, len(h.errors))
	for kind, n := range h.errors {
		errs[kind] = n
	}

	return input.Health{Connected: h.connected, ReadErrors: errs}
}
