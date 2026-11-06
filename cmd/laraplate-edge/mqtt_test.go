package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/input/opcua/opcuatest"
)

// mqttEnv is a document with one mqtt input "line" against a test broker.
type mqttEnv struct {
	broker *mqtttest.Broker
	config string
}

func newMQTTEnv(t *testing.T) mqttEnv {
	t.Helper()

	b := mqtttest.Start(t, mqtttest.Options{})
	file := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, file, `schema: laraplate-edge-config/1
inputs:
  - id: line
    driver: mqtt
    connection: {broker: 'mqtt://`+b.Addr()+`'}
    devices:
      - device: press-07
        points:
          - {signal: state, address: "plant/press/state|mode", type: string, read: subscribe}
          - {signal: temp, address: "plant/press/temp", type: float64, read: subscribe}
          - {signal: silent, address: "plant/press/silent", type: float64, read: subscribe}
outputs:
  - {id: main, type: laraplate-http, url: 'http://127.0.0.1:8000', token: secret://env/TOKEN}
`)

	return mqttEnv{broker: b, config: file}
}

func TestProbeAnMQTTInputPrintsNoMessageForASilentPoint(t *testing.T) {
	env := newMQTTEnv(t)
	env.broker.Publish("plant/press/state", []byte(`{"mode": "auto"}`), true, 1)
	env.broker.Publish("plant/press/temp", []byte("21.5"), true, 1)

	old := probeTimeout
	probeTimeout = 1500 * time.Millisecond
	t.Cleanup(func() { probeTimeout = old })

	r := runCLI(t, "", "probe", "line", "--config", env.config)
	if r.code != exitOK {
		t.Fatalf("exit code = %d, stderr = %q", r.code, r.stderr)
	}

	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	want := []string{
		"press-07\tstate\t\"auto\"\tstring\tgood",
		"press-07\ttemp\t21.5\tfloat\tgood",
		"press-07\tsilent\tno message",
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

func TestBrowseAnMQTTInput(t *testing.T) {
	env := newMQTTEnv(t)
	env.broker.Publish("plant/press/state", []byte(`{"mode": "auto", "count": 42}`), true, 1)
	env.broker.Publish("plant/press/temp", []byte("21.5"), true, 1)
	env.broker.Publish("other/raw", []byte("a\x00b"), true, 1)

	r := runCLI(t, "", "browse", "line", "--config", env.config, "--duration", "1s")
	if r.code != exitOK || r.stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q", r.code, r.stderr)
	}

	want := `other/raw  1 msg  3 B  retained  a\x00b
plant/press/state  1 msg  29 B  retained
  plant/press/state|count  42
  plant/press/state|mode  "auto"
plant/press/temp  1 msg  4 B  retained  21.5
`
	if r.stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", r.stdout, want)
	}

	r = runCLI(t, "", "browse", "line", "--config", env.config, "--duration", "1s", "--filter", "nothing/#")
	if r.code != exitOK || r.stdout != browseNoMessage+"\n" {
		t.Errorf("a filter matching nothing: %+v", r)
	}

	for _, secret := range []string{env.broker.Addr(), "secret://"} {
		if strings.Contains(r.stdout+r.stderr, secret) {
			t.Errorf("output names %q", secret)
		}
	}
}

func TestBrowseFlagsBelongToTheirDriver(t *testing.T) {
	env := newMQTTEnv(t)
	opcua := newOPCUAEnv(t, opcuatest.Options{})

	for name, tt := range map[string]struct {
		args []string
		want string
	}{
		"node with mqtt":      {[]string{"browse", "line", "--config", env.config, "--node", "i=85"}, errMQTTBrowseFlags},
		"depth with mqtt":     {[]string{"browse", "line", "--config", env.config, "--depth", "3"}, errMQTTBrowseFlags},
		"filter with opcua":   {append([]string{"browse", "plant", "--filter", "a/#"}, opcua.flags()...), errOtherBrowseFlags},
		"duration with opcua": {append([]string{"browse", "plant", "--duration", "5s"}, opcua.flags()...), errOtherBrowseFlags},
		"bad filter":          {[]string{"browse", "line", "--config", env.config, "--filter", "a/#/b"}, "--filter"},
		"duration too short":  {[]string{"browse", "line", "--config", env.config, "--duration", "500ms"}, "--duration"},
		"duration too long":   {[]string{"browse", "line", "--config", env.config, "--duration", "6m"}, "--duration"},
	} {
		r := runCLI(t, "", tt.args...)
		if r.code != exitUsage || r.stdout != "" || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("%s: %+v, want a usage error naming %q", name, r, tt.want)
		}
	}
}
