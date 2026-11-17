package mqtt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Read error kinds the decoder reports (spec section 4). payload_too_large and
// unmapped are decided by the input before Decode is called.
const (
	// KindMalformedJSON is a payload that is not valid JSON where a path needs it.
	KindMalformedJSON = "malformed_json"
	// KindBadTimestamp is a payload timestamp that is missing, unreadable or
	// outside 1970-01-01T00:00:00Z to the end of year 9999.
	KindBadTimestamp = "bad_timestamp"
	// KindConversion is a value that does not convert losslessly to the type.
	KindConversion = "conversion"
)

// TopicPlan is everything the decoder needs for one subscribed topic.
type TopicPlan struct {
	// Topic is the concrete topic.
	Topic string
	// Timestamp is the effective timestamp spec of the topic (the per-topic
	// override over the input's); nil: reception time.
	Timestamp *TimestampSpec
	// Points are the points on the topic, in configuration order.
	Points []PlannedPoint
}

// PlannedPoint is one point of a topic with its parsed address.
type PlannedPoint struct {
	Device string
	Signal string
	Point  config.Point
	// Path is the parsed address path; nil: the whole payload is the value.
	Path []string
}

// Plan groups the points of all devices of an input by topic. The error is a
// fixed text naming the device and point positions, never an address.
func Plan(cfg config.Input, conn Connection) (map[string]*TopicPlan, error) {
	plans := make(map[string]*TopicPlan)
	for di, dev := range cfg.Devices {
		for pi, p := range dev.Points {
			addr, err := ParseAddress(p.Address)
			if err != nil {
				return nil, errors.New("devices[" + strconv.Itoa(di) + "].points[" + strconv.Itoa(pi) + "]." + err.Error())
			}
			plan := plans[addr.Topic]
			if plan == nil {
				plan = &TopicPlan{Topic: addr.Topic, Timestamp: conn.Timestamp}
				if o, ok := conn.Topics[addr.Topic]; ok && o.Timestamp != nil {
					plan.Timestamp = o.Timestamp
				}
				plans[addr.Topic] = plan
			}
			plan.Points = append(plan.Points, PlannedPoint{Device: dev.Device, Signal: p.Signal, Point: p, Path: addr.Path})
		}
	}
	return plans, nil
}

// Decode turns one message payload into samples. received is the sample time
// when the topic has no timestamp spec. errs holds one read error kind per
// failure: one malformed_json and one bad_timestamp per message, one conversion
// per point. A point whose path is missing from the message gives no sample.
func Decode(plan *TopicPlan, payload []byte, received time.Time) ([]model.Sample, []string) {
	var errs []string

	needsJSON := plan.Timestamp != nil
	for _, pp := range plan.Points {
		if pp.Path != nil {
			needsJSON = true
		}
	}
	var doc any
	validJSON := false
	if needsJSON {
		doc, validJSON = parseJSON(payload)
	}

	ts := received.UTC()
	if plan.Timestamp != nil {
		t, ok := payloadTime(plan.Timestamp, doc, validJSON)
		if !ok {
			// The time is unreadable: every point of the topic is bad.
			if !validJSON && hasPath(plan) {
				errs = append(errs, KindMalformedJSON)
			}
			errs = append(errs, KindBadTimestamp)
			samples := make([]model.Sample, 0, len(plan.Points))
			for _, pp := range plan.Points {
				samples = append(samples, badSample(pp, ts))
			}
			return samples, errs
		}
		ts = t.UTC()
	}

	var samples []model.Sample
	malformedReported := false
	for _, pp := range plan.Points {
		var v model.Value
		var ok bool
		switch {
		case pp.Path == nil:
			v, ok = convertText(pp.Point.Type, string(payload))
		case !validJSON:
			if !malformedReported {
				errs = append(errs, KindMalformedJSON)
				malformedReported = true
			}
			samples = append(samples, badSample(pp, ts))
			continue
		default:
			raw, found := walk(doc, pp.Path)
			if !found {
				continue
			}
			v, ok = convertJSON(pp.Point.Type, raw)
		}
		if !ok {
			errs = append(errs, KindConversion)
			samples = append(samples, badSample(pp, ts))
			continue
		}
		samples = append(samples, model.Sample{Device: pp.Device, Signal: pp.Signal, TS: ts, Value: v, Quality: model.Good, Priority: pp.Point.Priority})
	}
	return samples, errs
}

func payloadTime(spec *TimestampSpec, doc any, validJSON bool) (time.Time, bool) {
	if !validJSON {
		return time.Time{}, false
	}
	raw, found := walk(doc, strings.Split(spec.Path, "."))
	if !found {
		return time.Time{}, false
	}
	return parseTimestampValue(raw, spec.Format)
}

func hasPath(plan *TopicPlan) bool {
	for _, pp := range plan.Points {
		if pp.Path != nil {
			return true
		}
	}
	return false
}

func badSample(pp PlannedPoint, ts time.Time) model.Sample {
	return model.Sample{Device: pp.Device, Signal: pp.Signal, TS: ts, Value: zeroValue(pp.Point.Type), Quality: model.Bad, Priority: pp.Point.Priority}
}

// zeroValue is the zero of the point type's kind, as the other drivers do.
func zeroValue(typ string) model.Value {
	switch typ {
	case "bool":
		return model.BoolValue(false)
	case "int16", "uint16", "int32", "uint32", "int64", "uint64":
		return model.IntValue(0)
	case "float32", "float64":
		return model.Value{Kind: model.KindFloat}
	default:
		return model.StringValue("")
	}
}

// parseJSON decodes exactly one JSON value, numbers kept as json.Number.
func parseJSON(payload []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// walk follows path through objects and lists; a numeric segment indexes a
// list. A missing step, or a step into a scalar, is not found.
func walk(v any, path []string) (any, bool) {
	for _, seg := range path {
		switch node := v.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, ok := listIndex(seg)
			if !ok || i >= len(node) {
				return nil, false
			}
			v = node[i]
		default:
			return nil, false
		}
	}
	return v, true
}

func listIndex(seg string) (int, bool) {
	for i := 0; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(seg)
	return n, err == nil
}

// convertJSON converts a value found at a path (decision Q4).
func convertJSON(typ string, v any) (model.Value, bool) {
	switch x := v.(type) {
	case json.Number:
		return convertNumber(typ, x.String())
	case bool:
		if typ == "bool" {
			return model.BoolValue(x), true
		}
	case string:
		if typ == "string" {
			return convertText(typ, x)
		}
	}
	return model.Value{}, false
}

// convertText converts plain text. Numbers and bools are trimmed of
// surrounding whitespace; a string is taken as is. A number must follow the
// JSON number grammar, with one leading + allowed: strconv also reads hex
// floats, inf, nan and underscores, which a JSON payload cannot carry either.
func convertText(typ, text string) (model.Value, bool) {
	switch typ {
	case "string":
		if !utf8.ValidString(text) {
			return model.Value{}, false
		}
		return model.StringValue(text), true
	case "bool":
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "1", "on":
			return model.BoolValue(true), true
		case "false", "0", "off":
			return model.BoolValue(false), true
		}
		return model.Value{}, false
	default:
		number := strings.TrimSpace(text)
		if !isPlainNumber(number) {
			return model.Value{}, false
		}
		return convertNumber(typ, number)
	}
}

// isPlainNumber reports whether s is a JSON number, optionally preceded by
// one +: -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?.
func isPlainNumber(s string) bool {
	if rest, ok := strings.CutPrefix(s, "+"); ok {
		s = rest
	} else {
		s = strings.TrimPrefix(s, "-")
	}

	i := 0
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		i = skipDigits(s, i)
	default:
		return false
	}

	if i < len(s) && s[i] == '.' {
		j := skipDigits(s, i+1)
		if j == i+1 {
			return false
		}
		i = j
	}

	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		j := skipDigits(s, i)
		if j == i {
			return false
		}
		i = j
	}

	return i == len(s)
}

// skipDigits returns the index of the first byte of s at or after i that is
// not an ASCII digit.
func skipDigits(s string, i int) int {
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

// convertNumber converts a numeric text to an integer or float type. An
// integer type needs the text to be an integer in the type's range: 3.0 and
// 1e3 are not integers.
func convertNumber(typ, s string) (model.Value, bool) {
	switch typ {
	case "int16":
		return intInRange(s, math.MinInt16, math.MaxInt16)
	case "int32":
		return intInRange(s, math.MinInt32, math.MaxInt32)
	case "int64":
		return intInRange(s, math.MinInt64, math.MaxInt64)
	case "uint16":
		return intInRange(s, 0, math.MaxUint16)
	case "uint32":
		return intInRange(s, 0, math.MaxUint32)
	case "uint64":
		// Model integers are int64: above 2^63-1 is bad.
		return intInRange(s, 0, math.MaxInt64)
	case "float32", "float64":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || (typ == "float32" && !fitsFloat32(f)) {
			return model.Value{}, false
		}
		val, err := model.FloatValue(f)
		return val, err == nil
	}
	return model.Value{}, false
}

// fitsFloat32 reports whether f is a float32 up to rounding: not above the
// float32 range, and not a non-zero value that float32 turns into zero.
func fitsFloat32(f float64) bool {
	return math.Abs(f) <= math.MaxFloat32 && (f == 0 || float32(f) != 0)
}

func intInRange(s string, lo, hi int64) (model.Value, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < lo || n > hi {
		return model.Value{}, false
	}
	return model.IntValue(n), true
}

// Largest timestamps accepted: the end of year 9999.
const (
	maxUnixSeconds = 253402300799
	maxUnixMillis  = maxUnixSeconds * 1000
)

// timestampCeiling is the first instant after the latest plausible payload
// timestamp: the envelope writes a four-digit year (date-time), so year
// 10000 would make the backend reject the whole batch.
var timestampCeiling = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)

// plausibleTimestamp is the rule of the opcua driver (non-zero, not before
// the Unix epoch) with the envelope's year 9999 as the upper bound: a
// timestamp outside it is bad_timestamp, never a sample the backend refuses.
func plausibleTimestamp(t time.Time) bool {
	return !t.IsZero() && t.Unix() >= 0 && t.Before(timestampCeiling)
}

// parseTimestampValue reads a payload timestamp in the given format and
// returns it in UTC; an implausible one is not ok.
func parseTimestampValue(v any, format string) (time.Time, bool) {
	var (
		t  time.Time
		ok bool
	)
	switch format {
	case TimestampRFC3339:
		s, isString := v.(string)
		if !isString {
			return time.Time{}, false
		}
		parsed, err := time.Parse(time.RFC3339Nano, s)
		t, ok = parsed.UTC(), err == nil
	case TimestampUnixSeconds, TimestampUnixMillis:
		num, isNumber := v.(json.Number)
		if !isNumber {
			return time.Time{}, false
		}
		t, ok = parseUnix(num.String(), format == TimestampUnixMillis)
	}
	if !ok || !plausibleTimestamp(t) {
		return time.Time{}, false
	}

	return t, true
}

func parseUnix(s string, millis bool) (time.Time, bool) {
	limit := int64(maxUnixSeconds)
	perSecond := int64(1)
	if millis {
		limit, perSecond = maxUnixMillis, 1000
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < -limit || n > limit {
			return time.Time{}, false
		}
		sec, rem := n/perSecond, n%perSecond
		return time.Unix(sec, rem*(int64(time.Second)/perSecond)).UTC(), true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.Abs(f) > float64(limit) {
		return time.Time{}, false
	}
	whole, frac := math.Modf(f)
	n := int64(whole)
	nsPerUnit := int64(time.Second) / perSecond
	return time.Unix(n/perSecond, (n%perSecond)*nsPerUnit+int64(math.Round(frac*float64(nsPerUnit)))).UTC(), true
}
