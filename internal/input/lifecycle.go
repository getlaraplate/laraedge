package input

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/model"
)

// Lifecycle tracks the last successful read of each device of an input. It
// emits death for a device that had no successful read for the input's
// death tolerance, and birth again at its first successful read afterwards
// (spec 4 device lifecycle, ruling R8). Every device starts alive at the
// creation time: the supervisor has just sent its birth. It is safe for
// concurrent use.
type Lifecycle struct {
	clk       clock.Clock
	tolerance time.Duration
	emit      func(model.Record)
	signals   map[string][]model.SignalInfo
	devices   []string

	// mu is held while emitting, so a death and a birth of the same device
	// are emitted in the order they were decided.
	mu   sync.Mutex
	last map[string]time.Time
	dead map[string]bool
}

// NewLifecycle returns the lifecycle of the devices of cfg, emitting through
// emit, which must not call the Lifecycle back.
func NewLifecycle(cfg config.Input, clk clock.Clock, emit func(model.Record)) *Lifecycle {
	tolerance := cfg.DeathTolerance
	if tolerance <= 0 {
		tolerance = config.DefaultDeathTolerance
	}

	l := &Lifecycle{
		clk:       clk,
		tolerance: tolerance,
		emit:      emit,
		signals:   make(map[string][]model.SignalInfo, len(cfg.Devices)),
		last:      make(map[string]time.Time, len(cfg.Devices)),
		dead:      make(map[string]bool, len(cfg.Devices)),
	}

	now := clk.Now()
	for _, d := range cfg.Devices {
		l.signals[d.Device] = append(l.signals[d.Device], Signals(d)...)
		if _, seen := l.last[d.Device]; !seen {
			l.devices = append(l.devices, d.Device)
		}
		l.last[d.Device] = now
	}
	slices.Sort(l.devices)

	return l
}

// Read records a successful read of device. A device reported dead is born
// again: its birth is emitted before Read returns, so before the sample the
// caller emits next. Devices not in the configuration are ignored.
func (l *Lifecycle) Read(device string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, known := l.last[device]; !known {
		return
	}

	now := l.clk.Now()
	l.last[device] = now
	if l.dead[device] {
		l.dead[device] = false
		l.emit(model.EventRecord(model.DeviceEvent{
			Kind:    model.Birth,
			Device:  device,
			Signals: slices.Clone(l.signals[device]),
			TS:      now.UTC(),
		}))
	}
}

// Check emits death for every live device whose last successful read is at
// least the tolerance old, in device name order.
func (l *Lifecycle) Check() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clk.Now()
	for _, device := range l.devices {
		if l.dead[device] || now.Sub(l.last[device]) < l.tolerance {
			continue
		}

		l.dead[device] = true
		l.emit(model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: device, TS: now.UTC()}))
	}
}

// Run calls Check on the clock, at the earliest deadline of a live device,
// until ctx is done.
func (l *Lifecycle) Run(ctx context.Context) {
	for {
		timer := l.clk.NewTimer(l.nextCheck())

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C():
			l.Check()
		}
	}
}

// nextCheck returns the wait until the earliest deadline of a live device,
// or the tolerance when every device is dead: a device born again has its
// deadline a whole tolerance after its read, never before that wait ends.
func (l *Lifecycle) nextCheck() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clk.Now()
	wait := l.tolerance
	for _, device := range l.devices {
		if l.dead[device] {
			continue
		}

		wait = min(wait, l.last[device].Add(l.tolerance).Sub(now))
	}

	return wait
}
