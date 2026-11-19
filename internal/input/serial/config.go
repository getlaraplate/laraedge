package serial

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/docrules"
)

// Defaults and limits of the connection (spec sections 4 to 6).
const (
	// DefaultMaxFrame is the longest frame read, in bytes.
	DefaultMaxFrame = 1024
	// MaxFrameLimit is the largest max_frame the document may set.
	MaxFrameLimit = 65536
	// DefaultEncoding is the text encoding of the frames.
	DefaultEncoding = "utf-8"
	// DefaultRequestTimeout is how long an answer is awaited.
	DefaultRequestTimeout = time.Second
	// MinGap is the shortest framing gap.
	MinGap = 20 * time.Millisecond
	// MinEvery is the shortest period of a request.
	MinEvery = 100 * time.Millisecond
	// MaxCommand is the longest request command, in bytes.
	MaxCommand = 64
	// MaxTerminator is the longest frame terminator, in bytes.
	MaxTerminator = 4
)

// Framing says where a frame ends (spec section 5). Exactly one of Terminator,
// Length and Gap is set.
type Framing struct {
	// Terminator ends a frame and is not part of it.
	Terminator []byte
	// Length is the exact size of a frame.
	Length int
	// Gap is the silence that ends a frame.
	Gap time.Duration
	// Start is the byte that begins a frame, when HasStart.
	Start    byte
	HasStart bool
}

// ContextRule sets a context key from a matching frame (spec section 7).
type ContextRule struct {
	// Key is serial, order_ref, operation_ref or lot.
	Key string
	// Match is the expression a frame is tried against.
	Match *regexp.Regexp
	// Group is the capture group holding the value; 0 is the whole match.
	Group int
}

// Request is a command sent to the instrument every Every.
type Request struct {
	Command []byte
	Every   time.Duration
}

// Connection is the parsed `connection` of a serial input.
type Connection struct {
	// Device is the port: /dev/ttyUSB0, a by-id path or COM4.
	Device string
	// Mode is the line setting.
	Mode Mode
	// Framing says where a frame ends.
	Framing Framing
	// MaxFrame is the longest frame read, in bytes.
	MaxFrame int
	// Encoding is utf-8, windows-1252 or iso-8859-1.
	Encoding string
	// Context holds the rules setting the context keys, in order.
	Context []ContextRule
	// ContextTTL drops a context key older than this; 0 keeps it for good.
	ContextTTL time.Duration
	// Requests are the commands sent to the instrument.
	Requests []Request
	// RequestTimeout is how long an answer is awaited before the next command.
	RequestTimeout time.Duration
}

var (
	connectionFields = []string{"device", "baud", "data_bits", "parity", "stop_bits", "framing", "max_frame", "encoding", "context", "context_ttl", "requests", "request_timeout"}
	framingFields    = []string{"terminator", "length", "gap", "start"}
	contextFields    = []string{"key", "match", "group"}
	requestFields    = []string{"command", "every"}
	contextKeys      = []string{"serial", "order_ref", "operation_ref", "lot"}
	encodings        = []string{DefaultEncoding, "windows-1252", "iso-8859-1"}
	pointTypes       = []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64", "float32", "float64", "string"}
)

// ParseConnection validates the `connection` object of a serial input and
// applies the defaults of spec sections 4 to 6. Every error is a fixed message
// naming the field, never the value.
func ParseConnection(raw json.RawMessage) (Connection, error) {
	fields, err := objectFields(raw, "connection")
	if err != nil {
		return Connection{}, err
	}
	for _, key := range sortedKeys(fields) {
		if !slices.Contains(connectionFields, key) {
			return Connection{}, docrules.UnknownKey("connection.", key)
		}
	}

	conn := Connection{
		Framing:  Framing{Terminator: []byte("\r")},
		MaxFrame: DefaultMaxFrame, Encoding: DefaultEncoding, RequestTimeout: DefaultRequestTimeout,
	}
	steps := []func(map[string]json.RawMessage, *Connection) error{
		parseLine, parseMaxFrame, parseFraming, parseEncoding, parseContext, parseRequests,
	}
	for _, step := range steps {
		if err := step(fields, &conn); err != nil {
			return Connection{}, err
		}
	}
	return conn, nil
}

func parseLine(fields map[string]json.RawMessage, conn *Connection) error {
	if err := stringField(fields, "connection.device", "device", &conn.Device); err != nil {
		return err
	}
	if conn.Device == "" {
		return errors.New("connection.device: required")
	}
	conn.Mode = Mode{DataBits: 8, StopBits: 1, Parity: "none"}

	baud, ok := intOf(fields["baud"])
	if !ok || baud < 1 {
		return errors.New("connection.baud: want a positive integer")
	}
	conn.Mode.Baud = baud

	if v, ok := fields["data_bits"]; ok {
		n, ok := intOf(v)
		if !ok || n < 5 || n > 8 {
			return errors.New("connection.data_bits: want 5, 6, 7 or 8")
		}
		conn.Mode.DataBits = n
	}
	if _, ok := fields["parity"]; ok {
		if err := stringField(fields, "connection.parity", "parity", &conn.Mode.Parity); err != nil {
			return err
		}
		if !slices.Contains([]string{"none", "even", "odd"}, conn.Mode.Parity) {
			return errors.New("connection.parity: want none, even or odd")
		}
	}
	if v, ok := fields["stop_bits"]; ok {
		n, ok := intOf(v)
		if !ok || n < 1 || n > 2 {
			return errors.New("connection.stop_bits: want 1 or 2")
		}
		conn.Mode.StopBits = n
	}
	return nil
}

func parseMaxFrame(fields map[string]json.RawMessage, conn *Connection) error {
	v, ok := fields["max_frame"]
	if !ok {
		return nil
	}
	n, ok := intOf(v)
	if !ok || n < 1 || n > MaxFrameLimit {
		return errors.New("connection.max_frame: want 1 to 65536")
	}
	conn.MaxFrame = n
	return nil
}

func parseFraming(fields map[string]json.RawMessage, conn *Connection) error {
	raw, ok := fields["framing"]
	if !ok {
		return nil
	}
	sub, err := objectFields(raw, "connection.framing")
	if err != nil {
		return err
	}
	for _, key := range sortedKeys(sub) {
		if !slices.Contains(framingFields, key) {
			return docrules.UnknownKey("connection.framing.", key)
		}
	}
	set := 0
	for _, key := range []string{"terminator", "length", "gap"} {
		if _, ok := sub[key]; ok {
			set++
		}
	}
	if set != 1 {
		return errors.New("connection.framing: want exactly one of terminator, length, gap")
	}

	f := Framing{}
	if _, ok := sub["terminator"]; ok {
		var text string
		if err := stringField(sub, "connection.framing.terminator", "terminator", &text); err != nil {
			return err
		}
		if len(text) < 1 || len(text) > MaxTerminator {
			return errors.New("connection.framing.terminator: want 1 to 4 bytes")
		}
		f.Terminator = []byte(text)
	}
	if v, ok := sub["length"]; ok {
		n, ok := intOf(v)
		if !ok || n < 1 || n > conn.MaxFrame {
			return errors.New("connection.framing.length: want 1 to max_frame")
		}
		f.Length = n
	}
	if v, ok := sub["gap"]; ok {
		d, ok := durationOf(v)
		if !ok || d < MinGap {
			return errors.New("connection.framing.gap: at least 20ms")
		}
		f.Gap = d
	}
	if _, ok := sub["start"]; ok {
		var text string
		if err := stringField(sub, "connection.framing.start", "start", &text); err != nil {
			return err
		}
		if len(text) != 1 {
			return errors.New("connection.framing.start: want one character")
		}
		f.Start, f.HasStart = text[0], true
	}
	conn.Framing = f
	return nil
}

func parseEncoding(fields map[string]json.RawMessage, conn *Connection) error {
	if _, ok := fields["encoding"]; !ok {
		return nil
	}
	if err := stringField(fields, "connection.encoding", "encoding", &conn.Encoding); err != nil {
		return err
	}
	if !slices.Contains(encodings, conn.Encoding) {
		return errors.New("connection.encoding: want utf-8, windows-1252 or iso-8859-1")
	}
	return nil
}

func parseContext(fields map[string]json.RawMessage, conn *Connection) error {
	if v, ok := fields["context_ttl"]; ok {
		d, ok := durationOf(v)
		if !ok || d <= 0 {
			return errors.New("connection.context_ttl: want a positive duration")
		}
		conn.ContextTTL = d
	}
	rules, err := listOf(fields, "context", "connection.context")
	if err != nil {
		return err
	}
	for i, rawRule := range rules {
		prefix := "connection.context." + strconv.Itoa(i)
		sub, err := objectFields(rawRule, prefix)
		if err != nil {
			return err
		}
		for _, key := range sortedKeys(sub) {
			if !slices.Contains(contextFields, key) {
				return docrules.UnknownKey(prefix+".", key)
			}
		}
		var rule ContextRule
		if err := stringField(sub, prefix+".key", "key", &rule.Key); err != nil {
			return err
		}
		if !slices.Contains(contextKeys, rule.Key) {
			return errors.New(prefix + ".key: want serial, order_ref, operation_ref or lot")
		}
		for _, earlier := range conn.Context {
			if earlier.Key == rule.Key {
				return errors.New(prefix + ".key: duplicate")
			}
		}
		var pattern string
		if err := stringField(sub, prefix+".match", "match", &pattern); err != nil {
			return err
		}
		if rule.Match, err = regexp.Compile(pattern); err != nil || pattern == "" {
			return errors.New(prefix + ".match: invalid expression")
		}
		rule.Group = 1
		if v, ok := sub["group"]; ok {
			n, ok := intOf(v)
			if !ok || n < 0 {
				return errors.New(prefix + ".group: the expression has no such group")
			}
			rule.Group = n
		}
		if rule.Group > rule.Match.NumSubexp() {
			return errors.New(prefix + ".group: the expression has no such group")
		}
		conn.Context = append(conn.Context, rule)
	}
	return nil
}

func parseRequests(fields map[string]json.RawMessage, conn *Connection) error {
	if v, ok := fields["request_timeout"]; ok {
		d, ok := durationOf(v)
		if !ok || d <= 0 {
			return errors.New("connection.request_timeout: want a positive duration")
		}
		conn.RequestTimeout = d
	}
	list, err := listOf(fields, "requests", "connection.requests")
	if err != nil {
		return err
	}
	for i, rawReq := range list {
		prefix := "connection.requests." + strconv.Itoa(i)
		sub, err := objectFields(rawReq, prefix)
		if err != nil {
			return err
		}
		for _, key := range sortedKeys(sub) {
			if !slices.Contains(requestFields, key) {
				return docrules.UnknownKey(prefix+".", key)
			}
		}
		var command string
		if err := stringField(sub, prefix+".command", "command", &command); err != nil {
			return err
		}
		if len(command) < 1 || len(command) > MaxCommand {
			return errors.New(prefix + ".command: want 1 to 64 bytes")
		}
		every, ok := durationOf(sub["every"])
		if !ok || every < MinEvery {
			return errors.New(prefix + ".every: at least 100ms")
		}
		conn.Requests = append(conn.Requests, Request{Command: []byte(command), Every: every})
	}
	return nil
}

// CheckPoint validates a point of a serial input and returns its compiled
// address: the read mode (event only), the type, and an address that is a
// regular expression.
func CheckPoint(p config.Point) (*regexp.Regexp, error) {
	if p.Read.Mode != config.ReadEvent {
		return nil, errors.New("read: want event")
	}
	if p.Type == "" {
		return nil, errors.New("type: required")
	}
	if !slices.Contains(pointTypes, p.Type) {
		return nil, errors.New("type: want bool, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string")
	}
	if strings.TrimSpace(p.Address) == "" {
		return nil, errors.New("address: required")
	}
	re, err := regexp.Compile(p.Address)
	if err != nil {
		return nil, errors.New("address: invalid expression")
	}
	return re, nil
}

// listOf decodes the JSON array at key; absent is an empty list.
func listOf(fields map[string]json.RawMessage, key, name string) ([]json.RawMessage, error) {
	v, ok := fields[key]
	if !ok {
		return nil, nil
	}
	var list []json.RawMessage
	if isNull(v) || json.Unmarshal(v, &list) != nil {
		return nil, errors.New(name + ": must be a list")
	}
	return list, nil
}

// durationOf decodes a JSON string holding a Go duration.
func durationOf(raw json.RawMessage) (time.Duration, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil || isNull(raw) {
		return 0, false
	}
	d, err := time.ParseDuration(text)
	return d, err == nil
}

// intOf decodes a JSON integer literal; a string, a fraction or an exponent is
// refused.
func intOf(raw json.RawMessage) (int, bool) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 32)
	return int(n), err == nil
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
// name is the path the error names. A JSON null is not a string.
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
