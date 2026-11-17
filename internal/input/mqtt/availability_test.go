package mqtt_test

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/mqtt"
	"github.com/getlaraplate/laraedge/internal/model"
)

var start = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) emit(rec model.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if rec.Event != nil {
		r.events = append(r.events, rec.Event.Kind.String()+" "+rec.Event.Device)
	}
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.events...)
}

func lifecycleInput() config.Input {
	return config.Input{
		ID:             "line",
		Driver:         config.DriverSimulated,
		DeathTolerance: 30 * time.Second,
		Devices: []config.Device{
			{Device: "press-07", Points: []config.Point{{Signal: "state", Type: "string"}}},
			{Device: "oven-02", Points: []config.Point{{Signal: "temp", Type: "float32"}}},
			{Device: "saw-01", Points: []config.Point{{Signal: "rpm", Type: "float32"}}},
		},
	}
}

func fixture(t *testing.T) (*clock.Fake, *recorder, *mqtt.Tracker) {
	t.Helper()

	clk := clock.NewFake(start)
	rec := &recorder{}
	life := input.NewLifecycle(lifecycleInput(), clk, rec.emit)
	tracker := mqtt.NewTracker(life, map[string]mqtt.Availability{
		"press-07": {Topic: "plant/press-07/status", Online: "online", Offline: "offline"},
		"oven-02":  {Topic: "plant/oven-02/status", Online: "1", Offline: "0"},
	}, []string{"press-07", "oven-02", "saw-01"})

	return clk, rec, tracker
}

func TestTrackerMessage(t *testing.T) {
	tests := []struct {
		name        string
		topic       string
		payload     string
		wantHandled bool
		wantKind    string
		wantEvents  []string
	}{
		{"offline kills only its device", "plant/press-07/status", "offline", true, "", []string{"death press-07"}},
		{"online on a live device is silent", "plant/press-07/status", "online", true, "", nil},
		{"unknown payload", "plant/press-07/status", "maybe", true, "availability", nil},
		{"payload is compared exactly", "plant/press-07/status", " offline", true, "availability", nil},
		{"other payload strings of another device", "plant/oven-02/status", "offline", true, "availability", nil},
		{"offline string of another device", "plant/oven-02/status", "0", true, "", []string{"death oven-02"}},
		{"not an availability topic", "plant/press-07/data", "offline", false, "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rec, tracker := fixture(t)

			handled, kind := tracker.Message(tt.topic, []byte(tt.payload))
			if handled != tt.wantHandled || kind != tt.wantKind {
				t.Fatalf("Message = (%v, %q), want (%v, %q)", handled, kind, tt.wantHandled, tt.wantKind)
			}
			if got := rec.got(); !reflect.DeepEqual(got, tt.wantEvents) {
				t.Fatalf("events = %q, want %q", got, tt.wantEvents)
			}
		})
	}
}

func TestTrackerOfflineHoldsDeadUntilOnlineOrSample(t *testing.T) {
	tests := []struct {
		name    string
		revive  func(tr *mqtt.Tracker)
		revived bool
	}{
		{"keep alive does not revive", func(tr *mqtt.Tracker) { tr.KeepAlive() }, false},
		{"online revives", func(tr *mqtt.Tracker) { tr.Message("plant/press-07/status", []byte("online")) }, true},
		{"a sample revives", func(tr *mqtt.Tracker) { tr.Sample("press-07") }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rec, tracker := fixture(t)

			tracker.Message("plant/press-07/status", []byte("offline"))
			tt.revive(tracker)

			want := []string{"death press-07"}
			if tt.revived {
				want = append(want, "birth press-07")
			}
			if got := rec.got(); !reflect.DeepEqual(got, want) {
				t.Fatalf("events = %q, want %q", got, want)
			}
		})
	}
}

func TestTrackerHeldDeadSurvivesKeepAliveAfterRevival(t *testing.T) {
	_, rec, tracker := fixture(t)

	tracker.Message("plant/press-07/status", []byte("offline"))
	tracker.Sample("press-07")
	tracker.Message("plant/press-07/status", []byte("offline"))
	tracker.KeepAlive()
	tracker.Message("plant/press-07/status", []byte("online"))

	want := []string{"death press-07", "birth press-07", "death press-07", "birth press-07"}
	if got := rec.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestTrackerKeepAliveSpansTheTolerance(t *testing.T) {
	clk, rec, tracker := fixture(t)

	// Every device, with or without availability, stays alive across three
	// tolerances as long as KeepAlive runs every half tolerance, while the
	// held-dead device stays dead.
	tracker.Message("plant/press-07/status", []byte("offline"))
	for range 6 {
		clk.Advance(15 * time.Second)
		tracker.KeepAlive()
	}

	if got, want := rec.got(), []string{"death press-07"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestTrackerWithoutKeepAliveDevicesDieAtTheTolerance(t *testing.T) {
	clk := clock.NewFake(start)
	rec := &recorder{}
	life := input.NewLifecycle(lifecycleInput(), clk, rec.emit)
	mqtt.NewTracker(life, nil, []string{"press-07", "oven-02", "saw-01"})

	clk.Advance(30 * time.Second)
	life.Check()

	want := []string{"death oven-02", "death press-07", "death saw-01"}
	if got := rec.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestTrackerUnknownDeviceIsIgnored(t *testing.T) {
	_, rec, tracker := fixture(t)

	tracker.Sample("not-configured")
	if got := rec.got(); got != nil {
		t.Fatalf("events = %q, want none", got)
	}
}

func TestTrackerTopics(t *testing.T) {
	clk := clock.NewFake(start)
	life := input.NewLifecycle(lifecycleInput(), clk, func(model.Record) {})
	tracker := mqtt.NewTracker(life, map[string]mqtt.Availability{
		"press-07": {Topic: "plant/z", Online: "on", Offline: "off"},
		"oven-02":  {Topic: "plant/a", Online: "on", Offline: "off"},
		"saw-01":   {Topic: "plant/z", Online: "on", Offline: "off"},
	}, []string{"press-07", "oven-02", "saw-01"})

	if got, want := tracker.Topics(), []string{"plant/a", "plant/z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Topics = %q, want %q", got, want)
	}
}

func TestTrackerSharedTopicAppliesToEveryDevice(t *testing.T) {
	clk := clock.NewFake(start)
	rec := &recorder{}
	life := input.NewLifecycle(lifecycleInput(), clk, rec.emit)
	tracker := mqtt.NewTracker(life, map[string]mqtt.Availability{
		"press-07": {Topic: "plant/z", Online: "on", Offline: "off"},
		"saw-01":   {Topic: "plant/z", Online: "on", Offline: "off"},
	}, []string{"press-07", "oven-02", "saw-01"})

	tracker.Message("plant/z", []byte("off"))
	tracker.KeepAlive()
	tracker.Message("plant/z", []byte("on"))

	want := []string{"death press-07", "death saw-01", "birth press-07", "birth saw-01"}
	if got := rec.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestTrackerWithoutAvailability(t *testing.T) {
	clk := clock.NewFake(start)
	life := input.NewLifecycle(lifecycleInput(), clk, func(model.Record) {})
	tracker := mqtt.NewTracker(life, nil, []string{"press-07"})

	if got := tracker.Topics(); len(got) != 0 {
		t.Fatalf("Topics = %q, want none", got)
	}
	if handled, kind := tracker.Message("any", []byte("x")); handled || kind != "" {
		t.Fatalf("Message = (%v, %q), want (false, \"\")", handled, kind)
	}
}
