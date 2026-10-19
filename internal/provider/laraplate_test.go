package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/provider"
	"github.com/swolley/laraplate-edge/internal/secrets"
	"github.com/swolley/laraplate-edge/internal/supervisor"
	"github.com/swolley/laraplate-edge/internal/version"
)

const (
	agentToken = "agent-tok-5d2e-SECRET"
	agentEnv   = "AGENT_TOKEN"
	configPath = "/api/v1/detail/mes/machine-agent-configs"
	reportPath = "/api/v1/insert/mes/machine-agent-config-reports"
	poll       = 60 * time.Second
)

// getRequest is one configuration request the fake backend received.
type getRequest struct {
	query     string // the version parameter, "" when absent
	hasQuery  bool
	auth      string
	accept    string
	userAgent string
}

// configBackend emulates the backend configuration routes: GET answers 304
// when ?version= equals the active version, the active document otherwise,
// 404 without one; status overrides the answer when set.
type configBackend struct {
	srv *httptest.Server

	mu            sync.Mutex
	activeVersion int64
	activeDoc     string
	status        int
	reportStatus  []int
	gets          []getRequest
	reports       []map[string]any
	changed       chan struct{}
}

func newConfigBackend(t *testing.T) *configBackend {
	t.Helper()

	be := &configBackend{changed: make(chan struct{}, 1)}
	be.srv = httptest.NewUnstartedServer(http.HandlerFunc(be.serve))
	be.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	be.srv.Start()
	t.Cleanup(be.srv.Close)

	return be
}

func (be *configBackend) setActive(v int64, document string) {
	be.mu.Lock()
	defer be.mu.Unlock()

	be.activeVersion, be.activeDoc = v, document
}

func (be *configBackend) set(fn func(*configBackend)) {
	be.mu.Lock()
	defer be.mu.Unlock()

	fn(be)
}

func (be *configBackend) serve(w http.ResponseWriter, r *http.Request) {
	defer func() {
		select {
		case be.changed <- struct{}{}:
		default:
		}
	}()

	be.mu.Lock()
	defer be.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == configPath:
		q, has := r.URL.Query()["version"]
		g := getRequest{hasQuery: has, auth: r.Header.Get("Authorization"), accept: r.Header.Get("Accept"), userAgent: r.Header.Get("User-Agent")}
		if has {
			g.query = q[0]
		}
		be.gets = append(be.gets, g)

		switch {
		case be.status != 0:
			w.WriteHeader(be.status)
		case be.activeDoc == "":
			w.WriteHeader(http.StatusNotFound)
		case has && g.query == itoa(int(be.activeVersion)):
			w.WriteHeader(http.StatusNotModified)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"version":`+itoa(int(be.activeVersion))+`,"document":`+be.activeDoc+`}`)
		}
	case r.Method == http.MethodPost && r.URL.Path == reportPath:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		be.reports = append(be.reports, body)
		status := http.StatusCreated
		if len(be.reportStatus) > 0 {
			status, be.reportStatus = be.reportStatus[0], be.reportStatus[1:]
		}
		w.WriteHeader(status)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func (be *configBackend) getsSoFar() []getRequest {
	be.mu.Lock()
	defer be.mu.Unlock()

	return append([]getRequest(nil), be.gets...)
}

func (be *configBackend) reportsSoFar() []map[string]any {
	be.mu.Lock()
	defer be.mu.Unlock()

	return append([]map[string]any(nil), be.reports...)
}

func (be *configBackend) waitGets(t *testing.T, n int) {
	t.Helper()
	waitFor(t, "configuration requests", func() bool { return len(be.getsSoFar()) >= n })
}

func (be *configBackend) waitReports(t *testing.T, n int) {
	t.Helper()
	waitFor(t, "reports", func() bool { return len(be.reportsSoFar()) >= n })
}

type laraplateFixture struct {
	t       *testing.T
	clk     *clock.Fake
	be      *configBackend
	dataDir string
	logs    *lockedBuffer
	env     map[string]string
	envMu   sync.Mutex
	// attempted is the provider's Attempted hook; nil means none.
	attempted func()
}

func newLaraplateFixture(t *testing.T) *laraplateFixture {
	t.Helper()

	return &laraplateFixture{
		t:       t,
		clk:     clock.NewFake(start),
		be:      newConfigBackend(t),
		dataDir: t.TempDir(),
		logs:    &lockedBuffer{},
		env:     map[string]string{agentEnv: agentToken},
	}
}

func (f *laraplateFixture) lookup(name string) (string, bool) {
	f.envMu.Lock()
	defer f.envMu.Unlock()

	v, ok := f.env[name]

	return v, ok
}

// start runs a provider applying through a.
func (f *laraplateFixture) start(a provider.Applier) *runner {
	f.t.Helper()

	p, err := provider.NewLaraplate(provider.LaraplateConfig{
		URL:          f.be.srv.URL + "/",
		Token:        "secret://env/" + agentEnv,
		PollInterval: poll,
		Resolver:     secrets.NewResolver(secrets.WithLookupEnv(f.lookup)),
		Applier:      a,
		DataDir:      f.dataDir,
		Clock:        f.clk,
		Logger:       slog.New(slog.NewJSONHandler(f.logs, nil)),
		Attempted:    f.attempted,
	})
	if err != nil {
		f.t.Fatalf("NewLaraplate() = %v", err)
	}

	return run(f.t, p.Run)
}

// pollAgain fires the next poll and waits until the provider waits again.
func (f *laraplateFixture) pollAgain(gets int) {
	f.t.Helper()

	waitTimers(f.t, f.clk, 1)
	f.clk.Advance(poll)
	f.be.waitGets(f.t, gets)
	waitTimers(f.t, f.clk, 1)
}

// providerState is the persisted state of the provider.
type providerState struct {
	LastAttempt *struct {
		Version int64  `json:"version"`
		Outcome string `json:"outcome"`
	} `json:"last_attempt"`
	PendingReports []map[string]any `json:"pending_reports"`
}

func (f *laraplateFixture) state() providerState {
	f.t.Helper()

	var st providerState
	data, err := os.ReadFile(filepath.Join(f.dataDir, "config", "laraplate-state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return st
	}
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		f.t.Fatalf("state %s: %v", data, err)
	}

	return st
}

// lastAttempt returns "version outcome" of the persisted last attempt.
func (f *laraplateFixture) lastAttempt() string {
	f.t.Helper()

	st := f.state()
	if st.LastAttempt == nil {
		return "none"
	}

	return itoa(int(st.LastAttempt.Version)) + " " + st.LastAttempt.Outcome
}

func report(v int, outcome string, errs ...string) map[string]any {
	var e any
	if errs != nil {
		list := make([]any, len(errs))
		for i, s := range errs {
			list[i] = s
		}
		e = list
	}

	return map[string]any{"version": float64(v), "outcome": outcome, "errors": e, "agent_version": version.String()}
}

func TestLaraplateAppliesReportsAndThenAsksWithTheVersion(t *testing.T) {
	f := newLaraplateFixture(t)
	f.be.setActive(3, doc(3))
	a := newFakeApplier()
	f.start(a)

	if got := a.next(t); got.Version != 3 {
		t.Fatalf("applied version %d, want 3", got.Version)
	}
	f.be.waitReports(t, 1)

	first := f.be.getsSoFar()[0]
	want := getRequest{auth: "Bearer " + agentToken, accept: "application/json", userAgent: "laraplate-edge/" + version.String()}
	if first != want {
		t.Fatalf("first request = %+v, want %+v (no version before anything is applied)", first, want)
	}
	if got := f.be.reportsSoFar(); !reflect.DeepEqual(got, []map[string]any{report(3, "applied")}) {
		t.Fatalf("reports = %v, want one applied report", got)
	}

	f.pollAgain(2)

	if g := f.be.getsSoFar()[1]; !g.hasQuery || g.query != "3" {
		t.Fatalf("second request version = %q (present %v), want 3", g.query, g.hasQuery)
	}
	if n := a.count(); n != 1 {
		t.Fatalf("a 304 applied a document (%d applies)", n)
	}
	if n := len(f.be.reportsSoFar()); n != 1 {
		t.Fatalf("a 304 sent a report (%d reports)", n)
	}
	if f.logs.has(agentToken) {
		t.Fatal("the logs contain the agent token")
	}
}

// TestLaraplateRefusesAnOlderVersionOnce is the Review Focus case: a document
// older than the applied one is refused, reported rejected once, and the
// running configuration stays.
func TestLaraplateRefusesAnOlderVersionOnce(t *testing.T) {
	f := newLaraplateFixture(t)
	sup, err := supervisor.New(supervisor.Config{
		DataDir:  f.dataDir,
		Resolver: secrets.NewResolver(secrets.WithLookupEnv(f.lookup)),
		Clock:    f.clk,
		Logger:   slog.New(slog.NewJSONHandler(f.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	f.be.setActive(5, doc(5))
	f.start(sup)
	f.be.waitReports(t, 1)
	waitTimers(t, f.clk, 1)

	f.be.setActive(3, doc(3))
	f.pollAgain(2)
	f.be.waitReports(t, 2)

	if g := f.be.getsSoFar()[1]; g.query != "5" {
		t.Fatalf("second request version = %q, want the applied 5", g.query)
	}
	wantReports := []map[string]any{
		report(5, "applied"),
		report(3, "rejected", "version: 3 is older than the applied version 5"),
	}
	if got := f.be.reportsSoFar(); !reflect.DeepEqual(got, wantReports) {
		t.Fatalf("reports = %v, want %v", got, wantReports)
	}
	if v := sup.AppliedVersion(); v != 5 {
		t.Fatalf("AppliedVersion() = %d, want the running 5", v)
	}
	if got := f.lastAttempt(); got != "3 rejected" {
		t.Fatalf("last attempt = %q, want 3 rejected", got)
	}

	// The backend keeps answering the refused version: it is neither applied
	// nor reported again.
	f.pollAgain(3)
	if got := f.be.reportsSoFar(); len(got) != 2 {
		t.Fatalf("the refused version was reported again: %v", got)
	}
	if v := sup.AppliedVersion(); v != 5 {
		t.Fatalf("AppliedVersion() = %d, want 5", v)
	}
}

func TestLaraplateRetriesAFailedReportOnTheNextPoll(t *testing.T) {
	f := newLaraplateFixture(t)
	f.be.setActive(2, doc(2))
	f.be.set(func(be *configBackend) { be.reportStatus = []int{http.StatusInternalServerError} })
	a := newFakeApplier()
	f.start(a)
	a.next(t)
	f.be.waitReports(t, 1)

	f.pollAgain(2)
	f.be.waitReports(t, 2)
	f.pollAgain(3)

	if got := f.be.reportsSoFar(); !reflect.DeepEqual(got, []map[string]any{report(2, "applied"), report(2, "applied")}) {
		t.Fatalf("reports = %v, want the failed report sent again once", got)
	}
	if n := a.count(); n != 1 {
		t.Fatalf("applied %d times, want 1", n)
	}
}

func TestLaraplateRolledBackVersionIsNotDownloadedAgain(t *testing.T) {
	f := newLaraplateFixture(t)
	f.be.setActive(8, doc(8))
	a := newFakeApplier()
	a.setOutcome(supervisor.Outcome{Result: supervisor.ResultRolledBack, Errors: []string{"inputs.0: did not start within 30s"}})
	r := f.start(a)
	a.next(t)
	f.be.waitReports(t, 1)

	f.pollAgain(2)
	if g := f.be.getsSoFar()[1]; g.query != "8" {
		t.Fatalf("second request version = %q, want the attempted 8", g.query)
	}
	if n := a.count(); n != 1 {
		t.Fatalf("the rolled back version was applied again (%d applies)", n)
	}

	// The attempted version survives a restart.
	r.stop(t)
	f.start(newFakeApplier())
	f.be.waitGets(t, 3)
	if g := f.be.getsSoFar()[2]; g.query != "8" {
		t.Fatalf("first request after a restart version = %q, want the persisted 8", g.query)
	}
	if got := f.be.reportsSoFar(); !reflect.DeepEqual(got, []map[string]any{report(8, "rolled_back", "inputs.0: did not start within 30s")}) {
		t.Fatalf("reports = %v", got)
	}
}

func TestLaraplateReportErrorsAreCappedToTheBackendLimits(t *testing.T) {
	f := newLaraplateFixture(t)
	f.be.setActive(4, doc(4))
	errs := make([]string, 150)
	for i := range errs {
		errs[i] = strings.Repeat("é", 1500)
	}
	a := newFakeApplier()
	a.setOutcome(supervisor.Outcome{Result: supervisor.ResultRolledBack, Errors: errs})
	f.start(a)
	f.be.waitReports(t, 1)

	got, _ := f.be.reportsSoFar()[0]["errors"].([]any)
	if len(got) != 100 {
		t.Fatalf("report carries %d errors, want 100", len(got))
	}
	for i, e := range got {
		if s, _ := e.(string); utf8.RuneCountInString(s) != 1000 || !utf8.ValidString(s) {
			t.Fatalf("error %d has %d characters, want 1000 valid ones", i, utf8.RuneCountInString(s))
		}
	}
}

func TestLaraplateDocumentVersion(t *testing.T) {
	tests := []struct {
		name        string
		document    string
		wantApplied int64
		wantReport  map[string]any
	}{
		{
			name:        "a document without version takes the published one",
			document:    doc(0),
			wantApplied: 6,
			wantReport:  report(6, "applied"),
		},
		{
			name:       "a document whose version differs from the published one",
			document:   doc(5),
			wantReport: report(6, "rejected", "version: differs from the published version 6"),
		},
		{
			name:       "a document that does not parse",
			document:   `{"schema":"laraplate-edge-config/1","inputs":[],"outputs":[],"bogus":1}`,
			wantReport: report(6, "rejected", "bogus: unknown key"),
		},
		{
			name:       "a document that is not an object",
			document:   `[1,2]`,
			wantReport: report(6, "rejected", "document: must be an object"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLaraplateFixture(t)
			f.be.setActive(6, tt.document)
			a := newFakeApplier()
			f.start(a)
			f.be.waitReports(t, 1)

			if got := f.be.reportsSoFar()[0]; !reflect.DeepEqual(got, tt.wantReport) {
				t.Fatalf("report = %v, want %v", got, tt.wantReport)
			}
			if tt.wantApplied == 0 {
				if n := a.count(); n != 0 {
					t.Fatalf("applied %d documents, want none", n)
				}

				return
			}
			if got := a.next(t); got.Version != tt.wantApplied {
				t.Fatalf("applied version %d, want %d", got.Version, tt.wantApplied)
			}
		})
	}
}

func TestLaraplateAnswersWithoutADocumentChangeNothing(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		noToken bool
		level   string
		message string
	}{
		{name: "no active configuration", status: http.StatusNotFound, level: "INFO", message: "no active configuration"},
		{name: "token refused", status: http.StatusUnauthorized, level: "WARN", message: "401"},
		{name: "permission refused", status: http.StatusForbidden, level: "WARN", message: "403"},
		{name: "server error", status: http.StatusInternalServerError, level: "WARN", message: "500"},
		{name: "token unavailable", noToken: true, level: "WARN", message: agentEnv},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLaraplateFixture(t)
			if tt.status != http.StatusNotFound {
				f.be.setActive(1, doc(1))
			}
			f.be.set(func(be *configBackend) { be.status = tt.status })
			if tt.noToken {
				f.envMu.Lock()
				f.env = map[string]string{}
				f.envMu.Unlock()
			}
			a := newFakeApplier()
			f.start(a)
			waitTimers(t, f.clk, 1)

			if n := a.count(); n != 0 {
				t.Fatalf("applied %d documents, want none", n)
			}
			if n := len(f.be.reportsSoFar()); n != 0 {
				t.Fatalf("sent %d reports, want none", n)
			}
			if n := len(f.be.getsSoFar()); tt.noToken && n != 0 {
				t.Fatalf("sent %d requests without a token, want none", n)
			}
			if !f.logs.has(`"level":"`+tt.level+`"`, tt.message) {
				t.Fatalf("no %s log with %q in %s", tt.level, tt.message, f.logs.String())
			}

			// The next poll tries again.
			f.envMu.Lock()
			f.env[agentEnv] = agentToken
			f.envMu.Unlock()
			f.be.set(func(be *configBackend) {
				be.status = 0
				be.activeVersion, be.activeDoc = 1, doc(1)
			})
			waitTimers(t, f.clk, 1)
			f.clk.Advance(poll)
			if got := a.next(t); got.Version != 1 {
				t.Fatalf("applied version %d on the next poll, want 1", got.Version)
			}
		})
	}
}

func TestNewLaraplateRefusesAnInsecureURL(t *testing.T) {
	_, err := provider.NewLaraplate(provider.LaraplateConfig{
		URL:          "http://erp.example.com",
		Token:        "secret://env/" + agentEnv,
		PollInterval: poll,
		Resolver:     secrets.NewResolver(),
		Applier:      newFakeApplier(),
		DataDir:      t.TempDir(),
	})
	if err == nil {
		t.Fatal("NewLaraplate() accepted plain http to a remote host")
	}
}

// TestLaraplateCancelledApplyIsNotAnAttempt: an apply the shutdown cut
// short (the supervisor returns no outcome) records nothing, so the version
// is downloaded and applied after the restart (ruling R45).
func TestLaraplateCancelledApplyIsNotAnAttempt(t *testing.T) {
	f := newLaraplateFixture(t)
	f.be.setActive(9, doc(9))
	a := newFakeApplier()
	a.hook = func(ctx context.Context) { <-ctx.Done() }
	a.setOutcome(supervisor.Outcome{})
	r := f.start(a)

	select {
	case <-a.entered:
	case <-time.After(guard):
		t.Fatal("the document was not applied")
	}
	r.stop(t)

	if got := f.be.reportsSoFar(); len(got) != 0 {
		t.Fatalf("reports = %v, want none for a cancelled apply", got)
	}
	if got := f.lastAttempt(); got != "none" {
		t.Fatalf("last attempt = %q, want none", got)
	}

	b := newFakeApplier()
	f.start(b)
	if got := b.next(t); got.Version != 9 {
		t.Fatalf("applied version %d after the restart, want 9", got.Version)
	}
	f.be.waitReports(t, 1)
	if g := f.be.getsSoFar()[1]; g.hasQuery {
		t.Fatalf("request after the restart asked for version %q, want none", g.query)
	}
}

// TestLaraplateAppliedAttemptDoesNotHideALostVersion: an applied version
// that the agent no longer runs after a restart (its last valid document
// could not be saved, or the offline start rolled back) is downloaded and
// applied again (ruling R45).
func TestLaraplateAppliedAttemptDoesNotHideALostVersion(t *testing.T) {
	tests := []struct {
		name      string
		running   int64
		wantQuery string
	}{
		{name: "an older last valid document runs", running: 3, wantQuery: "3"},
		{name: "nothing runs", running: 0, wantQuery: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLaraplateFixture(t)
			f.be.setActive(5, doc(5))
			r := f.start(newFakeApplier())
			f.be.waitReports(t, 1)
			r.stop(t)
			if got := f.lastAttempt(); got != "5 applied" {
				t.Fatalf("last attempt = %q, want 5 applied", got)
			}

			b := newFakeApplier()
			b.version = tt.running
			f.start(b)
			if got := b.next(t); got.Version != 5 {
				t.Fatalf("applied version %d after the restart, want 5", got.Version)
			}
			if g := f.be.getsSoFar()[1]; g.query != tt.wantQuery || g.hasQuery != (tt.wantQuery != "") {
				t.Fatalf("request after the restart asked for %q (present %v), want %q", g.query, g.hasQuery, tt.wantQuery)
			}
			f.be.waitReports(t, 2)
		})
	}
}

// TestLaraplatePendingReportSurvivesARestart: a report the backend did not
// record is persisted and sent after a restart (ruling R45).
func TestLaraplatePendingReportSurvivesARestart(t *testing.T) {
	f := newLaraplateFixture(t)
	f.be.setActive(5, doc(5))
	f.be.set(func(be *configBackend) { be.reportStatus = []int{http.StatusServiceUnavailable} })
	r := f.start(newFakeApplier())
	f.be.waitReports(t, 1)
	waitTimers(t, f.clk, 1)
	r.stop(t)

	if st := f.state(); !reflect.DeepEqual(st.PendingReports, []map[string]any{report(5, "applied")}) {
		t.Fatalf("pending reports = %v, want the undelivered one", st.PendingReports)
	}

	b := newFakeApplier()
	b.version = 5
	f.start(b)
	f.be.waitGets(t, 2)
	waitTimers(t, f.clk, 1)

	if got := f.be.reportsSoFar(); !reflect.DeepEqual(got, []map[string]any{report(5, "applied"), report(5, "applied")}) {
		t.Fatalf("reports = %v, want the pending report sent again after the restart", got)
	}
	if st := f.state(); len(st.PendingReports) != 0 {
		t.Fatalf("pending reports = %v after delivery, want none", st.PendingReports)
	}
	if n := b.count(); n != 0 {
		t.Fatalf("applied %d documents after the restart, want none (304)", n)
	}
}

func TestLaraplateRefusedReportIsDropped(t *testing.T) {
	f := newLaraplateFixture(t)
	f.be.setActive(5, doc(5))
	f.be.set(func(be *configBackend) { be.reportStatus = []int{http.StatusUnprocessableEntity} })
	f.start(newFakeApplier())
	f.be.waitReports(t, 1)
	f.pollAgain(2)

	if got := f.be.reportsSoFar(); len(got) != 1 {
		t.Fatalf("reports = %v, want the refused one sent once", got)
	}
	if st := f.state(); len(st.PendingReports) != 0 {
		t.Fatalf("pending reports = %v, want none", st.PendingReports)
	}
}

// TestLaraplateAppliedDuringShutdownIsStillReported: an apply that
// succeeded while the shutdown cancelled the context is an attempt: its
// report is persisted and sent after the restart.
func TestLaraplateAppliedDuringShutdownIsStillReported(t *testing.T) {
	f := newLaraplateFixture(t)
	f.be.setActive(9, doc(9))
	a := newFakeApplier()
	a.hook = func(ctx context.Context) { <-ctx.Done() }
	r := f.start(a)

	select {
	case <-a.entered:
	case <-time.After(guard):
		t.Fatal("the document was not applied")
	}
	r.stop(t)

	if got := f.lastAttempt(); got != "9 applied" {
		t.Fatalf("last attempt = %q, want 9 applied", got)
	}
	if st := f.state(); !reflect.DeepEqual(st.PendingReports, []map[string]any{report(9, "applied")}) {
		t.Fatalf("pending reports = %v, want the applied report", st.PendingReports)
	}

	b := newFakeApplier()
	b.version = 9
	f.start(b)
	f.be.waitReports(t, 1)
	if got := f.be.reportsSoFar(); !reflect.DeepEqual(got, []map[string]any{report(9, "applied")}) {
		t.Fatalf("reports = %v, want the applied report after the restart", got)
	}
}

// TestLaraplateAttemptedIsCalledOnceAfterTheFirstPoll covers the readiness
// hook (systemd READY=1): it follows the first poll whatever its outcome,
// here a backend without a configuration, and never again.
func TestLaraplateAttemptedIsCalledOnceAfterTheFirstPoll(t *testing.T) {
	f := newLaraplateFixture(t)
	var hook attempts
	f.attempted = hook.hook
	f.start(newFakeApplier())

	waitTimers(t, f.clk, 1)
	if got := hook.count(); got != 1 {
		t.Fatalf("Attempted called %d times after the first poll, want 1", got)
	}

	f.pollAgain(2)
	if got := hook.count(); got != 1 {
		t.Fatalf("Attempted called %d times after a later poll, want 1", got)
	}
}
