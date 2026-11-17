package mqtt

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

// startSparkplug starts the Sparkplug input of the test devices on b with
// the connection fields conn and waits until it serves.
func startSparkplug(t *testing.T, b *mqtttest.Broker, conn map[string]any) *harness {
	t.Helper()

	h := newHarness(t, sparkplugInput(t, "line", b, conn, sparkplugDevices()...), Options{DataDir: t.TempDir(), AgentID: "agent"})
	h.start()
	h.serving()

	return h
}

// eventIndex is the position of the n-th (from 1) event of kind for device
// in rs, -1 when there is none.
func eventIndex(rs []model.Record, kind model.EventKind, device string, n int) int {
	for i, r := range rs {
		if r.Event != nil && r.Event.Kind == kind && r.Event.Device == device {
			n--
			if n == 0 {
				return i
			}
		}
	}

	return -1
}

// sampleIndex is the position of the n-th (from 1) sample of signal in rs,
// -1 when there is none.
func sampleIndex(rs []model.Record, signal string, n int) int {
	for i, r := range rs {
		if r.Sample != nil && r.Sample.Signal == signal {
			n--
			if n == 0 {
				return i
			}
		}
	}

	return -1
}

func TestSparkplugInputBirthsDataAndDeaths(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	h := startSparkplug(t, b, nil)

	birthTS := driverEpoch.Add(-time.Minute)
	dataTS := driverEpoch.Add(-30 * time.Second)

	b.Publish(tNBIRTH, mqtttest.SparkplugPayload{
		Timestamp: ptr(uint64(birthTS.UnixMilli())),
		Seq:       ptr(uint64(0)),
		Metrics:   []mqtttest.SparkplugMetric{mqtttest.BdSeq(1), temp(1, 20.5)},
	}.Encode(), false, 1)
	b.Publish(tDBIRTH, mqtttest.SparkplugPayload{
		Timestamp: ptr(uint64(birthTS.UnixMilli())),
		Seq:       ptr(uint64(1)),
		Metrics:   []mqtttest.SparkplugMetric{speed(2, 7)},
	}.Encode(), false, 1)
	b.Publish(tNDATA, payload(ptr(uint64(2)), stamped(byAlias(1, 21.5), dataTS)), false, 1)
	b.Publish(tDDATA, payload(ptr(uint64(3)), mqtttest.SparkplugMetric{Alias: ptr(uint64(2)), Int: ptr(uint32(8))}), false, 1)

	temps := h.waitSamples("temp", 2)
	speeds := h.waitSamples("speed", 2)
	if temps[0] != tempSample(20.5, birthTS) || temps[1] != tempSample(21.5, dataTS) {
		t.Errorf("temp samples = %+v, want 20.5 at the birth and 21.5 at the metric timestamp", temps)
	}
	if speeds[0].Value != model.IntValue(7) || !speeds[0].TS.Equal(birthTS) || speeds[1].Value != model.IntValue(8) || speeds[1].Device != "oven" {
		t.Errorf("speed samples = %+v, want 7 then 8 as int32 of the oven", speeds)
	}
	if rs := h.snapshot(); countEvents(rs, model.Death, "press")+countEvents(rs, model.Birth, "press") != 0 {
		t.Errorf("events of a live node: %s", describe(rs))
	}

	// NDEATH kills the node and every device under it; a data message does
	// not revive them, only a birth does, its birth event before its sample.
	b.Publish(tNDEATH, ndeath(ptr(uint64(1))), false, 1)
	h.waitEvents(model.Death, 1, "press", "oven")
	b.Publish(tNDATA, payload(nil, byAlias(1, 99)), false, 1)
	b.Publish(tNBIRTH, payload(nil, mqtttest.BdSeq(2), temp(1, 30)), false, 1)
	h.waitSamples("temp", 3)

	rs := h.snapshot()
	if got := samplesOf(rs, "temp"); got[2].Value.Float != 30 {
		t.Errorf("third temp sample = %+v, want 30 (the sample of a dead node dropped)", got[2])
	}
	if birth, sample := eventIndex(rs, model.Birth, "press", 1), sampleIndex(rs, "temp", 3); birth < 0 || birth > sample {
		t.Errorf("records = %s, want the press birth before its sample", describe(rs))
	}
	if n := countEvents(rs, model.Birth, "oven"); n != 0 {
		t.Errorf("oven births = %d, want 0 before its own DBIRTH", n)
	}

	// DDEATH kills that device only.
	b.Publish(tDBIRTH, payload(nil, speed(2, 9)), false, 1)
	b.Publish(tDDEATH, payload(nil), false, 1)
	h.waitEvents(model.Death, 2, "oven")
	b.Publish(tNDATA, payload(nil, byAlias(1, 31)), false, 1)
	h.waitSamples("temp", 4)

	rs = h.snapshot()
	if countEvents(rs, model.Birth, "oven") != 1 || countEvents(rs, model.Death, "press") != 1 || countEvents(rs, model.Birth, "press") != 1 {
		t.Errorf("records = %s, want the oven born once and killed twice, the press killed and born once", describe(rs))
	}
	if errs := h.readErrors(); len(errs) != 0 {
		t.Errorf("read errors = %v, want none", errs)
	}
}

// TestSparkplugInputIgnoresAStaleWill: the NDEATH of an earlier connection
// of the node (its bdSeq differs from the last NBIRTH's) kills nothing.
func TestSparkplugInputIgnoresAStaleWill(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	h := startSparkplug(t, b, nil)

	b.Publish(tNBIRTH, nbirth(1, 0), false, 1)
	b.Publish(tNDEATH, ndeath(ptr(uint64(0))), false, 1)
	b.Publish(tNDATA, payload(nil, byAlias(1, 22)), false, 1)
	h.waitSamples("temp", 2)

	if rs := h.snapshot(); countEvents(rs, model.Death, "press")+countEvents(rs, model.Death, "oven") != 0 {
		t.Errorf("records = %s, want no death", describe(rs))
	}
}

// TestSparkplugInputLateStartWithoutRebirthPublishesNothing: an input
// started after the NBIRTH sees data by alias only. Without rebirth it
// counts unknown_alias and its client never publishes.
func TestSparkplugInputLateStartWithoutRebirthPublishesNothing(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	h := startSparkplug(t, b, nil)
	clientID := h.in.clientID

	b.Publish(tNDATA, payload(nil, byAlias(1, 5)), false, 1)
	b.Publish(tNDATA, payload(nil, byAlias(1, 6)), false, 1)
	b.Publish(tNDATA, payload(nil, temp(1, 7)), false, 1)
	if got := h.waitSamples("temp", 1); got[0].Value.Float != 7 {
		t.Fatalf("temp samples = %+v, want only the named metric", got)
	}
	if errs := h.readErrors(); !maps.Equal(errs, map[string]uint64{KindUnknownAlias: 2}) {
		t.Errorf("read errors = %v, want two unknown_alias", errs)
	}

	// Close disconnects after the outgoing packets are flushed.
	h.close()
	if got := b.Published(clientID); len(got) != 0 {
		t.Errorf("published = %+v, want nothing with rebirth off", got)
	}
}

// TestSparkplugInputLateStartAsksForARebirth: with rebirth on, the first
// unknown alias publishes one NCMD Node Control/Rebirth; the node's NBIRTH
// teaches the aliases; further unknown aliases within 30 s publish nothing
// more, and one after 30 s asks again.
func TestSparkplugInputLateStartAsksForARebirth(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	h := startSparkplug(t, b, map[string]any{"rebirth": true})
	clientID := h.in.clientID
	asked := h.clk.Now()

	b.Publish(tNDATA, payload(nil, byAlias(1, 5)), false, 1)
	got := b.WaitPublished(clientID, 1)
	ncmd := got[0]
	if ncmd.Topic != "spBv1.0/g/NCMD/n1" || ncmd.QoS != 0 || ncmd.Retain {
		t.Fatalf("published = %+v, want a QoS 0 NCMD of g/n1, not retained", ncmd)
	}
	p, err := decodePayload(ncmd.Payload)
	if err != nil {
		t.Fatalf("NCMD payload: %v", err)
	}
	if !p.HasTimestamp || p.Timestamp != uint64(asked.UnixMilli()) || len(p.Metrics) != 1 {
		t.Fatalf("NCMD payload = %+v, want the timestamp %d and one metric", p, asked.UnixMilli())
	}
	if m := p.Metrics[0]; m.Name != metricRebirth || m.Datatype != dtBoolean || m.Value.Field != spFieldBool || !m.Value.Bool {
		t.Errorf("NCMD metric = %+v, want Node Control/Rebirth = true", m)
	}

	// The node answers with its NBIRTH: the next NDATA by alias is read.
	b.Publish(tNBIRTH, payload(nil, mqtttest.BdSeq(1), temp(1, 20.5)), false, 1)
	b.Publish(tNDATA, payload(nil, byAlias(1, 21)), false, 1)
	if temps := h.waitSamples("temp", 2); temps[0].Value.Float != 20.5 || temps[1].Value.Float != 21 {
		t.Errorf("temp samples = %+v, want 20.5 then 21", temps)
	}

	// Two more unknown aliases within 30 s ask nothing. One at 30 s asks
	// again: the agent publishes in order on one connection, so a request
	// the two had wrongly made would reach the broker before it.
	b.Publish(tNDATA, payload(nil, byAlias(9, 1)), false, 1)
	b.Publish(tNDATA, payload(nil, byAlias(9, 2)), false, 1)
	b.Publish(tNDATA, payload(nil, temp(1, 22)), false, 1)
	h.waitSamples("temp", 3)
	h.steps(rebirthInterval)
	b.Publish(tNDATA, payload(nil, byAlias(9, 3)), false, 1)

	got = b.WaitPublished(clientID, 2)
	if len(got) != 2 || got[1].Topic != "spBv1.0/g/NCMD/n1" {
		t.Errorf("published = %+v, want exactly two NCMD of g/n1", got)
	}
	if errs := h.readErrors(); !maps.Equal(errs, map[string]uint64{KindUnknownAlias: 4}) {
		t.Errorf("read errors = %v, want four unknown_alias", errs)
	}
}

// TestSparkplugInputDropsCommandsAndStateUncounted: NCMD, DCMD and STATE on
// the subscribed filters change nothing and count nothing.
func TestSparkplugInputDropsCommandsAndStateUncounted(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	h := startSparkplug(t, b, nil)

	b.Publish("spBv1.0/g/NCMD/n1", payload(nil, temp(1, 1)), false, 1)
	b.Publish("spBv1.0/g/DCMD/n1/d1", payload(nil, speed(2, 1)), false, 1)
	b.Publish("spBv1.0/g/STATE/n1", []byte("not a payload"), false, 1)
	b.Publish(tNDATA, payload(nil, temp(1, 2)), false, 1)
	h.waitSamples("temp", 1)

	if rs := h.snapshot(); len(rs) != 1 {
		t.Errorf("records = %s, want the NDATA sample only", describe(rs))
	}
	if errs := h.readErrors(); len(errs) != 0 {
		t.Errorf("read errors = %v, want none", errs)
	}
}

func TestSparkplugInputDropsAPayloadAboveMaxPayload(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	small := payload(nil, temp(1, 1))
	h := startSparkplug(t, b, map[string]any{"max_payload": len(small)})

	b.Publish(tNDATA, payload(nil, temp(1, 1), temp(1, 2)), false, 1)
	b.Publish(tNDATA, small, false, 1)

	if got := h.waitSamples("temp", 1); len(got) != 1 || got[0].Value.Float != 1 {
		t.Fatalf("temp samples = %+v, want only the one at the limit", got)
	}
	if errs := h.readErrors(); !maps.Equal(errs, map[string]uint64{KindPayloadTooLarge: 1}) {
		t.Errorf("read errors = %v, want one payload_too_large", errs)
	}
}

// TestSparkplugInputSubscribesTheNodeFilters: the subscription set holds
// the node filters of the plan.
func TestSparkplugInputSubscribesTheNodeFilters(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	h := startSparkplug(t, b, nil)

	got, err := readTopics(h.in.opts.DataDir, "line")
	if err != nil || !slices.Equal(got, []string{"spBv1.0/g/+/n1/#"}) {
		t.Errorf("subscription set = %q, %v", got, err)
	}
}
