package output

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/docrules"
	"github.com/swolley/laraplate-edge/internal/model"
	"github.com/swolley/laraplate-edge/internal/secrets"
	"github.com/swolley/laraplate-edge/internal/version"
)

const (
	// ingestPath is the backend route of machine messages, below the URL.
	ingestPath = "/api/v1/insert/mes/machine-messages"
	// requestTimeout bounds one request, response body included.
	requestTimeout = 30 * time.Second
	// authRetry is the wait of a suspended output before it tries again.
	authRetry = 5 * time.Minute
	// skewThreshold is the clock offset above which a warning is logged.
	skewThreshold = 2 * time.Second
	// maxResponseBody bounds how much of a response is read.
	maxResponseBody = 64 << 10
)

// State is the sending state of an output (spec 6.8).
type State int32

const (
	// Sending is the normal state: batches are sent as they are sealed.
	Sending State = iota
	// Backoff waits after a temporary error before resending the batch.
	Backoff
	// SuspendedAuth waits after the backend refused the token, or the token
	// could not be resolved; records keep buffering.
	SuspendedAuth
)

// String returns the state's metric name.
func (s State) String() string {
	switch s {
	case Sending:
		return "sending"
	case Backoff:
		return "backoff"
	case SuspendedAuth:
		return "suspended_auth"
	default:
		return fmt.Sprintf("state(%d)", int32(s))
	}
}

// Output delivers the batches of one output to its destination.
type Output interface {
	// Run sends until ctx is done.
	Run(ctx context.Context) error
	// State returns the current sending state; safe for concurrent use.
	State() State
	// ReloadCredentials resolves the credentials again before the next
	// attempt and ends a suspension at once; safe for concurrent use.
	ReloadCredentials()
}

// SecretResolver resolves a secret:// reference (*secrets.Resolver).
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// HTTPConfig configures an HTTP output. There is deliberately no way to
// disable TLS certificate verification.
type HTTPConfig struct {
	// ID is the output id, used in logs.
	ID string
	// URL is the backend base URL: https, or http on a loopback host. A
	// trailing slash is tolerated.
	URL string
	// Token is the secret:// reference of the bearer token.
	Token string
	// Resolver resolves Token at start, on ReloadCredentials and after a
	// suspension.
	Resolver SecretResolver
	// Batcher seals the batches of this output.
	Batcher *Batcher
	// Clock times the backoff and the suspension; nil means clock.Real().
	Clock clock.Clock
	// Logger receives the output's logs; nil means slog.Default().
	Logger *slog.Logger
	// OnReconnect is called, from Run, on the first confirmation after
	// Backoff or SuspendedAuth (ruling R5). It must not block for long.
	OnReconnect func()
	// Client sends the requests; nil means a client with the system roots,
	// the proxy from the environment and no redirects. A client whose
	// transport skips certificate verification is refused, and redirects
	// are never followed whatever the client says.
	Client *http.Client
	// Rand returns the jitter in [0, 1); nil means math/rand/v2.
	Rand func() float64
}

// HTTP is the laraplate-http output: it sends the sealed batches of its
// Batcher to the backend ingest route, one at a time, and settles each one
// from the response (spec 6.5).
type HTTP struct {
	id          string
	endpoint    string
	tokenRef    string
	resolver    SecretResolver
	batcher     *Batcher
	clk         clock.Clock
	log         *slog.Logger
	onReconnect func()
	client      *http.Client

	reload        chan struct{}
	reloadPending atomic.Bool

	// Owned by the Run goroutine.
	token      string
	backoff    backoff
	recovering bool

	mu          sync.Mutex
	state       State
	lastConfirm time.Time
	lastErr     string
	lastErrAt   time.Time
	offset      time.Duration
	skewed      bool
}

var _ Output = (*HTTP)(nil)

// NewHTTP validates cfg and returns the output. Nothing is sent before Run.
func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	if cfg.ID == "" {
		return nil, errors.New("output: http output without id")
	}

	fail := func(format string, args ...any) error {
		return fmt.Errorf("output %s: "+format, append([]any{cfg.ID}, args...)...)
	}

	endpoint, err := ingestEndpoint(cfg.URL)
	if err != nil {
		return nil, fail("url: %w", err)
	}

	if err := secrets.Check(cfg.Token); err != nil && !errors.Is(err, secrets.ErrUnavailable) {
		return nil, fail("token: %w", err)
	}

	if cfg.Resolver == nil {
		return nil, fail("no secret resolver")
	}

	if cfg.Batcher == nil {
		return nil, fail("no batcher")
	}

	client, err := NewClient(cfg.Client)
	if err != nil {
		return nil, fail("client: %w", err)
	}

	h := &HTTP{
		id:          cfg.ID,
		endpoint:    endpoint,
		tokenRef:    cfg.Token,
		resolver:    cfg.Resolver,
		batcher:     cfg.Batcher,
		clk:         cfg.Clock,
		log:         cfg.Logger,
		onReconnect: cfg.OnReconnect,
		client:      client,
		reload:      make(chan struct{}, 1),
		backoff:     backoff{rand: cfg.Rand},
		state:       Sending,
	}

	if h.clk == nil {
		h.clk = clock.Real()
	}

	if h.log == nil {
		h.log = slog.Default()
	}

	if h.onReconnect == nil {
		h.onReconnect = func() {}
	}

	if h.backoff.rand == nil {
		h.backoff.rand = rand.Float64
	}

	return h, nil
}

// ingestEndpoint validates the base URL and returns the ingest route below it.
func ingestEndpoint(raw string) (string, error) {
	if err := docrules.CheckURL(raw); err != nil {
		return "", err
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("must be an absolute URL such as https://mes.example.com")
	}

	u.RawQuery, u.Fragment = "", ""

	return strings.TrimRight(u.String(), "/") + ingestPath, nil
}

// NewClient returns the client the agent uses to reach a backend: a copy of
// c that never follows redirects, or, when c is nil, a client with the
// system roots, TLS 1.2 or later and the proxy from the environment. A
// client whose transport skips certificate verification is refused. Only
// tests pass a non-nil c.
func NewClient(c *http.Client) (*http.Client, error) {
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	if c == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = http.ProxyFromEnvironment
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}

		return &http.Client{Transport: t, CheckRedirect: noRedirect}, nil
	}

	if t, ok := c.Transport.(*http.Transport); ok && t.TLSClientConfig != nil && t.TLSClientConfig.InsecureSkipVerify {
		return nil, errors.New("TLS certificate verification cannot be disabled")
	}

	cp := *c
	cp.CheckRedirect = noRedirect

	return &cp, nil
}

// Run sends the batches of the output until ctx is done, then returns nil;
// call it once.
// A batch is settled only from the backend's answer: confirmed on 2xx,
// split on 413, dropped with a rejected_invalid loss marker on 422 or on a
// one-record 413; every other answer and every local failure keeps it in
// flight to be resent identical after a backoff or a suspension.
func (h *HTTP) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if h.reloadPending.Swap(false) {
			h.token = ""
		}

		if h.token == "" && !h.resolveToken(ctx) {
			h.suspend(ctx)

			continue
		}

		b, err := h.batcher.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}

			h.log.Error("output: cannot seal the next batch", "output", h.id, "error", err)
			h.recordError("next batch: " + err.Error())
			h.backOff(ctx, 0)

			continue
		}

		switch res := h.send(ctx, b); res.kind {
		case resultSettled:
			h.setState(Sending)
		case resultRetry:
			h.backOff(ctx, res.retryAfter)
		case resultSuspend:
			h.suspend(ctx)
		}
	}

	return nil
}

// resolveToken resolves the token reference and reports whether a usable
// token is now held. Errors name the reference, never a value.
func (h *HTTP) resolveToken(ctx context.Context) bool {
	token, err := h.resolver.Resolve(ctx, h.tokenRef)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}

		level := slog.LevelWarn
		if errors.Is(err, secrets.ErrUnavailable) {
			level = slog.LevelError
		}

		h.log.Log(ctx, level, "output: cannot resolve the token; sending suspended", "output", h.id, "error", err)
		h.recordError("token unavailable: " + err.Error())

		return false
	}

	if !validHeaderValue(token) {
		h.log.Warn("output: the token is not a valid HTTP header value; sending suspended", "output", h.id, "token_ref", h.tokenRef)
		h.recordError("token unavailable: not a valid HTTP header value")

		return false
	}

	h.token = token

	return true
}

// validHeaderValue reports whether s can be sent in a header unchanged.
func validHeaderValue(s string) bool {
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}

	return s != ""
}

// resultKind is what Run does after a request.
type resultKind int

const (
	// resultSettled: the batch is confirmed, split or dropped; go on at once.
	resultSettled resultKind = iota
	// resultRetry: back off, then resend the batch in flight.
	resultRetry
	// resultSuspend: suspend, then resend with the token resolved again.
	resultSuspend
	// resultStopped: ctx is done.
	resultStopped
)

type result struct {
	kind       resultKind
	retryAfter time.Duration
}

// send posts b and settles it from the answer.
func (h *HTTP) send(ctx context.Context, b *Batch) result {
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost, h.endpoint, bytes.NewReader(b.Body))
	if err != nil {
		h.log.Error("output: cannot build the request", "output", h.id, "error", err)
		h.recordError("request: " + err.Error())

		return result{kind: resultRetry}
	}

	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "laraplate-edge/"+version.String())

	sentAt := h.clk.Now()
	resp, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return result{kind: resultStopped}
		}

		h.log.Warn("output: request failed; backing off", "output", h.id, "seq", b.Seq, "error", err)
		h.recordError("network error: " + err.Error())

		return result{kind: resultRetry}
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	_ = resp.Body.Close()
	recvAt := h.clk.Now()
	h.observeDate(resp.Header.Get("Date"), sentAt, recvAt)
	if readErr != nil {
		h.log.Debug("output: cannot read the response body", "output", h.id, "status", resp.StatusCode, "error", readErr)
	}

	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		return h.confirm(b, code, body)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		h.log.Warn("output: the backend refused the token; sending suspended", "output", h.id, "status", code, "retry_in", authRetry.String())
		h.recordError(fmt.Sprintf("HTTP %d: token refused", code))

		return result{kind: resultSuspend}
	case code == http.StatusRequestEntityTooLarge:
		return h.tooLarge(b)
	case code == http.StatusUnprocessableEntity:
		return h.invalid(b, body)
	}

	wait, _ := retryAfter(resp.Header.Get("Retry-After"), recvAt)
	if code == http.StatusTooManyRequests || code >= 500 {
		h.log.Warn("output: temporary backend error; backing off", "output", h.id, "seq", b.Seq, "status", code)
	} else {
		h.log.Error("output: unexpected backend response; check the output url and that the backend API is enabled", "output", h.id, "seq", b.Seq, "status", code)
	}

	h.recordError(fmt.Sprintf("HTTP %d", code))

	return result{kind: resultRetry, retryAfter: wait}
}

// confirm settles b as delivered after a 2xx.
func (h *HTTP) confirm(b *Batch, code int, body []byte) result {
	contract := code == http.StatusAccepted
	if code == http.StatusOK {
		var answer struct {
			Duplicate bool `json:"duplicate"`
		}

		contract = json.Unmarshal(body, &answer) == nil && answer.Duplicate
	}

	if !contract {
		h.log.Warn("output: unexpected success response; batch confirmed", "output", h.id, "seq", b.Seq, "status", code)
	}

	if err := h.batcher.Confirm(b); err != nil {
		h.log.Error("output: cannot settle a delivered batch; it will be resent", "output", h.id, "seq", b.Seq, "error", err)
		h.recordError("confirm: " + err.Error())

		return result{kind: resultRetry}
	}

	h.log.Debug("output: batch delivered", "output", h.id, "seq", b.Seq, "records", b.Len(), "status", code)

	h.backoff.reset()
	if h.recovering {
		h.recovering = false
		h.onReconnect()
	}

	now := h.clk.Now()
	h.mu.Lock()
	h.lastConfirm, h.state = now, Sending
	h.mu.Unlock()

	return result{kind: resultSettled}
}

// tooLarge answers a 413: split b in two, or drop a one-record batch, which
// cannot be split, with a rejected_invalid loss marker.
func (h *HTTP) tooLarge(b *Batch) result {
	if b.Len() <= 1 {
		h.log.Warn("output: the backend refused a one-record batch as too large; dropping it with a rejected_invalid loss marker", "output", h.id, "seq", b.Seq)
		if err := h.batcher.Drop(b, model.LossRejectedInvalid); err != nil {
			h.log.Error("output: cannot drop the refused batch; it will be resent", "output", h.id, "seq", b.Seq, "error", err)
			h.recordError("drop: " + err.Error())

			return result{kind: resultRetry}
		}

		h.recordError("HTTP 413: one record dropped as rejected_invalid")

		return result{kind: resultSettled}
	}

	n := b.Len() / 2
	if _, err := h.batcher.Shrink(b, n); err != nil {
		h.log.Error("output: cannot split the refused batch; it will be resent", "output", h.id, "seq", b.Seq, "error", err)
		h.recordError("split: " + err.Error())

		return result{kind: resultRetry}
	}

	h.log.Warn("output: the backend refused the batch as too large; split", "output", h.id, "seq", b.Seq, "records", b.Len(), "kept", n)

	return result{kind: resultSettled}
}

// invalid answers a 422: drop b with a rejected_invalid loss marker. The
// field paths of the errors are logged at warn, the body only at debug.
func (h *HTTP) invalid(b *Batch, body []byte) result {
	var answer struct {
		Errors map[string]json.RawMessage `json:"errors"`
	}
	_ = json.Unmarshal(body, &answer)

	fields := make([]string, 0, len(answer.Errors))
	for path := range answer.Errors {
		fields = append(fields, path)
	}

	slices.Sort(fields)

	h.log.Warn("output: the backend refused the batch as invalid; dropping it with a rejected_invalid loss marker", "output", h.id, "seq", b.Seq, "records", b.Len(), "fields", fields)
	h.log.Debug("output: invalid batch response", "output", h.id, "seq", b.Seq, "body", string(body))

	if err := h.batcher.Drop(b, model.LossRejectedInvalid); err != nil {
		h.log.Error("output: cannot drop the refused batch; it will be resent", "output", h.id, "seq", b.Seq, "error", err)
		h.recordError("drop: " + err.Error())

		return result{kind: resultRetry}
	}

	h.recordError("HTTP 422: batch dropped as rejected_invalid")

	return result{kind: resultSettled}
}

// backOff waits the next backoff step, at least retryAfter (capped).
func (h *HTTP) backOff(ctx context.Context, retryAfter time.Duration) {
	wait := max(h.backoff.next(), min(retryAfter, backoffMax))

	h.setState(Backoff)
	h.recovering = true
	h.log.Debug("output: waiting before resending", "output", h.id, "wait", wait.String())
	h.pause(ctx, wait, false)
}

// suspend waits authRetry, or until ReloadCredentials; the token is
// resolved again afterwards.
func (h *HTTP) suspend(ctx context.Context) {
	h.setState(SuspendedAuth)
	h.recovering = true
	h.token = ""
	h.pause(ctx, authRetry, true)
}

// pause waits d on the clock, until ctx is done or, when wakeOnReload,
// until ReloadCredentials is called.
func (h *HTTP) pause(ctx context.Context, d time.Duration, wakeOnReload bool) {
	if wakeOnReload && h.reloadPending.Load() {
		return
	}

	timer := h.clk.NewTimer(d)
	defer timer.Stop()

	var reload <-chan struct{}
	if wakeOnReload {
		reload = h.reload
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			return
		case <-reload:
			if h.reloadPending.Load() {
				return
			}
		}
	}
}

// ReloadCredentials makes the next attempt resolve the token again and ends
// a suspension at once (bootstrap reload, SIGHUP).
func (h *HTTP) ReloadCredentials() {
	h.reloadPending.Store(true)
	select {
	case h.reload <- struct{}{}:
	default:
	}
}

// observeDate updates the clock offset from a response Date header: the
// server time minus the midpoint of the local send and receive times.
func (h *HTTP) observeDate(date string, sentAt, recvAt time.Time) {
	if date == "" {
		return
	}

	server, err := http.ParseTime(date)
	if err != nil {
		return
	}

	offset := server.Sub(sentAt.Add(recvAt.Sub(sentAt) / 2))
	skewed := offset > skewThreshold || offset < -skewThreshold

	h.mu.Lock()
	crossed := skewed && !h.skewed
	h.offset, h.skewed = offset, skewed
	h.mu.Unlock()

	if crossed {
		h.log.Warn("output: the agent clock differs from the backend's; check NTP", "output", h.id, "offset", offset.String(), "threshold", skewThreshold.String())
	}
}

func (h *HTTP) setState(s State) {
	h.mu.Lock()
	h.state = s
	h.mu.Unlock()
}

func (h *HTTP) recordError(msg string) {
	now := h.clk.Now()

	h.mu.Lock()
	h.lastErr, h.lastErrAt = msg, now
	h.mu.Unlock()
}

// State returns the current sending state.
func (h *HTTP) State() State {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.state
}

// ClockOffset returns the backend clock minus the agent clock estimated
// from the Date header of the last response, zero before any (ruling R4).
func (h *HTTP) ClockOffset() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.offset
}

// LastConfirmation returns when the last batch was confirmed, zero before any.
func (h *HTTP) LastConfirmation() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.lastConfirm
}

// LastError returns the last error, a short text without secrets, and when
// it happened; empty before any.
func (h *HTTP) LastError() (string, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.lastErr, h.lastErrAt
}
