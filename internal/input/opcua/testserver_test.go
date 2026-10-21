package opcua

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/server"
	"github.com/gopcua/opcua/ua"
)

// guard bounds a wait for a goroutine or a network exchange; it is a deadlock
// guard, never a timing assertion (ruling R3).
const guard = 10 * time.Second

// testNamespace is the namespace URI of the test server's own nodes.
const testNamespace = "urn:laraplate:test"

// TestMain silences the standard log package: the library and its test server
// log through it unconditionally (notes section 10), and the agent routes it
// through logging.StdlogBridge only in the binary.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// fixtures holds what is expensive to build: one server certificate and one
// own certificate per test binary (RSA 2048 generation is slow under -race).
var fixtures struct {
	once      sync.Once
	err       error
	serverDER []byte
	serverKey *rsa.PrivateKey
	ownDER    []byte
	ownKey    *rsa.PrivateKey
}

func loadFixtures(t *testing.T) {
	t.Helper()

	fixtures.once.Do(func() {
		fixtures.serverDER, fixtures.serverKey, fixtures.err = serverCertificate("urn:test:server")
		if fixtures.err != nil {
			return
		}
		fixtures.ownDER, fixtures.ownKey, _, fixtures.err = generateOwn("test-agent", time.Now())
	})

	if fixtures.err != nil {
		t.Fatalf("test certificates: %v", fixtures.err)
	}
}

// serverCertificate is a self-signed RSA 2048 server certificate with the
// application URI uri and the IP 127.0.0.1.
func serverCertificate(uri string) ([]byte, *rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	appURI, err := url.Parse(uri)
	if err != nil {
		return nil, nil, err
	}

	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "laraplate test server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment |
			x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		URIs:                  []*url.URL{appURI},
	}

	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	return der, key, nil
}

// testServer is the library's in-process server behind a TCP proxy. Clients
// connect to URL, the proxy's address.
//
// The endpoint URL question: the server answers GetEndpoints only with the
// endpoints whose configured URL equals the request's EndpointURL
// (server/discovery_service.go; an unknown URL gets an empty list), returns
// in CreateSession only the matching endpoints too (server/session_service.go;
// not refused, the client then falls back to the anonymous policy id
// "Anonymous"), and listens on the first configured URL only
// (server/server.go Start). The client sends the URL it dials. So the server
// is configured with two URLs: its own listening address first, then the
// proxy's, as a server behind a NAT advertises its public address; a client
// dialling the proxy finds its URL among the server's endpoints. Without the
// proxy's URL, discovery through the proxy returns no endpoint (verified).
// The client always dials the configured endpoint, never the EndpointURL of a
// discovered endpoint.
type testServer struct {
	URL   string
	Cert  []byte
	Proxy *tcpProxy

	srv    *server.Server
	ns     *server.NodeNameSpace
	cancel context.CancelFunc
}

// Nodes of the test namespace. nodeTemp and nodeCount never change; tests
// that change a value use nodeChanging and never assume its value.
const (
	nodeTemp     = "temp"     // float64 20
	nodeCount    = "count"    // int32 1
	nodeChanging = "changing" // float64
)

// shared is the server of the tests that neither drop nor stall its proxy:
// building a server costs about a second under -race (the nodeset import),
// so one serves the whole test binary, -count included.
var shared struct {
	once sync.Once
	srv  *testServer
	err  error
}

// sharedTestServer returns the binary's shared server; tests must not drop
// or stall its proxy, nor assume the value of nodeChanging.
func sharedTestServer(t *testing.T) *testServer {
	t.Helper()
	loadFixtures(t)

	shared.once.Do(func() { shared.srv, shared.err = buildTestServer() })

	if shared.err != nil {
		t.Fatalf("shared test server: %v", shared.err)
	}

	return shared.srv
}

// startTestServer starts a server of the test alone, stopped at its end.
func startTestServer(t *testing.T) *testServer {
	t.Helper()
	loadFixtures(t)

	s, err := buildTestServer()
	if err != nil {
		t.Fatalf("test server: %v", err)
	}
	t.Cleanup(s.close)

	return s
}

// buildTestServer starts a server with None, Basic256Sha256 Sign and
// SignAndEncrypt endpoints, anonymous and user name authentication, and the
// namespace testNamespace holding the test nodes, behind a proxy.
func buildTestServer() (*testServer, error) {
	proxy, err := newTCPProxy()
	if err != nil {
		return nil, err
	}

	port, err := freePort()
	if err != nil {
		proxy.Close()

		return nil, err
	}

	srv := server.New(
		server.EnableSecurity(PolicyNone, ua.MessageSecurityModeNone),
		server.EnableSecurity(PolicyBasic256Sha256, ua.MessageSecurityModeSign),
		server.EnableSecurity(PolicyBasic256Sha256, ua.MessageSecurityModeSignAndEncrypt),
		server.EnableAuthMode(ua.UserTokenTypeAnonymous),
		server.EnableAuthMode(ua.UserTokenTypeUserName),
		server.EndPoint("127.0.0.1", port),
		server.EndPoint("127.0.0.1", proxy.Port()),
		server.PrivateKey(fixtures.serverKey),
		server.Certificate(fixtures.serverDER),
	)

	root, err := srv.Namespace(0)
	if err != nil {
		proxy.Close()

		return nil, err
	}

	ns := server.NewNodeNameSpace(srv, testNamespace)
	rootObjects := root.Objects()
	rootObjects.AddRef(ns.Objects(), id.HasComponent, true)

	for name, value := range map[string]any{nodeTemp: float64(20), nodeCount: int32(1), nodeChanging: float64(0)} {
		v := ns.AddNewVariableStringNode(name, value)
		ns.Objects().AddRef(v, id.HasComponent, true)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		proxy.Close()

		return nil, err
	}

	proxy.SetTarget("127.0.0.1:" + strconv.Itoa(port))

	return &testServer{
		URL:    "opc.tcp://127.0.0.1:" + strconv.Itoa(proxy.Port()),
		Cert:   fixtures.serverDER,
		Proxy:  proxy,
		srv:    srv,
		ns:     ns,
		cancel: cancel,
	}, nil
}

func (s *testServer) close() {
	s.Proxy.Close()
	_ = s.srv.Close()
	s.cancel()
}

// NodeID is the NodeId of a node of the test namespace, by name.
func (s *testServer) NodeID(name string) *ua.NodeID {
	return ua.NewStringNodeID(s.ns.ID(), name)
}

// NamespaceIndex is the index of testNamespace on the server.
func (s *testServer) NamespaceIndex() uint16 {
	return s.ns.ID()
}

// Set changes a node's value and notifies the subscribers.
func (s *testServer) Set(t *testing.T, name string, value any) {
	t.Helper()

	if status := s.ns.SetAttribute(s.NodeID(name), ua.AttributeIDValue, server.DataValueFromValue(value)); status != ua.StatusOK {
		t.Fatalf("set %s: %v", name, status)
	}
}

// freePort returns a port that was free on 127.0.0.1 a moment ago: the test
// server cannot listen on port 0 usefully (notes section 11).
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil
}

// tcpProxy forwards TCP connections to a target. Drop closes every live
// connection and keeps listening, as a network loss does; Stall stops
// forwarding bytes in both directions without closing anything, as a
// half-open connection does, until Resume.
type tcpProxy struct {
	ln   net.Listener
	done chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	target  string
	conns   map[net.Conn]struct{}
	stalled chan struct{} // non-nil while stalled; closed by Resume
}

func newTCPProxy() (*tcpProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	p := &tcpProxy{ln: ln, done: make(chan struct{}), conns: map[net.Conn]struct{}{}}

	p.wg.Add(1)
	go p.accept()

	return p, nil
}

// Port is the proxy's listening port.
func (p *tcpProxy) Port() int {
	return p.ln.Addr().(*net.TCPAddr).Port
}

// SetTarget sets the address new connections are forwarded to.
func (p *tcpProxy) SetTarget(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.target = addr
}

// Drop closes every live connection; new connections are still accepted.
func (p *tcpProxy) Drop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for c := range p.conns {
		_ = c.Close()
	}
	clear(p.conns)
}

// Stall stops forwarding bytes; the connections stay open.
func (p *tcpProxy) Stall() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stalled == nil {
		p.stalled = make(chan struct{})
	}
}

// Resume forwards bytes again after Stall.
func (p *tcpProxy) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stalled != nil {
		close(p.stalled)
		p.stalled = nil
	}
}

// Close stops listening, drops every connection and waits for the forwarding
// goroutines.
func (p *tcpProxy) Close() {
	select {
	case <-p.done:
		return
	default:
	}

	close(p.done)
	_ = p.ln.Close()
	p.Drop()
	p.wg.Wait()
}

func (p *tcpProxy) accept() {
	defer p.wg.Done()

	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}

		p.mu.Lock()
		target := p.target
		p.mu.Unlock()

		upstream, err := net.Dial("tcp", target)
		if err != nil {
			_ = client.Close()

			continue
		}

		if !p.track(client, upstream) {
			return
		}

		p.wg.Add(2)
		go p.pipe(upstream, client)
		go p.pipe(client, upstream)
	}
}

// track registers a connection pair, or closes it when the proxy is closed.
func (p *tcpProxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		for _, c := range conns {
			_ = c.Close()
		}

		return false
	default:
	}

	for _, c := range conns {
		p.conns[c] = struct{}{}
	}

	return true
}

// pipe copies src to dst, holding each chunk while the proxy is stalled.
// Closing either side ends both.
func (p *tcpProxy) pipe(dst, src net.Conn) {
	defer p.wg.Done()
	defer func() {
		_ = dst.Close()
		_ = src.Close()
	}()

	buf := make([]byte, 32<<10)

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if !p.waitForwarding() {
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// waitForwarding blocks while the proxy is stalled; false when it closed.
func (p *tcpProxy) waitForwarding() bool {
	for {
		p.mu.Lock()
		stalled := p.stalled
		p.mu.Unlock()

		if stalled == nil {
			return true
		}

		select {
		case <-stalled:
		case <-p.done:
			return false
		}
	}
}
