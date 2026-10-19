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
	configRoute     = "/api/v1/detail/mes/machine-agent-configs"
	reportRoute     = "/api/v1/insert/mes/machine-agent-config-reports"
	lastAttemptName = "last-attempt"
	requestTimeout  = 30 * time.Second
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
	// DataDir holds config/last-attempt.
	DataDir string
	// Clock drives the polling; nil means clock.Real().
	Clock clock.Clock
	// Logger receives the logs; nil means slog.Default().
	Logger *slog.Logger
	// Client is a test seam; production code leaves it nil for the
	// verifying default client of output.NewClient.
	Client *http.Client
}

// Laraplate polls the backend for the active configuration of this agent's
// source every poll interval, from the start (spec 5.4, 5.5). It asks with
// ?version=N, N the highest of the applied version and the last attempted
// one (ruling R32), so the backend answers 304 while nothing changed. A new
// document is applied and its outcome reported, with the errors and the agent
// version; a report that cannot be delivered is sent again at the next poll.
// The last attempted version is kept in data_dir/config/last-attempt, so a
// rejected or rolled back version is neither applied nor reported again,
// also after a restart.
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

	// Owned by Run.
	lastAttempt int64
	pending     []report
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

	l.lastAttempt, err = l.readLastAttempt()
	if err != nil {
		l.log.Warn("provider: cannot read the last attempted configuration version; starting from the applied one", "error", err)
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
	for {
		l.poll(ctx)

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
	if n := max(l.applier.AppliedVersion(), l.lastAttempt); n > 0 {
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

	if answer.Version == l.lastAttempt {
		l.log.Debug("provider: configuration version already attempted", "version", answer.Version)

		return
	}

	out, ok := l.apply(ctx, answer.Version, answer.Document)
	if !ok {
		return
	}

	l.lastAttempt = answer.Version
	if err := l.writeLastAttempt(answer.Version); err != nil {
		l.log.Warn("provider: cannot persist the last attempted configuration version", "version", answer.Version, "error", err)
	}

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
		l.log.Error("provider: configuration not applied", "version", published, "error", err)

		return out, false
	}
	logOutcome(l.log, "laraplate", published, out, err)

	return out, true
}

// sendReports posts the pending reports in order and stops at the first one
// not delivered. A 422 can never succeed: that report is dropped.
func (l *Laraplate) sendReports(ctx context.Context, token string) {
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

func (l *Laraplate) readLastAttempt() (int64, error) {
	data, err := os.ReadFile(filepath.Join(l.dir, lastAttemptName))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s does not hold a version", filepath.Join(l.dir, lastAttemptName))
	}

	return n, nil
}

func (l *Laraplate) writeLastAttempt(v int64) error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}

	return atomicfile.Write(l.dir, lastAttemptName, []byte(strconv.FormatInt(v, 10)+"\n"))
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
