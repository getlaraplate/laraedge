package plugin_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge-sdk/manifest"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

// guard bounds every wait on a real process: a deadlock guard, never a
// timing assertion.
const guard = 60 * time.Second

var epoch = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// pluginBinary and pluginManifest are the test plugin built once by TestMain
// and its canonical plugin.json.
var (
	pluginBinary   string
	pluginManifest []byte
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "laraedge-plugin-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin tests: %v\n", err)

		return 1
	}
	defer os.RemoveAll(dir)

	pluginBinary = filepath.Join(dir, exeName("testplugin"))
	build := exec.Command("go", "build", "-o", pluginBinary, "./testdata/plugin")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "plugin tests: build the test plugin: %v\n", err)

		return 1
	}
	out, err := exec.Command(pluginBinary, "--manifest").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin tests: test plugin manifest: %v\n", err)

		return 1
	}
	pluginManifest = bytes.TrimSuffix(out, []byte("\n"))

	return m.Run()
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}

	return name
}

// shortTempDir returns a temporary directory with a short path: the plugin
// socket path must fit the Unix limit.
func shortTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "lph")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// newSpec installs a copy of the test plugin with plugin.json and the given
// behaviour.json in a new directory and returns its verified Spec.
func newSpec(t *testing.T, behaviour string) plugin.Spec {
	t.Helper()

	return newSpecWithManifest(t, behaviour, pluginManifest)
}

func newSpecWithManifest(t *testing.T, behaviour string, manifestJSON []byte) plugin.Spec {
	t.Helper()

	dir := shortTempDir(t)
	binary := filepath.Join(dir, exeName("testplugin"))
	data, err := os.ReadFile(pluginBinary)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, binary, data, 0o700)
	writeFile(t, filepath.Join(dir, plugin.ManifestFile), manifestJSON, 0o600)
	setBehaviour(t, binary, behaviour)

	m, raw, err := manifest.Load(filepath.Join(dir, plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := plugin.HashFile(binary)
	if err != nil {
		t.Fatal(err)
	}

	return plugin.Spec{
		ID: "demo", Path: binary, Dir: dir, Manifest: m, ManifestRaw: raw,
		SHA256: sum, ManifestSHA256: plugin.SumHex(raw),
	}
}

// setBehaviour replaces the behaviour.json read by the next start of the
// plugin at binary.
func setBehaviour(t *testing.T, binary, behaviour string) {
	t.Helper()

	if behaviour == "" {
		behaviour = "{}"
	}
	writeFile(t, filepath.Join(filepath.Dir(binary), "behaviour.json"), []byte(behaviour), 0o600)
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

// lockedBuffer is a log sink safe for concurrent use.
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

type harness struct {
	t       *testing.T
	clk     *clock.Fake
	host    *plugin.Host
	spec    plugin.Spec
	log     *lockedBuffer
	dataDir string
}

// newHarness builds a host for spec on a fake clock, with a backoff without
// jitter, and stops it at cleanup.
func newHarness(t *testing.T, spec plugin.Spec) *harness {
	t.Helper()

	h := &harness{t: t, clk: clock.NewFake(epoch), spec: spec, log: &lockedBuffer{}, dataDir: shortTempDir(t)}
	h.host = plugin.NewHost(spec, plugin.HostOptions{
		DataDir: h.dataDir,
		Clock:   h.clk,
		Log:     slog.New(slog.NewTextHandler(h.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Rand:    func() float64 { return 1 },
	})
	t.Cleanup(h.stop)

	return h
}

// start starts the host and fails the test on an error.
func (h *harness) start() {
	h.t.Helper()

	if err := h.host.Start(context.Background()); err != nil {
		h.t.Fatalf("Start() = %v", err)
	}
}

// stop stops the host, advancing the fake clock while Stop waits on it.
func (h *harness) stop() {
	done := make(chan error, 1)
	go func() { done <- h.host.Stop(context.Background()) }()

	deadline := time.Now().Add(guard)
	for {
		select {
		case <-done:
			return
		default:
		}
		if time.Now().After(deadline) {
			h.t.Error("Stop did not return")

			return
		}
		if h.clk.PendingTimers() > 0 {
			h.clk.Advance(time.Second)
		}
		runtime.Gosched()
	}
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()

	deadline := time.Now().Add(guard)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("%s not reached; status %+v; log:\n%s", what, h.host.Status(), h.log.String())
		}
		runtime.Gosched()
	}
}

// waitRestarts waits until n restarts are counted and the host waits on one
// timer (its backoff).
func (h *harness) waitRestarts(n uint64) {
	h.t.Helper()

	h.waitFor(fmt.Sprintf("%d restarts and the backoff timer", n), func() bool {
		return h.host.Status().Restarts == n && h.clk.PendingTimers() == 1
	})
}

func (h *harness) waitUp() {
	h.t.Helper()

	h.waitFor("state up", func() bool { return h.host.Status().State == plugin.StateUp })
}

// kill ends the plugin process from outside, like a crash.
func (h *harness) kill() {
	h.t.Helper()

	pid := h.host.Status().Pid
	if pid == 0 {
		h.t.Fatal("no plugin process to kill")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		h.t.Fatal(err)
	}
}

// expectBackoff checks that the pending backoff fires after exactly d.
func (h *harness) expectBackoff(d time.Duration) {
	h.t.Helper()

	h.clk.Advance(d - time.Millisecond)
	if n := h.clk.PendingTimers(); n != 1 {
		h.t.Fatalf("the backoff fired before %v (%d timers pending)", d, n)
	}
	h.clk.Advance(time.Millisecond)
}

func TestHostStartsAndAnswersHealth(t *testing.T) {
	h := newHarness(t, newSpec(t, ""))
	h.start()

	status := h.host.Status()
	if status.State != plugin.StateUp || status.Pid == 0 || status.Restarts != 0 {
		t.Fatalf("Status() = %+v, want up with a pid and no restarts", status)
	}
	conn, generation := h.host.Conn()
	if conn == nil || generation != 1 {
		t.Fatalf("Conn() = %v, %d; want a connection, generation 1", conn, generation)
	}
	select {
	case got := <-h.host.Changes():
		if got != 1 {
			t.Fatalf("Changes() delivered %d, want 1", got)
		}
	default:
		t.Fatal("Changes() delivered nothing after the start")
	}
	resp, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health Check = %v, %v; want SERVING", resp, err)
	}
	if got := h.host.BinarySHA256(); got != h.spec.SHA256 {
		t.Fatalf("BinarySHA256() = %s, want %s", got, h.spec.SHA256)
	}

	socketDir := filepath.Join(h.dataDir, "run", "plugins", "demo")
	info, err := os.Stat(socketDir)
	if err != nil {
		t.Fatalf("socket directory: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("socket directory mode = %v, want 0700", info.Mode().Perm())
		}
		sockets, _ := filepath.Glob(filepath.Join(socketDir, "*.sock"))
		if len(sockets) != 1 {
			t.Fatalf("sockets in %s = %v, want one", socketDir, sockets)
		}
	}

	if err := h.host.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if got := h.host.Status().State; got != plugin.StateStopped {
		t.Fatalf("state after Stop = %s, want stopped", got)
	}
	if conn, _ := h.host.Conn(); conn != nil {
		t.Fatal("Conn() returned a connection after Stop")
	}
	if log := h.log.String(); strings.Contains(log, "SIGTERM") || strings.Contains(log, "killing") {
		t.Fatalf("a plugin honouring Shutdown got SIGTERM or kill:\n%s", log)
	}
	if _, err := os.Stat(socketDir); !os.IsNotExist(err) {
		t.Fatalf("socket directory after Stop: %v, want it removed", err)
	}
}

func TestHostStartAfterStopIsRefused(t *testing.T) {
	h := newHarness(t, newSpec(t, `{"env_file":"env.txt"}`))
	if err := h.host.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}

	err := h.host.Start(context.Background())
	if err == nil || err.Error() != "the plugin host is stopped" {
		t.Fatalf("Start() after Stop = %v, want the host is stopped", err)
	}
	if status := h.host.Status(); status.State != plugin.StateStopped || status.Pid != 0 {
		t.Fatalf("Status() = %+v, want stopped without a process", status)
	}
	if _, err := os.Stat(filepath.Join(h.spec.Dir, "env.txt")); !os.IsNotExist(err) {
		t.Fatalf("the plugin ran after Stop (env file: %v)", err)
	}
}

func TestHostChangesKeepsOnlyTheLatestGeneration(t *testing.T) {
	h := newHarness(t, newSpec(t, ""))
	h.start()

	h.kill()
	h.waitRestarts(1)
	h.expectBackoff(time.Second)
	h.waitUp()
	h.kill()
	h.waitRestarts(2)
	h.expectBackoff(2 * time.Second)
	h.waitUp()

	select {
	case got := <-h.host.Changes():
		if got != 3 {
			t.Fatalf("Changes() delivered %d, want the latest generation 3", got)
		}
	default:
		t.Fatal("Changes() delivered nothing")
	}
	select {
	case got := <-h.host.Changes():
		t.Fatalf("Changes() delivered a second generation %d", got)
	default:
	}
}

func TestHostStartRefusals(t *testing.T) {
	otherKinds := bytes.Replace(pluginManifest, []byte(`"types":["int32"]`), []byte(`"types":["int32","int64"]`), 1)

	tests := []struct {
		name    string
		spec    func(t *testing.T) plugin.Spec
		wantErr string
	}{
		{
			name:    "Info differs from plugin.json",
			spec:    func(t *testing.T) plugin.Spec { return newSpecWithManifest(t, "", otherKinds) },
			wantErr: "Info does not match plugin.json",
		},
		{
			name: "binary hash",
			spec: func(t *testing.T) plugin.Spec {
				s := newSpec(t, "")
				s.SHA256 = plugin.SumHex([]byte("another binary"))

				return s
			},
			wantErr: "sha256: does not match the plugin binary",
		},
		{
			name: "manifest hash",
			spec: func(t *testing.T) plugin.Spec {
				s := newSpec(t, "")
				s.ManifestSHA256 = plugin.SumHex([]byte("{}"))

				return s
			},
			wantErr: "manifest_sha256: does not match plugin.json",
		},
		{
			name:    "crash at start",
			spec:    func(t *testing.T) plugin.Spec { return newSpec(t, `{"crash_at_start":true}`) },
			wantErr: "exited before the handshake with exit code 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.spec(t))

			err := h.host.Start(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Start() = %v, want an error with %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "crashing at start") {
				t.Fatalf("the error quotes the plugin output: %v", err)
			}
			status := h.host.Status()
			if status.State != plugin.StateDown || status.Pid != 0 || status.LastError != err.Error() {
				t.Fatalf("Status() = %+v, want down with the error", status)
			}
			if conn, _ := h.host.Conn(); conn != nil {
				t.Fatal("Conn() returned a connection after a failed start")
			}
		})
	}
}

// TestHostRefusesChangedBinaryOnRestart is a Review Focus: the binary is
// replaced on disk while the plugin runs, so the next restart refuses it.
func TestHostRefusesChangedBinaryOnRestart(t *testing.T) {
	h := newHarness(t, newSpec(t, ""))
	h.start()
	<-h.host.Changes()

	h.kill()
	h.waitRestarts(1)
	data, err := os.ReadFile(h.spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Replaced by rename, as an upgrade does: the old inode may still be
	// held by the kernel for a moment after the process was reaped.
	replacement := h.spec.Path + ".new"
	writeFile(t, replacement, append(data, "replaced"...), 0o700)
	if err := os.Rename(replacement, h.spec.Path); err != nil {
		t.Fatal(err)
	}

	h.expectBackoff(time.Second)
	h.waitRestarts(2)
	h.expectBackoff(2 * time.Second)
	h.waitRestarts(3)

	status := h.host.Status()
	if status.State != plugin.StateDown || status.Pid != 0 {
		t.Fatalf("Status() = %+v, want down without a process", status)
	}
	if status.RestartsByReason[plugin.ReasonExit] != 1 || status.RestartsByReason[plugin.ReasonHash] != 2 {
		t.Fatalf("RestartsByReason = %v, want exit 1 and hash 2", status.RestartsByReason)
	}
	if !strings.Contains(status.LastError, "sha256: does not match the plugin binary") {
		t.Fatalf("LastError = %q, want the hash mismatch", status.LastError)
	}
	select {
	case g := <-h.host.Changes():
		t.Fatalf("the changed binary was started (generation %d)", g)
	default:
	}
}

// TestHostCrashLoopBackoff is a Review Focus: a plugin crashing at start
// waits 1 s, 2 s, 4 s … up to 1 min between attempts; a run shorter than a
// minute keeps the backoff, a stable minute resets it.
func TestHostCrashLoopBackoff(t *testing.T) {
	h := newHarness(t, newSpec(t, ""))
	h.start()

	setBehaviour(t, h.spec.Path, `{"crash_at_start":true}`)
	h.kill()
	waits := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, wait := range waits {
		h.waitRestarts(uint64(i + 1))
		h.expectBackoff(wait * time.Second)
	}
	h.waitRestarts(uint64(len(waits) + 1))
	if got := h.host.Status().RestartsByReason[plugin.ReasonExit]; got != uint64(len(waits)+1) {
		t.Fatalf("exit restarts = %d, want %d", got, len(waits)+1)
	}

	setBehaviour(t, h.spec.Path, "")
	h.expectBackoff(time.Minute)
	h.waitUp()

	h.kill()
	h.waitRestarts(uint64(len(waits) + 2))
	h.expectBackoff(time.Minute)
	h.waitUp()

	h.clk.Advance(time.Minute)
	h.kill()
	h.waitRestarts(uint64(len(waits) + 3))
	h.expectBackoff(time.Second)
	h.waitUp()
}

// TestHostStderrFloodIsRateLimited is a Review Focus: a plugin writing
// far more than the pipe holds still completes its handshake, and the log
// gets the burst of 200 lines and one WARN counting the rest.
func TestHostStderrFloodIsRateLimited(t *testing.T) {
	const lines = 5000
	h := newHarness(t, newSpec(t, fmt.Sprintf(`{"stderr_lines":%d}`, lines)))
	h.start()

	if err := h.host.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	log := h.log.String()
	if got := strings.Count(log, `msg="plugin output"`); got != 200 {
		t.Fatalf("logged %d plugin output lines, want the burst of 200", got)
	}
	if !strings.Contains(log, "level=INFO msg=\"plugin output\" plugin=demo stream=stderr") {
		t.Fatalf("output lines are not INFO with plugin and stream:\n%s", log[:min(len(log), 400)])
	}
	want := fmt.Sprintf(`level=WARN msg="plugin output lines dropped by the rate limit" plugin=demo dropped=%d`, lines-200)
	if strings.Count(log, "lines dropped") != 1 || !strings.Contains(log, want) {
		t.Fatalf("want one WARN %q in the log", want)
	}
}

func TestHostEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("TZ", "UTC")
	t.Setenv("LARAPLATE_EDGE_TEST_SECRET", "not for plugins")

	h := newHarness(t, newSpec(t, `{"env_file":"env.txt"}`))
	h.start()

	data, err := os.ReadFile(filepath.Join(h.spec.Dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	values := map[string]string{}
	for line := range strings.Lines(string(data)) {
		key, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		keys = append(keys, key)
		values[key] = value
	}
	if values["PATH"] != "/usr/bin:/bin" || values["TZ"] != "UTC" {
		t.Fatalf("PATH, TZ = %q, %q; want the agent's values", values["PATH"], values["TZ"])
	}
	slices.Sort(keys)

	want := []string{
		"LARAPLATE_EDGE_PLUGIN_COOKIE", "LARAPLATE_EDGE_PLUGIN_PROTOCOLS", "LARAPLATE_EDGE_PLUGIN_SOCKET_DIR", "PATH", "TZ",
	}
	if runtime.GOOS == "windows" {
		want = []string{"LARAPLATE_EDGE_PLUGIN_COOKIE", "LARAPLATE_EDGE_PLUGIN_PROTOCOLS", "PATH", "SystemRoot", "TZ"}
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("plugin environment keys = %v, want %v", keys, want)
	}
}
