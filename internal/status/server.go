// Package status serves the local, read-only status endpoint of the agent
// (spec 10.3): /healthz, /readyz, /status (JSON) and /metrics (Prometheus
// text). It binds a loopback address by default; binding any other address
// requires a bearer token, read through a secret:// reference, which every
// endpoint but /healthz then asks for.
package status

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/buffer"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/plugin"
	"github.com/getlaraplate/laraedge/internal/supervisor"
)

const (
	// readHeaderTimeout bounds how long a client may take to send its
	// request headers.
	readHeaderTimeout = 5 * time.Second
	// writeTimeout bounds one response.
	writeTimeout = 10 * time.Second
	// idleTimeout closes idle keep-alive connections.
	idleTimeout = 60 * time.Second
	// shutdownTimeout bounds the wait for the requests in progress at stop.
	shutdownTimeout = 5 * time.Second
	// maxHeaderBytes bounds the request headers.
	maxHeaderBytes = 8 << 10
)

// Source gives the state the endpoint reports (*supervisor.Supervisor).
type Source interface {
	Status() supervisor.Status
}

// Resolver resolves a secret:// reference (*secrets.Resolver).
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Config configures a Server.
type Config struct {
	// Listen is the host:port to bind (bootstrap status_listen).
	Listen string
	// TokenRef is the secret:// reference of the read-only bearer token
	// (bootstrap status_token); empty means no token. It is resolved once,
	// at Listen. It is required when Listen is not a loopback address.
	TokenRef string
	// Resolver resolves TokenRef.
	Resolver Resolver
	// AgentID and Version identify the agent in /status.
	AgentID string
	Version string
	// Source is the supervisor.
	Source Source
	// Clock gives the uptime and the age of the buffered records; nil means
	// clock.Real().
	Clock clock.Clock
	// Logger receives the logs; nil means slog.Default().
	Logger *slog.Logger
}

// Server is a bound status endpoint. Serve answers the requests until its
// context is done; Close releases a server that is never served.
type Server struct {
	ln      net.Listener
	http    *http.Server
	token   []byte
	agentID string
	version string
	source  Source
	clk     clock.Clock
	log     *slog.Logger
	started time.Time
}

// Listen resolves the token and binds cfg.Listen. It refuses a non-loopback
// address without a token, and a token reference that does not resolve to
// a value; the errors never hold a token value.
func Listen(ctx context.Context, cfg Config) (*Server, error) {
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("status_listen %q: must be host:port", cfg.Listen)
	}
	if cfg.Source == nil {
		return nil, errors.New("status: no source")
	}

	var token string
	if cfg.TokenRef != "" {
		if cfg.Resolver == nil {
			return nil, errors.New("status_token: no resolver")
		}
		token, err = cfg.Resolver.Resolve(ctx, cfg.TokenRef)
		if err != nil {
			return nil, fmt.Errorf("status_token %s: %w", cfg.TokenRef, err)
		}
		if token == "" {
			return nil, fmt.Errorf("status_token %s: the secret is empty", cfg.TokenRef)
		}
	}
	if token == "" && !isLoopbackHost(host) {
		return nil, fmt.Errorf("status_listen %s is not a loopback address: set status_token (a secret:// reference) to protect the status endpoint, or bind 127.0.0.1", cfg.Listen)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("status_listen %s: %w", cfg.Listen, err)
	}
	if token == "" {
		// A loopback name must have bound a loopback address.
		if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
			ln.Close()

			return nil, fmt.Errorf("status_listen %s bound %s, not a loopback address: set status_token", cfg.Listen, ln.Addr())
		}
	}

	s := &Server{
		ln:      ln,
		token:   []byte(token),
		agentID: cfg.AgentID,
		version: cfg.Version,
		source:  cfg.Source,
		clk:     cfg.Clock,
		log:     cfg.Logger,
	}
	if s.clk == nil {
		s.clk = clock.Real()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.started = s.clk.Now()
	s.http = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}

	return s, nil
}

// isLoopbackHost reports whether host names the loopback interface: a
// loopback IP or localhost. An empty host means every interface.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// Addr returns the bound address.
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

// Serve answers requests until ctx is done, then stops accepting, waits a
// bounded time for the requests in progress and returns. It returns an
// error only when serving fails before ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	served := make(chan error, 1)
	go func() { served <- s.http.Serve(s.ln) }()

	select {
	case err := <-served:
		return fmt.Errorf("status endpoint: %w", err)
	case <-ctx.Done():
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := s.http.Shutdown(stopCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		err = s.http.Close()
	}
	<-served

	if err != nil {
		return fmt.Errorf("status endpoint: stop: %w", err)
	}

	return nil
}

// Close releases the listener of a server that Serve never ran.
func (s *Server) Close() error {
	err := s.ln.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("status endpoint: %w", err)
	}

	return nil
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.authorized(s.readyz))
	mux.HandleFunc("/status", s.authorized(s.status))
	mux.HandleFunc("/metrics", s.authorized(s.metrics))

	return readOnly(mux)
}

// readOnly answers 405 to every method but GET and HEAD.
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorized asks for the bearer token, when one is configured, before
// next. The comparison takes the same time whatever the presented token.
func (s *Server) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.token) > 0 {
			presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(presented), s.token) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)

				return
			}
		}
		next(w, r)
	}
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// readiness is the body of /readyz.
type readiness struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	st := s.source.Status()

	answer := readiness{Ready: true}
	switch {
	case !st.Applied:
		answer = readiness{Reason: "no configuration applied"}
	default:
		for _, in := range st.Inputs {
			if in.State != supervisor.InputRunning {
				answer = readiness{Reason: fmt.Sprintf("input %q is %s", in.ID, in.State)}

				break
			}
		}
	}

	code := http.StatusOK
	if !answer.Ready {
		code = http.StatusServiceUnavailable
	}
	s.writeJSON(w, code, answer)
}

// statusDoc is the body of /status.
type statusDoc struct {
	AgentID       string         `json:"agent_id"`
	AgentVersion  string         `json:"agent_version"`
	UptimeSeconds float64        `json:"uptime_seconds"`
	ConfigApplied bool           `json:"config_applied"`
	ConfigVersion int64          `json:"config_version"`
	Inputs        []inputDoc     `json:"inputs"`
	Outputs       []outputDoc    `json:"outputs"`
	Plugins       []pluginDoc    `json:"plugins"`
	Processors    []processorDoc `json:"processors"`
}

// processorDoc is a processor of the applied document. Refused and Dropped
// count since the processor instance started.
type processorDoc struct {
	ID        string `json:"id"`
	Plugin    string `json:"plugin"`
	State     string `json:"state"`
	Refused   uint64 `json:"refused"`
	Dropped   uint64 `json:"dropped"`
	LastError string `json:"last_error,omitempty"`
}

// pluginDoc is a plugin the applied document uses. Restarts is the total;
// the split by reason is in /metrics.
type pluginDoc struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Pid       int    `json:"pid"`
	Restarts  uint64 `json:"restarts"`
	LastError string `json:"last_error,omitempty"`
}

type inputDoc struct {
	ID              string `json:"id"`
	Driver          string `json:"driver"`
	State           string `json:"state"`
	LastError       string `json:"last_error,omitempty"`
	TransformErrors uint64 `json:"transform_errors"`
	// Connected and ReadErrors are present only for inputs that report
	// their health.
	Connected  *bool             `json:"connected,omitempty"`
	ReadErrors map[string]uint64 `json:"read_errors,omitempty"`
}

type outputDoc struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	State string `json:"state"`
	// ClockOffsetSeconds is absent for an output of type plugin:<id>,
	// which has no clock offset.
	ClockOffsetSeconds *float64          `json:"clock_offset_seconds,omitempty"`
	Buffer             bufferDoc         `json:"buffer"`
	Dropped            map[string]uint64 `json:"dropped"`
	CorruptRecords     uint64            `json:"corrupt_records"`
	LastConfirmation   *time.Time        `json:"last_confirmation"`
	LastError          string            `json:"last_error,omitempty"`
	LastErrorAt        *time.Time        `json:"last_error_at"`
}

type bufferDoc struct {
	Critical queueDoc `json:"critical"`
	Bulk     queueDoc `json:"bulk"`
}

type queueDoc struct {
	Records                     uint64  `json:"records"`
	Bytes                       int64   `json:"bytes"`
	OldestUnconfirmedAgeSeconds float64 `json:"oldest_unconfirmed_age_seconds"`
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	st := s.source.Status()
	now := s.clk.Now()

	doc := statusDoc{
		AgentID:       s.agentID,
		AgentVersion:  s.version,
		UptimeSeconds: now.Sub(s.started).Seconds(),
		ConfigApplied: st.Applied,
		ConfigVersion: st.Version,
		Inputs:        make([]inputDoc, 0, len(st.Inputs)),
		Outputs:       make([]outputDoc, 0, len(st.Outputs)),
		Plugins:       make([]pluginDoc, 0, len(st.Plugins)),
		Processors:    make([]processorDoc, 0, len(st.Processors)),
	}
	for _, in := range st.Inputs {
		d := inputDoc{
			ID: in.ID, Driver: in.Driver, State: in.State, LastError: in.LastError,
			TransformErrors: in.TransformErrors,
		}
		if in.Connected != nil {
			d.Connected = in.Connected
			d.ReadErrors = make(map[string]uint64)
			for _, kind := range readErrorKinds(in.ReadErrors) {
				d.ReadErrors[kind] = in.ReadErrors[kind]
			}
		}
		doc.Inputs = append(doc.Inputs, d)
	}
	for _, out := range st.Outputs {
		dropped := make(map[string]uint64, len(out.Buffer.Dropped))
		for reason, n := range out.Buffer.Dropped {
			dropped[reason] = n
		}
		var offset *float64
		if hasClockOffset(out) {
			seconds := out.ClockOffset.Seconds()
			offset = &seconds
		}
		doc.Outputs = append(doc.Outputs, outputDoc{
			ID:                 out.ID,
			Type:               out.Type,
			State:              out.State,
			ClockOffsetSeconds: offset,
			Buffer: bufferDoc{
				Critical: queueOf(out.Buffer.Critical, now),
				Bulk:     queueOf(out.Buffer.Bulk, now),
			},
			Dropped:          dropped,
			CorruptRecords:   out.Buffer.CorruptRecords,
			LastConfirmation: timeOrNil(out.LastConfirmation),
			LastError:        out.LastError,
			LastErrorAt:      timeOrNil(out.LastErrorAt),
		})
	}

	for _, p := range st.Plugins {
		doc.Plugins = append(doc.Plugins, pluginDoc{ID: p.ID, State: p.State, Pid: p.Pid, Restarts: p.Restarts, LastError: p.LastError})
	}
	for _, p := range st.Processors {
		doc.Processors = append(doc.Processors, processorDoc{
			ID: p.ID, Plugin: p.Plugin, State: p.State, Refused: p.Refused, Dropped: p.Dropped, LastError: p.LastError,
		})
	}

	s.writeJSON(w, http.StatusOK, doc)
}

// hasClockOffset reports whether out estimates the backend clock: every
// output but those of type plugin:<id>.
func hasClockOffset(out supervisor.OutputStatus) bool {
	return !strings.HasPrefix(out.Type, plugin.DriverPrefix)
}

func queueOf(q buffer.QueueStats, now time.Time) queueDoc {
	return queueDoc{Records: q.Records, Bytes: q.Bytes, OldestUnconfirmedAgeSeconds: oldestAge(q, now).Seconds()}
}

// oldestAge is the age of the oldest unconfirmed record of q, zero when
// there is none or when its device clock is ahead of the agent's.
func oldestAge(q buffer.QueueStats, now time.Time) time.Duration {
	if q.OldestUnacked.IsZero() {
		return 0
	}

	return max(now.Sub(q.OldestUnacked), 0)
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()

	return &t
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.log.Error("status: cannot encode the answer", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}
