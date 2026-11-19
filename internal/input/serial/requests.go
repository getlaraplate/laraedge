package serial

import "time"

// scheduler decides when each configured request is written to the
// instrument. It is pure: no clock, no locks, no logging; the caller passes
// the time of every call.
//
// Caller contract, each read cycle: call frame(at) for every frame read, then
// expired(at) (counting no_response when true), then next(at) and write the
// command when ok. At most one command is awaiting an answer at any time, so
// next never sends while one is awaiting, even past its timeout, until
// expired has reported it.
type scheduler struct {
	reqs     []Request
	due      []time.Time
	timeout  time.Duration
	awaiting bool
	sentAt   time.Time
}

// newScheduler builds a scheduler whose requests are all first due at start.
func newScheduler(reqs []Request, timeout time.Duration, start time.Time) *scheduler {
	due := make([]time.Time, len(reqs))
	for i := range due {
		due[i] = start
	}
	return &scheduler{reqs: reqs, due: due, timeout: timeout}
}

// next returns the command to send at the given time: the due request with
// the earliest due time (configuration order on a tie), when none is awaiting
// an answer. The request is next due one Every after it was sent, so a turn
// missed while blocked is sent once, not repeated.
func (s *scheduler) next(at time.Time) ([]byte, bool) {
	if s.awaiting {
		return nil, false
	}
	best := -1
	for i := range s.reqs {
		if at.Before(s.due[i]) {
			continue
		}
		if best < 0 || s.due[i].Before(s.due[best]) {
			best = i
		}
	}
	if best < 0 {
		return nil, false
	}
	s.due[best] = at.Add(s.reqs[best].Every)
	s.awaiting = true
	s.sentAt = at
	return s.reqs[best].Command, true
}

// frame records that a frame was read: it answers the awaited command. With
// nothing awaiting (a late answer after expired) it does nothing.
func (s *scheduler) frame(at time.Time) {
	s.awaiting = false
}

// expired reports, once per command, that the awaited command got no frame
// within the request timeout, and frees the slot.
func (s *scheduler) expired(at time.Time) bool {
	if !s.awaiting || at.Sub(s.sentAt) < s.timeout {
		return false
	}
	s.awaiting = false
	return true
}
