package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/clock"
	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/input"
	"github.com/getlaraplate/laraplate-edge/internal/supervisor"
)

// agentRun is a runAgent running in a goroutine of the test.
type agentRun struct {
	cancel  context.CancelFunc
	done    chan error
	sup     *supervisor.Supervisor
	addr    string
	stopped bool
}

// startAgent runs runAgent on bootstrapFile and waits for its supervisor
// and its status endpoint.
func startAgent(t *testing.T, bootstrapFile string, deps agentDeps) *agentRun {
	t.Helper()

	started := make(chan *supervisor.Supervisor, 1)
	listening := make(chan string, 1)
	deps.started = func(s *supervisor.Supervisor) { started <- s }
	deps.listening = func(addr string) { listening <- addr }

	ctx, cancel := context.WithCancel(context.Background())
	a := &agentRun{cancel: cancel, done: make(chan error, 1)}
	go func() { a.done <- runAgent(ctx, bootstrapFile, deps, nil, io.Discard) }()
	t.Cleanup(func() { a.stop(t) })

	select {
	case a.sup = <-started:
	case err := <-a.done:
		t.Fatalf("runAgent() = %v before starting", err)
	case <-time.After(guard):
		t.Fatal("the supervisor was not started")
	}
	select {
	case a.addr = <-listening:
	case <-time.After(guard):
		t.Fatal("the status endpoint was not started")
	}

	return a
}

func (a *agentRun) stop(t *testing.T) {
	t.Helper()

	if a.stopped {
		return
	}
	a.stopped = true
	a.cancel()
	select {
	case err := <-a.done:
		if err != nil {
			t.Errorf("runAgent() = %v, want nil after a graceful stop", err)
		}
	case <-time.After(guard):
		t.Fatal("runAgent did not return after cancel")
	}
}

func quietConfig(t *testing.T, dir, outputURL string) string {
	t.Helper()

	configFile := filepath.Join(dir, "config.json")
	writeFile(t, configFile, `{"schema":"laraplate-edge-config/1","version":1,
		"inputs":[{"id":"line","driver":"simulated","devices":[{"device":"press-07","points":[{"signal":"state","address":"state","read":"subscribe"}]}]}],
		"outputs":[{"id":"main","type":"laraplate-http","url":"`+outputURL+`","token":"secret://env/`+runEnv+`","batch":{"max_samples":1}}]}`)

	return configFile
}

func TestRunAgentServesTheStatusEndpoint(t *testing.T) {
	dir := t.TempDir()
	be := newIngest(t)
	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, bootstrapFile, bootstrapYAML("plant-a-edge-01", quietConfig(t, dir, be.srv.URL), filepath.Join(dir, "data")))

	a := startAgent(t, bootstrapFile, agentDeps{
		lookupEnv: (&fakeEnv{vars: map[string]string{runEnv: runToken}}).lookup,
		clock:     clock.NewFake(time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)),
		drivers:   map[string]input.Factory{config.DriverSimulated: quietFactory},
		logger:    slog.New(slog.DiscardHandler),
	})
	waitFor(t, "configuration applied", func() bool { return a.sup.Status().Version == 1 })

	code, body := httpGet(t, "http://"+a.addr+"/status")
	if code != http.StatusOK {
		t.Fatalf("GET /status = %d", code)
	}
	var st struct {
		AgentID       string `json:"agent_id"`
		ConfigVersion int64  `json:"config_version"`
		Inputs        []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"inputs"`
		Outputs []struct {
			ID string `json:"id"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("GET /status = %s: %v", body, err)
	}
	if st.AgentID != "plant-a-edge-01" || st.ConfigVersion != 1 || len(st.Inputs) != 1 || st.Inputs[0].State != "running" || len(st.Outputs) != 1 || st.Outputs[0].ID != "main" {
		t.Fatalf("GET /status = %s", body)
	}
	if code, _ := httpGet(t, "http://"+a.addr+"/readyz"); code != http.StatusOK {
		t.Fatalf("GET /readyz = %d, want 200 once the configuration runs", code)
	}
	if code, body := httpGet(t, "http://"+a.addr+"/metrics"); code != http.StatusOK || !strings.Contains(body, "laraplate_edge_config_version 1\n") {
		t.Fatalf("GET /metrics = %d %s", code, body)
	}

	a.stop(t)
	if conn, err := net.Dial("tcp", a.addr); err == nil {
		conn.Close()
		t.Fatal("the status endpoint still listens after the agent stopped")
	}
}

func TestRunAgentRefusesAnOpenStatusEndpointWithoutAToken(t *testing.T) {
	dir := t.TempDir()
	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, bootstrapFile, "agent_id: plant-a-edge-01\nconfig:\n  provider: file\n  file: "+filepath.Join(dir, "config.json")+
		"\ndata_dir: "+filepath.Join(dir, "data")+"\nstatus_listen: 0.0.0.0:0\n")

	err := runAgent(t.Context(), bootstrapFile, agentDeps{lookupEnv: (&fakeEnv{vars: map[string]string{}}).lookup, clock: clock.Real(), logger: slog.New(slog.DiscardHandler)}, nil, io.Discard)

	if err == nil || !strings.Contains(err.Error(), "status_token") {
		t.Fatalf("runAgent() = %v, want a refusal naming status_token", err)
	}
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(body)
}
