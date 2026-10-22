package opcua

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/logging"
	"github.com/getlaraplate/laraedge/internal/model"
)

// driverEpoch is the fake clock's start in the driver tests.
var driverEpoch = time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)

// idleTimers is the number of clock timers of a started input at rest: the
// lifecycle's next check and the owner goroutine's wait (the next poll or
// watchdog read, or the backoff).
const idleTimers = 2

// fakeResolver resolves every password reference to value.
type fakeResolver struct {
	value string
	calls atomic.Int32
}

func (r *fakeResolver) Resolve(_ context.Context, _ string) (string, error) {
	r.calls.Add(1)

	return r.value, nil
}

// harness runs one input on a fake clock and records what it emits.
type harness struct {
	t       *testing.T
	clk     *clock.Fake
	in      *Input
	dataDir string
	logs    *syncBuffer

	mu      sync.Mutex
	records []model.Record

	closed atomic.Bool
	late   atomic.Int32
}

// newHarness builds the input of cfg with a data directory holding the
// fixture's own certificate (no key generation per test), a backoff jitter
// of 0 (waits of 0.5 s, 1 s, 2 s...) and logs at debug.
func newHarness(t *testing.T, cfg config.Input) *harness {
	t.Helper()
	loadFixtures(t)

	dataDir := t.TempDir()
	seedOwnCertificate(t, dataDir)

	logs := &syncBuffer{}
	clk := clock.NewFake(driverEpoch)

	in, err := New(cfg, clk, Options{
		DataDir:  dataDir,
		AgentID:  "test-agent",
		Resolver: &fakeResolver{value: "scan-pass-5521"},
		Logger:   logging.New(logs, slog.LevelDebug),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in.jitter = func() float64 { return 0 }
	in.timing = &testTimings

	return &harness{t: t, clk: clk, in: in, dataDir: dataDir, logs: logs}
}

// seedOwnCertificate writes the fixture's own certificate and key where
// EnsureOwnCertificate finds them.
func seedOwnCertificate(t *testing.T, dataDir string) {
	t.Helper()

	own := filepath.Join(PKIDir(dataDir), "own")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, ownCertFile), fixtures.ownDER, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, ownKeyFile), fixtures.ownPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) trust(der []byte) {
	h.t.Helper()

	if _, err := (TrustStore{Dir: PKIDir(h.dataDir)}).Trust(der); err != nil {
		h.t.Fatalf("trust: %v", err)
	}
}

func (h *harness) start() {
	h.t.Helper()

	if err := h.in.Start(h.t.Context(), h.emit); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	h.t.Cleanup(h.close)
}

func (h *harness) emit(r model.Record) {
	if h.closed.Load() {
		h.late.Add(1)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, r)
}

func (h *harness) close() {
	if err := h.in.Close(); err != nil {
		h.t.Errorf("Close: %v", err)
	}
	h.closed.Store(true)

	if n := h.late.Load(); n != 0 {
		h.t.Errorf("%d records emitted after Close", n)
	}
}

// mark is the number of records emitted so far, to look at the later ones.
func (h *harness) mark() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.records)
}

// since returns the records emitted after mark.
func (h *harness) since(mark int) []model.Record {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.records[mark:])
}

// samples returns the samples of signal emitted after mark.
func (h *harness) samples(mark int, signal string) []model.Sample {
	var out []model.Sample

	for _, r := range h.since(mark) {
		if r.Sample != nil && r.Sample.Signal == signal {
			if err := r.Sample.Validate(); err != nil {
				h.t.Errorf("invalid sample %+v: %v", *r.Sample, err)
			}
			out = append(out, *r.Sample)
		}
	}

	return out
}

// events lists the device events emitted after mark as "<kind> <device>".
func (h *harness) events(mark int) []string {
	var out []string

	for _, r := range h.since(mark) {
		if r.Event != nil {
			out = append(out, r.Event.Kind.String()+" "+r.Event.Device)
		}
	}

	return out
}

func (h *harness) connected() bool {
	return h.in.Health().Connected
}

// waitFor waits until cond holds; a deadlock guard, never a timing assertion.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()

	deadline := time.Now().Add(guard)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("%s: not reached", what)
		}
		runtime.Gosched()
	}
}

// waitSamples waits until signal has at least n samples after mark.
func (h *harness) waitSamples(mark int, signal string, n int) []model.Sample {
	h.t.Helper()

	h.waitFor(fmt.Sprintf("%d samples of %s", n, signal), func() bool { return len(h.samples(mark, signal)) >= n })

	return h.samples(mark, signal)
}

// idle waits until the input's goroutines are blocked on the clock.
func (h *harness) idle() {
	h.t.Helper()

	deadline := time.Now().Add(guard)
	for h.clk.PendingTimers() < idleTimers {
		if time.Now().After(deadline) {
			h.t.Fatalf("PendingTimers() = %d, want %d", h.clk.PendingTimers(), idleTimers)
		}
		runtime.Gosched()
	}
}

// step advances the clock by d and waits until the input is idle again.
func (h *harness) step(d time.Duration) {
	h.t.Helper()
	h.clk.Advance(d)
	h.idle()
}

// stepUntil advances the clock by d until cond holds, at most limit of
// clock time.
func (h *harness) stepUntil(what string, d, limit time.Duration, cond func() bool) {
	h.t.Helper()

	for elapsed := time.Duration(0); !cond(); elapsed += d {
		if elapsed > limit {
			h.t.Fatalf("%s: not reached within %s of clock time", what, limit)
		}
		h.step(d)
	}
}

// countWarnings returns the records above debug whose message contains text.
func (h *harness) warnings(text string) []map[string]any {
	var out []map[string]any

	for _, rec := range h.logs.records(h.t) {
		if rec["level"] != "DEBUG" && strings.Contains(fmt.Sprint(rec["msg"]), text) {
			out = append(out, rec)
		}
	}

	return out
}

// assertNothingSecretAboveDebug fails when a log record above debug carries
// one of the forbidden texts: a value, an endpoint, a user name, a password.
func assertNothingSecretAboveDebug(t *testing.T, logs *syncBuffer, forbidden ...string) {
	t.Helper()

	for _, rec := range logs.records(t) {
		if rec["level"] == "DEBUG" {
			continue
		}

		line := fmt.Sprint(rec)
		for _, f := range forbidden {
			if strings.Contains(line, f) {
				t.Errorf("a %v record carries %q: %s", rec["level"], f, line)
			}
		}
	}
}

func opcuaInput(connection string, tolerance time.Duration, devices ...config.Device) config.Input {
	return config.Input{
		ID:             "in1",
		Driver:         config.DriverOPCUA,
		Connection:     []byte(connection),
		DeathTolerance: tolerance,
		Devices:        devices,
	}
}

func noneConn(endpoint string) string {
	return `{"endpoint":"` + endpoint + `","security_policy":"None","publishing_interval":"50ms"}`
}

func secureConn(endpoint, auth string) string {
	conn := `{"endpoint":"` + endpoint + `","security_policy":"Basic256Sha256","security_mode":"SignAndEncrypt","publishing_interval":"50ms"`
	if auth != "" {
		conn += `,"auth":` + auth
	}

	return conn + "}"
}

func subscribed(signal, address, typ string) config.Point {
	return config.Point{
		Signal:   signal,
		Address:  address,
		Type:     typ,
		Read:     config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: config.SamplingDefault}},
		Priority: model.Critical,
	}
}

func polled(signal, address, typ string, every time.Duration) config.Point {
	return config.Point{
		Signal:   signal,
		Address:  address,
		Type:     typ,
		Read:     config.Read{Mode: config.ReadPoll, Interval: every},
		Priority: model.Critical,
	}
}

// address is the NodeId text of a node of the test namespace.
func (s *testServer) address(name string) string {
	return "ns=" + strconv.Itoa(int(s.NamespaceIndex())) + ";s=" + name
}

func device(name string, points ...config.Point) config.Device {
	return config.Device{Device: name, Points: points}
}

// countedClients wraps every library client the input builds in a
// countingClient; maxMonitored replaces the server's MaxMonitoredItemsPerCall.
type countedClients struct {
	maxMonitored uint32

	mu      sync.Mutex
	clients []*countingClient
}

func (c *countedClients) install(in *Input) {
	in.newClient = func(endpoint string, opts []opcua.Option, onState func(opcua.ConnState)) (uaClient, error) {
		inner, err := newLibraryClient(endpoint, opts, onState)
		if err != nil {
			return nil, err
		}

		cc := &countingClient{uaClient: inner, maxMonitored: c.maxMonitored}

		c.mu.Lock()
		c.clients = append(c.clients, cc)
		c.mu.Unlock()

		return cc, nil
	}
}

func (c *countedClients) all() []*countingClient {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.clients)
}

func TestFactoryValidatesOffline(t *testing.T) {
	good := polled("count", "ns=2;s=count", "int32", time.Second)

	tests := []struct {
		name       string
		connection string
		point      config.Point
		want       string
	}{
		{"no endpoint", `{}`, good, `opcua input "in1": connection.endpoint: required`},
		{"user with None", `{"endpoint":"opc.tcp://plc.example:4840","security_policy":"None","auth":{"user":"op","password":"secret://env/P"}}`, good,
			`opcua input "in1": connection.auth: user and password need a security_policy other than None`},
		{"bad NodeId", noneConn("opc.tcp://plc.example:4840"), polled("count", "ns=x;s=count", "int32", time.Second),
			`opcua input "in1": devices.0.points.1: address: namespace index: want an integer from 0 to 65535`},
		{"no type", noneConn("opc.tcp://plc.example:4840"), polled("count", "ns=2;s=count", "", time.Second),
			`opcua input "in1": devices.0.points.1: type: required`},
		{"event", noneConn("opc.tcp://plc.example:4840"), config.Point{Signal: "e", Address: "i=1", Type: "bool", Read: config.Read{Mode: config.ReadEvent}},
			`opcua input "in1": devices.0.points.1: read: want subscribe or poll`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			cfg := opcuaInput(tt.connection, time.Second, device("press-07", good, tt.point))

			_, err := Factory(Options{DataDir: dataDir, AgentID: "a"})(cfg, clock.NewFake(driverEpoch))
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Factory error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "plc.example") {
				t.Fatalf("the error names the endpoint: %v", err)
			}
		})
	}

	t.Run("valid, without touching the disk", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		cfg := opcuaInput(noneConn("opc.tcp://plc.example:4840"), time.Second, device("press-07", good, subscribed("temp", "nsu=urn:x;s=temp", "float64")))

		in, err := Factory(Options{DataDir: dataDir, AgentID: "a"})(cfg, clock.NewFake(driverEpoch))
		if err != nil {
			t.Fatalf("Factory: %v", err)
		}
		if err := in.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the factory touched the data directory: %v", err)
		}
	})
}

func TestStartFailsWithAnUnwritableDataDir(t *testing.T) {
	loadFixtures(t)

	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := opcuaInput(noneConn("opc.tcp://127.0.0.1:1"), time.Second, device("press-07", polled("count", "i=2258", "int32", time.Second)))
	in, err := New(cfg, clock.NewFake(driverEpoch), Options{DataDir: notADir, AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}

	err = in.Start(t.Context(), func(model.Record) {})
	if err == nil || !strings.HasPrefix(err.Error(), `opcua input "in1": start: `) {
		t.Fatalf("Start = %v, want a start error naming the input", err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNoneWarnsAtEveryStart(t *testing.T) {
	loadFixtures(t)

	const warning = "input in1: OPC UA connection is unprotected (security_policy None)"

	dataDir := t.TempDir()
	seedOwnCertificate(t, dataDir)

	logs := &syncBuffer{}
	factory := Factory(Options{DataDir: dataDir, AgentID: "test-agent", Logger: logging.New(logs, slog.LevelDebug)})
	points := device("press-07", polled("count", "i=2258", "int32", time.Second))

	for _, conn := range []string{noneConn("opc.tcp://127.0.0.1:1"), noneConn("opc.tcp://127.0.0.1:1"), secureConn("opc.tcp://127.0.0.1:1", "")} {
		in, err := factory(opcuaInput(conn, time.Second, points), clock.NewFake(driverEpoch))
		if err != nil {
			t.Fatal(err)
		}
		if err := in.Start(t.Context(), func(model.Record) {}); err != nil {
			t.Fatal(err)
		}
		if err := in.Close(); err != nil {
			t.Fatal(err)
		}
	}

	var got int

	for _, rec := range logs.records(t) {
		if rec["msg"] == warning {
			got++
			if rec["level"] != "WARN" || rec["input"] != "in1" {
				t.Errorf("warning record = %v, want WARN with input in1", rec)
			}
		}
	}
	if got != 2 {
		t.Fatalf("%d warnings %q, want one per Start of a None input (2)", got, warning)
	}
}

func TestSubscribeEmitsInitialValueAndChangesWithQualityAndSourceTimestamp(t *testing.T) {
	srv := startTestServer(t)
	h := newHarness(t, opcuaInput(noneConn(srv.URL), 30*time.Second, device("press-07",
		subscribed("level", srv.address(nodeChanging), "float64"),
		polled("level_polled", srv.address(nodeChanging), "float64", time.Second),
	)))
	h.start()

	initial := h.waitSamples(0, "level", 1)
	if s := initial[0]; s.Quality != model.Good || s.Value.Float != 0 || s.Device != "press-07" || s.Priority != model.Critical {
		t.Fatalf("initial sample = %+v, want good 0 of press-07", s)
	}
	h.waitSamples(0, "level_polled", 1)
	h.idle()

	tests := []struct {
		status  ua.StatusCode
		value   float64
		quality model.Quality
		want    float64
	}{
		{ua.StatusOK, 1.5, model.Good, 1.5},
		{ua.StatusUncertain, 2.5, model.Uncertain, 2.5},
		{ua.StatusBadSensorFailure, 3.5, model.Bad, 0},
	}

	for i, tt := range tests {
		source := time.Date(2026, 1, 2, 3, 4, 5+i, 0, time.UTC)
		mark := h.mark()

		dv := &ua.DataValue{
			EncodingMask:    ua.DataValueValue | ua.DataValueSourceTimestamp | ua.DataValueStatusCode,
			Value:           ua.MustVariant(tt.value),
			Status:          tt.status,
			SourceTimestamp: source,
		}
		if status := srv.ns.SetAttribute(srv.NodeID(nodeChanging), ua.AttributeIDValue, dv); status != ua.StatusOK {
			t.Fatalf("set: %v", status)
		}

		pushed := h.waitSamples(mark, "level", 1)[0]
		h.step(time.Second)
		read := h.waitSamples(mark, "level_polled", 1)[0]

		for name, s := range map[string]model.Sample{"subscribed": pushed, "polled": read} {
			if s.Quality != tt.quality || s.Value.Kind != model.KindFloat || s.Value.Float != tt.want || !s.TS.Equal(source) {
				t.Errorf("%s sample for status %v = %+v, want %s %v at %v", name, tt.status, s, tt.quality, tt.want, source)
			}
		}
	}
}

func TestPollPointsAtTheirIntervalOnTheFakeClock(t *testing.T) {
	srv := sharedTestServer(t)
	h := newHarness(t, opcuaInput(noneConn(srv.URL), 30*time.Second, device("press-07",
		polled("temp", srv.address(nodeTemp), "float64", 2*time.Second),
		polled("count", srv.address(nodeCount), "int32", 5*time.Second),
	)))
	h.start()

	h.waitSamples(0, "temp", 1)
	h.waitSamples(0, "count", 1)
	h.idle()

	for second := 1; second <= 10; second++ {
		h.step(time.Second)

		temps, counts := h.samples(0, "temp"), h.samples(0, "count")
		if len(temps) != 1+second/2 || len(counts) != 1+second/5 {
			t.Fatalf("after %d s: %d temp and %d count samples, want %d and %d", second, len(temps), len(counts), 1+second/2, 1+second/5)
		}
	}

	if s := h.samples(0, "temp")[0]; s.Value.Float != 20 || s.Quality != model.Good {
		t.Fatalf("temp = %+v, want good 20", s)
	}
	if s := h.samples(0, "count")[0]; s.Value.Int != 1 || s.Quality != model.Good {
		t.Fatalf("count = %+v, want good 1", s)
	}
}

// Review Focus: the server comes back with a new certificate.
func TestNewServerCertificateIsRefusedUntilTrusted(t *testing.T) {
	first := startTestServer(t)
	h := newHarness(t, opcuaInput(secureConn(first.URL, ""), 5*time.Second, device("press-07",
		subscribed("temp", first.address(nodeTemp), "float64"),
	)))
	h.trust(first.Cert)
	h.start()

	h.waitSamples(0, "temp", 1)
	h.idle()

	start := h.clk.Now()
	otherDER, otherKey := loadOtherServerCertificate(t)
	second := first.replace(t, serverOptions{certDER: otherDER, key: otherKey})
	thumb := Thumbprint(second.Cert)

	h.waitFor("connection lost", func() bool { return !h.connected() })
	h.idle()

	mark := h.mark()
	h.stepUntil("death", 500*time.Millisecond, time.Minute, func() bool { return len(h.events(mark)) > 0 })

	if got := h.events(mark); !slices.Equal(got, []string{"death press-07"}) {
		t.Fatalf("events = %v, want one death", got)
	}
	if elapsed := h.clk.Now().Sub(start); elapsed < 5*time.Second {
		t.Fatalf("death after %s, want at least the death tolerance", elapsed)
	}
	if len(h.samples(mark, "temp")) != 0 {
		t.Fatal("samples emitted from an untrusted server")
	}

	// More retries meet the same certificate: still one warning.
	h.step(10 * time.Second)
	h.step(10 * time.Second)

	warnings := h.warnings("server certificate not trusted")
	if len(warnings) != 1 || !strings.Contains(fmt.Sprint(warnings[0]["msg"]), thumb) || warnings[0]["level"] != "WARN" {
		t.Fatalf("warnings = %v, want one WARN naming thumbprint %s", warnings, thumb)
	}
	if _, err := os.Stat(filepath.Join(PKIDir(h.dataDir), "rejected", thumb+".der")); err != nil {
		t.Fatalf("rejected certificate not stored: %v", err)
	}
	if n := h.in.Health().ReadErrors[input.ReadErrorConnection]; n < 3 {
		t.Fatalf("connection errors = %d, want one per refused attempt", n)
	}

	h.trust(second.Cert)
	reborn := h.mark()
	h.stepUntil("reconnection", 500*time.Millisecond, 2*time.Minute, h.connected)
	h.waitSamples(reborn, "temp", 1)

	if got := h.events(reborn); !slices.Equal(got, []string{"birth press-07"}) {
		t.Fatalf("events after trust = %v, want one birth", got)
	}
	if !h.connected() {
		t.Fatal("not connected after the certificate was trusted")
	}

	assertNothingSecretAboveDebug(t, h.logs, first.URL, "127.0.0.1", strconv.Itoa(first.Proxy.Port()))
}

// Review Focus: values that never change keep their devices alive (O4).
func TestConstantSubscriptionKeepsDevicesAlive(t *testing.T) {
	srv := sharedTestServer(t)
	h := newHarness(t, opcuaInput(noneConn(srv.URL), 3*time.Second,
		device("press-07", subscribed("temp", srv.address(nodeTemp), "float64")),
		device("press-08", subscribed("count", srv.address(nodeCount), "int32")),
	))
	clients := &countedClients{}
	clients.install(h.in)
	h.start()

	h.waitSamples(0, "temp", 1)
	h.waitSamples(0, "count", 1)
	h.waitFor("connected", h.connected)
	h.idle()

	mark := h.mark()
	for range 12 {
		h.step(time.Second)
	}

	if got := h.events(mark); len(got) != 0 {
		t.Fatalf("events = %v, want none: the watchdog read keeps the devices alive", got)
	}
	if got := len(h.samples(mark, "temp")) + len(h.samples(mark, "count")); got != 0 {
		t.Fatalf("%d samples, want none (the values never change)", got)
	}

	reads, _ := clients.all()[0].sizes()
	if watchdogs := len(reads) - 2; watchdogs < 12 {
		t.Fatalf("%d watchdog reads in 12 s, want one per second", watchdogs)
	}
}

// Review Focus: more points than the server's MaxNodesPerRead (32).
func TestMorePointsThanTheServerLimits(t *testing.T) {
	srv := sharedTestServer(t)

	var points []config.Point
	for i := range 70 {
		points = append(points,
			polled("poll"+strconv.Itoa(i), srv.address(nodeTemp), "float64", time.Second),
			subscribed("sub"+strconv.Itoa(i), srv.address(nodeCount), "int32"),
		)
	}

	h := newHarness(t, opcuaInput(noneConn(srv.URL), 30*time.Second, device("press-07", points...)))
	clients := &countedClients{maxMonitored: 32}
	clients.install(h.in)
	h.start()

	for i := range 70 {
		h.waitSamples(0, "poll"+strconv.Itoa(i), 1)
		h.waitSamples(0, "sub"+strconv.Itoa(i), 1)
	}
	h.idle()
	h.step(time.Second)

	for i := range 70 {
		if got := h.samples(0, "poll"+strconv.Itoa(i)); len(got) != 2 || got[1].Quality != model.Good || got[1].Value.Float != 20 {
			t.Fatalf("poll%d = %+v, want two good samples of 20", i, got)
		}
		if got := h.samples(0, "sub"+strconv.Itoa(i)); got[0].Quality != model.Good || got[0].Value.Int != 1 {
			t.Fatalf("sub%d = %+v, want good 1", i, got)
		}
	}

	all := clients.all()
	if len(all) != 1 {
		t.Fatalf("%d clients, want one session for the whole test", len(all))
	}

	reads, monitors := all[0].sizes()
	if !slices.Equal(reads, []int{1, 1, 32, 32, 6, 32, 32, 6}) {
		t.Fatalf("read sizes = %v, want the limits, then two polls in chunks of 32", reads)
	}
	if !slices.Equal(monitors, []int{32, 32, 6}) {
		t.Fatalf("monitored item calls = %v, want chunks of 32", monitors)
	}
	if !h.connected() || h.in.Health().ReadErrors[input.ReadErrorConnection] != 0 {
		t.Fatalf("health = %+v, want connected without connection errors", h.in.Health())
	}
}

// Review Focus: nsu= points after a restart that registers the namespaces in
// another order.
func TestNamespaceURIsAreResolvedAgainAfterARestart(t *testing.T) {
	first := startTestServer(t)
	h := newHarness(t, opcuaInput(noneConn(first.URL), 30*time.Second, device("press-07",
		polled("count", "nsu="+testNamespace+";s="+nodeCount, "int32", time.Second),
		subscribed("temp", "nsu="+testNamespace+";s="+nodeTemp, "float64"),
	)))
	h.start()

	h.waitSamples(0, "count", 1)
	h.waitSamples(0, "temp", 1)
	h.idle()

	second := first.replace(t, serverOptions{namespacesBefore: []string{"urn:laraplate:other"}})
	if second.NamespaceIndex() == first.NamespaceIndex() {
		t.Fatalf("the restarted server kept namespace index %d", first.NamespaceIndex())
	}

	h.waitFor("connection lost", func() bool { return !h.connected() })
	h.idle()

	mark := h.mark()
	h.stepUntil("reconnection", 500*time.Millisecond, time.Minute, h.connected)
	h.waitSamples(mark, "count", 1)
	h.waitSamples(mark, "temp", 1)

	if s := h.samples(mark, "count")[0]; s.Quality != model.Good || s.Value.Int != 1 {
		t.Fatalf("count after the restart = %+v, want good 1 (resolved to index %d)", s, second.NamespaceIndex())
	}
	if s := h.samples(mark, "temp")[0]; s.Quality != model.Good || s.Value.Float != 20 {
		t.Fatalf("temp after the restart = %+v, want good 20", s)
	}
}

// Review Focus: an unknown node and a node of the wrong type among good ones.
func TestUnknownAndWrongTypeNodesAreBadAlone(t *testing.T) {
	srv := sharedTestServer(t)
	h := newHarness(t, opcuaInput(noneConn(srv.URL), 30*time.Second, device("press-07",
		subscribed("temp", srv.address(nodeTemp), "float64"),
		subscribed("missing_sub", srv.address("missing-sub"), "float64"),
		subscribed("wrong_sub", srv.address(nodeTemp), "bool"),
		polled("count", srv.address(nodeCount), "int32", time.Second),
		polled("missing_poll", srv.address("missing-poll"), "float64", time.Second),
		polled("wrong_poll", srv.address(nodeCount), "string", time.Second),
	)))
	h.start()

	want := map[string]model.Value{"temp": {Kind: model.KindFloat, Float: 20}, "count": model.IntValue(1)}
	bad := map[string]model.Value{
		"missing_sub":  {Kind: model.KindFloat},
		"wrong_sub":    model.BoolValue(false),
		"missing_poll": {Kind: model.KindFloat},
		"wrong_poll":   model.StringValue(""),
	}

	for signal := range want {
		h.waitSamples(0, signal, 1)
	}
	for signal := range bad {
		h.waitSamples(0, signal, 1)
	}
	h.idle()
	h.step(time.Second)

	for signal, v := range want {
		for _, s := range h.samples(0, signal) {
			if s.Quality != model.Good || s.Value != v {
				t.Errorf("%s = %+v, want good %v", signal, s, v)
			}
		}
	}
	for signal, v := range bad {
		for _, s := range h.samples(0, signal) {
			if s.Quality != model.Bad || s.Value != v {
				t.Errorf("%s = %+v, want bad %v", signal, s, v)
			}
		}
	}
	if n := len(h.samples(0, "count")); n != 2 {
		t.Fatalf("%d count samples, want 2: the poll went on", n)
	}

	health := h.in.Health()
	if !health.Connected || health.ReadErrors[input.ReadErrorConnection] != 0 ||
		health.ReadErrors[input.ReadErrorException] < 2 || health.ReadErrors[input.ReadErrorDecode] < 2 {
		t.Fatalf("health = %+v, want connected, exceptions and decode errors, no connection error", health)
	}
}

func TestServerLossIsDeathThenBirthWithSessionAndItemsRecreated(t *testing.T) {
	srv := startTestServer(t)
	h := newHarness(t, opcuaInput(noneConn(srv.URL), 5*time.Second, device("press-07",
		subscribed("temp", srv.address(nodeTemp), "float64"),
		polled("count", srv.address(nodeCount), "int32", time.Second),
	)))
	clients := &countedClients{}
	clients.install(h.in)
	h.start()

	h.waitSamples(0, "temp", 1)
	h.waitSamples(0, "count", 1)
	h.waitFor("connected", h.connected)
	h.idle()

	start := h.clk.Now()
	srv.down(t)
	h.waitFor("connection lost", func() bool { return !h.connected() })
	h.idle()

	mark := h.mark()
	for h.clk.Now().Sub(start) < 5*time.Second {
		if got := h.events(mark); len(got) != 0 {
			t.Fatalf("events %v after %s, before the death tolerance", got, h.clk.Now().Sub(start))
		}
		h.step(500 * time.Millisecond)
	}

	if got := h.events(mark); !slices.Equal(got, []string{"death press-07"}) {
		t.Fatalf("events at the death tolerance = %v, want one death", got)
	}
	if n := len(h.samples(mark, "temp")) + len(h.samples(mark, "count")); n != 0 || h.connected() {
		t.Fatalf("%d samples, connected %v while the server is down; want none, false", n, h.connected())
	}

	srv.up()
	reborn := h.mark()
	h.stepUntil("reconnection", 500*time.Millisecond, time.Minute, h.connected)
	h.waitSamples(reborn, "temp", 1)
	h.waitSamples(reborn, "count", 1)

	if got := h.events(reborn); !slices.Equal(got, []string{"birth press-07"}) {
		t.Fatalf("events after recovery = %v, want one birth", got)
	}
	if !h.connected() {
		t.Fatal("not connected after recovery")
	}

	records := h.since(reborn)
	if records[0].Event == nil || records[0].Event.Kind != model.Birth {
		t.Fatalf("first record after recovery = %+v, want the birth before any sample", records[0])
	}

	all := clients.all()
	if _, monitors := all[len(all)-1].sizes(); !slices.Equal(monitors, []int{1}) {
		t.Fatalf("monitored item calls of the new session = %v, want the item created again", monitors)
	}

	assertNothingSecretAboveDebug(t, h.logs, srv.URL, "127.0.0.1", strconv.Itoa(srv.Proxy.Port()))
}

// fakeSession is a session that answers nothing; it records Close.
type fakeSession struct {
	closed atomic.Bool
}

func (f *fakeSession) Limits() (int, int)                 { return limitUnknown, limitUnknown }
func (f *fakeSession) Resolve(n Node) (*ua.NodeID, error) { return n.ID, nil }
func (f *fakeSession) Read(context.Context, []*ua.NodeID) ([]*ua.DataValue, error) {
	return nil, errClosed
}

func (f *fakeSession) Subscribe(context.Context, time.Duration, []item) (<-chan notification, []ua.StatusCode, error) {
	return nil, nil, errClosed
}
func (f *fakeSession) CurrentTime(context.Context) error { return errClosed }
func (f *fakeSession) Browse(context.Context, *ua.NodeID) ([]*ua.ReferenceDescription, error) {
	return nil, errClosed
}
func (f *fakeSession) VariableAttributes(context.Context, []*ua.NodeID) ([]variableAttributes, error) {
	return nil, errClosed
}
func (f *fakeSession) Namespaces() []string { return nil }
func (f *fakeSession) Close()               { f.closed.Store(true) }

// TestCloseDuringAHungConnectReturnsAtOnce (Modbus R10): a connection
// attempt the library does not end is not waited for; the session it opens
// late is closed unused.
func TestCloseDuringAHungConnectReturnsAtOnce(t *testing.T) {
	h := newHarness(t, opcuaInput(noneConn("opc.tcp://127.0.0.1:1"), 30*time.Second, device("press-07",
		polled("count", "i=2258", "int32", time.Second),
	)))

	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	late := &fakeSession{}
	h.in.dial = func(context.Context, Connection, dialDeps) (session, error) {
		entered <- struct{}{}
		<-gate

		return late, nil
	}

	if err := h.in.Start(t.Context(), h.emit); err != nil {
		t.Fatal(err)
	}

	select {
	case <-entered:
	case <-time.After(guard):
		t.Fatal("the input never dialled")
	}

	done := make(chan error, 1)
	go func() { done <- h.in.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(guard):
		close(gate)
		t.Fatal("Close waited for the connection attempt")
	}
	h.closed.Store(true)

	close(gate)
	h.waitFor("late session closed", late.closed.Load)

	if n := h.mark(); n != 0 {
		t.Fatalf("%d records emitted, want none", n)
	}
}

func TestProbeReadsEveryPointOnceWithoutTheLoop(t *testing.T) {
	srv := sharedTestServer(t)
	h := newHarness(t, opcuaInput(noneConn(srv.URL), 30*time.Second,
		device("press-07",
			subscribed("temp", srv.address(nodeTemp), "float64"),
			polled("count", srv.address(nodeCount), "int32", time.Second),
		),
		device("press-08",
			polled("missing", srv.address("missing"), "float64", time.Second),
			subscribed("unknown_ns", "nsu=urn:not:there;s=x", "bool"),
		),
	))

	got, err := h.in.Probe(t.Context())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	want := []struct {
		device, signal string
		value          model.Value
		quality        model.Quality
	}{
		{"press-07", "temp", model.Value{Kind: model.KindFloat, Float: 20}, model.Good},
		{"press-07", "count", model.IntValue(1), model.Good},
		{"press-08", "missing", model.Value{Kind: model.KindFloat}, model.Bad},
		{"press-08", "unknown_ns", model.BoolValue(false), model.Bad},
	}
	if len(got) != len(want) {
		t.Fatalf("Probe = %+v, want %d samples", got, len(want))
	}
	for i, w := range want {
		s := got[i]
		if s.Device != w.device || s.Signal != w.signal || s.Value != w.value || s.Quality != w.quality || s.TS.IsZero() {
			t.Errorf("sample %d = %+v, want %s/%s %v %s", i, s, w.device, w.signal, w.value, w.quality)
		}
	}
	if n := h.clk.PendingTimers(); n != 0 {
		t.Fatalf("%d clock timers after Probe, want none (no loop, no lifecycle)", n)
	}
	if n := h.mark(); n != 0 {
		t.Fatalf("%d records emitted by Probe", n)
	}
}

func TestProbeOfAnUntrustedServerNamesTheThumbprint(t *testing.T) {
	srv := sharedTestServer(t)
	h := newHarness(t, opcuaInput(secureConn(srv.URL, ""), 30*time.Second, device("press-07",
		polled("count", srv.address(nodeCount), "int32", time.Second),
	)))

	_, err := h.in.Probe(t.Context())
	if !errors.Is(err, ErrUntrusted) || !strings.Contains(err.Error(), Thumbprint(srv.Cert)) || !strings.Contains(err.Error(), `opcua input "in1"`) {
		t.Fatalf("Probe = %v, want ErrUntrusted naming the input and thumbprint %s", err, Thumbprint(srv.Cert))
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the error names the endpoint: %v", err)
	}
}

func TestProbeNeedsTheDataDirectory(t *testing.T) {
	cfg := opcuaInput(noneConn("opc.tcp://127.0.0.1:1"), time.Second, device("press-07", polled("count", "i=2258", "int32", time.Second)))

	in, err := New(cfg, clock.NewFake(driverEpoch), Options{AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := in.Probe(t.Context()); err == nil || !strings.Contains(err.Error(), "data_dir") {
		t.Fatalf("Probe without a data directory = %v, want an error naming data_dir", err)
	}
}

// TestNoValueEndpointOrUserInLogsAboveDebug runs a user name input through an
// untrusted certificate, its trust and a connection reading a distinctive
// value, then scans every record above debug.
func TestNoValueEndpointOrUserInLogsAboveDebug(t *testing.T) {
	srv := sharedTestServer(t)
	h := newHarness(t, opcuaInput(secureConn(srv.URL, `{"user":"scan-user-7731","password":"secret://env/SCAN"}`), 5*time.Second, device("press-07",
		subscribed("marker", srv.address(nodeMarker), "float64"),
		polled("marker_polled", srv.address(nodeMarker), "float64", time.Second),
	)))
	h.start()

	h.waitFor("untrusted warning", func() bool { return len(h.warnings("not trusted")) == 1 })
	h.idle()
	h.trust(srv.Cert)
	h.stepUntil("connection", 500*time.Millisecond, time.Minute, h.connected)
	h.waitSamples(0, "marker", 1)
	h.waitSamples(0, "marker_polled", 1)

	if s := h.samples(0, "marker")[0]; s.Value.Float != markerValue {
		t.Fatalf("marker = %+v, want %v", s, markerValue)
	}

	assertNothingSecretAboveDebug(t, h.logs, srv.URL, "127.0.0.1", strconv.Itoa(srv.Proxy.Port()),
		"scan-user-7731", "scan-pass-5521", "4242.125")
}

// TestWatchdogFailureReconnects: a server that stops answering (a half-open
// connection) is found by the watchdog read, which closes the session.
func TestWatchdogFailureReconnects(t *testing.T) {
	srv := startTestServer(t)
	h := newHarness(t, opcuaInput(noneConn(srv.URL), 3*time.Second, device("press-07",
		subscribed("temp", srv.address(nodeTemp), "float64"),
	)))
	short := testTimings
	short.Request = 200 * time.Millisecond
	short.Close = 200 * time.Millisecond
	h.in.timing = &short
	h.start()

	h.waitSamples(0, "temp", 1)
	h.waitFor("connected", h.connected)
	h.idle()

	srv.Proxy.Stall()
	h.step(time.Second)
	h.waitFor("watchdog failure", func() bool { return !h.connected() })

	if n := h.in.Health().ReadErrors[input.ReadErrorTimeout]; n != 1 {
		t.Fatalf("timeouts = %d, want the watchdog read counted", n)
	}

	srv.Proxy.Resume()
	srv.Proxy.Drop()
	h.idle()

	mark := h.mark()
	h.stepUntil("reconnection", 500*time.Millisecond, time.Minute, h.connected)
	h.waitSamples(mark, "temp", 1)
}

// scriptedSession is a session that answers every Read with good values of 1
// unless values holds another data value for the node (by numeric id) or
// readErr is set, answers every watchdog read, and subscribes unless
// subscribeErr is set, with the create statuses of statuses (Good when
// nil). The test pushes notifications on notifyCh.
type scriptedSession struct {
	fakeSession

	subscribeErr error
	readErr      error
	values       map[uint32]*ua.DataValue
	statuses     []ua.StatusCode
	once         sync.Once
	notifyCh     chan notification
}

func newScriptedSession(subscribeErr error) *scriptedSession {
	return &scriptedSession{subscribeErr: subscribeErr, notifyCh: make(chan notification)}
}

func (s *scriptedSession) Read(_ context.Context, ids []*ua.NodeID) ([]*ua.DataValue, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}

	out := make([]*ua.DataValue, len(ids))
	for i, nodeID := range ids {
		out[i] = &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(float64(1))}
		if dv, ok := s.values[nodeID.IntID()]; ok {
			out[i] = dv
		}
	}

	return out, nil
}

func (s *scriptedSession) Subscribe(_ context.Context, _ time.Duration, items []item) (<-chan notification, []ua.StatusCode, error) {
	if s.subscribeErr != nil {
		return nil, nil, s.subscribeErr
	}

	statuses := s.statuses
	if statuses == nil {
		statuses = make([]ua.StatusCode, len(items))
	}

	return s.notifyCh, statuses, nil
}

func (s *scriptedSession) CurrentTime(context.Context) error { return nil }

func (s *scriptedSession) Close() {
	s.closed.Store(true)
	s.once.Do(func() { close(s.notifyCh) })
}

// scriptedDials hands out sessions in order, then the sessions of more, and
// keeps the OnLost and the clock time of each dial.
type scriptedDials struct {
	more func() *scriptedSession
	clk  *clock.Fake

	mu       sync.Mutex
	sessions []*scriptedSession
	onLost   []func()
	at       []time.Time
}

func (d *scriptedDials) install(in *Input) {
	in.dial = func(_ context.Context, _ Connection, deps dialDeps) (session, error) {
		d.mu.Lock()
		defer d.mu.Unlock()

		n := len(d.onLost)
		d.onLost = append(d.onLost, deps.OnLost)
		if d.clk != nil {
			d.at = append(d.at, d.clk.Now())
		}
		if n >= len(d.sessions) {
			d.sessions = append(d.sessions, d.more())
		}

		return d.sessions[n], nil
	}
}

func (d *scriptedDials) times() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()

	return slices.Clone(d.at)
}

func (d *scriptedDials) dialled() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return len(d.onLost)
}

func (d *scriptedDials) lost(n int) {
	d.mu.Lock()
	onLost := d.onLost[n]
	d.mu.Unlock()

	onLost()
}

func scriptedInput(t *testing.T) *harness {
	t.Helper()

	return newHarness(t, opcuaInput(noneConn("opc.tcp://127.0.0.1:1"), 30*time.Second, device("press-07",
		subscribed("state", "i=2258", "float64"),
		polled("count", "i=2259", "float64", time.Second),
	)))
}

func TestSubscribeErrorClosesTheSessionAndReconnects(t *testing.T) {
	h := scriptedInput(t)
	dials := &scriptedDials{sessions: []*scriptedSession{
		newScriptedSession(fmt.Errorf("opcua subscribe: %w", ErrTimeout)),
		newScriptedSession(nil),
	}}
	dials.install(h.in)
	h.start()

	h.waitFor("first session closed", dials.sessions[0].closed.Load)
	h.waitFor("disconnected", func() bool { return !h.connected() })
	h.idle()

	if n := h.in.Health().ReadErrors[input.ReadErrorTimeout]; n != 1 {
		t.Fatalf("timeouts = %d, want the Subscribe error counted once", n)
	}

	h.step(500 * time.Millisecond)

	if !h.connected() || dials.dialled() != 2 || dials.sessions[1].closed.Load() {
		t.Fatalf("connected %v, %d dials, second closed %v; want a second session serving", h.connected(), dials.dialled(), dials.sessions[1].closed.Load())
	}
}

func TestALateOnLostOfAClosedSessionIsIgnored(t *testing.T) {
	h := scriptedInput(t)
	dials := &scriptedDials{sessions: []*scriptedSession{newScriptedSession(nil), newScriptedSession(nil)}}
	dials.install(h.in)
	h.start()

	h.waitFor("connected", h.connected)
	h.idle()

	dials.lost(0)
	h.waitFor("first session closed", dials.sessions[0].closed.Load)
	h.waitFor("disconnected", func() bool { return !h.connected() })
	h.idle()
	h.step(500 * time.Millisecond)

	if !h.connected() || dials.dialled() != 2 {
		t.Fatalf("connected %v after %d dials, want the second session", h.connected(), dials.dialled())
	}

	dials.lost(0)
	dials.lost(0)
	h.step(time.Second)
	h.step(time.Second)

	if dials.sessions[1].closed.Load() || dials.dialled() != 2 || !h.connected() {
		t.Fatalf("second closed %v, %d dials, connected %v: a stale OnLost tore the current session down", dials.sessions[1].closed.Load(), dials.dialled(), h.connected())
	}
	if n := h.in.Health().ReadErrors[input.ReadErrorConnection]; n != 1 {
		t.Fatalf("connection errors = %d, want the one real loss", n)
	}
}

// TestSessionsFailingAtTheFirstReadBackOffAndDie: a server that accepts every
// session and fails its first poll is no successful exchange. The backoff
// keeps growing and the devices die after the tolerance.
func TestSessionsFailingAtTheFirstReadBackOffAndDie(t *testing.T) {
	h := newHarness(t, opcuaInput(noneConn("opc.tcp://127.0.0.1:1"), 5*time.Second, device("press-07",
		polled("count", "i=2259", "float64", time.Second),
	)))
	dials := &scriptedDials{clk: h.clk, more: func() *scriptedSession {
		s := newScriptedSession(nil)
		s.readErr = fmt.Errorf("opcua read: %w", errConnection)

		return s
	}}
	dials.install(h.in)
	h.start()

	h.waitFor("first session closed", func() bool { return dials.dialled() == 1 && !h.connected() })
	h.idle()
	h.stepUntil("six dials", 250*time.Millisecond, time.Minute, func() bool { return dials.dialled() >= 6 })

	at := dials.times()
	var waits []time.Duration
	for i := 1; i < len(at); i++ {
		waits = append(waits, at[i].Sub(at[i-1]))
	}
	if want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}; !slices.Equal(waits[:5], want) {
		t.Fatalf("waits between dials = %v, want the backoff growing %v", waits, want)
	}
	if got := h.events(0); !slices.Equal(got, []string{"death press-07"}) {
		t.Fatalf("events = %v, want one death: no session ever exchanged anything", got)
	}
	if n := len(h.samples(0, "count")); n != 0 {
		t.Fatalf("%d samples, want none", n)
	}
}

// TestExceptionCountsOncePerExchange (ruling R8): kind exception counts once
// per CreateMonitoredItems response, Read response or publish message that
// carries a Bad status; decode counts once per point.
func TestExceptionCountsOncePerExchange(t *testing.T) {
	h := newHarness(t, opcuaInput(noneConn("opc.tcp://127.0.0.1:1"), 30*time.Second, device("press-07",
		subscribed("a", "i=1", "float64"),
		subscribed("b", "i=2", "float64"),
		subscribed("c", "i=3", "float64"),
		polled("p1", "i=4", "float64", time.Second),
		polled("p2", "i=5", "float64", time.Second),
		polled("p3", "i=6", "bool", time.Second),
		polled("p4", "i=7", "string", time.Second),
	)))

	bad := &ua.DataValue{EncodingMask: ua.DataValueStatusCode, Status: ua.StatusBadSensorFailure}
	sess := newScriptedSession(nil)
	sess.statuses = []ua.StatusCode{ua.StatusBadNodeIDUnknown, ua.StatusBadNodeIDUnknown, ua.StatusOK}
	sess.values = map[uint32]*ua.DataValue{4: bad, 5: bad}
	dials := &scriptedDials{sessions: []*scriptedSession{sess}}
	dials.install(h.in)
	h.start()

	h.waitSamples(0, "p4", 1)
	h.idle()

	exceptions := func() uint64 { return h.in.Health().ReadErrors[input.ReadErrorException] }
	decodes := func() uint64 { return h.in.Health().ReadErrors[input.ReadErrorDecode] }

	if exceptions() != 2 || decodes() != 2 {
		t.Fatalf("after the setup and the first poll: exception %d, decode %d; want 2 (one create response, one Read) and 2 (two points)", exceptions(), decodes())
	}
	if got := h.samples(0, "a"); len(got) != 1 || got[0].Quality != model.Bad {
		t.Fatalf("a = %+v, want one bad sample for its refused item", got)
	}

	// Two Bad values in one publish message, then one in the next.
	for _, n := range []notification{{Handle: 2, Message: 1, Value: bad}, {Handle: 2, Message: 1, Value: bad}, {Handle: 2, Message: 2, Value: bad}} {
		sess.notifyCh <- n
	}
	h.waitSamples(0, "c", 3)

	if exceptions() != 4 {
		t.Fatalf("after two publish messages with Bad values: exception %d, want 4", exceptions())
	}

	h.step(time.Second)

	if exceptions() != 5 || decodes() != 4 {
		t.Fatalf("after the second poll: exception %d, decode %d; want 5 and 4", exceptions(), decodes())
	}
}
