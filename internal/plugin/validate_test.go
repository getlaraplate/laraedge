package plugin_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/bootstrap"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

const testManifest = `{"kinds":{"input":{"address_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","pattern":"^[a-z]+:[0-9]+$","type":"string"},` +
	`"browse":false,"connection_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"host":{"type":"string"},"port":{"type":"integer"},"token":{"type":"integer"}},"required":["host"],"type":"object"},` +
	`"read_modes":["poll"],"types":["int16"]}},"name":"demo","protocol_versions":["v1"],"version":"0.1.0"}`

const outputOnlyManifest = `{"kinds":{"output":{"config_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}}},"name":"demo","protocol_versions":["v1"],"version":"0.1.0"}`

const badManifest = `{"kinds":{},"name":"demo","protocol_versions":["v1"],"version":"0.1.0"}`

const testBinary = "not really a binary"

type fixture struct {
	dir      string
	manifest string
	binary   string
}

// newFixture writes plugins/demo/{plugin.json,demo} under a temp directory.
func newFixture(t *testing.T, manifest string) fixture {
	t.Helper()

	base := t.TempDir()
	dir := filepath.Join(base, "plugins", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo"), []byte(testBinary), 0o700); err != nil {
		t.Fatal(err)
	}

	return fixture{dir: base, manifest: manifest, binary: testBinary}
}

type docOptions struct {
	path, sha, manifestSHA string
	driver                 string
	connection             string
	point                  string
}

func (f fixture) document(t *testing.T, o docOptions) config.Document {
	t.Helper()

	if o.path == "" {
		o.path = "plugins/demo/demo"
	}
	if o.sha == "" {
		o.sha = plugin.SumHex([]byte(f.binary))
	}
	if o.manifestSHA == "" {
		o.manifestSHA = plugin.SumHex([]byte(f.manifest))
	}
	if o.driver == "" {
		o.driver = "plugin:demo"
	}
	if o.connection == "" {
		o.connection = `{"host":"h"}`
	}
	if o.point == "" {
		o.point = `{"signal":"s","address":"m:1","type":"int16","read":{"poll":"1s"}}`
	}

	text := fmt.Sprintf(`{"schema":"laraplate-edge-config/1","version":1,
"plugins":[{"id":"demo","path":%q,"sha256":%q,"manifest_sha256":%q}],
"outputs":[{"id":"o0","type":"laraplate-http","url":"https://example.com","token":"secret://env/T","route":{"include":["*"]}}],
"inputs":[{"id":"i0","driver":%q,"connection":%s,"devices":[{"device":"d1","points":[%s]}]}]}`,
		o.path, o.sha, o.manifestSHA, o.driver, o.connection, o.point)

	doc, msgs, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) > 0 {
		t.Fatalf("fixture document refused: %v", msgs)
	}

	return doc
}

func run(doc config.Document, paths plugin.Paths) []string {
	specs, msgs := plugin.Resolve(doc, paths)

	return append(msgs, plugin.Validate(doc, specs)...)
}

func TestResolveAndValidate(t *testing.T) {
	good := newFixture(t, testManifest)
	outputOnly := newFixture(t, outputOnlyManifest)
	bad := newFixture(t, badManifest)

	missingManifest := newFixture(t, testManifest)
	if err := os.Remove(filepath.Join(missingManifest.dir, "plugins", "demo", plugin.ManifestFile)); err != nil {
		t.Fatal(err)
	}
	missingBinary := newFixture(t, testManifest)
	if err := os.Remove(filepath.Join(missingBinary.dir, "plugins", "demo", "demo")); err != nil {
		t.Fatal(err)
	}

	zeros := strings.Repeat("0", 64)

	tests := []struct {
		name     string
		fx       fixture
		opts     docOptions
		provider string
		want     []string
	}{
		{name: "valid", fx: good},
		{name: "binary hash mismatch", fx: good, opts: docOptions{sha: zeros}, want: []string{"plugins.0.sha256: does not match the plugin binary"}},
		{name: "manifest hash mismatch", fx: good, opts: docOptions{manifestSHA: zeros}, want: []string{"plugins.0.manifest_sha256: does not match plugin.json"}},
		{name: "manifest fails Check", fx: bad, want: []string{"plugins.0.manifest: kinds: must offer at least one of input, processor, output"}},
		{name: "missing manifest", fx: missingManifest, want: []string{"plugins.0: plugin.json not found next to the binary"}},
		{name: "missing binary", fx: missingBinary, want: []string{"plugins.0.path: the plugin binary cannot be read"}},
		{name: "plugin without the input kind", fx: outputOnly, want: []string{"inputs.0.driver: the plugin does not offer the input kind"}},
		{name: "unknown plugin id", fx: good, opts: docOptions{driver: "plugin:other"}, want: []string{"inputs.0.driver: no plugin entry with this id"}},
		{name: "bad connection", fx: good, opts: docOptions{connection: `{"host":"h","port":"xyz"}`}, want: []string{"inputs.0.connection.port: "}},
		{name: "bad address", fx: good, opts: docOptions{point: `{"signal":"s","address":"BAD","type":"int16","read":{"poll":"1s"}}`}, want: []string{"inputs.0.devices.0.points.0.address: "}},
		{name: "read mode refused", fx: good, opts: docOptions{point: `{"signal":"s","address":"m:1","type":"int16","read":"subscribe"}`}, want: []string{"inputs.0.devices.0.points.0.read: read mode is not accepted by this plugin"}},
		{name: "type refused", fx: good, opts: docOptions{point: `{"signal":"s","address":"m:1","type":"float32","read":{"poll":"1s"}}`}, want: []string{"inputs.0.devices.0.points.0.type: type is not accepted by this plugin"}},
		{name: "secret:// value in a typed field (port) is not reported (R5)", fx: good, opts: docOptions{connection: `{"host":"h","port":"secret://env/PORT"}`}},
		{name: "problem at a secret key (token) is not reported (R5)", fx: good, opts: docOptions{connection: `{"host":"h","token":"secret://env/TOKEN"}`}},
		{name: "files missing: no manifest check of the sections", fx: missingManifest, opts: docOptions{connection: `{"port":"xyz"}`}, want: []string{"plugins.0: plugin.json not found next to the binary"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.opts.path == "ABS" {
				tc.opts.path = filepath.Join(tc.fx.dir, "plugins", "demo", "demo")
			}
			doc := tc.fx.document(t, tc.opts)

			got := run(doc, plugin.Paths{BaseDir: tc.fx.dir, Provider: tc.provider})

			if len(got) != len(tc.want) {
				t.Fatalf("messages = %q, want %q", got, tc.want)
			}
			for i, w := range tc.want {
				if !strings.HasPrefix(got[i], w) {
					t.Errorf("message %d = %q, want prefix %q", i, got[i], w)
				}
			}
		})
	}
}

func TestMessagesCarryNoValue(t *testing.T) {
	fx := newFixture(t, testManifest)
	secretValue := "s3cr3t-value-xyz"
	doc := fx.document(t, docOptions{
		sha:         strings.Repeat("a", 64),
		manifestSHA: strings.Repeat("b", 64),
		connection:  `{"host":"10.9.8.7","port":"` + secretValue + `"}`,
		point:       `{"signal":"s","address":"TOPSECRETADDR","type":"int16","read":{"poll":"1s"}}`,
	})

	_, msgs := plugin.Resolve(doc, plugin.Paths{BaseDir: fx.dir})
	if len(msgs) == 0 {
		t.Fatal("want hash problems")
	}
	// Resolve refused the plugin: re-resolve with right hashes to reach Validate.
	good := fx.document(t, docOptions{
		connection: `{"host":"10.9.8.7","port":"` + secretValue + `"}`,
		point:      `{"signal":"s","address":"TOPSECRETADDR","type":"int16","read":{"poll":"1s"}}`,
	})
	specs, _ := plugin.Resolve(good, plugin.Paths{BaseDir: fx.dir})
	msgs = append(msgs, plugin.Validate(good, specs)...)

	all := strings.Join(msgs, "\n")
	for _, value := range []string{secretValue, "10.9.8.7", "TOPSECRETADDR", strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if strings.Contains(all, value) {
			t.Errorf("a message contains a configured value: %q", all)
		}
	}
	if !slices.ContainsFunc(msgs, func(m string) bool { return strings.HasPrefix(m, "inputs.0.devices.0.points.0.address") }) {
		t.Errorf("want an address problem, got %q", msgs)
	}
}

func TestResolveReturnsSpecs(t *testing.T) {
	fx := newFixture(t, testManifest)
	doc := fx.document(t, docOptions{})

	specs, msgs := plugin.Resolve(doc, plugin.Paths{BaseDir: fx.dir, Provider: bootstrap.ProviderFile})
	if len(msgs) != 0 || len(specs) != 1 {
		t.Fatalf("specs = %d, msgs = %q", len(specs), msgs)
	}
	s := specs[0]
	wantDir := filepath.Join(fx.dir, "plugins", "demo")
	if s.ID != "demo" || s.Dir != wantDir || s.Path != filepath.Join(wantDir, "demo") || s.Manifest.Name != "demo" || string(s.ManifestRaw) != testManifest {
		t.Errorf("spec = %+v", s)
	}
}
