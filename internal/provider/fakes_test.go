package provider_test

import (
	"bytes"
	"context"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/supervisor"
)

// guard bounds every wait for a goroutine; it is a deadlock guard, never a
// timing assertion.
const guard = 10 * time.Second

var start = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

// fakeApplier records the documents it is asked to apply and answers with
// outcome.
type fakeApplier struct {
	mu      sync.Mutex
	docs    []config.Document
	outcome supervisor.Outcome
	version int64
	applied chan config.Document
	entered chan struct{}
	// hook runs inside Apply, after entered is signalled.
	hook func(ctx context.Context)
}

func newFakeApplier() *fakeApplier {
	return &fakeApplier{
		outcome: supervisor.Outcome{Result: supervisor.ResultApplied},
		applied: make(chan config.Document, 64),
		entered: make(chan struct{}, 64),
	}
}

func (a *fakeApplier) Apply(ctx context.Context, doc config.Document) (supervisor.Outcome, error) {
	a.entered <- struct{}{}
	if a.hook != nil {
		a.hook(ctx)
	}

	a.mu.Lock()
	a.docs = append(a.docs, doc)
	out := a.outcome
	if out.Result == supervisor.ResultApplied {
		a.version = doc.Version
	}
	a.mu.Unlock()
	a.applied <- doc

	return out, nil
}

func (a *fakeApplier) AppliedVersion() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.version
}

func (a *fakeApplier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.docs)
}

func (a *fakeApplier) setOutcome(o supervisor.Outcome) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.outcome = o
}

func (a *fakeApplier) next(t *testing.T) config.Document {
	t.Helper()

	select {
	case d := <-a.applied:
		return d
	case <-time.After(guard):
		t.Fatal("no document was applied")

		return config.Document{}
	}
}

// runner owns a provider's Run goroutine.
type runner struct {
	cancel context.CancelFunc
	done   chan error
}

func run(t *testing.T, fn func(context.Context) error) *runner {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- fn(ctx) }()
	t.Cleanup(func() { r.stop(t) })

	return r
}

func (r *runner) stop(t *testing.T) {
	t.Helper()

	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
		r.done <- nil
	case <-time.After(guard):
		t.Fatal("Run did not return after cancel")
	}
}

// waitTimers waits until the fake clock holds at least n pending timers:
// the provider is waiting for its next poll.
func waitTimers(t *testing.T, clk *clock.Fake, n int) {
	t.Helper()
	waitFor(t, "pending timers", func() bool { return clk.PendingTimers() >= n })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s not reached", what)
		}
		runtime.Gosched()
	}
}

// lockedBuffer is a log sink safe for concurrent use.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func (b *lockedBuffer) has(s ...string) bool {
	for _, l := range strings.Split(b.String(), "\n") {
		all := true
		for _, part := range s {
			all = all && strings.Contains(l, part)
		}
		if all {
			return true
		}
	}

	return false
}

func doc(version int) string {
	if version == 0 {
		return `{"schema":"laraplate-edge-config/1","inputs":[],"outputs":[]}`
	}

	return `{"schema":"laraplate-edge-config/1","version":` + itoa(version) + `,"inputs":[],"outputs":[]}`
}

func itoa(n int) string { return strconv.Itoa(n) }
