// Package simulated is the simulated input driver: a deterministic machine
// simulator for demos, development and end-to-end tests (spec 9.2). Each
// device runs its own instance of a scenario: a state machine with transition
// probabilities, a cycle time, a scrap rate and measurements drawn from normal
// distributions. With the same seed and the same clock steps the emitted
// sequence is identical across runs of one build. The generator is PCG, whose
// stream is fixed, and the normal draws are computed here from it rather than
// by the library; floating point arithmetic may still differ in the last bit
// between processor architectures.
//
// The connection of the input is:
//
//	scenario:      an inline scenario object, or
//	scenario_file: the path of a YAML or JSON scenario file, absolute or
//	               relative to the directory of the configuration file;
//	               exactly one of the two
//	seed:          an integer, 1 when not set
//
// A point addresses a scenario signal by name: state, good_count,
// scrap_count or a measurement signal.
package simulated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/docrules"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/model"
)

// DefaultSeed is the seed when the connection sets none.
const DefaultSeed int64 = 1

// maxCatchUpTicks bounds how many steps are replayed after the clock jumped
// forward or the input was starved; beyond it the simulation resumes from the
// present instead of replaying a long past.
const maxCatchUpTicks = 10_000

// Connection keys.
const (
	keyScenario     = "scenario"
	keyScenarioFile = "scenario_file"
	keySeed         = "seed"
)

// connection is the decoded connection of a simulated input.
type connection struct {
	scenario     json.RawMessage
	scenarioFile string
	seed         int64
}

// parseConnection decodes the connection of cfg. The messages name the
// field, never its value.
func parseConnection(raw json.RawMessage) (connection, error) {
	conn := connection{seed: DefaultSeed}

	var fields map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return connection{}, errors.New("connection: must be an object")
		}
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		switch key {
		case keyScenario, keyScenarioFile, keySeed:
		default:
			if docrules.IsSafeKey(key) {
				return connection{}, fmt.Errorf("connection: unknown key %s (want scenario, scenario_file or seed)", key)
			}

			return connection{}, errors.New("connection: unknown key (want scenario, scenario_file or seed)")
		}
	}

	if v, ok := fields[keyScenario]; ok && string(v) != "null" {
		conn.scenario = v
	}
	if v, ok := fields[keyScenarioFile]; ok {
		if err := json.Unmarshal(v, &conn.scenarioFile); err != nil || conn.scenarioFile == "" {
			return connection{}, errors.New("connection.scenario_file: must be a non-empty path")
		}
	}
	if (conn.scenario == nil) == (conn.scenarioFile == "") {
		return connection{}, errors.New("connection: exactly one of scenario and scenario_file is required")
	}

	if v, ok := fields[keySeed]; ok {
		seed, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return connection{}, errors.New("connection.seed: must be an integer")
		}
		conn.seed = seed
	}

	return conn, nil
}

// Factory returns the factory of simulated inputs. baseDir is the directory
// a relative scenario_file is resolved against, the configuration file's
// directory; empty, a relative scenario_file is refused. The factory reads
// the seed from the connection. It reads the scenario file when it can; a
// file it cannot read makes Start fail instead, since a local resource error
// is Start's to report.
func Factory(baseDir string) input.Factory {
	return func(cfg config.Input, clk clock.Clock) (input.Input, error) {
		conn, err := parseConnection(cfg.Connection)
		if err != nil {
			return nil, fmt.Errorf("simulated input %q: %w", cfg.ID, err)
		}

		in, err := newInput(cfg, clk, conn, conn.seed, baseDir)
		if err != nil {
			return nil, err
		}

		return in, nil
	}
}

// New returns the simulated input of cfg with the given seed. A relative
// scenario_file is refused: use Factory to give a base directory. The seed in
// the connection, if any, is ignored in favour of seed.
func New(cfg config.Input, clk clock.Clock, seed int64) (*Input, error) {
	conn, err := parseConnection(cfg.Connection)
	if err != nil {
		return nil, fmt.Errorf("simulated input %q: %w", cfg.ID, err)
	}

	return newInput(cfg, clk, conn, seed, "")
}

func newInput(cfg config.Input, clk clock.Clock, conn connection, seed int64, baseDir string) (*Input, error) {
	in := &Input{cfg: cfg, clk: clk, conn: conn, seed: seed, baseDir: baseDir}

	if err := in.prepareLocked(); err != nil {
		var local *localError
		if !errors.As(err, &local) {
			return nil, err
		}
		// An unreadable file is a local resource error: Start reports it.
	}

	return in, nil
}

// localError marks a failure to read the scenario file.
type localError struct{ err error }

func (e *localError) Error() string { return e.err.Error() }
func (e *localError) Unwrap() error { return e.err }

// Input is one simulated input: one machine per device.
type Input struct {
	cfg     config.Input
	clk     clock.Clock
	conn    connection
	seed    int64
	baseDir string

	// mu guards everything below and the state of the machines.
	mu       sync.Mutex
	prepared bool
	machines []*machine
	byName   map[string]*machine
	started  bool
	closed   bool
	cancel   context.CancelFunc
	tick     time.Duration

	wg sync.WaitGroup
}

// prepareLocked loads the scenario, checks every point against it and builds
// the machines, once. It is called before the input is shared, or with mu
// held.
func (in *Input) prepareLocked() error {
	if in.prepared {
		return nil
	}

	sc, err := in.loadScenario()
	if err != nil {
		return err
	}

	sinks, err := in.sinks(&sc)
	if err != nil {
		return err
	}

	in.machines = in.machines[:0]
	in.byName = make(map[string]*machine, len(in.cfg.Devices))
	for _, d := range in.cfg.Devices {
		if in.byName[d.Device] != nil {
			continue
		}
		m := newMachine(d.Device, &sc, in.seed, sinks[d.Device])
		in.machines = append(in.machines, m)
		in.byName[d.Device] = m
	}
	in.tick = sc.Tick
	in.prepared = true

	return nil
}

// loadScenario returns the scenario of the connection.
func (in *Input) loadScenario() (Scenario, error) {
	if in.conn.scenario != nil {
		sc, err := ParseScenario(in.conn.scenario)
		if err != nil {
			return Scenario{}, fmt.Errorf("simulated input %q: connection.scenario: %w", in.cfg.ID, err)
		}

		return sc, nil
	}

	path := in.conn.scenarioFile
	if !filepath.IsAbs(path) {
		if in.baseDir == "" {
			return Scenario{}, fmt.Errorf("simulated input %q: connection.scenario_file: a relative path needs the directory of the configuration file as its base directory, which this configuration source does not have; use an absolute path", in.cfg.ID)
		}
		path = filepath.Join(in.baseDir, path)
	}

	data, err := readLimited(path)
	if err != nil {
		return Scenario{}, &localError{fmt.Errorf("simulated input %q: connection.scenario_file: %w", in.cfg.ID, err)}
	}
	sc, err := ParseScenario(data)
	if err != nil {
		return Scenario{}, fmt.Errorf("simulated input %q: scenario file %s: %w", in.cfg.ID, path, err)
	}

	return sc, nil
}

// readLimited reads a file of at most maxScenarioSize bytes.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxScenarioSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxScenarioSize {
		return nil, fmt.Errorf("%s is larger than 1 MiB", path)
	}

	return data, nil
}

// sink is a point that receives the values of a scenario signal.
type sink struct {
	signal   string
	priority model.Priority
}

// sinks checks that every point addresses a signal the scenario offers, with
// a type that fits it, and returns the points by device and address.
func (in *Input) sinks(sc *Scenario) (map[string]map[string][]sink, error) {
	out := make(map[string]map[string][]sink, len(in.cfg.Devices))
	for _, d := range in.cfg.Devices {
		for _, p := range d.Points {
			ref, ok := sc.offered[p.Address]
			if !ok {
				return nil, fmt.Errorf("simulated input %q: device %q: point %q: the scenario offers no signal at this address (want state, good_count, scrap_count or a measurement)", in.cfg.ID, d.Device, p.Signal)
			}

			dataType := p.DataType()
			switch {
			case ref.kind == kindState && dataType != model.DataTypeString:
				return nil, fmt.Errorf("simulated input %q: device %q: point %q: the state signal is a string, so the point type must be string", in.cfg.ID, d.Device, p.Signal)
			case ref.kind != kindState && dataType != model.DataTypeNumber:
				return nil, fmt.Errorf("simulated input %q: device %q: point %q: a counter or measurement is a number, so the point type must be numeric", in.cfg.ID, d.Device, p.Signal)
			}

			priority := p.Priority
			if !priority.Valid() {
				priority = model.Critical
			}
			if out[d.Device] == nil {
				out[d.Device] = map[string][]sink{}
			}
			out[d.Device][p.Address] = append(out[d.Device][p.Address], sink{signal: p.Signal, priority: priority})
		}
	}

	return out, nil
}

// Start starts the simulation: the initial state and the counters at 0 are
// emitted at once, then the scenario advances by one tick each time the clock
// passes one. It fails only on a scenario it cannot read or use. The goroutine stops when ctx is
// done or on Close.
func (in *Input) Start(ctx context.Context, emit func(model.Record)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("simulated input %q: start: %w", in.cfg.ID, err)
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	switch {
	case in.closed:
		return fmt.Errorf("simulated input %q: start after close", in.cfg.ID)
	case in.started:
		return fmt.Errorf("simulated input %q: already started", in.cfg.ID)
	}
	if err := in.prepareLocked(); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	in.cancel = cancel
	in.started = true
	start := in.clk.Now()
	in.wg.Go(func() { in.run(runCtx, emit, start) })

	return nil
}

// Close stops the simulation and waits for its goroutine. It is safe on an
// input never started and when called twice.
func (in *Input) Close() error {
	in.mu.Lock()
	in.closed = true
	cancel := in.cancel
	in.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	in.wg.Wait()

	return nil
}

// Probe returns one sample per point with the value the machine has now:
// the state, the counters and, for a measurement, the last value drawn (its
// mean before the first part). Nothing is emitted.
func (in *Input) Probe(ctx context.Context) ([]model.Sample, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("simulated input %q: probe: %w", in.cfg.ID, err)
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	if err := in.prepareLocked(); err != nil {
		return nil, err
	}

	now := in.clk.Now().UTC()
	var out []model.Sample
	for _, d := range in.cfg.Devices {
		m := in.byName[d.Device]
		for _, p := range d.Points {
			priority := p.Priority
			if !priority.Valid() {
				priority = model.Critical
			}
			value, err := m.current(p.Address)
			if err != nil {
				return nil, fmt.Errorf("simulated input %q: device %q: point %q: %w", in.cfg.ID, d.Device, p.Signal, err)
			}
			out = append(out, model.Sample{Device: d.Device, Signal: p.Signal, TS: now, Value: value, Quality: model.Good, Priority: priority})
		}
	}

	return out, nil
}

// run emits the initial state of every machine, then steps them every tick
// until ctx is done. The steps are timestamped with their scheduled instant,
// so a late timer changes when a step runs, never what it emits.
func (in *Input) run(ctx context.Context, emit func(model.Record), start time.Time) {
	in.emitAll(emit, in.initialSamples(start))

	next := start.Add(in.tick)
	for {
		timer := in.clk.NewTimer(max(next.Sub(in.clk.Now()), 0))
		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C():
		}

		now := in.clk.Now()
		if now.Sub(next) > maxCatchUpTicks*in.tick {
			next = now
		}
		for !next.After(now) {
			if ctx.Err() != nil {
				return
			}
			in.emitAll(emit, in.stepSamples(next))
			next = next.Add(in.tick)
		}
	}
}

func (in *Input) initialSamples(ts time.Time) []model.Sample {
	in.mu.Lock()
	defer in.mu.Unlock()

	var out []model.Sample
	for _, m := range in.machines {
		out = m.initial(out, ts.UTC())
	}

	return out
}

func (in *Input) stepSamples(ts time.Time) []model.Sample {
	in.mu.Lock()
	defer in.mu.Unlock()

	var out []model.Sample
	for _, m := range in.machines {
		out = m.step(out, ts.UTC())
	}

	return out
}

// emitAll emits the samples outside the lock: emit may block.
func (in *Input) emitAll(emit func(model.Record), samples []model.Sample) {
	for _, s := range samples {
		emit(model.SampleRecord(s))
	}
}

// machine is the simulation of one device.
type machine struct {
	name  string
	sc    *Scenario
	rng   *rand.Rand
	sinks map[string][]sink

	state   int
	good    int64
	scrap   int64
	elapsed time.Duration // time spent on the part in progress
	cycle   time.Duration // cycle time drawn for the part in progress
	last    []float64     // last value of each measurement
}

// newMachine returns the machine of the device name. Its generator depends on
// the seed and the name only, so that a device behaves the same whatever
// other devices are configured.
func newMachine(name string, sc *Scenario, seed int64, sinks map[string][]sink) *machine {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))

	m := &machine{
		name:  name,
		sc:    sc,
		rng:   rand.New(rand.NewPCG(uint64(seed), h.Sum64())),
		sinks: sinks,
		state: sc.stateIndex[sc.Initial],
		last:  make([]float64, len(sc.Measurements)),
	}
	for i, ms := range sc.Measurements {
		m.last[i] = ms.Mean
	}
	if sc.States[m.state].Producing {
		m.cycle = m.drawCycle()
	}

	return m
}

// uniform returns a number in [0, 1).
func (m *machine) uniform() float64 {
	return float64(m.rng.Uint64()>>11) / (1 << 53)
}

// normal returns a draw from the normal distribution (Box-Muller). It always
// consumes two uniforms, so the stream does not depend on the parameters.
func (m *machine) normal(mean, stddev float64) float64 {
	u1 := 1 - m.uniform() // (0, 1]
	u2 := m.uniform()
	z := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)

	return mean + stddev*z
}

// drawCycle returns a cycle time, never shorter than one tick.
func (m *machine) drawCycle() time.Duration {
	seconds := m.normal(m.sc.CycleTime.Mean.Seconds(), m.sc.CycleTime.StdDev.Seconds())

	return max(time.Duration(seconds*float64(time.Second)), m.sc.Tick)
}

// initial appends the state the machine starts in and the counters at 0. The
// backend takes the first sample of a cumulative counter as a baseline and
// counts from the second, so without the 0 the first part would be lost.
func (m *machine) initial(out []model.Sample, ts time.Time) []model.Sample {
	out = m.add(out, SignalState, model.StringValue(m.sc.States[m.state].Name), ts)
	out = m.add(out, SignalGoodCount, model.IntValue(m.good), ts)

	return m.add(out, SignalScrapCount, model.IntValue(m.scrap), ts)
}

// step advances the machine by one tick, ending at ts, and appends what
// changed. The draws are in a fixed order: the part, if one completes (scrap,
// then the measurements in scenario order, then the next cycle), then the
// transition.
func (m *machine) step(out []model.Sample, ts time.Time) []model.Sample {
	sc := m.sc
	if sc.States[m.state].Producing {
		m.elapsed += sc.Tick
		if m.elapsed >= m.cycle {
			m.elapsed -= m.cycle
			m.cycle = m.drawCycle()
			out = m.completePart(out, ts)
		}
	}

	u := m.uniform()
	acc := 0.0
	for _, tr := range sc.transOf[m.state] {
		acc += tr.p
		if u < acc {
			out = m.moveTo(out, tr.to, ts)

			break
		}
	}

	return out
}

func (m *machine) completePart(out []model.Sample, ts time.Time) []model.Sample {
	if m.uniform() < m.sc.ScrapRate {
		m.scrap++
		out = m.add(out, SignalScrapCount, model.IntValue(m.scrap), ts)
	} else {
		m.good++
		out = m.add(out, SignalGoodCount, model.IntValue(m.good), ts)
	}

	for i, ms := range m.sc.Measurements {
		m.last[i] = m.normal(ms.Mean, ms.StdDev)
		if v, err := model.FloatValue(m.last[i]); err == nil {
			out = m.add(out, ms.Signal, v, ts)
		}
	}

	return out
}

func (m *machine) moveTo(out []model.Sample, to int, ts time.Time) []model.Sample {
	m.state = to

	// Any change of state abandons the part in progress; a producing state
	// starts a new one.
	m.elapsed = 0
	if m.sc.States[to].Producing {
		m.cycle = m.drawCycle()
	}

	return m.add(out, SignalState, model.StringValue(m.sc.States[to].Name), ts)
}

// add appends one sample per point addressing address.
func (m *machine) add(out []model.Sample, address string, v model.Value, ts time.Time) []model.Sample {
	for _, s := range m.sinks[address] {
		out = append(out, model.Sample{Device: m.name, Signal: s.signal, TS: ts, Value: v, Quality: model.Good, Priority: s.priority})
	}

	return out
}

// current returns the value of the signal at address now.
func (m *machine) current(address string) (model.Value, error) {
	ref, ok := m.sc.offered[address]
	if !ok {
		return model.Value{}, errors.New("the scenario offers no signal at this address")
	}

	switch ref.kind {
	case kindState:
		return model.StringValue(m.sc.States[m.state].Name), nil
	case kindGood:
		return model.IntValue(m.good), nil
	case kindScrap:
		return model.IntValue(m.scrap), nil
	default:
		return model.FloatValue(m.last[ref.index])
	}
}
