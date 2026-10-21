package driverkit

import (
	"context"
	"sync"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
)

// window is the span the rate limit is counted over.
const window = time.Second

// RateLimiter lets at most a fixed number of requests through in any
// sliding second. It is safe for concurrent use.
type RateLimiter struct {
	clk       clock.Clock
	perSecond int

	mu   sync.Mutex
	sent []time.Time // times of the requests let through in the last window, oldest first
}

// NewRateLimiter returns a limiter of perSecond requests per second on clk;
// a perSecond below 1 is taken as 1.
func NewRateLimiter(clk clock.Clock, perSecond int) *RateLimiter {
	return &RateLimiter{clk: clk, perSecond: max(perSecond, 1)}
}

// Wait blocks until a request may be sent and counts it. It returns
// ctx.Err() when ctx is done first, and then counts nothing.
func (l *RateLimiter) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		wait := l.reserve()
		if wait <= 0 {
			return nil
		}

		if err := l.clk.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// reserve counts a request now and returns 0 when the window has room, else
// returns how long until the oldest request leaves the window.
func (l *RateLimiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clk.Now()
	expired := 0
	for expired < len(l.sent) && !l.sent[expired].Add(window).After(now) {
		expired++
	}
	l.sent = l.sent[expired:]

	if len(l.sent) < l.perSecond {
		l.sent = append(l.sent, now)

		return 0
	}

	return l.sent[0].Add(window).Sub(now)
}
