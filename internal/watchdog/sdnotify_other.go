//go:build !linux

package watchdog

import (
	"context"
	"time"
)

// Notifier does nothing outside Linux: Windows uses the service recovery
// actions instead (spec 10.1).
type Notifier struct{}

// New returns a Notifier that does nothing.
func New(Config) *Notifier {
	return &Notifier{}
}

// Enabled reports false.
func (*Notifier) Enabled() bool { return false }

// Interval returns zero.
func (*Notifier) Interval() time.Duration { return 0 }

// Ready does nothing.
func (*Notifier) Ready() {}

// Stopping does nothing.
func (*Notifier) Stopping() {}

// Run returns nil at once.
func (*Notifier) Run(context.Context) error { return nil }
