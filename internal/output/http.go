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
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/docrules"
	"github.com/getlaraplate/laraedge/internal/secrets"
	"github.com/getlaraplate/laraedge/internal/version"
)

const (
	// ingestPath is the backend route of machine messages, below the URL.
	ingestPath = "/api/v1/insert/mes/machine-messages"
	// requestTimeout bounds one request, response body included.
	requestTimeout = 30 * time.Second
	// skewThreshold is the clock offset above which a warning is logged.
	skewThreshold = 2 * time.Second
	// maxResponseBody bounds how much of a response is read.
	maxResponseBody = 64 << 10
)

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

// HTTP is the laraplate-http output: a Loop over the sealed batches of its
// Batcher whose Sender posts each one to the backend ingest route and maps
// the response to an Outcome (spec 6.5).
type HTTP struct {
	sender *httpSender
	loop   *Loop
}

var _ Output = (*HTTP)(nil)

// httpSender is the Sender of an HTTP output.
type httpSender struct {
	id       string
	endpoint string
	tokenRef string
	resolver SecretResolver
	clk      clock.Clock
	log      *slog.Logger
	client   *http.Client

	reloadPending atomic.Bool

	// Owned by the Run goroutine.
	token string

	mu     sync.Mutex
	offset time.Duration
	skewed bool
}

var _ Sender = (*httpSender)(nil)

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

	clk, log := cfg.Clock, cfg.Logger
	if clk == nil {
		clk = clock.Real()
	}

	if log == nil {
		log = slog.Default()
	}

	s := &httpSender{
		id:       cfg.ID,
		endpoint: endpoint,
		tokenRef: cfg.Token,
		resolver: cfg.Resolver,
		clk:      clk,
		log:      log,
		client:   client,
	}

	loop := NewLoop(LoopConfig{
		ID:          cfg.ID,
		Sender:      s,
		Batcher:     cfg.Batcher,
		Clock:       clk,
		Logger:      log,
		OnReconnect: cfg.OnReconnect,
		Rand:        cfg.Rand,
	})

	return &HTTP{sender: s, loop: loop}, nil
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

	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false

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
	return h.loop.Run(ctx)
}

// ReloadCredentials makes the next attempt resolve the token again and ends
// a suspension at once (bootstrap reload, SIGHUP).
func (h *HTTP) ReloadCredentials() {
	h.loop.ReloadCredentials()
}

// State returns the current sending state.
func (h *HTTP) State() State {
	return h.loop.State()
}

// ClockOffset returns the backend clock minus the agent clock estimated
// from the Date header of the last response, zero before any (ruling R4).
func (h *HTTP) ClockOffset() time.Duration {
	h.sender.mu.Lock()
	defer h.sender.mu.Unlock()

	return h.sender.offset
}

// LastConfirmation returns when the last batch was confirmed, zero before any.
func (h *HTTP) LastConfirmation() time.Time {
	return h.loop.LastConfirmation()
}

// LastError returns the last error, a short text without secrets, and when
// it happened; empty before any.
func (h *HTTP) LastError() (string, time.Time) {
	return h.loop.LastError()
}

// prepare is the token step before the next batch is sealed: after
// ReloadCredentials or a suspension the token is resolved again, and an
// output whose token cannot be resolved suspends without sealing a batch.
func (s *httpSender) prepare(ctx context.Context) (Outcome, bool) {
	if s.reloadPending.Swap(false) {
		s.token = ""
	}

	return s.tokenStep(ctx)
}

// tokenStep resolves the token when none is held; a failure suspends.
func (s *httpSender) tokenStep(ctx context.Context) (Outcome, bool) {
	if s.token != "" {
		return Outcome{}, true
	}

	if reason, ok := s.resolveToken(ctx); !ok {
		return Outcome{Kind: Suspend, Reason: reason}, false
	}

	return Outcome{}, true
}

// ReloadCredentials makes the next attempt resolve the token again.
func (s *httpSender) ReloadCredentials() {
	s.reloadPending.Store(true)
}

// resolveToken resolves the token reference and reports whether a usable
// token is now held, or the error to record. Errors name the reference,
// never a value.
func (s *httpSender) resolveToken(ctx context.Context) (string, bool) {
	token, err := s.resolver.Resolve(ctx, s.tokenRef)
	if err != nil {
		if ctx.Err() != nil {
			return "", false
		}

		level := slog.LevelWarn
		if errors.Is(err, secrets.ErrUnavailable) {
			level = slog.LevelError
		}

		s.log.Log(ctx, level, "output: cannot resolve the token; sending suspended", "output", s.id, "error", err)

		return "token unavailable: " + err.Error(), false
	}

	if !validHeaderValue(token) {
		s.log.Warn("output: the token is not a valid HTTP header value; sending suspended", "output", s.id, "token_ref", s.tokenRef)

		return "token unavailable: not a valid HTTP header value", false
	}

	s.token = token

	return "", true
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

// Send posts b and maps the answer to an Outcome: 2xx Confirmed, 401 and
// 403 Suspend, 413 Split, 422 Invalid, anything else and every local
// failure Retry, with the Retry-After of the response.
func (s *httpSender) Send(ctx context.Context, b *Batch) Outcome {
	if out, ok := s.tokenStep(ctx); !ok {
		return out
	}

	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost, s.endpoint, bytes.NewReader(b.Body))
	if err != nil {
		s.log.Error("output: cannot build the request", "output", s.id, "error", err)

		return Outcome{Kind: Retry, Reason: "request: " + err.Error()}
	}

	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "laraplate-edge/"+version.String())

	sentAt := s.clk.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Outcome{Kind: Retry}
		}

		s.log.Warn("output: request failed; backing off", "output", s.id, "seq", b.Seq, "error", err)

		return Outcome{Kind: Retry, Reason: "network error: " + err.Error()}
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	_ = resp.Body.Close()
	recvAt := s.clk.Now()
	s.observeDate(resp.Header.Get("Date"), sentAt, recvAt)
	if readErr != nil {
		s.log.Debug("output: cannot read the response body", "output", s.id, "status", resp.StatusCode, "error", readErr)
	}

	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		s.confirmed(b, code, body)

		return Outcome{Kind: Confirmed}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		s.log.Warn("output: the backend refused the token; sending suspended", "output", s.id, "status", code, "retry_in", authRetry.String())
		s.token = ""

		return Outcome{Kind: Suspend, Reason: fmt.Sprintf("HTTP %d: token refused", code)}
	case code == http.StatusRequestEntityTooLarge:
		return Outcome{Kind: Split, Reason: "HTTP 413"}
	case code == http.StatusUnprocessableEntity:
		s.invalid(b, body)

		return Outcome{Kind: Invalid, Reason: "HTTP 422"}
	}

	wait, _ := retryAfter(resp.Header.Get("Retry-After"), recvAt)
	if code == http.StatusTooManyRequests || code >= 500 {
		s.log.Warn("output: temporary backend error; backing off", "output", s.id, "seq", b.Seq, "status", code)
	} else {
		s.log.Error("output: unexpected backend response; check the output url and that the backend API is enabled", "output", s.id, "seq", b.Seq, "status", code)
	}

	return Outcome{Kind: Retry, RetryAfter: wait, Reason: fmt.Sprintf("HTTP %d", code)}
}

// confirmed checks a 2xx against the contract, 202 or a 200 duplicate, and
// warns about any other; the batch is confirmed either way.
func (s *httpSender) confirmed(b *Batch, code int, body []byte) {
	contract := code == http.StatusAccepted
	if code == http.StatusOK {
		var answer struct {
			Duplicate bool `json:"duplicate"`
		}

		contract = json.Unmarshal(body, &answer) == nil && answer.Duplicate
	}

	if !contract {
		s.log.Warn("output: unexpected success response; batch confirmed", "output", s.id, "seq", b.Seq, "status", code)
	}
}

// invalid logs a 422: the field paths of the errors at warn, the body only
// at debug. The Loop drops the batch.
func (s *httpSender) invalid(b *Batch, body []byte) {
	var answer struct {
		Errors map[string]json.RawMessage `json:"errors"`
	}
	_ = json.Unmarshal(body, &answer)

	fields := make([]string, 0, len(answer.Errors))
	for path := range answer.Errors {
		fields = append(fields, path)
	}

	slices.Sort(fields)

	s.log.Warn("output: the backend refused the batch as invalid; dropping it with a rejected_invalid loss marker", "output", s.id, "seq", b.Seq, "records", b.Len(), "fields", fields)
	s.log.Debug("output: invalid batch response", "output", s.id, "seq", b.Seq, "body", string(body))
}

// observeDate updates the clock offset from a response Date header: the
// server time minus the midpoint of the local send and receive times.
func (s *httpSender) observeDate(date string, sentAt, recvAt time.Time) {
	if date == "" {
		return
	}

	server, err := http.ParseTime(date)
	if err != nil {
		return
	}

	offset := server.Sub(sentAt.Add(recvAt.Sub(sentAt) / 2))
	skewed := offset > skewThreshold || offset < -skewThreshold

	s.mu.Lock()
	crossed := skewed && !s.skewed
	s.offset, s.skewed = offset, skewed
	s.mu.Unlock()

	if crossed {
		s.log.Warn("output: the agent clock differs from the backend's; check NTP", "output", s.id, "offset", offset.String(), "threshold", skewThreshold.String())
	}
}
