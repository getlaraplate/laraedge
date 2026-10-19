package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/swolley/laraplate-edge/internal/atomicfile"
	"github.com/swolley/laraplate-edge/internal/bootstrap"
	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/docrules"
	"github.com/swolley/laraplate-edge/internal/output"
	"github.com/swolley/laraplate-edge/internal/secrets"
	"github.com/swolley/laraplate-edge/internal/supervisor"
	"github.com/swolley/laraplate-edge/internal/version"
)

// Backend limits of a configuration report (MachineAgentConfigService).
const (
	// MaxReportErrors is the most error messages a report carries.
	MaxReportErrors = 100
	// MaxReportErrorLength is the most characters of one message.
	MaxReportErrorLength = 1000
)

const (
	configRoute    = "/api/v1/detail/mes/machine-agent-configs"
	reportRoute    = "/api/v1/insert/mes/machine-agent-config-reports"
	stateName      = "laraplate-state.json"
	requestTimeout = 30 * time.Second
	// maxConfigResponse bounds a configuration response: the document, at
	// most config.MaxDocumentSize, and its envelope, with room for the
	// backend's own formatting.
	maxConfigResponse = 2 * config.MaxDocumentSize
	// maxPendingReports bounds the reports kept while the backend refuses
	// them; the oldest is dropped first.
	maxPendingReports = 16
)

// LaraplateConfig configures a Laraplate provider.
type LaraplateConfig struct {
	// URL is the backend base URL (bootstrap config.laraplate.url).
	URL string
	// Token is the secret:// reference of the agent token.
	Token string
	// PollInterval is config.laraplate.poll_seconds; zero means the
	// bootstrap default.
	PollInterval time.Duration
	// Resolver resolves Token at every poll, so a rotated secret is used at
	// once.
	Resolver output.SecretResolver
	// Applier applies the documents.
	Applier Applier
	// DataDir holds config/laraplate-state.json.
	DataDir string
	// Clock drives the polling; nil means clock.Real().
	Clock clock.Clock
	// Logger receives the logs; nil means slog.Default().
	Logger *slog.Logger
	// Client is a test seam; production code leaves it nil for the
	// verifying default client of output.NewClient.
	Client *http.Client
	// Attempted, when set, is called once, from Run, when the first poll
	// ended, whatever its outcome (systemd readiness).
	Attempted func()
}

// Laraplate polls the backend for the active configuration of this agent's
// source every poll interval, from the start (spec 5.4, 5.5). It asks with
// ?version=N, N the highest of the applied version and the last attempted
// one when that attempt was not applied (rulings R32, R45), so the backend
// answers 304 while nothing changed. A new document is applied and its
// outcome reported, with the errors and the agent version.
//
// data_dir/config/laraplate-state.json keeps, atomically, the last attempt
// with its outcome and the reports not yet delivered: a rejected or rolled
// back version is neither applied nor reported again, also after a restart;
// an applied version the agent no longer runs after a restart is downloaded
// again; an undelivered report is sent at every poll, from the first one
// after a restart, until the backend records it (2xx) or refuses it (422).
// An apply cut short by the shutdown is not an attempt and records nothing.
type Laraplate struct {
	configURL string
	reportURL string
	tokenRef  string
	resolver  output.SecretResolver
	applier   Applier
	dir       string
	clk       clock.Clock
	log       *slog.Logger
	client    *http.Client
	interval  time.Duration
	attempted func()

	// Owned by Run.
	lastAttempt attempt
	pending     []report
}

// attempt is the last version the provider tried to apply and its outcome.
type attempt struct {
	Version int64  `json:"version"`
	Outcome string `json:"outcome"`
}

// state is what the provider persists.
type state struct {
	LastAttempt    *attempt `json:"last_attempt,omitempty"`
	PendingReports []report `json:"pending_reports"`
}

// report is the body of a configuration report.
type report struct {
	Version      int64    `json:"version"`
	Outcome      string   `json:"outcome"`
	Errors       []string `json:"errors"`
	AgentVersion string   `json:"agent_version"`
}

// NewLaraplate validates cfg and reads the last attempted version.
func NewLaraplate(cfg LaraplateConfig) (*Laraplate, error) {
	base, err := baseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("provider: config.laraplate.url: %w", err)
	}
	if err := secrets.Check(cfg.Token); err != nil {
		return nil, fmt.Errorf("provider: config.laraplate.token: %w", err)
	}
	if cfg.Resolver == nil || cfg.Applier == nil || cfg.DataDir == "" {
		return nil, errors.New("provider: laraplate provider without resolver, applier or data directory")
	}
	client, err := output.NewClient(cfg.Client)
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}

	l := &Laraplate{
		configURL: base + configRoute,
		reportURL: base + reportRoute,
		tokenRef:  cfg.Token,
		resolver:  cfg.Resolver,
		applier:   cfg.Applier,
		dir:       filepath.Join(cfg.DataDir, "config"),
		clk:       cfg.Clock,
		log:       cfg.Logger,
		client:    client,
		interval:  cfg.PollInterval,
		attempted: cfg.Attempted,
	}
	if l.clk == nil {
		l.clk = clock.Real()
	}
	if l.log == nil {
		l.log = slog.Default()
	}
	if l.interval <= 0 {
		l.interval = bootstrap.DefaultPollSeconds * time.Second
	}

	if err := l.readState(); err != nil {
		l.log.Warn("provider: cannot read the provider state; starting from the applied configuration without pending reports", "error", err)
	}

	return l, nil
}

// baseURL validates raw with the backend URL rule and returns it without
// query, fragment or trailing slash.
func baseURL(raw string) (string, error) {
	if err := docrules.CheckURL(raw); err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("must be an absolute URL such as https://mes.example.com")
	}
	u.RawQuery, u.Fragment, u.ForceQuery = "", "", false

	return strings.TrimRight(u.String(), "/"), nil
}

// Run polls at once, then every poll interval, until ctx is done; it
// returns nil.
func (l *Laraplate) Run(ctx context.Context) error {
	attempted := l.attempted
	for {
		l.poll(ctx)
		if attempted != nil {
			attempted()
			attempted = nil
		}

		timer := l.clk.NewTimer(l.interval)
		select {
		case <-ctx.Done():
			timer.Stop()

			return nil
		case <-timer.C():
		}
	}
}

// poll sends the pending reports, then asks for the configuration.
func (l *Laraplate) poll(ctx context.Context) {
	token, err := l.resolver.Resolve(ctx, l.tokenRef)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("provider: cannot resolve the agent token; configuration not polled", "error", err)
		}

		return
	}
	if !validHeaderValue(token) {
		l.log.Warn("provider: the agent token is not a valid HTTP header value; configuration not polled", "token_ref", l.tokenRef)

		return
	}

	l.sendReports(ctx, token)

	target := l.configURL
	n := l.applier.AppliedVersion()
	if l.lastAttempt.Outcome != supervisor.ResultApplied {
		n = max(n, l.lastAttempt.Version)
	}
	if n > 0 {
		target += "?version=" + strconv.FormatInt(n, 10)
	}

	status, body, err := l.do(ctx, http.MethodGet, target, nil, token)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Warn("provider: configuration request failed", "error", err)
		}

		return
	}

	switch {
	case status == http.StatusNotModified:
		l.log.Debug("provider: configuration unchanged")
	case status == http.StatusOK:
		l.handle(ctx, token, body)
	case status == http.StatusNotFound:
		l.log.Info("provider: the backend has no active configuration for this agent")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		l.log.Warn("provider: the backend refused the agent token or its permissions; trying again at the next poll", "status", status)
	default:
		l.log.Warn("provider: unexpected configuration response; trying again at the next poll", "status", status)
	}
}

// handle applies a downloaded document and reports the outcome, unless its
// version is the one last attempted.
func (l *Laraplate) handle(ctx context.Context, token string, body []byte) {
	var answer struct {
		Version  int64           `json:"version"`
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Version < 1 || len(answer.Document) == 0 {
		l.log.Warn("provider: malformed configuration response; trying again at the next poll")

		return
	}

	if answer.Version == l.lastAttempt.Version && l.lastAttempt.Outcome != supervisor.ResultApplied {
		l.log.Debug("provider: configuration version already attempted", "version", answer.Version, "outcome", l.lastAttempt.Outcome)

		return
	}

	out, ok := l.apply(ctx, answer.Version, answer.Document)
	if !ok {
		return
	}

	l.lastAttempt = attempt{Version: answer.Version, Outcome: out.Result}
	l.pending = append(l.pending, report{
		Version:      answer.Version,
		Outcome:      out.Result,
		Errors:       capErrors(out.Errors),
		AgentVersion: version.String(),
	})
	if n := len(l.pending) - maxPendingReports; n > 0 {
		l.log.Warn("provider: too many undelivered configuration reports; dropping the oldest", "dropped", n)
		l.pending = l.pending[n:]
	}
	l.saveState()
	l.sendReports(ctx, token)
}

// apply turns the downloaded document into a Document and applies it. The
// published version is authoritative: it is written into a document
// without one, and a document carrying another one is rejected. ok is false
// when nothing was attempted (the supervisor is closing).
func (l *Laraplate) apply(ctx context.Context, published int64, raw json.RawMessage) (supervisor.Outcome, bool) {
	rejected := func(msgs ...string) (supervisor.Outcome, bool) {
		out := supervisor.Outcome{Result: supervisor.ResultRejected, Errors: msgs}
		logOutcome(l.log, "laraplate", published, out, nil)

		return out, true
	}

	var tree map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tree); err != nil || tree == nil {
		return rejected("document: must be an object")
	}
	if v, ok := tree["version"]; ok {
		var n int64
		if err := json.Unmarshal(v, &n); err == nil && n != published {
			return rejected(fmt.Sprintf("version: differs from the published version %d", published))
		}
	} else {
		tree["version"] = json.RawMessage(strconv.FormatInt(published, 10))
	}

	data, err := json.Marshal(tree)
	if err != nil {
		return rejected("document: cannot be encoded")
	}
	doc, msgs, err := config.Parse(data)
	if err != nil {
		l.log.Error("provider: cannot parse the configuration", "version", published, "error", err)

		return rejected("document: internal error while parsing")
	}
	if len(msgs) > 0 {
		return rejected(msgs...)
	}

	out, err := l.applier.Apply(ctx, doc)
	if out.Result == "" {
		// The supervisor's signal of an apply cut short by the shutdown: the
		// document had no chance, so nothing is recorded and it is applied
		// after the restart. An outcome is an attempt even when the context
		// is done by now: it is recorded and reported (after the restart if
		// need be).
		l.log.Warn("provider: configuration apply interrupted; it will be attempted again", "version", published, "error", err)

		return out, false
	}
	logOutcome(l.log, "laraplate", published, out, err)

	return out, true
}

// sendReports posts the pending reports in order and stops at the first one
// not delivered. A 422 can never succeed: that report is dropped. The state
// is saved when the list changed.
func (l *Laraplate) sendReports(ctx context.Context, token string) {
	before := len(l.pending)
	defer func() {
		if len(l.pending) != before {
			l.saveState()
		}
	}()

	for len(l.pending) > 0 {
		r := l.pending[0]
		body, err := json.Marshal(r)
		if err != nil {
			l.log.Error("provider: cannot encode a configuration report", "error", err)
			l.pending = l.pending[1:]

			continue
		}

		status, _, err := l.do(ctx, http.MethodPost, l.reportURL, body, token)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				l.log.Warn("provider: cannot send the configuration report; sending it again at the next poll", "version", r.Version, "error", err)
			}

			return
		case status >= 200 && status < 300:
			l.log.Info("provider: configuration report sent", "version", r.Version, "outcome", r.Outcome)
		case status == http.StatusUnprocessableEntity:
			l.log.Warn("provider: the backend refused the configuration report as invalid; dropping it", "version", r.Version, "outcome", r.Outcome)
		default:
			l.log.Warn("provider: the backend did not record the configuration report; sending it again at the next poll", "version", r.Version, "status", status)

			return
		}
		l.pending = l.pending[1:]
	}
}

// do sends one request and returns the status and at most maxConfigResponse
// bytes of the body.
func (l *Laraplate) do(ctx context.Context, method, target string, body []byte, token string) (int, []byte, error) {
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, target, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "laraplate-edge/"+version.String())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigResponse+1))
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxConfigResponse {
		return 0, nil, fmt.Errorf("response larger than %d bytes", maxConfigResponse)
	}

	return resp.StatusCode, data, nil
}

// readState loads the persisted state; a missing file is an empty state.
func (l *Laraplate) readState() error {
	data, err := os.ReadFile(filepath.Join(l.dir, stateName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("%s is corrupt", filepath.Join(l.dir, stateName))
	}
	if st.LastAttempt != nil {
		l.lastAttempt = *st.LastAttempt
	}
	l.pending = st.PendingReports

	return nil
}

// saveState persists the last attempt and the pending reports atomically. A
// failure is logged; the state in memory stays authoritative until the next
// save.
func (l *Laraplate) saveState() {
	st := state{PendingReports: l.pending}
	if l.lastAttempt.Version > 0 {
		la := l.lastAttempt
		st.LastAttempt = &la
	}
	if st.PendingReports == nil {
		st.PendingReports = []report{}
	}

	data, err := json.Marshal(st)
	if err == nil {
		if err = os.MkdirAll(l.dir, 0o700); err == nil {
			err = atomicfile.Write(l.dir, stateName, data)
		}
	}
	if err != nil {
		l.log.Warn("provider: cannot persist the provider state", "error", err)
	}
}

// capErrors keeps the first MaxReportErrors messages, each cut to
// MaxReportErrorLength characters (the backend counts characters).
func capErrors(errs []string) []string {
	if len(errs) == 0 {
		return nil
	}

	out := make([]string, 0, min(len(errs), MaxReportErrors))
	for _, e := range errs[:min(len(errs), MaxReportErrors)] {
		e = strings.ToValidUTF8(e, "�")
		if runes := []rune(e); len(runes) > MaxReportErrorLength {
			e = string(runes[:MaxReportErrorLength])
		}
		out = append(out, e)
	}

	return out
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
