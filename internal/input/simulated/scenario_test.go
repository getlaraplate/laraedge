package simulated

import (
	"strings"
	"testing"
	"time"
)

const validScenario = `{"tick":"1s","initial":"idle","states":[
	{"name":"idle","transitions":{"running":0.3,"setup":0.05}},
	{"name":"running","producing":true,"transitions":{"idle":0.02,"fault":0.01}},
	{"name":"setup","transitions":{"idle":0.2}},
	{"name":"fault","transitions":{"idle":0.1}}],
	"cycle_time":{"mean":"5s","stddev":"1s"},"scrap_rate":0.2,
	"measurements":[{"signal":"temperature","mean":80,"stddev":2,"unit":"C"},{"signal":"pressure","mean":150,"stddev":4}]}`

func TestParseScenarioAcceptsYAMLAndJSON(t *testing.T) {
	yamlDoc := `
tick: 500ms
initial: idle
states:
  - name: idle
    transitions: {running: 0.5}
  - name: running
    producing: true
cycle_time: {mean: 2s, stddev: 0s}
scrap_rate: 0.1
measurements:
  - {signal: temperature, mean: 80, stddev: 2, unit: C}
`
	for name, doc := range map[string]string{"json": validScenario, "yaml": yamlDoc} {
		t.Run(name, func(t *testing.T) {
			sc, err := ParseScenario([]byte(doc))
			if err != nil {
				t.Fatalf("ParseScenario: %v", err)
			}
			if sc.Initial != "idle" || len(sc.States) < 2 || len(sc.Measurements) == 0 {
				t.Fatalf("scenario not decoded: %+v", sc)
			}
		})
	}

	sc, err := ParseScenario([]byte(yamlDoc))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Tick != 500*time.Millisecond {
		t.Fatalf("Tick = %v, want 500ms", sc.Tick)
	}
}

func TestParseScenarioDefaultsTickToOneSecond(t *testing.T) {
	sc, err := ParseScenario([]byte(`{"initial":"idle","states":[{"name":"idle"}]}`))
	if err != nil {
		t.Fatalf("ParseScenario: %v", err)
	}
	if sc.Tick != time.Second {
		t.Fatalf("Tick = %v, want 1s", sc.Tick)
	}
}

func TestParseScenarioRefusesWithTheFieldNamed(t *testing.T) {
	idle := `{"name":"idle"}`
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"empty", ``, "empty"},
		{"unknown key", `{"initial":"idle","states":[` + idle + `],"colour":"red"}`, "unknown key colour"},
		{"second document", "initial: idle\nstates: [{name: idle}]\n---\ninitial: idle\n", "exactly one"},
		{"negative tick", `{"tick":"-1s","initial":"idle","states":[` + idle + `]}`, "tick"},
		{"tick too small", `{"tick":"1us","initial":"idle","states":[` + idle + `]}`, "tick"},
		{"tick too large", `{"tick":"2h","initial":"idle","states":[` + idle + `]}`, "tick"},
		{"no states", `{"initial":"idle","states":[]}`, "states"},
		{"unnamed state", `{"initial":"idle","states":[{"name":""}]}`, "states[0].name"},
		{"duplicate state", `{"initial":"idle","states":[` + idle + `,` + idle + `]}`, "states[1].name"},
		{"no initial", `{"states":[` + idle + `]}`, "initial"},
		{"unknown initial", `{"initial":"gone","states":[` + idle + `]}`, "initial"},
		{"unknown target", `{"initial":"idle","states":[{"name":"idle","transitions":{"gone":0.5}}]}`, "states[0].transitions"},
		{"self transition", `{"initial":"idle","states":[{"name":"idle","transitions":{"idle":0.5}}]}`, "states[0].transitions"},
		{"probability above one", `{"initial":"idle","states":[{"name":"idle","transitions":{"b":1.5}},{"name":"b"}]}`, "states[0].transitions"},
		{"negative probability", `{"initial":"idle","states":[{"name":"idle","transitions":{"b":-0.1}},{"name":"b"}]}`, "states[0].transitions"},
		{"probabilities above one", `{"initial":"idle","states":[{"name":"idle","transitions":{"b":0.7,"c":0.7}},{"name":"b"},{"name":"c"}]}`, "states[0].transitions"},
		{"producing without cycle time", `{"initial":"idle","states":[{"name":"idle","producing":true}]}`, "cycle_time.mean"},
		{"cycle shorter than tick", `{"tick":"1s","initial":"idle","states":[{"name":"idle","producing":true}],"cycle_time":{"mean":"500ms"}}`, "cycle_time.mean"},
		{"negative stddev", `{"initial":"idle","states":[{"name":"idle","producing":true}],"cycle_time":{"mean":"5s","stddev":"-1s"}}`, "cycle_time.stddev"},
		{"scrap above one", `{"initial":"idle","states":[` + idle + `],"scrap_rate":1.5}`, "scrap_rate"},
		{"scrap not a number", "initial: idle\nstates: [{name: idle}]\nscrap_rate: .nan\n", "scrap_rate"},
		{"unnamed measurement", `{"initial":"idle","states":[` + idle + `],"measurements":[{"signal":"","mean":1}]}`, "measurements[0].signal"},
		{"reserved measurement", `{"initial":"idle","states":[` + idle + `],"measurements":[{"signal":"good_count","mean":1}]}`, "measurements[0].signal"},
		{"duplicate measurement", `{"initial":"idle","states":[` + idle + `],"measurements":[{"signal":"t","mean":1},{"signal":"t","mean":2}]}`, "measurements[1].signal"},
		{"negative measurement stddev", `{"initial":"idle","states":[` + idle + `],"measurements":[{"signal":"t","mean":1,"stddev":-1}]}`, "measurements[0].stddev"},
		{"huge measurement mean", `{"initial":"idle","states":[` + idle + `],"measurements":[{"signal":"t","mean":1e300}]}`, "measurements[0].mean"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseScenario([]byte(tt.doc))
			if err == nil {
				t.Fatal("ParseScenario accepted the document")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

func TestParseScenarioErrorsNeverEchoDocumentValues(t *testing.T) {
	const secret = "hunter2-not-a-duration"
	docs := []string{
		`{"tick":"` + secret + `","initial":"idle","states":[{"name":"idle"}]}`,
		`{"initial":"idle","states":[{"name":"idle"}],"` + secret + `":1}`,
		`{"initial":"idle","states":[{"name":"idle","transitions":{"` + secret + `":0.5}}]}`,
		`{"initial":"idle","states":[{"name":"idle","producing":"` + secret + `"}]}`,
		`{"initial":"` + secret + `","states":[{"name":"idle"}]}`,
		`{"initial":"idle","states":[{"name":"idle"}],"measurements":[{"signal":"` + secret + `","mean":"x"}]}`,
	}

	for i, doc := range docs {
		_, err := ParseScenario([]byte(doc))
		if err == nil {
			t.Fatalf("document %d accepted", i)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("document %d: error %q echoes a document value", i, err)
		}
	}
}
