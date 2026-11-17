package mqtt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"slices"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// client is what the driver needs of an MQTT client: connect, subscribe,
// unsubscribe, disconnect and learn of a lost connection. It has no publish
// function, so the driver cannot send one (spec section 1). The messages go to
// the function given when the client is built, one at a time and in arrival
// order. *pahoClient implements it.
type client interface {
	// Connect opens the connection and reports whether the broker kept a
	// session for the client id. A ctx that ends first returns ctx.Err() and
	// abandons the attempt.
	Connect(ctx context.Context) (sessionPresent bool, err error)
	// Subscribe subscribes topics at qos and waits for the broker's answer;
	// no topics is a no-op. refused lists, in the order of topics, those the
	// broker refused (SUBACK return code 0x80, an ACL); the others are
	// subscribed and the connection is kept.
	Subscribe(ctx context.Context, topics []string, qos byte) (refused []string, err error)
	// Unsubscribe unsubscribes topics and waits for the broker's answer; no
	// topics is a no-op.
	Unsubscribe(ctx context.Context, topics []string) error
	// Disconnect closes the connection, if any, without unsubscribing: a
	// persistent session keeps its messages. It is not a loss.
	Disconnect()
	// Lost receives one error for every connection lost after a successful
	// Connect, a connection the broker closed included.
	Lost() <-chan error
}

// Errors of the client. They are fixed texts: the library's own messages
// carry the broker address, and a refusal must never echo the user name or
// the password. CONNACK codes 4 (bad user name or password) and 5 (not
// authorised) are one error: brokers use either for a wrong password (mochi
// answers 5).
var (
	errConnection         = errors.New("mqtt: connection to the broker failed")
	errTLSVerify          = errors.New("mqtt: the broker certificate failed verification")
	errBadCredentials     = errors.New("mqtt: the broker refused the credentials or did not authorise the client")
	errClientIDRejected   = errors.New("mqtt: the broker rejected the client id")
	errServerUnavailable  = errors.New("mqtt: the broker is unavailable")
	errProtocolRefused    = errors.New("mqtt: the broker refused MQTT 3.1.1")
	errConnectionLost     = errors.New("mqtt: connection to the broker lost")
	errSubscribe          = errors.New("mqtt: subscribe failed")
	errSubscribeRefused   = errors.New("mqtt: the broker refused the subscription")
	errUnsubscribe        = errors.New("mqtt: unsubscribe failed")
	errBrokerURL          = errors.New("mqtt: connection.broker: unsupported scheme")
	errCAFile             = errors.New("mqtt: connection.tls.ca_file: cannot be read as PEM certificates")
	errClientCertificate  = errors.New("mqtt: connection.tls.cert_file, key_file: cannot be loaded as a certificate and key pair")
	connackRefusalsByCode = map[byte]error{
		packets.ErrRefusedBadProtocolVersion:    errProtocolRefused,
		packets.ErrRefusedIDRejected:            errClientIDRejected,
		packets.ErrRefusedServerUnavailable:     errServerUnavailable,
		packets.ErrRefusedBadUsernameOrPassword: errBadCredentials,
		packets.ErrRefusedNotAuthorised:         errBadCredentials,
	}
)

// disconnectQuiesce is how long Disconnect waits for the DISCONNECT packet to
// leave, in milliseconds.
const disconnectQuiesce = 250

// subscribeFailure is the SUBACK return code of a refused subscription.
const subscribeFailure = 0x80

// pahoClient is the client on top of paho (MQTT 3.1.1). paho's automatic
// reconnection and connect retry are off: the driver runs its own loop with
// the kit's backoff (spec section 5). paho's package loggers stay at their
// silent defaults.
type pahoClient struct {
	lib  paho.Client
	lost chan error
}

// newPahoClient builds the client of conn without connecting. It reads the
// TLS files here, so a missing or broken file is a local resource error and
// not a connection failure. password is the resolved value of conn.Auth's
// reference; onMessage receives a copy of every payload and reports whether
// the message was handled: only then is a QoS 1 message acknowledged, so one
// left unhandled (the input is closing) stays in the persistent session and
// the broker delivers it again to the next connection.
func newPahoClient(conn Connection, clientID, password string, cleanSession bool, onMessage func(topic string, payload []byte) (handled bool)) (client, error) {
	return newLibClient(conn, clientID, password, cleanSession, func(m paho.Message) bool {
		return onMessage(m.Topic(), append([]byte(nil), m.Payload()...))
	})
}

// newPahoClientRetained is newPahoClient for a receiver that also wants the
// retain flag of a message and handles every message (browse and probe, on a
// clean session at QoS 0).
func newPahoClientRetained(conn Connection, clientID, password string, cleanSession bool, onMessage func(topic string, payload []byte, retained bool)) (client, error) {
	return newLibClient(conn, clientID, password, cleanSession, func(m paho.Message) bool {
		onMessage(m.Topic(), append([]byte(nil), m.Payload()...), m.Retained())

		return true
	})
}

// newLibClient builds the paho client. The automatic acknowledgement is off:
// a message is acknowledged after handle returns true, on the delivery
// goroutine, so in order.
func newLibClient(conn Connection, clientID, password string, cleanSession bool, handle func(m paho.Message) (handled bool)) (client, error) {
	broker, err := brokerAddress(conn)
	if err != nil {
		return nil, err
	}

	c := &pahoClient{lost: make(chan error, 1)}

	opts := paho.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetProtocolVersion(4).
		SetCleanSession(cleanSession).
		SetKeepAlive(conn.KeepAlive).
		SetOrderMatters(true).
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetAutoAckDisabled(true).
		SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) {
			if handle(m) {
				m.Ack()
			}
		}).
		SetConnectionLostHandler(func(paho.Client, error) {
			select {
			case c.lost <- errConnectionLost:
			default:
			}
		})

	if conn.Auth != nil {
		opts.SetUsername(conn.Auth.User).SetPassword(password)
	}

	if conn.Broker.Scheme == SchemeTLS {
		tlsConf, err := tlsConfig(conn.TLS)
		if err != nil {
			return nil, err
		}
		opts.SetTLSConfig(tlsConf)
	}

	c.lib = paho.NewClient(opts)

	return c, nil
}

// brokerAddress is the paho URL of conn's broker.
func brokerAddress(conn Connection) (string, error) {
	if conn.Broker == nil {
		return "", errBrokerURL
	}

	switch conn.Broker.Scheme {
	case SchemePlain:
		return "tcp://" + conn.Broker.Host, nil
	case SchemeTLS:
		return "ssl://" + conn.Broker.Host, nil
	default:
		return "", errBrokerURL
	}
}

// tlsConfig is the TLS configuration of settings: the system pool unless a CA
// file is set, the client certificate when both files are set, the server
// name when set, TLS 1.2 at least.
func tlsConfig(settings *TLS) (*tls.Config, error) {
	conf := &tls.Config{MinVersion: tls.VersionTLS12}
	if settings == nil {
		return conf, nil
	}

	if settings.CAFile != "" {
		pemBytes, err := os.ReadFile(settings.CAFile)
		if err != nil {
			return nil, errCAFile
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, errCAFile
		}
		conf.RootCAs = pool
	}

	if settings.CertFile != "" && settings.KeyFile != "" {
		pair, err := tls.LoadX509KeyPair(settings.CertFile, settings.KeyFile)
		if err != nil {
			return nil, errClientCertificate
		}
		conf.Certificates = []tls.Certificate{pair}
	}

	conf.ServerName = settings.ServerName

	return conf, nil
}

// Connect implements client.
func (c *pahoClient) Connect(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	token := c.lib.Connect()
	select {
	case <-token.Done():
	case <-ctx.Done():
		// paho finishes the attempt in the background; Disconnect waits for
		// it there and closes what it opened.
		c.lib.Disconnect(0)

		return false, ctx.Err()
	}

	if token.Error() == nil {
		ct, _ := token.(*paho.ConnectToken)
		if ct == nil {
			return false, nil
		}

		return ct.SessionPresent(), nil
	}

	return false, connectError(token)
}

// connectError maps a failed connect to a fixed error.
func connectError(token paho.Token) error {
	if ct, ok := token.(*paho.ConnectToken); ok {
		if err, refused := connackRefusalsByCode[ct.ReturnCode()]; refused {
			return err
		}
	}

	err := token.Error()
	var verification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &verification) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return errTLSVerify
	}

	return errConnection
}

// Subscribe implements client.
func (c *pahoClient) Subscribe(ctx context.Context, topics []string, qos byte) ([]string, error) {
	if len(topics) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	filters := make(map[string]byte, len(topics))
	for _, topic := range topics {
		filters[topic] = qos
	}

	token := c.lib.SubscribeMultiple(filters, nil)
	if err := wait(ctx, token); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}

		return nil, errSubscribe
	}

	st, ok := token.(*paho.SubscribeToken)
	if !ok {
		return nil, nil
	}

	results := st.Result()
	var refused []string
	for _, topic := range topics {
		if code, answered := results[topic]; answered && code == subscribeFailure && !slices.Contains(refused, topic) {
			refused = append(refused, topic)
		}
	}

	return refused, nil
}

// Unsubscribe implements client.
func (c *pahoClient) Unsubscribe(ctx context.Context, topics []string) error {
	if len(topics) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := wait(ctx, c.lib.Unsubscribe(topics...)); err != nil {
		if ctx.Err() != nil {
			return err
		}

		return errUnsubscribe
	}

	return nil
}

// Disconnect implements client.
func (c *pahoClient) Disconnect() {
	c.lib.Disconnect(disconnectQuiesce)
}

// Lost implements client.
func (c *pahoClient) Lost() <-chan error { return c.lost }

// wait waits for token or ctx; it returns ctx.Err() when ctx ends first,
// else the token's error.
func wait(ctx context.Context, token paho.Token) error {
	select {
	case <-token.Done():
		return token.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}
