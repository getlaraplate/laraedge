package transform_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/transform"
)

// chainDocument returns a document with one point of decoding type pointType
// (none when empty) carrying chain, a flow YAML (or JSON) list.
func chainDocument(inputID, device, signal, pointType, chain string) string {
	typeLine := ""
	if pointType != "" {
		typeLine = "\n            type: " + pointType
	}

	return `schema: laraplate-edge-config/1
inputs:
  - id: ` + inputID + `
    driver: simulated
    devices:
      - device: ` + device + `
        points:
          - signal: ` + signal + `
            address: a
            read: subscribe` + typeLine + `
            transforms: ` + chain + `
outputs: []
`
}

func parseDocument(t *testing.T, text string) config.Document {
	t.Helper()

	doc, msgs, err := config.Parse([]byte(text))
	if err != nil || len(msgs) != 0 {
		t.Fatalf("Parse = %q, %v", msgs, err)
	}

	return doc
}

func TestCheckDocument(t *testing.T) {
	const twoInputs = `schema: laraplate-edge-config/1
inputs:
  - id: line_a
    driver: simulated
    devices:
      - device: press_07
        points:
          - {signal: ok, address: a, read: subscribe, type: int16, transforms: [{bit: 3}, {edge_counter: rising}]}
          - {signal: temp, address: b, read: subscribe, type: float32, transforms: [{scale: 0.1}, {bit: 3}]}
      - device: oven-02
        points:
          - {signal: Zone-1, address: c, read: subscribe, type: string, transforms: [{trim: true}, {round: 1}]}
  - id: line-b
    driver: simulated
    devices:
      - device: press_08
        points:
          - {signal: count, address: d, read: subscribe, type: bool, transforms: [{edge_counter: both}, {heartbeat: 0s}]}
          - {signal: plain, address: e, read: subscribe}
outputs: []
`

	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "every transform on a kind it accepts",
			doc: `schema: laraplate-edge-config/1
inputs:
  - id: line_a
    driver: simulated
    devices:
      - device: press_07
        points:
          - {signal: a, address: a, read: subscribe, type: uint16, transforms: [{bcd: true}, {scale: 0.1}, {offset: -2}, {round: 1}, {valid_range: {min: 0}}, {deadband: '5%'}, {derivative: true}, {heartbeat: 1m}, {min_interval: 1s}, {debounce: 2s}]}
          - {signal: b, address: b, read: subscribe, type: int32, transforms: [{bit: 4}, {edge_counter: falling}]}
          - {signal: c, address: c, read: subscribe, type: string, transforms: [{trim: true}, {regex_extract: {pattern: 'v=(\d+)', as: integer}}, {map: {table: {'1': run}, default: idle}}]}
          - {signal: d, address: d, read: subscribe, transforms: [{round: 2}]}
          - {signal: e, address: e, read: subscribe}
outputs: []
`,
		},
		{
			name: "bit after float32",
			doc:  chainDocument("line_a", "press_07", "temp", "float32", "[{bit: 3}]"),
			want: []string{"inputs.0.devices.0.points.0.transforms: input line_a, device press_07, signal temp, step 1 (bit): accepts integer, got float"},
		},
		{
			name: "bit after scale on an integer",
			doc:  chainDocument("line_a", "press_07", "temp", "int16", "[{scale: 2}, {bit: 3}]"),
			want: []string{"inputs.0.devices.0.points.0.transforms: input line_a, device press_07, signal temp, step 2 (bit): accepts integer, got float"},
		},
		{
			name: "every bad chain in document order, names only of the safe shape",
			doc:  twoInputs,
			want: []string{
				"inputs.0.devices.0.points.1.transforms: input line_a, device press_07, signal temp, step 2 (bit): accepts integer, got float",
				"inputs.0.devices.1.points.0.transforms: input line_a, device *, signal *, step 2 (round): accepts number, got string",
				"inputs.1.devices.0.points.0.transforms: input *, device press_08, signal count, step 2 (heartbeat): parameter must be a positive duration such as 500ms, 2s or 1m30s",
			},
		},
		{
			name: "valid_range with min above max",
			doc:  chainDocument("line_a", "press_07", "temp", "float64", "[{valid_range: {min: 2, max: 1}}]"),
			want: []string{"inputs.0.devices.0.points.0.transforms: input line_a, device press_07, signal temp, step 1 (valid_range): min must not exceed max"},
		},
		{
			name: "regex_extract with an invalid pattern",
			doc:  chainDocument("line_a", "press_07", "temp", "string", "[{regex_extract: {pattern: '(a'}}]"),
			want: []string{"inputs.0.devices.0.points.0.transforms: input line_a, device press_07, signal temp, step 1 (regex_extract): pattern is not a valid RE2 expression"},
		},
		{
			name: "regex_extract with a missing group",
			doc:  chainDocument("line_a", "press_07", "temp", "string", "[{regex_extract: {pattern: 'a', group: 1}}]"),
			want: []string{"inputs.0.devices.0.points.0.transforms: input line_a, device press_07, signal temp, step 1 (regex_extract): group does not exist in the pattern"},
		},
		{
			name: "edge_counter on a number",
			doc:  chainDocument("line_a", "press_07", "temp", "", "[{edge_counter: rising}]"),
			want: []string{"inputs.0.devices.0.points.0.transforms: input line_a, device press_07, signal temp, step 1 (edge_counter): accepts boolean, got float"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := transform.CheckDocument(parseDocument(t, tt.doc))
			if !slices.Equal(got, tt.want) {
				t.Errorf("CheckDocument =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

// TestCheckDocumentNeverEchoesValues places a sentinel in every name and in a
// parameter only Go refuses: no message may repeat it.
func TestCheckDocumentNeverEchoesValues(t *testing.T) {
	const secret = "hunter2plain"

	docs := map[string]string{
		"names":         chainDocument("hunter2-x", "hunter2-press", "hunter2-signal", "float32", "[{bit: 3}]"),
		"safe names":    chainDocument("line_a", "hunter2plain", "hunter2plain_x", "float32", "[{bit: 3}]"),
		"regex pattern": chainDocument("line_a", "press_07", "temp", "string", "[{regex_extract: {pattern: '(hunter2plain'}}]"),
		"map number":    chainDocument("line_a", "press_07", "temp", "int16", "[{map: {table: {'1': 18446744073709551615}}}]"),
	}

	for name, text := range docs {
		t.Run(name, func(t *testing.T) {
			msgs := transform.CheckDocument(parseDocument(t, text))
			if len(msgs) == 0 {
				t.Fatal("CheckDocument accepted the document")
			}

			for _, m := range msgs {
				if name != "safe names" && strings.Contains(m, secret) {
					t.Errorf("message echoes a value: %q", m)
				}

				if name == "safe names" && !strings.Contains(m, "device hunter2plain,") {
					t.Errorf("message hides a name of the safe shape: %q", m)
				}
			}
		})
	}
}

// TestSchemaAndCompileAgree checks the two parsers of a transform parameter:
// the published schema (through config.Parse) and the transform builders.
// Every shape the schema refuses, Compile refuses too; every value the schema
// accepts, Compile accepts unless goOnly names the bound only Go checks.
func TestSchemaAndCompileAgree(t *testing.T) {
	tests := []struct {
		pointType string
		name      string
		params    string
		schemaOK  bool
		goOnly    string
	}{
		{"float64", "scale", `0.1`, true, ""},
		{"float64", "scale", `"2"`, false, ""},
		{"float64", "scale", `true`, false, ""},
		{"float64", "scale", `{}`, false, ""},
		{"float64", "offset", `-40`, true, ""},
		{"float64", "offset", `null`, false, ""},
		{"float64", "round", `0`, true, ""},
		{"float64", "round", `10`, true, ""},
		{"float64", "round", `2.0`, true, ""},
		{"float64", "round", `11`, false, ""},
		{"float64", "round", `-1`, false, ""},
		{"float64", "round", `1.5`, false, ""},
		{"float64", "round", `"1"`, false, ""},
		{"int16", "bit", `0`, true, ""},
		{"int16", "bit", `63`, true, ""},
		{"int16", "bit", `64`, false, ""},
		{"int16", "bit", `-1`, false, ""},
		{"int16", "bcd", `true`, true, ""},
		{"int16", "bcd", `false`, false, ""},
		{"int16", "bcd", `1`, false, ""},
		{"int16", "map", `{"table": {"0": "idle", "1": "run"}}`, true, ""},
		{"int16", "map", `{"table": {"0": 1, "1": 2.5}, "default": 0}`, true, ""},
		{"int16", "map", `{"table": {}, "default": true}`, true, ""},
		{"int16", "map", `{"table": {"0": 18446744073709551615}}`, true, "an integer beyond int64"},
		{"int16", "map", `{"table": {}}`, false, ""},
		{"int16", "map", `{}`, false, ""},
		{"int16", "map", `{"table": {"0": 1, "1": "x"}}`, false, ""},
		{"int16", "map", `{"table": {"0": true}, "default": 1}`, false, ""},
		{"int16", "map", `{"table": {"0": null}}`, false, ""},
		{"int16", "map", `{"table": {"0": [1]}}`, false, ""},
		{"int16", "map", `{"table": [1]}`, false, ""},
		{"int16", "map", `{"table": {"0": 1}, "else": 2}`, false, ""},
		{"int16", "map", `"x"`, false, ""},
		{"float64", "valid_range", `{"min": 0}`, true, ""},
		{"float64", "valid_range", `{"max": 1.5}`, true, ""},
		{"float64", "valid_range", `{"min": -1, "max": 1}`, true, ""},
		{"float64", "valid_range", `{"min": 2, "max": 1}`, true, "min above max"},
		{"float64", "valid_range", `{}`, false, ""},
		{"float64", "valid_range", `{"min": "0"}`, false, ""},
		{"float64", "valid_range", `{"low": 0}`, false, ""},
		{"float64", "valid_range", `0`, false, ""},
		{"float64", "deadband", `0`, true, ""},
		{"float64", "deadband", `0.5`, true, ""},
		{"float64", "deadband", `"5%"`, true, ""},
		{"float64", "deadband", `"0.5%"`, true, ""},
		{"float64", "deadband", `"` + strings.Repeat("9", 400) + `%"`, true, "a percentage beyond float64"},
		{"float64", "deadband", `-1`, false, ""},
		{"float64", "deadband", `"5"`, false, ""},
		{"float64", "deadband", `"-5%"`, false, ""},
		{"float64", "deadband", `"5 %"`, false, ""},
		{"float64", "deadband", `true`, false, ""},
		{"float64", "heartbeat", `"1m"`, true, ""},
		{"float64", "heartbeat", `"1h30m0.5s"`, true, ""},
		{"float64", "heartbeat", `"0s"`, true, "a zero duration"},
		{"float64", "heartbeat", `"99999999999h"`, true, "a duration beyond time.Duration"},
		{"float64", "heartbeat", `30`, false, ""},
		{"float64", "heartbeat", `"1d"`, false, ""},
		{"float64", "min_interval", `"500ms"`, true, ""},
		{"float64", "min_interval", `"-1s"`, false, ""},
		{"float64", "debounce", `"2s"`, true, ""},
		{"float64", "debounce", `""`, false, ""},
		{"bool", "edge_counter", `"rising"`, true, ""},
		{"bool", "edge_counter", `"falling"`, true, ""},
		{"bool", "edge_counter", `"both"`, true, ""},
		{"bool", "edge_counter", `"up"`, false, ""},
		{"bool", "edge_counter", `true`, false, ""},
		{"float64", "derivative", `true`, true, ""},
		{"float64", "derivative", `"true"`, false, ""},
		{"string", "trim", `true`, true, ""},
		{"string", "trim", `false`, false, ""},
		{"string", "regex_extract", `{"pattern": "(\\d+)"}`, true, ""},
		{"string", "regex_extract", `{"pattern": "v=(\\d+)", "group": 1, "as": "integer"}`, true, ""},
		{"string", "regex_extract", `{"pattern": "[0-9.]+", "group": 0, "as": "float"}`, true, ""},
		{"string", "regex_extract", `{"pattern": "(a)(b)", "group": 2, "as": "string"}`, true, ""},
		{"string", "regex_extract", `{"pattern": "(a"}`, true, "an invalid RE2 expression"},
		{"string", "regex_extract", `{"pattern": "a"}`, true, "a group the pattern does not have"},
		{"string", "regex_extract", `{}`, false, ""},
		{"string", "regex_extract", `{"pattern": 5}`, false, ""},
		{"string", "regex_extract", `{"pattern": "(a)", "group": -1}`, false, ""},
		{"string", "regex_extract", `{"pattern": "(a)", "group": 2147483648}`, false, ""},
		{"string", "regex_extract", `{"pattern": "(a)", "group": 1.5}`, false, ""},
		{"string", "regex_extract", `{"pattern": "(a)", "as": "date"}`, false, ""},
		{"string", "regex_extract", `{"pattern": "(a)", "flags": "i"}`, false, ""},
		{"string", "regex_extract", `"(a)"`, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name+" "+tt.params, func(t *testing.T) {
			_, msgs, err := config.Parse([]byte(chainDocument("line_a", "press_07", "x", tt.pointType, `[{"`+tt.name+`": `+tt.params+`}]`)))
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			if schemaOK := len(msgs) == 0; schemaOK != tt.schemaOK {
				t.Fatalf("schema accepts = %v, want %v (messages %q)", schemaOK, tt.schemaOK, msgs)
			}

			steps := []config.Transform{{Name: tt.name, Params: json.RawMessage(tt.params)}}
			_, compileErr := transform.Compile(steps, transform.KindOf(tt.pointType), nil)

			wantCompile := tt.schemaOK && tt.goOnly == ""
			if compileOK := compileErr == nil; compileOK != wantCompile {
				t.Errorf("Compile accepts = %v, want %v (error %v)", compileOK, wantCompile, compileErr)
			}
		})
	}
}

func TestOutputDataType(t *testing.T) {
	step := func(name, params string) config.Transform {
		return config.Transform{Name: name, Params: json.RawMessage(params)}
	}

	tests := []struct {
		name  string
		point config.Point
		want  string
	}{
		{"no chain, bool", config.Point{Type: "bool"}, model.DataTypeBoolean},
		{"no chain, string", config.Point{Type: "string"}, model.DataTypeString},
		{"no chain, integer", config.Point{Type: "uint32"}, model.DataTypeNumber},
		{"no chain, no type", config.Point{}, model.DataTypeNumber},
		{"int16 then bit", config.Point{Type: "int16", Transforms: []config.Transform{step("bit", `0`)}}, model.DataTypeBoolean},
		{"float32 then round", config.Point{Type: "float32", Transforms: []config.Transform{step("round", `1`)}}, model.DataTypeNumber},
		{"string then regex_extract as integer", config.Point{Type: "string", Transforms: []config.Transform{step("regex_extract", `{"pattern": "(\\d+)", "as": "integer"}`)}}, model.DataTypeNumber},
		{"string then regex_extract as float", config.Point{Type: "string", Transforms: []config.Transform{step("regex_extract", `{"pattern": "(\\d+)", "as": "float"}`)}}, model.DataTypeNumber},
		{"string then trim", config.Point{Type: "string", Transforms: []config.Transform{step("trim", `true`)}}, model.DataTypeString},
		{"bool then edge_counter", config.Point{Type: "bool", Transforms: []config.Transform{step("edge_counter", `"rising"`)}}, model.DataTypeNumber},
		{"integer then map to text", config.Point{Type: "uint16", Transforms: []config.Transform{step("map", `{"table": {"1": "run"}, "default": "idle"}`)}}, model.DataTypeString},
		{"bool then map to numbers", config.Point{Type: "bool", Transforms: []config.Transform{step("map", `{"table": {"true": 1, "false": 0}}`)}}, model.DataTypeNumber},
		{"int16 then bit then heartbeat", config.Point{Type: "int16", Transforms: []config.Transform{step("bit", `5`), step("heartbeat", `"1m"`)}}, model.DataTypeBoolean},
		{"invalid chain keeps the point type", config.Point{Type: "float32", Transforms: []config.Transform{step("bit", `0`)}}, model.DataTypeNumber},
		{"invalid chain on a string keeps the point type", config.Point{Type: "string", Transforms: []config.Transform{step("trim", `true`), step("round", `1`)}}, model.DataTypeString},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transform.OutputDataType(tt.point); got != tt.want {
				t.Errorf("OutputDataType = %q, want %q", got, tt.want)
			}
		})
	}
}
