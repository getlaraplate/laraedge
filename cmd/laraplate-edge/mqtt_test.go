package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/mqtt"
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

	shortProbe(t)

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

// commandWait is how long the tests let probe and browse wait for messages:
// the retained ones arrive at once, so it only bounds the silent ones.
const commandWait = 300 * time.Millisecond

// shortProbe makes probe wait commandWait for a silent point.
func shortProbe(t *testing.T) {
	t.Helper()

	old := probeTimeout
	probeTimeout = commandWait
	t.Cleanup(func() { probeTimeout = old })
}

// shortBrowse makes an mqtt browse collect for commandWait whatever its
// --duration.
func shortBrowse(t *testing.T) {
	t.Helper()

	old := browseWait
	browseWait = commandWait
	t.Cleanup(func() { browseWait = old })
}

func TestProbeAnMQTTInputPrintsWhyAPointHasNoGoodSample(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{Refuse: []string{"plant/vault"}})
	file := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, file, `schema: laraplate-edge-config/1
inputs:
  - id: line
    driver: mqtt
    connection: {broker: 'mqtt://`+b.Addr()+`'}
    devices:
      - device: press-07
        points:
          - {signal: count, address: "plant/press/state|count", type: int16, read: subscribe}
          - {signal: gone, address: "plant/press/state|gone", type: int16, read: subscribe}
          - {signal: open, address: "plant/vault", type: bool, read: subscribe}
          - {signal: silent, address: "plant/press/silent", type: float64, read: subscribe}
outputs:
  - {id: main, type: laraplate-http, url: 'http://127.0.0.1:8000', token: secret://env/TOKEN}
`)
	b.Publish("plant/press/state", []byte(`{"count": 70000}`), true, 1)
	shortProbe(t)

	r := runCLI(t, "", "probe", "line", "--config", file)
	if r.code != exitOK {
		t.Fatalf("exit code = %d, stderr = %q", r.code, r.stderr)
	}

	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	want := []string{
		"press-07\tcount\t0\tinteger\tbad\tconversion",
		"press-07\tgone\tno value at path",
		"press-07\topen\tsubscribe_refused",
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

	// probe connects as <client_id>-probe with a clean session; without a
	// bootstrap the agent id of the default client id is empty.
	want = []string{"laraplate-edge--line-probe clean"}
	if got := connects(b); !slices.Equal(got, want) {
		t.Errorf("CONNECT packets = %q, want %q", got, want)
	}
}

// connects describes the CONNECT packets b received, "<client id> clean" or
// "<client id> persistent".
func connects(b *mqtttest.Broker) []string {
	var out []string
	for _, c := range b.Connects() {
		session := "persistent"
		if c.CleanSession {
			session = "clean"
		}
		out = append(out, c.ClientID+" "+session)
	}

	return out
}

func TestBrowseAnMQTTInput(t *testing.T) {
	env := newMQTTEnv(t)
	shortBrowse(t)
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

	want = "laraplate-edge--line-browse clean"
	if got := connects(env.broker); len(got) != 2 || got[0] != want || got[1] != want {
		t.Errorf("CONNECT packets = %q, want twice %q", got, want)
	}

	for _, secret := range []string{env.broker.Addr(), "secret://"} {
		if strings.Contains(r.stdout+r.stderr, secret) {
			t.Errorf("output names %q", secret)
		}
	}
}

func TestMQTTBrowseLinesSayHowManyLeavesWereNotShown(t *testing.T) {
	entries := []mqtt.BrowseEntry{
		{Topic: "plant/a", Messages: 1, Size: 9, Paths: []mqtt.PathPreview{{Address: "plant/a|x", Preview: "1"}}, MorePaths: 7},
		{Topic: "plant/b", Messages: 1, Size: 9, Paths: []mqtt.PathPreview{{Address: "plant/b|y", Preview: "2"}}},
	}

	want := []string{
		"plant/a  1 msg  9 B  -",
		"  plant/a|x  1",
		"  ... 7 more leaves not shown",
		"plant/b  1 msg  9 B  -",
		"  plant/b|y  2",
	}
	if got := mqttBrowseLines(entries); !slices.Equal(got, want) {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
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

// TestMQTTExampleValidates: the document of examples/mqtt is accepted
// offline, the factory's checks of the connection, the addresses and the
// availability topic included.
func TestMQTTExampleValidates(t *testing.T) {
	r := runCLI(t, "", "config", "validate", filepath.Join("..", "..", "examples", "mqtt", "config.yaml"))
	if r.code != exitOK || !strings.HasSuffix(strings.TrimSpace(r.stdout), ": valid") {
		t.Errorf("config validate = %d, stdout %q, stderr %q, want exit 0 and valid", r.code, r.stdout, r.stderr)
	}
}
