// Package opcuatest starts an in-process OPC UA server for tests of the
// commands and of the agent that cannot reach the package-internal test
// server of internal/input/opcua. It imports the library's server package,
// which no production package may: only test files import this package.
package opcuatest

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
	"strconv"
	"sync"
	"testing"
	"time"

	"encoding/hex"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/server"
	"github.com/gopcua/opcua/server/attrs"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uapolicy"
)

// Namespace is the namespace URI of the server's own nodes.
const Namespace = "urn:laraplate:opcuatest"

// Options vary the server; the zero value is the usual one.
type Options struct {
	// Expired gives the server a certificate that has already expired.
	Expired bool
	// Bulk is the number of Int32 variables in the folder Bulk (none when 0).
	Bulk int
	// Proxied puts a TCP proxy in front of the server: URL is then the
	// proxy's address, and Down, Up and Drop simulate a lost network. The
	// server advertises the proxy's URL among its endpoints, as a server
	// behind a NAT does: it answers endpoint discovery only for the URL the
	// client dialled.
	Proxied bool
}

// Server is a running test server with the policies None and Basic256Sha256
// (Sign and SignAndEncrypt) and anonymous authentication. SetValue changes a
// variable; with Options.Proxied, Drop, Down and Up simulate a lost network.
// Its nodes, in
// Namespace below the namespace's Objects folder:
//
//	temp   Double variable
//	count  Int32 variable
//	Line1  folder
//	  speed  Double variable
//	  state  String variable
//	  Cell   folder
//	    deep   Boolean variable, read only (every other variable is rw)
//	    Line1  a reference back to the folder Line1 (a loop)
//	Bulk   folder of Options.Bulk Int32 variables v0, v1, ... (when asked)
type Server struct {
	// URL is the endpoint, opc.tcp://127.0.0.1:<port>.
	URL string
	// Cert is the server certificate (DER); Thumbprint is its lowercase hex
	// SHA-1, the name of its file in a trust store.
	Cert       []byte
	Thumbprint string
	// NamespaceIndex is the index of Namespace on the server.
	NamespaceIndex uint16

	ns    *server.NodeNameSpace
	proxy *proxy
	port  int
}

// SilenceLibraryLogs discards the standard log package output, through which
// the library and its server log unconditionally. Call it from TestMain.
func SilenceLibraryLogs() {
	log.SetOutput(io.Discard)
}

// keys are built once per test binary: RSA 2048 generation is slow under
// -race.
var keys struct {
	once    sync.Once
	err     error
	key     *rsa.PrivateKey
	valid   []byte
	expired []byte
}

func loadKeys() error {
	keys.once.Do(func() {
		keys.key, keys.err = rsa.GenerateKey(rand.Reader, 2048)
		if keys.err != nil {
			return
		}

		now := time.Now()
		if keys.valid, keys.err = certificate(keys.key, now.Add(-time.Hour), now.Add(24*time.Hour)); keys.err != nil {
			return
		}

		keys.expired, keys.err = certificate(keys.key, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	})

	return keys.err
}

func certificate(key *rsa.PrivateKey, notBefore, notAfter time.Time) ([]byte, error) {
	appURI, err := url.Parse("urn:laraplate:opcuatest:server")
	if err != nil {
		return nil, err
	}

	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "laraplate test server"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment |
			x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		URIs:                  []*url.URL{appURI},
	}

	return x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
}

// Start starts a server on 127.0.0.1 and stops it when the test ends.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()

	if err := loadKeys(); err != nil {
		t.Fatalf("opcuatest: server certificate: %v", err)
	}

	cert := keys.valid
	if opts.Expired {
		cert = keys.expired
	}

	// The server cannot listen on port 0 usefully: pick a free port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("opcuatest: free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	serverOpts := []server.Option{
		server.EnableSecurity("None", ua.MessageSecurityModeNone),
		server.EnableSecurity("Basic256Sha256", ua.MessageSecurityModeSign),
		server.EnableSecurity("Basic256Sha256", ua.MessageSecurityModeSignAndEncrypt),
		server.EnableAuthMode(ua.UserTokenTypeAnonymous),
		server.EndPoint("127.0.0.1", port),
		server.PrivateKey(keys.key),
		server.Certificate(cert),
	}

	var front *proxy
	if opts.Proxied {
		if front, err = newProxy(); err != nil {
			t.Fatalf("opcuatest: proxy: %v", err)
		}

		front.setTarget(addr(port))
		t.Cleanup(front.close)

		serverOpts = append(serverOpts, server.EndPoint("127.0.0.1", front.port()))
	}

	srv := server.New(serverOpts...)

	root, err := srv.Namespace(0)
	if err != nil {
		t.Fatalf("opcuatest: root namespace: %v", err)
	}

	ns := server.NewNodeNameSpace(srv, Namespace)
	root.Objects().AddRef(ns.Objects(), id.HasComponent, true)

	buildNodes(ns, opts)

	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("opcuatest: start: %v", err)
	}

	t.Cleanup(func() {
		_ = srv.Close()
		cancel()
	})

	endpoint := port
	if front != nil {
		endpoint = front.port()
	}

	return &Server{
		URL:            "opc.tcp://127.0.0.1:" + strconv.Itoa(endpoint),
		Cert:           cert,
		Thumbprint:     hex.EncodeToString(uapolicy.Thumbprint(cert)),
		NamespaceIndex: ns.ID(),
		ns:             ns,
		proxy:          front,
		port:           port,
	}
}

// SetValue changes the value of the variable name of Namespace and notifies
// its subscribers. Its Go type must be the variable's (float64 for a
// Double, int32 for an Int32, string, bool).
func (s *Server) SetValue(t testing.TB, name string, value any) {
	t.Helper()

	id := ua.NewStringNodeID(s.ns.ID(), name)
	if status := s.ns.SetAttribute(id, ua.AttributeIDValue, server.DataValueFromValue(value)); status != ua.StatusOK {
		t.Fatalf("opcuatest: set %s: %v", name, status)
	}
}

// Drop closes every live connection through the proxy; new ones are still
// accepted. It needs Options.Proxied.
func (s *Server) Drop(t testing.TB) {
	t.Helper()

	if s.proxy == nil {
		t.Fatal("opcuatest: Drop needs Options.Proxied")
	}

	s.proxy.drop()
}

// Down makes the server unreachable through the proxy: the live connections
// are dropped and the new ones closed at once, as a server that stopped. It
// needs Options.Proxied.
func (s *Server) Down(t testing.TB) {
	t.Helper()

	if s.proxy == nil {
		t.Fatal("opcuatest: Down needs Options.Proxied")
	}

	closed, err := freePort()
	if err != nil {
		t.Fatalf("opcuatest: free port: %v", err)
	}

	s.proxy.setTarget(addr(closed))
	s.proxy.drop()
}

// Up makes the server reachable again after Down.
func (s *Server) Up(t testing.TB) {
	t.Helper()

	if s.proxy == nil {
		t.Fatal("opcuatest: Up needs Options.Proxied")
	}

	s.proxy.setTarget(addr(s.port))
}

func buildNodes(ns *server.NodeNameSpace, opts Options) {
	objects := ns.Objects()

	rw := byte(ua.AccessLevelTypeCurrentRead | ua.AccessLevelTypeCurrentWrite)

	variable := func(parent *server.Node, name string, dataType uint32, access byte, value any) {
		n := ns.AddNewVariableStringNode(name, value)
		_ = n.SetAttribute(ua.AttributeIDDataType, server.DataValueFromValue(ua.NewNumericNodeID(0, dataType)))
		_ = n.SetAttribute(ua.AttributeIDAccessLevel, server.DataValueFromValue(access))
		parent.AddRef(n, id.HasComponent, true)
	}

	variable(objects, "temp", id.Double, rw, float64(20))
	variable(objects, "count", id.Int32, rw, int32(1))

	line := folder(ns, "Line1")
	cell := folder(ns, "Cell")

	objects.AddRef(line, id.HasComponent, true)
	variable(line, "speed", id.Double, rw, float64(1.5))
	variable(line, "state", id.String, rw, "idle")
	line.AddRef(cell, id.HasComponent, true)
	variable(cell, "deep", id.Boolean, byte(ua.AccessLevelTypeCurrentRead), true)
	cell.AddRef(line, id.HasComponent, true)

	if opts.Bulk > 0 {
		bulk := folder(ns, "Bulk")
		objects.AddRef(bulk, id.HasComponent, true)

		for i := range opts.Bulk {
			variable(bulk, "v"+strconv.Itoa(i), id.Int32, rw, int32(i))
		}
	}
}

// folder adds an Object node with no references of its own.
func folder(ns *server.NodeNameSpace, name string) *server.Node {
	return ns.AddNode(server.NewNode(
		ua.NewStringNodeID(ns.ID(), name),
		map[ua.AttributeID]*ua.DataValue{
			ua.AttributeIDNodeClass:     server.DataValueFromValue(uint32(ua.NodeClassObject)),
			ua.AttributeIDBrowseName:    server.DataValueFromValue(attrs.BrowseName(name)),
			ua.AttributeIDDisplayName:   server.DataValueFromValue(attrs.DisplayName(name, "")),
			ua.AttributeIDDescription:   server.DataValueFromValue(attrs.DisplayName(name, "")),
			ua.AttributeIDEventNotifier: server.DataValueFromValue(int16(0)),
		},
		nil,
		nil,
	))
}
