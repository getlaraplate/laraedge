package mqtt

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

func TestInputEmitsSamplesWithTheDecoderTimestamps(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	conn := map[string]any{
		"topics": map[string]any{"plant/press/state": map[string]any{"timestamp": map[string]any{"path": "ts", "format": "unix_ms"}}},
	}
	cfg := mqttInput(t, "line", b, conn, device("press",
		subscribePoint("count", "plant/press/state|count", "int32"),
		subscribePoint("mode", "plant/press/state|mode", "string"),
		subscribePoint("temp", "plant/press/temp", "float64"),
	))
	h := newHarness(t, cfg, Options{DataDir: t.TempDir(), AgentID: "agent"})
	h.start()
	h.serving()

	h.clk.Advance(time.Second)
	h.clk.BlockUntil(t, idleTimers)
	received := h.clk.Now()

	b.Publish("plant/press/state", []byte(`{"ts": 1762416000123, "count": 42, "mode": "auto"}`), false, 1)
	b.Publish("plant/press/temp", []byte("21.5"), false, 1)

	count := h.waitSamples("count", 1)[0]
	mode := h.waitSamples("mode", 1)[0]
	temp := h.waitSamples("temp", 1)[0]

	payloadTS := time.UnixMilli(1762416000123).UTC()
	if count.Value != model.IntValue(42) || count.Quality != model.Good || !count.TS.Equal(payloadTS) || count.Device != "press" {
		t.Errorf("count = %+v", count)
	}
	if mode.Value != model.StringValue("auto") || !mode.TS.Equal(payloadTS) {
		t.Errorf("mode = %+v", mode)
	}
	if temp.Value.Float != 21.5 || temp.Quality != model.Good || !temp.TS.Equal(received) {
		t.Errorf("temp = %+v, want the reception time %v", temp, received)
	}
}

func TestInputHealthReportsTheConnectionAndTheKinds(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	cfg := mqttInput(t, "line", b, nil, device("press",
		subscribePoint("count", "plant/press/state|count", "int16"),
		subscribePoint("ok", "plant/press/ok", "int32"),
	))
	h := newHarness(t, cfg, Options{})

	if h.in.Health().Connected {
		t.Fatal("Connected before Start")
	}
	h.start()
	h.serving()

	if !h.in.Health().Connected {
		t.Error("not Connected while serving")
	}

	b.Publish("plant/press/state", []byte(`{"count": `), false, 1)
	b.Publish("plant/press/state", []byte(`{"count": 70000}`), false, 1)
	b.Publish("plant/press/ok", []byte("1"), false, 1)
	h.waitSamples("ok", 1)

	got := h.in.Health().ReadErrors
	if got[KindMalformedJSON] != 1 || got[KindConversion] != 1 || got[edgeinput.ReadErrorConnection] != 0 {
		t.Errorf("ReadErrors = %v, want one malformed_json and one conversion", got)
	}
	bad := h.waitSamples("count", 2)
	if bad[0].Quality != model.Bad || bad[1].Quality != model.Bad {
		t.Errorf("count samples = %+v, want bad", bad)
	}

	h.close()
	if h.in.Health().Connected {
		t.Error("Connected after Close")
	}
}

func TestInputBrokerOutageKillsAndRevivesTheDevices(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	cfg := mqttInput(t, "line", b, nil,
		device("oven", subscribePoint("temp", "plant/oven/temp", "float64")),
		device("press", subscribePoint("count", "plant/press/count", "int32")),
	)
	h := newHarness(t, cfg, Options{})
	h.start()
	h.serving()

	b.Stop()
	h.waitEvent(traceDown)
	if h.in.Health().Connected {
		t.Error("Connected after the broker stopped")
	}

	// The tolerance passes in two steps, each letting exactly one refused
	// attempt run: the first ends the backoff, the second shows nothing more
	// happens to the dead devices.
	h.clk.BlockUntil(t, idleTimers)
	h.clk.Advance(testTolerance)
	h.waitEvent(traceDown)
	h.waitEvents(model.Death, 1, "oven", "press")
	h.clk.BlockUntil(t, idleTimers)
	h.clk.Advance(testTolerance)
	h.waitEvent(traceDown)
	h.clk.BlockUntil(t, idleTimers)

	b.Restart()
	h.advanceToBackoffEnd()
	h.waitEvent(traceServing)
	h.waitEvents(model.Birth, 1, "oven", "press")

	b.Publish("plant/press/count", []byte("7"), false, 1)
	if got := h.waitSamples("count", 1)[0]; got.Value != model.IntValue(7) {
		t.Errorf("count after the restart = %+v", got)
	}

	// The connection worked, so the backoff starts again from its minimum:
	// the first wait after the next loss is half of BackoffMin (jitter 0).
	b.Stop()
	h.waitEvent(traceDown)
	h.clk.BlockUntil(t, idleTimers)
	h.clk.Advance(driverkit.BackoffMin / 2)
	h.waitEvent(traceDown)

	records := h.snapshot()
	for _, d := range []string{"oven", "press"} {
		if n := countEvents(records, model.Death, d); n != 1 {
			t.Errorf("%s: %d deaths, want 1 (%s)", d, n, describe(records))
		}
	}
	if n := h.in.Health().ReadErrors[edgeinput.ReadErrorConnection]; n != 5 {
		t.Errorf("connection errors = %d, want two losses and three refused attempts", n)
	}
}

// TestInputOutageShorterThanTheTolerance loses the broker just before the
// next keep-alive, when the last renewal is almost half a tolerance old: the
// loss renews the devices, so an outage shorter than the tolerance kills
// nothing and one beyond it still does.
func TestInputOutageShorterThanTheTolerance(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("count", "plant/press/count", "int32")))
	h := newHarness(t, cfg, Options{})
	h.start()
	h.serving()

	h.clk.Advance(testTolerance/2 - time.Second)
	h.clk.BlockUntil(t, idleTimers)
	b.Stop()
	h.waitEvent(traceDown)

	// 9 s after the loss, 13 s after the last periodic renewal: the
	// lifecycle checks (its timer fires) and finds the press alive.
	h.clk.BlockUntil(t, idleTimers)
	h.clk.Advance(testTolerance - time.Second)
	h.waitEvent(traceDown)
	h.clk.BlockUntil(t, idleTimers)
	if records := h.snapshot(); len(records) != 0 {
		t.Fatalf("records of an outage shorter than the tolerance: %s", describe(records))
	}

	h.clk.Advance(2 * time.Second)
	h.waitEvents(model.Death, 1, "press")
}

func TestInputUnreachableBrokerAtStart(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	b.Stop()
	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("count", "plant/press/count", "int32")))
	h := newHarness(t, cfg, Options{})
	h.start()

	h.waitEvent(traceDown)
	if h.in.Health().Connected {
		t.Error("Connected without a broker")
	}
	if len(h.snapshot()) != 0 {
		t.Fatalf("records before the tolerance: %s", describe(h.snapshot()))
	}

	h.clk.BlockUntil(t, idleTimers)
	h.clk.Advance(testTolerance)
	h.waitEvents(model.Death, 1, "press")
}

func TestInputKeepsASilentDeviceAlive(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("count", "plant/press/count", "int32")))
	h := newHarness(t, cfg, Options{})
	h.start()
	h.serving()

	for range 8 {
		h.clk.Advance(testTolerance / 2)
		h.clk.BlockUntil(t, idleTimers)
	}

	if records := h.snapshot(); len(records) != 0 {
		t.Fatalf("records of a silent device over four tolerances: %s", describe(records))
	}
}

// TestInputPersistentSessionReplay closes an input, publishes while no
// client is connected and starts the next input of the same document: the
// broker kept the session of the client id, so the message reaches the new
// input and not the closed one, which emits nothing after Close.
func TestInputPersistentSessionReplay(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	dataDir := t.TempDir()
	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("count", "plant/press/count", "int32")))
	opts := Options{DataDir: dataDir, AgentID: "agent"}

	first := newHarness(t, cfg, opts)
	first.start()
	first.serving()
	warmUp(t, b, first, "plant/press/count", "count")
	first.close()
	if first.in.Health().Connected {
		t.Fatal("Connected after Close")
	}

	b.Publish("plant/press/count", []byte("11"), false, 1)

	next := newHarness(t, cfg, opts)
	next.start()
	// QoS 1 is at least once: a warm-up message whose acknowledgement the
	// close cut short comes again before the replayed one.
	next.waitValue("count", model.IntValue(11))

	// The closed input saw only the warm-up samples.
	h := first

	if n := h.late.Load(); n != 0 {
		t.Errorf("the closed input emitted %d records after Close", n)
	}
	if got := samplesOf(h.snapshot(), "count"); len(got) != warmUpMessages {
		t.Errorf("the closed input emitted %d samples, want the %d of the warm-up", len(got), warmUpMessages)
	}
}

func TestInputRetainedOfflineAtStart(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	conn := map[string]any{
		"availability": []any{map[string]any{"device": "press", "topic": "plant/press/status", "online": "online", "offline": "offline"}},
	}
	cfg := mqttInput(t, "line", b, conn,
		device("oven", subscribePoint("temp", "plant/oven/temp", "float64")),
		device("press", subscribePoint("count", "plant/press/count", "int32")),
	)
	b.Publish("plant/press/status", []byte("offline"), true, 1)

	h := newHarness(t, cfg, Options{})
	h.start()
	h.serving()
	h.waitEvents(model.Death, 1, "press")

	for range 4 {
		h.clk.Advance(testTolerance / 2)
		h.clk.BlockUntil(t, idleTimers)
	}
	records := h.snapshot()
	if countEvents(records, model.Birth, "press") != 0 || countEvents(records, model.Death, "oven") != 0 || countEvents(records, model.Death, "press") != 1 {
		t.Fatalf("records = %s, want only the press death", describe(records))
	}

	// A payload that is neither state changes nothing; a sample of the
	// device revives it, its birth before the sample.
	b.Publish("plant/press/status", []byte("unknown"), false, 1)
	b.Publish("plant/press/count", []byte("4"), false, 1)
	h.waitSamples("count", 1)
	records = h.snapshot()
	birth := slices.IndexFunc(records, func(r model.Record) bool { return r.Event != nil && r.Event.Kind == model.Birth })
	sample := slices.IndexFunc(records, func(r model.Record) bool { return r.Sample != nil })
	if birth < 0 || birth > sample {
		t.Errorf("records = %s, want the press birth before its sample", describe(records))
	}
	if n := h.in.Health().ReadErrors[ErrKindAvailability]; n != 1 {
		t.Errorf("availability errors = %d, want 1", n)
	}
}

// TestInputReloadDropsATopic restarts an input with a document that no
// longer reads a topic: the message queued for it in the persistent session
// is counted unmapped and not emitted, the topic is unsubscribed and the
// subscription set rewritten.
func TestInputReloadDropsATopic(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	dataDir := t.TempDir()
	opts := Options{DataDir: dataDir, AgentID: "agent"}

	before := mqttInput(t, "line", b, nil, device("press",
		subscribePoint("count", "plant/press/count", "int32"),
		subscribePoint("temp", "plant/press/temp", "float64"),
	))
	first := newHarness(t, before, opts)
	first.start()
	first.serving()
	warmUp(t, b, first, "plant/press/count", "count")
	if got, err := readTopics(dataDir, "line"); err != nil || !slices.Equal(got, []string{"plant/press/count", "plant/press/temp"}) {
		t.Fatalf("subscription set = %q, %v", got, err)
	}
	first.close()

	b.Publish("plant/press/temp", []byte("20.5"), false, 1)

	after := mqttInput(t, "line", b, nil, device("press", subscribePoint("count", "plant/press/count", "int32")))
	next := newHarness(t, after, opts)
	next.start()
	next.serving()

	// Published in this order on one connection: had temp still been
	// subscribed, its message would arrive before the count sample.
	b.Publish("plant/press/temp", []byte("21.5"), false, 1)
	b.Publish("plant/press/count", []byte("103"), false, 1)
	next.waitValue("count", model.IntValue(103))

	if n := next.in.Health().ReadErrors[KindUnmapped]; n != 1 {
		t.Errorf("unmapped = %d, want 1 (the queued message only)", n)
	}
	if got := samplesOf(next.snapshot(), "temp"); len(got) != 0 {
		t.Errorf("temp samples emitted: %+v", got)
	}
	if got, err := readTopics(dataDir, "line"); err != nil || !slices.Equal(got, []string{"plant/press/count"}) {
		t.Errorf("subscription set = %q, %v", got, err)
	}
}

// TestInputRollbackUnsubscribesTheFailedDocumentTopics replays a rollback:
// the input of a new document subscribes an extra topic and writes the
// subscription set, the supervisor deletes data_dir/inputs/{id}, and the
// input of the previous document starts again. It finds the set, which lives
// outside that directory, and unsubscribes the topic only the failed
// document used.
func TestInputRollbackUnsubscribesTheFailedDocumentTopics(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	dataDir := t.TempDir()
	opts := Options{DataDir: dataDir, AgentID: "agent"}

	previous := mqttInput(t, "line", b, nil, device("press", subscribePoint("count", "plant/press/count", "int32")))
	first := newHarness(t, previous, opts)
	first.start()
	first.serving()
	first.close()

	failed := mqttInput(t, "line", b, nil, device("press",
		subscribePoint("count", "plant/press/count", "int32"),
		subscribePoint("extra", "plant/press/extra", "float64"),
	))
	second := newHarness(t, failed, opts)
	second.start()
	second.serving()
	if got, err := readTopics(dataDir, "line"); err != nil || !slices.Equal(got, []string{"plant/press/count", "plant/press/extra"}) {
		t.Fatalf("subscription set of the failed document = %q, %v", got, err)
	}
	second.close()

	// The rollback removes the transform state of the restarted input.
	inputState := filepath.Join(dataDir, "inputs", "line")
	if err := os.MkdirAll(inputState, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(inputState); err != nil {
		t.Fatal(err)
	}

	restored := newHarness(t, previous, opts)
	restored.start()
	restored.serving()

	// Published in this order on one connection: had extra still been
	// subscribed, its message would arrive before the count sample.
	b.Publish("plant/press/extra", []byte("1.5"), false, 1)
	b.Publish("plant/press/count", []byte("7"), false, 1)
	restored.waitValue("count", model.IntValue(7))

	if n := restored.in.Health().ReadErrors[KindUnmapped]; n != 0 {
		t.Errorf("unmapped = %d, want 0: the failed document's topic stayed subscribed", n)
	}
	if got, err := readTopics(dataDir, "line"); err != nil || !slices.Equal(got, []string{"plant/press/count"}) {
		t.Errorf("subscription set = %q, %v", got, err)
	}
}

func TestInputDropsAPayloadAboveMaxPayload(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	cfg := mqttInput(t, "line", b, map[string]any{"max_payload": 8}, device("press", subscribePoint("mode", "plant/press/mode", "string")))
	h := newHarness(t, cfg, Options{})
	h.start()
	h.serving()

	b.Publish("plant/press/mode", []byte("123456789"), false, 1)
	b.Publish("plant/press/mode", []byte("12345678"), false, 1)

	got := h.waitSamples("mode", 1)
	if len(got) != 1 || got[0].Value != model.StringValue("12345678") {
		t.Fatalf("samples = %+v, want only the one at the limit", got)
	}
	if n := h.in.Health().ReadErrors[KindPayloadTooLarge]; n != 1 {
		t.Errorf("payload_too_large = %d, want 1", n)
	}
}

func TestInputResolvesThePasswordAtEachConnection(t *testing.T) {
	const secret = "s3cret-pw-6612"
	b := mqtttest.Start(t, mqtttest.Options{Users: map[string]string{"edge": secret}})
	conn := map[string]any{"auth": map[string]any{"user": "edge", "password": "secret://env/MQTT_PASSWORD"}}
	cfg := mqttInput(t, "line", b, conn, device("press", subscribePoint("count", "plant/press/count", "int32")))
	resolver := &fakeResolver{value: secret}

	h := newHarness(t, cfg, Options{Resolver: resolver})
	h.start()
	h.serving()
	if n := resolver.calls.Load(); n != 1 {
		t.Fatalf("resolver calls = %d, want 1", n)
	}

	b.Stop()
	h.waitEvent(traceDown)
	b.Restart()
	h.advanceToBackoffEnd()
	h.waitEvent(traceServing)
	if n := resolver.calls.Load(); n != 2 {
		t.Errorf("resolver calls = %d, want 2", n)
	}
}

func TestInputWithoutResolverRetries(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	conn := map[string]any{"auth": map[string]any{"user": "edge", "password": "secret://env/MQTT_PASSWORD"}}
	cfg := mqttInput(t, "line", b, conn, device("press", subscribePoint("count", "plant/press/count", "int32")))

	h := newHarness(t, cfg, Options{})
	h.start()
	h.waitEvent(traceDown)
	if n := h.in.Health().ReadErrors[edgeinput.ReadErrorConnection]; n != 1 {
		t.Errorf("connection errors = %d, want 1", n)
	}
}

func TestInputConnectsOverTLS(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{TLS: true})
	cfg := mqttInput(t, "line", b, map[string]any{"tls": map[string]any{"ca_file": b.CA()}}, device("press", subscribePoint("count", "plant/press/count", "int32")))
	cfg.Connection = []byte(strings.Replace(string(cfg.Connection), "mqtt://", "mqtts://", 1))

	h := newHarness(t, cfg, Options{})
	h.start()
	h.serving()

	b.Publish("plant/press/count", []byte("5"), false, 1)
	h.waitSamples("count", 1)
}

// warmUpMessages is how many messages warmUp delivers.
const warmUpMessages = 3

// warmUp delivers warmUpMessages QoS 1 messages on topic to the input of h
// and waits for their samples of signal. mochi keeps the packet ids it sends
// and those it receives in one inflight map, so a message queued for a
// persistent session under id 1 makes it answer the next client's SUBSCRIBE
// or UNSUBSCRIBE with id 1 as "identifier in use" without acting on it (MQTT
// 3.1.1 gives each direction its own id space; real brokers do). Moving the
// broker's next id past the client's first ones keeps the tests about the
// driver.
func warmUp(t *testing.T, b *mqtttest.Broker, h *harness, topic, signal string) {
	t.Helper()

	for i := range warmUpMessages {
		b.Publish(topic, []byte(strconv.Itoa(i)), false, 1)
	}
	h.waitSamples(signal, warmUpMessages)
}

// waitValue waits for a sample of signal with value v.
func (h *harness) waitValue(signal string, v model.Value) {
	h.t.Helper()

	h.waitRecords(signal+" sample", func(rs []model.Record) bool {
		return slices.ContainsFunc(samplesOf(rs, signal), func(s model.Sample) bool { return s.Value == v })
	})
}

// TestRouterHandledReportsWhatToAcknowledge: a message the router counts
// is handled, so acknowledged; one arriving after stop is not.
func TestRouterHandledReportsWhatToAcknowledge(t *testing.T) {
	r := &router{in: &Input{conn: Connection{MaxPayload: 1}}}
	if !r.message("plant/a", []byte("too large")) {
		t.Error("a message counted payload_too_large was not handled")
	}
	if n := r.in.Health().ReadErrors[KindPayloadTooLarge]; n != 1 {
		t.Errorf("payload_too_large = %d, want 1", n)
	}

	r.stop()
	if r.message("plant/a", []byte("x")) {
		t.Error("a message after stop was handled, so acknowledged")
	}
	if n := r.in.Health().ReadErrors[KindPayloadTooLarge]; n != 1 {
		t.Errorf("payload_too_large = %d after stop, want still 1", n)
	}
}
