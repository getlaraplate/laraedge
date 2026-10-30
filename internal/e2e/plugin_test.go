//go:build e2e && unix

package e2e

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	// sdkExamples is the package path prefix of the SDK example plugins the
	// tests build from the SDK module the agent depends on.
	sdkExamples = "github.com/getlaraplate/laraedge-sdk/examples/"
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

	return buildSDKPlugin(t, t.TempDir(), pluginName)
}

// buildSDKPlugin builds the SDK example plugin name, from the SDK module the
// agent depends on, into root/name next to its plugin.json, and returns that
// directory.
func buildSDKPlugin(t *testing.T, root, name string) string {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(dir, name), sdkExamples+name)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the SDK example %s: %v\n%s", name, err, out)
	}

	// The manifest is what the binary prints: the agent compares it with
	// what the plugin answers.
	manifest, err := exec.Command(filepath.Join(dir, name), "--manifest").Output()
	if err != nil {
		t.Fatalf("%s --manifest: %v", name, err)
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

	return pluginStateOf(t, addr, pluginName)
}

// pluginStateOf returns the pid and state of plugin id as /status shows it.
func pluginStateOf(t *testing.T, addr, id string) (pid int, state string, restarts uint64) {
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
		if p.ID == id {
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
plugins_dir: %s
status_listen: 127.0.0.1:0
`, b.url(), tokenEnv, filepath.Join(work, "data"), dir))

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

const (
	// processorPlugin and outputPlugin are the SDK example plugins of
	// TestPluginProcessorOutput, named after their manifests.
	processorPlugin = "line-state"
	outputPlugin    = "jsonl"
	// machineDevice is the device the input reads and the processor
	// observes; lineDevice is the device the processor derives.
	machineDevice = "press-1"
	lineDevice    = "line-1"
	// stateSignal is the signal line-state reads and derives.
	stateSignal = "state"
	// processorID and sinkID are the processor and output ids.
	processorID = "line"
	sinkID      = "sink"
)

// pinnedPlugin is a plugins entry of the document for the plugin built in
// dir, pinned by "plugin hash".
func pinnedPlugin(t *testing.T, dir, id string) map[string]any {
	t.Helper()

	binarySum, manifestSum := pluginHashes(t, dir)

	return map[string]any{
		"id": id, "path": filepath.Join(dir, id), "sha256": binarySum, "manifest_sha256": manifestSum,
	}
}

// processorDocument is a document with the three SDK examples: the
// simulated-counter input reads state of press-1 every 100 ms (a counter
// adding 1 at every poll), the line-state processor derives state of line-1
// from it, and the jsonl output appends everything to sinkPath.
func processorDocument(t *testing.T, root, sinkPath string) json.RawMessage {
	t.Helper()

	return marshal(t, map[string]any{
		"schema":  "laraplate-edge-config/1",
		"version": 1,
		"plugins": []any{
			pinnedPlugin(t, filepath.Join(root, pluginName), pluginName),
			pinnedPlugin(t, filepath.Join(root, processorPlugin), processorPlugin),
			pinnedPlugin(t, filepath.Join(root, outputPlugin), outputPlugin),
		},
		"inputs": []any{map[string]any{
			"id":              counterInput,
			"driver":          "plugin:" + pluginName,
			"death_tolerance": "3s",
			"connection":      map[string]any{},
			"devices": []any{map[string]any{
				"device": machineDevice,
				"points": []any{map[string]any{
					"signal": stateSignal, "address": "counter:1", "type": "int32",
					"read": map[string]any{"poll": "100ms"}, "priority": "critical",
				}},
			}},
		}},
		"processors": []any{map[string]any{
			"id":              processorID,
			"plugin":          processorPlugin,
			"observe":         []any{machineDevice},
			"death_tolerance": "2s",
			"devices": []any{map[string]any{
				"device":  lineDevice,
				"signals": []any{map[string]any{"signal": stateSignal, "type": "int"}},
			}},
		}},
		"outputs": []any{map[string]any{
			"id":     sinkID,
			"type":   "plugin:" + outputPlugin,
			"config": map[string]any{"path": sinkPath},
			"batch":  map[string]any{"max_samples": 40, "max_wait": "200ms"},
		}},
	})
}

// sinkLine is one batch as the jsonl plugin writes it.
type sinkLine struct {
	MessageID string `json:"message_id"`
	SourceSeq uint64 `json:"source_seq"`
	Records   []struct {
		Sample *struct {
			Device  string          `json:"device"`
			Signal  string          `json:"signal"`
			TS      time.Time       `json:"ts"`
			Value   json.RawMessage `json:"value"`
			Quality string          `json:"quality"`
		} `json:"sample"`
		Event *struct {
			Kind   string `json:"kind"`
			Device string `json:"device"`
		} `json:"event"`
	} `json:"records"`
	Losses []json.RawMessage `json:"losses"`
}

// sinkRecord is one record of the jsonl file, flattened: kind is the
// quality of a sample or the kind of an event.
type sinkRecord struct {
	device, signal, kind string
	ts                   time.Time
	value                string
}

// readSink reads the batches of the jsonl file in source_seq order and
// their records in order. A line that is not a batch (one torn by a kill
// of the plugin) is skipped and counted; a missing file holds nothing.
func readSink(t *testing.T, path string) (batches []sinkLine, records []sinkRecord, torn int) {
	t.Helper()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, 0
	}
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	// The last element is what follows the last newline: a line being
	// written, not yet a batch.
	for _, line := range lines[:len(lines)-1] {
		var b sinkLine
		if json.Unmarshal([]byte(line), &b) != nil || b.MessageID == "" {
			torn++

			continue
		}
		batches = append(batches, b)
	}
	slices.SortStableFunc(batches, func(x, y sinkLine) int { return cmp.Compare(x.SourceSeq, y.SourceSeq) })
	for _, b := range batches {
		for _, r := range b.Records {
			switch {
			case r.Sample != nil:
				records = append(records, sinkRecord{
					device: r.Sample.Device, signal: r.Sample.Signal, kind: r.Sample.Quality,
					ts: r.Sample.TS, value: string(r.Sample.Value),
				})
			case r.Event != nil:
				records = append(records, sinkRecord{device: r.Event.Device, kind: r.Event.Kind})
			}
		}
	}

	return batches, records, torn
}

// countRecords counts the records of device with the given kind (a sample
// quality or an event kind), from index from on.
func countRecords(records []sinkRecord, from int, device, kind string) int {
	n := 0
	for _, r := range records[min(from, len(records)):] {
		if r.device == device && r.kind == kind {
			n++
		}
	}

	return n
}

// lastIndex returns the index of the last record of device with kind, -1
// when none.
func lastIndex(records []sinkRecord, device, kind string) int {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].device == device && records[i].kind == kind {
			return i
		}
	}

	return -1
}

// statusDoc is the part of /status TestPluginProcessorOutput reads.
type statusDoc struct {
	Processors []struct {
		ID      string `json:"id"`
		Plugin  string `json:"plugin"`
		State   string `json:"state"`
		Refused uint64 `json:"refused"`
		Dropped uint64 `json:"dropped"`
	} `json:"processors"`
	Outputs []map[string]json.RawMessage `json:"outputs"`
}

func readStatus(t *testing.T, addr string) statusDoc {
	t.Helper()

	code, body := getStatus(t, addr, "/status")
	var st statusDoc
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
		t.Fatalf("/status = %d %q (%v), want 200 JSON", code, body, err)
	}

	return st
}

// processorState returns the state of the processor in /status.
func processorState(t *testing.T, addr string) string {
	t.Helper()

	for _, p := range readStatus(t, addr).Processors {
		if p.ID == processorID {
			return p.State
		}
	}

	return ""
}

// sinkState returns the state of the jsonl output in /status and its
// unconfirmed critical records.
func sinkState(t *testing.T, addr string) (state string, buffered int64) {
	t.Helper()

	for _, out := range readStatus(t, addr).Outputs {
		var id string
		if json.Unmarshal(out["id"], &id) != nil || id != sinkID {
			continue
		}
		_ = json.Unmarshal(out["state"], &state)
		var buf struct {
			Critical struct {
				Records int64 `json:"records"`
			} `json:"critical"`
		}
		_ = json.Unmarshal(out["buffer"], &buf)

		return state, buf.Critical.Records
	}

	return "", 0
}

// TestPluginProcessorOutput: the built agent runs the SDK's
// simulated-counter as an input, line-state as a processor and jsonl as an
// output. Machine and derived samples land in the jsonl file. Killing jsonl
// loses nothing: the batch in flight is sent again and answered DUPLICATE
// when it was written. The .suspend file puts the output in suspended_auth,
// the records keep buffering, and removing it with a SIGHUP resumes
// delivery. Killing line-state while its binary is away leaves machine data
// flowing, sends its derived signal bad and then the derived device dead;
// once the binary is back the plugin restarts, the device is born again and
// derived data flows again. At the end the file holds every machine sample
// (the counter in steps of 1), every source_seq once, no loss marker, and
// every derived value is the machine value of the same instant.
func TestPluginProcessorOutput(t *testing.T) {
	b := newBackend(t)
	root := t.TempDir()
	for _, name := range []string{pluginName, processorPlugin, outputPlugin} {
		buildSDKPlugin(t, root, name)
	}
	work := t.TempDir()
	sinkPath := filepath.Join(work, "sink.jsonl")
	b.publish(1, processorDocument(t, root, sinkPath))

	boot := filepath.Join(work, "bootstrap.yaml")
	writeFile(t, boot, fmt.Sprintf(`agent_id: e2e-processor
config:
  provider: laraplate
  laraplate:
    url: %s
    token: secret://env/%s
    poll_seconds: 5
data_dir: %s
plugins_dir: %s
status_listen: 127.0.0.1:0
`, b.url(), tokenEnv, filepath.Join(work, "data"), root))

	sink := func() []sinkRecord {
		_, records, _ := readSink(t, sinkPath)

		return records
	}

	a := startAgent(t, boot)
	b.waitFor(t, a, "version 1 reported applied", func() bool { return b.reported(1, "applied") })
	b.waitFor(t, a, "machine and derived samples in the file", func() bool {
		r := sink()

		return countRecords(r, 0, machineDevice, "good") >= 5 && countRecords(r, 0, lineDevice, "good") >= 5 &&
			countRecords(r, 0, machineDevice, "birth") >= 1 && countRecords(r, 0, lineDevice, "birth") >= 1
	})

	addr := a.statusAddr(t)
	st := readStatus(t, addr)
	if len(st.Processors) != 1 || st.Processors[0].ID != processorID || st.Processors[0].Plugin != processorPlugin ||
		st.Processors[0].State != "up" || st.Processors[0].Refused != 0 || st.Processors[0].Dropped != 0 {
		t.Errorf("/status processors = %+v, want %s on %s up, nothing refused or dropped", st.Processors, processorID, processorPlugin)
	}
	for _, out := range st.Outputs {
		if _, ok := out["clock_offset_seconds"]; ok {
			t.Errorf("/status output %s has clock_offset_seconds, want none for a plugin output", out["id"])
		}
	}
	if state, _ := sinkState(t, addr); state != "sending" {
		t.Errorf("/status output %s state = %q, want sending", sinkID, state)
	}
	if code, body := getStatus(t, addr, "/metrics"); code != http.StatusOK ||
		!strings.Contains(body, `laraplate_edge_processor_up{processor="line"} 1`+"\n") ||
		!strings.Contains(body, `laraplate_edge_processor_refused_total{processor="line"} 0`+"\n") ||
		strings.Contains(body, `laraplate_edge_clock_offset_seconds{output="sink"}`) {
		t.Errorf("/metrics = %d, want processor_up 1, refused 0 and no clock offset for the plugin output:\n%s", code, body)
	}

	// jsonl is killed: its batches wait in the buffer, the plugin restarts,
	// the output configures it again and delivery goes on.
	sinkPid, _, _ := pluginStateOf(t, addr, outputPlugin)
	if err := syscall.Kill(sinkPid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill jsonl: %v", err)
	}
	b.waitFor(t, a, "jsonl restarted", func() bool {
		pid, state, restarts := pluginStateOf(t, addr, outputPlugin)

		return state == "up" && pid != sinkPid && pid > 0 && restarts >= 1
	})
	before := countRecords(sink(), 0, machineDevice, "good")
	b.waitFor(t, a, "machine samples after the jsonl restart", func() bool {
		return countRecords(sink(), 0, machineDevice, "good") >= before+10
	})

	// The .suspend file: SUSPEND_AUTH, the records keep buffering.
	suspend := sinkPath + ".suspend"
	writeFile(t, suspend, "")
	b.waitFor(t, a, "the output suspended_auth", func() bool {
		state, _ := sinkState(t, addr)

		return state == "suspended_auth"
	})
	b.waitFor(t, a, "records buffered while suspended", func() bool {
		_, buffered := sinkState(t, addr)

		return buffered >= 10
	})
	if code, body := getStatus(t, addr, "/metrics"); code != http.StatusOK ||
		!strings.Contains(body, `laraplate_edge_output_state{output="sink",state="suspended_auth"} 1`+"\n") {
		t.Errorf("/metrics = %d, want the output suspended_auth:\n%s", code, body)
	}
	// Removed, and a SIGHUP instead of the 5-minute retry.
	if err := os.Remove(suspend); err != nil {
		t.Fatal(err)
	}
	if err := a.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP: %v", err)
	}
	before = countRecords(sink(), 0, machineDevice, "good")
	b.waitFor(t, a, "delivery resumed", func() bool {
		state, _ := sinkState(t, addr)

		return state == "sending" && countRecords(sink(), 0, machineDevice, "good") >= before+20
	})

	// line-state is killed with its binary away, so it stays down past the
	// processor's death tolerance of 2 s.
	stateBinary := filepath.Join(root, processorPlugin, processorPlugin)
	if err := os.Rename(stateBinary, stateBinary+".away"); err != nil {
		t.Fatal(err)
	}
	statePid, _, _ := pluginStateOf(t, addr, processorPlugin)
	if err := syscall.Kill(statePid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill line-state: %v", err)
	}
	b.waitFor(t, a, "the derived signal bad, then the derived device dead", func() bool {
		r := sink()
		bad := lastIndex(r, lineDevice, "bad")

		return bad >= 0 && lastIndex(r, lineDevice, "death") > bad
	})
	if state := processorState(t, addr); state != "down" {
		t.Errorf("/status processor state = %q while line-state is down, want down", state)
	}
	if code, body := getStatus(t, addr, "/metrics"); code != http.StatusOK ||
		!strings.Contains(body, `laraplate_edge_processor_up{processor="line"} 0`+"\n") {
		t.Errorf("/metrics = %d, want processor_up 0:\n%s", code, body)
	}
	b.waitFor(t, a, "machine samples after the derived device died", func() bool {
		r := sink()

		return countRecords(r, lastIndex(r, lineDevice, "death"), machineDevice, "good") >= 10
	})

	if err := os.Rename(stateBinary+".away", stateBinary); err != nil {
		t.Fatal(err)
	}
	b.waitFor(t, a, "line-state restarted", func() bool {
		pid, state, _ := pluginStateOf(t, addr, processorPlugin)

		return state == "up" && pid != statePid && pid > 0
	})
	b.waitFor(t, a, "the derived device born again with derived data", func() bool {
		r := sink()
		birth := lastIndex(r, lineDevice, "birth")

		return birth > lastIndex(r, lineDevice, "death") && countRecords(r, birth, lineDevice, "good") >= 5
	})
	if state := processorState(t, addr); state != "up" {
		t.Errorf("/status processor state = %q after the restart, want up", state)
	}

	a.stop(t)

	checkSink(t, sinkPath)
	b.mu.Lock()
	for _, v := range b.violations {
		t.Error(v)
	}
	b.mu.Unlock()
	a.checkOutput(t, false)
}

// checkSink checks the jsonl file of TestPluginProcessorOutput: every
// source_seq once and in a row, no loss marker, the machine counter in
// steps of 1 (nothing lost or repeated across the kills and the
// suspension), and every good derived sample equal to a machine sample of
// the same instant.
func checkSink(t *testing.T, path string) {
	t.Helper()

	batches, records, torn := readSink(t, path)
	if len(batches) == 0 {
		t.Fatal("the jsonl file holds no batch")
	}
	t.Logf("jsonl file: %d batches, %d records, %d torn lines", len(batches), len(records), torn)
	for i, bt := range batches {
		if i > 0 && bt.SourceSeq != batches[i-1].SourceSeq+1 {
			t.Errorf("source_seq %d follows %d, want consecutive and each once", bt.SourceSeq, batches[i-1].SourceSeq)
		}
		if len(bt.Losses) > 0 {
			t.Errorf("batch %d carries loss markers %s", bt.SourceSeq, bt.Losses)
		}
	}

	machine := map[time.Time]string{}
	var last int64
	seen := false
	for _, r := range records {
		if r.device != machineDevice || r.kind != "good" {
			continue
		}
		v, err := strconv.ParseInt(r.value, 10, 64)
		if err != nil {
			t.Errorf("machine value %s is not an integer", r.value)

			continue
		}
		if seen && v != last+1 {
			t.Errorf("machine counter %d follows %d, want +1: a sample lost or repeated", v, last)
		}
		last, seen = v, true
		machine[r.ts] = r.value
	}

	derived := 0
	for _, r := range records {
		if r.device != lineDevice || r.kind != "good" {
			continue
		}
		derived++
		if r.signal != stateSignal || machine[r.ts] != r.value {
			t.Errorf("derived %s/%s = %s at %s, want the machine value %q of that instant", r.device, r.signal, r.value, r.ts, machine[r.ts])
		}
	}
	if derived == 0 {
		t.Error("no good derived sample in the file")
	}
	if n := countRecords(records, 0, machineDevice, "bad") + countRecords(records, 0, machineDevice, "death"); n > 0 {
		t.Errorf("%d bad samples or deaths of the machine device: machine data must flow untouched", n)
	}
}
