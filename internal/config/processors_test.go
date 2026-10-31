package config_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
)

const procBase = base + `processors:
  - id: line-state
    plugin: oee
    observe: [press-07]
    devices:
      - device: line-1
        signals:
          - {signal: state, type: int}
          - {signal: speed, type: float, priority: bulk, unit: rpm}
    config: {mode: fast}
plugins:
  - {id: oee, path: /a, sha256: x, manifest_sha256: m}
`

func TestParseProcessor(t *testing.T) {
	doc := mustParse(t, []byte(procBase))
	if len(doc.Processors) != 1 {
		t.Fatalf("processors = %d", len(doc.Processors))
	}
	p := doc.Processors[0]
	if p.ID != "line-state" || p.Plugin != "oee" || !slices.Equal(p.Observe, []string{"press-07"}) {
		t.Errorf("processor = %+v", p)
	}
	if p.DeathTolerance != config.DefaultDeathTolerance {
		t.Errorf("death tolerance = %v", p.DeathTolerance)
	}
	if string(p.Config) != `{"mode":"fast"}` {
		t.Errorf("config = %s", p.Config)
	}
	want := []config.DerivedSignal{
		{Signal: "state", Type: "int", Priority: "critical"},
		{Signal: "speed", Type: "float", Priority: "bulk", Unit: "rpm"},
	}
	if len(p.Devices) != 1 || p.Devices[0].Device != "line-1" || !slices.Equal(p.Devices[0].Signals, want) {
		t.Errorf("devices = %+v", p.Devices)
	}

	doc = mustParse(t, []byte(edit(t, procBase, "    config: {mode: fast}\n", "    death_tolerance: 5s\n")))
	if got := doc.Processors[0]; got.DeathTolerance != 5*time.Second || string(got.Config) != `{}` {
		t.Errorf("tolerance/config = %v/%s", got.DeathTolerance, got.Config)
	}
}

func TestParsePluginOutputConfig(t *testing.T) {
	doc := mustParse(t, []byte(edit(t, base, "type: laraplate-http\n    url: https://erp.example.com\n    token: secret://env/TOKEN", "type: 'plugin:kafka'\n    config: {topic: t, password: 'secret://env/P'}")))
	if got := string(doc.Outputs[0].Config); got != `{"password":"secret://env/P","topic":"t"}` {
		t.Errorf("config = %s", got)
	}
	if mustParse(t, []byte(base)).Outputs[0].Config != nil {
		t.Error("absent config is not nil")
	}
}

func TestParseProcessorRefusals(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"config on laraplate-http", edit(t, base, "token: secret://env/TOKEN", "token: secret://env/TOKEN\n    config: {a: 1}"), "outputs.0.config"},
		{"unknown observed device", edit(t, procBase, "observe: [press-07]", "observe: [press-07, nope]"), "processors.0.observe.1: is not a device of any input"},
		{"derived equals real device", edit(t, procBase, "device: line-1", "device: press-07"), "processors.0.devices.0.device: is the name of a device of an input"},
		{"derived equals other processor's", edit(t, procBase, "plugins:", "  - {id: p2, plugin: oee, observe: [press-07], devices: [{device: line-1, signals: [{signal: x, type: bool}]}]}\nplugins:"), "processors.1.devices.0.device: duplicate of processors.0.devices.0.device"},
		{"duplicate signal", edit(t, procBase, "{signal: speed,", "{signal: state,"), "processors.0.devices.0.signals.1.signal: duplicate of processors.0.devices.0.signals.0.signal"},
		{"bad type", edit(t, procBase, "type: float", "type: decimal"), "processors.0.devices.0.signals.1.type"},
		{"bad priority", edit(t, procBase, "priority: bulk", "priority: low"), "processors.0.devices.0.signals.1.priority"},
		{"duplicate id", edit(t, procBase, "plugins:", "  - {id: line-state, plugin: oee, observe: [press-07], devices: [{device: l2, signals: [{signal: x, type: bool}]}]}\nplugins:"), "processors.1.id: duplicate of processors.0.id"},
		{"plain secret in processor config", edit(t, procBase, "config: {mode: fast}", "config: {nested: {db_password: hunter2}}"), "processors.0.config.nested.db_password"},
		{"plain secret in output config", edit(t, base, "type: laraplate-http\n    url: https://erp.example.com\n    token: secret://env/TOKEN", "type: 'plugin:kafka'\n    config: {API_KEY: hunter2}"), "outputs.0.config."},
		{"no observe", edit(t, procBase, "observe: [press-07]", "observe: []"), "processors.0.observe"},
		{"url on a plugin output", edit(t, base, "type: laraplate-http\n    url: https://erp.example.com\n    token: secret://env/TOKEN", "type: 'plugin:kafka'\n    url: https://erp.example.com"), "outputs.0.url"},
		{"token on a plugin output", edit(t, base, "type: laraplate-http\n    url: https://erp.example.com\n    token: secret://env/TOKEN", "type: 'plugin:kafka'\n    token: secret://env/TOKEN"), "outputs.0.token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, msgs, err := config.Parse([]byte(tt.doc))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(msgs, func(m string) bool { return strings.HasPrefix(m, tt.want) }) {
				t.Errorf("messages %q lack %q", msgs, tt.want)
			}
			for _, m := range msgs {
				if strings.Contains(m, "hunter2") {
					t.Errorf("message echoes a value: %q", m)
				}
			}
		})
	}
}

func TestCheckBuildProcessorsAndPluginOutputs(t *testing.T) {
	pluginOut := edit(t, base, "type: laraplate-http\n    url: https://erp.example.com\n    token: secret://env/TOKEN", "type: 'plugin:kafka'\n    config: {topic: t}")
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{"processor", procBase, nil},
		{"plugin output", pluginOut, nil},
		{"mqtt still refused", edit(t, base, "type: laraplate-http", "type: laraplate-mqtt"), []string{"outputs.0.type: not available in this build"}},
		{"keystore in output config", edit(t, pluginOut, "config: {topic: t}", "config: {token: 'secret://keystore/k'}"), []string{"outputs.0.config.token: not available in this build"}},
		{"keystore in processor config", edit(t, procBase, "config: {mode: fast}", "config: {a_secret: 'secret://keystore/k'}"), []string{"processors.0.config.a_secret: not available in this build"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := config.CheckBuild(mustParse(t, []byte(tt.doc))); !slices.Equal(got, tt.want) {
				t.Errorf("CheckBuild = %q, want %q", got, tt.want)
			}
		})
	}
}
