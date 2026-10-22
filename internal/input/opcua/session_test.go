package opcua

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/logging"
)

// syncBuffer is a log destination safe for the drain goroutine and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

// records returns the JSON records written so far.
func (b *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()

	b.mu.Lock()
	text := b.buf.String()
	b.mu.Unlock()

	var out []map[string]any

	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}

		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, rec)
	}

	return out
}

// testDeps returns the dial dependencies of input "in1" with a fresh data
// directory and its log destination.
func testDeps(t *testing.T) (dialDeps, *syncBuffer) {
	t.Helper()
	loadFixtures(t)

	logs := &syncBuffer{}

	return dialDeps{
		InputID:  "in1",
		OwnCert:  fixtures.ownDER,
		OwnKey:   fixtures.ownKey,
		Trust:    &TrustStore{Dir: PKIDir(t.TempDir())},
		Rejected: &rejectedSet{},
		Log:      logging.New(logs, slog.LevelDebug),
		timing:   &testTimings,
	}, logs
}

func noneConnection(endpoint string) Connection {
	return Connection{Endpoint: endpoint, SecurityPolicy: PolicyNone, SecurityMode: ModeNone, PublishingInterval: time.Second}
}

func secureConnection(endpoint, mode string) Connection {
	return Connection{Endpoint: endpoint, SecurityPolicy: PolicyBasic256Sha256, SecurityMode: mode, PublishingInterval: time.Second}
}

func mustDial(t *testing.T, conn Connection, deps dialDeps) session {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	s, err := dial(ctx, conn, deps)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(s.Close)

	return s
}

func readOne(t *testing.T, s session, ids ...*ua.NodeID) []*ua.DataValue {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	got, err := s.Read(ctx, ids)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("Read returned %d values for %d ids", len(got), len(ids))
	}

	return got
}

func wantValue(t *testing.T, dv *ua.DataValue, want any) {
	t.Helper()

	if dv == nil || dv.Status != ua.StatusOK || dv.Value == nil || dv.Value.Value() != want {
		t.Fatalf("data value = %+v, want Good %v (%T)", dv, want, want)
	}
}

func TestDialNoneConnectsAndReads(t *testing.T) {
	srv := sharedTestServer(t)
	deps, _ := testDeps(t)

	s := mustDial(t, noneConnection(srv.URL), deps)

	got := readOne(t, s, srv.NodeID(nodeTemp), srv.NodeID(nodeCount))
	wantValue(t, got[0], float64(20))
	wantValue(t, got[1], int32(1))

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	if err := s.CurrentTime(ctx); err != nil {
		t.Fatalf("CurrentTime: %v", err)
	}
}

func TestDialSecureConnectsOnlyWhenTrusted(t *testing.T) {
	for _, mode := range []string{ModeSignAndEncrypt, ModeSign} {
		t.Run(mode, func(t *testing.T) {
			srv := sharedTestServer(t)
			deps, _ := testDeps(t)
			conn := secureConnection(srv.URL, mode)

			ctx, cancel := context.WithTimeout(context.Background(), guard)
			defer cancel()

			if s, err := dial(ctx, conn, deps); !errors.Is(err, ErrUntrusted) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("dial before trust = %v, want ErrUntrusted", err)
			}

			if _, err := deps.Trust.Trust(srv.Cert); err != nil {
				t.Fatalf("Trust: %v", err)
			}

			s := mustDial(t, conn, deps)
			wantValue(t, readOne(t, s, srv.NodeID(nodeTemp))[0], float64(20))
		})
	}
}

func TestDialUntrustedRejectsAndWarnsOnce(t *testing.T) {
	srv := sharedTestServer(t)
	deps, logs := testDeps(t)
	conn := secureConnection(srv.URL, ModeSignAndEncrypt)
	thumb := Thumbprint(srv.Cert)

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	for attempt := range 2 {
		_, err := dial(ctx, conn, deps)

		var untrusted *UntrustedError
		if !errors.As(err, &untrusted) || !errors.Is(err, ErrUntrusted) || untrusted.Thumbprint != thumb {
			t.Fatalf("attempt %d: dial = %v, want an UntrustedError with thumbprint %s", attempt, err, thumb)
		}
		if strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("error names the endpoint: %v", err)
		}
	}

	rejected, err := os.ReadFile(filepath.Join(deps.Trust.Dir, "rejected", thumb+".der"))
	if err != nil || !bytes.Equal(rejected, srv.Cert) {
		t.Fatalf("rejected/%s.der = %d bytes, %v; want the server certificate", thumb, len(rejected), err)
	}
	if deps.Trust.Trusted(thumb) {
		t.Fatal("a rejected certificate is trusted")
	}

	var warns []map[string]any

	for _, rec := range logs.records(t) {
		line, _ := json.Marshal(rec)
		if rec["level"] != "DEBUG" && strings.Contains(string(line), "127.0.0.1") {
			t.Fatalf("a record above debug names the endpoint: %s", line)
		}
		if rec["level"] == "WARN" {
			warns = append(warns, rec)
		}
	}

	if len(warns) != 1 {
		t.Fatalf("got %d WARN records, want one per thumbprint: %v", len(warns), warns)
	}

	want := "input in1: server certificate not trusted, thumbprint " + thumb + "; trust it with: laraplate-edge opcua trust in1"
	if warns[0]["msg"] != want || warns[0]["thumbprint"] != thumb || warns[0]["input"] != "in1" {
		t.Fatalf("WARN = %v, want message %q", warns[0], want)
	}
}

func TestDialUserNameResolvesThePasswordAtEveryDial(t *testing.T) {
	srv := sharedTestServer(t)
	deps, logs := testDeps(t)

	if _, err := deps.Trust.Trust(srv.Cert); err != nil {
		t.Fatalf("Trust: %v", err)
	}

	const password = "pw-c4f0451-not-logged"

	var calls atomic.Int32

	deps.Password = func(context.Context) (string, error) {
		calls.Add(1)

		return password, nil
	}

	conn := secureConnection(srv.URL, ModeSignAndEncrypt)
	conn.User = "operator"
	conn.Password = "secret://env/OPCUA_PASSWORD"

	for range 2 {
		s := mustDial(t, conn, deps)
		wantValue(t, readOne(t, s, srv.NodeID(nodeTemp))[0], float64(20))
		s.Close()
	}

	if calls.Load() != 2 {
		t.Fatalf("password resolved %d times, want once per dial", calls.Load())
	}

	for _, rec := range logs.records(t) {
		line, _ := json.Marshal(rec)
		if strings.Contains(string(line), password) || strings.Contains(string(line), "operator") {
			t.Fatalf("a log record carries the user or the password: %s", line)
		}
	}
}

func TestDialPasswordErrorIsAConnectionError(t *testing.T) {
	srv := sharedTestServer(t)
	deps, _ := testDeps(t)

	if _, err := deps.Trust.Trust(srv.Cert); err != nil {
		t.Fatalf("Trust: %v", err)
	}

	deps.Password = func(context.Context) (string, error) {
		return "", errors.New("secret secret://env/X: environment variable X is not set or empty")
	}

	conn := secureConnection(srv.URL, ModeSignAndEncrypt)
	conn.User = "operator"
	conn.Password = "secret://env/X"

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	if _, err := dial(ctx, conn, deps); !errors.Is(err, errPassword) || errors.Is(err, ErrTimeout) {
		t.Fatalf("dial = %v, want errPassword", err)
	}
}

func TestDialUnreachableNamesNoEndpoint(t *testing.T) {
	deps, logs := testDeps(t)
	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	endpoint := "opc.tcp://127.0.0.1:" + strconv.Itoa(port)

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	for _, conn := range []Connection{noneConnection(endpoint), secureConnection(endpoint, ModeSignAndEncrypt)} {
		_, err := dial(ctx, conn, deps)
		if err == nil || errors.Is(err, ErrUntrusted) {
			t.Fatalf("%s: dial = %v, want a connection error", conn.SecurityPolicy, err)
		}
		if strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("%s: error names the endpoint: %v", conn.SecurityPolicy, err)
		}
	}

	for _, rec := range logs.records(t) {
		line, _ := json.Marshal(rec)
		if rec["level"] != "DEBUG" && strings.Contains(string(line), "127.0.0.1") {
			t.Fatalf("a record above debug names the endpoint: %s", line)
		}
	}
}

func TestDialCanceledContext(t *testing.T) {
	deps, _ := testDeps(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := dial(ctx, noneConnection("opc.tcp://127.0.0.1:4840"), deps); !errors.Is(err, context.Canceled) {
		t.Fatalf("dial = %v, want context.Canceled", err)
	}
}

func TestDiscover(t *testing.T) {
	srv := sharedTestServer(t)

	tests := []struct {
		name       string
		conn       Connection
		wantPolicy string
		wantMode   ua.MessageSecurityMode
		wantErr    bool
	}{
		{name: "none", conn: noneConnection(srv.URL), wantPolicy: ua.SecurityPolicyURINone, wantMode: ua.MessageSecurityModeNone},
		{name: "sign", conn: secureConnection(srv.URL, ModeSign), wantPolicy: ua.SecurityPolicyURIBasic256Sha256, wantMode: ua.MessageSecurityModeSign},
		{name: "sign and encrypt", conn: secureConnection(srv.URL, ModeSignAndEncrypt), wantPolicy: ua.SecurityPolicyURIBasic256Sha256, wantMode: ua.MessageSecurityModeSignAndEncrypt},
		{
			name:    "policy the server lacks",
			conn:    Connection{Endpoint: srv.URL, SecurityPolicy: PolicyAes256Sha256RsaPss, SecurityMode: ModeSignAndEncrypt},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), guard)
			defer cancel()

			ep, err := discover(ctx, tt.conn)
			if tt.wantErr {
				if !errors.Is(err, errNoEndpoint) {
					t.Fatalf("discover = %v, %v; want errNoEndpoint", ep, err)
				}

				return
			}
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			if ep.SecurityPolicyURI != tt.wantPolicy || ep.SecurityMode != tt.wantMode || !bytes.Equal(ep.ServerCertificate, srv.Cert) {
				t.Fatalf("endpoint = %s %s, want %s %s with the server certificate", ep.SecurityPolicyURI, ep.SecurityMode, tt.wantPolicy, tt.wantMode)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	srv := sharedTestServer(t)
	deps, _ := testDeps(t)
	s := mustDial(t, noneConnection(srv.URL), deps)

	numeric := ua.NewNumericNodeID(0, id.Server_ServerStatus_CurrentTime)
	if got, err := s.Resolve(Node{ID: numeric}); err != nil || got != numeric {
		t.Fatalf("Resolve(ID) = %v, %v; want the same NodeId", got, err)
	}

	got, err := s.Resolve(Node{NamespaceURI: testNamespace, Rest: "s=" + nodeTemp})
	if err != nil {
		t.Fatalf("Resolve(nsu=): %v", err)
	}
	if got.Namespace() != srv.NamespaceIndex() || got.StringID() != nodeTemp {
		t.Fatalf("Resolve(nsu=) = %s, want ns=%d;s=%s", got, srv.NamespaceIndex(), nodeTemp)
	}
	wantValue(t, readOne(t, s, got)[0], float64(20))

	if _, err := s.Resolve(Node{NamespaceURI: "urn:not:there", Rest: "s=x"}); !errors.Is(err, ErrUnknownNamespace) {
		t.Fatalf("Resolve(unknown uri) = %v, want ErrUnknownNamespace", err)
	}
}

// countingClient wraps a client and records the size of every Read and
// Monitor request. A non-zero maxMonitored replaces the server's
// MaxMonitoredItemsPerCall in the limits read.
type countingClient struct {
	uaClient

	maxMonitored uint32

	mu       sync.Mutex
	reads    []int
	monitors []int
}

func (c *countingClient) Read(ctx context.Context, req *ua.ReadRequest) (*ua.ReadResponse, error) {
	c.mu.Lock()
	c.reads = append(c.reads, len(req.NodesToRead))
	c.mu.Unlock()

	res, err := c.uaClient.Read(ctx, req)
	if err == nil && c.maxMonitored != 0 {
		for i, rv := range req.NodesToRead {
			if rv.NodeID.IntID() == id.Server_ServerCapabilities_OperationLimits_MaxMonitoredItemsPerCall {
				res.Results[i] = &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(c.maxMonitored)}
			}
		}
	}

	return res, err
}

func (c *countingClient) Subscribe(ctx context.Context, p *opcua.SubscriptionParameters, ch chan<- *opcua.PublishNotificationData) (uaSubscription, error) {
	sub, err := c.uaClient.Subscribe(ctx, p, ch)
	if err != nil {
		return nil, err
	}

	return countingSubscription{uaSubscription: sub, c: c}, nil
}

func (c *countingClient) sizes() (reads, monitors []int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.reads), slices.Clone(c.monitors)
}

func (c *countingClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.reads, c.monitors = nil, nil
}

type countingSubscription struct {
	uaSubscription

	c *countingClient
}

func (s countingSubscription) Monitor(ctx context.Context, ts ua.TimestampsToReturn, items ...*ua.MonitoredItemCreateRequest) (*ua.CreateMonitoredItemsResponse, error) {
	s.c.mu.Lock()
	s.c.monitors = append(s.c.monitors, len(items))
	s.c.mu.Unlock()

	return s.uaSubscription.Monitor(ctx, ts, items...)
}

// counting makes deps build the library client inside c.
func counting(deps *dialDeps, c *countingClient) {
	deps.newClient = func(endpoint string, opts []opcua.Option, onState func(opcua.ConnState)) (uaClient, error) {
		inner, err := newLibraryClient(endpoint, opts, onState)
		c.uaClient = inner

		return c, err
	}
}

func TestLimitsAreReadAfterConnect(t *testing.T) {
	srv := sharedTestServer(t)
	deps, _ := testDeps(t)
	cc := &countingClient{}
	counting(&deps, cc)

	s := mustDial(t, noneConnection(srv.URL), deps)

	if maxRead, maxMonitored := s.Limits(); maxRead != 32 || maxMonitored != limitUnknown {
		t.Fatalf("Limits() = %d, %d; want 32 (the server's MaxNodesPerRead) and %d (MaxMonitoredItemsPerCall is not on the test server)", maxRead, maxMonitored, limitUnknown)
	}
	if reads, _ := cc.sizes(); !slices.Equal(reads, []int{1, 1}) {
		t.Fatalf("limits read sizes = %v, want two single-node requests (ruling R6)", reads)
	}
}

func TestReadChunksAtMaxNodesPerRead(t *testing.T) {
	srv := sharedTestServer(t)
	deps, _ := testDeps(t)
	cc := &countingClient{}
	counting(&deps, cc)

	s := mustDial(t, noneConnection(srv.URL), deps)

	tests := []struct {
		ids  int
		want []int
	}{
		{ids: 70, want: []int{32, 32, 6}},
		{ids: 64, want: []int{32, 32}},
		{ids: 1, want: []int{1}},
		{ids: 0, want: nil},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.ids), func(t *testing.T) {
			cc.reset()

			ids := make([]*ua.NodeID, tt.ids)
			for i := range ids {
				if i%2 == 0 {
					ids[i] = srv.NodeID(nodeTemp)
				} else {
					ids[i] = srv.NodeID(nodeCount)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), guard)
			defer cancel()

			got, err := s.Read(ctx, ids)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(got) != tt.ids {
				t.Fatalf("Read returned %d values, want %d", len(got), tt.ids)
			}
			for i, dv := range got {
				if i%2 == 0 {
					wantValue(t, dv, float64(20))
				} else {
					wantValue(t, dv, int32(1))
				}
			}

			if reads, _ := cc.sizes(); !slices.Equal(reads, tt.want) {
				t.Fatalf("request sizes = %v, want %v", reads, tt.want)
			}
		})
	}
}

// nextNotification waits for a notification of handle whose value is want.
func nextNotification(t *testing.T, ch <-chan notification, handle uint32, want any) {
	t.Helper()

	timeout := time.After(guard)

	for {
		select {
		case n, ok := <-ch:
			if !ok {
				t.Fatalf("notification channel closed while waiting for handle %d = %v", handle, want)
			}
			if n.Handle == handle && n.Value != nil && n.Value.Value != nil && n.Value.Value.Value() == want {
				return
			}
		case <-timeout:
			t.Fatalf("no notification of handle %d = %v", handle, want)
		}
	}
}

func TestSubscribeDeliversInitialValuesAndChanges(t *testing.T) {
	srv := sharedTestServer(t)
	deps, _ := testDeps(t)
	s := mustDial(t, noneConnection(srv.URL), deps)

	unknown := ua.NewStringNodeID(srv.NamespaceIndex(), "missing")
	items := []item{
		{Handle: 1, Node: srv.NodeID(nodeTemp), Sub: config.Subscribe{Sampling: config.SamplingDefault, Deadband: "0.5"}},
		{Handle: 2, Node: srv.NodeID(nodeCount), Sub: config.Subscribe{Sampling: 100 * time.Millisecond, Queue: 5}},
		{Handle: 3, Node: unknown, Sub: config.Subscribe{Sampling: config.SamplingDefault, Deadband: "10%"}},
		{Handle: 4, Node: srv.NodeID(nodeChanging), Sub: config.Subscribe{Sampling: config.SamplingDefault}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	ch, statuses, err := s.Subscribe(ctx, 50*time.Millisecond, items)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(statuses) != len(items) {
		t.Fatalf("statuses = %v, want one per item", statuses)
	}

	initial := map[uint32]any{}
	timeout := time.After(guard)

	for len(initial) < 2 {
		select {
		case n := <-ch:
			if (n.Handle == 1 || n.Handle == 2) && n.Value != nil && n.Value.Value != nil {
				initial[n.Handle] = n.Value.Value.Value()
			}
		case <-timeout:
			t.Fatalf("initial values = %v, want handles 1 and 2", initial)
		}
	}

	if initial[1] != float64(20) || initial[2] != int32(1) {
		t.Fatalf("initial values = %v, want 1: 20, 2: 1", initial)
	}

	changed := float64(time.Now().UnixNano())
	srv.Set(t, nodeChanging, changed)
	nextNotification(t, ch, 4, changed)

	if _, _, err := s.Subscribe(ctx, time.Second, items); err == nil {
		t.Fatal("a second Subscribe on one session succeeded")
	}

	s.Close()

	timeout = time.After(guard)

	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("Close did not close the notification channel")
		}
	}
}

func TestSubscribeChunksAtMaxMonitoredItemsPerCall(t *testing.T) {
	srv := sharedTestServer(t)
	deps, _ := testDeps(t)
	cc := &countingClient{maxMonitored: 3}
	counting(&deps, cc)

	s := mustDial(t, noneConnection(srv.URL), deps)

	if _, maxMonitored := s.Limits(); maxMonitored != 3 {
		t.Fatalf("maxMonitored = %d, want 3", maxMonitored)
	}

	items := make([]item, 7)
	for i := range items {
		items[i] = item{Handle: uint32(i + 1), Node: srv.NodeID(nodeTemp), Sub: config.Subscribe{Sampling: config.SamplingDefault}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	_, statuses, err := s.Subscribe(ctx, 50*time.Millisecond, items)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(statuses) != len(items) {
		t.Fatalf("statuses = %v, want %d", statuses, len(items))
	}
	for i, st := range statuses {
		if st != ua.StatusOK {
			t.Fatalf("status %d = %v, want Good", i, st)
		}
	}

	if _, monitors := cc.sizes(); !slices.Equal(monitors, []int{3, 3, 1}) {
		t.Fatalf("monitor sizes = %v, want [3 3 1]", monitors)
	}
}

func TestSubscribeWithoutItemsSendsNothing(t *testing.T) {
	srv := sharedTestServer(t)
	deps, _ := testDeps(t)
	cc := &countingClient{}
	counting(&deps, cc)

	s := mustDial(t, noneConnection(srv.URL), deps)

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	if _, _, err := s.Subscribe(ctx, time.Second, nil); err == nil {
		t.Fatal("Subscribe without items succeeded")
	}
	if _, monitors := cc.sizes(); len(monitors) != 0 {
		t.Fatalf("monitor requests = %v, want none", monitors)
	}
}

// stateRecorder forwards the library's states to the session and records
// them, so a test can wait until the client reached a state.
type stateRecorder struct {
	states chan opcua.ConnState
}

func recordStates(deps *dialDeps) *stateRecorder {
	r := &stateRecorder{states: make(chan opcua.ConnState, 64)}
	deps.newClient = func(endpoint string, opts []opcua.Option, onState func(opcua.ConnState)) (uaClient, error) {
		return newLibraryClient(endpoint, opts, func(s opcua.ConnState) {
			onState(s)
			r.states <- s
		})
	}

	return r
}

func (r *stateRecorder) waitFor(t *testing.T, want opcua.ConnState) {
	t.Helper()

	timeout := time.After(guard)

	for {
		select {
		case s := <-r.states:
			if s == want {
				return
			}
		case <-timeout:
			t.Fatalf("client never reached state %v", want)
		}
	}
}

// lossCounter is an OnLost that counts its calls and signals the first.
type lossCounter struct {
	calls atomic.Int32
	first chan struct{}
	once  sync.Once
}

func newLossCounter(deps *dialDeps) *lossCounter {
	c := &lossCounter{first: make(chan struct{})}
	deps.OnLost = func() {
		c.calls.Add(1)
		c.once.Do(func() { close(c.first) })
	}

	return c
}

func (c *lossCounter) wait(t *testing.T) {
	t.Helper()

	select {
	case <-c.first:
	case <-time.After(guard):
		t.Fatal("OnLost was not called")
	}
}

func TestProxyDropCallsOnLostOnce(t *testing.T) {
	srv := startTestServer(t)
	deps, _ := testDeps(t)
	lost := newLossCounter(&deps)
	states := recordStates(&deps)

	s := mustDial(t, noneConnection(srv.URL), deps)
	states.waitFor(t, opcua.Connected)

	srv.Proxy.Drop()

	lost.wait(t)
	states.waitFor(t, opcua.Closed)
	s.Close()

	if n := lost.calls.Load(); n != 1 {
		t.Fatalf("OnLost called %d times, want once", n)
	}
}

func TestCloseDoesNotCallOnLost(t *testing.T) {
	srv := sharedTestServer(t)
	deps, _ := testDeps(t)
	lost := newLossCounter(&deps)
	states := recordStates(&deps)

	s := mustDial(t, noneConnection(srv.URL), deps)
	s.Close()
	s.Close()
	states.waitFor(t, opcua.Closed)

	if n := lost.calls.Load(); n != 0 {
		t.Fatalf("OnLost called %d times after Close, want never", n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	if _, err := s.Read(ctx, []*ua.NodeID{srv.NodeID(nodeTemp)}); !errors.Is(err, errClosed) {
		t.Fatalf("Read after Close = %v, want errClosed", err)
	}
}

// stalledRead makes deps build the library client inside a wrapper that
// signals each Read before sending it.
func stalledRead(deps *dialDeps) <-chan struct{} {
	started := make(chan struct{}, 16)
	deps.newClient = func(endpoint string, opts []opcua.Option, onState func(opcua.ConnState)) (uaClient, error) {
		inner, err := newLibraryClient(endpoint, opts, onState)
		if err != nil {
			return nil, err
		}

		return readSignal{uaClient: inner, started: started}, nil
	}

	return started
}

type readSignal struct {
	uaClient

	started chan struct{}
}

func (c readSignal) Read(ctx context.Context, req *ua.ReadRequest) (*ua.ReadResponse, error) {
	select {
	case c.started <- struct{}{}:
	default:
	}

	return c.uaClient.Read(ctx, req)
}

func TestCloseWhileAReadIsInFlight(t *testing.T) {
	srv := startTestServer(t)
	deps, _ := testDeps(t)
	slow := testTimings
	slow.Request = time.Minute // only Close can end the stalled Read
	slow.Close = 200 * time.Millisecond
	deps.timing = &slow
	lost := newLossCounter(&deps)
	started := stalledRead(&deps)

	s := mustDial(t, noneConnection(srv.URL), deps)

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	ch, _, err := s.Subscribe(ctx, 50*time.Millisecond, []item{{Handle: 1, Node: srv.NodeID(nodeTemp), Sub: config.Subscribe{Sampling: config.SamplingDefault}}})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for len(started) > 0 {
		<-started
	}

	srv.Proxy.Stall()
	defer srv.Proxy.Resume()

	readErr := make(chan error, 1)

	go func() {
		_, err := s.Read(context.Background(), []*ua.NodeID{srv.NodeID(nodeTemp)})
		readErr <- err
	}()

	select {
	case <-started:
	case <-time.After(guard):
		t.Fatal("the Read did not start")
	}

	closed := make(chan struct{})

	go func() {
		s.Close()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(guard):
		t.Fatal("Close did not return while a Read was in flight")
	}

	select {
	case err := <-readErr:
		if !errors.Is(err, errClosed) {
			t.Fatalf("in-flight Read = %v, want errClosed", err)
		}
	case <-time.After(guard):
		t.Fatal("the in-flight Read did not return after Close")
	}

	timeout := time.After(guard)

	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if n := lost.calls.Load(); n != 0 {
					t.Fatalf("OnLost called %d times by Close", n)
				}

				return
			}
		case <-timeout:
			t.Fatal("the drain goroutine did not end: notification channel still open")
		}
	}
}

func TestOnLostNeverForAFailedDial(t *testing.T) {
	deps, _ := testDeps(t)
	lost := newLossCounter(&deps)
	withFake(&deps, &fakeClient{lossOnConnect: true, failLimits: true})

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	if _, err := dial(ctx, noneConnection("opc.tcp://fake:4840"), deps); err == nil {
		t.Fatal("dial succeeded with a failing limits read")
	}

	// The watcher is never armed, so no OnLost goroutine exists to wait for.
	if n := lost.calls.Load(); n != 0 {
		t.Fatalf("OnLost called %d times for a failed dial", n)
	}
}

func TestOnLostAtMostOncePerSession(t *testing.T) {
	deps, _ := testDeps(t)
	lost := newLossCounter(&deps)
	f := &fakeClient{}
	withFake(&deps, f)

	s := mustDial(t, noneConnection("opc.tcp://fake:4840"), deps)

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	ch, _, err := s.Subscribe(ctx, time.Second, fakeItems)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	f.publish(t, &opcua.PublishNotificationData{Value: &ua.StatusChangeNotification{Status: ua.StatusBadTimeout}})
	lost.wait(t)

	f.publish(t, &opcua.PublishNotificationData{Error: ua.StatusBadSubscriptionIDInvalid})
	f.onState(opcua.Disconnected)
	f.onState(opcua.Closed)

	// The data change follows the second loss message, so both were handled.
	f.publish(t, &opcua.PublishNotificationData{Value: &ua.DataChangeNotification{
		MonitoredItems: []*ua.MonitoredItemNotification{{ClientHandle: 1, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(float64(3))}}},
	}})
	nextNotification(t, ch, 1, float64(3))

	if n := lost.calls.Load(); n != 1 {
		t.Fatalf("OnLost called %d times, want once per session", n)
	}
}

// TestLateOnLostIsBoundToItsSession shows the documented window: a loss
// detected before Close reaches OnLost after Close returned, and the caller
// tells it apart by the generation its per-dial closure carries.
func TestLateOnLostIsBoundToItsSession(t *testing.T) {
	type call struct{ generation int }

	calls := make(chan call, 4)
	entered := make(chan struct{})
	gate := make(chan struct{})

	onLost := func(generation int, hold bool) func() {
		return func() {
			if hold {
				close(entered)
				<-gate
			}
			calls <- call{generation: generation}
		}
	}

	depsA, _ := testDeps(t)
	depsA.OnLost = onLost(1, true)
	fakeA := &fakeClient{}
	withFake(&depsA, fakeA)
	a := mustDial(t, noneConnection("opc.tcp://fake:4840"), depsA)

	fakeA.onState(opcua.Disconnected)

	select {
	case <-entered:
	case <-time.After(guard):
		t.Fatal("OnLost of the first session did not start")
	}

	a.Close()

	depsB, _ := testDeps(t)
	depsB.OnLost = onLost(2, false)
	withFake(&depsB, &fakeClient{})
	mustDial(t, noneConnection("opc.tcp://fake:4840"), depsB)
	current := 2

	close(gate)

	select {
	case c := <-calls:
		if c.generation == current {
			t.Fatalf("the late OnLost carries the current generation %d", current)
		}
		if c.generation != 1 {
			t.Fatalf("late OnLost generation = %d, want 1", c.generation)
		}
	case <-time.After(guard):
		t.Fatal("the late OnLost never ran")
	}

	select {
	case c := <-calls:
		t.Fatalf("unexpected OnLost of generation %d", c.generation)
	default:
	}
}

func TestCurrentTimeTimesOutWhenTheProxyStalls(t *testing.T) {
	srv := startTestServer(t)
	deps, _ := testDeps(t)
	short := testTimings
	short.Request = 200 * time.Millisecond
	short.Close = 200 * time.Millisecond
	deps.timing = &short

	s := mustDial(t, noneConnection(srv.URL), deps)

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	if err := s.CurrentTime(ctx); err != nil {
		t.Fatalf("CurrentTime before the stall: %v", err)
	}

	srv.Proxy.Stall()
	defer srv.Proxy.Resume()

	if err := s.CurrentTime(ctx); !errors.Is(err, ErrTimeout) {
		t.Fatalf("CurrentTime while stalled = %v, want ErrTimeout", err)
	}
}

// fakeClient is a library client without a server; panicAt names the call
// that panics: connect, read (not the limits read), subscribe or monitor.
type fakeClient struct {
	panicAt string
	onState func(opcua.ConnState)
	// lossOnConnect reports Disconnected right after Connected; failLimits
	// fails the limits read with a service error.
	lossOnConnect bool
	failLimits    bool
	// browseFn and browseNextFn answer Browse and BrowseNext; nil means
	// unsupported.
	browseFn     func(*ua.BrowseRequest) (*ua.BrowseResponse, error)
	browseNextFn func(*ua.BrowseNextRequest) (*ua.BrowseNextResponse, error)

	mu     sync.Mutex
	notify chan<- *opcua.PublishNotificationData
	// params are the parameters of the last Subscribe.
	params opcua.SubscriptionParameters
	// dataTypeReads are the sizes of the DataType reads.
	dataTypeReads []int
}

func (f *fakeClient) Connect(context.Context) error {
	if f.panicAt == "connect" {
		panic("injected connect panic")
	}
	f.onState(opcua.Connected)
	if f.lossOnConnect {
		f.onState(opcua.Disconnected)
	}

	return nil
}

func (f *fakeClient) Close(context.Context) error {
	f.onState(opcua.Closed)

	return nil
}

func (f *fakeClient) Read(_ context.Context, req *ua.ReadRequest) (*ua.ReadResponse, error) {
	res := &ua.ReadResponse{}
	limits := false

	if len(req.NodesToRead) > 0 && req.NodesToRead[0].AttributeID == ua.AttributeIDDataType {
		f.mu.Lock()
		f.dataTypeReads = append(f.dataTypeReads, len(req.NodesToRead))
		f.mu.Unlock()
	}

	for _, rv := range req.NodesToRead {
		switch rv.NodeID.IntID() {
		case id.Server_ServerCapabilities_OperationLimits_MaxNodesPerRead, id.Server_ServerCapabilities_OperationLimits_MaxMonitoredItemsPerCall:
			limits = true
			res.Results = append(res.Results, &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(uint32(32))})
		default:
			res.Results = append(res.Results, &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(float64(1))})
		}
	}

	if f.panicAt == "read" && !limits {
		panic("injected read panic")
	}
	if f.failLimits && limits {
		return nil, ua.StatusBadCommunicationError
	}

	return res, nil
}

func (f *fakeClient) Browse(_ context.Context, req *ua.BrowseRequest) (*ua.BrowseResponse, error) {
	if f.browseFn == nil {
		return nil, ua.StatusBadServiceUnsupported
	}

	return f.browseFn(req)
}

func (f *fakeClient) BrowseNext(_ context.Context, req *ua.BrowseNextRequest) (*ua.BrowseNextResponse, error) {
	if f.browseNextFn == nil {
		return nil, ua.StatusBadServiceUnsupported
	}

	return f.browseNextFn(req)
}

func (f *fakeClient) Subscribe(_ context.Context, p *opcua.SubscriptionParameters, ch chan<- *opcua.PublishNotificationData) (uaSubscription, error) {
	if f.panicAt == "subscribe" {
		panic("injected subscribe panic")
	}

	f.mu.Lock()
	f.notify = ch
	f.params = *p
	f.mu.Unlock()

	return fakeSubscription{f: f}, nil
}

func (f *fakeClient) Namespaces() []string {
	return []string{"http://opcfoundation.org/UA/", testNamespace}
}

func (f *fakeClient) publish(t *testing.T, data *opcua.PublishNotificationData) {
	t.Helper()

	f.mu.Lock()
	ch := f.notify
	f.mu.Unlock()

	select {
	case ch <- data:
	case <-time.After(guard):
		t.Fatal("the session does not drain the library channel")
	}
}

type fakeSubscription struct {
	f *fakeClient
}

func (s fakeSubscription) Monitor(_ context.Context, _ ua.TimestampsToReturn, items ...*ua.MonitoredItemCreateRequest) (*ua.CreateMonitoredItemsResponse, error) {
	if s.f.panicAt == "monitor" {
		panic("injected monitor panic")
	}

	res := &ua.CreateMonitoredItemsResponse{}
	for range items {
		res.Results = append(res.Results, &ua.MonitoredItemCreateResult{StatusCode: ua.StatusOK})
	}

	return res, nil
}

func withFake(deps *dialDeps, f *fakeClient) {
	deps.newClient = func(_ string, _ []opcua.Option, onState func(opcua.ConnState)) (uaClient, error) {
		f.onState = onState

		return f, nil
	}
}

var fakeItems = []item{{Handle: 1, Node: ua.NewStringNodeID(1, "x"), Sub: config.Subscribe{Sampling: config.SamplingDefault}}}

func TestLibraryPanicsBecomeErrors(t *testing.T) {
	tests := []struct {
		panicAt string
		call    func(ctx context.Context, s session) error
	}{
		{panicAt: "connect"},
		{panicAt: "read", call: func(ctx context.Context, s session) error {
			_, err := s.Read(ctx, []*ua.NodeID{ua.NewStringNodeID(1, "x")})

			return err
		}},
		{panicAt: "read", call: func(ctx context.Context, s session) error { return s.CurrentTime(ctx) }},
		{panicAt: "subscribe", call: func(ctx context.Context, s session) error {
			_, _, err := s.Subscribe(ctx, time.Second, fakeItems)

			return err
		}},
		{panicAt: "monitor", call: func(ctx context.Context, s session) error {
			_, _, err := s.Subscribe(ctx, time.Second, fakeItems)

			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.panicAt, func(t *testing.T) {
			deps, _ := testDeps(t)
			withFake(&deps, &fakeClient{panicAt: tt.panicAt})

			ctx, cancel := context.WithTimeout(context.Background(), guard)
			defer cancel()

			s, err := dial(ctx, noneConnection("opc.tcp://fake:4840"), deps)
			if tt.call == nil {
				if !errors.Is(err, errLibraryPanic) {
					t.Fatalf("dial = %v, want errLibraryPanic", err)
				}

				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer s.Close()

			if err := tt.call(ctx, s); !errors.Is(err, errLibraryPanic) {
				t.Fatalf("call = %v, want errLibraryPanic", err)
			}
		})
	}
}

func TestSubscriptionLossCallsOnLost(t *testing.T) {
	tests := []struct {
		name string
		data *opcua.PublishNotificationData
		lost bool
	}{
		{name: "status change", data: &opcua.PublishNotificationData{Value: &ua.StatusChangeNotification{Status: ua.StatusBadTimeout}}, lost: true},
		{name: "publish fault", data: &opcua.PublishNotificationData{Error: ua.StatusBadSubscriptionIDInvalid}, lost: true},
		{name: "malformed message", data: &opcua.PublishNotificationData{Error: errors.New("missing NotificationData parameter")}},
		{name: "events", data: &opcua.PublishNotificationData{Value: &ua.EventNotificationList{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, _ := testDeps(t)
			lost := newLossCounter(&deps)
			f := &fakeClient{}
			withFake(&deps, f)

			s := mustDial(t, noneConnection("opc.tcp://fake:4840"), deps)

			ctx, cancel := context.WithTimeout(context.Background(), guard)
			defer cancel()

			ch, _, err := s.Subscribe(ctx, time.Second, fakeItems)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}

			f.publish(t, tt.data)

			if tt.lost {
				lost.wait(t)

				return
			}

			// A data change published after the message proves the message
			// was handled, without OnLost.
			f.publish(t, &opcua.PublishNotificationData{Value: &ua.DataChangeNotification{
				MonitoredItems: []*ua.MonitoredItemNotification{{ClientHandle: 1, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(float64(7))}}},
			}})
			nextNotification(t, ch, 1, float64(7))

			if n := lost.calls.Load(); n != 0 {
				t.Fatalf("OnLost called %d times, want never", n)
			}
		})
	}
}

// TestNotificationsCarryTheirPublishMessage: the data changes of one publish
// message share its number, so the driver counts a Bad status once per
// message (ruling R8).
func TestNotificationsCarryTheirPublishMessage(t *testing.T) {
	deps, _ := testDeps(t)
	f := &fakeClient{}
	withFake(&deps, f)

	s := mustDial(t, noneConnection("opc.tcp://fake:4840"), deps)

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()

	ch, _, err := s.Subscribe(ctx, time.Second, fakeItems)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	change := func(handles ...uint32) *opcua.PublishNotificationData {
		var items []*ua.MonitoredItemNotification
		for _, h := range handles {
			items = append(items, &ua.MonitoredItemNotification{ClientHandle: h, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(float64(h))}})
		}

		return &opcua.PublishNotificationData{Value: &ua.DataChangeNotification{MonitoredItems: items}}
	}

	f.publish(t, change(1, 2))
	f.publish(t, change(3))

	var got []notification

	timeout := time.After(guard)
	for len(got) < 3 {
		select {
		case n := <-ch:
			got = append(got, n)
		case <-timeout:
			t.Fatalf("notifications = %+v, want 3", got)
		}
	}

	if got[0].Message == 0 || got[0].Message != got[1].Message || got[2].Message == got[0].Message {
		t.Fatalf("messages = %d %d %d, want the first two equal and non-zero, the third another", got[0].Message, got[1].Message, got[2].Message)
	}
}

func TestLimitValue(t *testing.T) {
	good := func(v any) *ua.DataValue {
		return &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(v)}
	}

	tests := []struct {
		name string
		dv   *ua.DataValue
		want int
	}{
		{name: "uint32", dv: good(uint32(32)), want: 32},
		{name: "int32", dv: good(int32(500)), want: 500},
		{name: "uint16", dv: good(uint16(7)), want: 7},
		{name: "zero is no limit", dv: good(uint32(0)), want: 1000},
		{name: "above the cap", dv: good(uint32(1_000_000)), want: 1000},
		{name: "at the cap", dv: good(uint32(1000)), want: 1000},
		{name: "negative", dv: good(int32(-1)), want: limitUnknown},
		{name: "float", dv: good(float64(32)), want: limitUnknown},
		{name: "string", dv: good("32"), want: limitUnknown},
		{name: "bad status", dv: &ua.DataValue{EncodingMask: ua.DataValueStatusCode, Status: ua.StatusBadNodeIDUnknown}, want: limitUnknown},
		{name: "uncertain status", dv: &ua.DataValue{EncodingMask: ua.DataValueValue | ua.DataValueStatusCode, Status: ua.StatusUncertain, Value: ua.MustVariant(uint32(32))}, want: limitUnknown},
		{name: "no value", dv: &ua.DataValue{}, want: limitUnknown},
		{name: "nil", dv: nil, want: limitUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := limitValue(tt.dv); got != tt.want {
				t.Fatalf("limitValue = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTimingsWatchdog(t *testing.T) {
	tests := []struct {
		deathTolerance time.Duration
		want           time.Duration
	}{
		{deathTolerance: 30 * time.Second, want: 5 * time.Second},
		{deathTolerance: 15 * time.Second, want: 5 * time.Second},
		{deathTolerance: 9 * time.Second, want: 3 * time.Second},
		{deathTolerance: 2 * time.Second, want: time.Second},
	}

	for _, tt := range tests {
		if got := productionTimings.watchdog(tt.deathTolerance); got != tt.want {
			t.Fatalf("watchdog(%v) = %v, want %v", tt.deathTolerance, got, tt.want)
		}
	}
}

// TestSubscriptionLifetime: at least three keep-alive counts, otherwise a
// minute of publishing intervals.
func TestSubscriptionLifetime(t *testing.T) {
	tests := []struct {
		interval time.Duration
		want     uint32
	}{
		{interval: time.Second, want: 60},
		{interval: 50 * time.Millisecond, want: 1200},
		{interval: 7 * time.Second, want: 30},
		{interval: time.Minute, want: 30},
		{interval: 40 * time.Second, want: 30},
		{interval: 2300 * time.Microsecond, want: 26087},
	}

	for _, tt := range tests {
		if got := productionTimings.lifetime(tt.interval); got != tt.want {
			t.Errorf("lifetime(%v) = %d, want %d", tt.interval, got, tt.want)
		}
	}
}

// TestSubscribeRequestsTheLifetime: the subscription request carries the
// derived lifetime count, or the one of the timings when set.
func TestSubscribeRequestsTheLifetime(t *testing.T) {
	tests := []struct {
		name   string
		timing timings
		want   uint32
	}{
		{name: "production", timing: productionTimings, want: 1200},
		{name: "tests", timing: testTimings, want: 1 << 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, _ := testDeps(t)
			deps.timing = &tt.timing
			f := &fakeClient{}
			withFake(&deps, f)

			s := mustDial(t, noneConnection("opc.tcp://fake:4840"), deps)

			ctx, cancel := context.WithTimeout(context.Background(), guard)
			defer cancel()

			if _, _, err := s.Subscribe(ctx, 50*time.Millisecond, fakeItems); err != nil {
				t.Fatalf("Subscribe: %v", err)
			}

			f.mu.Lock()
			got := f.params
			f.mu.Unlock()

			if got.LifetimeCount != tt.want || got.MaxKeepAliveCount != productionTimings.MaxKeepAliveCount {
				t.Fatalf("lifetime %d, keep-alive %d; want %d, %d", got.LifetimeCount, got.MaxKeepAliveCount, tt.want, productionTimings.MaxKeepAliveCount)
			}
		})
	}
}
