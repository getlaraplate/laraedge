package plugin_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/getlaraplate/laraedge/internal/bootstrap"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

const (
	msgDisabled = "plugins.0.path: plugins are disabled: set plugins_dir in the bootstrap"
	msgOutside  = "plugins.0.path: must be inside plugins_dir"
)

// outsideCopy copies the plugin of fx, whose tree <fx.dir>/plugins is the
// plugins_dir of the tests below, into a directory outside it and returns
// that directory.
func outsideCopy(t *testing.T, fx fixture) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{plugin.ManifestFile, "demo"} {
		data, err := os.ReadFile(filepath.Join(fx.dir, "plugins", "demo", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	return out
}

func TestTrustAnchor(t *testing.T) {
	fx := newFixture(t, testManifest)
	pluginsDir := filepath.Join(fx.dir, "plugins")
	abs := filepath.Join(pluginsDir, "demo", "demo")
	outside := outsideCopy(t, fx)

	// A symlink inside plugins_dir pointing at a directory outside it.
	link := filepath.Join(pluginsDir, "link")
	symlinks := os.Symlink(outside, link) == nil

	tests := []struct {
		name     string
		path     string
		paths    plugin.Paths
		needsSym bool
		want     []string
	}{
		{name: "laraplate without plugins_dir is disabled", path: "demo/demo", paths: plugin.Paths{Provider: bootstrap.ProviderLaraplate}, want: []string{msgDisabled}},
		{name: "laraplate disabled even for an absolute path", path: abs, paths: plugin.Paths{Provider: bootstrap.ProviderLaraplate}, want: []string{msgDisabled}},
		{name: "laraplate relative path resolves under plugins_dir", path: "demo/demo", paths: plugin.Paths{Provider: bootstrap.ProviderLaraplate, PluginsDir: pluginsDir, BaseDir: t.TempDir()}},
		{name: "laraplate absolute path inside", path: abs, paths: plugin.Paths{Provider: bootstrap.ProviderLaraplate, PluginsDir: pluginsDir}},
		{name: "laraplate absolute path outside", path: filepath.Join(outside, "demo"), paths: plugin.Paths{Provider: bootstrap.ProviderLaraplate, PluginsDir: pluginsDir}, want: []string{msgOutside}},
		{name: "laraplate dot-dot escape", path: "../../" + filepath.Base(filepath.Dir(outside)) + "/elsewhere/demo", paths: plugin.Paths{Provider: bootstrap.ProviderLaraplate, PluginsDir: pluginsDir}, want: []string{msgOutside}},
		{name: "laraplate symlink escaping", path: "link/demo", paths: plugin.Paths{Provider: bootstrap.ProviderLaraplate, PluginsDir: pluginsDir}, needsSym: true, want: []string{msgOutside}},
		{name: "laraplate missing binary stays a missing-file problem", path: "demo/gone", paths: plugin.Paths{Provider: bootstrap.ProviderLaraplate, PluginsDir: pluginsDir}, want: []string{"plugins.0.path: the plugin binary cannot be read"}},
		{name: "laraplate unresolvable plugins_dir", path: abs, paths: plugin.Paths{Provider: bootstrap.ProviderLaraplate, PluginsDir: filepath.Join(fx.dir, "nope")}, want: []string{"plugins.0.path: plugins_dir cannot be resolved"}},
		{name: "file provider without plugins_dir is unconstrained", path: filepath.Join(outside, "demo"), paths: plugin.Paths{Provider: bootstrap.ProviderFile, BaseDir: fx.dir}},
		{name: "file provider relative path inside plugins_dir", path: "plugins/demo/demo", paths: plugin.Paths{Provider: bootstrap.ProviderFile, BaseDir: fx.dir, PluginsDir: pluginsDir}},
		{name: "file provider absolute path outside plugins_dir", path: filepath.Join(outside, "demo"), paths: plugin.Paths{Provider: bootstrap.ProviderFile, BaseDir: fx.dir, PluginsDir: pluginsDir}, want: []string{msgOutside}},
		{name: "file provider relative path outside plugins_dir", path: "../" + filepath.Base(filepath.Dir(outside)) + "/elsewhere/demo", paths: plugin.Paths{Provider: bootstrap.ProviderFile, BaseDir: fx.dir, PluginsDir: pluginsDir}, want: []string{msgOutside}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsSym && (!symlinks || runtime.GOOS == "windows") {
				t.Skip("symlinks are not available")
			}
			opts := docOptions{path: tc.path}
			doc := fx.document(t, opts)
			// The hashes pin the same bytes in every copy.
			_, got := plugin.Resolve(doc, tc.paths)

			if len(got) != len(tc.want) {
				t.Fatalf("messages = %q, want %q", got, tc.want)
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("message %d = %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

func TestTrustAnchorExecutesTheResolvedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are not available")
	}
	fx := newFixture(t, testManifest)
	pluginsDir := filepath.Join(fx.dir, "plugins")
	link := filepath.Join(pluginsDir, "current")
	if err := os.Symlink(filepath.Join(pluginsDir, "demo"), link); err != nil {
		t.Skip("symlinks are not available")
	}
	realBinary, err := filepath.EvalSymlinks(filepath.Join(pluginsDir, "demo", "demo"))
	if err != nil {
		t.Fatal(err)
	}

	doc := fx.document(t, docOptions{path: "current/demo"})
	specs, msgs := plugin.Resolve(doc, plugin.Paths{Provider: bootstrap.ProviderLaraplate, PluginsDir: pluginsDir})

	if len(msgs) != 0 || len(specs) != 1 {
		t.Fatalf("specs %d, messages %q, want one spec and none", len(specs), msgs)
	}
	if specs[0].Path != realBinary {
		t.Errorf("Spec.Path = %q, want the resolved path %q", specs[0].Path, realBinary)
	}
}
