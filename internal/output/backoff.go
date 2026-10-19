package output

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// backoffMin is the first backoff step after a temporary error.
	backoffMin = time.Second
	// backoffMax caps the backoff and a Retry-After (spec 6.5).
	backoffMax = 5 * time.Minute
)

// backoff is the exponential backoff with jitter of one output: the step
// doubles from backoffMin to backoffMax after every temporary error and the
// wait is drawn in [step/2, step]. It is owned by one goroutine.
type backoff struct {
	attempt int
	rand    func() float64
}

// next returns the wait before the next attempt and raises the step.
func (b *backoff) next() time.Duration {
	step := backoffMin
	for i := 0; i < b.attempt && step < backoffMax; i++ {
		step *= 2
	}

	step = min(step, backoffMax)
	if step < backoffMax {
		b.attempt++
	}

	r := b.rand()
	switch {
	case r < 0 || math.IsNaN(r):
		r = 0
	case r > 1:
		r = 1
	}

	half := step / 2

	return half + time.Duration(r*float64(step-half))
}

// reset starts the backoff again from backoffMin, after a confirmation.
func (b *backoff) reset() {
	b.attempt = 0
}

// retryAfter parses a Retry-After header, delay seconds or an HTTP date,
// into a wait capped at backoffMax; a date in the past is zero. It reports
// false when the header is absent or malformed.
func retryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	if secs, err := strconv.ParseUint(value, 10, 64); err == nil {
		if secs >= uint64(backoffMax/time.Second) {
			return backoffMax, true
		}

		return time.Duration(secs) * time.Second, true
	}

	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}

	return min(max(at.Sub(now), 0), backoffMax), true
}
