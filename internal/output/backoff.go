package output

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/backoff"
)

const (
	// backoffMin is the first backoff step after a temporary error.
	backoffMin = time.Second
	// backoffMax caps the backoff and a Retry-After (spec 6.5).
	backoffMax = 5 * time.Minute
)

// newBackoff returns the exponential backoff with jitter of one output: the
// step doubles from backoffMin to backoffMax after every temporary error and
// the wait is drawn in [step/2, step]. It is owned by the Run goroutine.
func newBackoff(rand func() float64) backoff.Exponential {
	return backoff.New(backoffMin, backoffMax, rand)
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
