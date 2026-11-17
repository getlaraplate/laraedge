package mqtt

import (
	"context"
	"errors"
	"testing"
	"time"
)

// doneToken is a paho token that is already done with err.
type doneToken struct{ err error }

func (doneToken) Wait() bool                     { return true }
func (doneToken) WaitTimeout(time.Duration) bool { return true }
func (d doneToken) Error() error                 { return d.err }

func (doneToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)

	return ch
}

// TestWaitPrefersADoneTokenToAnEndedContext: when the broker's answer and
// the end of the context are both ready, the answer wins, every time.
func TestWaitPrefersADoneTokenToAnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	failed := errors.New("refused")
	for range 1000 {
		if err := wait(ctx, doneToken{}); err != nil {
			t.Fatalf("wait = %v, want the token's success", err)
		}
		if err := wait(ctx, doneToken{err: failed}); !errors.Is(err, failed) {
			t.Fatalf("wait = %v, want the token's error", err)
		}
	}
}
