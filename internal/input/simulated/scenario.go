package simulated

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/getlaraplate/laraplate-edge/internal/docrules"
)

// Signals every scenario offers, besides its measurements.
const (
	// SignalState is the current state name, a string.
	SignalState = "state"
	// SignalGoodCount is the cumulative number of good parts, an integer.
	SignalGoodCount = "good_count"
	// SignalScrapCount is the cumulative number of scrap parts, an integer.
	SignalScrapCount = "scrap_count"
)

// Bounds of a scenario.
const (
	// DefaultTick is the simulation step when the scenario sets none.
	DefaultTick = time.Second
	// MinTick and MaxTick bound the simulation step.
	MinTick = time.Millisecond
	MaxTick = time.Hour

	// maxCycleMean bounds the mean cycle time.
	maxCycleMean = 24 * time.Hour
	// maxMagnitude bounds the mean and standard deviation of a measurement,
	// so that no drawn value can overflow.
	maxMagnitude = 1e12
	// maxScenarioSize bounds a scenario file or inline scenario, in bytes.
	maxScenarioSize = 1 << 20
	// probabilityTolerance absorbs the rounding of probabilities that are
	// meant to sum to one.
	probabilityTolerance = 1e-9
)

// Scenario describes how one simulated machine behaves. Every device of an
// input runs its own instance of it. Time advances in steps of Tick:
//
//   - at each step the machine may leave its state, with the probability its
//     state gives for each other state per step (the remainder stays);
//   - while in a producing state a part completes when the cycle time drawn
//     for it has elapsed; the shortest cycle is one tick, so a step completes
//     at most one part. Any change of state abandons the part in
//     progress, and entering a producing state starts a new part;
//   - a completed part is scrap with probability ScrapRate; and every
//     measurement is drawn once per completed part.
type Scenario struct {
	// Tick is the simulation step, DefaultTick when not set.
	Tick time.Duration `yaml:"tick"`
	// Initial names the state the machine starts in.
	Initial string `yaml:"initial"`
	// States are the states of the machine.
	States []State `yaml:"states"`
	// CycleTime is the time to make one part, normally distributed. It is
	// required when a state produces.
	CycleTime CycleTime `yaml:"cycle_time"`
	// ScrapRate is the probability, from 0 to 1, that a part is scrap.
	ScrapRate float64 `yaml:"scrap_rate"`
	// Measurements are the process values drawn per completed part.
	Measurements []Measurement `yaml:"measurements"`

	// Compiled by validation, read only afterwards.
	stateIndex map[string]int
	transOf    [][]transition
	offered    map[string]signalRef
}

// State is one state of the machine.
type State struct {
	// Name is the value of the state signal while in this state.
	Name string `yaml:"name"`
	// Producing says that parts are made in this state.
	Producing bool `yaml:"producing"`
	// Transitions maps another state to the probability, per step, of going
	// there. The probabilities sum to at most 1.
	Transitions map[string]float64 `yaml:"transitions"`
}

// CycleTime is a normal distribution of durations.
type CycleTime struct {
	Mean   time.Duration `yaml:"mean"`
	StdDev time.Duration `yaml:"stddev"`
}

// Measurement is a process value drawn from a normal distribution once per
// completed part. Unit is informative: the unit a point reports is its own.
type Measurement struct {
	Signal string  `yaml:"signal"`
	Mean   float64 `yaml:"mean"`
	StdDev float64 `yaml:"stddev"`
	Unit   string  `yaml:"unit"`
}

type transition struct {
	to int
	p  float64
}

type signalKind uint8

const (
	kindState signalKind = iota + 1
	kindGood
	kindScrap
	kindMeasurement
)

// signalRef says what a signal address of the scenario stands for.
type signalRef struct {
	kind  signalKind
	index int
}

// ParseScenario decodes and validates a scenario, YAML or JSON. Unknown keys
// are an error. No error repeats a value of the document, so a secret written
// in the wrong place does not reach a log; the errors name the field instead.
func ParseScenario(data []byte) (Scenario, error) {
	if len(data) > maxScenarioSize {
		return Scenario{}, errors.New("larger than 1 MiB")
	}

	var sc Scenario
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&sc); err != nil {
		if errors.Is(err, io.EOF) {
			return Scenario{}, errors.New("the scenario is empty")
		}

		return Scenario{}, errors.New(docrules.DescribeYAMLError(err))
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return Scenario{}, errors.New("the scenario must hold exactly one YAML document")
	}

	if problems := sc.compile(); len(problems) > 0 {
		return Scenario{}, errors.New(strings.Join(problems, "; "))
	}

	return sc, nil
}

// compile applies the defaults, validates the scenario and builds the lookup
// tables. It returns one message per problem.
func (sc *Scenario) compile() []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	switch {
	case sc.Tick == 0:
		sc.Tick = DefaultTick
	case sc.Tick < MinTick || sc.Tick > MaxTick:
		add("tick: must be between %s and %s", MinTick, MaxTick)
	}

	producing := false
	sc.stateIndex = make(map[string]int, len(sc.States))
	if len(sc.States) == 0 {
		add("states: at least one state is required")
	}
	for i, st := range sc.States {
		if st.Name == "" {
			add("states[%d].name: required", i)

			continue
		}
		if _, dup := sc.stateIndex[st.Name]; dup {
			add("states[%d].name: duplicate state name", i)

			continue
		}
		sc.stateIndex[st.Name] = i
		producing = producing || st.Producing
	}

	if _, ok := sc.stateIndex[sc.Initial]; !ok {
		add("initial: must name one of the states")
	}

	sc.transOf = make([][]transition, len(sc.States))
	for i, st := range sc.States {
		trans, msgs := sc.compileTransitions(i, st)
		sc.transOf[i] = trans
		problems = append(problems, msgs...)
	}

	if producing {
		if sc.CycleTime.Mean < sc.Tick || sc.CycleTime.Mean > maxCycleMean {
			add("cycle_time.mean: required when a state produces, between the tick (%s) and %s", sc.Tick, maxCycleMean)
		}
	}
	if sc.CycleTime.StdDev < 0 || sc.CycleTime.StdDev > maxCycleMean {
		add("cycle_time.stddev: must be between 0 and %s", maxCycleMean)
	}

	if !(sc.ScrapRate >= 0 && sc.ScrapRate <= 1) {
		add("scrap_rate: must be a number from 0 to 1")
	}

	sc.offered = map[string]signalRef{
		SignalState:      {kind: kindState},
		SignalGoodCount:  {kind: kindGood},
		SignalScrapCount: {kind: kindScrap},
	}
	for i, m := range sc.Measurements {
		switch {
		case m.Signal == "":
			add("measurements[%d].signal: required", i)
		case sc.offered[m.Signal].kind != 0:
			add("measurements[%d].signal: duplicate, or one of state, good_count and scrap_count", i)
		default:
			sc.offered[m.Signal] = signalRef{kind: kindMeasurement, index: i}
		}
		if !(math.Abs(m.Mean) <= maxMagnitude) {
			add("measurements[%d].mean: must be finite and at most %g in magnitude", i, maxMagnitude)
		}
		if !(m.StdDev >= 0 && m.StdDev <= maxMagnitude) {
			add("measurements[%d].stddev: must be between 0 and %g", i, float64(maxMagnitude))
		}
	}

	return problems
}

// compileTransitions validates the transitions of state i and returns them
// sorted by target name, so that the draw does not depend on the map order.
func (sc *Scenario) compileTransitions(i int, st State) ([]transition, []string) {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("states[%d].transitions: ", i)+fmt.Sprintf(format, args...))
	}

	names := make([]string, 0, len(st.Transitions))
	for name := range st.Transitions {
		names = append(names, name)
	}
	slices.Sort(names)

	var trans []transition
	sum := 0.0
	for _, name := range names {
		p := st.Transitions[name]
		to, known := sc.stateIndex[name]
		switch {
		case !known:
			add("%s is not a state", quoteIfSafe(name))
		case to == i:
			add("a state does not move to itself; the remainder of the probability stays")
		case !(p >= 0 && p <= 1):
			add("the probability of %s must be from 0 to 1", quoteIfSafe(name))
		default:
			trans = append(trans, transition{to: to, p: p})
			sum += p
		}
	}
	if sum > 1+probabilityTolerance {
		add("the probabilities sum to more than 1")
	}

	return trans, problems
}

// quoteIfSafe returns name for use in a message when it has the shape of a
// key, and a placeholder otherwise: a name that does not could be a secret.
func quoteIfSafe(name string) string {
	if docrules.IsSafeKey(name) {
		return `"` + name + `"`
	}

	return `"*"`
}
