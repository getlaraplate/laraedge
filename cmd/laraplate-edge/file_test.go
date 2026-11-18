package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fileEnv is a document with one file input "cmm-line" watching a directory,
// and a bootstrap whose data_dir holds the ledger.
type fileEnv struct {
	dir       string
	dataDir   string
	config    string
	bootstrap string
}

func newFileEnv(t *testing.T) fileEnv {
	t.Helper()

	root := t.TempDir()
	env := fileEnv{
		dir:       filepath.Join(root, "watched"),
		dataDir:   filepath.Join(root, "data"),
		config:    filepath.Join(root, "config.yaml"),
		bootstrap: filepath.Join(root, "bootstrap.yaml"),
	}
	for _, d := range []string{env.dir, env.dataDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, env.config, `schema: laraplate-edge-config/1
inputs:
  - id: cmm-line
    driver: file
    connection: {directory: '`+env.dir+`', format: csv, interval: 1s, stable_for: 1s}
    devices:
      - device: cmm
        points:
          - {signal: bore, address: Bore, type: float64, read: event}
          - {signal: depth, address: Depth, type: float64, read: event}
outputs:
  - {id: main, type: laraplate-http, url: 'http://127.0.0.1:8000', token: secret://env/TOKEN}
`)
	writeFile(t, env.bootstrap, bootstrapYAML("edge-1", env.config, env.dataDir))

	return env
}

// report writes a file of the watched directory modified age ago.
func (e fileEnv) report(t *testing.T, name, content string, age time.Duration) {
	t.Helper()

	p := filepath.Join(e.dir, name)
	writeFile(t, p, content)
	mod := time.Now().Add(-age)
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestProbeAFileInputPrintsTheSamplesAndTheSource(t *testing.T) {
	env := newFileEnv(t)
	env.report(t, "old.csv", "Bore,Other\n1,2\n", 3*time.Hour)
	env.report(t, "new.csv", "Bore,Other\n12.5,2\n", time.Hour)

	r := runCLI(t, "", "probe", "cmm-line", "--config", env.config, "--bootstrap", env.bootstrap)
	if r.code != exitOK || r.stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q", r.code, r.stderr)
	}

	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	want := []string{
		"cmm\tbore\t12.5\tfloat\tgood",
		"cmm\tdepth\tmissing_field",
		"source: new.csv",
	}
	if len(lines) != len(want)+1 {
		t.Fatalf("stdout = %q", r.stdout)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i+1, lines[i], want[i])
		}
	}
	if !strings.HasPrefix(lines[3], "response time: ") {
		t.Errorf("last line = %q", lines[3])
	}
}

func TestProbeAFileInputWithoutAFileSaysNoFile(t *testing.T) {
	env := newFileEnv(t)

	r := runCLI(t, "", "probe", "cmm-line", "--config", env.config)
	if r.code != exitOK {
		t.Fatalf("exit code = %d, stderr = %q", r.code, r.stderr)
	}
	if !strings.HasPrefix(r.stdout, "cmm\tbore\tno file\ncmm\tdepth\tno file\nresponse time: ") || strings.Contains(r.stdout, "source:") {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestProbeAFileInputWithAnUnlistableDirectoryFails(t *testing.T) {
	env := newFileEnv(t)
	if err := os.Remove(env.dir); err != nil {
		t.Fatal(err)
	}

	r := runCLI(t, "", "probe", "cmm-line", "--config", env.config)
	if r.code != exitFailure || r.stdout != "" || !strings.Contains(r.stderr, "the directory cannot be listed") || strings.Contains(r.stderr, env.dir) {
		t.Errorf("probe: %+v", r)
	}
}

func TestProbeAFileInputWritesNothing(t *testing.T) {
	env := newFileEnv(t)
	// A first agent run would have recorded it; here the probe is run twice
	// on the same directory: it never records, so the file is probed twice.
	env.report(t, "a.csv", "Bore,Depth\n1,2\n", time.Hour)

	for range 2 {
		r := runCLI(t, "", "probe", "cmm-line", "--config", env.config, "--bootstrap", env.bootstrap)
		if r.code != exitOK || !strings.Contains(r.stdout, "source: a.csv\n") {
			t.Fatalf("probe: %+v", r)
		}
	}
	entries, err := os.ReadDir(env.dataDir)
	if err != nil || len(entries) != 0 {
		t.Errorf("data_dir = %v, err = %v", entries, err)
	}
}

var browseFileLine = regexp.MustCompile(`^\S+  \d+ B  \d+s  (waiting|ready|seen|incomplete)$`)

func TestBrowseAFileInputPrintsFilesAndAddresses(t *testing.T) {
	env := newFileEnv(t)
	env.report(t, "old.csv", "Bore,Depth\n1,2\n", 3*time.Hour)
	env.report(t, "new.csv", "Bore,Depth\n12.5,x\n", time.Hour)

	r := runCLI(t, "", "browse", "cmm-line", "--config", env.config, "--bootstrap", env.bootstrap)
	if r.code != exitOK || r.stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q", r.code, r.stderr)
	}

	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("stdout = %q", r.stdout)
	}
	for i, name := range []string{"new.csv", "old.csv"} {
		if !browseFileLine.MatchString(lines[i]) || !strings.HasPrefix(lines[i], name+"  ") || !strings.HasSuffix(lines[i], "  ready") {
			t.Errorf("line %d = %q", i+1, lines[i])
		}
	}
	rest := strings.Join(lines[2:], "\n")
	if want := "\naddresses of new.csv:\n  Bore  \"12.5\"\n  Depth  \"x\""; rest != want {
		t.Errorf("rest = %q, want %q", rest, want)
	}
}

func TestBrowseAFileInputWithoutAFile(t *testing.T) {
	env := newFileEnv(t)

	r := runCLI(t, "", "browse", "cmm-line", "--config", env.config)
	if r.code != exitOK || r.stdout != "no file to read\n" {
		t.Errorf("browse: %+v", r)
	}
}

func TestBrowseAFileInputLimit(t *testing.T) {
	env := newFileEnv(t)
	env.report(t, "a.csv", "Bore\n1\n", 3*time.Hour)
	env.report(t, "b.csv", "Bore\n2\n", time.Hour)

	r := runCLI(t, "", "browse", "cmm-line", "--config", env.config, "--limit", "1")
	if r.code != exitOK || strings.Count(r.stdout, " B  ") != 1 || !strings.HasPrefix(r.stdout, "b.csv  ") {
		t.Errorf("--limit 1: %+v", r)
	}

	for _, bad := range []string{"0", "1001", "-1"} {
		r = runCLI(t, "", "browse", "cmm-line", "--config", env.config, "--limit", bad)
		if r.code != exitUsage || r.stdout != "" || !strings.Contains(r.stderr, "--limit") {
			t.Errorf("--limit %s: %+v", bad, r)
		}
	}
}

func TestBrowseAFileInputRefusesTheFlagsOfOtherDrivers(t *testing.T) {
	env := newFileEnv(t)

	for _, flag := range [][]string{{"--node", "i=85"}, {"--depth", "2"}, {"--filter", "#"}, {"--duration", "2s"}} {
		r := runCLI(t, "", append([]string{"browse", "cmm-line", "--config", env.config}, flag...)...)
		if r.code != exitUsage || r.stdout != "" || !strings.Contains(r.stderr, errFileBrowseFlags) {
			t.Errorf("%v: %+v", flag, r)
		}
	}
}

func TestBrowseRefusesLimitForOtherDrivers(t *testing.T) {
	env := newMQTTEnv(t)

	r := runCLI(t, "", "browse", "line", "--config", env.config, "--limit", "5")
	if r.code != exitUsage || !strings.Contains(r.stderr, errNotFileBrowseLimit) {
		t.Errorf("browse: %+v", r)
	}
}

func TestBrowseAQDASInputPrintsCharacteristicsAndContext(t *testing.T) {
	env := newFileEnv(t)
	doc, err := os.ReadFile(env.config)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, env.config, strings.Replace(string(doc), "format: csv", "format: qdas", 1))
	env.report(t, "a.dfq", "K0100 2\nK2001/1 Bore\nK2002/1 Diameter\nK2101/1 12.5\nK2142/1 mm\n12.5\x0f3\nK0014/0 P-1\n", time.Hour)
	env.report(t, "b.dfx", "1\n", 2*time.Hour)

	r := runCLI(t, "", "browse", "cmm-line", "--config", env.config, "--bootstrap", env.bootstrap)
	if r.code != exitOK || r.stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) < 2 || !strings.HasSuffix(lines[0], "  ready") || !strings.HasSuffix(lines[1], "  incomplete") {
		t.Fatalf("stdout = %q", r.stdout)
	}
	want := "\naddresses of a.dfq:\n  Bore  \"12.5\"  Diameter  nominal 12.5  mm\n  #2  \"3\"\n\ncontext fields:\n  K0014  \"P-1\""
	if rest := strings.Join(lines[2:], "\n"); rest != want {
		t.Errorf("rest = %q, want %q", rest, want)
	}
}
