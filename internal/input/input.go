// Package input defines what the supervisor needs of an input driver: a
// component that reads machines and emits samples and device events. Drivers
// are read-only (spec 9.1). The package also holds the device lifecycle helper
// shared by drivers (ruling R8).
package input

import (
	"context"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/model"
)

// Input is one running input instance (ruling R33).
type Input interface {
	// Start starts reading. It returns once the input is running, or an
	// error when it cannot start; it must give up when ctx is done. Start
	// fails only on an invalid configuration or a local resource error (a
	// serial port that cannot be opened, a file that cannot be read), never
	// because the machine is unreachable: an unreachable machine is reported
	// by death after the death tolerance and birth on recovery, while the
	// input keeps retrying (ruling R47). A failing Start rolls the whole
	// configuration back. The goroutines it starts stop when ctx is
	// cancelled. emit receives every sample and device event, from any
	// goroutine of the input, and may block while the record is written to
	// the buffers.
	//
	// The supervisor sends the births of the input's devices itself, on
	// start; the input emits death after its death tolerance without a
	// successful read and birth again on recovery (see Lifecycle).
	Start(ctx context.Context, emit func(model.Record)) error
	// Close stops the input and waits for its goroutines; after it returns
	// emit is never called again. It is safe on an input never started.
	Close() error
	// Probe reads every point once and returns the samples, without
	// emitting them (field command probe).
	Probe(ctx context.Context) ([]model.Sample, error)
}

// Factory builds the input of cfg. It only validates and prepares: nothing
// may open a connection or a device before Start, so the supervisor can build every new input of a
// document and refuse the document without touching the running ones.
type Factory func(cfg config.Input, clk clock.Clock) (Input, error)

// Signals returns the birth signals of d: one per point, with the data type
// derived from the point type (ruling R3 revised) and the unit.
func Signals(d config.Device) []model.SignalInfo {
	out := make([]model.SignalInfo, 0, len(d.Points))
	for _, p := range d.Points {
		out = append(out, model.SignalInfo{Signal: p.Signal, DataType: p.DataType(), Unit: p.Unit})
	}

	return out
}
