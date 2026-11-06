package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

var _ edgeinput.Input = (*Input)(nil)

func TestFactoryValidatesWithoutConnecting(t *testing.T) {
	ok := config.Input{
		ID:         "line",
		Driver:     config.DriverMQTT,
		Connection: json.RawMessage(`{"broker":"mqtt://192.0.2.1"}`),
		Devices:    []config.Device{device("press", subscribePoint("temp", "plant/press/temp", "float64"))},
	}

	tests := []struct {
		name    string
		edit    func(*config.Input)
		wantErr string
	}{
		{"valid, broker unreachable", func(*config.Input) {}, ""},
		{"unknown connection key", func(c *config.Input) {
			c.Connection = json.RawMessage(`{"broker":"mqtt://192.0.2.1","bogus":1}`)
		}, `mqtt input "line": connection.bogus: unknown field`},
		{"wildcard in an address", func(c *config.Input) {
			c.Devices = []config.Device{device("press", subscribePoint("a", "plant/ok", "int32"), subscribePoint("b", "plant/+/temp", "float64"))}
		}, `mqtt input "line": devices.0.points.1: address: topic:`},
		{"poll read", func(c *config.Input) {
			p := subscribePoint("a", "plant/ok", "int32")
			p.Read = config.Read{Mode: config.ReadPoll, Interval: time.Second}
			c.Devices = []config.Device{device("press", p)}
		}, `mqtt input "line": devices.0.points.0: read: want subscribe`},
		{"missing type", func(c *config.Input) {
			c.Devices = []config.Device{device("press", subscribePoint("a", "plant/ok", ""))}
		}, `mqtt input "line": devices.0.points.0: type: required`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ok
			tt.edit(&cfg)

			in, err := Factory(Options{})(cfg, clock.Real())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Factory: %v", err)
				}
				if err := in.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}

				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
				t.Fatalf("Factory error = %v, want prefix %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "192.0.2.1") || strings.Contains(err.Error(), "plant/") {
				t.Errorf("error echoes a value: %v", err)
			}
		})
	}
}

func TestClientID(t *testing.T) {
	devs := []config.Device{device("press", subscribePoint("temp", "plant/press/temp", "float64"))}

	tests := []struct {
		name string
		conn string
		want string
	}{
		{"default", `{"broker":"mqtt://192.0.2.1"}`, "laraplate-edge-agent-7-line"},
		{"document", `{"broker":"mqtt://192.0.2.1","client_id":"press-gw"}`, "press-gw"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := New(config.Input{ID: "line", Connection: json.RawMessage(tt.conn), Devices: devs}, clock.Real(), Options{AgentID: "agent-7"})
			if err != nil {
				t.Fatal(err)
			}
			if in.clientID != tt.want {
				t.Errorf("clientID = %q, want %q", in.clientID, tt.want)
			}
		})
	}
}

func TestStartFailsOnAMissingTLSFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-ca.pem")
	conn, err := json.Marshal(map[string]any{"broker": "mqtts://127.0.0.1:1", "tls": map[string]any{"ca_file": missing}})
	if err != nil {
		t.Fatal(err)
	}
	in, err := New(config.Input{ID: "line", Connection: conn, Devices: []config.Device{device("press", subscribePoint("temp", "t", "float64"))}}, clock.NewFake(driverEpoch), Options{})
	if err != nil {
		t.Fatalf("New must not read the TLS files: %v", err)
	}

	err = in.Start(t.Context(), func(model.Record) {})
	if !errors.Is(err, errCAFile) {
		t.Fatalf("Start error = %v, want errCAFile", err)
	}
	if strings.Contains(err.Error(), missing) {
		t.Errorf("error names the file: %v", err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartAndCloseStates(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("temp", "t", "float64")))

	never := newHarness(t, cfg, Options{})
	if err := never.in.Close(); err != nil {
		t.Fatalf("Close on an input never started: %v", err)
	}
	if err := never.in.Start(t.Context(), func(model.Record) {}); err == nil {
		t.Fatal("Start after Close succeeded")
	}

	h := newHarness(t, cfg, Options{})
	h.start()
	if err := h.in.Start(t.Context(), h.emit); err == nil {
		t.Fatal("second Start succeeded")
	}
	h.serving()
	h.close()
	if err := h.in.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if h.in.Health().Connected {
		t.Error("Connected after Close")
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := newHarness(t, cfg, Options{}).in.Start(cancelled, func(model.Record) {}); err == nil {
		t.Error("Start with a done context succeeded")
	}
}

func TestTopicsFile(t *testing.T) {
	dir := t.TempDir()

	if _, err := readTopics(dir, "line"); err == nil {
		t.Fatal("readTopics of a missing file succeeded")
	}
	if _, err := readTopics("", "line"); !errors.Is(err, errNoDataDir) {
		t.Fatalf("readTopics without data_dir = %v", err)
	}
	if err := writeTopics(dir, "line", []string{"a", "b/c"}); err != nil {
		t.Fatal(err)
	}
	got, err := readTopics(dir, "line")
	if err != nil || !slices.Equal(got, []string{"a", "b/c"}) {
		t.Fatalf("readTopics = %q, %v", got, err)
	}

	if got := staleTopics([]string{"z", "a", "b", "a"}, []string{"b"}); !slices.Equal(got, []string{"a", "z"}) {
		t.Errorf("staleTopics = %q", got)
	}
	if got := staleTopics(nil, []string{"b"}); len(got) != 0 {
		t.Errorf("staleTopics of nothing = %q", got)
	}
}
