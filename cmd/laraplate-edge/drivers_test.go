package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/bootstrap"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/logging"
	"github.com/getlaraplate/laraedge/internal/secrets"
	"github.com/getlaraplate/laraedge/internal/supervisor"
)

const demoScenario = `{"tick":"1s","initial":"idle","states":[{"name":"idle","transitions":{"running":0.5}},{"name":"running","producing":true}],"cycle_time":{"mean":"3s"}}`

func simulatedInput(connection string) config.Input {
	return config.Input{
		ID:         "line",
		Driver:     config.DriverSimulated,
		Connection: []byte(connection),
		Devices:    []config.Device{{Device: "press-07", Points: []config.Point{{Signal: "state", Address: "state", Type: "string"}}}},
	}
}

func TestInputDriversRegisterSimulatedWithTheConfigFileDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "scenario.json"), demoScenario)
	clk := clock.NewFake(time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC))
	relative := simulatedInput(`{"scenario_file":"scenario.json"}`)

	fileBoot := bootstrap.File{Config: bootstrap.Config{Provider: bootstrap.ProviderFile, File: filepath.Join(dir, "config.yaml")}}
	factory := inputDrivers(fileBoot, nil, nil, nil)[config.DriverSimulated]
	if factory == nil {
		t.Fatal("simulated is not registered")
	}
	in, err := factory(relative, clk)
	if err != nil {
		t.Fatalf("a scenario file next to the configuration file: %v", err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}

	laraplateBoot := bootstrap.File{Config: bootstrap.Config{Provider: bootstrap.ProviderLaraplate}}
	if _, err := inputDrivers(laraplateBoot, nil, nil, nil)[config.DriverSimulated](relative, clk); err == nil || !strings.Contains(err.Error(), "scenario_file") {
		t.Fatalf("a relative scenario file with the laraplate provider: err = %v, want a refusal naming scenario_file", err)
	}
	abs := simulatedInput(`{"scenario_file":` + `"` + filepath.ToSlash(filepath.Join(dir, "scenario.json")) + `"}`)
	if _, err := inputDrivers(laraplateBoot, nil, nil, nil)[config.DriverSimulated](abs, clk); err != nil {
		t.Fatalf("an absolute scenario file with the laraplate provider: %v", err)
	}
}

func TestInputDriversRegisterModbus(t *testing.T) {
	boot := bootstrap.File{Config: bootstrap.Config{Provider: bootstrap.ProviderLaraplate}}
	factory := inputDrivers(boot, nil, nil, nil)[config.DriverModbus]
	if factory == nil {
		t.Fatal("modbus is not registered")
	}

	cfg := config.Input{
		ID:         "plc",
		Driver:     config.DriverModbus,
		Connection: []byte(`{"transport":"tcp","host":"127.0.0.1","port":1}`),
		Devices: []config.Device{{Device: "press-07", Points: []config.Point{
			{Signal: "speed", Address: "hr:10", Type: "int16", Read: config.Read{Mode: config.ReadPoll, Interval: time.Second}},
		}}},
	}
	in, err := factory(cfg, clock.NewFake(time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("modbus factory: %v", err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInputDriversLetExtraFactoriesReplaceBuiltIns(t *testing.T) {
	called := false
	extra := map[string]input.Factory{config.DriverSimulated: func(config.Input, clock.Clock) (input.Input, error) {
		called = true

		return quietInput{}, nil
	}}

	boot := bootstrap.File{Config: bootstrap.Config{Provider: bootstrap.ProviderFile, File: "config.yaml"}}
	if _, err := inputDrivers(boot, nil, nil, extra)[config.DriverSimulated](config.Input{}, clock.Real()); err != nil || !called {
		t.Fatalf("the extra factory was not used: called = %v, err = %v", called, err)
	}
}

// bodies is a fake ingest route recording every request body.
type bodies struct {
	srv *httptest.Server
	mu  sync.Mutex
	all []string
}

func newBodies(t *testing.T) *bodies {
	t.Helper()

	b := &bodies{}
	b.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.all = append(b.all, string(body))
		b.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"accepted"}`)
	}))
	b.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	b.srv.Start()
	t.Cleanup(b.srv.Close)

	return b
}

func (b *bodies) contain(text string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, body := range b.all {
		if strings.Contains(body, text) {
			return true
		}
	}

	return false
}

// TestRunAgentRunsTheBuiltInSimulatedDriver runs the agent with no injected
// driver: the simulated input reads its scenario file next to the
// configuration file and its first sample reaches the backend.
func TestRunAgentRunsTheBuiltInSimulatedDriver(t *testing.T) {
	dir := t.TempDir()
	be := newBodies(t)
	writeFile(t, filepath.Join(dir, "scenario.json"), demoScenario)
	configFile := filepath.Join(dir, "config.json")
	writeFile(t, configFile, `{"schema":"laraplate-edge-config/1","version":1,
		"inputs":[{"id":"line","driver":"simulated","connection":{"scenario_file":"scenario.json","seed":7},
			"devices":[{"device":"press-07","points":[{"signal":"machine_state","address":"state","type":"string","read":"subscribe"}]}]}],
		"outputs":[{"id":"main","type":"laraplate-http","url":"`+be.srv.URL+`","token":"secret://env/`+runEnv+`","batch":{"max_samples":1}}]}`)
	bootstrapFile := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, bootstrapFile, bootstrapYAML("plant-a-edge-01", configFile, filepath.Join(dir, "data")))

	env := &fakeEnv{vars: map[string]string{runEnv: runToken}}
	started := make(chan *supervisor.Supervisor, 1)
	deps := agentDeps{
		lookupEnv: env.lookup,
		clock:     clock.NewFake(time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)),
		started:   func(s *supervisor.Supervisor) { started <- s },
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runAgent(ctx, bootstrapFile, deps, nil, io.Discard) }()
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

	waitFor(t, "input running", func() bool {
		st := sup.Status()
		return len(st.Inputs) == 1 && st.Inputs[0].State == supervisor.InputRunning
	})
	waitFor(t, "initial state delivered", func() bool { return be.contain(`"machine_state"`) && be.contain(`"idle"`) })
}

func TestSimulatedExampleBootstrapLoads(t *testing.T) {
	boot, err := bootstrap.Load(filepath.Join("..", "..", "examples", "simulated", "bootstrap.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if boot.Config.Provider != bootstrap.ProviderFile || boot.Config.File != "config.yaml" || boot.DataDir != "./data" || boot.StatusListen != bootstrap.DefaultStatusListen {
		t.Fatalf("unexpected bootstrap: %+v", boot)
	}
}

func TestInputDriversRegisterOPCUAWithTheBootstrap(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	boot := bootstrap.File{AgentID: "plant-a-edge-01", DataDir: dataDir, Config: bootstrap.Config{Provider: bootstrap.ProviderLaraplate}}

	factory := inputDrivers(boot, secrets.NewResolver(), slog.New(slog.DiscardHandler), nil)[config.DriverOPCUA]
	if factory == nil {
		t.Fatal("opcua is not registered")
	}

	cfg := config.Input{
		ID:         "press",
		Driver:     config.DriverOPCUA,
		Connection: []byte(`{"endpoint":"opc.tcp://127.0.0.1:1","security_policy":"None"}`),
		Devices: []config.Device{{Device: "press-07", Points: []config.Point{
			{Signal: "speed", Address: "ns=2;s=Speed", Type: "float64", Read: config.Read{Mode: config.ReadPoll, Interval: time.Second}},
		}}},
	}
	in, err := factory(cfg, clock.NewFake(time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("opcua factory: %v", err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the factory touched data_dir: %v", err)
	}
}

func TestRouteLibraryLogsSendsTheStandardLogToDebug(t *testing.T) {
	var logs bytes.Buffer

	restore := routeLibraryLogs(logging.New(&logs, slog.LevelDebug))
	log.Print("opc.tcp://10.0.5.20:4840: connecting")
	restore()

	var rec map[string]any
	if err := json.Unmarshal(logs.Bytes(), &rec); err != nil {
		t.Fatalf("log output = %q, want one JSON record: %v", logs.String(), err)
	}
	if rec["level"] != "DEBUG" || rec["source"] != "gopcua" || rec["msg"] != "opc.tcp://10.0.5.20:4840: connecting" {
		t.Fatalf("record = %v, want the line at debug with source=gopcua and no date prefix", rec)
	}

	logs.Reset()
	routeLibraryLogs(logging.New(&logs, slog.LevelInfo))()
	if logs.Len() != 0 {
		t.Fatalf("restore left output %q", logs.String())
	}
}
