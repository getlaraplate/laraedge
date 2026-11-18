package mqtt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
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

// NoValueAtPath is what probe says of a point whose path is missing from the
// message of its topic. It is not a read error kind: the running input
// counts nothing for it.
const NoValueAtPath = "no value at path"

// Decode turns one message payload into samples. received is the sample time
// when the topic has no timestamp spec. errs holds one read error kind per
// failure: one malformed_json and one bad_timestamp per message, one conversion
// per point. A point whose path is missing from the message gives no sample.
func Decode(plan *TopicPlan, payload []byte, received time.Time) ([]model.Sample, []string) {
	samples, errs, _ := decode(plan, payload, received)

	return samples, errs
}

// decode is Decode that also returns, for every point of the plan in order,
// why it did not decode: the read error kind of its bad sample, NoValueAtPath
// when it has no sample, "" for a good sample.
func decode(plan *TopicPlan, payload []byte, received time.Time) ([]model.Sample, []string, []string) {
	var errs []string
	pointKinds := make([]string, len(plan.Points))

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
			for i, pp := range plan.Points {
				samples = append(samples, badSample(pp, ts))
				pointKinds[i] = KindBadTimestamp
				if pp.Path != nil && !validJSON {
					pointKinds[i] = KindMalformedJSON
				}
			}
			return samples, errs, pointKinds
		}
		ts = t.UTC()
	}

	var samples []model.Sample
	malformedReported := false
	for i, pp := range plan.Points {
		var v model.Value
		var ok bool
		switch {
		case pp.Path == nil:
			v, ok = driverkit.ConvertText(pp.Point.Type, string(payload))
		case !validJSON:
			if !malformedReported {
				errs = append(errs, KindMalformedJSON)
				malformedReported = true
			}
			samples = append(samples, badSample(pp, ts))
			pointKinds[i] = KindMalformedJSON
			continue
		default:
			raw, found := walk(doc, pp.Path)
			if !found {
				pointKinds[i] = NoValueAtPath
				continue
			}
			v, ok = convertJSON(pp.Point.Type, raw)
		}
		if !ok {
			errs = append(errs, KindConversion)
			samples = append(samples, badSample(pp, ts))
			pointKinds[i] = KindConversion
			continue
		}
		samples = append(samples, model.Sample{Device: pp.Device, Signal: pp.Signal, TS: ts, Value: v, Quality: model.Good, Priority: pp.Point.Priority})
	}
	return samples, errs, pointKinds
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
	return model.Sample{Device: pp.Device, Signal: pp.Signal, TS: ts, Value: driverkit.ZeroValue(pp.Point.Type), Quality: model.Bad, Priority: pp.Point.Priority}
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
		return driverkit.ConvertNumber(typ, x.String())
	case bool:
		if typ == "bool" {
			return model.BoolValue(x), true
		}
	case string:
		if typ == "string" {
			return driverkit.ConvertText(typ, x)
		}
	}
	return model.Value{}, false
}

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
		t, ok = driverkit.ParseUnix(num.String(), format == TimestampUnixMillis)
	}
	if !ok || !plausibleTimestamp(t) {
		return time.Time{}, false
	}

	return t, true
}
