//go:build e2e && unix

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	// agentToken is the agent token the fake backend expects; it must never
	// appear in the agent's output.
	agentToken = "e2e-agent-token-7c1d-SECRET"
	// tokenEnv carries agentToken to the agent (secret://env/...).
	tokenEnv = "LARAPLATE_EDGE_TOKEN"
	// agentVersion is linked into the binary and expected in the reports.
	agentVersion = "e2e-1.0.0"

	ingestPath = "/api/v1/insert/mes/machine-messages"
	configPath = "/api/v1/detail/mes/machine-agent-configs"
	reportPath = "/api/v1/insert/mes/machine-agent-config-reports"

	// schemaFile is the backend envelope schema, copied verbatim.
	schemaFile = "../envelope/testdata/laraplate-machine-1.schema.json"

	// guard bounds every wait: a deadlock guard, never a timing assertion.
	guard = 90 * time.Second
	// pollEvery is the period of the bounded polling loops.
	pollEvery = 20 * time.Millisecond
)

// agentBinary is the agent built once by TestMain.
var agentBinary string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "laraplate-edge-e2e-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)

		return 1
	}
	defer os.RemoveAll(dir)

	agentBinary = filepath.Join(dir, "laraplate-edge")
	args := []string{"build", "-o", agentBinary, "-ldflags", "-X github.com/swolley/laraplate-edge/internal/version.version=" + agentVersion}
	if raceEnabled() {
		args = append(args, "-race")
	}
	args = append(args, "github.com/swolley/laraplate-edge/cmd/laraplate-edge")
	build := exec.Command("go", args...)
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build the agent: %v\n", err)

		return 1
	}

	return m.Run()
}

// raceEnabled reports whether this test binary runs with the race detector;
// the agent is then built with it too.
func raceEnabled() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, s := range info.Settings {
		if s.Key == "-race" {
			return s.Value == "true"
		}
	}

	return false
}

// TestNetworkCut: the backend answers 503, then drops the connections, for
// several backoff steps. After it recovers every source_seq from 0 is
// accepted once as new, the batch in flight is resent identical, and no
// counter sample is lost or repeated.
func TestNetworkCut(t *testing.T) {
	b := newBackend(t)
	dir := t.TempDir()
	boot := writeFileSetup(t, dir, b.url())

	a := startAgent(t, boot)
	b.waitFor(t, a, "5 batches accepted", func() bool { return b.acceptedCount() >= 5 })

	b.setMode(modeUnavailable)
	b.waitFor(t, a, "2 attempts answered 503", func() bool { return b.attemptsWith(outcomeUnavailable) >= 2 })
	b.setMode(modeReset)
	b.waitFor(t, a, "an attempt cut without an answer", func() bool { return b.attemptsWith(outcomeReset) >= 1 })

	refused := b.refusedIDs()
	lastRefused := b.maxAttemptedSeq()
	b.setMode(modeAccept)
	b.waitFor(t, a, "the backlog delivered past the outage", func() bool {
		return b.acceptedCount() > int(lastRefused)+5 && b.allAccepted(refused)
	})

	a.stop(t)

	b.checkDelivery(t, []*agent{a})
	if n := b.maxAttemptsOfOneMessage(); n < 2 {
		t.Errorf("no message was resent after the outage (at most %d attempt per message)", n)
	}
	a.checkOutput(t, false)
}

// TestKillAndRestart: the agent is killed with SIGKILL twice, once while the
// backend holds a batch without accepting it and once after it accepted a
// batch whose answer never left. Each restart resends that batch identical:
// accepted the first time, a duplicate the second; source_seq has no gap and
// nothing confirmed is lost.
func TestKillAndRestart(t *testing.T) {
	b := newBackend(t)
	dir := t.TempDir()
	boot := writeFileSetup(t, dir, b.url())

	first := startAgent(t, boot)
	b.waitFor(t, first, "3 batches accepted", func() bool { return b.acceptedCount() >= 3 })
	b.setMode(modeHold)
	b.waitFor(t, first, "a batch held unanswered", func() bool { return len(b.heldIDs()) >= 1 })
	notAccepted := b.heldIDs()[0]
	first.kill(t)
	b.setMode(modeAccept)

	second := startAgent(t, boot)
	before := b.acceptedCount()
	b.waitFor(t, second, "the held batch resent and accepted", func() bool {
		return b.allAccepted([]string{notAccepted}) && b.acceptedCount() >= before+3
	})
	heldBefore := len(b.heldIDs())
	b.setMode(modeAcceptThenHold)
	b.waitFor(t, second, "a batch accepted without an answer", func() bool { return len(b.heldIDs()) > heldBefore })
	acceptedUnanswered := b.heldIDs()[heldBefore]
	second.kill(t)
	b.setMode(modeAccept)

	third := startAgent(t, boot)
	before = b.acceptedCount()
	b.waitFor(t, third, "the accepted batch resent as a duplicate", func() bool {
		return b.duplicatesOf(acceptedUnanswered) >= 1 && b.acceptedCount() >= before+3
	})
	third.stop(t)

	b.checkDelivery(t, []*agent{first, second, third})
	first.checkOutput(t, true)
	second.checkOutput(t, true)
	third.checkOutput(t, false)
}

// TestLaraplateProvider: the agent downloads version 1, then version 2 (a
// changed batch max_wait, so the output restarts), reports both applied with
// its version, then polls with ?version=2 and gets 304. The status endpoints
// answer, and delivery stays whole across the output restart.
func TestLaraplateProvider(t *testing.T) {
	b := newBackend(t)
	b.publish(1, configDocument(t, 1, b.url(), "200ms"))
	dir := t.TempDir()
	boot := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf(`agent_id: e2e-laraplate
config:
  provider: laraplate
  laraplate:
    url: %s
    token: secret://env/%s
    poll_seconds: 5
data_dir: %s
status_listen: 127.0.0.1:0
`, b.url(), tokenEnv, filepath.Join(dir, "data")))

	a := startAgent(t, boot)
	b.waitFor(t, a, "version 1 reported applied", func() bool { return b.reported(1, "applied") })
	b.waitFor(t, a, "3 batches accepted", func() bool { return b.acceptedCount() >= 3 })

	b.publish(2, configDocument(t, 2, b.url(), "300ms"))
	b.waitFor(t, a, "version 2 reported applied", func() bool { return b.reported(2, "applied") })
	b.waitFor(t, a, "a poll with ?version=2 answered 304", func() bool { return b.configGetsWith("2", http.StatusNotModified) >= 1 })
	before := b.acceptedCount()
	b.waitFor(t, a, "batches accepted after the output restart", func() bool { return b.acceptedCount() >= before+3 })

	checkStatusEndpoints(t, a.statusAddr(t))

	a.stop(t)

	b.checkDelivery(t, []*agent{a})
	b.checkReports(t, map[int64]int{1: 1, 2: 1})
	if n := b.configGetsWith("", http.StatusOK); n != 1 {
		t.Errorf("polls without ?version= = %d, want 1 (the first)", n)
	}
	a.checkOutput(t, false)
}

// checkStatusEndpoints asks the four status endpoints of a running agent.
func checkStatusEndpoints(t *testing.T, addr string) {
	t.Helper()

	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}

		return resp.StatusCode, string(body)
	}

	if code, body := get("/healthz"); code != http.StatusOK || body != "ok\n" {
		t.Errorf("/healthz = %d %q, want 200 ok", code, body)
	}

	if code, body := get("/readyz"); code != http.StatusOK || !strings.Contains(body, `"ready":true`) {
		t.Errorf("/readyz = %d %q, want 200 ready", code, body)
	}

	code, body := get("/status")
	var st struct {
		AgentID       string `json:"agent_id"`
		AgentVersion  string `json:"agent_version"`
		ConfigApplied bool   `json:"config_applied"`
		ConfigVersion int64  `json:"config_version"`
		Inputs        []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"inputs"`
		Outputs []struct {
			ID               string     `json:"id"`
			LastConfirmation *time.Time `json:"last_confirmation"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
		t.Fatalf("/status = %d %q (%v), want 200 JSON", code, body, err)
	}
	if st.AgentID != "e2e-laraplate" || st.AgentVersion != agentVersion || !st.ConfigApplied || st.ConfigVersion != 2 {
		t.Errorf("/status = %+v, want agent e2e-laraplate %s with version 2 applied", st, agentVersion)
	}
	if len(st.Inputs) != 1 || st.Inputs[0].State != "running" {
		t.Errorf("/status inputs = %+v, want press-line running", st.Inputs)
	}
	if len(st.Outputs) != 1 || st.Outputs[0].ID != "laraplate" || st.Outputs[0].LastConfirmation == nil {
		t.Errorf("/status outputs = %+v, want laraplate with a confirmation", st.Outputs)
	}

	code, body = get("/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	for _, want := range []string{
		"laraplate_edge_config_version 2\n",
		`laraplate_edge_input_up{input="press-line"} 1` + "\n",
		`laraplate_edge_buffer_records{output="laraplate",priority="critical"} `,
		`laraplate_edge_dropped_samples_total{output="laraplate",reason="buffer_full"} 0` + "\n",
		`laraplate_edge_output_state{output="laraplate",state="suspended_auth"} 0` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q:\n%s", want, body)
		}
	}
}

// ---------------------------------------------------------------------------
// The setup the agent runs.

// scenario is the press of examples/simulated with a 50 ms tick and short
// cycles, so parts complete within seconds.
func scenario() map[string]any {
	return map[string]any{
		"tick":    "50ms",
		"initial": "running",
		"states": []any{
			map[string]any{"name": "running", "producing": true, "transitions": map[string]any{"idle": 0.01, "fault": 0.002}},
			map[string]any{"name": "idle", "transitions": map[string]any{"running": 0.2}},
			map[string]any{"name": "fault", "transitions": map[string]any{"idle": 0.1}},
		},
		"cycle_time": map[string]any{"mean": "200ms", "stddev": "50ms"},
		"scrap_rate": 0.1,
		"measurements": []any{
			map[string]any{"signal": "press_force", "mean": 182, "stddev": 3.5, "unit": "kN"},
			map[string]any{"signal": "die_temperature", "mean": 78, "stddev": 1.2, "unit": "C"},
		},
	}
}

// devices are the presses of examples/simulated.
var devices = []string{"press-07", "press-08"}

// points are the points of every press, as in examples/simulated.
var points = []map[string]any{
	{"signal": "state", "address": "state", "type": "string", "read": "subscribe", "priority": "critical"},
	{"signal": "good_count", "address": "good_count", "type": "uint32", "unit": "pcs", "read": "subscribe", "priority": "critical"},
	{"signal": "scrap_count", "address": "scrap_count", "type": "uint32", "unit": "pcs", "read": "subscribe", "priority": "critical"},
	{"signal": "press_force", "address": "press_force", "type": "float64", "unit": "kN", "read": "subscribe", "priority": "bulk"},
	{"signal": "die_temperature", "address": "die_temperature", "type": "float64", "unit": "C", "read": "subscribe", "priority": "bulk"},
}

// configDocument returns the configuration document of examples/simulated
// with the scenario inline (a downloaded document has no directory to
// resolve a scenario file against), sent to url.
func configDocument(t *testing.T, version int64, url, maxWait string) json.RawMessage {
	t.Helper()

	return marshal(t, map[string]any{
		"schema":  "laraplate-edge-config/1",
		"version": version,
		"inputs": []any{map[string]any{
			"id":         "press-line",
			"driver":     "simulated",
			"connection": map[string]any{"scenario": scenario(), "seed": 42},
			"devices":    deviceList(),
		}},
		"outputs": []any{map[string]any{
			"id":    "laraplate",
			"type":  "laraplate-http",
			"url":   url,
			"token": "secret://env/" + tokenEnv,
			"batch": map[string]any{"max_samples": 40, "max_wait": maxWait},
		}},
	})
}

func deviceList() []any {
	out := make([]any, 0, len(devices))
	for _, d := range devices {
		out = append(out, map[string]any{"device": d, "points": points})
	}

	return out
}

// writeFileSetup writes the file provider setup of examples/simulated into
// dir: the scenario file (JSON, which the agent reads like YAML), resolved
// against the configuration file's directory, the document as YAML and the
// bootstrap. It returns the bootstrap path.
func writeFileSetup(t *testing.T, dir, url string) string {
	t.Helper()

	writeFile(t, filepath.Join(dir, "scenario.json"), string(marshal(t, scenario())))

	var doc strings.Builder
	fmt.Fprintf(&doc, "schema: laraplate-edge-config/1\nversion: 1\ninputs:\n  - id: press-line\n    driver: simulated\n    connection:\n      scenario_file: scenario.json\n      seed: 42\n    devices:\n")
	for _, d := range devices {
		fmt.Fprintf(&doc, "      - device: %s\n        points:\n", d)
		for _, p := range points {
			fmt.Fprintf(&doc, "          - %s\n", marshal(t, p))
		}
	}
	fmt.Fprintf(&doc, "outputs:\n  - id: laraplate\n    type: laraplate-http\n    url: %s\n    token: secret://env/%s\n    batch: {max_samples: 40, max_wait: 200ms}\n", url, tokenEnv)
	writeFile(t, filepath.Join(dir, "config.yaml"), doc.String())

	boot := filepath.Join(dir, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf("agent_id: e2e-file\nconfig:\n  provider: file\n  file: %s\ndata_dir: %s\nstatus_listen: 127.0.0.1:0\n",
		filepath.Join(dir, "config.yaml"), filepath.Join(dir, "data")))

	return boot
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func marshal(t *testing.T, v any) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

// ---------------------------------------------------------------------------
// The agent process.

// agent is one run of the built binary.
type agent struct {
	cmd     *exec.Cmd
	stdout  *syncBuffer
	stderr  *syncBuffer
	started time.Time

	exitOnce sync.Once
	exited   chan struct{}
	exitErr  error
}

// startAgent runs the agent on the bootstrap file with the token in its
// environment and without the systemd variables of the test's own
// environment. A run still alive at the end of the test is killed.
func startAgent(t *testing.T, bootstrapFile string) *agent {
	t.Helper()

	a := &agent{stdout: &syncBuffer{}, stderr: &syncBuffer{}, exited: make(chan struct{})}
	a.cmd = exec.Command(agentBinary, "run", "--bootstrap", bootstrapFile)
	a.cmd.Env = agentEnv()
	a.cmd.Stdout, a.cmd.Stderr = a.stdout, a.stderr

	a.started = time.Now()
	if err := a.cmd.Start(); err != nil {
		t.Fatalf("start the agent: %v", err)
	}
	go func() {
		err := a.cmd.Wait()
		a.exitOnce.Do(func() {
			a.exitErr = err
			close(a.exited)
		})
	}()
	t.Cleanup(func() {
		if !a.done() {
			_ = a.cmd.Process.Kill()
			<-a.exited
		}
	})

	return a
}

func agentEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case tokenEnv, "NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID":
			continue
		}
		env = append(env, kv)
	}

	return append(env, tokenEnv+"="+agentToken)
}

// done reports whether the process has exited.
func (a *agent) done() bool {
	select {
	case <-a.exited:
		return true
	default:
		return false
	}
}

// stop sends SIGTERM and wants a clean exit.
func (a *agent) stop(t *testing.T) {
	t.Helper()

	if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	a.wait(t)
	if a.exitErr != nil {
		t.Fatalf("the agent exited with %v after SIGTERM\nstderr:\n%s", a.exitErr, a.stderr.String())
	}
}

// kill sends SIGKILL, as kill -9 does.
func (a *agent) kill(t *testing.T) {
	t.Helper()

	if err := a.cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	a.wait(t)
}

func (a *agent) wait(t *testing.T) {
	t.Helper()

	select {
	case <-a.exited:
	case <-time.After(guard):
		t.Fatalf("the agent did not exit within %s", guard)
	}
}

// statusAddr returns the address the status endpoint bound, from the start
// log line.
func (a *agent) statusAddr(t *testing.T) string {
	t.Helper()

	for _, line := range a.logLines() {
		var entry struct {
			Msg          string `json:"msg"`
			StatusListen string `json:"status_listen"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "agent: starting" {
			return entry.StatusListen
		}
	}
	t.Fatalf("no start log line with status_listen in:\n%s", a.stdout.String())

	return ""
}

// logLines returns the complete lines written to stdout.
func (a *agent) logLines() []string {
	out := a.stdout.String()
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	} else {
		return nil
	}

	return strings.Split(out, "\n")
}

// checkOutput checks what the agent wrote: JSON log lines on stdout, no
// data race, and never the token. A killed run may end on a cut line.
func (a *agent) checkOutput(t *testing.T, killed bool) {
	t.Helper()

	out, errOut := a.stdout.String(), a.stderr.String()
	if strings.Contains(out+errOut, agentToken) {
		t.Errorf("the agent output holds the token")
	}
	if strings.Contains(errOut, "DATA RACE") {
		t.Errorf("the agent reported a data race:\n%s", errOut)
	}
	if !killed && strings.TrimSpace(errOut) != "" {
		t.Errorf("stderr = %q, want empty: logs go to stdout", errOut)
	}
	lines := a.logLines()
	if len(lines) == 0 {
		t.Errorf("the agent wrote no log line")
	}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Errorf("log line %q is not JSON", line)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the writer goroutine of exec and
// the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.String()
}

// ---------------------------------------------------------------------------
// The fake backend.

// ingestMode is how the fake backend answers the ingest route.
type ingestMode int

const (
	// modeAccept accepts a new message (202) and answers a known one as a
	// duplicate (200).
	modeAccept ingestMode = iota
	// modeUnavailable answers 503.
	modeUnavailable
	// modeReset closes the connection without an answer.
	modeReset
	// modeHold keeps the request unanswered and accepts nothing.
	modeHold
	// modeAcceptThenHold accepts the message, then keeps the request
	// unanswered: the agent never learns it was accepted.
	modeAcceptThenHold
)

// Outcomes of an ingest attempt.
const (
	outcomeAccepted    = "accepted"
	outcomeDuplicate   = "duplicate"
	outcomeUnavailable = "unavailable"
	outcomeReset       = "reset"
	outcomeHeld        = "held"
)

// envelope is what the fake backend reads of an accepted envelope.
type envelope struct {
	MessageID string `json:"message_id"`
	SourceSeq uint64 `json:"source_seq"`
	Devices   []struct {
		Device  string `json:"device"`
		Type    string `json:"type"`
		Samples []struct {
			Signal string          `json:"signal"`
			TS     time.Time       `json:"ts"`
			Value  json.RawMessage `json:"value"`
		} `json:"samples"`
	} `json:"devices"`
	Losses []json.RawMessage `json:"losses"`
}

type attempt struct {
	messageID string
	seq       uint64
	outcome   string
}

type configGet struct {
	version string
	status  int
}

type report struct {
	Version      int64    `json:"version"`
	Outcome      string   `json:"outcome"`
	Errors       []string `json:"errors"`
	AgentVersion string   `json:"agent_version"`
}

// backend is a fake Laraplate on plain HTTP over loopback: the ingest
// route, validating every body against the backend envelope schema and
// checking that a message_id always comes with the same body and seq, the
// configuration route and the report route.
type backend struct {
	srv     *httptest.Server
	schema  *jsonschema.Schema
	release chan struct{}

	mu         sync.Mutex
	mode       ingestMode
	attempts   []attempt
	held       []string
	bodyOf     map[string][]byte
	seqOf      map[string]uint64
	idOfSeq    map[uint64]string
	accepted   map[uint64]envelope
	acceptedID map[string]bool
	duplicates map[string]int
	violations []string

	docs       map[int64]json.RawMessage
	active     int64
	configGets []configGet
	reports    []report
}

func newBackend(t *testing.T) *backend {
	t.Helper()

	b := &backend{
		schema:     compileSchema(t),
		release:    make(chan struct{}),
		bodyOf:     map[string][]byte{},
		seqOf:      map[string]uint64{},
		idOfSeq:    map[uint64]string{},
		accepted:   map[uint64]envelope{},
		acceptedID: map[string]bool{},
		duplicates: map[string]int{},
		docs:       map[int64]json.RawMessage{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+ingestPath, b.ingest)
	mux.HandleFunc("GET "+configPath, b.config)
	mux.HandleFunc("POST "+reportPath, b.report)
	b.srv = httptest.NewUnstartedServer(b.authorized(mux))
	b.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	b.srv.Start()
	t.Cleanup(func() {
		close(b.release)
		b.srv.Close()
	})

	return b
}

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource("laraplate-machine-1.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("laraplate-machine-1.schema.json")
	if err != nil {
		t.Fatal(err)
	}

	return schema
}

func (b *backend) url() string {
	return b.srv.URL
}

func (b *backend) violate(format string, args ...any) {
	b.violations = append(b.violations, fmt.Sprintf(format, args...))
}

// authorized answers 401 to a request without the agent token.
func (b *backend) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+agentToken {
			b.mu.Lock()
			b.violate("%s %s without the agent token", r.Method, r.URL.Path)
			b.mu.Unlock()
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Unauthenticated."})

			return
		}
		next.ServeHTTP(w, r)
	})
}

func (b *backend) ingest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err == nil {
		err = b.schema.Validate(inst)
	}
	if err != nil {
		b.mu.Lock()
		b.violate("an envelope fails the backend schema: %v\n%s", err, body)
		b.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "The envelope is invalid.", "errors": map[string]any{"body": []string{err.Error()}}})

		return
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		b.mu.Lock()
		b.violate("an envelope does not decode: %v", err)
		b.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "The envelope is invalid."})

		return
	}
	id, seq := env.MessageID, env.SourceSeq

	b.mu.Lock()
	if first, ok := b.bodyOf[id]; ok && !bytes.Equal(first, body) {
		b.violate("message %s resent with a different body", id)
	} else if !ok {
		b.bodyOf[id] = body
	}
	if s, ok := b.seqOf[id]; ok && s != seq {
		b.violate("message %s sent with source_seq %d, then %d", id, s, seq)
	}
	b.seqOf[id] = seq
	if other, ok := b.idOfSeq[seq]; ok && other != id {
		b.violate("source_seq %d sent as message %s, then as message %s", seq, other, id)
	}
	b.idOfSeq[seq] = id

	record := func(outcome string) {
		b.attempts = append(b.attempts, attempt{messageID: id, seq: seq, outcome: outcome})
	}
	switch b.mode {
	case modeUnavailable:
		record(outcomeUnavailable)
		b.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": "Service Unavailable"})

		return
	case modeReset:
		record(outcomeReset)
		b.mu.Unlock()
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}

		return
	case modeHold:
		record(outcomeHeld)
		b.held = append(b.held, id)
		b.mu.Unlock()
		b.hold(r)

		return
	}

	if b.acceptedID[id] {
		record(outcomeDuplicate)
		b.duplicates[id]++
		b.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "duplicate": true, "message_id": id})

		return
	}
	record(outcomeAccepted)
	b.accepted[seq] = env
	b.acceptedID[id] = true
	if b.mode == modeAcceptThenHold {
		b.held = append(b.held, id)
		b.mu.Unlock()
		b.hold(r)

		return
	}
	b.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "message_id": id})
}

// hold waits until the client gives the request up or the backend closes.
func (b *backend) hold(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-b.release:
	}
}

// config serves the active document: 304 when ?version= is the active
// version, 404 when there is none (MachineAgentConfigController).
func (b *backend) config(w http.ResponseWriter, r *http.Request) {
	version := r.URL.Query().Get("version")

	b.mu.Lock()
	active, doc := b.active, b.docs[b.active]
	status := http.StatusOK
	switch {
	case active == 0:
		status = http.StatusNotFound
	case version == strconv.FormatInt(active, 10):
		status = http.StatusNotModified
	}
	b.configGets = append(b.configGets, configGet{version: version, status: status})
	b.mu.Unlock()

	switch status {
	case http.StatusNotFound:
		writeJSON(w, status, map[string]any{"message": "No active configuration."})
	case http.StatusNotModified:
		w.WriteHeader(status)
	default:
		writeJSON(w, status, map[string]any{"version": active, "document": doc})
	}
}

// report records a configuration report, validated with the backend rules
// (MachineAgentConfigReportController): 201, or 422 with no record.
func (b *backend) report(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return
	}

	var fields map[string]json.RawMessage
	var rep report
	problem := ""
	switch {
	case json.Unmarshal(body, &fields) != nil:
		problem = "not a JSON object"
	case json.Unmarshal(fields["version"], &rep.Version) != nil || rep.Version < 1:
		problem = "version is not a positive integer"
	case json.Unmarshal(fields["outcome"], &rep.Outcome) != nil || !slices.Contains([]string{"applied", "rejected", "rolled_back"}, rep.Outcome):
		problem = "outcome is not applied, rejected or rolled_back"
	case fields["errors"] != nil && json.Unmarshal(fields["errors"], &rep.Errors) != nil:
		problem = "errors is not null or a list of strings"
	case len(rep.Errors) > 100:
		problem = "more than 100 errors"
	case json.Unmarshal(fields["agent_version"], &rep.AgentVersion) != nil || rep.AgentVersion == "" || len([]rune(rep.AgentVersion)) > 64:
		problem = "agent_version is not a string of 1 to 64 characters"
	}
	for _, e := range rep.Errors {
		if len([]rune(e)) > 1000 {
			problem = "an error longer than 1000 characters"
		}
	}

	b.mu.Lock()
	if problem == "" {
		if _, published := b.docs[rep.Version]; !published {
			problem = "version is not published"
		}
	}
	if problem != "" {
		b.violate("an invalid configuration report: %s: %s", problem, body)
		b.mu.Unlock()
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "The report is invalid."})

		return
	}
	b.reports = append(b.reports, rep)
	id := len(b.reports)
	b.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"status": "recorded", "id": id})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// publish makes doc the active version.
func (b *backend) publish(version int64, doc json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.docs[version] = doc
	b.active = version
}

func (b *backend) setMode(m ingestMode) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.mode = m
}

func (b *backend) acceptedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.accepted)
}

func (b *backend) attemptsWith(outcome string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0
	for _, a := range b.attempts {
		if a.outcome == outcome {
			n++
		}
	}

	return n
}

// refusedIDs returns the messages refused so far and not accepted.
func (b *backend) refusedIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	var ids []string
	for _, a := range b.attempts {
		if (a.outcome == outcomeUnavailable || a.outcome == outcomeReset) && !b.acceptedID[a.messageID] && !slices.Contains(ids, a.messageID) {
			ids = append(ids, a.messageID)
		}
	}

	return ids
}

func (b *backend) maxAttemptedSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	var top uint64
	for _, a := range b.attempts {
		top = max(top, a.seq)
	}

	return top
}

func (b *backend) allAccepted(ids []string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, id := range ids {
		if !b.acceptedID[id] {
			return false
		}
	}

	return true
}

func (b *backend) heldIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.held)
}

func (b *backend) duplicatesOf(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.duplicates[id]
}

func (b *backend) maxAttemptsOfOneMessage() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	count := map[string]int{}
	top := 0
	for _, a := range b.attempts {
		count[a.messageID]++
		top = max(top, count[a.messageID])
	}

	return top
}

func (b *backend) reported(version int64, outcome string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.ContainsFunc(b.reports, func(r report) bool { return r.Version == version && r.Outcome == outcome })
}

func (b *backend) configGetsWith(version string, status int) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0
	for _, g := range b.configGets {
		if g.version == version && g.status == status {
			n++
		}
	}

	return n
}

// waitFor polls cond until it holds; it fails at once when the agent exits
// and after guard otherwise.
func (b *backend) waitFor(t *testing.T, a *agent, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for !cond() {
		if a.done() {
			t.Fatalf("the agent exited (%v) while waiting for %s\nstderr:\n%s\nlogs:\n%s", a.exitErr, what, a.stderr.String(), a.stdout.String())
		}
		if time.Now().After(deadline) {
			b.mu.Lock()
			violations := slices.Clone(b.violations)
			b.mu.Unlock()
			t.Fatalf("waited %s for %s; violations: %q\nlogs:\n%s", guard, what, violations, a.stdout.String())
		}
		time.Sleep(pollEvery)
	}
}

// checkReports wants exactly the given number of applied reports per
// version, each with the agent version and no error.
func (b *backend) checkReports(t *testing.T, want map[int64]int) {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	got := map[int64]int{}
	for _, r := range b.reports {
		got[r.Version]++
		if r.Outcome != "applied" || r.AgentVersion != agentVersion || len(r.Errors) != 0 {
			t.Errorf("report %+v, want applied by %s without errors", r, agentVersion)
		}
	}
	for v, n := range want {
		if got[v] != n {
			t.Errorf("reports of version %d = %d, want %d", v, got[v], n)
		}
	}
}

// checkDelivery checks what the backend accepted from the runs of one
// data_dir: no violation (every body valid against the schema, a message_id
// always with the same body and seq, a seq never reused), every source_seq
// from 0 to the highest accepted once, no loss marker, the births of every
// run, and every counter of every device of every run arriving as 0, 1, 2,
// ... with no value lost or repeated. A sample belongs to the run whose start
// precedes its ts.
func (b *backend) checkDelivery(t *testing.T, runs []*agent) {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	for _, v := range b.violations {
		t.Error(v)
	}

	n := len(b.accepted)
	if n == 0 {
		t.Fatal("the backend accepted nothing")
	}
	starts := make([]time.Time, len(runs))
	for i, a := range runs {
		starts[i] = a.started.Truncate(time.Millisecond)
	}

	type key struct {
		run            int
		device, signal string
	}
	counters := map[key][]int64{}
	births := 0
	for seq := range uint64(n) {
		env, ok := b.accepted[seq]
		if !ok {
			t.Errorf("source_seq %d was never accepted, yet %d batches were: a gap", seq, n)

			continue
		}
		if len(env.Losses) > 0 {
			t.Errorf("batch %d carries loss markers: %s", seq, env.Losses)
		}
		for _, dev := range env.Devices {
			if dev.Type == "birth" {
				births++
			}
			for _, s := range dev.Samples {
				if s.Signal != "good_count" && s.Signal != "scrap_count" {
					continue
				}
				v, err := strconv.ParseInt(string(s.Value), 10, 64)
				if err != nil {
					t.Errorf("batch %d: %s %s = %s, want an integer", seq, dev.Device, s.Signal, s.Value)

					continue
				}
				run := 0
				for i, start := range starts {
					if !s.TS.Before(start) {
						run = i
					}
				}
				k := key{run, dev.Device, s.Signal}
				counters[k] = append(counters[k], v)
			}
		}
	}

	parts := 0
	for _, values := range counters {
		parts += max(len(values)-1, 0)
	}
	if parts == 0 {
		t.Errorf("no part completed: the counter check proves nothing")
	}
	if births < len(runs)*len(devices) {
		t.Errorf("births = %d, want at least %d (each device at each start)", births, len(runs)*len(devices))
	}
	for run := range runs {
		for _, d := range devices {
			for _, signal := range []string{"good_count", "scrap_count"} {
				values := counters[key{run, d, signal}]
				if len(values) == 0 {
					t.Errorf("run %d: no %s of %s, want at least its initial 0", run+1, signal, d)
				}
				for i, v := range values {
					if v != int64(i) {
						t.Errorf("run %d: %s of %s arrived as %v, want 0, 1, 2, ...: a sample lost or repeated", run+1, signal, d, values)

						break
					}
				}
			}
		}
	}
}
