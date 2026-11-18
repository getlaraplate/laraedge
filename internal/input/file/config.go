package file

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/docrules"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
)

// Input formats (spec section 4).
const (
	// FormatCSV is delimited text.
	FormatCSV = "csv"
	// FormatQDAS is Q-DAS, not in this build.
	FormatQDAS = "qdas"
)

// What happens to a file once it is read (spec section 5).
const (
	// AfterReadMove moves the file to done/ or error/; the default.
	AfterReadMove = "move"
	// AfterReadKeep leaves the file where it is.
	AfterReadKeep = "keep"
)

// CSV layouts (spec section 6).
const (
	// LayoutWide is one row per part, one column per characteristic; the default.
	LayoutWide = "wide"
	// LayoutLong is one row per characteristic, the file being one part.
	LayoutLong = "long"
)

// Timestamp formats of a file timestamp (spec section 6); any other format is
// a token layout.
const (
	// TimestampRFC3339 is a text timestamp in RFC 3339; without an offset it
	// is read in the connection's time zone.
	TimestampRFC3339 = "rfc3339"
	// TimestampUnixSeconds is a number of seconds since the epoch.
	TimestampUnixSeconds = "unix_s"
	// TimestampUnixMillis is a number of milliseconds since the epoch.
	TimestampUnixMillis = "unix_ms"
)

// Defaults and limits of the connection (spec section 4).
const (
	// DefaultPattern matches every file name.
	DefaultPattern = "*"
	// DefaultInterval is the scan period.
	DefaultInterval = 2 * time.Second
	// MinInterval is the shortest scan period.
	MinInterval = 500 * time.Millisecond
	// DefaultStableFor is how long a file must stay unchanged to be read.
	DefaultStableFor = 5 * time.Second
	// DefaultMaxFileSize is the largest file read, 16 MiB.
	DefaultMaxFileSize = 16 << 20
	// MaxFileSizeLimit is the largest max_file_size the document may set.
	MaxFileSizeLimit = 16 << 20
	// DefaultMaxFilesPerScan is the number of files read in one scan.
	DefaultMaxFilesPerScan = 100
	// MaxFilesPerScanLimit is the largest max_files_per_scan.
	MaxFilesPerScanLimit = 10000
	// DefaultSeenRetention is how long the ledger keeps a hash, 30 days.
	DefaultSeenRetention = 30 * 24 * time.Hour
	// MinSeenRetention is the shortest seen_retention.
	MinSeenRetention = time.Hour
	// DefaultEncoding is the text encoding of the files.
	DefaultEncoding = "utf-8"
)

// Connection is the parsed `connection` of a file input.
type Connection struct {
	// Directory is the absolute directory watched, at its top level only.
	Directory string
	// Pattern is a path.Match glob on the file name, matched case-insensitively.
	Pattern string
	// Format is FormatCSV.
	Format string
	// AfterRead is AfterReadMove or AfterReadKeep.
	AfterRead string
	// Encoding is utf-8, windows-1252 or iso-8859-1.
	Encoding string
	// Interval is the scan period.
	Interval time.Duration
	// StableFor is how long size and modification time must stay unchanged.
	StableFor time.Duration
	// SeenRetention is how long the ledger keeps the hash of a read file.
	SeenRetention time.Duration
	// MaxFileSize is the largest file read, in bytes.
	MaxFileSize int
	// MaxFilesPerScan is the number of files read in one scan.
	MaxFilesPerScan int
	// Location reads the timestamps written without an offset.
	Location *time.Location
	// Timestamp says where the sample time is; nil: the modification time.
	Timestamp *TimestampSpec
	// Context maps the context keys to columns.
	Context ContextMap
	// CSV holds the CSV settings.
	CSV CSVSettings
}

// TimestampSpec says where the sample time is in a file.
type TimestampSpec struct {
	// Column is the column holding the timestamp.
	Column string
	// Format is TimestampRFC3339, TimestampUnixSeconds, TimestampUnixMillis
	// or a token layout.
	Format string
	// Layout is the parsed layout, set when Format is a token layout.
	Layout TimeLayout
}

// ContextMap names the column of each context key; empty: not mapped.
type ContextMap struct {
	Serial       string
	OrderRef     string
	OperationRef string
	Lot          string
}

// CSVSettings are the settings of `connection.csv`.
type CSVSettings struct {
	// Delimiter separates the fields of a row.
	Delimiter rune
	// Decimal is the decimal separator of numbers, '.' or ','.
	Decimal byte
	// HeaderRow is the 1-based row holding the column names.
	HeaderRow int
	// Layout is LayoutWide or LayoutLong.
	Layout string
	// KeyColumn and ValueColumn name the columns of LayoutLong.
	KeyColumn   string
	ValueColumn string
}

var (
	connectionFields = []string{"directory", "pattern", "format", "interval", "stable_for", "max_file_size", "max_files_per_scan", "after_read", "seen_retention", "encoding", "timezone", "timestamp", "context", "csv"}
	timestampFields  = []string{"column", "format"}
	contextKeys      = []string{"serial", "order_ref", "operation_ref", "lot"}
	csvFields        = []string{"delimiter", "decimal", "header_row", "layout", "key_column", "value_column"}
	encodings        = []string{DefaultEncoding, "windows-1252", "iso-8859-1"}
)

const (
	intervalShape      = "connection.interval: want a duration of at least 500ms, such as 2s"
	stableShape        = "connection.stable_for: want a duration of at least interval, such as 5s"
	maxFileSizeShape   = "connection.max_file_size: want 1 byte to 16MiB, as bytes or a size such as 1MiB"
	maxFilesShape      = "connection.max_files_per_scan: want a whole number from 1 to 10000"
	retentionShape     = "connection.seen_retention: want a duration of at least 1h, or days such as 30d"
	timestampFmtShape  = "connection.timestamp.format: want rfc3339, unix_s, unix_ms or a layout of YYYY, MM, DD, hh, mm, ss"
	delimiterShape     = "connection.csv.delimiter: want one character that is not a quote or a line break"
	headerRowShape     = "connection.csv.header_row: want a whole number from 1"
	absoluteDirMessage = "connection.directory: must be an absolute path"
)

// ParseConnection validates the `connection` object of a file input and applies
// the defaults of spec section 4. Every error is a fixed message naming the
// field, never the value, so a path or a column name cannot reach a log.
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
		Pattern: DefaultPattern, AfterRead: AfterReadMove, Encoding: DefaultEncoding,
		Interval: DefaultInterval, SeenRetention: DefaultSeenRetention,
		MaxFileSize: DefaultMaxFileSize, MaxFilesPerScan: DefaultMaxFilesPerScan,
		Location: time.Local,
		CSV:      CSVSettings{Delimiter: ',', Decimal: '.', HeaderRow: 1, Layout: LayoutWide},
	}
	steps := []func(map[string]json.RawMessage, *Connection) error{
		parseDirectory, parseFormat, parsePattern, parseTiming, parseLimits,
		parseAfterRead, parseEncoding, parseTimezone, parseTimestampField, parseContext, parseCSV,
	}
	for _, step := range steps {
		if err := step(fields, &conn); err != nil {
			return Connection{}, err
		}
	}
	return conn, nil
}

func parseDirectory(fields map[string]json.RawMessage, conn *Connection) error {
	if err := stringField(fields, "connection.directory", "directory", &conn.Directory); err != nil {
		return err
	}
	if conn.Directory == "" {
		return errors.New("connection.directory: required")
	}
	if !filepath.IsAbs(conn.Directory) {
		return errors.New(absoluteDirMessage)
	}
	return nil
}

func parseFormat(fields map[string]json.RawMessage, conn *Connection) error {
	var format string
	if err := stringField(fields, "connection.format", "format", &format); err != nil {
		return err
	}
	switch format {
	case "":
		return errors.New("connection.format: required")
	case FormatCSV:
		conn.Format = FormatCSV
		return nil
	case FormatQDAS:
		return errors.New("connection.format: not available in this build")
	default:
		return errors.New("connection.format: want csv")
	}
}

func parsePattern(fields map[string]json.RawMessage, conn *Connection) error {
	if _, ok := fields["pattern"]; !ok {
		return nil
	}
	if err := stringField(fields, "connection.pattern", "pattern", &conn.Pattern); err != nil {
		return err
	}
	if _, err := path.Match(conn.Pattern, ""); conn.Pattern == "" || err != nil {
		return errors.New("connection.pattern: not a valid glob")
	}
	return nil
}

func parseTiming(fields map[string]json.RawMessage, conn *Connection) error {
	if v, ok := fields["interval"]; ok {
		d, ok := durationOf(v)
		if !ok || d < MinInterval {
			return errors.New(intervalShape)
		}
		conn.Interval = d
	}
	conn.StableFor = max(DefaultStableFor, conn.Interval)
	if v, ok := fields["stable_for"]; ok {
		d, ok := durationOf(v)
		if !ok || d < conn.Interval {
			return errors.New(stableShape)
		}
		conn.StableFor = d
	}
	if v, ok := fields["seen_retention"]; ok {
		d, ok := retentionOf(v)
		if !ok || d < MinSeenRetention {
			return errors.New(retentionShape)
		}
		conn.SeenRetention = d
	}
	return nil
}

func parseLimits(fields map[string]json.RawMessage, conn *Connection) error {
	if v, ok := fields["max_file_size"]; ok {
		n, err := driverkit.ParseSize(v, MaxFileSizeLimit)
		if err != nil {
			return errors.New(maxFileSizeShape)
		}
		conn.MaxFileSize = n
	}
	if v, ok := fields["max_files_per_scan"]; ok {
		n, ok := intOf(v)
		if !ok || n < 1 || n > MaxFilesPerScanLimit {
			return errors.New(maxFilesShape)
		}
		conn.MaxFilesPerScan = n
	}
	return nil
}

func parseAfterRead(fields map[string]json.RawMessage, conn *Connection) error {
	if _, ok := fields["after_read"]; !ok {
		return nil
	}
	if err := stringField(fields, "connection.after_read", "after_read", &conn.AfterRead); err != nil {
		return err
	}
	if conn.AfterRead != AfterReadMove && conn.AfterRead != AfterReadKeep {
		return errors.New("connection.after_read: want move or keep")
	}
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

func parseTimezone(fields map[string]json.RawMessage, conn *Connection) error {
	if _, ok := fields["timezone"]; !ok {
		return nil
	}
	var name string
	if err := stringField(fields, "connection.timezone", "timezone", &name); err != nil {
		return err
	}
	loc, err := time.LoadLocation(name)
	if name == "" || err != nil {
		return errors.New("connection.timezone: not an IANA time zone name")
	}
	conn.Location = loc
	return nil
}

func parseTimestampField(fields map[string]json.RawMessage, conn *Connection) error {
	raw, ok := fields["timestamp"]
	if !ok {
		return nil
	}
	sub, err := objectFields(raw, "connection.timestamp")
	if err != nil {
		return err
	}
	for _, key := range sortedKeys(sub) {
		if !slices.Contains(timestampFields, key) {
			return docrules.UnknownKey("connection.timestamp.", key)
		}
	}
	var spec TimestampSpec
	if err := stringField(sub, "connection.timestamp.column", "column", &spec.Column); err != nil {
		return err
	}
	spec.Column = strings.TrimSpace(spec.Column)
	if spec.Column == "" {
		return errors.New("connection.timestamp.column: required")
	}
	if err := stringField(sub, "connection.timestamp.format", "format", &spec.Format); err != nil {
		return err
	}
	switch spec.Format {
	case "":
		return errors.New("connection.timestamp.format: required")
	case TimestampRFC3339, TimestampUnixSeconds, TimestampUnixMillis:
	default:
		layout, err := ParseTimeLayout(spec.Format)
		if err != nil {
			return errors.New(timestampFmtShape)
		}
		spec.Layout = layout
	}
	conn.Timestamp = &spec
	return nil
}

func parseContext(fields map[string]json.RawMessage, conn *Connection) error {
	raw, ok := fields["context"]
	if !ok {
		return nil
	}
	sub, err := objectFields(raw, "connection.context")
	if err != nil {
		return err
	}
	for _, key := range sortedKeys(sub) {
		if !slices.Contains(contextKeys, key) {
			return docrules.UnknownKey("connection.context.", key)
		}
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"serial", &conn.Context.Serial}, {"order_ref", &conn.Context.OrderRef}, {"operation_ref", &conn.Context.OperationRef}, {"lot", &conn.Context.Lot}} {
		if _, ok := sub[f.key]; !ok {
			continue
		}
		name := "connection.context." + f.key
		if err := stringField(sub, name, f.key, f.dst); err != nil {
			return err
		}
		*f.dst = strings.TrimSpace(*f.dst)
		if *f.dst == "" {
			return errors.New(name + ": required")
		}
	}
	return nil
}

func parseCSV(fields map[string]json.RawMessage, conn *Connection) error {
	raw, ok := fields["csv"]
	if !ok {
		return nil
	}
	sub, err := objectFields(raw, "connection.csv")
	if err != nil {
		return err
	}
	for _, key := range sortedKeys(sub) {
		if !slices.Contains(csvFields, key) {
			return docrules.UnknownKey("connection.csv.", key)
		}
	}
	c := &conn.CSV
	if _, ok := sub["delimiter"]; ok {
		var text string
		if err := stringField(sub, "connection.csv.delimiter", "delimiter", &text); err != nil {
			return err
		}
		r, size := utf8.DecodeRuneInString(text)
		if text == "" || size != len(text) || r == utf8.RuneError || r == '"' || r == '\r' || r == '\n' || r == 0 {
			return errors.New(delimiterShape)
		}
		c.Delimiter = r
	}
	if _, ok := sub["decimal"]; ok {
		var text string
		if err := stringField(sub, "connection.csv.decimal", "decimal", &text); err != nil {
			return err
		}
		if text != "." && text != "," {
			return errors.New("connection.csv.decimal: want . or ,")
		}
		c.Decimal = text[0]
	}
	if c.Decimal == ',' && c.Delimiter == ',' {
		return errors.New("connection.csv.decimal: must not be , when the delimiter is ,")
	}
	if v, ok := sub["header_row"]; ok {
		n, ok := intOf(v)
		if !ok || n < 1 {
			return errors.New(headerRowShape)
		}
		c.HeaderRow = n
	}
	if _, ok := sub["layout"]; ok {
		if err := stringField(sub, "connection.csv.layout", "layout", &c.Layout); err != nil {
			return err
		}
		if c.Layout != LayoutWide && c.Layout != LayoutLong {
			return errors.New("connection.csv.layout: want wide or long")
		}
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"key_column", &c.KeyColumn}, {"value_column", &c.ValueColumn}} {
		name := "connection.csv." + f.key
		if err := stringField(sub, name, f.key, f.dst); err != nil {
			return err
		}
		*f.dst = strings.TrimSpace(*f.dst)
		_, set := sub[f.key]
		switch {
		case c.Layout == LayoutLong && *f.dst == "":
			return errors.New(name + ": required with layout long")
		case c.Layout != LayoutLong && set:
			return errors.New(name + ": only with layout long")
		}
	}
	return nil
}

// ParseTimestamp reads text as the timestamp spec describes, in UTC. A value
// without an offset is read in loc; an rfc3339 value with one ignores loc.
// Not ok when spec is nil or the text does not read.
func ParseTimestamp(spec *TimestampSpec, text string, loc *time.Location) (time.Time, bool) {
	if spec == nil {
		return time.Time{}, false
	}
	text = strings.TrimSpace(text)
	switch spec.Format {
	case TimestampRFC3339:
		if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return t.UTC(), true
		}
		if t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", text, loc); err == nil {
			return t.UTC(), true
		}
		return time.Time{}, false
	case TimestampUnixSeconds:
		return driverkit.ParseUnix(text, false)
	case TimestampUnixMillis:
		return driverkit.ParseUnix(text, true)
	default:
		return spec.Layout.Parse(text, loc)
	}
}

// pointTypes are the decoding types of a point.
var pointTypes = []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64", "float32", "float64", "string"}

// CheckPoint validates a point: the read mode (event only), the required type,
// the absence of a byte order and a non-blank address (a column name or a key).
func CheckPoint(p config.Point) error {
	if p.Read.Mode != config.ReadEvent {
		return errors.New("read: want event")
	}
	if p.Type == "" {
		return errors.New("type: required")
	}
	if !slices.Contains(pointTypes, p.Type) {
		return errors.New("type: want bool, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string")
	}
	if p.ByteOrder != "" {
		return errors.New("byte_order: not for file")
	}
	if strings.TrimSpace(p.Address) == "" {
		return errors.New("address: required")
	}
	return nil
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

// retentionOf decodes seen_retention: a Go duration or a number of days, <n>d.
func retentionOf(raw json.RawMessage) (time.Duration, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil || isNull(raw) {
		return 0, false
	}
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.ParseUint(days, 10, 32)
		if err != nil || days == "" || days[0] == '+' {
			return 0, false
		}
		return time.Duration(n) * 24 * time.Hour, true
	}
	d, err := time.ParseDuration(text)
	return d, err == nil
}

// intOf decodes a JSON number that is a whole number.
func intOf(raw json.RawMessage) (int, bool) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || n > math.MaxInt32 || n < math.MinInt32 {
		return 0, false
	}
	return int(n), true
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
