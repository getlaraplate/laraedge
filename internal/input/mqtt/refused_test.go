package mqtt

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

// logBuffer collects the JSON log lines of a logger, safe for concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

// logger returns a logger writing to l at debug and above.
func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lines returns the log lines at level whose message is msg.
func (l *logBuffer) lines(t *testing.T, level slog.Level, msg string) []map[string]any {
	t.Helper()

	l.mu.Lock()
	text := l.buf.String()
	l.mu.Unlock()

	var out []map[string]any
	for line := range strings.Lines(text) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if entry["level"] == level.String() && entry["msg"] == msg {
			out = append(out, entry)
		}
	}

	return out
}

func TestPahoClientSubscribeReportsTheRefusedTopics(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{Refuse: []string{"plant/vault"}})
	c, messages := newTestClient(t, testConnection(b, SchemePlain), "refused-topics", "", true)
	ctx := testContext(t)

	if _, err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	refused, err := c.Subscribe(ctx, []string{"plant/press", "plant/vault"}, 1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if !slices.Equal(refused, []string{"plant/vault"}) {
		t.Errorf("refused = %q, want plant/vault", refused)
	}

	// The accepted topic is subscribed and the connection kept.
	b.Publish("plant/press", []byte("1"), false, 1)
	if got := receive(t, messages); got.topic != "plant/press" {
		t.Errorf("message = %+v", got)
	}
}

// TestInputRefusedSubscriptionKeepsTheConnection: the broker refuses one of
// the two topics. The connection is kept, the refusal is logged at warn once
// and counted per point, the accepted topic delivers, the device of the
// refused topic dies at the tolerance while the other one lives, and the
// refused topic is left out of the subscription set. Once the ACL allows it,
// the next connection subscribes it.
func TestInputRefusedSubscriptionKeepsTheConnection(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{Refuse: []string{"plant/vault/state"}})
	dataDir := t.TempDir()
	cfg := mqttInput(t, "line", b, nil,
		device("press", subscribePoint("count", "plant/press/count", "int32")),
		device("vault",
			subscribePoint("open", "plant/vault/state|open", "bool"),
			subscribePoint("temp", "plant/vault/state|temp", "float64"),
		),
	)
	var logs logBuffer
	h := newHarness(t, cfg, Options{DataDir: dataDir, AgentID: "agent", Logger: logs.logger()})
	h.start()
	h.serving()

	health := h.in.Health()
	if !health.Connected {
		t.Error("not Connected with one topic accepted")
	}
	if n := health.ReadErrors[KindSubscribeRefused]; n != 2 {
		t.Errorf("subscribe_refused = %d, want one per point of the refused topic", n)
	}
	if n := health.ReadErrors[edgeinput.ReadErrorConnection]; n != 0 {
		t.Errorf("connection errors = %d, want none", n)
	}
	warned := logs.lines(t, slog.LevelWarn, msgSubscribeRefused)
	if len(warned) != 1 || warned[0]["topic"] != "plant/vault/state" || warned[0]["input"] != "line" {
		t.Errorf("warnings = %v, want one naming the topic and the input", warned)
	}
	if got, err := readTopics(dataDir, "line"); err != nil || !slices.Equal(got, []string{"plant/press/count"}) {
		t.Errorf("subscription set = %q, %v, want the accepted topic only", got, err)
	}

	// The accepted topic delivers. warmUp, and the acknowledgements reaching
	// the broker, keep mochi's shared packet ids from colliding with the
	// SUBSCRIBE of the next connection.
	warmUp(t, b, h, "plant/press/count", "count")
	b.WaitAcknowledged("laraplate-edge-agent-line", warmUpMessages)

	// Over the tolerance the vault, never kept alive, dies; the press lives.
	for range 3 {
		h.clk.Advance(testTolerance / 2)
		h.clk.BlockUntil(t, idleTimers)
	}
	h.waitEvents(model.Death, 1, "vault")
	if n := countEvents(h.snapshot(), model.Death, "press"); n != 0 {
		t.Errorf("press deaths = %d, want 0", n)
	}
	if n := len(logs.lines(t, slog.LevelWarn, msgSubscribeRefused)); n != 1 {
		t.Errorf("%d warnings on one connection, want 1", n)
	}

	// The ACL is fixed; a new connection subscribes the topic again.
	b.SetRefused()
	b.Stop()
	h.waitEvent(traceDown)
	b.Restart()
	h.advanceToBackoffEnd()
	h.waitEvent(traceServing)

	b.Publish("plant/vault/state", []byte(`{"open": true, "temp": 4.5}`), false, 1)
	h.waitValue("open", model.BoolValue(true))
	h.waitEvents(model.Birth, 1, "vault")
	if got, err := readTopics(dataDir, "line"); err != nil || !slices.Equal(got, []string{"plant/press/count", "plant/vault/state"}) {
		t.Errorf("subscription set = %q, %v, want both topics", got, err)
	}
	if n := h.in.Health().ReadErrors[KindSubscribeRefused]; n != 2 {
		t.Errorf("subscribe_refused = %d after the fix, want still 2", n)
	}
}

// TestInputConnectedOnlyAfterTheSuback: the input reports Connected only
// once the broker answered the subscriptions, not at CONNACK.
func TestInputConnectedOnlyAfterTheSuback(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	hold := b.HoldSubscriptions()
	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("count", "plant/press/count", "int32")))
	h := newHarness(t, cfg, Options{})
	h.start()

	select {
	case <-hold.Arrived():
	case <-time.After(waitTimeout):
		t.Fatal("no SUBSCRIBE reached the broker")
	}
	if h.in.Health().Connected {
		t.Error("Connected before the SUBACK")
	}
	if records := h.snapshot(); len(records) != 0 {
		t.Errorf("records before the SUBACK: %s", describe(records))
	}

	hold.Release()
	h.serving()
	if !h.in.Health().Connected {
		t.Error("not Connected after the SUBACK")
	}
}

func TestBrowseARefusedFilterIsAnError(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{Refuse: []string{"#"}})
	cfg := mqttInput(t, "line", b, nil, device("press", subscribePoint("temp", "plant/press/temp", "float64")))
	in, _, _ := newCommandInput(t, cfg, Options{})

	entries, _, err := in.Browse(testContext(t), "#", time.Second)
	if !errors.Is(err, errSubscribeRefused) || entries != nil {
		t.Fatalf("Browse = %v, %v, want the refusal", entries, err)
	}
}

func TestSubscriptionSet(t *testing.T) {
	topics := []string{"a", "b", "c"}
	got := subscriptionSet(topics, []string{"b", "c"}, []string{"a", "c", "gone"})
	if want := []string{"a", "c"}; !slices.Equal(got, want) {
		t.Errorf("subscriptionSet = %q, want %q (c was held before, b is new)", got, want)
	}
}

// TestInputKeepsARefusedTopicItHeldBefore: a topic granted on an earlier
// connection and refused on a later one stays in the subscription set, so
// the document that drops it still unsubscribes it.
func TestInputKeepsARefusedTopicItHeldBefore(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	dataDir := t.TempDir()
	opts := Options{DataDir: dataDir, AgentID: "agent"}
	press := device("press", subscribePoint("count", "plant/press/count", "int32"))
	vault := device("vault", subscribePoint("open", "plant/vault", "bool"))

	first := newHarness(t, mqttInput(t, "line", b, nil, press, vault), opts)
	first.start()
	first.serving()
	warmUp(t, b, first, "plant/press/count", "count")
	b.WaitAcknowledged("laraplate-edge-agent-line", warmUpMessages)

	b.SetRefused("plant/vault")
	b.Stop()
	first.waitEvent(traceDown)
	b.Restart()
	first.advanceToBackoffEnd()
	first.waitEvent(traceServing)

	if n := first.in.Health().ReadErrors[KindSubscribeRefused]; n != 1 {
		t.Fatalf("subscribe_refused = %d, want 1", n)
	}
	if got, err := readTopics(dataDir, "line"); err != nil || !slices.Equal(got, []string{"plant/press/count", "plant/vault"}) {
		t.Fatalf("subscription set = %q, %v, want the refused topic kept", got, err)
	}
	first.close()

	// The ACL opens again, so only the unsubscription keeps a message of the
	// topic, still subscribed in the session, from reaching the input.
	b.SetRefused()
	next := newHarness(t, mqttInput(t, "line", b, nil, press), opts)
	next.start()
	next.serving()
	if got, err := readTopics(dataDir, "line"); err != nil || !slices.Equal(got, []string{"plant/press/count"}) {
		t.Errorf("subscription set = %q, %v, want the dropped topic gone", got, err)
	}

	b.Publish("plant/vault", []byte("true"), false, 1)
	b.Publish("plant/press/count", []byte("103"), false, 1)
	next.waitValue("count", model.IntValue(103))
	if n := next.in.Health().ReadErrors[KindUnmapped]; n != 0 {
		t.Errorf("unmapped = %d, want 0: the dropped topic was not unsubscribed", n)
	}
}

// TestSparkplugInputRefusedNodeFilter: under sparkplug_b the broker refuses
// the filter of one node. The refusal counts once per point of that node,
// its device dies at the tolerance, and the quiet device of the other node,
// whose filter was accepted, is kept alive.
func TestSparkplugInputRefusedNodeFilter(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{Refuse: []string{"spBv1.0/g/+/n1/#"}})
	cfg := sparkplugInput(t, "line", b, nil,
		device("press",
			subscribePoint("temp", "g/n1|Temp", "float64"),
			subscribePoint("count", "g/n1|Count", "int64"),
		),
		device("saw", subscribePoint("on", "g/n2|On", "bool")),
	)
	h := newHarness(t, cfg, Options{DataDir: t.TempDir(), AgentID: "agent"})
	h.start()
	h.serving()

	if n := h.in.Health().ReadErrors[KindSubscribeRefused]; n != 2 {
		t.Errorf("subscribe_refused = %d, want one per point of the refused node", n)
	}

	for range 3 {
		h.clk.Advance(testTolerance / 2)
		h.clk.BlockUntil(t, idleTimers)
	}
	h.waitEvents(model.Death, 1, "press")
	if n := countEvents(h.snapshot(), model.Death, "saw"); n != 0 {
		t.Errorf("saw deaths = %d, want 0: its node filter was accepted", n)
	}
}
