package provider_test

import (
	"encoding/json"
	"io"
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

func (f *laraplateFixture) lastAttempt() string {
	data, err := os.ReadFile(filepath.Join(f.dataDir, "config", "last-attempt"))
	if err != nil {
		return "error: " + err.Error()
	}

	return strings.TrimSpace(string(data))
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
	if got := f.lastAttempt(); got != "3" {
		t.Fatalf("last-attempt = %q, want 3", got)
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
