package transform

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

// MaxSampleLine is the longest line of a sample file ReadSamples accepts, in
// bytes. A longer line is an error naming its number.
const MaxSampleLine = 1 << 20

// sampleTimeLayout is the timestamp of a sample file: RFC 3339, UTC, with
// milliseconds.
const sampleTimeLayout = "2006-01-02T15:04:05.000Z"

// Fixed read errors. None of them carries a value of the file.
var (
	errSampleSyntax  = errors.New("not a sample object with device, signal, ts, value and quality")
	errSampleUnknown = errors.New("unknown device or signal")
	errSampleTime    = errors.New("ts is not an RFC 3339 timestamp")
	errSampleQuality = errors.New("quality must be good, uncertain or bad")
	errSampleTooLong = errors.New("line is too long")
)

// sampleLine is one line of a sample file (decision P7).
type sampleLine struct {
	Device  *string         `json:"device"`
	Signal  *string         `json:"signal"`
	TS      *string         `json:"ts"`
	Value   json.RawMessage `json:"value"`
	Quality *string         `json:"quality"`
}

// WriteSamples writes s as JSON Lines, one object per sample:
//
//	{"device":"press-07","signal":"force","ts":"2026-10-19T08:00:01.250Z","value":182.5,"quality":"good"}
//
// ts is UTC with milliseconds. Nothing is written when a sample cannot be: a
// sample without device, signal, timestamp or quality, or with an invalid or
// non-finite value.
func WriteSamples(w io.Writer, s []model.Sample) error {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	for i, sm := range s {
		line, err := encodeSample(sm)
		if err != nil {
			return fmt.Errorf("sample %d: %w", i+1, err)
		}

		if err := enc.Encode(line); err != nil {
			return fmt.Errorf("sample %d: %w", i+1, err)
		}
	}

	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write samples: %w", err)
	}

	return nil
}

type writtenLine struct {
	Device  string          `json:"device"`
	Signal  string          `json:"signal"`
	TS      string          `json:"ts"`
	Value   json.RawMessage `json:"value"`
	Quality string          `json:"quality"`
}

func encodeSample(s model.Sample) (writtenLine, error) {
	switch {
	case s.Device == "":
		return writtenLine{}, errors.New("no device")
	case s.Signal == "":
		return writtenLine{}, errors.New("no signal")
	case s.TS.IsZero():
		return writtenLine{}, errors.New("no timestamp")
	case !s.Quality.Valid():
		return writtenLine{}, errors.New("invalid quality")
	}

	if err := s.Value.Validate(); err != nil {
		return writtenLine{}, err
	}

	var (
		value []byte
		err   error
	)

	switch s.Value.Kind {
	case model.KindFloat:
		value, err = json.Marshal(s.Value.Float)
	case model.KindInt:
		value = strconv.AppendInt(nil, s.Value.Int, 10)
	case model.KindBool:
		value, err = json.Marshal(s.Value.Bool)
	default:
		value, err = json.Marshal(s.Value.Str)
	}

	if err != nil {
		return writtenLine{}, err
	}

	return writtenLine{
		Device:  s.Device,
		Signal:  s.Signal,
		TS:      s.TS.UTC().Format(sampleTimeLayout),
		Value:   value,
		Quality: s.Quality.String(),
	}, nil
}

// ReadSamples reads a sample file written by WriteSamples (or by hand): JSON
// Lines, blank lines skipped. kindOf returns the value kind of a point, which
// is the kind the line's value must have: a JSON number for an integer point
// must be integral, a boolean and a string must match, a number for a float
// point must be finite. A device and signal kindOf does not know are an
// error. Samples get priority critical, which the file does not carry.
//
// The first bad line stops the read: the error names its number and says
// what is wrong, never the device, the signal or the value, and no samples
// are returned.
func ReadSamples(r io.Reader, kindOf func(device, signal string) (model.ValueKind, bool)) ([]model.Sample, error) {
	var out []model.Sample

	sc := bufio.NewScanner(r)
	sc.Buffer(nil, MaxSampleLine)

	n := 0
	for sc.Scan() {
		n++

		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		s, err := decodeSample(line, kindOf)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}

		out = append(out, s)
	}

	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("line %d: %w", n+1, errSampleTooLong)
		}

		return nil, fmt.Errorf("line %d: read: %w", n+1, err)
	}

	return out, nil
}

func decodeSample(line []byte, kindOf func(device, signal string) (model.ValueKind, bool)) (model.Sample, error) {
	var l sampleLine

	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&l); err != nil {
		return model.Sample{}, errSampleSyntax
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return model.Sample{}, errSampleSyntax
	}

	if l.Device == nil || l.Signal == nil || l.TS == nil || l.Quality == nil || len(l.Value) == 0 || string(l.Value) == "null" {
		return model.Sample{}, errSampleSyntax
	}

	kind, known := kindOf(*l.Device, *l.Signal)
	if !known {
		return model.Sample{}, errSampleUnknown
	}

	ts, err := time.Parse(time.RFC3339Nano, *l.TS)
	if err != nil {
		return model.Sample{}, errSampleTime
	}

	quality, err := model.ParseQuality(*l.Quality)
	if err != nil {
		return model.Sample{}, errSampleQuality
	}

	value, err := decodeSampleValue(l.Value, kind)
	if err != nil {
		return model.Sample{}, err
	}

	return model.Sample{
		Device:   *l.Device,
		Signal:   *l.Signal,
		TS:       ts.UTC(),
		Value:    value,
		Quality:  quality,
		Priority: model.Critical,
	}, nil
}

// decodeSampleValue reads raw as a value of the given kind.
func decodeSampleValue(raw json.RawMessage, kind model.ValueKind) (model.Value, error) {
	raw = bytes.TrimSpace(raw)

	switch kind {
	case model.KindBool:
		switch string(raw) {
		case "true":
			return model.BoolValue(true), nil
		case "false":
			return model.BoolValue(false), nil
		}

		return model.Value{}, errors.New("value must be true or false")
	case model.KindString:
		var s string
		if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
			return model.StringValue(s), nil
		}

		return model.Value{}, errors.New("value must be a string")
	case model.KindInt:
		if i, ok := integerNumber(raw); ok {
			return model.IntValue(i), nil
		}

		return model.Value{}, errors.New("value must be an integer")
	default:
		if raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
			return model.Value{}, errors.New("value must be a number")
		}

		f, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return model.Value{}, errors.New("value must be a finite number")
		}

		v, err := model.FloatValue(f)
		if err != nil {
			return model.Value{}, errors.New("value must be a finite number")
		}

		return v, nil
	}
}

// integerNumber reads raw, which must be a JSON number, as an int64. A number
// written with a fraction or an exponent counts when it is integral and fits.
func integerNumber(raw []byte) (int64, bool) {
	if raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
		return 0, false
	}

	if i, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
		return i, true
	}

	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || f != math.Trunc(f) || f < -(1<<63) || f >= 1<<63 {
		return 0, false
	}

	return int64(f), true
}
