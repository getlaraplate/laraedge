package output_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/buffer"
	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/model"
	"github.com/swolley/laraplate-edge/internal/output"
	"github.com/swolley/laraplate-edge/internal/secrets"
)

const (
	testToken  = "tok-3f9a1c-SECRET-never-logged"
	tokenEnv   = "EDGE_TOKEN"
	tokenRef   = "secret://env/" + tokenEnv
	ingestPath = "/api/v1/insert/mes/machine-messages"

	acceptedBody  = `{"status":"accepted","message_id":"x"}`
	duplicateBody = `{"status":"duplicate","duplicate":true,"message_id":"x"}`
)

// guard bounds every wait of a test for a goroutine; it is a deadlock guard,
// never a timing assertion.
const guard = 10 * time.Second

// reply is one scripted answer of the fake backend.
type reply struct {
	status int
	header map[string]string
	body   string
	// drop closes the connection without an answer: a network error.
	drop bool
	// hold blocks until the client gives the request up.
	hold bool
}

// request is one request the fake backend received.
type request struct {
	method    string
	path      string
	header    http.Header
	body      []byte
	messageID string
	seq       uint64
	losses    []wireLoss
}

type wireLoss struct {
	Priority string `json:"priority"`
	Samples  int    `json:"samples"`
	Reason   string `json:"reason"`
}

// backend is an httptest TLS server answering from a script, one reply per
// request in order, then 202 accepted. Its Date header follows the fake
// clock unless a reply sets one.
type backend struct {
	clk *clock.Fake
	srv *httptest.Server

	mu     sync.Mutex
	script []reply
	reqs   []request

	got chan request
}

func newBackend(t *testing.T, clk *clock.Fake, replies ...reply) *backend {
	t.Helper()

	be := &backend{clk: clk, script: replies, got: make(chan request, 256)}
	be.srv = httptest.NewUnstartedServer(http.HandlerFunc(be.serve))
	be.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	be.srv.StartTLS()
	t.Cleanup(be.srv.Close)

	return be
}

func (be *backend) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	var env struct {
		MessageID string     `json:"message_id"`
		SourceSeq uint64     `json:"source_seq"`
		Losses    []wireLoss `json:"losses"`
	}
	_ = json.Unmarshal(body, &env)

	req := request{
		method:    r.Method,
		path:      r.URL.Path,
		header:    r.Header.Clone(),
		body:      body,
		messageID: env.MessageID,
		seq:       env.SourceSeq,
		losses:    env.Losses,
	}

	be.mu.Lock()
	rep := reply{status: http.StatusAccepted, body: acceptedBody}
	if len(be.script) > 0 {
		rep, be.script = be.script[0], be.script[1:]
	}
	be.reqs = append(be.reqs, req)
	be.mu.Unlock()

	switch {
	case rep.drop:
		be.got <- req
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}

		return
	case rep.hold:
		be.got <- req
		<-r.Context().Done()

		return
	}

	w.Header().Set("Date", be.clk.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/json")
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}

	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
	be.got <- req
}

// next returns the next request the backend answered.
func (be *backend) next(t *testing.T) request {
	t.Helper()

	select {
	case r := <-be.got:
		return r
	case <-time.After(guard):
		t.Fatal("the backend received no request")

		return request{}
	}
}

func (be *backend) requests() []request {
	be.mu.Lock()
	defer be.mu.Unlock()

	return append([]request(nil), be.reqs...)
}

func (be *backend) count() int {
	be.mu.Lock()
	defer be.mu.Unlock()

	return len(be.reqs)
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

// lines returns the log lines containing s.
func (b *lockedBuffer) lines(s string) []string {
	var out []string
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.Contains(l, s) {
			out = append(out, l)
		}
	}

	return out
}

// envStore is a fake environment for secrets.WithLookupEnv.
type envStore struct {
	mu   sync.Mutex
	vars map[string]string
}

func (e *envStore) lookup(name string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	v, ok := e.vars[name]

	return v, ok
}

func (e *envStore) set(name, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.vars[name] = value
}

// runner owns one Run goroutine and a poller reading the accessors
// concurrently (for -race).
type runner struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func startRun(t *testing.T, out *output.HTTP) *runner {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{cancel: cancel, done: make(chan error, 1)}

	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for ctx.Err() == nil {
			_ = out.State().String()
			_ = out.ClockOffset()
			_, _ = out.LastError()
			_ = out.LastConfirmation()
			runtime.Gosched()
		}
	}()

	go func() {
		r.done <- out.Run(ctx)
		<-polled
	}()

	t.Cleanup(func() { r.stop(t) })

	return r
}

// stop cancels Run and fails the test unless it returns nil.
func (r *runner) stop(t *testing.T) {
	t.Helper()

	r.once.Do(func() {
		r.cancel()
		select {
		case err := <-r.done:
			if err != nil {
				t.Errorf("Run() = %v, want nil on cancellation", err)
			}
		case <-time.After(guard):
			t.Error("Run did not return after cancellation")
		}
	})
}

// waitFor yields until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s", what)
		}

		runtime.Gosched()
	}
}

// fixture is one HTTP output against a fake backend, with the real buffer,
// inflight and batcher in a temporary directory and a fake clock.
type fixture struct {
	t    *testing.T
	h    *harness
	be   *backend
	env  *envStore
	logs *lockedBuffer
	// rand is the jitter source's fixed answer.
	rand       float64
	reconnects atomic.Int32
	// client replaces the backend's client when set; useDefault leaves
	// HTTPConfig.Client nil.
	client     *http.Client
	useDefault bool

	out *output.HTTP
	run *runner
}

func newFixture(t *testing.T, maxSamples int, replies ...reply) *fixture {
	t.Helper()

	h := newHarness(t, output.BatchLimits{MaxSamples: maxSamples, MaxBody: 1 << 20})

	return &fixture{
		t:    t,
		h:    h,
		be:   newBackend(t, h.clk, replies...),
		env:  &envStore{vars: map[string]string{tokenEnv: testToken}},
		logs: &lockedBuffer{},
	}
}

func (f *fixture) config() output.HTTPConfig {
	client := f.be.srv.Client()
	if f.client != nil {
		client = f.client
	}

	if f.useDefault {
		client = nil
	}

	return output.HTTPConfig{
		ID:          "laraplate",
		URL:         f.be.srv.URL + "/",
		Token:       tokenRef,
		Resolver:    secrets.NewResolver(secrets.WithLookupEnv(f.env.lookup)),
		Batcher:     f.h.bt,
		Clock:       f.h.clk,
		Logger:      slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnReconnect: func() { f.reconnects.Add(1) },
		Client:      client,
		Rand:        func() float64 { return f.rand },
	}
}

// start opens the harness when needed, appends records and starts Run.
func (f *fixture) start(records ...model.Record) *fixture {
	f.t.Helper()

	if f.h.bt == nil {
		f.h.open()
	}

	f.h.append(records...)

	out, err := output.NewHTTP(f.config())
	if err != nil {
		f.t.Fatalf("NewHTTP() = %v", err)
	}

	f.out = out
	f.run = startRun(f.t, out)

	return f
}

// waitDelivered waits until the buffer holds no record and the output has
// recorded a confirmation at the current fake time.
func (f *fixture) waitDelivered() {
	f.t.Helper()

	waitFor(f.t, "the last batch to be confirmed", func() bool {
		return f.h.buf.Stats().Records == 0 && f.out.State() == output.Sending && f.out.LastConfirmation().Equal(f.h.clk.Now())
	})
}

// waitSettled waits until the buffer holds no record and the output is
// back to sending, for a last batch dropped rather than confirmed.
func (f *fixture) waitSettled() {
	f.t.Helper()

	waitFor(f.t, "the last batch to be settled", func() bool {
		return f.h.buf.Stats().Records == 0 && f.out.State() == output.Sending
	})
}

// expectWait asserts that Run waits exactly d on the fake clock, then lets it go.
func (f *fixture) expectWait(d time.Duration, state output.State) {
	f.t.Helper()

	waitTimers(f.t, f.h.clk, 1)
	if got := f.out.State(); got != state {
		f.t.Fatalf("State() = %s while waiting, want %s", got, state)
	}

	f.h.clk.Advance(d - time.Nanosecond)
	if f.h.clk.PendingTimers() != 1 {
		f.t.Fatalf("the wait ended before %v", d)
	}

	f.h.clk.Advance(time.Nanosecond)
}

// stoppedInflight stops Run and reads the inflight file afresh.
func (f *fixture) stoppedInflight() (*output.Batch, uint64) {
	f.t.Helper()

	f.run.stop(f.t)

	b, next, err := output.NewInflight(f.h.dir).Load()
	if err != nil {
		f.t.Fatalf("Inflight.Load() = %v", err)
	}

	return b, next
}

func sameRequest(t *testing.T, got, want request) {
	t.Helper()

	if got.messageID != want.messageID || got.seq != want.seq || !bytes.Equal(got.body, want.body) {
		t.Fatalf("resent request differs: message_id %q seq %d, want %q seq %d (bodies equal: %v)",
			got.messageID, got.seq, want.messageID, want.seq, bytes.Equal(got.body, want.body))
	}
}

func TestStateString(t *testing.T) {
	for state, want := range map[output.State]string{
		output.Sending:       "sending",
		output.Backoff:       "backoff",
		output.SuspendedAuth: "suspended_auth",
	} {
		if got := state.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(state), got, want)
		}
	}
}

func TestHTTPConfirmsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  reply
		warned bool
	}{
		{name: "202 accepted", reply: reply{status: http.StatusAccepted, body: acceptedBody}},
		{name: "200 duplicate", reply: reply{status: http.StatusOK, body: duplicateBody}},
		{name: "other 2xx", reply: reply{status: http.StatusNoContent}, warned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 2, tc.reply)
			f.start(bulk(0), bulk(1))

			req := f.be.next(t)
			f.waitDelivered()

			if req.method != http.MethodPost || req.path != ingestPath {
				t.Errorf("request %s %s, want POST %s", req.method, req.path, ingestPath)
			}

			for k, want := range map[string]string{
				"Authorization": "Bearer " + testToken,
				"Content-Type":  "application/json",
				"Accept":        "application/json",
				"User-Agent":    "laraplate-edge/dev",
			} {
				if got := req.header.Get(k); got != want {
					t.Errorf("header %s = %q, want %q", k, got, want)
				}
			}

			if got := f.out.LastConfirmation(); !got.Equal(f.h.clk.Now()) {
				t.Errorf("LastConfirmation() = %v, want %v", got, f.h.clk.Now())
			}

			if got := f.out.State(); got != output.Sending {
				t.Errorf("State() = %s, want sending", got)
			}

			b, next := f.stoppedInflight()
			if b != nil || next != 1 {
				t.Errorf("inflight = batch %v, next seq %d; want none, 1", b != nil, next)
			}

			if f.reconnects.Load() != 0 {
				t.Errorf("OnReconnect called %d times without a prior failure", f.reconnects.Load())
			}

			if warned := len(f.logs.lines("level=WARN")) > 0; warned != tc.warned {
				t.Errorf("warning logged = %v, want %v\n%s", warned, tc.warned, f.logs)
			}
		})
	}
}

func TestHTTPTemporaryErrorsResendTheSameBatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply reply
		// misconfig: logged at error, not warn.
		misconfig bool
	}{
		{name: "500", reply: reply{status: http.StatusInternalServerError}},
		{name: "503", reply: reply{status: http.StatusServiceUnavailable}},
		{name: "429", reply: reply{status: http.StatusTooManyRequests}},
		{name: "network error", reply: reply{drop: true}},
		{name: "redirect not followed", reply: reply{status: http.StatusFound, header: map[string]string{"Location": "/elsewhere"}}, misconfig: true},
		{name: "404 wrong url", reply: reply{status: http.StatusNotFound}, misconfig: true},
		{name: "400", reply: reply{status: http.StatusBadRequest}, misconfig: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 2, tc.reply)
			f.start(bulk(0), bulk(1))

			first := f.be.next(t)
			failedAt := f.h.clk.Now()
			f.expectWait(500*time.Millisecond, output.Backoff)

			msg, at := f.out.LastError()
			if msg == "" || !at.Equal(failedAt) {
				t.Errorf("LastError() = %q at %v, want a message at %v", msg, at, failedAt)
			}

			sameRequest(t, f.be.next(t), first)
			f.waitDelivered()

			if f.be.count() != 2 {
				t.Errorf("backend got %d requests, want 2", f.be.count())
			}

			for _, r := range f.be.requests() {
				if r.path != ingestPath {
					t.Errorf("request to %s: a redirect was followed", r.path)
				}
			}

			if got := f.reconnects.Load(); got != 1 {
				t.Errorf("OnReconnect called %d times, want 1", got)
			}

			if errLogged := len(f.logs.lines("level=ERROR")) > 0; errLogged != tc.misconfig {
				t.Errorf("error logged = %v, want %v\n%s", errLogged, tc.misconfig, f.logs)
			}

			if got := f.out.State(); got != output.Sending {
				t.Errorf("State() = %s after the confirmation, want sending", got)
			}
		})
	}
}

func TestHTTPBackoffDoublesFromOneSecondToFiveMinutesWithJitter(t *testing.T) {
	steps := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300}

	for _, tc := range []struct {
		name string
		rand float64
		// scale maps a step d to the expected wait.
		scale func(d time.Duration) time.Duration
	}{
		{name: "low jitter waits half", rand: 0, scale: func(d time.Duration) time.Duration { return d / 2 }},
		{name: "mid jitter", rand: 0.5, scale: func(d time.Duration) time.Duration { return d * 3 / 4 }},
		{name: "high jitter waits all", rand: 1, scale: func(d time.Duration) time.Duration { return d }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var replies []reply
			for range steps {
				replies = append(replies, reply{status: http.StatusInternalServerError})
			}

			replies = append(replies, reply{status: http.StatusAccepted, body: acceptedBody}, reply{status: http.StatusBadGateway})

			f := newFixture(t, 1, replies...)
			f.rand = tc.rand
			f.start(bulk(0))

			first := f.be.next(t)
			for _, s := range steps {
				f.expectWait(tc.scale(s*time.Second), output.Backoff)

				sameRequest(t, f.be.next(t), first)
			}

			f.waitDelivered()

			// A confirmation resets the backoff.
			f.h.append(bulk(1))
			f.be.next(t)
			f.expectWait(tc.scale(time.Second), output.Backoff)
			f.be.next(t)
			f.waitDelivered()
		})
	}
}

func TestHTTPRetryAfterRaisesTheWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		value  func(now time.Time) string
		want   time.Duration
	}{
		{name: "seconds", status: http.StatusTooManyRequests, value: func(time.Time) string { return "30" }, want: 30 * time.Second},
		{name: "http date", status: http.StatusTooManyRequests, value: func(now time.Time) string { return now.Add(45 * time.Second).UTC().Format(http.TimeFormat) }, want: 45 * time.Second},
		{name: "capped at five minutes", status: http.StatusServiceUnavailable, value: func(time.Time) string { return "3600" }, want: 5 * time.Minute},
		{name: "shorter than the backoff", status: http.StatusTooManyRequests, value: func(time.Time) string { return "0" }, want: 500 * time.Millisecond},
		{name: "date in the past", status: http.StatusTooManyRequests, value: func(now time.Time) string { return now.Add(-time.Hour).UTC().Format(http.TimeFormat) }, want: 500 * time.Millisecond},
		{name: "malformed ignored", status: http.StatusTooManyRequests, value: func(time.Time) string { return "soon" }, want: 500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 1)
			f.be.script = []reply{{status: tc.status, header: map[string]string{"Retry-After": tc.value(f.h.clk.Now())}}}
			f.start(bulk(0))

			first := f.be.next(t)
			f.expectWait(tc.want, output.Backoff)
			sameRequest(t, f.be.next(t), first)
			f.waitDelivered()
		})
	}
}

func TestHTTPAuthRefusalSuspendsAndRetriesEveryFiveMinutes(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFixture(t, 2, reply{status: status}, reply{status: status})
			f.start(bulk(0), bulk(1))

			first := f.be.next(t)
			f.expectWait(5*time.Minute, output.SuspendedAuth)
			sameRequest(t, f.be.next(t), first)

			f.expectWait(5*time.Minute, output.SuspendedAuth)
			sameRequest(t, f.be.next(t), first)
			f.waitDelivered()

			if got := f.out.State(); got != output.Sending {
				t.Errorf("State() = %s after the confirmation, want sending", got)
			}

			if got := f.reconnects.Load(); got != 1 {
				t.Errorf("OnReconnect called %d times, want 1", got)
			}

			if dropped := f.h.buf.Stats().Dropped; len(dropped) != 0 {
				t.Errorf("Dropped = %v, want nothing dropped on an auth refusal", dropped)
			}
		})
	}
}

func TestHTTPReloadCredentialsResumesAtOnce(t *testing.T) {
	f := newFixture(t, 2, reply{status: http.StatusUnauthorized})
	f.start(bulk(0), bulk(1))

	first := f.be.next(t)
	waitTimers(t, f.h.clk, 1)
	if got := f.out.State(); got != output.SuspendedAuth {
		t.Fatalf("State() = %s, want suspended_auth", got)
	}

	f.env.set(tokenEnv, "tok-rotated")
	f.out.ReloadCredentials()

	second := f.be.next(t)
	sameRequest(t, second, first)
	if got := second.header.Get("Authorization"); got != "Bearer tok-rotated" {
		t.Errorf("Authorization after reload = %q, want the new token", got)
	}

	f.waitDelivered()
}

// Review Focus: a secret://env/NAME token with NAME unset starts the output
// suspended; it buffers and recovers once the variable exists.
func TestHTTPUnsetEnvTokenStartsSuspended(t *testing.T) {
	for _, tc := range []struct {
		name    string
		recover func(f *fixture)
	}{
		{name: "on reload", recover: func(f *fixture) { f.out.ReloadCredentials() }},
		{name: "after five minutes", recover: func(f *fixture) { f.h.clk.Advance(5 * time.Minute) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 2)
			f.env.vars = map[string]string{}
			f.start(bulk(0), bulk(1))

			waitTimers(t, f.h.clk, 1)
			if got := f.out.State(); got != output.SuspendedAuth {
				t.Fatalf("State() = %s, want suspended_auth", got)
			}

			if f.be.count() != 0 {
				t.Fatalf("backend got %d requests without a token", f.be.count())
			}

			if msg, _ := f.out.LastError(); !strings.Contains(msg, tokenEnv) {
				t.Errorf("LastError() = %q, want it to name the variable", msg)
			}

			if got := f.h.buf.Stats().Records; got != 2 {
				t.Errorf("buffer holds %d records, want 2", got)
			}

			f.env.set(tokenEnv, testToken)
			tc.recover(f)

			if got := f.be.next(t).header.Get("Authorization"); got != "Bearer "+testToken {
				t.Errorf("Authorization = %q", got)
			}

			f.waitDelivered()
			if got := f.reconnects.Load(); got != 1 {
				t.Errorf("OnReconnect called %d times, want 1", got)
			}
		})
	}
}

func TestHTTP413SplitsReusingTheRefusedSeq(t *testing.T) {
	f := newFixture(t, 4, reply{status: http.StatusRequestEntityTooLarge, body: `{"message":"too large"}`})
	f.start(bulk(0), bulk(1), bulk(2), bulk(3))

	refused := f.be.next(t)
	half := f.be.next(t)
	rest := f.be.next(t)
	f.waitDelivered()

	if refused.seq != 0 || half.seq != 0 || rest.seq != 1 {
		t.Errorf("seqs = %d, %d, %d; want 0, 0, 1", refused.seq, half.seq, rest.seq)
	}

	if half.messageID == refused.messageID || rest.messageID == half.messageID {
		t.Errorf("message ids not new: %q, %q, %q", refused.messageID, half.messageID, rest.messageID)
	}

	for name, r := range map[string]request{"first half": half, "rest": rest} {
		if got := bodySamples(t, r.body); len(got) != 2 {
			t.Errorf("%s carries %d samples, want 2", name, len(got))
		}
	}

	if f.h.clk.PendingTimers() != 0 {
		t.Errorf("the split waited on the clock")
	}

	if b, next := f.stoppedInflight(); b != nil || next != 2 {
		t.Errorf("inflight = batch %v, next seq %d; want none, 2", b != nil, next)
	}
}

func TestHTTPRefusalsDropWithARejectedInvalidMarker(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxSamples int
		reply      reply
		dropped    int
	}{
		// Review Focus: a 413 on a one-sample batch cannot be split.
		{name: "one-record 413", maxSamples: 1, reply: reply{status: http.StatusRequestEntityTooLarge, body: `{"message":"too large"}`}, dropped: 1},
		{name: "422", maxSamples: 2, reply: reply{
			status: http.StatusUnprocessableEntity,
			body:   `{"message":"body-text-only-at-debug","errors":{"devices.0.samples.1.value":["body-text-only-at-debug"],"source_seq":["x"]}}`,
		}, dropped: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.maxSamples, tc.reply)
			var records []model.Record
			for i := range tc.maxSamples + 1 {
				records = append(records, bulk(i))
			}

			f.start(records...)

			refused := f.be.next(t)
			next := f.be.next(t)
			f.waitDelivered()

			if next.seq != refused.seq || next.messageID == refused.messageID {
				t.Errorf("next batch seq %d id %q, want the refused seq %d under a new id", next.seq, next.messageID, refused.seq)
			}

			want := []wireLoss{{Priority: "bulk", Samples: tc.dropped, Reason: model.LossRejectedInvalid}}
			if !reflect.DeepEqual(next.losses, want) {
				t.Errorf("next batch losses = %+v, want %+v", next.losses, want)
			}

			if got := bodySamples(t, next.body); len(got) != 1 {
				t.Errorf("next batch carries %d samples, want 1", len(got))
			}

			if f.h.clk.PendingTimers() != 0 || f.be.count() != 2 {
				t.Errorf("the drop waited or resent: timers %d, requests %d", f.h.clk.PendingTimers(), f.be.count())
			}

			if tc.reply.status == http.StatusUnprocessableEntity {
				warn := f.logs.lines("level=WARN")
				if len(warn) == 0 || !strings.Contains(warn[0], "devices.0.samples.1.value") || !strings.Contains(warn[0], "source_seq") {
					t.Errorf("warn log lacks the field paths:\n%s", f.logs)
				}

				for _, l := range f.logs.lines("body-text-only-at-debug") {
					if !strings.Contains(l, "level=DEBUG") {
						t.Errorf("response body logged above debug: %s", l)
					}
				}
			}
		})
	}
}

// markFault fails MarkSealed while fail is set.
type markFault struct {
	output.Buffer
	fail atomic.Bool
}

func (m *markFault) MarkSealed(p model.Priority, upTo buffer.Position) error {
	if m.fail.Load() {
		return errors.New("faulty: mark sealed refused")
	}

	return m.Buffer.MarkSealed(p, upTo)
}

// Drop and Shrink failing (markers or marks not durable) keep the batch in
// flight, back off and handle the resent batch's answer again.
func TestHTTPDropAndShrinkErrorsKeepTheBatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply reply
		// arm makes the next Drop or Shrink fail and returns the cure.
		arm func(h *harness) func()
	}{
		{name: "drop on 422", reply: reply{status: http.StatusUnprocessableEntity, body: `{"message":"x","errors":{}}`}, arm: func(h *harness) func() {
			var fb *faultyBuffer
			h.wrap = func(b *buffer.Buffer) output.Buffer {
				fb = &faultyBuffer{Buffer: b, addLoss: faultFailing}

				return fb
			}

			return func() { fb.addLoss = faultNone }
		}},
		{name: "shrink on 413", reply: reply{status: http.StatusRequestEntityTooLarge}, arm: func(h *harness) func() {
			var mf *markFault
			h.wrap = func(b *buffer.Buffer) output.Buffer {
				mf = &markFault{Buffer: b}
				mf.fail.Store(true)

				return mf
			}

			return func() { mf.fail.Store(false) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 2, tc.reply, tc.reply)
			cure := tc.arm(f.h)
			f.h.open()
			f.start(bulk(0), bulk(1))

			first := f.be.next(t)
			waitTimers(t, f.h.clk, 1)
			if got := f.out.State(); got != output.Backoff {
				t.Fatalf("State() = %s after a failed settlement, want backoff", got)
			}

			if b, _, err := output.NewInflight(f.h.dir).Load(); err != nil || b == nil || b.MessageID != first.messageID {
				t.Fatalf("inflight lost the refused batch: %v, %v", b, err)
			}

			cure()
			f.expectWait(500*time.Millisecond, output.Backoff)
			sameRequest(t, f.be.next(t), first)
			f.waitSettled()
		})
	}
}

func TestHTTPNeverLogsTheToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "valid token", token: testToken},
		{name: "token with a control character", token: "tok-ctl\x01SECRET-ctl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 1,
				reply{status: http.StatusUnauthorized, body: `{"message":"Unauthenticated."}`},
				reply{status: http.StatusUnprocessableEntity, body: `{"message":"bad","errors":{"devices":["bad"]}}`},
				reply{status: http.StatusServiceUnavailable},
				reply{drop: true},
			)
			f.env.vars[tokenEnv] = tc.token

			if tc.token != testToken {
				f.start(bulk(0))
				waitTimers(t, f.h.clk, 1)
				if got := f.out.State(); got != output.SuspendedAuth {
					t.Fatalf("State() = %s with an invalid token, want suspended_auth", got)
				}
			} else {
				f.start(bulk(0), bulk(1))
				f.be.next(t) // 401
				waitTimers(t, f.h.clk, 1)
				f.out.ReloadCredentials()
				f.be.next(t) // 422, dropped
				f.be.next(t) // 503
				f.expectWait(500*time.Millisecond, output.Backoff)
				f.be.next(t) // network error
				f.expectWait(time.Second, output.Backoff)
				f.be.next(t) // 202
				f.waitDelivered()
			}

			f.run.stop(t)

			secret := tc.token
			if i := strings.Index(secret, "SECRET"); i >= 0 {
				secret = secret[i:]
			}

			if strings.Contains(f.logs.String(), secret) {
				t.Errorf("the token appears in the logs:\n%s", f.logs)
			}

			if msg, _ := f.out.LastError(); msg == "" || strings.Contains(msg, secret) {
				t.Errorf("LastError() = %q, want a message without the token", msg)
			}
		})
	}
}

func TestHTTPClockOffsetFromTheDateHeader(t *testing.T) {
	f := newFixture(t, 1)
	now := f.h.clk.Now()
	date := func(d time.Duration) reply {
		return reply{status: http.StatusAccepted, body: acceptedBody, header: map[string]string{"Date": now.Add(d).UTC().Format(http.TimeFormat)}}
	}
	f.be.script = []reply{date(5 * time.Second), date(6 * time.Second), date(0), date(-7 * time.Second)}

	f.start()
	for i, want := range []time.Duration{5 * time.Second, 6 * time.Second, 0, -7 * time.Second} {
		f.h.append(bulk(i))
		f.be.next(t)
		f.waitDelivered()

		if got := f.out.ClockOffset(); got != want {
			t.Errorf("response %d: ClockOffset() = %v, want %v", i, got, want)
		}
	}

	if got := len(f.logs.lines("clock")); got != 2 {
		t.Errorf("clock warnings = %d, want 2 (once per crossing)\n%s", got, f.logs)
	}
}

func TestHTTPRunReturnsOnCancel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   reply
		records int
		// inflight: the batch is still in flight after the stop.
		inflight bool
	}{
		{name: "idle", records: 0},
		{name: "in backoff", reply: reply{status: http.StatusServiceUnavailable}, records: 1, inflight: true},
		{name: "suspended", reply: reply{status: http.StatusUnauthorized}, records: 1, inflight: true},
		{name: "request in flight", reply: reply{hold: true}, records: 1, inflight: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 1, tc.reply)
			var records []model.Record
			for i := range tc.records {
				records = append(records, bulk(i))
			}

			f.start(records...)
			if tc.records > 0 {
				f.be.next(t)
			}

			if !tc.reply.hold && tc.records > 0 {
				waitTimers(t, f.h.clk, 1)
			}

			b, _ := f.stoppedInflight()
			if (b != nil) != tc.inflight {
				t.Errorf("batch in flight after stop = %v, want %v", b != nil, tc.inflight)
			}
		})
	}
}

func TestHTTPTLSVerificationCannotBeDisabled(t *testing.T) {
	t.Run("no config field", func(t *testing.T) {
		typ := reflect.TypeFor[output.HTTPConfig]()
		for i := range typ.NumField() {
			name := strings.ToLower(typ.Field(i).Name)
			for _, bad := range []string{"insecure", "skip", "verify", "tls"} {
				if strings.Contains(name, bad) {
					t.Errorf("HTTPConfig has field %s", typ.Field(i).Name)
				}
			}
		}
	})

	t.Run("default client verifies", func(t *testing.T) {
		f := newFixture(t, 1)
		f.useDefault = true
		f.start(bulk(0))

		waitTimers(t, f.h.clk, 1)
		if got := f.out.State(); got != output.Backoff {
			t.Errorf("State() = %s against an untrusted certificate, want backoff", got)
		}

		if f.be.count() != 0 {
			t.Errorf("an untrusted server received %d requests", f.be.count())
		}

		if msg, _ := f.out.LastError(); !strings.Contains(msg, "certificate") {
			t.Errorf("LastError() = %q, want a certificate error", msg)
		}
	})

	t.Run("insecure client refused", func(t *testing.T) {
		f := newFixture(t, 1)
		f.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the test asserts it is refused
		f.h.open()
		if _, err := output.NewHTTP(f.config()); err == nil {
			t.Error("NewHTTP() accepted a client that skips TLS verification")
		}
	})
}

func TestNewHTTPValidatesTheConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(c *output.HTTPConfig)
		ok     bool
	}{
		{name: "https", mutate: func(*output.HTTPConfig) {}, ok: true},
		{name: "http on loopback", mutate: func(c *output.HTTPConfig) { c.URL = "http://127.0.0.1:8080" }, ok: true},
		{name: "http on localhost", mutate: func(c *output.HTTPConfig) { c.URL = "http://localhost/laraplate/" }, ok: true},
		{name: "keystore reference suspends at run", mutate: func(c *output.HTTPConfig) { c.Token = "secret://keystore/edge" }, ok: true},
		{name: "http elsewhere", mutate: func(c *output.HTTPConfig) { c.URL = "http://mes.example.com" }},
		{name: "other scheme", mutate: func(c *output.HTTPConfig) { c.URL = "ftp://mes.example.com" }},
		{name: "credentials in url", mutate: func(c *output.HTTPConfig) { c.URL = "https://u:p@mes.example.com" }},
		{name: "relative url", mutate: func(c *output.HTTPConfig) { c.URL = "/api" }},
		{name: "plain token", mutate: func(c *output.HTTPConfig) { c.Token = testToken }},
		{name: "malformed reference", mutate: func(c *output.HTTPConfig) { c.Token = "secret://env/1BAD" }},
		{name: "no id", mutate: func(c *output.HTTPConfig) { c.ID = "" }},
		{name: "no resolver", mutate: func(c *output.HTTPConfig) { c.Resolver = nil }},
		{name: "no batcher", mutate: func(c *output.HTTPConfig) { c.Batcher = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 1)
			f.h.open()
			cfg := f.config()
			tc.mutate(&cfg)

			_, err := output.NewHTTP(cfg)
			if (err == nil) != tc.ok {
				t.Fatalf("NewHTTP() error = %v, want ok %v", err, tc.ok)
			}

			if err != nil && strings.Contains(err.Error(), testToken) {
				t.Errorf("error echoes the token: %v", err)
			}
		})
	}
}
