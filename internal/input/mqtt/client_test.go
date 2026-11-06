package mqtt

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
)

// waitTimeout bounds every wait on the in-process broker; reaching it fails
// the test, it never synchronises one.
const waitTimeout = 5 * time.Second

type message struct {
	topic   string
	payload string
}

// testConnection is the connection to b with the given scheme.
func testConnection(b *mqtttest.Broker, scheme string) Connection {
	return Connection{
		Broker:            &url.URL{Scheme: scheme, Host: b.Addr()},
		QoS:               DefaultQoS,
		PersistentSession: DefaultPersistentSession,
		KeepAlive:         DefaultKeepAlive,
	}
}

// newTestClient builds a client whose messages go to the returned channel.
func newTestClient(t *testing.T, conn Connection, clientID, password string, cleanSession bool) (client, <-chan message) {
	t.Helper()

	messages := make(chan message, 16)
	c, err := newPahoClient(conn, clientID, password, cleanSession, func(topic string, payload []byte) bool {
		messages <- message{topic: topic, payload: string(payload)}

		return true
	})
	if err != nil {
		t.Fatalf("newPahoClient: %v", err)
	}
	t.Cleanup(c.Disconnect)

	return c, messages
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	t.Cleanup(cancel)

	return ctx
}

func receive(t *testing.T, messages <-chan message) message {
	t.Helper()

	select {
	case m := <-messages:
		return m
	case <-time.After(waitTimeout):
		t.Fatal("no message within the timeout")

		return message{}
	}
}

func waitLost(t *testing.T, c client) error {
	t.Helper()

	select {
	case err := <-c.Lost():
		return err
	case <-time.After(waitTimeout):
		t.Fatal("Lost did not fire within the timeout")

		return nil
	}
}

func TestPahoClientReceivesAPublishedMessage(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	c, messages := newTestClient(t, testConnection(b, SchemePlain), "receive", "", true)
	ctx := testContext(t)

	present, err := c.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if present {
		t.Error("a clean session reported a present session")
	}
	if err := c.Subscribe(ctx, []string{"plant/line1/temp"}, 1); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	b.Publish("plant/line1/temp", []byte(`{"v":21.5}`), false, 1)

	got := receive(t, messages)
	if got != (message{topic: "plant/line1/temp", payload: `{"v":21.5}`}) {
		t.Errorf("message = %+v", got)
	}
}

// TestPahoClientUnhandledMessageIsRedelivered: a QoS 1 message the receiver
// did not handle (the router stopped by Close) is not acknowledged, so the
// persistent session gives it to the next connection; a handled one is
// acknowledged and not given again.
func TestPahoClientUnhandledMessageIsRedelivered(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	conn := testConnection(b, SchemePlain)
	ctx := testContext(t)

	connect := func(handled bool) (client, <-chan message) {
		messages := make(chan message, 16)
		c, err := newPahoClient(conn, "unacked", "", false, func(topic string, payload []byte) bool {
			messages <- message{topic: topic, payload: string(payload)}

			return handled
		})
		if err != nil {
			t.Fatalf("newPahoClient: %v", err)
		}
		t.Cleanup(c.Disconnect)
		if _, err := c.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if err := c.Subscribe(ctx, []string{"plant/count"}, 1); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}

		return c, messages
	}

	dropping, dropped := connect(false)
	b.Publish("plant/count", []byte("1"), false, 1)
	if got := receive(t, dropped); got.payload != "1" {
		t.Fatalf("message = %+v", got)
	}
	dropping.Disconnect()

	handling, handled := connect(true)
	if got := receive(t, handled); got.payload != "1" {
		t.Fatalf("redelivered message = %+v, want the unacknowledged one", got)
	}
	// Acknowledged now: after a new connection the next message is the
	// first one received, nothing comes again before it.
	handling.Disconnect()

	_, last := connect(true)
	b.Publish("plant/count", []byte("2"), false, 1)
	if got := receive(t, last); got.payload != "2" {
		t.Errorf("message after the acknowledgement = %+v, want 2", got)
	}
}

func TestPahoClientUnsubscribeStopsDelivery(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	c, messages := newTestClient(t, testConnection(b, SchemePlain), "unsubscribe", "", true)
	ctx := testContext(t)

	if _, err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.Subscribe(ctx, []string{"a/old", "a/kept"}, 1); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := c.Unsubscribe(ctx, []string{"a/old"}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}

	// Delivery is ordered: the first message received is a/kept only if
	// a/old, published before it, was not delivered.
	b.Publish("a/old", []byte("1"), false, 1)
	b.Publish("a/kept", []byte("2"), false, 1)

	if got := receive(t, messages); got.topic != "a/kept" {
		t.Errorf("first message on %q, want a/kept", got.topic)
	}
}

func TestPahoClientEmptyTopicListsAreNoOps(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	c, _ := newTestClient(t, testConnection(b, SchemePlain), "empty", "", true)

	// Not connected: a round trip would fail, so these must not make one.
	if err := c.Subscribe(t.Context(), nil, 1); err != nil {
		t.Errorf("Subscribe(nil) = %v", err)
	}
	if err := c.Unsubscribe(t.Context(), nil); err != nil {
		t.Errorf("Unsubscribe(nil) = %v", err)
	}
}

func TestPahoClientHonoursAnExpiredContext(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	c, _ := newTestClient(t, testConnection(b, SchemePlain), "expired", "", true)

	if _, err := c.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Subscribe(ctx, []string{"a"}, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("Subscribe = %v, want context.Canceled", err)
	}
	if err := c.Unsubscribe(ctx, []string{"a"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Unsubscribe = %v, want context.Canceled", err)
	}
	if _, err := c.Connect(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Connect = %v, want context.Canceled", err)
	}
}

func TestPahoClientTLS(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{TLS: true})

	tests := []struct {
		name    string
		tls     *TLS
		wantErr error
	}{
		{name: "test CA", tls: &TLS{CAFile: b.CA()}},
		{name: "test CA and server name", tls: &TLS{CAFile: b.CA(), ServerName: "localhost"}},
		{name: "system pool", tls: nil, wantErr: errTLSVerify},
		{name: "server name not in the certificate", tls: &TLS{CAFile: b.CA(), ServerName: "broker.invalid"}, wantErr: errTLSVerify},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := testConnection(b, SchemeTLS)
			conn.TLS = tt.tls
			c, _ := newTestClient(t, conn, "tls", "", true)

			_, err := c.Connect(testContext(t))
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Connect: %v", err)
				}

				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Connect = %v, want %v", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), b.Addr()) {
				t.Errorf("error %q names the broker address", err)
			}
		})
	}
}

func TestPahoClientCredentials(t *testing.T) {
	const user, password = "line-operator", "correct-horse-battery"
	b := mqtttest.Start(t, mqtttest.Options{Users: map[string]string{user: password}})

	tests := []struct {
		name     string
		password string
		wantErr  error
	}{
		{name: "right password", password: password},
		{name: "wrong password", password: "wrong-staple-xyz", wantErr: errBadCredentials},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := testConnection(b, SchemePlain)
			conn.Auth = &Auth{User: user, Password: "secret://mqtt/password"}
			c, _ := newTestClient(t, conn, "credentials", tt.password, true)

			_, err := c.Connect(testContext(t))
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Connect: %v", err)
				}

				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Connect = %v, want %v", err, tt.wantErr)
			}
			for _, secret := range []string{user, tt.password, b.Addr()} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q contains %q", err, secret)
				}
			}
		})
	}
}

func TestPahoClientConnectionRefused(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	b.Stop()
	c, _ := newTestClient(t, testConnection(b, SchemePlain), "refused", "", true)

	_, err := c.Connect(testContext(t))
	if !errors.Is(err, errConnection) {
		t.Fatalf("Connect = %v, want %v", err, errConnection)
	}
	if strings.Contains(err.Error(), b.Addr()) {
		t.Errorf("error %q names the broker address", err)
	}
}

func TestPahoClientDoesNotReconnectByItself(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	c, _ := newTestClient(t, testConnection(b, SchemePlain), "no-auto-reconnect", "", true)

	options := c.(*pahoClient).lib.OptionsReader()
	if options.AutoReconnect() || options.ConnectRetry() {
		t.Fatalf("AutoReconnect = %v, ConnectRetry = %v, want both false", options.AutoReconnect(), options.ConnectRetry())
	}

	if _, err := c.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	b.Stop()

	if err := waitLost(t, c); !errors.Is(err, errConnectionLost) {
		t.Errorf("Lost = %v, want %v", err, errConnectionLost)
	}
	if c.(*pahoClient).lib.IsConnected() {
		t.Error("the client is connected after the loss")
	}

	// The same client connects again when asked, and a deliberate
	// Disconnect is not a loss.
	b.Restart()
	if _, err := c.Connect(testContext(t)); err != nil {
		t.Fatalf("Connect after Restart: %v", err)
	}
	c.Disconnect()
	select {
	case err := <-c.Lost():
		t.Errorf("Lost fired again: %v", err)
	default:
	}
}

func TestPahoClientPersistentSessionKeepsQoS1Messages(t *testing.T) {
	b := mqtttest.Start(t, mqtttest.Options{})
	c, messages := newTestClient(t, testConnection(b, SchemePlain), "persistent", "", false)
	ctx := testContext(t)

	if _, err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := c.Subscribe(ctx, []string{"plant/counter"}, 1); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	b.Stop()
	waitLost(t, c)

	b.Publish("plant/counter", []byte("42"), false, 1)
	b.Restart()

	present, err := c.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect after Restart: %v", err)
	}
	if !present {
		t.Error("sessionPresent = false after a persistent session")
	}
	if got := receive(t, messages); got != (message{topic: "plant/counter", payload: "42"}) {
		t.Errorf("message = %+v", got)
	}
}

func TestNewPahoClientLocalFiles(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "not.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pem")

	tests := []struct {
		name    string
		tls     TLS
		wantErr error
	}{
		{name: "missing CA file", tls: TLS{CAFile: missing}, wantErr: errCAFile},
		{name: "CA file without a certificate", tls: TLS{CAFile: notPEM}, wantErr: errCAFile},
		{name: "missing client certificate", tls: TLS{CertFile: missing, KeyFile: missing}, wantErr: errClientCertificate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := Connection{Broker: &url.URL{Scheme: SchemeTLS, Host: "127.0.0.1:8883"}, KeepAlive: DefaultKeepAlive, TLS: &tt.tls}

			_, err := newPahoClient(conn, "files", "", true, func(string, []byte) bool { return true })
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("newPahoClient = %v, want %v", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), dir) {
				t.Errorf("error %q names the file", err)
			}
		})
	}
}
