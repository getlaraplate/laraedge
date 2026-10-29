//go:build e2e && unix

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

const (
	// sdkExample is the package of the SDK example plugin the test builds
	// from the SDK module the agent depends on.
	sdkExample = "github.com/getlaraplate/laraedge-sdk/examples/simulated-counter"
	// pluginName is the name in the example's plugin.json, and so the name of
	// its binary.
	pluginName = "simulated-counter"
	// counterInput is the input the test documents declare.
	counterInput = "counter-line"
)

// buildCounterPlugin builds the SDK's simulated-counter next to its
// plugin.json in a new directory and returns the directory.
func buildCounterPlugin(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, pluginName), sdkExample)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the SDK example: %v\n%s", err, out)
	}

	// The manifest is what the binary prints: the agent compares it with
	// what the plugin answers.
	manifest, err := exec.Command(filepath.Join(dir, pluginName), "--manifest").Output()
	if err != nil {
		t.Fatalf("%s --manifest: %v", pluginName, err)
	}
	writeFile(t, filepath.Join(dir, "plugin.json"), string(manifest))

	return dir
}

// pluginHashes returns the two hashes "plugin hash" prints for dir.
func pluginHashes(t *testing.T, dir string) (binary, manifest string) {
	t.Helper()

	out, err := exec.Command(agentBinary, "plugin", "hash", dir).Output()
	if err != nil {
		t.Fatalf("plugin hash: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "sha256: "); ok {
			binary = v
		}
		if v, ok := strings.CutPrefix(line, "manifest_sha256: "); ok {
			manifest = v
		}
	}
	if binary == "" || manifest == "" {
		t.Fatalf("plugin hash printed %q, want both hashes", out)
	}

	return binary, manifest
}

// counterDocument is a document using the plugin at dir, pinned to the given
// binary hash, with one point polled every 100 ms.
func counterDocument(t *testing.T, version int64, url, dir, binarySum, manifestSum string) json.RawMessage {
	t.Helper()

	return marshal(t, map[string]any{
		"schema":  "laraplate-edge-config/1",
		"version": version,
		"plugins": []any{map[string]any{
			"id":              pluginName,
			"path":            filepath.Join(dir, pluginName),
			"sha256":          binarySum,
			"manifest_sha256": manifestSum,
		}},
		"inputs": []any{map[string]any{
			"id":              counterInput,
			"driver":          "plugin:" + pluginName,
			"death_tolerance": "3s",
			"connection":      map[string]any{},
			"devices": []any{map[string]any{
				"device": "press-1",
				"points": []any{map[string]any{
					"signal": "strokes", "address": "counter:1", "type": "int32",
					"read": map[string]any{"poll": "100ms"}, "priority": "critical",
				}},
			}},
		}},
		"outputs": []any{map[string]any{
			"id": "laraplate", "type": "laraplate-http", "url": url, "token": "secret://env/" + tokenEnv,
			"batch": map[string]any{"max_samples": 40, "max_wait": "200ms"},
		}},
	})
}

// pluginState returns the pid and state of the plugin as /status shows it.
func pluginState(t *testing.T, addr string) (pid int, state string, restarts uint64) {
	t.Helper()

	code, body := getStatus(t, addr, "/status")
	var st struct {
		Plugins []struct {
			ID       string `json:"id"`
			State    string `json:"state"`
			Pid      int    `json:"pid"`
			Restarts uint64 `json:"restarts"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
		t.Fatalf("/status = %d %q (%v), want 200 JSON", code, body, err)
	}
	for _, p := range st.Plugins {
		if p.ID == pluginName {
			return p.Pid, p.State, p.Restarts
		}
	}

	return 0, "", 0
}

// countSamples counts the samples of the device with the given quality.
func countSamples(events []busEvent, quality string) int {
	n := 0
	for _, e := range events {
		if e.kind == "sample" && e.quality == quality {
			n++
		}
	}

	return n
}

// TestPlugin: the built agent runs the SDK's simulated-counter as a plugin
// and delivers its data to the backend. Killing the plugin process gives a
// bad sample, a restart and data again. A new
// document pinning a wrong binary hash is not applied: the previous document
// keeps running, its plugin delivering data, and the backend is told.
func TestPlugin(t *testing.T) {
	b := newBackend(t)
	dir := buildCounterPlugin(t)
	binarySum, manifestSum := pluginHashes(t, dir)

	b.publish(1, counterDocument(t, 1, b.url(), dir, binarySum, manifestSum))
	work := t.TempDir()
	boot := filepath.Join(work, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf(`agent_id: e2e-plugin
config:
  provider: laraplate
  laraplate:
    url: %s
    token: secret://env/%s
    poll_seconds: 5
data_dir: %s
status_listen: 127.0.0.1:0
`, b.url(), tokenEnv, filepath.Join(work, "data")))

	a := startAgent(t, boot)
	b.waitFor(t, a, "version 1 reported applied", func() bool { return b.reported(1, "applied") })
	b.waitFor(t, a, "good samples of the plugin", func() bool { return countSamples(b.events(), "good") >= 5 })

	addr := a.statusAddr(t)
	pid, state, _ := pluginState(t, addr)
	if state != "up" || pid <= 0 {
		t.Fatalf("/status plugin = %q pid %d, want up with a pid", state, pid)
	}
	if code, body := getStatus(t, addr, "/metrics"); code != http.StatusOK ||
		!strings.Contains(body, `laraplate_edge_plugin_up{plugin="simulated-counter"} 1`+"\n") {
		t.Errorf("/metrics = %d, want plugin_up 1:\n%s", code, body)
	}

	// The plugin is killed: a bad sample, a restart, data again. The restart
	// is faster than the death tolerance, so the device does not die.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the plugin: %v", err)
	}
	b.waitFor(t, a, "the plugin restarted", func() bool {
		newPid, st, restarts := pluginState(t, addr)

		return st == "up" && newPid != pid && newPid > 0 && restarts >= 1
	})
	b.waitFor(t, a, "a bad sample", func() bool { return countSamples(b.events(), "bad") >= 1 })
	before := countSamples(b.events(), "good")
	b.waitFor(t, a, "good samples after the restart", func() bool { return countSamples(b.events(), "good") >= before+5 })
	if code, body := getStatus(t, addr, "/metrics"); code != http.StatusOK ||
		!strings.Contains(body, `laraplate_edge_plugin_restarts_total{plugin="simulated-counter",reason="exit"} 1`+"\n") {
		t.Errorf("/metrics = %d, want one restart for reason exit:\n%s", code, body)
	}

	// Version 2 pins a wrong binary hash: it is not applied.
	pidBefore, _, _ := pluginState(t, addr)
	wrong := strings.Repeat("0", len(binarySum))
	b.publish(2, counterDocument(t, 2, b.url(), dir, wrong, manifestSum))
	b.waitFor(t, a, "version 2 not applied", func() bool {
		return b.reported(2, "rejected")
	})
	if b.reported(2, "applied") {
		t.Errorf("version 2 was reported applied with a wrong hash")
	}
	if slices.ContainsFunc(b.reportErrors(2), func(e string) bool { return strings.Contains(e, wrong) }) {
		t.Errorf("a report of version 2 holds the hash")
	}
	if pidAfter, st, _ := pluginState(t, addr); st != "up" || pidAfter != pidBefore {
		t.Errorf("plugin after the rejected document = %q pid %d, want up with the same pid %d (nothing stopped)", st, pidAfter, pidBefore)
	}
	before = countSamples(b.events(), "good")
	b.waitFor(t, a, "good samples after the refused document", func() bool { return countSamples(b.events(), "good") >= before+5 })

	lastPid, _, _ := pluginState(t, addr)
	a.stop(t)

	if err := syscall.Kill(lastPid, 0); err != syscall.ESRCH {
		t.Errorf("kill(plugin pid, 0) after the stop = %v, want ESRCH (the plugin is gone)", err)
	}
	if _, err := os.Stat(filepath.Join(work, "data", "run", "plugins", pluginName)); err == nil {
		t.Errorf("the plugin socket directory is still there after a clean stop")
	}
	b.mu.Lock()
	for _, v := range b.violations {
		t.Error(v)
	}
	b.mu.Unlock()
	a.checkOutput(t, false)
}

// reportErrors returns the errors of every report of version.
func (b *backend) reportErrors(version int64) []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []string
	for _, r := range b.reports {
		if r.Version == version {
			out = append(out, r.Errors...)
		}
	}

	return out
}
