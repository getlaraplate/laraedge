package driverkit

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getlaraplate/laraedge/internal/model"
)

// ZeroValue is the zero of the point type's kind, as the other drivers do.
func ZeroValue(typ string) model.Value {
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

// ConvertText converts plain text. Numbers and bools are trimmed of
// surrounding whitespace; a string is taken as is. A number must follow the
// JSON number grammar, with one leading + allowed: strconv also reads hex
// floats, inf, nan and underscores, which a JSON payload cannot carry either.
func ConvertText(typ, text string) (model.Value, bool) {
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
		return ConvertNumber(typ, number)
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

// ConvertNumber converts a numeric text to an integer or float type. An
// integer type needs the text to be an integer in the type's range: 3.0 and
// 1e3 are not integers.
func ConvertNumber(typ, s string) (model.Value, bool) {
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

// ParseUnix reads s as Unix seconds, or milliseconds when millis, as an
// integer or a decimal, in UTC. A value beyond the end of year 9999 (either
// sign) or not a number is not ok.
func ParseUnix(s string, millis bool) (time.Time, bool) {
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
