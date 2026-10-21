package opcua

import (
	"errors"
	"math"
	"time"
	"unicode/utf8"

	"github.com/gopcua/opcua/ua"

	"github.com/getlaraplate/laraedge/internal/model"
)

// Conversion errors have fixed texts: they never carry the value, so they are
// safe in logs. Every error of Convert is of health kind decode.
var (
	errConvertPointType = errors.New("convert: unknown point type")
	errConvertMissing   = errors.New("convert: the data value carries no value")
	errConvertType      = errors.New("convert: the variant type does not convert to the point type")
	errConvertRange     = errors.New("convert: integer value is outside the range of the point type")
	errConvertPrecision = errors.New("convert: value is not exactly representable in the point type")
	errConvertUint64    = errors.New("convert: uint64 value above 9223372036854775807")
	errConvertNotFinite = errors.New("convert: float value is not finite")
	errConvertString    = errors.New("convert: string is not valid UTF-8")
)

const (
	maxExactFloat64 = int64(1) << 53 // largest magnitude below which every integer is a float64
	maxExactFloat32 = int64(1) << 24 // same for float32
)

// Quality maps an OPC UA status code to a sample quality by its severity class,
// the two top bits: 0 is good, 1 uncertain, and 2 (bad) or 3 (reserved) is bad.
func Quality(sc ua.StatusCode) model.Quality {
	switch uint32(sc) >> 30 {
	case 0:
		return model.Good
	case 1:
		return model.Uncertain
	default:
		return model.Bad
	}
}

// Timestamp returns the time of a data value in UTC: the source timestamp when
// the data value carries a non-zero one, else the server timestamp likewise,
// else now (the agent clock).
func Timestamp(dv *ua.DataValue, now time.Time) time.Time {
	if dv != nil {
		if dv.Has(ua.DataValueSourceTimestamp) && !dv.SourceTimestamp.IsZero() {
			return dv.SourceTimestamp.UTC()
		}
		if dv.Has(ua.DataValueServerTimestamp) && !dv.ServerTimestamp.IsZero() {
			return dv.ServerTimestamp.UTC()
		}
	}

	return now.UTC()
}

// Convert turns an OPC UA data value into the value and quality of a sample of
// the given point type (bool, int16, uint16, int32, uint32, int64, uint64,
// float32, float64 or string). A non-nil error is a decode failure: the value
// is then the zero of the point's kind and the quality is bad.
//
// The quality is Quality(dv.Status). A Bad status is the server's verdict, not
// a conversion failure: the result is quality bad, the zero value of the kind
// and no error, whatever the variant holds. A Good or Uncertain status needs a
// value that converts; a missing value (no value flag, nil variant or Null
// variant) or one that does not convert is quality bad with an error.
//
// Conversion is lossless or it fails:
//   - an integer variant (SByte to UInt64) converts to an integer type when it
//     is in range, and to a float type when it is exactly representable: at
//     most 2^53 in magnitude for float64, at most 2^24 for float32;
//   - a uint64 variant above 2^63-1 fails for every type, as model integers
//     are int64;
//   - a Float variant converts to float64; a Double variant converts to
//     float64, and to float32 only when float64(float32(v)) == v;
//   - a float variant never converts to an integer type, and a non-finite
//     float (NaN, infinity) always fails;
//   - a Boolean variant converts only to bool and a String variant (which
//     must be valid UTF-8) only to string;
//   - arrays, DateTime, ByteString and every other variant type fail.
//
// An unknown point type is an error even for a Bad status; the value is then
// the zero model.Value.
func Convert(dv *ua.DataValue, pointType string) (model.Value, model.Quality, error) {
	zero, ok := zeroValue(pointType)
	if !ok {
		return model.Value{}, model.Bad, errConvertPointType
	}
	if dv == nil {
		return zero, model.Bad, errConvertMissing
	}

	quality := Quality(dv.Status)
	if quality == model.Bad {
		return zero, model.Bad, nil
	}
	if !dv.Has(ua.DataValueValue) || dv.Value == nil || dv.Value.Type() == ua.TypeIDNull {
		return zero, model.Bad, errConvertMissing
	}

	v, err := convertVariant(dv.Value.Value(), pointType)
	if err != nil {
		return zero, model.Bad, err
	}

	return v, quality, nil
}

// zeroValue is the zero of the kind of a point type, the value of a bad sample.
func zeroValue(pointType string) (model.Value, bool) {
	switch pointType {
	case "bool":
		return model.BoolValue(false), true
	case "int16", "uint16", "int32", "uint32", "int64", "uint64":
		return model.IntValue(0), true
	case "float32", "float64":
		return model.Value{Kind: model.KindFloat}, true
	case "string":
		return model.StringValue(""), true
	default:
		return model.Value{}, false
	}
}

func convertVariant(raw interface{}, pointType string) (model.Value, error) {
	switch x := raw.(type) {
	case bool:
		if pointType != "bool" {
			return model.Value{}, errConvertType
		}

		return model.BoolValue(x), nil
	case string:
		if pointType != "string" {
			return model.Value{}, errConvertType
		}
		if !utf8.ValidString(x) {
			return model.Value{}, errConvertString
		}

		return model.StringValue(x), nil
	case float32:
		return convertFloat(float64(x), true, pointType)
	case float64:
		return convertFloat(x, false, pointType)
	}

	i, ok, err := integerOf(raw)
	if !ok {
		return model.Value{}, errConvertType
	}
	if err != nil {
		return model.Value{}, err
	}

	return convertInteger(i, pointType)
}

// integerOf extracts an integer variant as int64. ok is false for any other
// type; err is set for a uint64 that int64 cannot hold.
func integerOf(raw interface{}) (i int64, ok bool, err error) {
	switch x := raw.(type) {
	case int8:
		return int64(x), true, nil
	case uint8:
		return int64(x), true, nil
	case int16:
		return int64(x), true, nil
	case uint16:
		return int64(x), true, nil
	case int32:
		return int64(x), true, nil
	case uint32:
		return int64(x), true, nil
	case int64:
		return x, true, nil
	case uint64:
		if x > math.MaxInt64 {
			return 0, true, errConvertUint64
		}

		return int64(x), true, nil
	default:
		return 0, false, nil
	}
}

func convertInteger(i int64, pointType string) (model.Value, error) {
	var lo, hi int64

	switch pointType {
	case "int16":
		lo, hi = math.MinInt16, math.MaxInt16
	case "uint16":
		lo, hi = 0, math.MaxUint16
	case "int32":
		lo, hi = math.MinInt32, math.MaxInt32
	case "uint32":
		lo, hi = 0, math.MaxUint32
	case "int64":
		lo, hi = math.MinInt64, math.MaxInt64
	case "uint64":
		lo, hi = 0, math.MaxInt64
	case "float64":
		return exactFloat(i, maxExactFloat64)
	case "float32":
		return exactFloat(i, maxExactFloat32)
	default:
		return model.Value{}, errConvertType
	}
	if i < lo || i > hi {
		return model.Value{}, errConvertRange
	}

	return model.IntValue(i), nil
}

func exactFloat(i, limit int64) (model.Value, error) {
	if i < -limit || i > limit {
		return model.Value{}, errConvertPrecision
	}

	return model.FloatValue(float64(i))
}

// convertFloat converts a Float (isFloat32) or Double variant. A Float widens
// to float64 and fits float32 as it is.
func convertFloat(f float64, isFloat32 bool, pointType string) (model.Value, error) {
	if pointType != "float32" && pointType != "float64" {
		return model.Value{}, errConvertType
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return model.Value{}, errConvertNotFinite
	}
	if pointType == "float32" && !isFloat32 && float64(float32(f)) != f {
		return model.Value{}, errConvertPrecision
	}

	return model.FloatValue(f)
}
