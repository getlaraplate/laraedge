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

	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/schema"
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
	wantReads := []config.Read{{Mode: config.ReadSubscribe}, {Mode: config.ReadPoll, Interval: time.Second}, {Mode: config.ReadPoll, Interval: time.Second}}
	wantPriorities := []model.Priority{model.Critical, model.Critical, model.Bulk}
	for i, p := range points {
		if p.Read != wantReads[i] || p.Priority != wantPriorities[i] {
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

	if p := doc.Plugins[0]; p.ID != "fanuc" || p.Path != "/opt/laraplate-edge/plugins/fanuc-focas" || p.SHA256 != "..." {
		t.Errorf("plugin = %+v", p)
	}
}

func TestCheckBuildRefusesExactlyTheSpecExampleNonBuildParts(t *testing.T) {
	doc := mustParse(t, readFixture(t, "spec-5.3.yaml"))

	got := config.CheckBuild(doc)
	want := []string{
		"inputs.0.driver: not available in this build",
		"inputs.0.connection.auth.password: not available in this build",
		"inputs.0.devices.0.points.2.transforms: not available in this build",
		"inputs.0.devices.0.derived: not available in this build",
		"outputs.0.token: not available in this build",
		"plugins.0: not available in this build",
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
		{Signal: "state", Address: "state", Read: config.Read{Mode: config.ReadSubscribe}, Priority: model.Critical, Type: "int16"},
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
		{"duplicate plugin id", base + "plugins:\n  - {id: p, path: /a, sha256: x}\n  - {id: p, path: /b, sha256: y}\n", []string{"plugins.1.id: duplicate of plugins.0.id"}},
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
		{"plugin without sha256", base + "plugins:\n  - {id: p, path: /a}\n", []string{"plugins.0.sha256: is required"}},
		{"plugin with empty sha256", base + "plugins:\n  - {id: p, path: /a, sha256: ''}\n", []string{"plugins.0.sha256: must be at least 1 characters"}},
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
		}
	}
	keyDocs := map[string]string{
		"id":                 edit(t, base, "id: main", "id: "+oddShape),
		"unknown key":        edit(t, base, "read: subscribe", "read: subscribe\n            "+oddShape+": 1"),
		"transform name":     edit(t, base, "read: subscribe", "read: subscribe\n            transforms: [{"+oddShape+": 1}]"),
		"yaml duplicate key": edit(t, base, "scenario: s.json", oddShape+": 1\n      "+oddShape+": 2"),
		"json duplicate key": `{"` + oddShape + `": 1, "` + oddShape + `": 2}`,
		"connection key":     edit(t, base, "scenario: s.json", "scenario: s.json\n      "+oddShape+": {password: plain}"),
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
		{"modbus", edit(t, base, "driver: simulated", "driver: modbus"), []string{"inputs.0.driver: not available in this build"}},
		{"plugin driver", edit(t, base, "driver: simulated", "driver: 'plugin:fanuc'"), []string{"inputs.0.driver: not available in this build"}},
		{"mqtt output", edit(t, base, "type: laraplate-http", "type: laraplate-mqtt"), []string{"outputs.0.type: not available in this build"}},
		{"plugin output", edit(t, base, "type: laraplate-http", "type: 'plugin:kafka'"), []string{"outputs.0.type: not available in this build"}},
		{"keystore token", edit(t, base, "secret://env/TOKEN", "secret://keystore/main"), []string{"outputs.0.token: not available in this build"}},
		{"keystore in a connection list", edit(t, base, "scenario: s.json", "scenario: s.json\n      peers: [{a: {secret: 'secret://keystore/x'}}]"), []string{"inputs.0.connection.peers.0.a.secret: not available in this build"}},
		{"keystore under an odd key", edit(t, base, "scenario: s.json", "scenario: s.json\n      Odd-Key: {token: 'secret://keystore/x'}"), []string{"inputs.0.connection.*.token: not available in this build"}},
		{"transforms", edit(t, base, "read: subscribe", "read: subscribe\n            transforms: [{scale: 2}]"), []string{"inputs.0.devices.0.points.0.transforms: not available in this build"}},
		{"empty transforms", edit(t, base, "read: subscribe", "read: subscribe\n            transforms: []"), nil},
		{"keystore under a suffixed key", edit(t, base, "scenario: s.json", "scenario: s.json\n      Client_Secret: 'secret://keystore/x'\n      api_key: 'secret://keystore/y'"), []string{"inputs.0.connection.*: not available in this build", "inputs.0.connection.api_key: not available in this build"}},
		{"derived with transforms", edit(t, base, "            read: subscribe\n", "            read: subscribe\n        derived: [{signal: d, type: int, expr: '1', transforms: [{deadband: 1}]}]\n"), []string{"inputs.0.devices.0.derived: not available in this build", "inputs.0.devices.0.derived.0.transforms: not available in this build"}},
		{"derived", edit(t, base, "            read: subscribe\n", "            read: subscribe\n        derived: [{signal: d, type: int, expr: '1'}]\n"), []string{"inputs.0.devices.0.derived: not available in this build"}},
		{"plugins", base + "plugins:\n  - {id: a, path: /a, sha256: x}\n  - {id: b, path: /b, sha256: y}\n", []string{"plugins.0: not available in this build", "plugins.1: not available in this build"}},
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
