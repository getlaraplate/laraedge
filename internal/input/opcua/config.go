// Package opcua is the OPC UA input driver: the parsing and validation of the
// connection, of the NodeIds and of the points (this file). The driver is
// read-only (parent 9.1): it never sends a Write, a Call or a node management
// request.
//
// Every error of this file is a fixed message: it names the field, never the
// value, so an endpoint, a user name or a rejected setting cannot reach a log
// (global constraint on connection settings). The factory wraps the errors with
// the input and the point position.
package opcua

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gopcua/opcua/ua"

	"github.com/getlaraplate/laraedge/internal/config"
)

// Security policies of the connection (spec section 6).
const (
	// PolicyNone is no security: neither signing nor encryption.
	PolicyNone = "None"
	// PolicyBasic256Sha256 is the default policy.
	PolicyBasic256Sha256 = "Basic256Sha256"
	// PolicyAes128Sha256RsaOaep is Aes128_Sha256_RsaOaep.
	PolicyAes128Sha256RsaOaep = "Aes128_Sha256_RsaOaep"
	// PolicyAes256Sha256RsaPss is Aes256_Sha256_RsaPss.
	PolicyAes256Sha256RsaPss = "Aes256_Sha256_RsaPss"
)

// Message security modes of the connection (spec section 6). ModeNone is not
// a document value: it is what the mode becomes under PolicyNone.
const (
	// ModeNone is the effective mode of PolicyNone.
	ModeNone = "None"
	// ModeSign signs the messages.
	ModeSign = "Sign"
	// ModeSignAndEncrypt signs and encrypts the messages; the default.
	ModeSignAndEncrypt = "SignAndEncrypt"
)

// Defaults and limits of the connection (spec section 6).
const (
	// DefaultSecurityPolicy is the policy when the document gives none.
	DefaultSecurityPolicy = PolicyBasic256Sha256
	// DefaultSecurityMode is the mode when the document gives none.
	DefaultSecurityMode = ModeSignAndEncrypt
	// DefaultPublishingInterval is the subscription's publishing interval.
	DefaultPublishingInterval = time.Second
	// MinPublishingInterval is the shortest publishing interval: the protocol
	// carries it in whole milliseconds.
	MinPublishingInterval = time.Millisecond
	// EndpointScheme starts every endpoint.
	EndpointScheme = "opc.tcp://"
	// SecretScheme starts the reference of a secret.
	SecretScheme = "secret://"
)

// Connection is the parsed `connection` of an OPC UA input.
type Connection struct {
	// Endpoint is the server's opc.tcp:// URL.
	Endpoint string
	// SecurityPolicy is one of the Policy constants.
	SecurityPolicy string
	// SecurityMode is ModeSign or ModeSignAndEncrypt, or ModeNone under
	// PolicyNone (the document's mode is ignored there).
	SecurityMode string
	// User and Password are the user name identity; both empty for anonymous.
	// Password is the secret:// reference: the driver resolves it at every
	// connect, the value never passes through the configuration.
	User     string
	Password string
	// PublishingInterval is the subscription's publishing interval.
	PublishingInterval time.Duration
}

var (
	connectionFields = []string{"endpoint", "security_policy", "security_mode", "auth", "publishing_interval"}
	authFields       = []string{"user", "password"}
	securityPolicies = []string{PolicyNone, PolicyBasic256Sha256, PolicyAes128Sha256RsaOaep, PolicyAes256Sha256RsaPss}
	securityModes    = []string{ModeSign, ModeSignAndEncrypt}
)

const (
	endpointShape   = "connection.endpoint: want opc.tcp://<host>[:<port>]"
	authShape       = `connection.auth: want "anonymous" or {user, password}`
	intervalShape   = "connection.publishing_interval: want a duration such as 1s"
	intervalMinimum = "connection.publishing_interval: must be at least 1ms"
)

// ParseConnection validates the `connection` object of an OPC UA input and
// applies the defaults of spec section 6. With PolicyNone the document's
// security_mode is checked but ignored.
func ParseConnection(raw json.RawMessage) (Connection, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Connection{}, errors.New("connection: must be an object")
	}
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(connectionFields, key) {
			return Connection{}, errors.New("connection." + key + ": unknown field")
		}
	}

	var conn Connection
	if err := parseEndpoint(fields, &conn); err != nil {
		return Connection{}, err
	}

	conn.SecurityPolicy = DefaultSecurityPolicy
	if _, ok := fields["security_policy"]; ok {
		if err := stringField(fields, "connection.security_policy", "security_policy", &conn.SecurityPolicy); err != nil {
			return Connection{}, err
		}
		if !slices.Contains(securityPolicies, conn.SecurityPolicy) {
			return Connection{}, errors.New("connection.security_policy: want None, Basic256Sha256, Aes128_Sha256_RsaOaep or Aes256_Sha256_RsaPss")
		}
	}
	conn.SecurityMode = DefaultSecurityMode
	if _, ok := fields["security_mode"]; ok {
		if err := stringField(fields, "connection.security_mode", "security_mode", &conn.SecurityMode); err != nil {
			return Connection{}, err
		}
		if !slices.Contains(securityModes, conn.SecurityMode) {
			return Connection{}, errors.New("connection.security_mode: want Sign or SignAndEncrypt")
		}
	}
	if conn.SecurityPolicy == PolicyNone {
		conn.SecurityMode = ModeNone
	}

	if v, ok := fields["auth"]; ok {
		if err := parseAuth(v, &conn); err != nil {
			return Connection{}, err
		}
	}
	if conn.User != "" && conn.SecurityPolicy == PolicyNone {
		return Connection{}, errors.New("connection.auth: user and password need a security_policy other than None")
	}

	conn.PublishingInterval = DefaultPublishingInterval
	if v, ok := fields["publishing_interval"]; ok {
		var text string
		if err := json.Unmarshal(v, &text); err != nil {
			return Connection{}, errors.New(intervalShape)
		}
		d, err := time.ParseDuration(text)
		if err != nil {
			return Connection{}, errors.New(intervalShape)
		}
		if d < MinPublishingInterval {
			return Connection{}, errors.New(intervalMinimum)
		}
		conn.PublishingInterval = d
	}
	return conn, nil
}

func parseEndpoint(fields map[string]json.RawMessage, conn *Connection) error {
	if err := stringField(fields, "connection.endpoint", "endpoint", &conn.Endpoint); err != nil {
		return err
	}
	if conn.Endpoint == "" {
		return errors.New("connection.endpoint: required")
	}
	if !strings.HasPrefix(conn.Endpoint, EndpointScheme) {
		return errors.New(endpointShape)
	}
	u, err := url.Parse(conn.Endpoint)
	if err != nil || u.Hostname() == "" {
		return errors.New(endpointShape)
	}
	if u.User != nil {
		return errors.New("connection.endpoint: must not contain credentials")
	}
	return nil
}

// parseAuth decodes `auth`: the string "anonymous", or an object holding a
// user and the secret:// reference of its password.
func parseAuth(raw json.RawMessage, conn *Connection) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New(authShape)
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text != "anonymous" {
			return errors.New(authShape)
		}
		return nil
	case '{':
	default:
		return errors.New(authShape)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return errors.New(authShape)
	}
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(authFields, key) {
			return errors.New("connection.auth." + key + ": unknown field")
		}
	}
	if err := stringField(fields, "connection.auth.user", "user", &conn.User); err != nil {
		return err
	}
	if conn.User == "" {
		return errors.New("connection.auth.user: required")
	}
	if _, ok := fields["password"]; !ok {
		return errors.New("connection.auth.password: required")
	}
	if err := stringField(fields, "connection.auth.password", "password", &conn.Password); err != nil {
		return err
	}
	if conn.Password == "" {
		return errors.New("connection.auth.password: required")
	}
	if rest, ok := strings.CutPrefix(conn.Password, SecretScheme); !ok || rest == "" {
		return errors.New("connection.auth.password: must be a secret:// reference")
	}
	return nil
}

// stringField decodes the JSON string at key into dst when the key is present;
// name is the path the error names.
func stringField(fields map[string]json.RawMessage, name, key string, dst *string) error {
	v, ok := fields[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(v, dst); err != nil {
		return errors.New(name + ": must be a string")
	}
	return nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Node is a parsed point address. A NodeId with a namespace index (or none) is
// in ID. A NodeId with a namespace URI (`nsu=`) cannot be built before the
// connection: the URI is looked up in the server's NamespaceArray, which the
// driver reads after every connect. It holds NamespaceURI and Rest, the
// identifier part (`s=Name`, `i=17`), and a nil ID.
type Node struct {
	// ID is the NodeId, nil for an `nsu=` address.
	ID *ua.NodeID
	// NamespaceURI is the namespace of an `nsu=` address, empty otherwise.
	NamespaceURI string
	// Rest is the identifier part of an `nsu=` address, empty otherwise.
	Rest string
}

const (
	nodeShape      = "address: want ns=<n>;<i|s|g|b>=<id>, <i|s|g|b>=<id> or nsu=<uri>;<i|s|g|b>=<id>"
	nodeIdentifier = "address: identifier: invalid"
)

// ParseNode parses a point address, a NodeId in the textual form of the
// specification: `ns=3;s=Machine.State`, `i=2258`, `ns=2;g=<guid>`,
// `ns=2;b=<base64>`, or `nsu=<uri>;s=Name`. The identifier needs its type
// letter (a bare `Name` is refused: a mistyped letter would otherwise name
// another node). It checks the syntax and ranges only.
func ParseNode(s string) (Node, error) {
	if s == "" {
		return Node{}, errors.New("address: required")
	}

	if after, ok := strings.CutPrefix(s, "nsu="); ok {
		uri, rest, found := strings.Cut(after, ";")
		switch {
		case !found || !hasIdentifierType(rest):
			return Node{}, errors.New(nodeShape)
		case uri == "":
			return Node{}, errors.New("address: namespace uri: required")
		}
		if _, err := ua.ParseNodeID("ns=0;" + rest); err != nil {
			return Node{}, errors.New(nodeIdentifier)
		}
		return Node{NamespaceURI: uri, Rest: rest}, nil
	}

	identifier := s
	if after, ok := strings.CutPrefix(s, "ns="); ok {
		index, rest, found := strings.Cut(after, ";")
		if !found {
			return Node{}, errors.New(nodeShape)
		}
		if _, ok := digits(index, math.MaxUint16); !ok {
			return Node{}, errors.New("address: namespace index: want an integer from 0 to 65535")
		}
		identifier = rest
	}
	if !hasIdentifierType(identifier) {
		return Node{}, errors.New(nodeShape)
	}
	id, err := ua.ParseNodeID(s)
	if err != nil || id == nil {
		return Node{}, errors.New(nodeIdentifier)
	}
	return Node{ID: id}, nil
}

// hasIdentifierType reports whether s is an i=, s=, g= or b= identifier with a
// non-empty value.
func hasIdentifierType(s string) bool {
	return len(s) >= 3 && s[1] == '=' && strings.IndexByte("isgb", s[0]) >= 0
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

const deadbandShape = "deadband: want a non-negative number or N% up to 100"

// ParseDeadband parses the deadband of a subscribed point: a non-negative
// number (an absolute change) or `N%` with N from 0 to 100 (a percentage of the
// value's range, which the server needs an EURange to apply). kind is the
// DeadbandType of the OPC UA DataChangeFilter, ua.DeadbandTypeAbsolute or
// ua.DeadbandTypePercent; the empty string is no deadband, ua.DeadbandTypeNone.
func ParseDeadband(s string) (kind uint32, value float64, err error) {
	if s == "" {
		return uint32(ua.DeadbandTypeNone), 0, nil
	}
	number, isPercent := strings.CutSuffix(s, "%")
	if !isDecimal(number) {
		return 0, 0, errors.New(deadbandShape)
	}
	value, err = strconv.ParseFloat(number, 64)
	if err != nil || math.IsInf(value, 0) || (isPercent && value > 100) {
		return 0, 0, errors.New(deadbandShape)
	}
	if isPercent {
		return uint32(ua.DeadbandTypePercent), value, nil
	}
	return uint32(ua.DeadbandTypeAbsolute), value, nil
}

// isDecimal reports whether s is digits with an optional fraction: no sign,
// exponent, hexadecimal or special value.
func isDecimal(s string) bool {
	whole, fraction, hasFraction := strings.Cut(s, ".")
	allDigits := func(t string) bool {
		return t != "" && strings.IndexFunc(t, func(r rune) bool { return r < '0' || r > '9' }) < 0
	}
	return allDigits(whole) && (!hasFraction || allDigits(fraction))
}

// pointTypes are the decoding types of a point (decision O8).
var pointTypes = []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64", "float32", "float64", "string"}

// CheckPoint validates a point apart from its address: the read mode (subscribe
// or poll), the required type, the absence of a byte order and the deadband of
// a subscription. The address is checked by ParseNode.
func CheckPoint(p config.Point) error {
	if p.Read.Mode != config.ReadPoll && p.Read.Mode != config.ReadSubscribe {
		return errors.New("read: want subscribe or poll")
	}
	if p.Type == "" {
		return errors.New("type: required")
	}
	if !slices.Contains(pointTypes, p.Type) {
		return errors.New("type: want bool, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string")
	}
	if p.ByteOrder != "" {
		return errors.New("byte_order: not for opcua")
	}
	if p.Read.Subscribe != nil {
		if _, _, err := ParseDeadband(p.Read.Subscribe.Deadband); err != nil {
			return err
		}
	}
	return nil
}
