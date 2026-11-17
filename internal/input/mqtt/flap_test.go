package mqtt

import (
	"log/slog"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
)

func TestFlapDetector(t *testing.T) {
	short, long := flapWindow-time.Millisecond, flapWindow

	tests := []struct {
		name  string
		lasts []time.Duration
		warns []bool
	}{
		{"three short losses warn once", []time.Duration{short, short, short, short, short}, []bool{false, false, true, false, false}},
		{"a long connection resets the streak", []time.Duration{short, short, long, short, short, short}, []bool{false, false, false, false, false, true}},
		{"warn again only after a reset", []time.Duration{short, short, short, long, short, short, short}, []bool{false, false, true, false, false, false, true}},
		{"a zero length connection is short", []time.Duration{0, 0, 0}, []bool{false, false, true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f flapDetector
			for i, d := range tt.lasts {
				if got := f.lost(d); got != tt.warns[i] {
					t.Errorf("loss %d after %v: warn = %v, want %v", i+1, d, got, tt.warns[i])
				}
			}
		})
	}

	if flapWindow != 5*time.Second || flapCount != 3 {
		t.Errorf("flapWindow, flapCount = %v, %d, want 5s and 3", flapWindow, flapCount)
	}
}

// TestInputWarnsOfAConnectionTakenOverAtOnce runs two inputs with one client
// id on one broker: each connection takes the other's over at once, so the
// first input warns, once, after its third loss of a connection that lasted
// no time on its clock.
func TestInputWarnsOfAConnectionTakenOverAtOnce(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	cfg := mqttInput(t, "line", b, map[string]any{"client_id": "clone"}, device("press", subscribePoint("count", "plant/press/count", "int32")))

	var logs logBuffer
	first := newHarness(t, cfg, Options{Logger: logs.logger()})
	first.start()
	first.serving()

	second := newHarness(t, cfg, Options{})
	second.start()
	second.serving()

	for loss := 1; loss <= flapCount+1; loss++ {
		first.waitEvent(traceDown)
		warned := logs.lines(t, slog.LevelWarn, msgClientIDTakenOver)
		want := 0
		if loss >= flapCount {
			want = 1
		}
		if len(warned) != want {
			t.Fatalf("after loss %d: %d warnings, want %d", loss, len(warned), want)
		}
		if want == 1 && (warned[0]["client_id"] != "clone" || warned[0]["input"] != "line") {
			t.Errorf("warning = %v, want the client id and the input", warned[0])
		}
		if loss > flapCount {
			break
		}

		first.advanceToBackoffEnd()
		first.waitEvent(traceServing)
		second.waitEvent(traceDown)
		second.advanceToBackoffEnd()
		second.waitEvent(traceServing)
	}
}
