package plugin_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

const configSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","properties":{"host":{"type":"string"},"port":{"type":"integer"},"token":{"type":"integer"}},"required":["host"],"type":"object"}`

const componentsManifest = `{"kinds":{"input":{"address_schema":{"type":"string"},"browse":false,"connection_schema":{"type":"object"},"read_modes":["poll"],"types":["int16"]},` +
	`"output":{"config_schema":` + configSchema + `},"processor":{"config_schema":` + configSchema + `}},"name":"demo","protocol_versions":["v1"],"version":"0.1.0"}`

const inputOnlyManifest = `{"kinds":{"input":{"address_schema":{"type":"string"},"browse":false,"connection_schema":{"type":"object"},"read_modes":["poll"],"types":["int16"]}},"name":"demo","protocol_versions":["v1"],"version":"0.1.0"}`

func (f fixture) componentsDocument(t *testing.T, processor, output string) config.Document {
	t.Helper()

	text := fmt.Sprintf(`{"schema":"laraplate-edge-config/1","version":1,
"plugins":[{"id":"demo","path":"plugins/demo/demo","sha256":%q,"manifest_sha256":%q}],
"outputs":[{"id":"o0","type":"laraplate-http","url":"https://example.com","token":"secret://env/T","route":{"include":["*"]}},%s],
"inputs":[{"id":"i0","driver":"plugin:demo","connection":{},"devices":[{"device":"d1","points":[{"signal":"s","address":"m:1","type":"int16","read":{"poll":"1s"}}]}]}],
"processors":[%s]}`,
		plugin.SumHex([]byte(f.binary)), plugin.SumHex([]byte(f.manifest)), output, processor)

	doc, msgs, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) > 0 {
		t.Fatalf("fixture document refused: %v", msgs)
	}

	return doc
}

func processorJSON(pluginID, cfg string) string {
	return fmt.Sprintf(`{"id":"p0","plugin":%q,"observe":["d1"],"devices":[{"device":"dd","signals":[{"signal":"x","type":"float"}]}]%s}`, pluginID, cfg)
}

func outputJSON(typ, cfg string) string {
	return fmt.Sprintf(`{"id":"o1","type":%q,"route":{"include":["*"]}%s}`, typ, cfg)
}

func TestValidateProcessorsAndOutputs(t *testing.T) {
	full := newFixture(t, componentsManifest)
	inputOnly := newFixture(t, inputOnlyManifest)

	tests := []struct {
		name      string
		fx        fixture
		processor string
		output    string
		want      []string
	}{
		{name: "valid", fx: full, processor: processorJSON("demo", `,"config":{"host":"h"}`), output: outputJSON("plugin:demo", `,"config":{"host":"h"}`)},
		{name: "output without config is validated against {}", fx: full, processor: processorJSON("demo", `,"config":{"host":"h"}`), output: outputJSON("plugin:demo", ""),
			want: []string{"outputs.1.config.host: is required"}},
		{name: "unknown processor plugin", fx: full, processor: processorJSON("other", ""), output: outputJSON("plugin:demo", `,"config":{"host":"h"}`),
			want: []string{"processors.0.plugin: no plugin entry with this id"}},
		{name: "unknown output plugin", fx: full, processor: processorJSON("demo", `,"config":{"host":"h"}`), output: outputJSON("plugin:other", ""),
			want: []string{"outputs.1.type: no plugin entry with this id"}},
		{name: "processor kind missing", fx: inputOnly, processor: processorJSON("demo", ""), output: outputJSON("laraplate-http", `,"url":"https://e.com","token":"secret://env/T"`),
			want: []string{"processors.0.plugin: the plugin does not offer the processor kind"}},
		{name: "output kind missing", fx: inputOnly, processor: processorJSON("demo", ""), output: outputJSON("plugin:demo", ""),
			want: []string{"processors.0.plugin: the plugin does not offer the processor kind", "outputs.1.type: the plugin does not offer the output kind"}},
		{name: "processor config refused", fx: full, processor: processorJSON("demo", `,"config":{"host":"h","port":"xyz"}`), output: outputJSON("plugin:demo", `,"config":{"host":"h"}`),
			want: []string{"processors.0.config.port: "}},
		{name: "output config refused", fx: full, processor: processorJSON("demo", `,"config":{"host":"h"}`), output: outputJSON("plugin:demo", `,"config":{"host":1}`),
			want: []string{"outputs.1.config.host: "}},
		{name: "secret:// value in a typed field is not reported", fx: full, processor: processorJSON("demo", `,"config":{"host":"h","port":"secret://env/PORT"}`), output: outputJSON("plugin:demo", `,"config":{"host":"h","port":"secret://env/PORT"}`)},
		{name: "problem at a secret key is not reported", fx: full, processor: processorJSON("demo", `,"config":{"host":"h","token":"secret://env/TOKEN"}`), output: outputJSON("plugin:demo", `,"config":{"host":"h","token":"secret://env/TOKEN"}`)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := tc.fx.componentsDocument(t, tc.processor, tc.output)

			got := run(doc, plugin.Paths{BaseDir: tc.fx.dir})

			if len(got) != len(tc.want) {
				t.Fatalf("messages = %q, want %q", got, tc.want)
			}
			for i, w := range tc.want {
				if !strings.HasPrefix(got[i], w) {
					t.Errorf("message %d = %q, want prefix %q", i, got[i], w)
				}
			}
			for _, m := range got {
				if strings.Contains(m, "xyz") || strings.Contains(m, "secret://") {
					t.Errorf("message %q contains a value", m)
				}
			}
		})
	}
}
