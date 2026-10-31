package status

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/buffer"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/supervisor"
)

// guard bounds every wait for a goroutine; it is a deadlock guard, never a
// timing assertion.
const guard = 10 * time.Second

const (
	testToken = "status-tok-5f2e-SECRET"
	tokenRef  = "secret://env/EDGE_STATUS_TOKEN"
)

var epoch = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

// fakeSource is a supervisor whose status the test sets.
type fakeSource struct {
	mu sync.Mutex
	st supervisor.Status
}

func (f *fakeSource) Status() supervisor.Status {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.st
}

func (f *fakeSource) set(st supervisor.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.st = st
}

// fakeResolver resolves the references of its map and fails on the others.
type fakeResolver map[string]string

func (r fakeResolver) Resolve(_ context.Context, ref string) (string, error) {
	v, ok := r[ref]
	if !ok {
		return "", errors.New("secret " + ref + ": not set")
	}

	return v, nil
}

// sampleStatus is a supervisor status with one running and one failed input,
// one running output in backoff and one failed output.
func sampleStatus() supervisor.Status {
	return supervisor.Status{
		Applied: true,
		Version: 7,
		Inputs: []supervisor.InputStatus{
			{ID: "line", Driver: "simulated", State: supervisor.InputRunning, TransformErrors: 3},
			{ID: "probe", Driver: "simulated", State: supervisor.StateFailed, LastError: "start: scenario_file: cannot read"},
		},
		Outputs: []supervisor.OutputStatus{
			{
				ID:    "main",
				Type:  "laraplate-http",
				State: "backoff",
				Buffer: buffer.Stats{
					Critical:       buffer.QueueStats{Records: 3, Bytes: 300, OldestUnacked: epoch.Add(-30 * time.Second)},
					Bulk:           buffer.QueueStats{Records: 5, Bytes: 1024},
					Records:        8,
					Bytes:          1324,
					Dropped:        map[string]uint64{"buffer_full": 4, "rejected_invalid": 1},
					CorruptRecords: 2,
				},
				ClockOffset:      1500 * time.Millisecond,
				LastConfirmation: epoch.Add(-time.Minute),
				LastError:        "HTTP 503",
				LastErrorAt:      epoch.Add(-10 * time.Second),
			},
			{ID: "spare", Type: "laraplate-http", State: supervisor.StateFailed, LastError: "open buffer: permission denied"},
		},
	}
}

type harness struct {
	srv *Server
	src *fakeSource
	clk *clock.Fake
	url string
}

func start(t *testing.T, listen, ref string, res fakeResolver) *harness {
	t.Helper()

	src := &fakeSource{st: sampleStatus()}
	clk := clock.NewFake(epoch.Add(-90 * time.Second))
	srv, err := Listen(t.Context(), Config{
		Listen:   listen,
		TokenRef: ref,
		Resolver: res,
		AgentID:  "plant-a-edge-01",
		Version:  "1.2.3",
		Source:   src,
		Clock:    clk,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}
	clk.Advance(90 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve() = %v after cancel, want nil", err)
			}
		case <-time.After(guard):
			t.Error("Serve did not return after cancel")
		}
	})

	_, port, _ := net.SplitHostPort(srv.Addr().String())

	return &harness{srv: srv, src: src, clk: clk, url: "http://127.0.0.1:" + port}
}

func (h *harness) get(t *testing.T, method, path, auth string) (int, http.Header, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, h.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, resp.Header, string(body)
}

func TestListenRefusesANonLoopbackBindWithoutAToken(t *testing.T) {
	tests := []struct {
		name    string
		listen  string
		ref     string
		res     fakeResolver
		wantErr []string
	}{
		{name: "every interface, IPv4", listen: "0.0.0.0:0", wantErr: []string{"status_listen", "0.0.0.0:0", "not a loopback address", "status_token"}},
		{name: "every interface, empty host", listen: ":0", wantErr: []string{"not a loopback address", "status_token"}},
		{name: "every interface, IPv6", listen: "[::]:0", wantErr: []string{"not a loopback address"}},
		{name: "a host name", listen: "edge-01.plant.example:0", wantErr: []string{"not a loopback address"}},
		{name: "a plant address", listen: "192.0.2.10:9180", wantErr: []string{"not a loopback address"}},
		{
			name:    "a token reference that does not resolve",
			listen:  "0.0.0.0:0",
			ref:     tokenRef,
			res:     fakeResolver{},
			wantErr: []string{"status_token", tokenRef},
		},
		{
			name:    "a token that resolves empty",
			listen:  "127.0.0.1:0",
			ref:     tokenRef,
			res:     fakeResolver{tokenRef: ""},
			wantErr: []string{"status_token", "empty"},
		},
		{name: "a malformed address", listen: "nonsense", wantErr: []string{"status_listen"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, err := Listen(t.Context(), Config{
				Listen:   tt.listen,
				TokenRef: tt.ref,
				Resolver: tt.res,
				Source:   &fakeSource{},
				Logger:   slog.New(slog.DiscardHandler),
			})
			if err == nil {
				_ = srv.Close()
				t.Fatalf("Listen(%q) started, want a refusal", tt.listen)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Listen(%q) = %v, want it to contain %q", tt.listen, err, want)
				}
			}
		})
	}
}

func TestListenOnLoopbackWithoutAToken(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:0", "localhost:0"} {
		t.Run(listen, func(t *testing.T) {
			srv, err := Listen(t.Context(), Config{Listen: listen, Source: &fakeSource{}})
			if err != nil {
				t.Fatalf("Listen(%q) = %v", listen, err)
			}
			if err := srv.Close(); err != nil {
				t.Fatalf("Close() = %v", err)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{host: "127.0.0.1", want: true},
		{host: "127.1.2.3", want: true},
		{host: "::1", want: true},
		{host: "localhost", want: true},
		{host: "LocalHost", want: true},
		{host: "", want: false},
		{host: "0.0.0.0", want: false},
		{host: "::", want: false},
		{host: "10.0.0.5", want: false},
		{host: "edge-01", want: false},
	}

	for _, tt := range tests {
		if got := isLoopbackHost(tt.host); got != tt.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestANonLoopbackBindWithATokenStarts(t *testing.T) {
	h := start(t, "0.0.0.0:0", tokenRef, fakeResolver{tokenRef: testToken})

	if code, _, _ := h.get(t, http.MethodGet, "/status", "Bearer "+testToken); code != http.StatusOK {
		t.Fatalf("GET /status = %d, want 200 with the token", code)
	}
}

func TestAuthorization(t *testing.T) {
	paths := []string{"/healthz", "/readyz", "/status", "/metrics"}

	tests := []struct {
		name      string
		ref       string
		auth      string
		wantCode  map[string]int
		wantProbe bool
	}{
		{
			name:     "no token configured: every endpoint is open",
			wantCode: map[string]int{"/healthz": 200, "/readyz": 503, "/status": 200, "/metrics": 200},
		},
		{
			name:     "token configured, no header: only healthz answers",
			ref:      tokenRef,
			wantCode: map[string]int{"/healthz": 200, "/readyz": 401, "/status": 401, "/metrics": 401},
		},
		{
			name:     "token configured, wrong token",
			ref:      tokenRef,
			auth:     "Bearer " + testToken + "x",
			wantCode: map[string]int{"/healthz": 200, "/readyz": 401, "/status": 401, "/metrics": 401},
		},
		{
			name:     "token configured, not a bearer",
			ref:      tokenRef,
			auth:     "Basic " + testToken,
			wantCode: map[string]int{"/healthz": 200, "/readyz": 401, "/status": 401, "/metrics": 401},
		},
		{
			name:     "token configured, right token",
			ref:      tokenRef,
			auth:     "Bearer " + testToken,
			wantCode: map[string]int{"/healthz": 200, "/readyz": 503, "/status": 200, "/metrics": 200},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, "127.0.0.1:0", tt.ref, fakeResolver{tokenRef: testToken})

			for _, path := range paths {
				code, header, body := h.get(t, http.MethodGet, path, tt.auth)
				if code != tt.wantCode[path] {
					t.Errorf("GET %s = %d, want %d", path, code, tt.wantCode[path])
				}
				if code == http.StatusUnauthorized {
					if got := header.Get("WWW-Authenticate"); got != "Bearer" {
						t.Errorf("GET %s: WWW-Authenticate = %q, want Bearer", path, got)
					}
					if strings.Contains(body, testToken) {
						t.Errorf("GET %s: the refusal echoes the token", path)
					}
				}
			}
		})
	}
}

func TestOnlyGetAndHeadAreAllowed(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		code, header, _ := h.get(t, method, "/status", "")
		if code != http.StatusMethodNotAllowed {
			t.Errorf("%s /status = %d, want 405", method, code)
		}
		if got := header.Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s /status: Allow = %q, want GET, HEAD", method, got)
		}
	}
	if code, _, _ := h.get(t, http.MethodHead, "/healthz", ""); code != http.StatusOK {
		t.Errorf("HEAD /healthz = %d, want 200", code)
	}
	if code, _, _ := h.get(t, http.MethodGet, "/nope", ""); code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", code)
	}
}

func TestHealthz(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	h.src.set(supervisor.Status{})

	code, _, body := h.get(t, http.MethodGet, "/healthz", "")

	if code != http.StatusOK || body != "ok\n" {
		t.Fatalf("GET /healthz = %d %q, want 200 ok", code, body)
	}
}

func TestReadyz(t *testing.T) {
	running := supervisor.InputStatus{ID: "line", Driver: "simulated", State: supervisor.InputRunning}
	failed := supervisor.InputStatus{ID: "probe", Driver: "simulated", State: supervisor.StateFailed, LastError: "boom"}

	tests := []struct {
		name       string
		st         supervisor.Status
		wantCode   int
		wantReason string
	}{
		{name: "no configuration applied", st: supervisor.Status{}, wantCode: 503, wantReason: "no configuration applied"},
		{name: "every input running", st: supervisor.Status{Applied: true, Version: 2, Inputs: []supervisor.InputStatus{running}}, wantCode: 200},
		{name: "a document without inputs", st: supervisor.Status{Applied: true, Version: 2}, wantCode: 200},
		{name: "a failed input", st: supervisor.Status{Applied: true, Version: 2, Inputs: []supervisor.InputStatus{running, failed}}, wantCode: 503, wantReason: `input "probe" is failed`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, "127.0.0.1:0", "", nil)
			h.src.set(tt.st)

			code, header, body := h.get(t, http.MethodGet, "/readyz", "")

			if code != tt.wantCode {
				t.Fatalf("GET /readyz = %d %s, want %d", code, body, tt.wantCode)
			}
			if got := header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			var answer struct {
				Ready  bool   `json:"ready"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(body), &answer); err != nil {
				t.Fatalf("body %q is not JSON: %v", body, err)
			}
			if answer.Ready != (tt.wantCode == 200) || answer.Reason != tt.wantReason {
				t.Fatalf("body = %+v, want ready %v and reason %q", answer, tt.wantCode == 200, tt.wantReason)
			}
		})
	}
}

func TestStatusJSON(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)

	code, header, body := h.get(t, http.MethodGet, "/status", "")

	if code != http.StatusOK {
		t.Fatalf("GET /status = %d", code)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}

	want := map[string]any{
		"agent_id":       "plant-a-edge-01",
		"agent_version":  "1.2.3",
		"uptime_seconds": 90.0,
		"config_applied": true,
		"config_version": 7.0,
		"inputs": []any{
			map[string]any{"id": "line", "driver": "simulated", "state": "running", "transform_errors": 3.0},
			map[string]any{"id": "probe", "driver": "simulated", "state": "failed", "last_error": "start: scenario_file: cannot read", "transform_errors": 0.0},
		},
		"outputs": []any{
			map[string]any{
				"id":                   "main",
				"type":                 "laraplate-http",
				"state":                "backoff",
				"clock_offset_seconds": 1.5,
				"buffer": map[string]any{
					"critical": map[string]any{"records": 3.0, "bytes": 300.0, "oldest_unconfirmed_age_seconds": 30.0},
					"bulk":     map[string]any{"records": 5.0, "bytes": 1024.0, "oldest_unconfirmed_age_seconds": 0.0},
				},
				"dropped":           map[string]any{"buffer_full": 4.0, "rejected_invalid": 1.0},
				"corrupt_records":   2.0,
				"last_confirmation": "2026-10-19T07:59:00Z",
				"last_error":        "HTTP 503",
				"last_error_at":     "2026-10-19T07:59:50Z",
			},
			map[string]any{
				"id":                   "spare",
				"type":                 "laraplate-http",
				"state":                "failed",
				"clock_offset_seconds": 0.0,
				"buffer": map[string]any{
					"critical": map[string]any{"records": 0.0, "bytes": 0.0, "oldest_unconfirmed_age_seconds": 0.0},
					"bulk":     map[string]any{"records": 0.0, "bytes": 0.0, "oldest_unconfirmed_age_seconds": 0.0},
				},
				"dropped":           map[string]any{},
				"corrupt_records":   0.0,
				"last_confirmation": nil,
				"last_error":        "open buffer: permission denied",
				"last_error_at":     nil,
			},
		},
		"plugins":    []any{},
		"processors": []any{},
	}

	gotJSON, _ := json.MarshalIndent(got, "", "  ")
	wantJSON, _ := json.MarshalIndent(want, "", "  ")
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("GET /status =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
}

func TestStatusJSONBeforeAnyConfiguration(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	h.src.set(supervisor.Status{})

	_, _, body := h.get(t, http.MethodGet, "/status", "")

	for _, want := range []string{`"config_applied":false`, `"config_version":0`, `"inputs":[]`, `"outputs":[]`, `"plugins":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /status = %s, want it to contain %s", body, want)
		}
	}
}

func TestOldestAgeIsNeverNegative(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	st := sampleStatus()
	// A device clock ahead of the agent clock.
	st.Outputs[0].Buffer.Critical.OldestUnacked = epoch.Add(time.Hour)
	h.src.set(st)

	_, _, body := h.get(t, http.MethodGet, "/status", "")

	if !strings.Contains(body, `"critical":{"records":3,"bytes":300,"oldest_unconfirmed_age_seconds":0}`) {
		t.Fatalf("GET /status = %s, want a zero age for a record from the future", body)
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	srv, err := Listen(t.Context(), Config{Listen: "127.0.0.1:0", Source: &fakeSource{}})
	if err != nil {
		t.Fatal(err)
	}
	addr := srv.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() = %v, want nil", err)
		}
	case <-time.After(guard):
		t.Fatal("Serve did not return after cancel")
	}

	if conn, err := net.Dial("tcp", addr); err == nil {
		conn.Close()
		t.Fatal("the listener is still open after Serve returned")
	}
}

func TestStatusJSONReportsHealthOfReportingInputsOnly(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	h.src.set(healthStatus())

	_, _, body := h.get(t, http.MethodGet, "/status", "")

	var doc struct {
		Inputs []map[string]any `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}

	byID := map[string]map[string]any{}
	for _, in := range doc.Inputs {
		byID[in["id"].(string)] = in
	}

	zero := map[string]any{"timeout": 0.0, "connection": 0.0, "exception": 0.0, "decode": 0.0}
	oven := map[string]any{"timeout": 4.0, "connection": 0.0, "exception": 0.0, "decode": 1.0}
	for id, want := range map[string]struct {
		connected any
		errors    any
	}{
		"cell":   {true, zero},
		"oven":   {false, oven},
		"line":   {nil, nil},
		"broken": {nil, nil},
	} {
		in := byID[id]
		if got := in["connected"]; !reflect.DeepEqual(got, want.connected) {
			t.Errorf("input %s connected = %v, want %v", id, got, want.connected)
		}
		if got := in["read_errors"]; !reflect.DeepEqual(got, want.errors) {
			t.Errorf("input %s read_errors = %v, want %v", id, got, want.errors)
		}
	}
}

// pluginStatus is a status with a plugin up after two restarts and one a
// rollback could not start again.
func pluginStatus() supervisor.Status {
	return supervisor.Status{
		Applied: true,
		Version: 3,
		Plugins: []supervisor.PluginStatus{
			{ID: "demo", State: "up", Pid: 4242, Restarts: 2, RestartsByReason: map[string]uint64{"exit": 1, "memory": 1}, LastError: "the plugin used more memory than max_memory"},
			{ID: "gone", State: supervisor.StateFailed, LastError: "the plugin files on disk do not match their hashes"},
		},
		Processors: []supervisor.ProcessorStatus{
			{ID: "line-state", Plugin: "demo", State: supervisor.ProcessorUp, Refused: 4, Dropped: 9},
			{ID: "spare-state", Plugin: "demo", State: supervisor.ProcessorDown},
			{ID: "stuck", Plugin: "gone", State: supervisor.StateFailed, LastError: "its plugin is not running"},
		},
		Outputs: []supervisor.OutputStatus{{
			ID: "sink", Type: "plugin:demo", State: "sending",
			Buffer:           buffer.Stats{Critical: buffer.QueueStats{Records: 2, Bytes: 200}},
			LastConfirmation: epoch.Add(-time.Minute),
		}},
	}
}

func TestStatusJSONReportsPlugins(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	h.src.set(pluginStatus())

	_, _, body := h.get(t, http.MethodGet, "/status", "")

	var got struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	want := []map[string]any{
		{"id": "demo", "state": "up", "pid": 4242.0, "restarts": 2.0, "last_error": "the plugin used more memory than max_memory"},
		{"id": "gone", "state": "failed", "pid": 0.0, "restarts": 0.0, "last_error": "the plugin files on disk do not match their hashes"},
	}
	if !reflect.DeepEqual(got.Plugins, want) {
		t.Fatalf("plugins = %v, want %v", got.Plugins, want)
	}
}

func TestStatusJSONReportsProcessorsAndPluginOutputs(t *testing.T) {
	h := start(t, "127.0.0.1:0", "", nil)
	h.src.set(pluginStatus())

	_, _, body := h.get(t, http.MethodGet, "/status", "")

	var got struct {
		Processors []map[string]any `json:"processors"`
		Outputs    []map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	want := []map[string]any{
		{"id": "line-state", "plugin": "demo", "state": "up", "refused": 4.0, "dropped": 9.0},
		{"id": "spare-state", "plugin": "demo", "state": "down", "refused": 0.0, "dropped": 0.0},
		{"id": "stuck", "plugin": "gone", "state": "failed", "refused": 0.0, "dropped": 0.0, "last_error": "its plugin is not running"},
	}
	if !reflect.DeepEqual(got.Processors, want) {
		t.Fatalf("processors = %v, want %v", got.Processors, want)
	}
	if len(got.Outputs) != 1 || got.Outputs[0]["type"] != "plugin:demo" || got.Outputs[0]["state"] != "sending" {
		t.Fatalf("outputs = %v, want sink of type plugin:demo", got.Outputs)
	}
	if _, ok := got.Outputs[0]["clock_offset_seconds"]; ok {
		t.Fatalf("output %v has a clock offset, want none for a plugin output", got.Outputs[0])
	}
}
