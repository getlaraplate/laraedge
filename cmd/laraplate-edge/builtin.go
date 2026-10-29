package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/getlaraplate/laraedge-sdk/manifest"
	"github.com/getlaraplate/laraedge-sdk/sdk"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/simulated"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/version"
)

// builtinPrefix starts the executable base name under which the agent serves
// one of its built-in drivers over the plugin protocol (decision H8).
const builtinPrefix = "laraedge-builtin-"

// builtinDriver returns the driver an executable named exe serves, and
// whether exe is a busybox name at all: the base name, without a ".exe", is
// laraedge-builtin-<driver>.
func builtinDriver(exe string) (driver string, ok bool) {
	base := filepath.Base(exe)
	if strings.HasSuffix(strings.ToLower(base), ".exe") {
		base = base[:len(base)-len(".exe")]
	}

	return strings.CutPrefix(base, builtinPrefix)
}

// serveBuiltin serves the built-in driver of the busybox name exe over the
// plugin protocol and exits; it returns only the exit code of a name it
// cannot serve. Only simulated is served: modbus and opcua need servers to
// talk to (spec 7).
func serveBuiltin(exe, driver string, stderr io.Writer) int {
	if driver != config.DriverSimulated {
		fmt.Fprintf(stderr, "%s: the built-in driver %q cannot be served as a plugin (only simulated)\n", filepath.Base(exe), driver)

		return exitUsage
	}

	sdk.Serve(simulatedPlugin(clock.Real()))

	return exitOK
}

// simulatedPlugin is the simulated driver as an SDK plugin. Its name is the
// busybox name and its version the agent's.
func simulatedPlugin(clk clock.Clock) sdk.Plugin {
	return sdk.Plugin{
		Name:     builtinPrefix + config.DriverSimulated,
		Version:  version.String(),
		Input:    newSimulatedService(clk),
		Manifest: manifest.Kinds{Input: &simulatedKind},
	}
}

// simulatedKind is the input section of the manifest. It accepts what the
// agent's own configuration schema accepts of a simulated input: a
// connection with exactly one of scenario and scenario_file and an optional
// integer seed, any address, every read mode and every point type. The
// scenario itself and the points against it are checked by Configure.
var simulatedKind = manifest.InputKind{
	ConnectionSchema: json.RawMessage(`{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"scenario": {"type": "object"},
			"scenario_file": {"type": "string", "minLength": 1},
			"seed": {"type": "integer"}
		},
		"oneOf": [{"required": ["scenario"]}, {"required": ["scenario_file"]}]
	}`),
	AddressSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string","minLength":1}`),
	ReadModes:     []string{"poll", "subscribe", "event"},
	Types:         []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64", "float32", "float64", "string"},
}

// simulatedService serves the simulated driver through sdk.Input. The driver
// runs one input per Start, so every Run and Probe builds a fresh one from
// the configuration of the instance and closes it when done.
type simulatedService struct {
	factory input.Factory
	clk     clock.Clock

	mu        sync.Mutex
	instances map[string]config.Input
}

var _ sdk.Input = (*simulatedService)(nil)

func newSimulatedService(clk clock.Clock) *simulatedService {
	return &simulatedService{
		// A relative scenario_file has no base directory in a plugin: the
		// agent sends the section as written.
		factory:   simulated.Factory(""),
		clk:       clk,
		instances: map[string]config.Input{},
	}
}

// Configure checks the section like the agent does for a simulated input:
// the connection, the scenario and every point against it.
func (s *simulatedService) Configure(_ context.Context, instance string, raw json.RawMessage) []sdk.ConfigError {
	section, err := sdk.DecodeInput(raw)
	if err != nil {
		var ce sdk.ConfigError
		if !errors.As(err, &ce) {
			ce = sdk.ConfigError{Message: "the section cannot be decoded"}
		}

		return []sdk.ConfigError{ce}
	}

	cfg, problem := inputFromSection(section)
	if problem != nil {
		return []sdk.ConfigError{*problem}
	}

	in, err := s.factory(cfg, s.clk)
	if err != nil {
		return []sdk.ConfigError{{Path: "connection", Message: err.Error()}}
	}
	_ = in.Close()

	s.mu.Lock()
	s.instances[instance] = cfg
	s.mu.Unlock()

	return nil
}

// inputFromSection converts the section the SDK decoded into the input the
// simulated driver takes. Only what the driver reads is converted.
func inputFromSection(section sdk.InputConfig) (config.Input, *sdk.ConfigError) {
	cfg := config.Input{ID: section.ID, Driver: config.DriverSimulated, Connection: section.Connection}
	for i, d := range section.Devices {
		device := config.Device{Device: d.Device}
		for j, p := range d.Points {
			priority := model.Critical
			switch p.Priority {
			case "", "critical":
			case "bulk":
				priority = model.Bulk
			default:
				return config.Input{}, &sdk.ConfigError{Path: fmt.Sprintf("devices[%d].points[%d].priority", i, j), Message: "must be critical or bulk"}
			}
			device.Points = append(device.Points, config.Point{
				Signal: p.Signal, Address: p.Address, Type: p.Type, Unit: p.Unit, Priority: priority,
			})
		}
		cfg.Devices = append(cfg.Devices, device)
	}

	return cfg, nil
}

// open builds a fresh driver input for a configured instance.
func (s *simulatedService) open(instance string) (input.Input, error) {
	s.mu.Lock()
	cfg, ok := s.instances[instance]
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("instance is not configured")
	}

	return s.factory(cfg, s.clk)
}

// Probe reads every point once, without starting the simulation.
func (s *simulatedService) Probe(ctx context.Context, instance string) ([]sdk.Sample, error) {
	in, err := s.open(instance)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()

	samples, err := in.Probe(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]sdk.Sample, 0, len(samples))
	for _, m := range samples {
		sample, err := sdkSample(m)
		if err != nil {
			return nil, err
		}
		out = append(out, sample)
	}

	return out, nil
}

// Run starts the simulation and forwards its samples until ctx is cancelled.
// Every sample is followed by the answer of its device.
func (s *simulatedService) Run(ctx context.Context, instance string, out sdk.InputEmitter) error {
	in, err := s.open(instance)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		once      sync.Once
		emitError error
	)
	fail := func(err error) {
		once.Do(func() { emitError = err })
		cancel()
	}

	err = in.Start(ctx, func(r model.Record) {
		if r.Sample == nil {
			return
		}
		sample, err := sdkSample(*r.Sample)
		if err == nil {
			err = out.Sample(sample)
		}
		if err == nil {
			err = out.Answered(sample.Device)
		}
		if err != nil && ctx.Err() == nil {
			fail(err)
		}
	})
	if err != nil {
		return err
	}

	<-ctx.Done()
	// Close waits for the simulation goroutine; after it emitError is final.
	_ = in.Close()

	return emitError
}

// Stop has nothing to release: Run and Probe close what they open.
func (s *simulatedService) Stop(context.Context, string) error { return nil }

// sdkSample converts a driver sample to the SDK's.
func sdkSample(m model.Sample) (sdk.Sample, error) {
	var (
		value sdk.Value
		err   error
	)
	switch m.Value.Kind {
	case model.KindFloat:
		value, err = sdk.FloatValue(m.Value.Float)
	case model.KindInt:
		value = sdk.IntValue(m.Value.Int)
	case model.KindBool:
		value = sdk.BoolValue(m.Value.Bool)
	case model.KindString:
		value = sdk.StringValue(m.Value.Str)
	default:
		err = errors.New("unknown value kind")
	}
	if err != nil {
		return sdk.Sample{}, fmt.Errorf("sample of %s/%s: %w", m.Device, m.Signal, err)
	}

	return sdk.Sample{
		Device: m.Device, Signal: m.Signal, TS: m.TS, Value: value,
		Quality: sdk.Quality(m.Quality), Priority: sdk.Priority(m.Priority),
	}, nil
}
