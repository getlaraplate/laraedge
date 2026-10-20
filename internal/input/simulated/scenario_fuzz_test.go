package simulated

import (
	"testing"

	"github.com/getlaraplate/laraplate-edge/internal/model"
)

// FuzzParseScenario checks that ParseScenario never panics, and that a
// scenario it accepts can be run: every sample it produces is valid.
func FuzzParseScenario(f *testing.F) {
	f.Add([]byte(validScenario))
	f.Add([]byte(certain))
	f.Add([]byte("initial: a\nstates: [{name: a, transitions: {b: 1}}, {name: b, producing: true}]\ncycle_time: {mean: 2s}\n"))
	f.Add([]byte("a: &x [*x, *x]\n"))
	f.Add([]byte(`{"initial":"a","states":[{"name":"a"}],"measurements":[{"signal":"t","mean":1e11,"stddev":1e11}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		sc, err := ParseScenario(data)
		if err != nil {
			return
		}

		sinks := map[string][]sink{}
		for address := range sc.offered {
			sinks[address] = []sink{{signal: address, priority: model.Critical}}
		}
		m := newMachine("dev", &sc, 1, sinks)
		ts := t0
		for range 50 {
			ts = ts.Add(sc.Tick)
			for _, s := range m.step(nil, ts) {
				if err := s.Validate(); err != nil {
					t.Fatalf("invalid sample %+v: %v", s, err)
				}
			}
		}
		if _, err := m.current(SignalState); err != nil {
			t.Fatal(err)
		}
	})
}
