package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pluginDoc is a configuration document and the directory it lives in, with
// one plugin installed under plugins/<id>/.
type pluginDoc struct {
	config string
	dir    string
}

// installPluginDoc copies binary to plugins/<id>/<name of binary> next to a
// new document that declares it as plugin <id>, pinned by the hashes of the
// files, with inputs as the JSON array of the document's inputs. manifest is
// the plugin.json content; extra files (name to content) go next to the
// binary.
func installPluginDoc(t *testing.T, id, binary string, manifestJSON []byte, inputs string, extra map[string]string) pluginDoc {
	t.Helper()

	dir := shortTempDir(t)
	pluginDir := filepath.Join(dir, "plugins", id)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(pluginDir, filepath.Base(binary))
	copyFile(t, binary, installed, 0o700)
	writeFile(t, filepath.Join(pluginDir, "plugin.json"), string(manifestJSON))
	for name, content := range extra {
		writeFile(t, filepath.Join(pluginDir, name), content)
	}

	binaryBytes, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"schema":"laraplate-edge-config/1","version":1,
"plugins":[{"id":"` + id + `","path":"plugins/` + id + `/` + filepath.Base(binary) + `","sha256":"` + hashHex(binaryBytes) + `","manifest_sha256":"` + hashHex(manifestJSON) + `"}],
"outputs":[{"id":"o0","type":"laraplate-http","url":"https://example.com","token":"secret://env/T","route":{"include":["*"]}}],
"inputs":` + inputs + `}`
	config := filepath.Join(dir, "config.json")
	writeFile(t, config, doc)

	return pluginDoc{config: config, dir: dir}
}

// isolateTemp points the temporary directory of the process at a fresh one
// and returns it: a command that cleans up leaves no data_dir ("lpe*") in it.
// The tools are built first, outside it.
func isolateTemp(t *testing.T) string {
	t.Helper()

	toolsOnce.Do(buildTools)
	dir := shortTempDir(t)
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, dir)
	}

	return dir
}

// assertNoDataDir checks that the command left no temporary data_dir in tmp.
func assertNoDataDir(t *testing.T, tmp string) {
	t.Helper()

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "lpe") {
			t.Errorf("the command left its data_dir %s behind", e.Name())
		}
	}
}

// simulatedInputs is the inputs array of a document with one simulated input
// served by plugin "sim", points as given.
func simulatedInputs(points string) string {
	return `[{"id":"line","driver":"plugin:sim","connection":{"seed":3,"scenario":` + serviceScenario + `},
"devices":[{"device":"press-1","points":[` + points + `]}]}]`
}

func committedManifest(t *testing.T) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(builtinDir, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func TestProbeOfAPluginInput(t *testing.T) {
	tmp := isolateTemp(t)
	doc := installPluginDoc(t, "sim", tool(t, builtinSimulated), committedManifest(t), simulatedInputs(servicePoints), nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"probe", "line", "--config", doc.config}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d (stderr: %q)", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	want := []string{
		"press-1\tstate\t\"idle\"\tstring\tgood",
		"press-1\tforce\t182\tfloat\tgood",
	}
	if len(lines) != len(want)+1 {
		t.Fatalf("%d lines, want %d: %q", len(lines), len(want)+1, stdout.String())
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i+1, lines[i], w)
		}
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "response time: ") {
		t.Errorf("last line = %q, want the response time", last)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}

	// The plugin is stopped and its data_dir removed.
	assertNoDataDir(t, tmp)
}

func TestProbeRecordsThePluginSamples(t *testing.T) {
	isolateTemp(t)
	doc := installPluginDoc(t, "sim", tool(t, builtinSimulated), committedManifest(t), simulatedInputs(servicePoints), nil)
	record := filepath.Join(doc.dir, "samples.jsonl")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"probe", "line", "--config", doc.config, "--record", record}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d (stderr: %q)", code, stderr.String())
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Errorf("%d recorded samples, want 2: %q", n, data)
	}
}

func TestProbeOfAPluginInputFailures(t *testing.T) {
	tests := []struct {
		name   string
		inputs string
		tamper func(t *testing.T, d pluginDoc)
		want   string
	}{
		{
			name:   "the binary does not match its hash",
			inputs: simulatedInputs(servicePoints),
			tamper: func(t *testing.T, d pluginDoc) {
				f, err := os.OpenFile(filepath.Join(d.dir, "plugins", "sim", exe(builtinSimulated)), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if _, err := f.WriteString("x"); err != nil {
					t.Fatal(err)
				}
			},
			want: "plugins.0.sha256: does not match the plugin binary",
		},
		{
			name:   "the plugin refuses the configuration",
			inputs: simulatedInputs(`{"signal":"x","address":"nope","type":"float64","read":"subscribe"}`),
			want:   "offers no signal",
		},
		{
			name:   "no plugin entry",
			inputs: strings.Replace(simulatedInputs(servicePoints), "plugin:sim", "plugin:other", 1),
			want:   `no plugin entry with id "other"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := isolateTemp(t)
			doc := installPluginDoc(t, "sim", tool(t, builtinSimulated), committedManifest(t), tc.inputs, nil)
			if tc.tamper != nil {
				tc.tamper(t, doc)
			}

			var stdout, stderr bytes.Buffer
			if code := run([]string{"probe", "line", "--config", doc.config}, &stdout, &stderr); code != exitFailure {
				t.Fatalf("exit code = %d, want 1 (stdout: %q, stderr: %q)", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			assertNoDataDir(t, tmp)
		})
	}
}

func TestBrowseOfAPluginThatDoesNotBrowse(t *testing.T) {
	tmp := isolateTemp(t)
	doc := installPluginDoc(t, "sim", tool(t, builtinSimulated), committedManifest(t), simulatedInputs(servicePoints), nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"browse", "line", "--config", doc.config}, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit code = %d, want 1 (stderr: %q)", code, stderr.String())
	}
	if want := `laraplate-edge browse: input "line": ` + errPluginNoBrowse + "\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	// Refused before anything started.
	assertNoDataDir(t, tmp)
}

// browsingTestPlugin installs the test plugin, which browses when its
// behaviour.json says so, under id "tp".
func browsingTestPlugin(t *testing.T) pluginDoc {
	t.Helper()

	binary := tool(t, "testplugin")
	probeDir := shortTempDir(t)
	copyFile(t, binary, filepath.Join(probeDir, filepath.Base(binary)), 0o700)
	const behaviour = `{"browse":true}`
	writeFile(t, filepath.Join(probeDir, "behaviour.json"), behaviour)

	manifestJSON, err := exec.Command(filepath.Join(probeDir, filepath.Base(binary)), "--manifest").Output()
	if err != nil {
		t.Fatalf("test plugin --manifest: %v", err)
	}
	manifestJSON = bytes.TrimSuffix(manifestJSON, []byte("\n"))

	inputs := `[{"id":"line","driver":"plugin:tp","devices":[{"device":"d","points":[{"signal":"s","type":"int16","address":"x","read":{"poll":"1s"}}]}]}]`

	return installPluginDoc(t, "tp", binary, manifestJSON, inputs, map[string]string{"behaviour.json": behaviour})
}

func TestBrowseOfAPluginInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		// The plugin counts depth from zero (the children only); the command,
		// like OPC UA browse, from one. The test plugin answers
		// "<node>/<depth>".
		{"default depth", nil, "/1  n1  int32  read\n"},
		{"children only", []string{"--depth", "1"}, "/0  n1  int32  read\n"},
		{"a node and a depth", []string{"--node", "a/b", "--depth", "3"}, "a/b/2  n1  int32  read\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := isolateTemp(t)
			doc := browsingTestPlugin(t)

			var stdout, stderr bytes.Buffer
			args := append([]string{"browse", "line", "--config", doc.config}, tc.args...)
			if code := run(args, &stdout, &stderr); code != exitOK {
				t.Fatalf("exit code = %d (stderr: %q)", code, stderr.String())
			}
			if stdout.String() != tc.want {
				t.Errorf("stdout = %q, want %q", stdout.String(), tc.want)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
			assertNoDataDir(t, tmp)
		})
	}
}
