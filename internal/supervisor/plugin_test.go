package supervisor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

// The test plugin of internal/plugin/testdata/plugin, built once on first
// use into testPluginDir, which TestMain removes.
var (
	testPluginDir      string
	testPluginOnce     sync.Once
	testPluginBinary   string
	testPluginManifest []byte
	testPluginErr      error
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "laraedge-supervisor-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "supervisor tests: %v\n", err)

		return 1
	}
	defer os.RemoveAll(dir)
	testPluginDir = dir

	return m.Run()
}

// builtTestPlugin builds the test plugin once and returns its binary and
// canonical plugin.json.
func builtTestPlugin(t *testing.T) (string, []byte) {
	t.Helper()

	testPluginOnce.Do(func() {
		binary := filepath.Join(testPluginDir, exeName("testplugin"))
		build := exec.Command("go", "build", "-o", binary, "../plugin/testdata/plugin")
		if out, err := build.CombinedOutput(); err != nil {
			testPluginErr = fmt.Errorf("build the test plugin: %w\n%s", err, out)

			return
		}
		out, err := exec.Command(binary, "--manifest").Output()
		if err != nil {
			testPluginErr = fmt.Errorf("test plugin manifest: %w", err)

			return
		}
		testPluginBinary, testPluginManifest = binary, bytes.TrimSuffix(out, []byte("\n"))
	})
	if testPluginErr != nil {
		t.Fatal(testPluginErr)
	}

	return testPluginBinary, testPluginManifest
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

	dir, err := os.MkdirTemp("", "lps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// installedPlugin is a copy of the test plugin in a directory of its own,
// with its plugin.json and behaviour.json.
type installedPlugin struct {
	id, dir, binary string
	sha, manifest   string
}

func installPlugin(t *testing.T, id, behaviour string) installedPlugin {
	t.Helper()

	binary, manifestJSON := builtTestPlugin(t)
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	dir := shortTempDir(t)
	p := installedPlugin{id: id, dir: dir, binary: filepath.Join(dir, exeName("testplugin"))}
	writeTestFile(t, p.binary, data, 0o700)
	writeTestFile(t, filepath.Join(dir, plugin.ManifestFile), manifestJSON, 0o600)
	if behaviour == "" {
		behaviour = "{}"
	}
	writeTestFile(t, filepath.Join(dir, "behaviour.json"), []byte(behaviour), 0o600)

	if p.sha, err = plugin.HashFile(p.binary); err != nil {
		t.Fatal(err)
	}
	p.manifest = plugin.SumHex(manifestJSON)

	return p
}

func writeTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

// entry is the plugins entry of p; extra adds fields.
func (p installedPlugin) entry(extra map[string]any) map[string]any {
	e := map[string]any{"id": p.id, "path": p.binary, "sha256": p.sha, "manifest_sha256": p.manifest}
	for k, v := range extra {
		e[k] = v
	}

	return e
}

// traceCount counts the lines of the plugin's trace file equal to line.
func (p installedPlugin) traceCount(t *testing.T, line string) int {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(p.dir, "trace"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return countItems(strings.Split(string(data), "\n"), line)
}

// pluginInput is a plugin input reading device with the int32 points
// signals, scaled by 2, through plugin pluginID; variant changes its section
// without changing what it reads.
func pluginInput(id, pluginID, device string, variant int, signals ...string) map[string]any {
	var points []any
	for _, s := range signals {
		points = append(points, map[string]any{
			"signal": s, "address": s, "type": "int32", "read": map[string]any{"poll": "1s"},
			"transforms": []any{map[string]any{"scale": 2}},
		})
	}

	return map[string]any{
		"id": id, "driver": plugin.DriverPrefix + pluginID,
		"connection": map[string]any{"variant": variant},
		"devices":    []any{map[string]any{"device": device, "points": points}},
	}
}

// pluginDocument is base (a document of the fixture) with plugins and the
// plugin inputs appended to its inputs at the end.
func (f *fixture) pluginDocument(base config.Document, plugins []any, inputs ...map[string]any) config.Document {
	f.t.Helper()

	tree := map[string]any{"schema": config.SchemaName, "version": base.Version, "plugins": plugins}
	var ins, outs []any
	for _, in := range base.Inputs {
		var raw any
		if err := json.Unmarshal(in.Raw, &raw); err != nil {
			f.t.Fatal(err)
		}
		ins = append(ins, raw)
	}
	for _, in := range inputs {
		ins = append(ins, in)
	}
	for _, out := range base.Outputs {
		var raw any
		if err := json.Unmarshal(out.Raw, &raw); err != nil {
			f.t.Fatal(err)
		}
		outs = append(outs, raw)
	}
	tree["inputs"], tree["outputs"] = orEmpty(ins), orEmpty(outs)

	return f.parse(tree)
}

func orEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}

	return v
}

// newPluginFixture is a fixture whose data directory has a short path.
func newPluginFixture(t *testing.T) (*fixture, string) {
	t.Helper()

	dataDir := shortTempDir(t)
	f := newFixture(t, func(c *Config) { c.DataDir = dataDir })
	f.dataDir = dataDir

	return f, dataDir
}

func (f *fixture) pluginStatus(id string) (PluginStatus, bool) {
	for _, p := range f.sup.Status().Plugins {
		if p.ID == id {
			return p, true
		}
	}

	return PluginStatus{}, false
}

func (f *fixture) runningPluginPid(id string) int {
	f.t.Helper()

	p, ok := f.pluginStatus(id)
	if !ok || p.State != string(plugin.StateUp) || p.Pid == 0 {
		f.t.Fatalf("plugin %s is not up: %+v", id, f.sup.Status().Plugins)
	}

	return p.Pid
}

const sampleScript = `{"trace_file":"trace","script":[{"sample":{"device":"d1","signal":"s1","value":21,"ts":"2026-10-19T08:00:00Z","quality":"good"}}]}`

// TestPluginInputRuns: a document with a plugin input starts its plugin,
// routes what the plugin sends through the point transforms and reports the
// plugin in Status.
func TestPluginInputRuns(t *testing.T) {
	f, _ := newPluginFixture(t)
	demo := installPlugin(t, "demo", sampleScript)

	f.mustApply(f.pluginDocument(f.document(1, nil, []outSpec{{id: "main"}}),
		[]any{demo.entry(nil)}, pluginInput("p1", "demo", "d1", 0, "s1")))

	f.be.waitDetail(t, "main", "birth d1 [s1:number]", 1)
	f.be.waitDetail(t, "main", "d1/s1=42 good", 1)

	st := f.sup.Status()
	if len(st.Inputs) != 1 || st.Inputs[0].ID != "p1" || st.Inputs[0].State != InputRunning {
		t.Fatalf("Status().Inputs = %+v, want p1 running", st.Inputs)
	}
	if len(st.Plugins) != 1 || st.Plugins[0].ID != "demo" || st.Plugins[0].State != string(plugin.StateUp) || st.Plugins[0].Pid == 0 {
		t.Fatalf("Status().Plugins = %+v, want demo up with a pid", st.Plugins)
	}
}

// TestPluginNoComponentUsesIsNotStarted: a plugins entry no input names
// starts no process.
func TestPluginNoComponentUsesIsNotStarted(t *testing.T) {
	f, _ := newPluginFixture(t)
	demo := installPlugin(t, "demo", sampleScript)

	f.mustApply(f.pluginDocument(f.document(1, nil, []outSpec{{id: "main"}}), []any{demo.entry(nil)}))

	if st := f.sup.Status(); len(st.Plugins) != 0 {
		t.Fatalf("Status().Plugins = %+v, want none", st.Plugins)
	}
}

// TestPluginStartFailureRollsBack: a plugin that does not start, or that
// refuses its input section, rolls the document back; the refusal is
// reported at the input's document path.
func TestPluginStartFailureRollsBack(t *testing.T) {
	tests := []struct {
		name      string
		behaviour string
		want      func([]string) bool
	}{
		{
			name:      "the plugin crashes at start",
			behaviour: `{"crash_at_start":true}`,
			want: func(errs []string) bool {
				return len(errs) == 1 && strings.HasPrefix(errs[0], "plugins.0: ")
			},
		},
		{
			name:      "the plugin refuses the input section",
			behaviour: `{"refuse_config":true}`,
			want: func(errs []string) bool {
				return slices.Equal(errs, []string{"inputs.1.connection.port: must be an integer"})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newPluginFixture(t)
			demo := installPlugin(t, "demo", tc.behaviour)
			doc1 := f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}})
			f.mustApply(doc1)

			out := f.apply(f.pluginDocument(f.document(2, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}),
				[]any{demo.entry(nil)}, pluginInput("p1", "demo", "d1", 0, "s1")))

			if out.Result != ResultRolledBack || !tc.want(out.Errors) {
				t.Fatalf("Apply() = %+v, want rolled_back", out)
			}
			st := f.sup.Status()
			if st.Version != 1 || len(st.Inputs) != 1 || st.Inputs[0].ID != "a" || st.Inputs[0].State != InputRunning {
				t.Fatalf("Status() = %+v, want version 1 with a running", st)
			}
			if len(st.Plugins) != 0 {
				t.Fatalf("Status().Plugins = %+v, want none after the rollback", st.Plugins)
			}
		})
	}
}

// TestReloadOfOnePluginInputKeepsThePluginProcess: a document changing one
// plugin input reconfigures only that instance; the plugin keeps its pid.
func TestReloadOfOnePluginInputKeepsThePluginProcess(t *testing.T) {
	f, _ := newPluginFixture(t)
	demo := installPlugin(t, "demo", sampleScript)
	base := func(version int64) config.Document { return f.document(version, nil, []outSpec{{id: "main"}}) }

	f.mustApply(f.pluginDocument(base(1), []any{demo.entry(nil)},
		pluginInput("p1", "demo", "d1", 0, "s1"), pluginInput("p2", "demo", "d2", 0, "s1")))
	pid := f.runningPluginPid("demo")

	f.mustApply(f.pluginDocument(base(2), []any{demo.entry(nil)},
		pluginInput("p1", "demo", "d1", 1, "s1", "s2"), pluginInput("p2", "demo", "d2", 0, "s1")))

	if got := f.runningPluginPid("demo"); got != pid {
		t.Fatalf("plugin pid = %d after the reload, want %d kept", got, pid)
	}
	if got := demo.traceCount(t, "configure p1"); got != 2 {
		t.Errorf("p1 configured %d times, want 2", got)
	}
	if got := demo.traceCount(t, "configure p2"); got != 1 {
		t.Errorf("p2 configured %d times, want 1 (unchanged)", got)
	}
}

// TestChangedPluginEntryRestartsThePlugin: a changed plugins entry starts a
// new plugin process and configures every instance on it again.
func TestChangedPluginEntryRestartsThePlugin(t *testing.T) {
	f, _ := newPluginFixture(t)
	demo := installPlugin(t, "demo", sampleScript)
	base := func(version int64) config.Document { return f.document(version, nil, []outSpec{{id: "main"}}) }
	inputs := []map[string]any{pluginInput("p1", "demo", "d1", 0, "s1"), pluginInput("p2", "demo", "d2", 0, "s1")}

	f.mustApply(f.pluginDocument(base(1), []any{demo.entry(nil)}, inputs...))
	pid := f.runningPluginPid("demo")

	f.mustApply(f.pluginDocument(base(2), []any{demo.entry(map[string]any{"max_memory": "1GiB"})}, inputs...))

	if got := f.runningPluginPid("demo"); got == pid {
		t.Fatalf("plugin pid = %d after the entry changed, want a new process", got)
	}
	for _, id := range []string{"p1", "p2"} {
		if got := demo.traceCount(t, "configure "+id); got != 2 {
			t.Errorf("%s configured %d times, want 2", id, got)
		}
	}
	f.be.waitDetail(t, "main", "d1/s1=42 good", 2)
}

// TestRemovingTheLastPluginComponentStopsThePlugin: the plugin of no input
// of the document is stopped, its socket directory removed.
func TestRemovingTheLastPluginComponentStopsThePlugin(t *testing.T) {
	f, dataDir := newPluginFixture(t)
	demo := installPlugin(t, "demo", sampleScript)

	f.mustApply(f.pluginDocument(f.document(1, nil, []outSpec{{id: "main"}}),
		[]any{demo.entry(nil)}, pluginInput("p1", "demo", "d1", 0, "s1")))
	f.runningPluginPid("demo")
	socketDir := filepath.Join(dataDir, "run", "plugins", "demo")
	if _, err := os.Stat(socketDir); err != nil {
		t.Fatalf("socket directory of the running plugin: %v", err)
	}

	f.mustApply(f.pluginDocument(f.document(2, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}),
		[]any{demo.entry(nil)}))

	if st := f.sup.Status(); len(st.Plugins) != 0 {
		t.Fatalf("Status().Plugins = %+v, want none", st.Plugins)
	}
	if _, err := os.Stat(socketDir); !os.IsNotExist(err) {
		t.Fatalf("socket directory after the plugin stopped: %v, want it removed", err)
	}
}

// TestCloseStopsPlugins: Close stops every plugin.
func TestCloseStopsPlugins(t *testing.T) {
	f, dataDir := newPluginFixture(t)
	demo := installPlugin(t, "demo", sampleScript)
	f.mustApply(f.pluginDocument(f.document(1, nil, nil), []any{demo.entry(nil)}, pluginInput("p1", "demo", "d1", 0, "s1")))

	if err := f.sup.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	if st := f.sup.Status(); len(st.Plugins) != 0 {
		t.Fatalf("Status().Plugins after Close = %+v, want none", st.Plugins)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "run", "plugins", "demo")); !os.IsNotExist(err) {
		t.Fatalf("socket directory after Close: %v, want it removed", err)
	}
}

// TestPluginCrashLeavesTheRestRunning: a plugin process that dies does not
// stop the other inputs or the outputs.
func TestPluginCrashLeavesTheRestRunning(t *testing.T) {
	f, _ := newPluginFixture(t)
	demo := installPlugin(t, "demo", sampleScript)
	f.mustApply(f.pluginDocument(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}),
		[]any{demo.entry(nil)}, pluginInput("p1", "demo", "d1", 0, "s1")))
	f.be.waitDetail(t, "main", "d1/s1=42 good", 1)

	proc, err := os.FindProcess(f.runningPluginPid("demo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	f.waitStatus("the plugin down", func(st Status) bool {
		return len(st.Plugins) == 1 && st.Plugins[0].State != string(plugin.StateUp) && st.Plugins[0].Restarts == 1
	})

	f.drv.instance(t, "a").send("a1", "state")
	f.be.waitItem(t, "main", "a1/state")

	st := f.sup.Status()
	for _, in := range st.Inputs {
		if in.State != InputRunning {
			t.Errorf("input %s is %s, want running", in.ID, in.State)
		}
	}
	if len(st.Inputs) != 2 || len(st.Outputs) != 1 || st.Outputs[0].State == StateFailed {
		t.Fatalf("Status() = %+v, want a, p1 and main running", st)
	}
}

// waitLogCount waits until the logs hold n lines containing text.
func (f *fixture) waitLogCount(text string, n int) {
	f.t.Helper()

	deadline := time.Now().Add(guard)
	for strings.Count(f.logs.String(), text) < n {
		if time.Now().After(deadline) {
			f.t.Fatalf("logs: %d lines with %s not reached:\n%s", n, text, f.logs.String())
		}
		runtime.Gosched()
	}
}

// TestRollbackStartsTheReplacedPluginAgain: a document that restarted a
// plugin and then failed starts the plugin again from its previous entry,
// with its inputs.
func TestRollbackStartsTheReplacedPluginAgain(t *testing.T) {
	f, _ := newPluginFixture(t)
	demo := installPlugin(t, "demo", sampleScript)
	f.mustApply(f.pluginDocument(f.document(1, nil, []outSpec{{id: "main"}}),
		[]any{demo.entry(nil)}, pluginInput("p1", "demo", "d1", 0, "s1")))
	pid := f.runningPluginPid("demo")

	f.drv.failStart["b"] = errRefused
	out := f.apply(f.pluginDocument(f.document(2, []inSpec{{id: "b", devices: []string{"b1"}}}, []outSpec{{id: "main"}}),
		[]any{demo.entry(map[string]any{"max_memory": "1GiB"})}, pluginInput("p1", "demo", "d1", 0, "s1")))

	if want := []string{"inputs.0: " + errRefused.Error()}; out.Result != ResultRolledBack || !slices.Equal(out.Errors, want) {
		t.Fatalf("Apply() = %+v, want rolled_back with %q", out, want)
	}
	if got := f.runningPluginPid("demo"); got == pid {
		t.Fatalf("plugin pid = %d, want the replaced plugin started again", got)
	}
	st := f.sup.Status()
	if st.Version != 1 || len(st.Inputs) != 1 || st.Inputs[0].ID != "p1" || st.Inputs[0].State != InputRunning {
		t.Fatalf("Status() = %+v, want version 1 with p1 running", st)
	}
	if got := demo.traceCount(t, "configure p1"); got != 2 {
		t.Errorf("p1 configured %d times, want 2 (start, rollback)", got)
	}
}
