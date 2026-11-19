// Package mqtttest runs an in-process MQTT broker for the tests of the mqtt
// driver (decision Q10). It is a test-only package: only _test.go files may
// import it, so the broker library (mochi-mqtt, MIT) never reaches the
// shipped binary.
//
// One mochi server lives for the whole test. Its clients do not arrive through
// a mochi listener but through a front listener of this package, which hands
// every accepted connection to the server: Stop closes those connections and
// the front listener, Restart listens again on the same address. The server
// itself is never closed before the test ends, so the sessions it keeps in
// memory, and the QoS 1 messages queued for them, survive a Stop.
package mqtttest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// listenerID names the front listener on the mochi server.
const listenerID = "mqtttest"

// waitTimeout bounds WaitAcknowledged; reaching it fails the test.
const waitTimeout = 5 * time.Second

// Options configure a test broker.
type Options struct {
	// TLS serves TLS with a certificate for 127.0.0.1 and localhost, signed by
	// a CA generated for the test (CA returns its PEM file).
	TLS bool
	// Users are the accepted user name and password pairs; empty accepts
	// every client, with or without credentials.
	Users map[string]string
	// Refuse are the topic filters the broker refuses to subscribe, as an
	// ACL would: their SUBACK return code is 0x80 (MQTT 3.1.1). SetRefused
	// changes them later.
	Refuse []string
}

// Broker is an in-process MQTT broker.
type Broker struct {
	t       testing.TB
	server  *mochi.Server
	addr    string
	tlsConf *tls.Config
	caFile  string

	mu      sync.Mutex
	ln      net.Listener
	conns   map[net.Conn]struct{}
	running sync.WaitGroup

	// acks counts, by client id, the QoS flows the broker completed; acked
	// is closed and replaced at every count.
	acks  map[string]int
	acked chan struct{}

	// refuse are the topic filters refused at subscribe.
	refuse map[string]bool
	// connects are the CONNECT packets received, in order.
	connects []Connect
	// hold, when set, holds every SUBSCRIBE until it is released.
	hold *Hold
	// published are the PUBLISH packets received from clients, the broker's
	// own excluded, in order; changed is closed and replaced at every one.
	published []Message
	changed   chan struct{}
	// subscribed counts, by client id, the SUBSCRIBE packets the broker
	// answered with a SUBACK; changed is closed and replaced at every one
	// too.
	subscribed map[string]int
	// granted holds, by topic filter, the client id that last subscribed to
	// it with the subscription granted, once its SUBACK is sent.
	granted map[string]string
	// pending holds the filters a SUBSCRIBE granted until its SUBACK is
	// sent: mochi calls OnSubscribed before it writes the SUBACK, and a
	// message published in between could reach the client first.
	pending map[subscription][]string
}

// subscription names a SUBSCRIBE packet of a client.
type subscription struct {
	clientID string
	packetID uint16
}

// Message is a PUBLISH packet the broker received from a client.
type Message struct {
	ClientID string
	Topic    string
	Payload  []byte
	QoS      byte
	Retain   bool
}

// Hold holds the SUBSCRIBE packets the broker receives until Release.
type Hold struct {
	arrived     chan struct{}
	arrivedOnce sync.Once
	released    chan struct{}
	releaseOnce sync.Once
}

// Arrived is closed when the broker holds its first SUBSCRIBE.
func (h *Hold) Arrived() <-chan struct{} { return h.arrived }

// Release lets the held SUBSCRIBE packets, and every later one, through;
// it is safe to call twice.
func (h *Hold) Release() { h.releaseOnce.Do(func() { close(h.released) }) }

// Start starts a broker on a free port of 127.0.0.1 and stops it at the end
// of the test.
func Start(t testing.TB, opts Options) *Broker {
	t.Helper()

	server := mochi.New(&mochi.Options{
		InlineClient: true,
		Logger:       slog.New(slog.DiscardHandler),
	})
	b := &Broker{
		t:       t,
		server:  server,
		conns:   map[net.Conn]struct{}{},
		acks:    map[string]int{},
		acked:   make(chan struct{}),
		changed: make(chan struct{}),

		subscribed: map[string]int{},
		granted:    map[string]string{},
		pending:    map[subscription][]string{},
	}
	b.SetRefused(opts.Refuse...)
	if err := server.AddHook(&authHook{users: opts.Users, broker: b}, nil); err != nil {
		t.Fatalf("mqtttest: auth hook: %v", err)
	}
	if err := server.AddHook(&ackHook{broker: b}, nil); err != nil {
		t.Fatalf("mqtttest: ack hook: %v", err)
	}
	if err := server.AddHook(&recordHook{broker: b}, nil); err != nil {
		t.Fatalf("mqtttest: record hook: %v", err)
	}
	if err := server.Serve(); err != nil {
		t.Fatalf("mqtttest: serve: %v", err)
	}

	if opts.TLS {
		b.tlsConf, b.caFile = newTLS(t)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("mqtttest: listen: %v", err)
	}
	b.addr = ln.Addr().String()
	b.serve(ln)

	t.Cleanup(func() {
		b.Stop()
		server.Listeners.ClientsWg.Wait()
		_ = server.Close()
	})

	return b
}

// Addr is the host:port of the broker; it does not change on Restart.
func (b *Broker) Addr() string { return b.addr }

// CA is the path of the PEM file of the CA that signed the broker
// certificate; empty without TLS.
func (b *Broker) CA() string { return b.caFile }

// Publish publishes a message as the broker itself, to every subscriber of
// topic, connected or holding a persistent session. It fails the test on a
// refusal, so call it from the test goroutine.
func (b *Broker) Publish(topic string, payload []byte, retain bool, qos byte) {
	b.t.Helper()

	if err := b.server.Publish(topic, payload, retain, qos); err != nil {
		b.t.Fatalf("mqtttest: publish: %v", err)
	}
}

// WaitAcknowledged waits until the broker has completed n QoS flows with the
// client clientID since the start of the test: for a client that only
// receives, n messages it was sent at QoS 1 or 2 and acknowledged. A client
// acknowledges on its own goroutine, so a test that saw a message handled
// and wants the acknowledgement to have reached the broker (before a
// disconnect would drop it) waits here. It fails the test after a timeout,
// so call it from the test goroutine.
func (b *Broker) WaitAcknowledged(clientID string, n int) {
	b.t.Helper()

	timeout := time.NewTimer(waitTimeout)
	defer timeout.Stop()

	for {
		b.mu.Lock()
		count, acked := b.acks[clientID], b.acked
		b.mu.Unlock()

		if count >= n {
			return
		}

		select {
		case <-acked:
		case <-timeout.C:
			b.t.Fatalf("mqtttest: %d of %d acknowledgements from %q within the timeout", count, n, clientID)

			return
		}
	}
}

// Published returns the PUBLISH packets the broker received from clientID
// since the start of the test, in arrival order; the broker's own publishes
// are not included.
func (b *Broker) Published(clientID string) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []Message
	for _, m := range b.published {
		if m.ClientID == clientID {
			out = append(out, m)
		}
	}

	return out
}

// WaitPublished waits until the broker has received n PUBLISH packets from
// clientID and returns those received so far. It fails the test after a
// timeout, so call it from the test goroutine.
func (b *Broker) WaitPublished(clientID string, n int) []Message {
	b.t.Helper()

	timeout := time.NewTimer(waitTimeout)
	defer timeout.Stop()

	for {
		b.mu.Lock()
		changed := b.changed
		b.mu.Unlock()

		got := b.Published(clientID)
		if len(got) >= n {
			return got
		}

		select {
		case <-changed:
		case <-timeout.C:
			b.t.Fatalf("mqtttest: %d of %d publishes from %q within the timeout", len(got), n, clientID)

			return got
		}
	}
}

// WaitSubscribed waits until the broker has answered n SUBSCRIBE packets
// from clientID with their SUBACK, so a message published afterwards reaches
// it after the SUBACK, retained or not. It fails the test after a timeout, so call it from the test goroutine.
func (b *Broker) WaitSubscribed(clientID string, n int) {
	b.t.Helper()

	timeout := time.NewTimer(waitTimeout)
	defer timeout.Stop()

	for {
		b.mu.Lock()
		count, changed := b.subscribed[clientID], b.changed
		b.mu.Unlock()

		if count >= n {
			return
		}

		select {
		case <-changed:
		case <-timeout.C:
			b.t.Fatalf("mqtttest: %d of %d subscriptions from %q within the timeout", count, n, clientID)

			return
		}
	}
}

// WaitSubscribedTo waits until the broker has granted a client's
// subscription to filter and sent its SUBACK, so a message published
// afterwards on a matching topic reaches it after the SUBACK, and returns that client's id. It fails the test after a
// timeout, so call it from the test goroutine.
func (b *Broker) WaitSubscribedTo(filter string) string {
	b.t.Helper()

	timeout := time.NewTimer(waitTimeout)
	defer timeout.Stop()

	for {
		b.mu.Lock()
		clientID, changed := b.granted[filter], b.changed
		b.mu.Unlock()

		if clientID != "" {
			return clientID
		}

		select {
		case <-changed:
		case <-timeout.C:
			b.t.Fatalf("mqtttest: no subscription to %q granted within the timeout", filter)

			return ""
		}
	}
}

// SetRefused replaces the topic filters the broker refuses to subscribe; a
// subscription already granted is kept.
func (b *Broker) SetRefused(filters ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refuse = make(map[string]bool, len(filters))
	for _, f := range filters {
		b.refuse[f] = true
	}
}

// Connect is what the broker recorded of a CONNECT packet.
type Connect struct {
	ClientID     string
	CleanSession bool
}

// Connects returns the CONNECT packets the broker received since the start
// of the test, in arrival order.
func (b *Broker) Connects() []Connect {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]Connect(nil), b.connects...)
}

// HoldSubscriptions makes the broker hold every SUBSCRIBE, and so its
// SUBACK, until the returned hold is released; the end of the test
// releases it.
func (b *Broker) HoldSubscriptions() *Hold {
	h := &Hold{arrived: make(chan struct{}), released: make(chan struct{})}
	b.t.Cleanup(h.Release)

	b.mu.Lock()
	b.hold = h
	b.mu.Unlock()

	return h
}

// refused reports whether a subscription to filter is refused.
func (b *Broker) refused(filter string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.refuse[filter]
}

// acknowledged counts a QoS flow the broker completed with clientID.
func (b *Broker) acknowledged(clientID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.acks[clientID]++
	close(b.acked)
	b.acked = make(chan struct{})
}

// Stop closes every client connection and stops accepting new ones: a
// connection attempt is refused until Restart. The sessions are kept.
func (b *Broker) Stop() {
	b.mu.Lock()
	ln := b.ln
	b.ln = nil
	conns := make([]net.Conn, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	b.running.Wait()
}

// Restart accepts connections again on the same address after Stop; a
// running broker is left as it is.
func (b *Broker) Restart() {
	b.t.Helper()

	b.mu.Lock()
	running := b.ln != nil
	b.mu.Unlock()
	if running {
		return
	}

	ln, err := net.Listen("tcp", b.addr)
	if err != nil {
		b.t.Fatalf("mqtttest: listen again: %v", err)
	}
	b.serve(ln)
}

// serve accepts on ln until Stop and hands every connection to the server.
func (b *Broker) serve(ln net.Listener) {
	b.mu.Lock()
	b.ln = ln
	b.mu.Unlock()

	b.running.Add(1)
	go func() {
		defer b.running.Done()

		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			if !b.track(ln, conn) {
				_ = conn.Close()

				return
			}

			go func() {
				defer b.untrack(conn)

				var c net.Conn = conn
				if b.tlsConf != nil {
					c = tls.Server(conn, b.tlsConf)
				}
				_ = b.server.EstablishConnection(listenerID, c)
				_ = conn.Close()
			}()
		}
	}()
}

// track records conn unless the listener it came from was stopped meanwhile.
func (b *Broker) track(ln net.Listener, conn net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.ln != ln {
		return false
	}
	b.conns[conn] = struct{}{}

	return true
}

func (b *Broker) untrack(conn net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.conns, conn)
}

// authHook accepts the configured user and password pairs, or every client
// when none is configured, and allows every topic but the refused filters,
// which it denies for reading: a subscription to one of them is refused.
type authHook struct {
	mochi.HookBase
	users  map[string]string
	broker *Broker
}

func (h *authHook) ID() string { return "mqtttest-auth" }

func (h *authHook) Provides(b byte) bool {
	return b == mochi.OnConnectAuthenticate || b == mochi.OnACLCheck
}

func (h *authHook) OnConnectAuthenticate(_ *mochi.Client, pk packets.Packet) bool {
	if len(h.users) == 0 {
		return true
	}

	password, ok := h.users[string(pk.Connect.Username)]

	return ok && pk.Connect.PasswordFlag && bytes.Equal(pk.Connect.Password, []byte(password))
}

func (h *authHook) OnACLCheck(_ *mochi.Client, topic string, write bool) bool {
	return write || !h.broker.refused(topic)
}

// ackHook tells the broker of every QoS flow completed: a PUBACK or PUBCOMP
// received for a message the broker sent, or the acknowledgement the broker
// sent for one it received.
type ackHook struct {
	mochi.HookBase
	broker *Broker
}

func (h *ackHook) ID() string { return "mqtttest-ack" }

func (h *ackHook) Provides(b byte) bool { return b == mochi.OnQosComplete }

func (h *ackHook) OnQosComplete(cl *mochi.Client, _ packets.Packet) {
	h.broker.acknowledged(cl.ID)
}

// recordHook records every CONNECT and every PUBLISH of a client, and holds
// the SUBSCRIBE packets while a Hold is set.
type recordHook struct {
	mochi.HookBase
	broker *Broker
}

func (h *recordHook) ID() string { return "mqtttest-record" }

func (h *recordHook) Provides(b byte) bool {
	return b == mochi.OnConnect || b == mochi.OnSubscribe || b == mochi.OnSubscribed || b == mochi.OnPacketSent || b == mochi.OnPublish
}

func (h *recordHook) OnPublish(cl *mochi.Client, pk packets.Packet) (packets.Packet, error) {
	if cl.Net.Inline {
		return pk, nil
	}

	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()

	h.broker.published = append(h.broker.published, Message{
		ClientID: cl.ID,
		Topic:    pk.TopicName,
		Payload:  append([]byte(nil), pk.Payload...),
		QoS:      pk.FixedHeader.Qos,
		Retain:   pk.FixedHeader.Retain,
	})
	close(h.broker.changed)
	h.broker.changed = make(chan struct{})

	return pk, nil
}

func (h *recordHook) OnConnect(_ *mochi.Client, pk packets.Packet) error {
	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()

	h.broker.connects = append(h.broker.connects, Connect{ClientID: pk.Connect.ClientIdentifier, CleanSession: pk.Connect.Clean})

	return nil
}

func (h *recordHook) OnSubscribed(cl *mochi.Client, pk packets.Packet, reasonCodes []byte) {
	if cl.Net.Inline {
		return
	}

	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()

	var granted []string
	for i, sub := range pk.Filters {
		if i < len(reasonCodes) && reasonCodes[i] < packets.ErrUnspecifiedError.Code {
			granted = append(granted, sub.Filter)
		}
	}
	h.broker.pending[subscription{cl.ID, pk.PacketID}] = granted
}

// OnPacketSent counts a SUBSCRIBE, and records the filters it granted, once
// its SUBACK is written: a PUBLISH the broker writes afterwards follows it on
// the connection.
func (h *recordHook) OnPacketSent(cl *mochi.Client, pk packets.Packet, _ []byte) {
	if cl.Net.Inline || pk.FixedHeader.Type != packets.Suback {
		return
	}

	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()

	key := subscription{cl.ID, pk.PacketID}
	granted, ok := h.broker.pending[key]
	if !ok {
		return
	}
	delete(h.broker.pending, key)

	h.broker.subscribed[cl.ID]++
	for _, filter := range granted {
		h.broker.granted[filter] = cl.ID
	}
	close(h.broker.changed)
	h.broker.changed = make(chan struct{})
}

func (h *recordHook) OnSubscribe(cl *mochi.Client, pk packets.Packet) packets.Packet {
	if cl.Net.Inline {
		return pk
	}

	h.broker.mu.Lock()
	hold := h.broker.hold
	h.broker.mu.Unlock()

	if hold != nil {
		hold.arrivedOnce.Do(func() { close(hold.arrived) })
		<-hold.released
	}

	return pk
}

// newTLS generates a CA and a server certificate for 127.0.0.1 and localhost
// signed by it; it returns the server configuration and the CA PEM file.
func newTLS(t testing.TB) (*tls.Config, string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mqtttest: CA key: %v", err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mqtttest CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("mqtttest: CA certificate: %v", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("mqtttest: CA certificate: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("mqtttest: server key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("mqtttest: server certificate: %v", err)
	}

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatalf("mqtttest: write CA: %v", err)
	}

	conf := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}

	return conf, caFile
}
