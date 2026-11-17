package mqtt

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

// twoNodeDevices are press on the node g/n1 and saw on the node g/n2.
func twoNodeDevices() []config.Device {
	return []config.Device{
		device("press", subscribePoint("temp", "g/n1|Temp", "float64")),
		device("saw", subscribePoint("temp", "g/n2|Temp", "float64")),
	}
}

type probed struct {
	samples []model.Sample
	kinds   map[PointRef]string
	err     error
}

// startProbe runs ProbePoints on its own goroutine and returns when the
// broker has processed its subscription: the broker double does not deliver
// a retained message to a node-level filter ending in /#, so the tests
// publish after the subscription, as a live node does.
func startProbe(t *testing.T, b *mqtttest.Broker, in *Input, ctx context.Context) <-chan probed {
	t.Helper()

	done := make(chan probed, 1)
	go func() {
		s, k, err := in.ProbePoints(ctx)
		done <- probed{s, k, err}
	}()
	b.WaitSubscribed(in.clientID+"-probe", 1)

	return done
}

func (p probed) check(t *testing.T) probed {
	t.Helper()

	if p.err != nil {
		t.Fatalf("ProbePoints: %v", p.err)
	}

	return p
}

func TestProbeSparkplugReadsABirthAndANDATAByName(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	in, _, _ := newCommandInput(t, sparkplugInput(t, "line", b, nil, sparkplugDevices()...), Options{})
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()

	done := startProbe(t, b, in, ctx)
	b.Publish(tNBIRTH, payload(ptr(uint64(0)), mqtttest.BdSeq(1), temp(1, 20.5)), false, 1)
	b.Publish(tDBIRTH, payload(ptr(uint64(1)), speed(2, 7)), false, 1)
	r := (<-done).check(t)
	if ctx.Err() != nil {
		t.Fatal("ProbePoints waited for the context instead of returning at the last sample")
	}
	if len(r.samples) != 2 || r.samples[0].Signal != "temp" || r.samples[0].Value.Float != 20.5 || r.samples[1].Signal != "speed" || r.samples[1].Value.Int != 7 {
		t.Fatalf("samples = %+v, want temp 20.5 then speed 7", r.samples)
	}
	if len(r.kinds) != 0 {
		t.Errorf("kinds = %v, want none", r.kinds)
	}

	// An NDATA by name is read without any birth.
	c := mqtttest.Start(t, mqtttest.Options{})
	in2, _, _ := newCommandInput(t, sparkplugInput(t, "line", c, nil, sparkplugDevices()[0]), Options{})
	done = startProbe(t, c, in2, ctx)
	c.Publish(tNDATA, payload(nil, mqtttest.SparkplugMetric{Name: ptr("Temp"), Datatype: ptr(dtDouble), Double: ptr(3.5)}), false, 1)
	if r = (<-done).check(t); len(r.samples) != 1 || r.samples[0].Value.Float != 3.5 {
		t.Errorf("samples = %+v, want 3.5", r.samples)
	}
}

// A retained message on a device topic reaches the probe at once; the
// broker double matches retained messages of a node topic only when they
// are published after the subscription, which the tests above do.
func TestProbeSparkplugReadsARetainedDeviceMessage(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	b.Publish(tDDATA, payload(nil, mqtttest.SparkplugMetric{Name: ptr("Speed"), Datatype: ptr(dtInt32), Int: ptr(uint32(9))}), true, 1)

	in, _, _ := newCommandInput(t, sparkplugInput(t, "line", b, nil, sparkplugDevices()[1]), Options{})
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()

	samples, _, err := in.ProbePoints(ctx)
	if err != nil || len(samples) != 1 || samples[0].Value.Int != 9 || ctx.Err() != nil {
		t.Fatalf("ProbePoints = %+v, %v", samples, err)
	}
}

func TestProbeSparkplugSaysUnknownAliasForANodeThatPublishesAliasesOnly(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	in, _, events := newCommandInput(t, sparkplugInput(t, "line", b, nil, sparkplugDevices()...), Options{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := startProbe(t, b, in, ctx)
	b.Publish(tNDATA, payload(nil, byAlias(1, 5)), false, 1)
	waitFor(t, events, traceProbeMessage, 1)
	cancel()

	r := (<-done).check(t)
	if len(r.samples) != 0 {
		t.Fatalf("samples = %+v, want none", r.samples)
	}
	if got := r.kinds[PointRef{"press", "temp"}]; got != KindUnknownAlias {
		t.Errorf("kind of press/temp = %q, want %q", got, KindUnknownAlias)
	}
	if got, ok := r.kinds[PointRef{"oven", "speed"}]; ok {
		t.Errorf("kind of oven/speed = %q, want none: nothing arrived for it", got)
	}
}

func TestProbeSparkplugSaysMalformedTooLargeAndRefused(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{Refuse: []string{"spBv1.0/g/+/n3/#"}})
	big := payload(nil, temp(1, 1), temp(1, 2))

	devices := []config.Device{
		device("a", subscribePoint("v", "g/n1|Temp", "float64")),
		device("b", subscribePoint("v", "g/n2|Temp", "float64")),
		device("c", subscribePoint("v", "g/n3|Temp", "float64")),
	}
	in, _, events := newCommandInput(t, sparkplugInput(t, "line", b, map[string]any{"max_payload": len(big) - 1}, devices...), Options{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := startProbe(t, b, in, ctx)
	b.Publish("spBv1.0/g/NDATA/n1", []byte{0xff, 0xff}, false, 1)
	b.Publish("spBv1.0/g/NDATA/n2", big, false, 1)
	waitFor(t, events, traceProbeMessage, 2)
	cancel()

	kinds := (<-done).check(t).kinds
	want := map[PointRef]string{
		{"a", "v"}: KindMalformedPayload,
		{"b", "v"}: KindPayloadTooLarge,
		{"c", "v"}: KindSubscribeRefused,
	}
	for k, w := range want {
		if kinds[k] != w {
			t.Errorf("kind of %v = %q, want %q", k, kinds[k], w)
		}
	}
}

func TestProbeSparkplugRebirthPublishesOneNCMDPerNodeAndReadsTheBirths(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	in, _, _ := newCommandInput(t, sparkplugInput(t, "line", b, map[string]any{"rebirth": true}, twoNodeDevices()...), Options{})
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()

	done := startProbe(t, b, in, ctx)

	got := b.WaitPublished(in.clientID+"-probe", 2)
	topics := []string{got[0].Topic, got[1].Topic}
	slices.Sort(topics)
	if !slices.Equal(topics, []string{"spBv1.0/g/NCMD/n1", "spBv1.0/g/NCMD/n2"}) {
		t.Fatalf("published = %v, want one NCMD per node", topics)
	}
	for _, m := range got {
		if m.QoS != 0 || m.Retain {
			t.Errorf("NCMD %q: QoS %d retain %v, want QoS 0 not retained", m.Topic, m.QoS, m.Retain)
		}
		p, err := decodePayload(m.Payload)
		if err != nil || len(p.Metrics) != 1 || p.Metrics[0].Name != metricRebirth || !p.Metrics[0].Value.Bool {
			t.Errorf("NCMD payload = %+v, %v, want Node Control/Rebirth = true", p, err)
		}
	}

	// The births the nodes answer with end the probe.
	b.Publish("spBv1.0/g/NBIRTH/n1", payload(ptr(uint64(0)), mqtttest.BdSeq(1), temp(1, 1.5)), false, 1)
	b.Publish("spBv1.0/g/NBIRTH/n2", payload(ptr(uint64(0)), mqtttest.BdSeq(1), temp(1, 2.5)), false, 1)
	r := (<-done).check(t)
	if len(r.samples) != 2 || r.samples[0].Value.Float != 1.5 || r.samples[1].Value.Float != 2.5 {
		t.Fatalf("samples = %+v, want the two births", r.samples)
	}
	if n := len(b.Published(in.clientID + "-probe")); n != 2 {
		t.Errorf("published %d messages, want the two NCMD only", n)
	}
}

func TestProbeSparkplugWithoutRebirthPublishesNothing(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	in, _, _ := newCommandInput(t, sparkplugInput(t, "line", b, nil, twoNodeDevices()...), Options{})
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()

	done := startProbe(t, b, in, ctx)
	b.Publish("spBv1.0/g/NBIRTH/n1", payload(ptr(uint64(0)), mqtttest.BdSeq(1), temp(1, 1.5)), false, 1)
	b.Publish("spBv1.0/g/NBIRTH/n2", payload(ptr(uint64(0)), mqtttest.BdSeq(1), temp(1, 2.5)), false, 1)
	if r := (<-done).check(t); len(r.samples) != 2 {
		t.Fatalf("samples = %+v, want two", r.samples)
	}
	if got := b.Published(in.clientID + "-probe"); len(got) != 0 {
		t.Errorf("published = %+v, want nothing with rebirth off", got)
	}
}

// browseSparkplug runs BrowseSparkplug on its own goroutine, waits for n
// messages, then ends the window on the fake clock.
func browseSparkplug(t *testing.T, in *Input, filter string, n int, events <-chan string, advance func()) ([]SparkplugEntry, bool) {
	t.Helper()

	type result struct {
		entries   []SparkplugEntry
		truncated bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		e, tr, err := in.BrowseSparkplug(t.Context(), filter, time.Second)
		done <- result{e, tr, err}
	}()
	waitFor(t, events, traceBrowseMessage, n)
	advance()
	r := <-done
	if r.err != nil {
		t.Fatalf("BrowseSparkplug: %v", r.err)
	}

	return r.entries, r.truncated
}

func publishBrowseFixture(b *mqtttest.Broker) {
	dataset := mqtttest.SparkplugMetric{Name: ptr("Tab"), Datatype: ptr(uint32(16))}
	b.Publish("spBv1.0/g/NBIRTH/n1", payload(ptr(uint64(0)), mqtttest.BdSeq(1), temp(1, 20.5), dataset), true, 1)
	b.Publish("spBv1.0/g/DBIRTH/n1/d1", payload(ptr(uint64(1)), speed(2, 7)), true, 1)
	b.Publish("spBv1.0/g/NDATA/n2", []byte{0xff, 0xff}, true, 1)
}

func TestBrowseSparkplugListsEntriesAndMetrics(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	publishBrowseFixture(b)

	in, clk, events := newCommandInput(t, sparkplugInput(t, "line", b, nil, sparkplugDevices()...), Options{})
	entries, truncated := browseSparkplug(t, in, DefaultSparkplugBrowseFilter, 3, events, func() {
		clk.BlockUntil(t, 1)
		clk.Advance(time.Second)
	})

	want := []SparkplugEntry{
		{Group: "g", Node: "n1", Messages: 1, Born: true, Metrics: []SparkplugMetricEntry{
			{"g/n1|Tab", "unsupported:DataSet"}, {"g/n1|Temp", "float64"}, {"g/n1|bdSeq", "int64"},
		}},
		{Group: "g", Node: "n1", Device: "d1", Messages: 1, Born: true, Metrics: []SparkplugMetricEntry{{"g/n1/d1|Speed", "int32"}}},
		{Group: "g", Node: "n2", Messages: 1, Malformed: 1},
	}
	if truncated || len(entries) != len(want) {
		t.Fatalf("entries = %+v, truncated %v", entries, truncated)
	}
	for i := range want {
		got := entries[i]
		if got.Group != want[i].Group || got.Node != want[i].Node || got.Device != want[i].Device || got.Messages != want[i].Messages ||
			got.Born != want[i].Born || got.Malformed != want[i].Malformed || !slices.Equal(got.Metrics, want[i].Metrics) {
			t.Errorf("entry %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestBrowseSparkplugRebirthPublishesOneNCMDPerNodeSeen(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	publishBrowseFixture(b)

	in, clk, events := newCommandInput(t, sparkplugInput(t, "line", b, map[string]any{"rebirth": true}, sparkplugDevices()...), Options{})
	id := in.clientID + "-browse"
	browseSparkplug(t, in, DefaultSparkplugBrowseFilter, 3, events, func() {
		b.WaitPublished(id, 2)
		clk.BlockUntil(t, 1)
		clk.Advance(time.Second)
	})

	got := b.Published(id)
	topics := make([]string, len(got))
	for i, m := range got {
		topics[i] = m.Topic
		if m.QoS != 0 || m.Retain {
			t.Errorf("NCMD %q: QoS %d retain %v", m.Topic, m.QoS, m.Retain)
		}
	}
	slices.Sort(topics)
	if !slices.Equal(topics, []string{"spBv1.0/g/NCMD/n1", "spBv1.0/g/NCMD/n2"}) {
		t.Errorf("published = %v, want one NCMD per node (n1 has two messages)", topics)
	}
}

func TestBrowseSparkplugWithoutRebirthPublishesNothing(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	publishBrowseFixture(b)

	in, clk, events := newCommandInput(t, sparkplugInput(t, "line", b, nil, sparkplugDevices()...), Options{})
	browseSparkplug(t, in, DefaultSparkplugBrowseFilter, 3, events, func() {
		clk.BlockUntil(t, 1)
		clk.Advance(time.Second)
	})

	if got := b.Published(in.clientID + "-browse"); len(got) != 0 {
		t.Errorf("published = %+v, want nothing with rebirth off", got)
	}
}

func TestBrowseSparkplugRefusesBadArguments(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	in, _, _ := newCommandInput(t, sparkplugInput(t, "line", b, nil, sparkplugDevices()...), Options{})

	for _, tt := range []struct {
		filter   string
		duration time.Duration
	}{{"", time.Second}, {"a/#/b", time.Second}, {"#", 999 * time.Millisecond}, {"#", 6 * time.Minute}} {
		if _, _, err := in.BrowseSparkplug(t.Context(), tt.filter, tt.duration); err == nil {
			t.Errorf("BrowseSparkplug(%q, %v) succeeded", tt.filter, tt.duration)
		}
	}
	if DefaultSparkplugBrowseFilter != "spBv1.0/#" {
		t.Errorf("DefaultSparkplugBrowseFilter = %q", DefaultSparkplugBrowseFilter)
	}
}
