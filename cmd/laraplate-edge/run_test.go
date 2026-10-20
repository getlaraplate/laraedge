package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/supervisor"
)

// guard bounds every wait for a goroutine; it is a deadlock guard, never a
// timing assertion.
const guard = 10 * time.Second

const (
	runToken = "run-tok-91b7-SECRET"
	runEnv   = "EDGE_OUTPUT_TOKEN"
)

func TestPrepareDataDir(t *testing.T) {
	tests := []struct {
		name    string
		format  string // "" means no format file
		wantErr string
	}{
		{name: "a new data directory gets format 1"},
		{name: "format 1 is this agent's", format: "1\n"},
		{name: "a newer format is refused", format: "2\n", wantErr: "has format 2, newer than this agent supports (1)"},
		{name: "a format file without a number is refused", format: "x\n", wantErr: "does not hold a format version"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			if tt.format != "" {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "format"), []byte(tt.format), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := prepareDataDir(dir)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("prepareDataDir() = %v, want an error with %q", err, tt.wantErr)
				}
				data, _ := os.ReadFile(filepath.Join(dir, "format"))
				if string(data) != tt.format {
					t.Fatalf("format file = %q after a refusal, want it untouched", data)
				}

				return
			}
			if err != nil {
				t.Fatalf("prepareDataDir() = %v", err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "format"))
			if err != nil || strings.TrimSpace(string(data)) != "1" {
				t.Fatalf("format file = %q, %v; want 1", data, err)
			}
		})
	}
}

// quietInput is a fake simulated driver that starts and reads nothing: the
// supervisor still sends the births of its devices.
type quietInput struct{}

func (quietInput) Start(context.Context, func(model.Record)) error { return nil }
func (quietInput) Close() error                                    { return nil }
func (quietInput) Probe(context.Context) ([]model.Sample, error)   { return nil, nil }

func quietFactory(config.Input, clock.Clock) (input.Input, error) { return quietInput{}, nil }

// ingest is a fake ingest route recording the Authorization of every request.
type ingest struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
}

func newIngest(t *testing.T) *ingest {
	t.Helper()

	in := &ingest{}
	in.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		in.mu.Lock()
		in.auth = append(in.auth, r.Header.Get("Authorization"))
		in.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"accepted"}`)
	}))
	in.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	in.srv.Start()
	t.Cleanup(in.srv.Close)

	return in
}

func (in *ingest) requests() []string {
	in.mu.Lock()
	defer in.mu.Unlock()

	return append([]string(nil), in.auth...)
}

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

type fakeEnv struct {
	mu   sync.Mutex
	vars map[string]string
}

func (e *fakeEnv) lookup(name string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	v, ok := e.vars[name]

	return v, ok
}

func (e *fakeEnv) set(name, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.vars[name] = value
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bootstrapYAML returns a bootstrap file of the file provider whose status
// endpoint binds a free loopback port.
func bootstrapYAML(agentID, configFile, dataDir string) string {
	return "agent_id: " + agentID + "\nconfig:\n  provider: file\n  file: " + configFile + "\ndata_dir: " + dataDir + "\nstatus_listen: 127.0.0.1:0\n"
}

// TestRunAgentReloadResumesAnOutputWhoseTokenWasUnset is the Review Focus
// case: an output token secret://env/NAME with NAME unset starts suspended
// and buffers; once NAME is set, the bootstrap reload (SIGHUP) resumes
// sending.
func TestRunAgentReloadResumesAnOutputWhoseTokenWasUnset(t *testing.T) {
	dir := t.TempDir()
	be := newIngest(t)
	configFile := filepath.Join(dir, "config.json")
	writeFile(t, configFile, `{"schema":"laraplate-edge-config/1","version":1,
		"inputs":[{"id":"line","driver":"simulated","devices":[{"device":"press-07","points":[{"signal":"state","address":"state","read":"subscribe"}]}]}],
		"outputs":[{"id":"main","type":"laraplate-http","url":"`+be.srv.URL+`","token":"secret://env/`+runEnv+`","batch":{"max_samples":1}}]}`)
	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	dataDir := filepath.Join(dir, "data")
	writeFile(t, bootstrapFile, bootstrapYAML("plant-a-edge-01", configFile, dataDir))

	env := &fakeEnv{vars: map[string]string{}}
	logs := &lockedBuffer{}
	started := make(chan *supervisor.Supervisor, 1)
	reload := make(chan struct{}, 1)
	deps := agentDeps{
		lookupEnv: env.lookup,
		clock:     clock.NewFake(time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)),
		drivers:   map[string]input.Factory{config.DriverSimulated: quietFactory},
		logger:    slog.New(slog.NewJSONHandler(logs, nil)),
		started:   func(s *supervisor.Supervisor) { started <- s },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runAgent(ctx, bootstrapFile, deps, reload, io.Discard) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(guard):
			t.Error("runAgent did not return after cancel")
		}
	}()

	var sup *supervisor.Supervisor
	select {
	case sup = <-started:
	case <-time.After(guard):
		t.Fatal("the supervisor was not started")
	}

	waitFor(t, "output suspended", func() bool {
		st := sup.Status()
		return st.Version == 1 && len(st.Outputs) == 1 && st.Outputs[0].State == "suspended_auth"
	})
	waitFor(t, "birth buffered", func() bool { return sup.Status().Outputs[0].Buffer.Records > 0 })
	if got := be.requests(); len(got) != 0 {
		t.Fatalf("the backend received %d requests without a token", len(got))
	}

	// An identity change needs a restart: it is reported, the credentials
	// are still reloaded.
	writeFile(t, bootstrapFile, bootstrapYAML("plant-a-edge-02", configFile, dataDir))
	env.set(runEnv, runToken)
	reload <- struct{}{}

	waitFor(t, "sending resumed", func() bool { return len(be.requests()) > 0 })
	if got := be.requests()[0]; got != "Bearer "+runToken {
		t.Fatalf("Authorization = %q, want the token set before the reload", got)
	}
	waitFor(t, "restart warning", func() bool {
		return strings.Contains(logs.String(), `"level":"WARN"`) && strings.Contains(logs.String(), `"key":"agent_id"`)
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runAgent() = %v, want nil after a graceful stop", err)
		}
		done <- nil
	case <-time.After(guard):
		t.Fatal("runAgent did not return after cancel")
	}

	if strings.Contains(logs.String(), runToken) {
		t.Fatal("the logs contain the token")
	}
	if data, err := os.ReadFile(filepath.Join(dataDir, "format")); err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("data_dir/format = %q, %v; want 1", data, err)
	}
}

func TestRunAgentRefusesANewerDataDir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dataDir, "format"), "2\n")
	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, bootstrapFile, bootstrapYAML("plant-a-edge-01", filepath.Join(dir, "config.json"), dataDir))

	err := runAgent(t.Context(), bootstrapFile, agentDeps{lookupEnv: os.LookupEnv, clock: clock.Real()}, nil, io.Discard)

	if err == nil || !strings.Contains(err.Error(), "newer than this agent supports") {
		t.Fatalf("runAgent() = %v, want the newer data_dir refused", err)
	}
}
