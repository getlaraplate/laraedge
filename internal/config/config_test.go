package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/schema"
)

// base is a document this build applies; the refusal cases edit it.
const base = `schema: laraplate-edge-config/1
version: 3
inputs:
  - id: sim-line
    driver: simulated
    connection:
      scenario: s.json
    devices:
      - device: press-07
        points:
          - signal: state
            address: state
            read: subscribe
outputs:
  - id: main
    type: laraplate-http
    url: https://erp.example.com
    token: secret://env/TOKEN
`

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func mustParse(t *testing.T, data []byte) config.Document {
	t.Helper()

	doc, msgs, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("Parse messages: %q", msgs)
	}

	return doc
}

func edit(t *testing.T, doc string, pairs ...string) string {
	t.Helper()

	for i := 0; i+1 < len(pairs); i += 2 {
		if !strings.Contains(doc, pairs[i]) {
			t.Fatalf("edit: %q not found in the document", pairs[i])
		}
		doc = strings.Replace(doc, pairs[i], pairs[i+1], 1)
	}

	return doc
}

func TestParseSpecExample(t *testing.T) {
	doc := mustParse(t, readFixture(t, "spec-5.3.yaml"))

	if doc.Schema != config.SchemaName || doc.Version != 42 {
		t.Fatalf("schema/version = %q/%d", doc.Schema, doc.Version)
	}
	if len(doc.Inputs) != 1 || len(doc.Outputs) != 1 || len(doc.Plugins) != 1 {
		t.Fatalf("components = %d/%d/%d", len(doc.Inputs), len(doc.Outputs), len(doc.Plugins))
	}

	in := doc.Inputs[0]
	if in.ID != "press-line" || in.Driver != "opcua" || in.DeathTolerance != 30*time.Second {
		t.Errorf("input = %+v", in)
	}
	wantConn := `{"auth":{"password":"secret://keystore/opcua-press","user":"mes"},"endpoint":"opc.tcp://10.0.5.20:4840","security_policy":"Basic256Sha256"}`
	if string(in.Connection) != wantConn {
		t.Errorf("connection = %s, want %s", in.Connection, wantConn)
	}

	points := in.Devices[0].Points
	wantReads := []config.Read{{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: -1}}, {Mode: config.ReadPoll, Interval: time.Second}, {Mode: config.ReadPoll, Interval: time.Second}}
	wantPriorities := []model.Priority{model.Critical, model.Critical, model.Bulk}
	for i, p := range points {
		if !reflect.DeepEqual(p.Read, wantReads[i]) || p.Priority != wantPriorities[i] {
			t.Errorf("point %d read/priority = %+v/%v, want %+v/%v", i, p.Read, p.Priority, wantReads[i], wantPriorities[i])
		}
	}
	wantTransforms := []config.Transform{{Name: "scale", Params: json.RawMessage(`0.1`)}, {Name: "deadband", Params: json.RawMessage(`0.5`)}}
	if !reflect.DeepEqual(points[2].Transforms, wantTransforms) {
		t.Errorf("transforms = %+v", points[2].Transforms)
	}

	derived := in.Devices[0].Derived
	if len(derived) != 1 || derived[0].Signal != "light_code" || derived[0].Type != "int" || derived[0].Priority != model.Critical || derived[0].Interval != 0 {
		t.Errorf("derived = %+v", derived)
	}

	out := doc.Outputs[0]
	wantOut := config.Output{
		ID: "laraplate-main", Type: config.OutputLaraplateHTTP, URL: "https://erp.example.com",
		Token:  "secret://keystore/laraplate-main",
		Route:  config.Route{Include: []string{"*"}},
		Batch:  config.Batch{MaxSamples: 1000, MaxBody: 1024 << 10, MaxWait: 2 * time.Second},
		Buffer: config.Buffer{MaxSize: 1 << 30, MinFreePercent: 5, FsyncInterval: 200 * time.Millisecond},
		Raw:    out.Raw,
	}
	if !reflect.DeepEqual(out, wantOut) {
		t.Errorf("output = %+v\nwant     %+v", out, wantOut)
	}

	if p := doc.Plugins[0]; p.ID != "fanuc" || p.Path != "/opt/laraplate-edge/plugins/fanuc-focas" || p.SHA256 != "..." || p.ManifestSHA256 != "..." || p.MaxMemory != 256<<20 {
		t.Errorf("plugin = %+v", p)
	}
}

func TestCheckBuildRefusesExactlyTheSpecExampleNonBuildParts(t *testing.T) {
	doc := mustParse(t, readFixture(t, "spec-5.3.yaml"))

	got := config.CheckBuild(doc)
	want := []string{
		"inputs.0.connection.auth.password: not available in this build",
		"inputs.0.devices.0.derived: not available in this build",
		"outputs.0.token: not available in this build",
	}
	if !slices.Equal(got, want) {
		t.Errorf("CheckBuild =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestParseBuildDocument(t *testing.T) {
	doc := mustParse(t, readFixture(t, "build.yaml"))

	if msgs := config.CheckBuild(doc); len(msgs) != 0 {
		t.Fatalf("CheckBuild = %q", msgs)
	}

	in := doc.Inputs[0]
	if in.DeathTolerance != 45*time.Second {
		t.Errorf("death tolerance = %v", in.DeathTolerance)
	}
	if want := `{"auth":{"token":"secret://env/SIM_TOKEN"},"scenario":"scenarios/press.json","seed":7}`; string(in.Connection) != want {
		t.Errorf("connection = %s, want %s", in.Connection, want)
	}

	wantPoints := []config.Point{
		{Signal: "state", Address: "state", Read: config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: -1}}, Priority: model.Critical, Type: "int16"},
		{Signal: "good_count", Address: "counters.good", Read: config.Read{Mode: config.ReadPoll, Interval: 500 * time.Millisecond}, Priority: model.Critical, Type: "uint32", ByteOrder: "CDAB", Unit: "pcs"},
		{Signal: "door_open", Address: "door", Read: config.Read{Mode: config.ReadEvent}, Priority: model.Bulk, Type: "bool"},
	}
	if !reflect.DeepEqual(in.Devices[0].Points, wantPoints) {
		t.Errorf("points = %+v\nwant     %+v", in.Devices[0].Points, wantPoints)
	}
	if p := in.Devices[1].Points[0]; p.Read.Interval != 90*time.Second || p.Unit != "°C" {
		t.Errorf("oven point = %+v", p)
	}

	main := doc.Outputs[0]
	wantBatch := config.Batch{MaxSamples: 500, MaxBody: 512 << 10, MaxWait: 1500 * time.Millisecond}
	wantBuffer := config.Buffer{MaxSize: 256 << 20, MinFreePercent: 2.5, FsyncInterval: time.Second}
	if main.Batch != wantBatch || main.Buffer != wantBuffer || !slices.Equal(main.Route.Include, []string{"press-*", "oven-02/temp"}) {
		t.Errorf("main output = %+v", main)
	}

	local := doc.Outputs[1]
	if local.URL != "http://127.0.0.1:8000" || local.Batch.MaxSamples != config.DefaultMaxSamples || !slices.Equal(local.Route.Include, []string{"*"}) {
		t.Errorf("local output = %+v", local)
	}

	for i, raw := range [][]byte{doc.Raw, in.Raw, main.Raw, local.Raw} {
		if !json.Valid(raw) || bytes.ContainsAny(raw, "\n\t") {
			t.Errorf("raw %d is not compact JSON: %s", i, raw)
		}
	}
}

func TestParseYAMLAndJSONGiveTheSameDocument(t *testing.T) {
	for _, name := range []string{"spec-5.3", "build"} {
		t.Run(name, func(t *testing.T) {
			fromYAML := mustParse(t, readFixture(t, name+".yaml"))
			fromJSON := mustParse(t, readFixture(t, name+".json"))

			if !reflect.DeepEqual(fromYAML, fromJSON) {
				t.Errorf("documents differ\nyaml: %s\njson: %s", fromYAML.Raw, fromJSON.Raw)
			}
		})
	}
}

func TestRawIsCanonicalAndPerComponent(t *testing.T) {
	implicit := mustParse(t, []byte(base))
	explicit := mustParse(t, []byte(edit(t, base,
		"    token: secret://env/TOKEN\n",
		"    token: secret://env/TOKEN\n    buffer: {fsync_interval: 200ms, max_size: 1GiB, min_free_percent: 5.0}\n    batch: {max_wait: 2s, max_body: 1024KiB, max_samples: 1e3}\n    route: {include: ['*']}\n",
		"    devices:\n", "    death_tolerance: 30s\n    devices:\n",
		"            read: subscribe\n", "            read: subscribe\n            priority: critical\n",
	)))

	if !bytes.Equal(implicit.Outputs[0].Raw, explicit.Outputs[0].Raw) {
		t.Errorf("output raw differs:\n%s\n%s", implicit.Outputs[0].Raw, explicit.Outputs[0].Raw)
	}
	if !bytes.Equal(implicit.Inputs[0].Raw, explicit.Inputs[0].Raw) {
		t.Errorf("input raw differs:\n%s\n%s", implicit.Inputs[0].Raw, explicit.Inputs[0].Raw)
	}

	changed := mustParse(t, []byte(edit(t, base, "address: state", "address: state2")))
	if bytes.Equal(implicit.Inputs[0].Raw, changed.Inputs[0].Raw) {
		t.Error("a changed point left the input raw unchanged")
	}
	if !bytes.Equal(implicit.Outputs[0].Raw, changed.Outputs[0].Raw) {
		t.Error("a changed input changed the output raw")
	}

	again := mustParse(t, implicit.Raw)
	if !reflect.DeepEqual(implicit, again) {
		t.Errorf("parsing the document raw gives another document:\n%s\n%s", implicit.Raw, again.Raw)
	}
}

func TestParseRefusals(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{"plain output token", edit(t, base, "secret://env/TOKEN", "plain-value"), []string{"outputs.0.token: must be a secret:// reference"}},
		{"plain connection password", edit(t, base, "scenario: s.json", "scenario: s.json\n      auth: {user: mes, password: plain}"), []string{"inputs.0.connection.auth.password: must be a secret:// reference"}},
		{"plain connection token in a list", edit(t, base, "scenario: s.json", "scenario: s.json\n      peers: [{token: plain}]"), []string{"inputs.0.connection.peers.0.token: must be a secret:// reference"}},
		{"plain top connection secret", edit(t, base, "scenario: s.json", "scenario: s.json\n      secret: plain"), []string{"inputs.0.connection.secret: must be a secret:// reference"}},
		{"numeric secret", edit(t, base, "scenario: s.json", "scenario: s.json\n      password: 1234"), []string{"inputs.0.connection.password: must be string"}},
		{"unknown secret kind", edit(t, base, "secret://env/TOKEN", "secret://vault/x"), []string{"outputs.0.token: must be a secret:// reference"}},
		{"bad env name", edit(t, base, "secret://env/TOKEN", "secret://env/1-X"), []string{"outputs.0.token: must be a secret:// reference"}},
		{"max_samples above the backend default", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    batch: {max_samples: 5001}"), []string{"outputs.0.batch.max_samples: must be at most 5000"}},
		{"max_samples zero", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    batch: {max_samples: 0}"), []string{"outputs.0.batch.max_samples: must be at least 1"}},
		{"max_samples fractional", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    batch: {max_samples: 10.5}"), []string{"outputs.0.batch.max_samples: must be integer"}},
		{"max_body above the backend default", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    batch: {max_body: 1025KiB}"), []string{"outputs.0.batch.max_body: must not exceed 1024KiB"}},
		{"max_body in MiB above the cap", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    batch: {max_body: 2MiB}"), []string{"outputs.0.batch.max_body: must not exceed 1024KiB"}},
		{"max_body zero", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    batch: {max_body: 0B}"), []string{"outputs.0.batch.max_body: must be greater than zero"}},
		{"size overflow", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    buffer: {max_size: 99999999999999999999GiB}"), []string{"outputs.0.buffer.max_size: is too large"}},
		{"size unit", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    buffer: {max_size: 1GB}"), []string{"outputs.0.buffer.max_size: must be a size such as"}},
		{"max_size below two segments", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    buffer: {max_size: 31MiB}"), []string{"outputs.0.buffer.max_size: must be at least 32MiB (two segments of 16MiB)"}},
		{"max_size in KiB below two segments", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    buffer: {max_size: 32767KiB}"), []string{"outputs.0.buffer.max_size: must be at least 32MiB"}},
		{"min_free_percent above 50", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    buffer: {min_free_percent: 60}"), []string{"outputs.0.buffer.min_free_percent: must be at most 50"}},
		{"duration without unit", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    batch: {max_wait: 30}"), []string{"outputs.0.batch.max_wait: must be string"}},
		{"duration format", edit(t, base, "    devices:\n", "    death_tolerance: soon\n    devices:\n"), []string{"inputs.0.death_tolerance: must be a duration such as"}},
		{"zero duration", edit(t, base, "read: subscribe", "read: {poll: 0s}"), []string{"inputs.0.devices.0.points.0.read.poll: must be greater than zero"}},
		{"duration overflow", edit(t, base, "read: subscribe", "read: {poll: 99999999999h}"), []string{"inputs.0.devices.0.points.0.read.poll: is too large"}},
		{"unknown read mode", edit(t, base, "read: subscribe", "read: sometimes"), []string{"inputs.0.devices.0.points.0.read: must be one of subscribe, event"}},
		{"read object without poll", edit(t, base, "read: subscribe", "read: {every: 1s}"), []string{"inputs.0.devices.0.points.0.read.poll: is required", "inputs.0.devices.0.points.0.read.every: unknown key"}},
		{"subscribe unknown key", edit(t, base, "read: subscribe", "read: {subscribe: {rate: 1s}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.rate: unknown key"}},
		{"subscribe sampling zero", edit(t, base, "read: subscribe", "read: {subscribe: {sampling: 0s}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.sampling: must be greater than zero"}},
		{"subscribe sampling negative", edit(t, base, "read: subscribe", "read: {subscribe: {sampling: -1s}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.sampling: must be a duration such as"}},
		{"subscribe sampling overflow", edit(t, base, "read: subscribe", "read: {subscribe: {sampling: 99999999999h}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.sampling: is too large"}},
		{"subscribe queue zero", edit(t, base, "read: subscribe", "read: {subscribe: {queue: 0}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.queue: must be at least 1"}},
		{"subscribe queue fractional", edit(t, base, "read: subscribe", "read: {subscribe: {queue: 1.5}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.queue: must be integer"}},
		{"subscribe queue above uint32", edit(t, base, "read: subscribe", "read: {subscribe: {queue: 4294967296}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.queue: must be at most 4294967295"}},
		{"subscribe deadband word", edit(t, base, "read: subscribe", "read: {subscribe: {deadband: x}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.deadband: must be a non-negative number or a percentage"}},
		{"subscribe deadband negative text", edit(t, base, "read: subscribe", "read: {subscribe: {deadband: '-1'}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.deadband: must be a non-negative number or a percentage"}},
		{"subscribe deadband negative number", edit(t, base, "read: subscribe", "read: {subscribe: {deadband: -1}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.deadband: must be at least 0"}},
		{"subscribe deadband above 100 percent", edit(t, base, "read: subscribe", "read: {subscribe: {deadband: 101%}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.deadband: must be at most 100%"}},
		{"subscribe deadband beyond float64", edit(t, base, "read: subscribe", "read: {subscribe: {deadband: '1"+strings.Repeat("0", 400)+"'}}"), []string{"inputs.0.devices.0.points.0.read.subscribe.deadband: is too large"}},
		{"subscribe with poll", edit(t, base, "read: subscribe", "read: {poll: 1s, subscribe: {}}"), []string{"inputs.0.devices.0.points.0.read.poll: is not allowed"}},
		{"subscribe object of the wrong type", edit(t, base, "read: subscribe", "read: {subscribe: yes}"), []string{"inputs.0.devices.0.points.0.read.subscribe: must be object"}},
		{"unknown driver", edit(t, base, "driver: simulated", "driver: s7"), []string{"inputs.0.driver: unknown driver"}},
		{"plugin driver without id", edit(t, base, "driver: simulated", "driver: 'plugin:'"), []string{"inputs.0.driver: unknown driver"}},
		{"unknown output type", edit(t, base, "type: laraplate-http", "type: kafka"), []string{"outputs.0.type: unknown output type"}},
		{"wrong schema", edit(t, base, "laraplate-edge-config/1", "laraplate-edge-config/2"), []string{"schema: must be laraplate-edge-config/1"}},
		{"missing schema", edit(t, base, "schema: laraplate-edge-config/1\n", ""), []string{"schema: is required"}},
		{"version zero", edit(t, base, "version: 3", "version: 0"), []string{"version: must be at least 1"}},
		{"version as text", edit(t, base, "version: 3", "version: '3'"), []string{"version: must be integer"}},
		{"missing outputs", base[:strings.Index(base, "outputs:")], []string{"outputs: is required"}},
		{"unknown top-level key", base + "metrics: []\n", []string{"metrics: unknown key"}},
		{"unknown point key", edit(t, base, "read: subscribe", "read: subscribe\n            colour: red"), []string{"inputs.0.devices.0.points.0.colour: unknown key"}},
		{"unknown key of odd shape", edit(t, base, "read: subscribe", "read: subscribe\n            Odd-Key: red"), []string{"inputs.0.devices.0.points.0.*: unknown key"}},
		{"id with spaces", edit(t, base, "id: sim-line", "id: Sim Line"), []string{"inputs.0.id: must be 1 to 64 characters"}},
		{"id too long", edit(t, base, "id: main", "id: "+long(65)), []string{"outputs.0.id: must be 1 to 64 characters"}},
		{"duplicate input id", edit(t, base, "outputs:", "  - id: sim-line\n    driver: simulated\n    devices:\n      - device: other\n        points:\n          - {signal: s, address: a, read: event}\noutputs:"), []string{"inputs.1.id: duplicate of inputs.0.id"}},
		{"duplicate output id", base + "  - {id: main, type: laraplate-http, url: 'https://b.example.com', token: 'secret://env/B'}\n", []string{"outputs.1.id: duplicate of outputs.0.id"}},
		{"duplicate plugin id", base + "plugins:\n  - {id: p, path: /a, sha256: x, manifest_sha256: x}\n  - {id: p, path: /b, sha256: y, manifest_sha256: y}\n", []string{"plugins.1.id: duplicate of plugins.0.id"}},
		{"duplicate device and signal across inputs", edit(t, base, "outputs:", "  - id: second\n    driver: simulated\n    devices:\n      - device: press-07\n        points:\n          - {signal: state, address: a, read: event}\noutputs:"), []string{"inputs.1.devices.0.points.0.signal: duplicate of inputs.0.devices.0.points.0.signal on the same device"}},
		{"duplicate signal in a device", edit(t, base, "            read: subscribe\n", "            read: subscribe\n          - {signal: state, address: b, read: event}\n"), []string{"inputs.0.devices.0.points.1.signal: duplicate of inputs.0.devices.0.points.0.signal on the same device"}},
		{"device too long", edit(t, base, "device: press-07", "device: "+long(129)), []string{"inputs.0.devices.0.device: must be at most 128 characters"}},
		{"signal too long", edit(t, base, "signal: state", "signal: "+long(161)), []string{"inputs.0.devices.0.points.0.signal: must be at most 160 characters"}},
		{"unit too long", edit(t, base, "read: subscribe", "read: subscribe\n            unit: "+long(17)), []string{"inputs.0.devices.0.points.0.unit: must be at most 16 characters"}},
		{"empty address", edit(t, base, "address: state", "address: ''"), []string{"inputs.0.devices.0.points.0.address: must be at least 1 characters"}},
		{"no devices", edit(t, base, "    devices:\n      - device: press-07\n        points:\n          - signal: state\n            address: state\n            read: subscribe\n", "    devices: []\n"), []string{"inputs.0.devices: must have at least 1 items"}},
		{"no points", edit(t, base, "        points:\n          - signal: state\n            address: state\n            read: subscribe\n", "        points: []\n"), []string{"inputs.0.devices.0.points: must have at least 1 items"}},
		{"unknown priority", edit(t, base, "read: subscribe", "read: subscribe\n            priority: urgent"), []string{"inputs.0.devices.0.points.0.priority: must be one of critical, bulk"}},
		{"unknown decoding type", edit(t, base, "read: subscribe", "read: subscribe\n            type: int8"), []string{"inputs.0.devices.0.points.0.type: must be one of bool,"}},
		{"unknown byte order", edit(t, base, "read: subscribe", "read: subscribe\n            byte_order: XYZW"), []string{"inputs.0.devices.0.points.0.byte_order: must be one of ABCD, CDAB, BADC, DCBA"}},
		{"unknown transform", edit(t, base, "read: subscribe", "read: subscribe\n            transforms: [{explode: 1}]"), []string{"inputs.0.devices.0.points.0.transforms.0: unknown transform explode"}},
		{"transform with two names", edit(t, base, "read: subscribe", "read: subscribe\n            transforms: [{scale: 1, offset: 2}]"), []string{"inputs.0.devices.0.points.0.transforms.0: must have at most 1 keys"}},
		{"derived without expr", edit(t, base, "            read: subscribe\n", "            read: subscribe\n        derived: [{signal: d, type: int}]\n"), []string{"inputs.0.devices.0.derived.0.expr: is required"}},
		{"laraplate-http without url and token", edit(t, base, "    url: https://erp.example.com\n    token: secret://env/TOKEN\n", ""), []string{"outputs.0.url: is required", "outputs.0.token: is required"}},
		{"http url on a remote host", edit(t, base, "https://erp.example.com", "http://erp.example.com"), []string{"outputs.0.url: must use https (http is accepted only for a loopback host)"}},
		{"url with credentials", edit(t, base, "https://erp.example.com", "https://u:p@erp.example.com"), []string{"outputs.0.url: must not contain credentials"}},
		{"url scheme", edit(t, base, "https://erp.example.com", "ftp://erp.example.com"), []string{"outputs.0.url: must be an http or https URL"}},
		{"bad route pattern", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    route: {include: ['press-[']}"), []string{"outputs.0.route.include.0: invalid pattern"}},
		{"empty route", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    route: {include: []}"), []string{"outputs.0.route.include: must have at least 1 items"}},
		{"plugin without manifest_sha256", base + "plugins:\n  - {id: p, path: /a, sha256: x}\n", []string{"plugins.0.manifest_sha256: is required"}},
		{"plugin with empty manifest_sha256", base + "plugins:\n  - {id: p, path: /a, sha256: x, manifest_sha256: ''}\n", []string{"plugins.0.manifest_sha256: must be at least 1 characters"}},
		{"plugin with a bad max_memory", base + "plugins:\n  - {id: p, path: /a, sha256: x, manifest_sha256: y, max_memory: lots}\n", []string{"plugins.0.max_memory: must be a size such as 512KiB, 16MiB or 1GiB"}},
		{"plugin with a zero max_memory", base + "plugins:\n  - {id: p, path: /a, sha256: x, manifest_sha256: y, max_memory: 0MiB}\n", []string{"plugins.0.max_memory: must be greater than zero"}},
		{"plugin without sha256", base + "plugins:\n  - {id: p, path: /a, manifest_sha256: m}\n", []string{"plugins.0.sha256: is required"}},
		{"plugin with empty sha256", base + "plugins:\n  - {id: p, path: /a, sha256: '', manifest_sha256: m}\n", []string{"plugins.0.sha256: must be at least 1 characters"}},
		{"not an object", "- a\n- b\n", []string{"document: must be object"}},
		{"empty document", "", []string{"document: is empty"}},
		{"only a comment", "# nothing\n", []string{"document: is empty"}},
		{"yaml non-string key", base + "1: x\n", []string{"line 19: mapping keys must be strings"}},
		{"yaml null key", base + "~: x\n", []string{"line 19: mapping keys must be strings"}},
		{"yaml duplicate key", base + "version: 4\n", []string{"line 19: duplicate key version"}},
		{"yaml duplicate key of odd shape", edit(t, base, "scenario: s.json", "Odd-Key: 1\n      Odd-Key: 2"), []string{"line 8: duplicate key"}},
		{"yaml merge key", edit(t, base, "scenario: s.json", "<<: {a: 1}"), []string{"line 7: merge keys are not supported"}},
		{"yaml custom tag", edit(t, base, "scenario: s.json", "scenario: !custom s.json"), []string{"line 7: unsupported tag"}},
		{"yaml infinity", edit(t, base, "scenario: s.json", "scenario: .inf"), []string{"inputs.0.connection.scenario: number out of range"}},
		{"yaml syntax", "schema: [unclosed\n", []string{"line"}},
		{"yaml two documents", base + "---\nschema: x\n", []string{"document: must hold exactly one YAML document"}},
		{"yaml recursive alias", "a: &x [*x]\n", []string{"line 1: recursive alias"}},
		{"yaml deep nesting", "a: " + strings.Repeat("[", 100) + strings.Repeat("]", 100) + "\n", []string{"document: nesting deeper than 64 levels"}},
		{"json syntax", `{"schema": "laraplate-edge-config/1",}`, []string{"line 1, column 38: invalid JSON"}},
		{"json truncated", `{"schema": `, []string{"document: unexpected end of JSON"}},
		{"json duplicate key", `{"schema": "laraplate-edge-config/1", "schema": "x", "inputs": [], "outputs": []}`, []string{"line 1, column 39: duplicate key schema"}},
		{"json trailing data", `{"schema": "laraplate-edge-config/1", "inputs": [], "outputs": []} {}`, []string{"line 1, column 68: unexpected data after the JSON document"}},
		{"json number out of range", `{"schema": "laraplate-edge-config/1", "version": 1e400, "inputs": [], "outputs": []}`, []string{"version: number out of range"}},
		{"json array document", `[]`, []string{"document: must be object"}},
		{"invalid utf-8", "schema: \xff\n", []string{"document: is not valid UTF-8"}},
		{"min_free_percent zero", edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    buffer: {min_free_percent: 0}"), []string{"outputs.0.buffer.min_free_percent: must be greater than 0"}},
		{"slash in device", edit(t, base, "device: press-07", "device: line/press-07"), []string{"inputs.0.devices.0.device: must not contain /"}},
		{"slash in signal", edit(t, base, "signal: state", "signal: a/state"), []string{"inputs.0.devices.0.points.0.signal: must not contain /"}},
		{"slash in derived signal", edit(t, base, "            read: subscribe\n", "            read: subscribe\n        derived: [{signal: a/b, type: int, expr: '1'}]\n"), []string{"inputs.0.devices.0.derived.0.signal: must not contain /"}},
		{"plain capitalised password", edit(t, base, "scenario: s.json", "scenario: s.json\n      Password: plain"), []string{"inputs.0.connection.*: must be a secret:// reference"}},
		{"plain api_token", edit(t, base, "scenario: s.json", "scenario: s.json\n      api_token: plain"), []string{"inputs.0.connection.api_token: must be a secret:// reference"}},
		{"plain client_secret in a list", edit(t, base, "scenario: s.json", "scenario: s.json\n      oauth: [{client_secret: plain}]"), []string{"inputs.0.connection.oauth.0.client_secret: must be a secret:// reference"}},
		{"plain api_key", edit(t, base, "scenario: s.json", "scenario: s.json\n      api_key: plain"), []string{"inputs.0.connection.api_key: must be a secret:// reference"}},
		{"plain upper-case API_KEY deep", edit(t, base, "scenario: s.json", "scenario: s.json\n      a: {b: {API_KEY: plain}}"), []string{"inputs.0.connection.a.b.*: must be a secret:// reference"}},
		{"plain password next to transforms", edit(t, base, "read: subscribe", "read: subscribe\n            transforms: [{scale: 2}]", "scenario: s.json", "scenario: s.json\n      password: plain"), []string{"inputs.0.connection.password: must be a secret:// reference"}},
		{"derived transform unknown", edit(t, base, "            read: subscribe\n", "            read: subscribe\n        derived: [{signal: d, type: int, expr: '1', transforms: [{explode: 1}]}]\n"), []string{"inputs.0.devices.0.derived.0.transforms.0: unknown transform explode"}},
		{"too large", base + "# " + strings.Repeat("x", config.MaxDocumentSize) + "\n", []string{"document: larger than 4 MiB"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, msgs, err := config.Parse([]byte(tt.doc))
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if len(msgs) == 0 {
				t.Fatal("Parse accepted the document")
			}
			if !reflect.DeepEqual(doc, config.Document{}) {
				t.Errorf("document not zero with messages: %+v", doc)
			}
			for _, want := range tt.want {
				if !slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, want) }) {
					t.Errorf("messages %q, want one containing %q", msgs, want)
				}
			}
		})
	}
}

// transformChain returns base with chain (flow YAML) as the transforms of
// its point.
func transformChain(t *testing.T, chain string) string {
	t.Helper()

	return edit(t, base, "read: subscribe", "read: subscribe\n            transforms: "+chain)
}

// TestParseAcceptsEveryTransformParameterShape covers every parameter shape of
// spec section 4.
func TestParseAcceptsEveryTransformParameterShape(t *testing.T) {
	chains := []string{
		"[{scale: 0.1}]", "[{scale: -2}]", "[{offset: 273.15}]", "[{offset: -40}]",
		"[{round: 0}]", "[{round: 10}]", "[{round: 2.0}]",
		"[{bit: 0}]", "[{bit: 63}]", "[{bcd: true}]",
		"[{map: {table: {'0': idle, '1': running}}}]",
		"[{map: {table: {'1': 1, '2': 2.5}, default: 0}}]",
		"[{map: {table: {'true': 'on'}, default: 'off'}}]",
		"[{map: {table: {}, default: true}}]",
		"[{map: {table: {a: false}}}]",
		"[{valid_range: {min: 0}}]", "[{valid_range: {max: 100.5}}]", "[{valid_range: {min: -1, max: 1}}]",
		"[{deadband: 0}]", "[{deadband: 0.5}]", "[{deadband: '5%'}]", "[{deadband: '0.5%'}]",
		"[{heartbeat: 1m}]", "[{min_interval: 500ms}]", "[{debounce: 1m30s}]",
		"[{edge_counter: rising}]", "[{edge_counter: falling}]", "[{edge_counter: both}]",
		"[{derivative: true}]", "[{trim: true}]",
		"[{regex_extract: {pattern: '(\\d+)'}}]",
		"[{regex_extract: {pattern: 'v=(\\d+)', group: 1, as: integer}}]",
		"[{regex_extract: {pattern: '[0-9.]+', group: 0, as: float}}]",
		"[{regex_extract: {pattern: '(a)(b)', group: 2, as: string}}]",
		"[{scale: 0.1}, {offset: 2}, {round: 1}, {deadband: '1%'}, {heartbeat: 30s}]",
	}

	for _, chain := range chains {
		t.Run(chain, func(t *testing.T) {
			if _, msgs, err := config.Parse([]byte(transformChain(t, chain))); err != nil || len(msgs) != 0 {
				t.Errorf("Parse = %q, %v", msgs, err)
			}
		})
	}
}

// TestParseRefusesTransformParameters checks the exact, fixed messages of the
// parameters the schema refuses.
func TestParseRefusesTransformParameters(t *testing.T) {
	const p = "inputs.0.devices.0.points.0.transforms.0."

	tests := []struct {
		name  string
		chain string
		want  []string
	}{
		{"round above 10", "[{round: 11}]", []string{p + "round: must be at most 10"}},
		{"round fractional", "[{round: 1.5}]", []string{p + "round: must be integer (got number)"}},
		{"bit above 63", "[{bit: 64}]", []string{p + "bit: must be at most 63"}},
		{"bit negative", "[{bit: -1}]", []string{p + "bit: must be at least 0"}},
		{"scale as text", "[{scale: '2'}]", []string{p + "scale: must be number (got string)"}},
		{"offset missing", "[{offset: null}]", []string{p + "offset: must be number (got null)"}},
		{"bcd false", "[{bcd: false}]", []string{p + "bcd: must be true"}},
		{"trim as number", "[{trim: 1}]", []string{p + "trim: must be true"}},
		{"derivative as text", "[{derivative: 'yes'}]", []string{p + "derivative: must be true"}},
		{"deadband percent without sign", "[{deadband: '5'}]", []string{p + "deadband: must be a non-negative number or a percentage such as 5%"}},
		{"deadband negative", "[{deadband: -1}]", []string{p + "deadband: must be at least 0"}},
		{"deadband negative percent", "[{deadband: '-5%'}]", []string{p + "deadband: must be a non-negative number or a percentage such as 5%"}},
		{"heartbeat without unit", "[{heartbeat: 30}]", []string{p + "heartbeat: must be string (got number)"}},
		{"min_interval format", "[{min_interval: soon}]", []string{p + "min_interval: must be a duration such as 500ms, 2s or 1m30s"}},
		{"debounce format", "[{debounce: 1d}]", []string{p + "debounce: must be a duration such as 500ms, 2s or 1m30s"}},
		{"edge_counter mode", "[{edge_counter: up}]", []string{p + "edge_counter: must be one of rising, falling, both"}},
		{"valid_range empty", "[{valid_range: {}}]", []string{p + "valid_range: must have at least 1 keys"}},
		{"valid_range text bound", "[{valid_range: {min: 'a'}}]", []string{p + "valid_range.min: must be number (got string)"}},
		{"valid_range unknown bound", "[{valid_range: {low: 1}}]", []string{p + "valid_range.low: unknown key"}},
		{"valid_range as number", "[{valid_range: 5}]", []string{p + "valid_range: must be object (got number)"}},
		{"regex_extract empty", "[{regex_extract: {}}]", []string{p + "regex_extract.pattern: is required"}},
		{"regex_extract pattern as number", "[{regex_extract: {pattern: 5}}]", []string{p + "regex_extract.pattern: must be string (got number)"}},
		{"regex_extract group negative", "[{regex_extract: {pattern: x, group: -1}}]", []string{p + "regex_extract.group: must be at least 0"}},
		{"regex_extract unknown as", "[{regex_extract: {pattern: x, as: date}}]", []string{p + "regex_extract.as: must be one of string, integer, float"}},
		{"regex_extract unknown key", "[{regex_extract: {pattern: x, flags: i}}]", []string{p + "regex_extract.flags: unknown key"}},
		{"map empty", "[{map: {}}]", []string{p + "map.table: is required"}},
		{"map empty table without default", "[{map: {table: {}}}]", []string{p + "map.table: must have at least 1 keys"}},
		{"map mixed table", "[{map: {table: {a: 1, b: x}}}]", []string{p + "map: table values and default must all be booleans, all numbers or all strings"}},
		{"map default of another kind", "[{map: {table: {a: true}, default: x}}]", []string{p + "map: table values and default must all be booleans, all numbers or all strings"}},
		{"map null value", "[{map: {table: {a: null}}}]", []string{p + "map.table.a: must be boolean or number or string (got null)"}},
		{"map object value", "[{map: {table: {a: {b: 1}}}}]", []string{p + "map.table.a: must be boolean or number or string (got object)"}},
		{"map unknown key", "[{map: {table: {a: 1}, fallback: 2}}]", []string{p + "map.fallback: unknown key"}},
		{"map table as list", "[{map: {table: [1]}}]", []string{p + "map.table: must be object (got array)"}},
		{"second step refused", "[{scale: 2}, {round: 11}]", []string{"inputs.0.devices.0.points.0.transforms.1.round: must be at most 10"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, msgs, err := config.Parse([]byte(transformChain(t, tt.chain)))
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if !reflect.DeepEqual(doc, config.Document{}) {
				t.Errorf("document not zero with messages: %+v", doc)
			}
			if !slices.Equal(msgs, tt.want) {
				t.Errorf("messages =\n%s\nwant\n%s", strings.Join(msgs, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

// TestParseNeverEchoesValues places a sentinel where a secret could be written
// by mistake and checks that no message repeats it, whatever the failure.
// Values are never echoed. Keys are echoed only when they have the shape of a
// document key (ruling R31), so key positions are checked with a sentinel of
// another shape.
func TestParseNeverEchoesValues(t *testing.T) {
	const oddShape, keyShape = "Hunter2-Plain-Secret-998877", "hunter2plainsecret998877"

	valueDocs := func(secret string) map[string]string {
		return map[string]string{
			"output token":           edit(t, base, "secret://env/TOKEN", secret),
			"connection password":    edit(t, base, "scenario: s.json", "scenario: s.json\n      auth: {password: "+secret+"}"),
			"connection deep secret": edit(t, base, "scenario: s.json", "scenario: s.json\n      a: {b: [{c: {secret: "+secret+"}}]}"),
			"connection Password":    edit(t, base, "scenario: s.json", "scenario: s.json\n      Password: "+secret),
			"connection api_token":   edit(t, base, "scenario: s.json", "scenario: s.json\n      api_token: "+secret),
			"connection secret list": edit(t, base, "scenario: s.json", "scenario: s.json\n      oauth: [{client_secret: "+secret+"}]"),
			"connection API_KEY":     edit(t, base, "scenario: s.json", "scenario: s.json\n      a: {API_KEY: "+secret+"}"),
			"connection api_key":     edit(t, base, "scenario: s.json", "scenario: s.json\n      api_key: "+secret),
			"malformed reference":    edit(t, base, "secret://env/TOKEN", "secret://vault/"+secret),
			"env reference name":     edit(t, base, "secret://env/TOKEN", "secret://env/"+secret+"-x"),
			"driver":                 edit(t, base, "driver: simulated", "driver: "+secret),
			"output type":            edit(t, base, "type: laraplate-http", "type: "+secret),
			"schema":                 edit(t, base, "laraplate-edge-config/1", secret),
			"version":                edit(t, base, "version: 3", "version: "+secret),
			"read":                   edit(t, base, "read: subscribe", "read: "+secret),
			"priority":               edit(t, base, "read: subscribe", "read: subscribe\n            priority: "+secret),
			"decoding type":          edit(t, base, "read: subscribe", "read: subscribe\n            type: "+secret),
			"duration":               edit(t, base, "read: subscribe", "read: {poll: "+secret+"}"),
			"size":                   edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    buffer: {max_size: "+secret+"}"),
			"subscribe sampling":     edit(t, base, "read: subscribe", "read: {subscribe: {sampling: "+secret+"}}"),
			"subscribe queue":        edit(t, base, "read: subscribe", "read: {subscribe: {queue: "+secret+"}}"),
			"subscribe deadband":     edit(t, base, "read: subscribe", "read: {subscribe: {deadband: "+secret+"}}"),
			"subscribe object":       edit(t, base, "read: subscribe", "read: {subscribe: "+secret+"}"),
			"url credentials":        edit(t, base, "https://erp.example.com", "https://"+secret+":x@erp.example.com"),
			"url scheme":             edit(t, base, "https://erp.example.com", secret+"://erp.example.com"),
			"route":                  edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    route: {include: ['["+secret+"']}"),
			"unknown key value":      edit(t, base, "read: subscribe", "read: subscribe\n            colour: "+secret),
			"duplicate id and token": base + "  - {id: main, type: laraplate-http, url: 'https://b.example.com', token: " + secret + "}\n",
			"yaml alias":             edit(t, base, "scenario: s.json", "scenario: *"+secret),
			"yaml int tag":           edit(t, base, "scenario: s.json", "scenario: !!int "+secret),
			"yaml custom tag":        edit(t, base, "scenario: s.json", "scenario: !"+secret+" x"),
			"yaml non-string key":    edit(t, base, "scenario: s.json", "? ["+secret+"]\n      : x"),
			"yaml syntax":            "schema: \"" + secret + "\n  bad: [\n",
			"json syntax":            `{"schema": ` + secret + `}`,
			"json type":              `{"schema": "laraplate-edge-config/1", "inputs": "` + secret + `", "outputs": []}`,
			"json trailing":          `{"schema": "x"} ` + secret,
			"transform number":       transformChain(t, "[{round: "+secret+"}]"),
			"transform true":         transformChain(t, "[{bcd: "+secret+"}]"),
			"transform deadband":     transformChain(t, "[{deadband: "+secret+"}]"),
			"transform duration":     transformChain(t, "[{heartbeat: "+secret+"}]"),
			"transform edge mode":    transformChain(t, "[{edge_counter: "+secret+"}]"),
			"transform map kinds":    transformChain(t, "[{map: {table: {a: 1, b: "+secret+"}}}]"),
			"transform map default":  transformChain(t, "[{map: {table: {a: true}, default: "+secret+"}}]"),
			"transform regex as":     transformChain(t, "[{regex_extract: {pattern: x, as: "+secret+"}}]"),
			"transform regex group":  transformChain(t, "[{regex_extract: {pattern: x, group: "+secret+"}}]"),
			"transform range bound":  transformChain(t, "[{valid_range: {min: "+secret+"}}]"),
		}
	}
	keyDocs := map[string]string{
		"id":                 edit(t, base, "id: main", "id: "+oddShape),
		"unknown key":        edit(t, base, "read: subscribe", "read: subscribe\n            "+oddShape+": 1"),
		"subscribe key":      edit(t, base, "read: subscribe", "read: {subscribe: {"+oddShape+": 1}}"),
		"transform name":     edit(t, base, "read: subscribe", "read: subscribe\n            transforms: [{"+oddShape+": 1}]"),
		"yaml duplicate key": edit(t, base, "scenario: s.json", oddShape+": 1\n      "+oddShape+": 2"),
		"json duplicate key": `{"` + oddShape + `": 1, "` + oddShape + `": 2}`,
		"connection key":     edit(t, base, "scenario: s.json", "scenario: s.json\n      "+oddShape+": {password: plain}"),
		"map table key":      transformChain(t, "[{map: {table: {"+oddShape+": {x: 1}}}}]"),
		"range key":          transformChain(t, "[{valid_range: {"+oddShape+": 1}}]"),
		"regex key":          transformChain(t, "[{regex_extract: {pattern: x, "+oddShape+": 1}}]"),
	}

	cases := map[string]string{}
	for _, secret := range []string{oddShape, keyShape} {
		for name, doc := range valueDocs(secret) {
			cases[name+"/"+secret[:8]] = doc
		}
	}
	for name, doc := range keyDocs {
		cases["key "+name] = doc
	}

	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, msgs, err := config.Parse([]byte(doc))
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if len(msgs) == 0 {
				t.Fatal("Parse accepted the document")
			}
			for _, m := range msgs {
				if strings.Contains(strings.ToLower(m), "hunter2") {
					t.Errorf("message echoes a value: %q", m)
				}
			}
		})
	}
}

func TestCheckBuild(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{"simulated and laraplate-http", base, nil},
		{"env and file references", edit(t, base, "scenario: s.json", "scenario: s.json\n      password: secret://file/etc/p\n      token: secret://env/T"), nil},
		{"modbus", edit(t, base, "driver: simulated", "driver: modbus"), nil},
		{"opcua", edit(t, base, "driver: simulated", "driver: opcua"), nil},
		{"plugin driver", edit(t, base, "driver: simulated", "driver: 'plugin:fanuc'"), nil},
		{"mqtt output", edit(t, base, "type: laraplate-http", "type: laraplate-mqtt"), []string{"outputs.0.type: not available in this build"}},
		{"plugin output", edit(t, base, "type: laraplate-http", "type: 'plugin:kafka'"), []string{"outputs.0.type: not available in this build"}},
		{"keystore token", edit(t, base, "secret://env/TOKEN", "secret://keystore/main"), []string{"outputs.0.token: not available in this build"}},
		{"keystore in a connection list", edit(t, base, "scenario: s.json", "scenario: s.json\n      peers: [{a: {secret: 'secret://keystore/x'}}]"), []string{"inputs.0.connection.peers.0.a.secret: not available in this build"}},
		{"keystore under an odd key", edit(t, base, "scenario: s.json", "scenario: s.json\n      Odd-Key: {token: 'secret://keystore/x'}"), []string{"inputs.0.connection.*.token: not available in this build"}},
		{"point transforms", edit(t, base, "read: subscribe", "read: subscribe\n            transforms: [{scale: 2}, {deadband: '5%'}, {heartbeat: 1m}]"), nil},
		{"empty transforms", edit(t, base, "read: subscribe", "read: subscribe\n            transforms: []"), nil},
		{"keystore under a suffixed key", edit(t, base, "scenario: s.json", "scenario: s.json\n      Client_Secret: 'secret://keystore/x'\n      api_key: 'secret://keystore/y'"), []string{"inputs.0.connection.*: not available in this build", "inputs.0.connection.api_key: not available in this build"}},
		{"derived with transforms", edit(t, base, "            read: subscribe\n", "            read: subscribe\n        derived: [{signal: d, type: int, expr: '1', transforms: [{deadband: 1}]}]\n"), []string{"inputs.0.devices.0.derived: not available in this build", "inputs.0.devices.0.derived.0.transforms: not available in this build"}},
		{"derived", edit(t, base, "            read: subscribe\n", "            read: subscribe\n        derived: [{signal: d, type: int, expr: '1'}]\n"), []string{"inputs.0.devices.0.derived: not available in this build"}},
		{"plugins", base + "plugins:\n  - {id: a, path: /a, sha256: x, manifest_sha256: m}\n  - {id: b, path: /b, sha256: y, manifest_sha256: n, max_memory: 64MiB}\n", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := config.CheckBuild(mustParse(t, []byte(tt.doc)))
			if !slices.Equal(got, tt.want) {
				t.Errorf("CheckBuild = %q, want %q", got, tt.want)
			}
		})
	}
}

func compile(t *testing.T, url string, data []byte) *jsonschema.Schema {
	t.Helper()

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse schema %s: %v", url, err)
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatalf("add schema %s: %v", url, err)
	}

	s, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compile schema %s: %v", url, err)
	}

	return s
}

// TestBackendFixturesValidate is decision C2: every valid document of the
// backend's structural schema tests validates against the agent schema.
func TestBackendFixturesValidate(t *testing.T) {
	structural := compile(t, "structural.json", readFixture(t, "backend/laraplate-edge-config-1.structural.schema.json"))
	agent := compile(t, schema.ConfigV1ID, schema.ConfigV1())

	names, err := filepath.Glob(filepath.Join("testdata", "backend", "*.json"))
	if err != nil {
		t.Fatal(err)
	}

	fixtures := 0
	for _, path := range names {
		if strings.HasSuffix(path, ".schema.json") {
			continue
		}
		fixtures++

		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}

			if err := structural.Validate(inst); err != nil {
				t.Fatalf("backend schema refuses its own fixture: %v", err)
			}
			if err := agent.Validate(inst); err != nil {
				t.Errorf("agent schema refuses a backend fixture: %v", err)
			}
			if _, msgs, err := config.Parse(data); err != nil || len(msgs) != 0 {
				t.Errorf("Parse = %q, %v", msgs, err)
			}
		})
	}

	if fixtures != 4 {
		t.Errorf("found %d backend fixtures, want 4", fixtures)
	}
}

// TestSchemaDefaultsMatchTheCode keeps the published defaults and the ones the
// parser applies identical.
func TestSchemaDefaultsMatchTheCode(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal(schema.ConfigV1(), &s); err != nil {
		t.Fatal(err)
	}

	lookup := func(path ...string) any {
		var node any = s
		for _, key := range path {
			m, ok := node.(map[string]any)
			if !ok {
				t.Fatalf("schema path %v: not an object at %s", path, key)
			}
			node = m[key]
		}
		return node
	}

	defs := func(rest ...string) []string { return append([]string{"$defs"}, rest...) }
	out := func(group, key string) []string {
		return defs("output", "properties", group, "properties", key, "default")
	}

	doc := mustParse(t, []byte(base))
	o := doc.Outputs[0]

	tests := []struct {
		path []string
		want any
	}{
		{defs("input", "properties", "death_tolerance", "default"), "30s"},
		{defs("priority", "default"), "critical"},
		{out("route", "include"), []any{"*"}},
		{out("batch", "max_samples"), float64(1000)},
		{out("batch", "max_body"), "1024KiB"},
		{out("batch", "max_wait"), "2s"},
		{out("buffer", "max_size"), "1GiB"},
		{out("buffer", "min_free_percent"), float64(5)},
		{out("buffer", "fsync_interval"), "200ms"},
		{defs("output", "properties", "batch", "properties", "max_samples", "maximum"), float64(config.MaxSamples)},
	}
	for _, tt := range tests {
		if got := lookup(tt.path...); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("schema %s = %v, want %v", strings.Join(tt.path, "/"), got, tt.want)
		}
	}

	if doc.Inputs[0].DeathTolerance != config.DefaultDeathTolerance || doc.Inputs[0].Devices[0].Points[0].Priority != model.Critical {
		t.Errorf("input defaults = %+v", doc.Inputs[0])
	}
	wantBatch := config.Batch{MaxSamples: config.DefaultMaxSamples, MaxBody: config.DefaultMaxBody, MaxWait: config.DefaultMaxWait}
	wantBuffer := config.Buffer{MaxSize: config.DefaultBufferMaxSize, MinFreePercent: config.DefaultMinFreePercent, FsyncInterval: config.DefaultFsyncInterval}
	if o.Batch != wantBatch || o.Buffer != wantBuffer || !slices.Equal(o.Route.Include, []string{"*"}) {
		t.Errorf("output defaults = %+v", o)
	}
	if config.DefaultMaxSamples != 1000 || config.DefaultMaxBody != 1024<<10 || config.MaxBody != 1024<<10 || config.DefaultMaxWait != 2*time.Second ||
		config.DefaultBufferMaxSize != 1<<30 || config.DefaultMinFreePercent != 5 || config.DefaultFsyncInterval != 200*time.Millisecond ||
		config.DefaultDeathTolerance != 30*time.Second || config.MaxSamples != 5000 {
		t.Error("default constants drifted from the spec values")
	}
}

func TestRouteMatch(t *testing.T) {
	tests := []struct {
		include []string
		device  string
		signal  string
		want    bool
	}{
		{[]string{"*"}, "press-07", "state", true},
		{[]string{"press-07"}, "press-07", "state", true},
		{[]string{"press-07"}, "press-08", "state", false},
		{[]string{"press-*"}, "press-08", "temp", true},
		{[]string{"press-07/state"}, "press-07", "state", true},
		{[]string{"press-07/state"}, "press-07", "temp", false},
		{[]string{"*/temp*"}, "oven-02", "temp_zone1", true},
		{[]string{"oven-*", "press-07/state"}, "press-07", "state", true},
		{[]string{"oven-*", "press-07/state"}, "press-07", "temp", false},
		{nil, "press-07", "state", false},
	}
	for _, tt := range tests {
		r := config.Route{Include: tt.include}
		if got := r.Match(tt.device, tt.signal); got != tt.want {
			t.Errorf("Route%v.Match(%q, %q) = %v, want %v", tt.include, tt.device, tt.signal, got, tt.want)
		}
	}
}

func TestPointDataType(t *testing.T) {
	tests := map[string]string{
		"":        model.DataTypeNumber,
		"bool":    model.DataTypeBoolean,
		"string":  model.DataTypeString,
		"int16":   model.DataTypeNumber,
		"uint64":  model.DataTypeNumber,
		"float32": model.DataTypeNumber,
	}
	for typ, want := range tests {
		if got := (config.Point{Type: typ}).DataType(); got != want {
			t.Errorf("Point{Type: %q}.DataType() = %q, want %q", typ, got, want)
		}
	}
}

func TestParseDerivedOnInterval(t *testing.T) {
	doc := mustParse(t, []byte(edit(t, base, "            read: subscribe\n",
		"            read: subscribe\n        derived:\n          - {signal: d, type: int, priority: bulk, expr: '1', on: {interval: 1s}}\n")))

	want := []config.Derived{{Signal: "d", Type: "int", Priority: model.Bulk, Expr: "1", Interval: time.Second}}
	if got := doc.Inputs[0].Devices[0].Derived; !reflect.DeepEqual(got, want) {
		t.Errorf("derived = %+v, want %+v", got, want)
	}
}

func TestParseIsSafeForConcurrentUse(t *testing.T) {
	data := readFixture(t, "build.yaml")
	want := mustParse(t, data)

	errs := make(chan string, 8)
	for range 8 {
		go func() {
			doc, msgs, err := config.Parse(data)
			switch {
			case err != nil || len(msgs) != 0:
				errs <- fmt.Sprintf("Parse = %q, %v", msgs, err)
			case !reflect.DeepEqual(doc, want):
				errs <- "a concurrent Parse gave another document"
			default:
				errs <- ""
			}
		}()
	}
	for range 8 {
		if e := <-errs; e != "" {
			t.Error(e)
		}
	}
}

// TestParseReportsSchemaAndSemanticProblemsTogether checks that one run lists
// every problem, and that a value the schema already refused gets no second
// message from the semantic checks.
func TestParseReportsSchemaAndSemanticProblemsTogether(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "plain token, remote http url, duplicate output id",
			doc: edit(t, base, "https://erp.example.com", "http://erp.example.com", "secret://env/TOKEN", "plain") +
				"  - {id: main, type: laraplate-http, url: 'https://b.example.com', token: 'secret://env/B', batch: {max_body: 2MiB}}\n",
			want: []string{
				"outputs.0.token: " + "must be a secret:// reference (secret://env/NAME, secret://file/PATH or secret://keystore/NAME), not a plain value",
				"outputs.0.url: must use https (http is accepted only for a loopback host)",
				"outputs.1.id: duplicate of outputs.0.id",
				"outputs.1.batch.max_body: must not exceed 1024KiB (the backend default of mes.machine.max_body_kb)",
			},
		},
		{
			name: "values refused by the schema are not checked again",
			doc: edit(t, base, "    devices:\n", "    death_tolerance: soon\n    devices:\n", "read: subscribe", "read: 5",
				"secret://env/TOKEN", "secret://env/TOKEN\n    batch: {max_body: 10, max_wait: '0'}\n    buffer: wrong\n    route: {include: ['']}"),
			want: []string{
				"inputs.0.death_tolerance: must be a duration such as 500ms, 2s or 1m30s",
				"inputs.0.devices.0.points.0.read: must be object (got number)",
				"outputs.0.batch.max_body: must be string (got number)",
				"outputs.0.batch.max_wait: must be a duration such as 500ms, 2s or 1m30s",
				"outputs.0.buffer: must be object (got string)",
				"outputs.0.route.include.0: must be at least 1 characters",
			},
		},
		{
			name: "missing ids and device names do not count as duplicates",
			doc: edit(t, base, "  - id: sim-line\n", "  - id: ''\n", "device: press-07", "device: ''") +
				"  - {type: laraplate-http, url: 'https://b.example.com', token: 'secret://env/B'}\n  - {type: laraplate-http, url: 'https://c.example.com', token: 'secret://env/C'}\n",
			want: []string{
				"inputs.0.devices.0.device: must be at least 1 characters",
				"inputs.0.id: must be 1 to 64 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit",
				"outputs.1.id: is required",
				"outputs.2.id: is required",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, msgs, err := config.Parse([]byte(tt.doc))
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			got := slices.Sorted(slices.Values(msgs))
			want := slices.Sorted(slices.Values(tt.want))
			if !slices.Equal(got, want) {
				t.Errorf("messages =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

func TestParseConnectionSecretKeysAcceptReferences(t *testing.T) {
	doc := mustParse(t, []byte(edit(t, base, "scenario: s.json",
		"scenario: s.json\n      Password: secret://env/P\n      api_token: secret://file/etc/t\n      oauth: [{client_secret: secret://env/S}]\n      API_KEY: secret://env/K\n      token_url: https://idp.example.com/token\n      tokens: 3")))

	want := `{"API_KEY":"secret://env/K","Password":"secret://env/P","api_token":"secret://file/etc/t","oauth":[{"client_secret":"secret://env/S"}],"scenario":"s.json","token_url":"https://idp.example.com/token","tokens":3}`
	if got := string(doc.Inputs[0].Connection); got != want {
		t.Errorf("connection = %s, want %s", got, want)
	}
}

func TestParseDerivedTransforms(t *testing.T) {
	doc := mustParse(t, []byte(edit(t, base, "            read: subscribe\n",
		"            read: subscribe\n        derived: [{signal: d, type: int, expr: '1', transforms: [{deadband: 0.5}, {heartbeat: 10s}]}]\n")))

	want := []config.Transform{{Name: "deadband", Params: json.RawMessage(`0.5`)}, {Name: "heartbeat", Params: json.RawMessage(`"10s"`)}}
	if got := doc.Inputs[0].Devices[0].Derived[0].Transforms; !reflect.DeepEqual(got, want) {
		t.Errorf("derived transforms = %+v, want %+v", got, want)
	}
}

// TestParseVersionBeyond32Bits keeps versions above 2^31 valid on 32-bit
// platforms (run with GOARCH=386 as well).
func TestParseVersionBeyond32Bits(t *testing.T) {
	for _, version := range []int64{3_000_000_000, 9_007_199_254_740_991} {
		v := strconv.FormatInt(version, 10)
		for _, data := range []string{
			edit(t, base, "version: 3", "version: "+v),
			`{"schema":"laraplate-edge-config/1","version":` + v + `,"inputs":[],"outputs":[]}`,
		} {
			doc, msgs, err := config.Parse([]byte(data))
			if err != nil || len(msgs) != 0 {
				t.Fatalf("Parse(version %s) = %q, %v", v, msgs, err)
			}
			if doc.Version != version {
				t.Errorf("version = %d, want %d", doc.Version, version)
			}
		}
	}
}

// TestBufferMaxSizeAtTheMinimum: two segments of 16 MiB is the smallest
// buffer, in any unit.
func TestBufferMaxSizeAtTheMinimum(t *testing.T) {
	for _, size := range []string{"32MiB", "32768KiB", "33554432B", "1GiB"} {
		doc := mustParse(t, []byte(edit(t, base, "secret://env/TOKEN", "secret://env/TOKEN\n    buffer: {max_size: "+size+"}")))
		if got := doc.Outputs[0].Buffer.MaxSize; got < config.MinBufferMaxSize {
			t.Errorf("max_size %s parsed as %d, below the minimum", size, got)
		}
	}
}

// TestDeadbandNumbersReachDriversInDecimal: a number YAML or JSON writes
// with an exponent, or that encoding/json renders with one, is plain decimal
// text in the document.
func TestDeadbandNumbersReachDriversInDecimal(t *testing.T) {
	const jsonDoc = `{"schema": "laraplate-edge-config/1", "version": 3,
"inputs": [{"id": "sim-line", "driver": "simulated", "connection": {"scenario": "s.json"},
  "devices": [{"device": "press-07", "points": [{"signal": "state", "address": "state", "read": {"subscribe": {"deadband": %s}}}]}]}],
"outputs": [{"id": "main", "type": "laraplate-http", "url": "https://erp.example.com", "token": "secret://env/TOKEN"}]}`

	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"YAML small", edit(t, base, "read: subscribe", "read: {subscribe: {deadband: 0.0000001}}"), "0.0000001"},
		{"YAML large", edit(t, base, "read: subscribe", "read: {subscribe: {deadband: 1e21}}"), "1000000000000000000000"},
		{"JSON small", fmt.Sprintf(jsonDoc, "0.0000001"), "0.0000001"},
		{"JSON large", fmt.Sprintf(jsonDoc, "1e21"), "1000000000000000000000"},
		{"JSON exponent", fmt.Sprintf(jsonDoc, "2.5E-3"), "0.0025"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mustParse(t, []byte(tt.doc))
			if got := doc.Inputs[0].Devices[0].Points[0].Read.Subscribe.Deadband; got != tt.want {
				t.Errorf("deadband = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseSubscribeLongForm(t *testing.T) {
	tests := []struct {
		name string
		read string
		want config.Read
	}{
		{"string form", "subscribe", config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: -1}}},
		{"empty long form", "{subscribe: {}}", config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: -1}}},
		{"all keys", "{subscribe: {sampling: 250ms, queue: 5, deadband: 2%}}", config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: 250 * time.Millisecond, Queue: 5, Deadband: "2%"}}},
		{"percent at the cap", "{subscribe: {deadband: 100%}}", config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: -1, Deadband: "100%"}}},
		{"absolute number", "{subscribe: {deadband: 0.5}}", config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: -1, Deadband: "0.5"}}},
		{"absolute text", "{subscribe: {deadband: '3'}}", config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: -1, Deadband: "3"}}},
		{"queue only", "{subscribe: {queue: 4294967295}}", config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: -1, Queue: 4294967295}}},
		{"poll leaves Subscribe nil", "{poll: 2s}", config.Read{Mode: config.ReadPoll, Interval: 2 * time.Second}},
		{"event leaves Subscribe nil", "event", config.Read{Mode: config.ReadEvent}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mustParse(t, []byte(edit(t, base, "read: subscribe", "read: "+tt.read)))
			if got := doc.Inputs[0].Devices[0].Points[0].Read; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("read = %+v (subscribe %+v), want %+v (subscribe %+v)", got, got.Subscribe, tt.want, tt.want.Subscribe)
			}
		})
	}
}
