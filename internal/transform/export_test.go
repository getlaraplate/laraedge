package transform

import (
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

// NewChain builds a Chain from ready steps, bypassing the catalogue, so tests
// can drive the chain with fake steps.
var NewChain = newChain

// ArmedDeadline returns the deadline the processor's goroutine waits on, and
// whether it waits on one.
func ArmedDeadline(p *Processor) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.armed, p.armedOK
}

// DeviceDown tells the chain's steps that their device died.
func DeviceDown(c *Chain) ([]model.Sample, []error) { return c.deviceDown() }

// ClockSteppedBack tells the chain's steps the wall clock went back to now.
func ClockSteppedBack(c *Chain, now time.Time) { c.clockSteppedBack(now) }
