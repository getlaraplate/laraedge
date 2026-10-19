package main

import (
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// statusEndpoint is a fake status endpoint recording the requests.
type statusEndpoint struct {
	srv    *httptest.Server
	mu     sync.Mutex
	auth   []string
	paths  []string
	status int
	body   string
}

func newStatusEndpoint(t *testing.T, status int, body string) *statusEndpoint {
	t.Helper()

	e := &statusEndpoint{status: status, body: body}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.auth = append(e.auth, r.Header.Get("Authorization"))
		e.paths = append(e.paths, r.Method+" "+r.URL.Path)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.status)
		_, _ = io.WriteString(w, e.body)
	}))
	e.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	e.srv.Start()
	t.Cleanup(e.srv.Close)

	return e
}

func (e *statusEndpoint) addr() string {
	return strings.TrimPrefix(e.srv.URL, "http://")
}

func TestStatusCommand(t *testing.T) {
	const body = `{"agent_id":"plant-a-edge-01","config_version":3,"inputs":[]}`

	tests := []struct {
		name       string
		status     int
		body       string
		token      string
		wantCode   int
		wantStdout string
		wantAuth   string
		wantStderr []string
	}{
		{
			name:       "prints the status indented",
			status:     http.StatusOK,
			body:       body,
			wantCode:   0,
			wantStdout: "{\n  \"agent_id\": \"plant-a-edge-01\",\n  \"config_version\": 3,\n  \"inputs\": []\n}\n",
		},
		{
			name:     "sends the token of the environment",
			status:   http.StatusOK,
			body:     body,
			token:    "status-cli-tok-SECRET",
			wantCode: 0,
			wantAuth: "Bearer status-cli-tok-SECRET",
		},
		{
			name:       "a refusal is an error",
			status:     http.StatusUnauthorized,
			body:       `{"error":"unauthorized"}`,
			wantCode:   1,
			wantStderr: []string{"laraplate-edge status:", "401", "LARAPLATE_EDGE_STATUS_TOKEN"},
		},
		{
			name:       "an answer that is not JSON is an error",
			status:     http.StatusOK,
			body:       "<html>",
			wantCode:   1,
			wantStderr: []string{"laraplate-edge status:", "not JSON"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(statusTokenEnv, tt.token)
			e := newStatusEndpoint(t, tt.status, tt.body)
			var stdout, stderr bytes.Buffer

			code := run([]string{"status", "--listen", e.addr()}, &stdout, &stderr)

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if code != 0 && stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty on failure", stdout.String())
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if len(e.paths) != 1 || e.paths[0] != "GET /status" {
				t.Fatalf("requests = %v, want one GET /status", e.paths)
			}
			if e.auth[0] != tt.wantAuth {
				t.Errorf("Authorization = %q, want %q", e.auth[0], tt.wantAuth)
			}
			if tt.token != "" && strings.Contains(stderr.String()+stdout.String(), tt.token) {
				t.Error("the output echoes the token")
			}
		})
	}
}

func TestStatusCommandWithoutAnAgent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	var stdout, stderr bytes.Buffer

	code := run([]string{"status", "--listen", addr}, &stdout, &stderr)

	if code != exitFailure || !strings.Contains(stderr.String(), "laraplate-edge status:") || !strings.Contains(stderr.String(), addr) {
		t.Fatalf("exit code = %d, stderr = %q; want 1 naming the address", code, stderr.String())
	}
}
