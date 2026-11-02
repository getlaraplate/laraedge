package config

import (
	"slices"
	"testing"
)

const observeDoc = `schema: laraplate-edge-config/1
version: 3
inputs:
  - id: sim-line
    driver: simulated
    connection: {scenario: s.json}
    devices:
      - device: d1
        points: [{signal: state, address: state, read: subscribe}]
outputs:
  - id: main
    type: laraplate-http
    url: https://erp.example.com
    token: secret://env/TOKEN
processors:
  - id: p
    plugin: oee
    observe: [d1, d1, d1]
    devices: [{device: line-1, signals: [{signal: x, type: bool}]}]
plugins:
  - {id: oee, path: /a, sha256: x, manifest_sha256: m}
`

// TestObserveDuplicatesEachLayerOnItsOwn: the schema and the semantic rule
// each refuse a device observed twice, in the same words, and Parse merges
// them into one message per duplicate entry.
func TestObserveDuplicatesEachLayerOnItsOwn(t *testing.T) {
	tree, msgs := decodeTree([]byte(observeDoc))
	if len(msgs) > 0 {
		t.Fatal(msgs)
	}

	schemaMsgs, err := validateSchema(tree)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"processors.0.observe.1: duplicate of processors.0.observe.0"}; !slices.Equal(schemaMsgs, want) {
		t.Errorf("schema = %q, want %q (the schema reports the first pair only)", schemaMsgs, want)
	}

	applyDefaults(tree.(map[string]any))
	_, semantic, err := build(tree.(map[string]any), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"processors.0.observe.1: duplicate of processors.0.observe.0",
		"processors.0.observe.2: duplicate of processors.0.observe.0",
	}
	if !slices.Equal(semantic, want) {
		t.Errorf("semantic = %q, want %q", semantic, want)
	}

	_, merged, err := Parse([]byte(observeDoc))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(merged, want) {
		t.Errorf("Parse = %q, want one message per duplicate entry %q", merged, want)
	}
}
