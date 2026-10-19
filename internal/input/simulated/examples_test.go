package simulated

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/model"
)

const examplesDir = "../../../examples/simulated"

func TestExampleConfigurationIsValidForThisBuild(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(examplesDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	doc, msgs, err := config.Parse(data)
	if err != nil || len(msgs) > 0 {
		t.Fatalf("Parse: err = %v, messages = %v", err, msgs)
	}
	if msgs := config.CheckBuild(doc); len(msgs) > 0 {
		t.Fatalf("CheckBuild: %v", msgs)
	}
	if doc.Version != 1 || len(doc.Inputs) != 1 || len(doc.Inputs[0].Devices) != 2 || len(doc.Outputs) != 1 {
		t.Fatalf("unexpected shape: version %d, %d inputs, %d outputs", doc.Version, len(doc.Inputs), len(doc.Outputs))
	}
	if doc.Outputs[0].Token != "secret://env/LARAPLATE_EDGE_TOKEN" || doc.Outputs[0].URL != "http://127.0.0.1:8000" {
		t.Fatalf("unexpected output: %+v", doc.Outputs[0])
	}
}

func TestExampleScenarioLoads(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(examplesDir, "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	sc, err := ParseScenario(data)
	if err != nil {
		t.Fatalf("ParseScenario: %v", err)
	}
	var names []string
	for _, s := range sc.States {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	if want := []string{"fault", "idle", "running", "setup"}; !slices.Equal(names, want) {
		t.Fatalf("states = %v, want %v", names, want)
	}
}

func TestExampleConfigurationRunsEndToEnd(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(examplesDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc, msgs, err := config.Parse(data)
	if err != nil || len(msgs) > 0 {
		t.Fatalf("Parse: err = %v, messages = %v", err, msgs)
	}

	clk := clock.NewFake(t0)
	in, err := Factory(examplesDir)(doc.Inputs[0], clk)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	c := drive(t, in, clk, 3600, time.Second)

	signals := map[string]bool{}
	states := map[string]bool{}
	for _, rec := range c.records() {
		signals[rec.Sample.Device+"/"+rec.Sample.Signal] = true
		if rec.Sample.Signal == "state" {
			states[rec.Sample.Value.Str] = true
		}
		if err := rec.Validate(); err != nil {
			t.Fatalf("invalid record: %v", err)
		}
		if rec.Sample.Quality != model.Good {
			t.Fatalf("quality %v", rec.Sample.Quality)
		}
	}
	for _, dev := range doc.Inputs[0].Devices {
		for _, p := range dev.Points {
			if !signals[dev.Device+"/"+p.Signal] {
				t.Errorf("%s/%s never emitted in an hour", dev.Device, p.Signal)
			}
		}
	}
	if !states["running"] || !states["idle"] {
		t.Errorf("states seen in an hour: %v", states)
	}
}
