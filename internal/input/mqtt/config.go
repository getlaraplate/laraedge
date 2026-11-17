// Package mqtt is the MQTT input driver: the parsing and validation of the
// connection, of the point addresses and of the points (this file). The driver
// never publishes (spec section 1: the agent is a plain subscriber).
//
// Every error of this file is a fixed message: it names the field, never the
// value, so a broker address, a user name, a topic or a rejected setting cannot
// reach a log (global constraint on connection settings). The factory wraps the
// errors with the input and the point position.
package mqtt

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/docrules"
)

// Schemes of the broker URL and their default ports (spec section 3).
const (
	// SchemePlain is a TCP connection without TLS.
	SchemePlain = "mqtt"
	// SchemeTLS is a TCP connection with TLS.
	SchemeTLS = "mqtts"
	// DefaultPortPlain is the port of SchemePlain without an explicit one.
	DefaultPortPlain = "1883"
	// DefaultPortTLS is the port of SchemeTLS without an explicit one.
	DefaultPortTLS = "8883"
	// SecretScheme starts the reference of a secret.
	SecretScheme = "secret://"
)

// Payload formats (spec section 3).
const (
	// FormatJSON reads JSON and plain payloads; the default.
	FormatJSON = "json"
	// FormatSparkplugB is Sparkplug B (spec section 7).
	FormatSparkplugB = "sparkplug_b"
)

// Timestamp formats of a payload timestamp (spec section 3).
const (
	// TimestampRFC3339 is a text timestamp in RFC 3339.
	TimestampRFC3339 = "rfc3339"
	// TimestampUnixSeconds is a number of seconds since the epoch.
	TimestampUnixSeconds = "unix_s"
	// TimestampUnixMillis is a number of milliseconds since the epoch.
	TimestampUnixMillis = "unix_ms"
)

// Defaults and limits of the connection (spec section 3, decision Q5).
const (
	// DefaultQoS is the QoS of the subscriptions.
	DefaultQoS byte = 1
	// DefaultPersistentSession keeps the broker-side session across restarts.
	DefaultPersistentSession = true
	// DefaultKeepAlive is the MQTT keep alive.
	DefaultKeepAlive = 30 * time.Second
	// DefaultMaxPayload is the largest message accepted, 256 KiB.
	DefaultMaxPayload = 256 << 10
	// MaxPayloadLimit is the largest max_payload the document may set.
	MaxPayloadLimit = 16 << 20
)

// Connection is the parsed `connection` of an MQTT input.
type Connection struct {
	// Broker is the broker URL with the port filled in.
	Broker *url.URL
	// ClientID is the document's client_id; empty when not set (the input
	// applies the default laraplate-edge-<agent_id>-<input_id>).
	ClientID string
	// Auth is the user name identity; nil for anonymous.
	Auth *Auth
	// TLS holds the TLS settings; nil for the system defaults.
	TLS *TLS
	// Format is FormatJSON or FormatSparkplugB.
	Format string
	// Rebirth asks an edge node to publish its births again when the input
	// misses a message (spec section 7); only with FormatSparkplugB.
	Rebirth bool
	// QoS is the QoS of the subscriptions, 0 or 1.
	QoS byte
	// PersistentSession is clean session false in 3.1.1 terms.
	PersistentSession bool
	// KeepAlive is the MQTT keep alive.
	KeepAlive time.Duration
	// MaxPayload is the largest message accepted, in bytes.
	MaxPayload int
	// Timestamp reads the sample time from the payload; nil: reception time.
	Timestamp *TimestampSpec
	// Topics are the per-topic overrides, keyed by topic.
	Topics map[string]TopicOverride
	// Availability is the availability topic of a device, keyed by device.
	Availability map[string]Availability
}

// Auth is a user name identity.
type Auth struct {
	// User is the user name.
	User string
	// Password is the secret:// reference: the driver resolves it at every
	// connect, the value never passes through the configuration.
	Password string
}

// TLS holds the TLS settings of an mqtts:// connection.
type TLS struct {
	// CAFile is a PEM file of trusted authorities; empty: the system pool.
	CAFile string
	// CertFile and KeyFile are the client certificate and key (mutual TLS),
	// both set or both empty.
	CertFile string
	KeyFile  string
	// ServerName overrides the name the server certificate is checked for.
	ServerName string
}

// TimestampSpec says where the sample time is in a JSON payload.
type TimestampSpec struct {
	// Path is the dot path of the timestamp, as in an address.
	Path string
	// Format is TimestampRFC3339, TimestampUnixSeconds or TimestampUnixMillis.
	Format string
}

// TopicOverride is the per-topic override of the connection settings.
type TopicOverride struct {
	// Timestamp replaces the connection's timestamp for this topic.
	Timestamp *TimestampSpec
}

// Availability is the availability topic of one device.
type Availability struct {
	// Topic is the concrete topic the device publishes its availability on.
	Topic string
	// Online and Offline are the exact payloads of the two states.
	Online  string
	Offline string
}

var (
	connectionFields   = []string{"broker", "client_id", "auth", "tls", "format", "qos", "persistent_session", "keepalive", "max_payload", "timestamp", "topics", "availability", "rebirth"}
	authFields         = []string{"user", "password"}
	tlsFields          = []string{"ca_file", "cert_file", "key_file", "server_name"}
	timestampFields    = []string{"path", "format"}
	topicOverrideKeys  = []string{"timestamp"}
	availabilityFields = []string{"device", "topic", "online", "offline"}
	timestampFormats   = []string{TimestampRFC3339, TimestampUnixSeconds, TimestampUnixMillis}
)

const (
	brokerShape     = "connection.broker: want mqtt://<host>[:<port>] or mqtts://<host>[:<port>]"
	authShape       = `connection.auth: want "anonymous" or {user, password}`
	keepAliveShape  = "connection.keepalive: want a positive duration such as 30s"
	maxPayloadShape = "connection.max_payload: want 1 byte to 16MiB, as bytes or a size such as 256KiB"
	topicsKeyShape  = "connection.topics: topic must be non-empty without + or #"
	topicsKeyUnused = "connection.topics: a key is not the topic of a point"
	notSparkplug    = ": not with format sparkplug_b"
)

// ParseConnection validates the `connection` object of an MQTT input and
// applies the defaults of spec section 3. cfg is the input the connection
// belongs to: the availability entries name its devices.
func ParseConnection(raw json.RawMessage, cfg config.Input) (Connection, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Connection{}, errors.New("connection: must be an object")
	}
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(connectionFields, key) {
			return Connection{}, docrules.UnknownKey("connection.", key)
		}
	}

	var conn Connection
	if err := parseBroker(fields, &conn); err != nil {
		return Connection{}, err
	}
	if v, ok := fields["tls"]; ok {
		if conn.Broker.Scheme != SchemeTLS {
			return Connection{}, errors.New("connection.tls: needs an mqtts:// broker")
		}
		tlsSettings, err := parseTLS(v)
		if err != nil {
			return Connection{}, err
		}
		conn.TLS = tlsSettings
	}
	if v, ok := fields["auth"]; ok {
		auth, err := parseAuth(v)
		if err != nil {
			return Connection{}, err
		}
		conn.Auth = auth
	}
	if _, ok := fields["client_id"]; ok {
		if err := stringField(fields, "connection.client_id", "client_id", &conn.ClientID); err != nil {
			return Connection{}, err
		}
		if conn.ClientID == "" {
			return Connection{}, errors.New("connection.client_id: must not be empty")
		}
	}
	if err := parseFormat(fields, &conn); err != nil {
		return Connection{}, err
	}
	if err := parseRebirth(fields, &conn); err != nil {
		return Connection{}, err
	}
	if err := parseSession(fields, &conn); err != nil {
		return Connection{}, err
	}

	if conn.Format == FormatSparkplugB {
		for _, key := range []string{"timestamp", "topics", "availability"} {
			if _, ok := fields[key]; ok {
				return Connection{}, errors.New("connection." + key + notSparkplug)
			}
		}
		return conn, nil
	}

	if v, ok := fields["timestamp"]; ok {
		spec, err := parseTimestamp(v, "connection.timestamp")
		if err != nil {
			return Connection{}, err
		}
		conn.Timestamp = spec
	}
	if v, ok := fields["topics"]; ok {
		topics, err := parseTopics(v, pointTopics(cfg))
		if err != nil {
			return Connection{}, err
		}
		conn.Topics = topics
	}
	if v, ok := fields["availability"]; ok {
		availability, err := parseAvailability(v, cfg, pointTopics(cfg))
		if err != nil {
			return Connection{}, err
		}
		conn.Availability = availability
	}
	return conn, nil
}

// parseBroker decodes `broker`, required, and fills in the default port when
// the URL has none; a colon without a port (mqtt://b:) is refused.
func parseBroker(fields map[string]json.RawMessage, conn *Connection) error {
	var text string
	if err := stringField(fields, "connection.broker", "broker", &text); err != nil {
		return err
	}
	if text == "" {
		return errors.New("connection.broker: required")
	}
	u, err := url.Parse(text)
	if err != nil || (u.Scheme != SchemePlain && u.Scheme != SchemeTLS) || u.Hostname() == "" || u.Opaque != "" {
		return errors.New(brokerShape)
	}
	if u.User != nil {
		return errors.New("connection.broker: must not contain credentials")
	}
	if u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(text, "?#") {
		return errors.New("connection.broker: must not contain a path or query")
	}
	if strings.HasSuffix(u.Host, ":") {
		return errors.New("connection.broker: empty port")
	}
	port := u.Port()
	switch {
	case port == "" && u.Scheme == SchemeTLS:
		port = DefaultPortTLS
	case port == "":
		port = DefaultPortPlain
	default:
		if n, ok := digits(port, 65535); !ok || n == 0 {
			return errors.New(brokerShape)
		}
	}
	u.Host = net.JoinHostPort(u.Hostname(), port)
	conn.Broker = u
	return nil
}

// parseTLS decodes `tls`: an object of file paths and a server name. A client
// certificate needs its key and the other way round. There is no option to
// skip the verification of the server certificate.
func parseTLS(raw json.RawMessage) (*TLS, error) {
	fields, err := objectFields(raw, "connection.tls")
	if err != nil {
		return nil, err
	}
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(tlsFields, key) {
			return nil, docrules.UnknownKey("connection.tls.", key)
		}
	}
	var t TLS
	for _, f := range []struct {
		key string
		dst *string
	}{{"ca_file", &t.CAFile}, {"cert_file", &t.CertFile}, {"key_file", &t.KeyFile}, {"server_name", &t.ServerName}} {
		if err := stringField(fields, "connection.tls."+f.key, f.key, f.dst); err != nil {
			return nil, err
		}
	}
	switch {
	case t.CertFile != "" && t.KeyFile == "":
		return nil, errors.New("connection.tls.cert_file: needs key_file")
	case t.KeyFile != "" && t.CertFile == "":
		return nil, errors.New("connection.tls.key_file: needs cert_file")
	}
	return &t, nil
}

// parseAuth decodes `auth`: the string "anonymous" (nil result), or an object
// holding a user and the secret:// reference of its password.
func parseAuth(raw json.RawMessage) (*Auth, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New(authShape)
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text != "anonymous" {
			return nil, errors.New(authShape)
		}
		return nil, nil
	case '{':
	default:
		return nil, errors.New(authShape)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return nil, errors.New(authShape)
	}
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(authFields, key) {
			return nil, docrules.UnknownKey("connection.auth.", key)
		}
	}
	var a Auth
	if err := stringField(fields, "connection.auth.user", "user", &a.User); err != nil {
		return nil, err
	}
	if a.User == "" {
		return nil, errors.New("connection.auth.user: required")
	}
	if _, ok := fields["password"]; !ok {
		return nil, errors.New("connection.auth.password: required")
	}
	if err := stringField(fields, "connection.auth.password", "password", &a.Password); err != nil {
		return nil, err
	}
	if a.Password == "" {
		return nil, errors.New("connection.auth.password: required")
	}
	if rest, ok := strings.CutPrefix(a.Password, SecretScheme); !ok || rest == "" {
		return nil, errors.New("connection.auth.password: must be a secret:// reference")
	}
	return &a, nil
}

// parseFormat decodes `format`: json, the default, or sparkplug_b.
func parseFormat(fields map[string]json.RawMessage, conn *Connection) error {
	conn.Format = FormatJSON
	if _, ok := fields["format"]; !ok {
		return nil
	}
	var format string
	if err := stringField(fields, "connection.format", "format", &format); err != nil {
		return err
	}
	switch format {
	case FormatJSON:
		return nil
	case FormatSparkplugB:
		conn.Format = FormatSparkplugB
		return nil
	default:
		return errors.New("connection.format: want json or sparkplug_b")
	}
}

// parseRebirth decodes `rebirth`, a boolean that needs format sparkplug_b.
func parseRebirth(fields map[string]json.RawMessage, conn *Connection) error {
	v, ok := fields["rebirth"]
	if !ok {
		return nil
	}
	if conn.Format != FormatSparkplugB {
		return errors.New("connection.rebirth: needs format sparkplug_b")
	}
	if err := json.Unmarshal(v, &conn.Rebirth); err != nil || isNull(v) {
		return errors.New("connection.rebirth: must be a boolean")
	}
	return nil
}

// parseSession decodes qos, persistent_session, keepalive and max_payload,
// with their defaults.
func parseSession(fields map[string]json.RawMessage, conn *Connection) error {
	conn.QoS = DefaultQoS
	if v, ok := fields["qos"]; ok {
		n, err := strconv.ParseInt(string(bytes.TrimSpace(v)), 10, 8)
		if err != nil || n < 0 || n > 1 {
			return errors.New("connection.qos: want 0 or 1")
		}
		conn.QoS = byte(n)
	}

	conn.PersistentSession = DefaultPersistentSession
	if v, ok := fields["persistent_session"]; ok {
		if isNull(v) || json.Unmarshal(v, &conn.PersistentSession) != nil {
			return errors.New("connection.persistent_session: must be true or false")
		}
	}

	conn.KeepAlive = DefaultKeepAlive
	if v, ok := fields["keepalive"]; ok {
		var text string
		if err := json.Unmarshal(v, &text); err != nil {
			return errors.New(keepAliveShape)
		}
		d, err := time.ParseDuration(text)
		if err != nil || d <= 0 {
			return errors.New(keepAliveShape)
		}
		conn.KeepAlive = d
	}

	conn.MaxPayload = DefaultMaxPayload
	if v, ok := fields["max_payload"]; ok {
		n, err := parseSize(v)
		if err != nil {
			return err
		}
		conn.MaxPayload = n
	}
	return nil
}

// parseSize decodes max_payload: an integer number of bytes or a string with
// the suffix KiB or MiB, from 1 byte to MaxPayloadLimit (decision Q5).
func parseSize(raw json.RawMessage) (int, error) {
	trimmed := bytes.TrimSpace(raw)
	var n uint64
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return 0, errors.New(maxPayloadShape)
		}
		unit := uint64(0)
		switch {
		case strings.HasSuffix(text, "KiB"):
			unit, text = 1<<10, strings.TrimSuffix(text, "KiB")
		case strings.HasSuffix(text, "MiB"):
			unit, text = 1<<20, strings.TrimSuffix(text, "MiB")
		default:
			return 0, errors.New(maxPayloadShape)
		}
		count, ok := digits(text, MaxPayloadLimit)
		if !ok {
			return 0, errors.New(maxPayloadShape)
		}
		n = count * unit
	} else {
		count, err := strconv.ParseUint(string(trimmed), 10, 64)
		if err != nil {
			return 0, errors.New(maxPayloadShape)
		}
		n = count
	}
	if n < 1 || n > MaxPayloadLimit {
		return 0, errors.New(maxPayloadShape)
	}
	return int(n), nil
}

// parseTimestamp decodes a {path, format} object; name is the path the errors
// name.
func parseTimestamp(raw json.RawMessage, name string) (*TimestampSpec, error) {
	fields, err := objectFields(raw, name)
	if err != nil {
		return nil, err
	}
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(timestampFields, key) {
			return nil, docrules.UnknownKey(name+".", key)
		}
	}
	var spec TimestampSpec
	if err := stringField(fields, name+".path", "path", &spec.Path); err != nil {
		return nil, err
	}
	if spec.Path == "" {
		return nil, errors.New(name + ".path: required")
	}
	if _, err := splitPath(spec.Path); err != nil {
		return nil, errors.New(name + ".path: " + err.Error())
	}
	if err := stringField(fields, name+".format", "format", &spec.Format); err != nil {
		return nil, err
	}
	if spec.Format == "" {
		return nil, errors.New(name + ".format: required")
	}
	if !slices.Contains(timestampFormats, spec.Format) {
		return nil, errors.New(name + ".format: want rfc3339, unix_s or unix_ms")
	}
	return &spec, nil
}

// pointTopics returns the topics of the points of cfg, or nil when an
// address does not parse: the point check then refuses the document with its
// own message, so the cross checks against point topics are skipped.
func pointTopics(cfg config.Input) map[string]bool {
	topics := make(map[string]bool)
	for _, dev := range cfg.Devices {
		for _, p := range dev.Points {
			addr, err := ParseAddress(p.Address)
			if err != nil {
				return nil
			}
			topics[addr.Topic] = true
		}
	}
	return topics
}

// parseTopics decodes `topics`: per-topic overrides keyed by a concrete topic
// that a point of the input uses (pointTopics; nil skips that check). The
// topic is never named in an error, since it comes from the document.
func parseTopics(raw json.RawMessage, pointTopics map[string]bool) (map[string]TopicOverride, error) {
	entries, err := objectFields(raw, "connection.topics")
	if err != nil {
		return nil, err
	}
	topics := make(map[string]TopicOverride, len(entries))
	for _, topic := range sortedKeys(entries) {
		if topicProblem(topic) != "" {
			return nil, errors.New(topicsKeyShape)
		}
		fields, err := objectFields(entries[topic], "connection.topics.*")
		if err != nil {
			return nil, err
		}
		for _, key := range sortedKeys(fields) {
			if !slices.Contains(topicOverrideKeys, key) {
				return nil, docrules.UnknownKey("connection.topics.*.", key)
			}
		}
		var override TopicOverride
		if v, ok := fields["timestamp"]; ok {
			spec, err := parseTimestamp(v, "connection.topics.*.timestamp")
			if err != nil {
				return nil, err
			}
			override.Timestamp = spec
		}
		if pointTopics != nil && !pointTopics[topic] {
			return nil, errors.New(topicsKeyUnused)
		}
		topics[topic] = override
	}
	return topics, nil
}

// parseAvailability decodes `availability`: a list of entries, at most one per
// device, each naming a device of cfg on a topic no point uses (pointTopics;
// nil skips that check): the router gives an availability topic to the
// tracker only, so a point on it would never have a sample.
func parseAvailability(raw json.RawMessage, cfg config.Input, pointTopics map[string]bool) (map[string]Availability, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, errors.New("connection.availability: must be a list")
	}
	result := make(map[string]Availability, len(entries))
	for i, entry := range entries {
		name := "connection.availability[" + strconv.Itoa(i) + "]"
		device, a, err := parseAvailabilityEntry(entry, name, cfg)
		if err != nil {
			return nil, err
		}
		if pointTopics[a.Topic] {
			return nil, errors.New(name + ".topic: also the topic of a point")
		}
		if _, dup := result[device]; dup {
			return nil, errors.New(name + ".device: already has an availability entry")
		}
		result[device] = a
	}
	return result, nil
}

func parseAvailabilityEntry(raw json.RawMessage, name string, cfg config.Input) (string, Availability, error) {
	fields, err := objectFields(raw, name)
	if err != nil {
		return "", Availability{}, err
	}
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(availabilityFields, key) {
			return "", Availability{}, docrules.UnknownKey(name+".", key)
		}
	}

	var device string
	if err := stringField(fields, name+".device", "device", &device); err != nil {
		return "", Availability{}, err
	}
	if device == "" {
		return "", Availability{}, errors.New(name + ".device: required")
	}
	if !slices.ContainsFunc(cfg.Devices, func(d config.Device) bool { return d.Device == device }) {
		return "", Availability{}, errors.New(name + ".device: not a device of this input")
	}

	var a Availability
	if err := stringField(fields, name+".topic", "topic", &a.Topic); err != nil {
		return "", Availability{}, err
	}
	switch topicProblem(a.Topic) {
	case "":
	case problemEmpty:
		return "", Availability{}, errors.New(name + ".topic: required")
	default:
		return "", Availability{}, errors.New(name + ".topic: " + topicProblem(a.Topic))
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"online", &a.Online}, {"offline", &a.Offline}} {
		if err := stringField(fields, name+"."+f.key, f.key, f.dst); err != nil {
			return "", Availability{}, err
		}
		if *f.dst == "" {
			return "", Availability{}, errors.New(name + "." + f.key + ": required")
		}
	}
	if a.Online == a.Offline {
		return "", Availability{}, errors.New(name + ".offline: must differ from online")
	}
	return device, a, nil
}

// Reasons a topic is refused, as the messages of the fields that carry one.
const (
	problemEmpty    = "required"
	problemWildcard = "must not contain + or #"
	problemNUL      = "must not contain a NUL character"
)

// topicProblem returns why s is not a concrete topic, or "" when it is.
func topicProblem(s string) string {
	switch {
	case s == "":
		return problemEmpty
	case strings.ContainsAny(s, "+#"):
		return problemWildcard
	case strings.ContainsRune(s, 0):
		return problemNUL
	}
	return ""
}

// Address is a parsed point address: a concrete topic and, for a JSON payload,
// the path of the value in it.
type Address struct {
	// Topic is the concrete topic the point subscribes to.
	Topic string
	// Path is the dot path split into segments; nil: the whole payload is the
	// value. A numeric segment stays a string here: the decoder reads it as a
	// list index.
	Path []string
}

// ParseAddress parses `<topic>[|<path>]`, split at the first `|` (spec
// section 4). The topic is concrete (`+` and `#` are refused); the path is dot
// notation, with no empty segment.
func ParseAddress(s string) (Address, error) {
	if s == "" {
		return Address{}, errors.New("address: required")
	}
	topic, path, hasPath := strings.Cut(s, "|")
	if problem := topicProblem(topic); problem != "" {
		return Address{}, errors.New("address: topic: " + problem)
	}
	addr := Address{Topic: topic}
	if !hasPath {
		return addr, nil
	}
	if path == "" {
		return Address{}, errors.New("address: path: must not be empty after |")
	}
	segments, err := splitPath(path)
	if err != nil {
		return Address{}, errors.New("address: path: " + err.Error())
	}
	addr.Path = segments
	return addr, nil
}

// splitPath splits a dot path into its segments.
func splitPath(path string) ([]string, error) {
	segments := strings.Split(path, ".")
	if slices.Contains(segments, "") {
		return nil, errors.New("must not contain an empty segment")
	}
	return segments, nil
}

// pointTypes are the decoding types of a point.
var pointTypes = []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64", "float32", "float64", "string"}

// CheckPoint validates a point apart from its address: the read mode
// (subscribe only, short or long form; the long form's settings are ignored),
// the required type and the absence of a byte order. The address is checked by
// ParseAddress.
func CheckPoint(p config.Point) error {
	if p.Read.Mode != config.ReadSubscribe {
		return errors.New("read: want subscribe")
	}
	if p.Type == "" {
		return errors.New("type: required")
	}
	if !slices.Contains(pointTypes, p.Type) {
		return errors.New("type: want bool, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string")
	}
	if p.ByteOrder != "" {
		return errors.New("byte_order: not for mqtt")
	}
	return nil
}

// objectFields decodes raw as a JSON object; name is the path the error names.
func objectFields(raw json.RawMessage, name string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New(name + ": must be an object")
	}
	return fields, nil
}

// stringField decodes the JSON string at key into dst when the key is present;
// name is the path the error names. A JSON null is not a string: encoding/json
// would leave dst as it was.
func stringField(fields map[string]json.RawMessage, name, key string, dst *string) error {
	v, ok := fields[key]
	if !ok {
		return nil
	}
	if isNull(v) || json.Unmarshal(v, dst) != nil {
		return errors.New(name + ": must be a string")
	}
	return nil
}

// isNull reports whether raw is the JSON literal null.
func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// digits parses a non-empty run of ASCII digits no greater than limit. A sign,
// a space or a prefix is refused.
func digits(s string, limit uint64) (uint64, bool) {
	if s == "" || len(s) > 10 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n > limit {
		return 0, false
	}
	return n, true
}
