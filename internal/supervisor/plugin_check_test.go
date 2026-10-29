package supervisor

import (
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/bootstrap"
	"github.com/getlaraplate/laraedge/internal/config"
)

const pluginCheckDoc = `{"schema":"laraplate-edge-config/1","version":1,
"plugins":[{"id":"demo","path":"plugins/demo/demo","sha256":"` + zeros64 + `","manifest_sha256":"` + zeros64 + `"}],
"outputs":[{"id":"o0","type":"laraplate-http","url":"https://example.com","token":"secret://env/T","route":{"include":["*"]}}],
"inputs":[{"id":"i0","driver":"plugin:demo","devices":[{"device":"d","points":[{"signal":"s","address":"a","type":"int16","read":{"poll":"1s"}}]}]}]}`

const zeros64 = "0000000000000000000000000000000000000000000000000000000000000000"

func TestCheckRejectsUnresolvablePlugins(t *testing.T) {
	doc, msgs, err := config.Parse([]byte(pluginCheckDoc))
	if err != nil || len(msgs) > 0 {
		t.Fatalf("Parse: %v %v", err, msgs)
	}

	tests := []struct {
		name     string
		provider string
		want     string
	}{
		{"missing files under the file provider", bootstrap.ProviderFile, "plugins.0: plugin.json not found next to the binary"},
		{"relative path under the laraplate provider", bootstrap.ProviderLaraplate, "plugins.0.path: must be absolute with the laraplate provider"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(c *Config) {
				c.ConfigDir = t.TempDir()
				c.Provider = tc.provider
			})

			got := f.sup.check(doc)
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("check() = %q, want a message %q", got, tc.want)
			}
		})
	}
}
