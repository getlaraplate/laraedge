package mqtt

import (
	"errors"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/getlaraplate/laraedge/internal/model"
)

// KindMalformedPayload is a Sparkplug payload that is not valid protobuf
// (decision S6).
const KindMalformedPayload = "malformed_payload"

// Sparkplug B DataType numbers the driver reads.
const (
	spInt8     uint32 = 1
	spInt16    uint32 = 2
	spInt32    uint32 = 3
	spInt64    uint32 = 4
	spUInt8    uint32 = 5
	spUInt16   uint32 = 6
	spUInt32   uint32 = 7
	spUInt64   uint32 = 8
	spFloat    uint32 = 9
	spDouble   uint32 = 10
	spBoolean  uint32 = 11
	spString   uint32 = 12
	spDateTime uint32 = 13
	spText     uint32 = 14
	spUUID     uint32 = 15
)

// Metric value field numbers.
const (
	spFieldInt    protowire.Number = 10
	spFieldLong   protowire.Number = 11
	spFieldFloat  protowire.Number = 12
	spFieldDouble protowire.Number = 13
	spFieldBool   protowire.Number = 14
	spFieldString protowire.Number = 15
)

var errSparkplugWire = errors.New("sparkplug payload: malformed protobuf")

// spPayload is a decoded Sparkplug B payload (only the fields the driver uses).
type spPayload struct {
	Timestamp    uint64
	HasTimestamp bool
	Seq          uint64
	HasSeq       bool
	Metrics      []spMetric
}

// spMetric is a decoded Sparkplug B metric.
type spMetric struct {
	Name         string
	HasName      bool
	Alias        uint64
	HasAlias     bool
	Timestamp    uint64
	HasTimestamp bool
	// Datatype is 0 when absent.
	Datatype   uint32
	Historical bool
	Null       bool
	Quality    int64
	HasQuality bool
	Value      spValue
}

// spValue is the value oneof of a metric; the last one present wins.
type spValue struct {
	// Field is the field number of the value, 0 when there is none.
	Field   protowire.Number
	Uint    uint64
	Float64 float64
	Bool    bool
	Text    string
}

// decodePayload parses a Sparkplug B payload by hand. Unknown fields are
// skipped by wire type; a truncated field, a bad length, a group or a field
// of the wrong wire type is an error. It never panics.
func decodePayload(b []byte) (spPayload, error) {
	var p spPayload
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return spPayload{}, errSparkplugWire
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return spPayload{}, errSparkplugWire
			}
			p.Timestamp, p.HasTimestamp = v, true
			b = b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return spPayload{}, errSparkplugWire
			}
			p.Seq, p.HasSeq = v, true
			b = b[n:]
		case num == 2 && typ == protowire.BytesType:
			raw, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return spPayload{}, errSparkplugWire
			}
			m, err := decodeMetric(raw)
			if err != nil {
				return spPayload{}, err
			}
			p.Metrics = append(p.Metrics, m)
			b = b[n:]
		case num >= 1 && num <= 3:
			return spPayload{}, errSparkplugWire
		default:
			n, err := skipField(b, num, typ)
			if err != nil {
				return spPayload{}, err
			}
			b = b[n:]
		}
	}
	return p, nil
}

// skipField returns the length of an unknown field's value. Groups are
// refused.
func skipField(b []byte, num protowire.Number, typ protowire.Type) (int, error) {
	if typ == protowire.StartGroupType || typ == protowire.EndGroupType {
		return 0, errSparkplugWire
	}
	n := protowire.ConsumeFieldValue(num, typ, b)
	if n < 0 {
		return 0, errSparkplugWire
	}
	return n, nil
}

func decodeMetric(b []byte) (spMetric, error) {
	var m spMetric
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return spMetric{}, errSparkplugWire
		}
		b = b[n:]
		var (
			u    uint64
			raw  []byte
			f32  uint32
			f64  uint64
			used int
		)
		switch typ {
		case protowire.VarintType:
			u, used = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			raw, used = protowire.ConsumeBytes(b)
		case protowire.Fixed32Type:
			f32, used = protowire.ConsumeFixed32(b)
		case protowire.Fixed64Type:
			f64, used = protowire.ConsumeFixed64(b)
		default:
			return spMetric{}, errSparkplugWire
		}
		if used < 0 {
			return spMetric{}, errSparkplugWire
		}
		b = b[used:]

		switch {
		case num == 1 && typ == protowire.BytesType:
			m.Name, m.HasName = string(raw), true
		case num == 2 && typ == protowire.VarintType:
			m.Alias, m.HasAlias = u, true
		case num == 3 && typ == protowire.VarintType:
			m.Timestamp, m.HasTimestamp = u, true
		case num == 4 && typ == protowire.VarintType:
			m.Datatype = uint32(u)
		case num == 5 && typ == protowire.VarintType:
			m.Historical = u != 0
		case num == 7 && typ == protowire.VarintType:
			m.Null = u != 0
		case num == 9 && typ == protowire.BytesType:
			q, has, err := decodeQuality(raw)
			if err != nil {
				return spMetric{}, err
			}
			if has {
				m.Quality, m.HasQuality = q, true
			}
		case num == spFieldInt && typ == protowire.VarintType, num == spFieldLong && typ == protowire.VarintType:
			m.Value = spValue{Field: num, Uint: u}
		case num == spFieldFloat && typ == protowire.Fixed32Type:
			m.Value = spValue{Field: num, Float64: float64(math.Float32frombits(f32))}
		case num == spFieldDouble && typ == protowire.Fixed64Type:
			m.Value = spValue{Field: num, Float64: math.Float64frombits(f64)}
		case num == spFieldBool && typ == protowire.VarintType:
			m.Value = spValue{Field: num, Bool: u != 0}
		case num == spFieldString && typ == protowire.BytesType:
			m.Value = spValue{Field: num, Text: string(raw)}
		case num >= 16 && num <= 19:
			// bytes, dataset, template, extension: the value is present
			// but not one the driver reads.
			m.Value = spValue{Field: num}
		case (num >= 1 && num <= 5) || num == 7 || num == 9 || (num >= 10 && num <= 15):
			// A known field with the wrong wire type.
			return spMetric{}, errSparkplugWire
		}
	}
	return m, nil
}

// decodeQuality reads the Quality property of a PropertySet: keys and values
// are paired by position, and only an int_value or long_value counts.
func decodeQuality(b []byte) (int64, bool, error) {
	var (
		keys   []string
		values []int64
		kinds  []bool
	)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, false, errSparkplugWire
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			raw, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return 0, false, errSparkplugWire
			}
			keys = append(keys, string(raw))
			b = b[n:]
		case num == 2 && typ == protowire.BytesType:
			raw, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return 0, false, errSparkplugWire
			}
			v, isInt, err := decodePropertyInt(raw)
			if err != nil {
				return 0, false, err
			}
			values = append(values, v)
			kinds = append(kinds, isInt)
			b = b[n:]
		case num == 1 || num == 2:
			return 0, false, errSparkplugWire
		default:
			n, err := skipField(b, num, typ)
			if err != nil {
				return 0, false, err
			}
			b = b[n:]
		}
	}
	for i, k := range keys {
		if k == "Quality" && i < len(values) && kinds[i] {
			return values[i], true, nil
		}
	}
	return 0, false, nil
}

// decodePropertyInt returns the integer of a PropertyValue and whether it
// carries one (int_value 3 or long_value 4).
func decodePropertyInt(b []byte) (int64, bool, error) {
	var (
		v     int64
		isInt bool
	)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, false, errSparkplugWire
		}
		b = b[n:]
		if (num == 3 || num == 4) && typ == protowire.VarintType {
			u, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return 0, false, errSparkplugWire
			}
			v, isInt = int64(u), true
			b = b[n:]
			continue
		}
		if num == 3 || num == 4 {
			return 0, false, errSparkplugWire
		}
		n, err := skipField(b, num, typ)
		if err != nil {
			return 0, false, err
		}
		b = b[n:]
	}
	return v, isInt, nil
}

// metricValue converts a metric value to the point type with the lossless
// rules of the json driver (decision S7). datatype is the one learnt from a
// birth; the metric's own wins when present. The kind is "" on success.
func metricValue(m spMetric, datatype uint32, pointType string) (model.Value, string) {
	if m.Datatype != 0 {
		datatype = m.Datatype
	}
	text := ""
	switch datatype {
	case spInt8, spInt16, spInt32, spUInt8, spUInt16, spUInt32:
		if m.Value.Field != spFieldInt {
			return model.Value{}, KindConversion
		}
		u := m.Value.Uint
		switch datatype {
		case spInt8:
			text = strconv.FormatInt(int64(int8(u)), 10)
		case spInt16:
			text = strconv.FormatInt(int64(int16(u)), 10)
		case spInt32:
			text = strconv.FormatInt(int64(int32(u)), 10)
		case spUInt8:
			if u > math.MaxUint8 {
				return model.Value{}, KindConversion
			}
			text = strconv.FormatUint(u, 10)
		case spUInt16:
			if u > math.MaxUint16 {
				return model.Value{}, KindConversion
			}
			text = strconv.FormatUint(u, 10)
		default:
			if u > math.MaxUint32 {
				return model.Value{}, KindConversion
			}
			text = strconv.FormatUint(u, 10)
		}
	case spInt64:
		if m.Value.Field != spFieldLong {
			return model.Value{}, KindConversion
		}
		text = strconv.FormatInt(int64(m.Value.Uint), 10)
	case spUInt64, spDateTime:
		if m.Value.Field != spFieldLong {
			return model.Value{}, KindConversion
		}
		text = strconv.FormatUint(m.Value.Uint, 10)
	case spFloat, spDouble:
		want := spFieldFloat
		if datatype == spDouble {
			want = spFieldDouble
		}
		f := m.Value.Float64
		if m.Value.Field != want || math.IsNaN(f) || math.IsInf(f, 0) {
			return model.Value{}, KindConversion
		}
		bits := 64
		if datatype == spFloat {
			bits = 32
		}
		// Integer points need plain digits: 'g' switches to an exponent at
		// 1e6, which the integer parse refuses.
		format := byte('g')
		if pointType != "float32" && pointType != "float64" {
			format = 'f'
		}
		text = strconv.FormatFloat(f, format, -1, bits)
	case spBoolean:
		if m.Value.Field != spFieldBool || pointType != "bool" {
			return model.Value{}, KindConversion
		}
		return model.BoolValue(m.Value.Bool), ""
	case spString, spText, spUUID:
		if m.Value.Field != spFieldString || pointType != "string" || !utf8.ValidString(m.Value.Text) {
			return model.Value{}, KindConversion
		}
		return model.StringValue(m.Value.Text), ""
	default:
		return model.Value{}, KindConversion
	}
	v, ok := convertNumber(pointType, text)
	if !ok {
		return model.Value{}, KindConversion
	}
	return v, ""
}

// metricQuality maps the Quality property: 192 and above good, 64 to 191
// uncertain, below 64 bad; absent is good (decision S7).
func metricQuality(m spMetric) model.Quality {
	switch {
	case !m.HasQuality, m.Quality >= 192:
		return model.Good
	case m.Quality >= 64:
		return model.Uncertain
	default:
		return model.Bad
	}
}

// metricTime is the metric's timestamp, else the payload's, else reception
// (decision S8). ok is false for a timestamp outside 1970 to 9999; the time
// is then the zero time.
func metricTime(m spMetric, p spPayload, received time.Time) (time.Time, bool) {
	var ms uint64
	switch {
	case m.HasTimestamp:
		ms = m.Timestamp
	case p.HasTimestamp:
		ms = p.Timestamp
	default:
		t := received.UTC()
		return t, plausibleTimestamp(t)
	}
	if ms > math.MaxInt64 {
		return time.Time{}, false
	}
	t := time.UnixMilli(int64(ms)).UTC()
	if !plausibleTimestamp(t) {
		return time.Time{}, false
	}
	return t, true
}

// encodeRebirth builds the payload of a rebirth request: the payload
// timestamp and one Boolean metric Node Control/Rebirth = true.
func encodeRebirth(now time.Time) []byte {
	var metric []byte
	metric = protowire.AppendTag(metric, 1, protowire.BytesType)
	metric = protowire.AppendString(metric, metricRebirth)
	metric = protowire.AppendTag(metric, 4, protowire.VarintType)
	metric = protowire.AppendVarint(metric, uint64(spBoolean))
	metric = protowire.AppendTag(metric, spFieldBool, protowire.VarintType)
	metric = protowire.AppendVarint(metric, 1)

	var out []byte
	out = protowire.AppendTag(out, 1, protowire.VarintType)
	out = protowire.AppendVarint(out, uint64(now.UnixMilli()))
	out = protowire.AppendTag(out, 2, protowire.BytesType)
	out = protowire.AppendBytes(out, metric)
	return out
}

var spDatatypeNames = map[uint32]string{
	1: "Int8", 2: "Int16", 3: "Int32", 4: "Int64",
	5: "UInt8", 6: "UInt16", 7: "UInt32", 8: "UInt64",
	9: "Float", 10: "Double", 11: "Boolean", 12: "String", 13: "DateTime",
	14: "Text", 15: "UUID", 16: "DataSet", 17: "Bytes", 18: "File",
	19: "Template", 20: "PropertySet", 21: "PropertySetList",
	22: "Int8Array", 23: "Int16Array", 24: "Int32Array", 25: "Int64Array",
	26: "UInt8Array", 27: "UInt16Array", 28: "UInt32Array", 29: "UInt64Array",
	30: "FloatArray", 31: "DoubleArray", 32: "BooleanArray", 33: "StringArray",
	34: "DateTimeArray",
}

// DatatypeName is the Sparkplug name of a DataType number, Unknown otherwise.
func DatatypeName(dt uint32) string {
	if n, ok := spDatatypeNames[dt]; ok {
		return n
	}
	return "Unknown"
}

// SuggestedType is the point type browse suggests for a DataType (decision
// S14): unsupported:<name> when no point type fits.
func SuggestedType(dt uint32) string {
	switch dt {
	case spInt8, spInt16:
		return "int16"
	case spInt32:
		return "int32"
	case spInt64, spDateTime:
		return "int64"
	case spUInt8, spUInt16:
		return "uint16"
	case spUInt32:
		return "uint32"
	case spUInt64:
		return "uint64"
	case spFloat:
		return "float32"
	case spDouble:
		return "float64"
	case spBoolean:
		return "bool"
	case spString, spText, spUUID:
		return "string"
	}
	return "unsupported:" + DatatypeName(dt)
}
