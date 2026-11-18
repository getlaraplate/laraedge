package mqtt

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

func TestDecodePointKinds(t *testing.T) {
	plan := &TopicPlan{Topic: "t", Points: []PlannedPoint{
		pt("good", "t|a", "int32", model.Critical),
		pt("conv", "t|b", "int16", model.Critical),
		pt("missing", "t|c", "int32", model.Critical),
	}}
	_, _, kinds := decode(plan, []byte(`{"a": 1, "b": 70000}`), receivedAt)
	if want := []string{"", KindConversion, NoValueAtPath}; !equalStrings(kinds, want) {
		t.Errorf("kinds = %q, want %q", kinds, want)
	}

	_, _, kinds = decode(plan, []byte(`{"a": `), receivedAt)
	if want := []string{KindMalformedJSON, KindMalformedJSON, KindMalformedJSON}; !equalStrings(kinds, want) {
		t.Errorf("kinds of a malformed payload = %q, want %q", kinds, want)
	}

	stamped := &TopicPlan{Topic: "t", Timestamp: &TimestampSpec{Path: "ts", Format: TimestampUnixSeconds}, Points: []PlannedPoint{
		pt("plain", "t", "string", model.Critical),
		pt("path", "t|a", "int32", model.Critical),
	}}
	_, _, kinds = decode(stamped, []byte(`{"a": 1}`), receivedAt)
	if want := []string{KindBadTimestamp, KindBadTimestamp}; !equalStrings(kinds, want) {
		t.Errorf("kinds without a timestamp = %q, want %q", kinds, want)
	}
	_, _, kinds = decode(stamped, []byte(`not json`), receivedAt)
	if want := []string{KindBadTimestamp, KindMalformedJSON}; !equalStrings(kinds, want) {
		t.Errorf("kinds of a payload that is not JSON = %q, want %q", kinds, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// TestProbePointsReportsWhyAPointHasNoGoodSample: every point whose message
// arrived and did not decode carries the kind the running input would count,
// a missing path says so, and the points of a refused topic are
// subscribe_refused.
func TestProbePointsReportsWhyAPointHasNoGoodSample(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{Refuse: []string{"plant/vault"}})
	b.Publish("plant/press/temp", []byte("21.5"), true, 1)
	b.Publish("plant/press/state", []byte(`{"count": 70000}`), true, 1)
	b.Publish("plant/press/raw", []byte(`{"mode": `), true, 1)
	b.Publish("plant/press/big", []byte(`"0123456789012345678901234567890123456789"`), true, 1)

	cfg := mqttInput(t, "line", b, map[string]any{"max_payload": 32}, device("press",
		subscribePoint("temp", "plant/press/temp", "float64"),
		subscribePoint("count", "plant/press/state|count", "int16"),
		subscribePoint("gone", "plant/press/state|gone", "int16"),
		subscribePoint("mode", "plant/press/raw|mode", "string"),
		subscribePoint("big", "plant/press/big", "string"),
		subscribePoint("open", "plant/vault", "bool"),
	))
	in, _, events := newCommandInput(t, cfg, Options{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type result struct {
		samples []model.Sample
		kinds   map[PointRef]string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		samples, kinds, err := in.ProbePoints(ctx)
		done <- result{samples, kinds, err}
	}()

	// The four retained messages, then the probe waits for gone: end it.
	waitFor(t, events, traceProbeMessage, 4)
	cancel()

	var r result
	select {
	case r = <-done:
	case <-time.After(waitTimeout):
		t.Fatal("ProbePoints did not return")
	}
	if r.err != nil {
		t.Fatalf("ProbePoints: %v", r.err)
	}

	want := map[PointRef]string{
		{Device: "press", Signal: "count"}: KindConversion,
		{Device: "press", Signal: "gone"}:  NoValueAtPath,
		{Device: "press", Signal: "mode"}:  KindMalformedJSON,
		{Device: "press", Signal: "big"}:   KindPayloadTooLarge,
		{Device: "press", Signal: "open"}:  KindSubscribeRefused,
	}
	if !maps.Equal(r.kinds, want) {
		t.Errorf("kinds = %v, want %v", r.kinds, want)
	}

	var signals []string
	for _, s := range r.samples {
		signals = append(signals, s.Signal+"/"+s.Quality.String())
	}
	if want := []string{"temp/good", "count/bad", "mode/bad"}; !equalStrings(signals, want) {
		t.Errorf("samples = %q, want %q", signals, want)
	}
}

// TestProbeEndsWhenEveryReachablePointHasASample: the points of a refused
// topic will never have a message, so they do not hold the probe until the
// context ends.
func TestProbeEndsWhenEveryReachablePointHasASample(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{Refuse: []string{"plant/vault"}})
	b.Publish("plant/press/temp", []byte("21.5"), true, 1)

	cfg := mqttInput(t, "line", b, nil, device("press",
		subscribePoint("temp", "plant/press/temp", "float64"),
		subscribePoint("open", "plant/vault", "bool"),
	))
	in, _, _ := newCommandInput(t, cfg, Options{})

	ctx := testContext(t)
	samples, kinds, err := in.ProbePoints(ctx)
	if err != nil || len(samples) != 1 || kinds[PointRef{Device: "press", Signal: "open"}] != KindSubscribeRefused {
		t.Fatalf("ProbePoints = %v, %v, %v", samples, kinds, err)
	}
	if ctx.Err() != nil {
		t.Error("ProbePoints waited for the context to end")
	}
}

func TestDescribePayloadCountsTheLeavesBeyondTheCap(t *testing.T) {
	items := make([]string, browseMaxPaths+5)
	for i := range items {
		items[i] = "1"
	}
	payload := []byte(`{"a": [` + strings.Join(items, ",") + `], "b": {"c": 2}, "d.e": 3}`)

	paths, more, text := describePayload("t", payload)
	if len(paths) != browseMaxPaths || more != 6 || text != "" {
		t.Fatalf("describePayload = %d paths, %d more, text %q, want %d and 6", len(paths), more, text, browseMaxPaths)
	}

	paths, more, _ = describePayload("t", []byte(`{"a": 1, "d.e": 3}`))
	if len(paths) != 1 || more != 0 {
		t.Errorf("describePayload = %d paths, %d more, want 1 and 0 (a dotted key is not counted)", len(paths), more)
	}
}
