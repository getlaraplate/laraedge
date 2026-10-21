//go:build e2e && unix

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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
	args := []string{"build", "-o", agentBinary, "-ldflags", "-X github.com/getlaraplate/laraedge/internal/version.version=" + agentVersion}
	if raceEnabled() {
		args = append(args, "-race")
	}
	args = append(args, "github.com/getlaraplate/laraedge/cmd/laraplate-edge")
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
	boot := writeFileSetup(t, dir, b.url(), points)

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
	boot := writeFileSetup(t, dir, b.url(), points)

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

// TestTransforms: the presses of examples/simulated with transform chains:
// state mapped to an integer code, press_force scaled from kN to N and sent
// only when it moved by the deadband. Companion points without a chain read
// the same addresses and are configured after the chained ones, so the raw
// value of every instant reaches the backend after the transformed one and
// each transformed sample is checked against it. Every envelope validates
// against the backend schema, the births carry the chain's data types, no
// runtime error is counted, and the counters stay whole.
func TestTransforms(t *testing.T) {
	b := newBackend(t)
	dir := t.TempDir()
	boot := writeFileSetup(t, dir, b.url(), transformPoints)

	a := startAgent(t, boot)
	b.waitFor(t, a, "20 raw press_force samples and a state change", func() bool {
		samples, _ := b.received()
		forces, states := 0, 0
		for _, d := range devices {
			forces += len(samples[d]["press_force_kn"])
			states += len(samples[d]["state_text"])
		}

		return forces >= 20 && states > len(devices)
	})

	addr := a.statusAddr(t)
	if code, body := getStatus(t, addr, "/metrics"); code != http.StatusOK ||
		!strings.Contains(body, `laraplate_edge_transform_errors_total{input="press-line"} 0`+"\n") {
		t.Errorf("/metrics = %d, want transform_errors_total 0 for press-line:\n%s", code, body)
	}
	code, body := getStatus(t, addr, "/status")
	var st struct {
		Inputs []struct {
			ID              string  `json:"id"`
			TransformErrors *uint64 `json:"transform_errors"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
		t.Fatalf("/status = %d %q (%v), want 200 JSON", code, body, err)
	}
	if len(st.Inputs) != 1 || st.Inputs[0].TransformErrors == nil || *st.Inputs[0].TransformErrors != 0 {
		t.Errorf("/status inputs = %s, want press-line with transform_errors 0", body)
	}

	a.stop(t)

	b.checkDelivery(t, []*agent{a})
	checkTransforms(t, b)
	a.checkOutput(t, false)
}

// checkStatusEndpoints asks the four status endpoints of a running agent.
func checkStatusEndpoints(t *testing.T, addr string) {
	t.Helper()

	get := func(path string) (int, string) {
		t.Helper()

		return getStatus(t, addr, path)
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

// getStatus asks path of the status endpoint at addr and returns the status
// code and the body.
func getStatus(t *testing.T, addr, path string) (int, string) {
	t.Helper()

	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
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

// stateCodes is the map table of the state point: every state of the
// scenario has a code, so no sample is a runtime error.
var stateCodes = map[string]int64{"running": 1, "idle": 2, "fault": 3}

const (
	// forceScale turns kN into N.
	forceScale = 1000.0
	// forceDeadband is the deadband of the scaled force, in N.
	forceDeadband = 2000.0
	// deadbandTolerance is the relative tolerance of the agent's deadband
	// comparison (transform.thresholdTolerance).
	deadbandTolerance = 1e-9
)

// transformPoints are the points of every press in TestTransforms: those of
// examples/simulated with chains on state and press_force, each followed by
// a companion point without a chain on the same address.
var transformPoints = []map[string]any{
	{"signal": "state", "address": "state", "type": "string", "read": "subscribe", "priority": "critical",
		"transforms": []any{map[string]any{"map": map[string]any{"table": stateCodes}}}},
	{"signal": "state_text", "address": "state", "type": "string", "read": "subscribe", "priority": "critical"},
	{"signal": "good_count", "address": "good_count", "type": "uint32", "unit": "pcs", "read": "subscribe", "priority": "critical"},
	{"signal": "scrap_count", "address": "scrap_count", "type": "uint32", "unit": "pcs", "read": "subscribe", "priority": "critical"},
	{"signal": "press_force", "address": "press_force", "type": "float64", "unit": "N", "read": "subscribe", "priority": "bulk",
		"transforms": []any{map[string]any{"scale": forceScale}, map[string]any{"deadband": forceDeadband}}},
	{"signal": "press_force_kn", "address": "press_force", "type": "float64", "unit": "kN", "read": "subscribe", "priority": "bulk"},
	{"signal": "die_temperature", "address": "die_temperature", "type": "float64", "unit": "C", "read": "subscribe", "priority": "bulk"},
}

// checkTransforms checks what TestTransforms delivered. For each press: every
// birth announces state as number (the map produces integers), state_text as
// string and both forces as number; every raw state has a mapped state of
// the same instant with its code; the force samples are exactly the raw
// forces scaled and filtered by the deadband, replayed here; every
// transformed sample is good. A transformed sample after the last raw sample
// of its point is not checked: its raw companion was still in the buffer at
// the stop.
func checkTransforms(t *testing.T, b *backend) {
	t.Helper()

	samples, births := b.received()
	wantTypes := map[string]string{"state": "number", "state_text": "string", "press_force": "number", "press_force_kn": "number"}
	raws, dropped := 0, 0
	for _, d := range devices {
		if len(births[d]) == 0 {
			t.Errorf("%s: no birth", d)
		}
		for _, signals := range births[d] {
			got := map[string]string{}
			for _, s := range signals {
				got[s.Signal] = s.DataType
			}
			for signal, want := range wantTypes {
				if got[signal] != want {
					t.Errorf("%s: birth announces %s as %q, want %q", d, signal, got[signal], want)
				}
			}
		}

		rawStates := samples[d]["state_text"]
		mapped := within(samples[d]["state"], rawStates)
		if len(mapped) != len(rawStates) {
			t.Errorf("%s: %d mapped states for %d raw states", d, len(mapped), len(rawStates))
		}
		byTS := map[time.Time]receivedSample{}
		for _, m := range mapped {
			byTS[m.TS] = m
		}
		for _, r := range rawStates {
			var name string
			if err := json.Unmarshal(r.Value, &name); err != nil {
				t.Errorf("%s: raw state %s is not a string", d, r.Value)

				continue
			}
			m, ok := byTS[r.TS]
			if !ok {
				t.Errorf("%s: raw state %q at %s has no mapped state", d, name, r.TS)

				continue
			}
			if want := strconv.FormatInt(stateCodes[name], 10); string(m.Value) != want || m.Quality != "good" {
				t.Errorf("%s: state %q at %s mapped to %s (%s), want %s (good)", d, name, r.TS, m.Value, m.Quality, want)
			}
		}

		type forceSample struct {
			ts    time.Time
			value float64
		}
		var want []forceSample
		sent, last := false, 0.0
		rawForces := samples[d]["press_force_kn"]
		raws += len(rawForces)
		for _, r := range rawForces {
			v, err := strconv.ParseFloat(string(r.Value), 64)
			if err != nil {
				t.Errorf("%s: raw force %s is not a number", d, r.Value)

				continue
			}
			scaled := v * forceScale
			diff := math.Abs(scaled - last)
			if sent && (diff == 0 || diff < forceDeadband*(1-deadbandTolerance)) {
				dropped++

				continue
			}
			sent, last = true, scaled
			want = append(want, forceSample{ts: r.TS, value: scaled})
		}
		got := within(samples[d]["press_force"], rawForces)
		if len(got) != len(want) {
			t.Errorf("%s: %d forces sent, want %d (of %d raw)", d, len(got), len(want), len(rawForces))

			continue
		}
		for i, g := range got {
			v, err := strconv.ParseFloat(string(g.Value), 64)
			if err != nil || !g.TS.Equal(want[i].ts) || math.Abs(v-want[i].value) > 1e-9*math.Abs(want[i].value) || g.Quality != "good" {
				t.Errorf("%s: force %d = %s at %s (%s), want %v at %s (good)", d, i, g.Value, g.TS, g.Quality, want[i].value, want[i].ts)
			}
		}
	}
	if dropped == 0 {
		t.Errorf("the deadband dropped none of %d raw forces: the check proves nothing", raws)
	}
	t.Logf("raw forces %d, dropped by the deadband %d", raws, dropped)
}

// within returns the samples of chained whose ts is not after the last
// sample of raw.
func within(chained, raw []receivedSample) []receivedSample {
	if len(raw) == 0 {
		return nil
	}
	horizon := raw[len(raw)-1].TS
	var out []receivedSample
	for _, s := range chained {
		if !s.TS.After(horizon) {
			out = append(out, s)
		}
	}

	return out
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
// dir, with pts as the points of every press: the scenario file (JSON, which
// the agent reads like YAML), resolved against the configuration file's
// directory, the document as YAML and the bootstrap. It returns the bootstrap
// path.
func writeFileSetup(t *testing.T, dir, url string, pts []map[string]any) string {
	t.Helper()

	writeFile(t, filepath.Join(dir, "scenario.json"), string(marshal(t, scenario())))

	var doc strings.Builder
	fmt.Fprintf(&doc, "schema: laraplate-edge-config/1\nversion: 1\ninputs:\n  - id: press-line\n    driver: simulated\n    connection:\n      scenario_file: scenario.json\n      seed: 42\n    devices:\n")
	for _, d := range devices {
		fmt.Fprintf(&doc, "      - device: %s\n        points:\n", d)
		for _, p := range pts {
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
