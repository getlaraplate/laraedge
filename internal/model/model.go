// Package model defines the internal data model of the agent: the samples and
// device events that flow from inputs to outputs, and the loss markers that
// record every drop. The types are plain data (exported fields, no funcs) so
// that the disk buffer can serialize them.
package model

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Quality is the quality of a sample, mapped by the driver from its errors.
// The zero value is invalid.
type Quality uint8

// Sample qualities, in the order of the protocol.
const (
	Good Quality = iota + 1
	Uncertain
	Bad
)

// Valid reports whether q is one of the defined qualities.
func (q Quality) Valid() bool { return q >= Good && q <= Bad }

// String returns the wire string of the quality.
func (q Quality) String() string {
	switch q {
	case Good:
		return "good"
	case Uncertain:
		return "uncertain"
	case Bad:
		return "bad"
	default:
		return fmt.Sprintf("invalid(%d)", uint8(q))
	}
}

// ParseQuality converts a wire string to a Quality.
func ParseQuality(s string) (Quality, error) {
	switch s {
	case "good":
		return Good, nil
	case "uncertain":
		return Uncertain, nil
	case "bad":
		return Bad, nil
	default:
		return 0, fmt.Errorf("unknown quality %q (want good, uncertain or bad)", s)
	}
}

// Priority is the class of a sample: which of the two buffer queues holds it.
// The zero value is invalid.
type Priority uint8

// Priority classes.
const (
	// Critical is for states, alarms, counts, measurements, references, birth and death.
	Critical Priority = iota + 1
	// Bulk is for process values.
	Bulk
)

// Valid reports whether p is one of the defined priorities.
func (p Priority) Valid() bool { return p == Critical || p == Bulk }

// String returns the wire string of the priority.
func (p Priority) String() string {
	switch p {
	case Critical:
		return "critical"
	case Bulk:
		return "bulk"
	default:
		return fmt.Sprintf("invalid(%d)", uint8(p))
	}
}

// ParsePriority converts a wire string to a Priority.
func ParsePriority(s string) (Priority, error) {
	switch s {
	case "critical":
		return Critical, nil
	case "bulk":
		return Bulk, nil
	default:
		return 0, fmt.Errorf("unknown priority %q (want critical or bulk)", s)
	}
}

// ValueKind says which field of a Value is set. The zero value is invalid.
type ValueKind uint8

// Value kinds.
const (
	KindFloat ValueKind = iota + 1
	KindInt
	KindBool
	KindString
)

// Value holds exactly one of float64, int64, bool or string: the one named by
// Kind. Build it with the constructors; the fields are exported only so that
// the value can be serialized.
type Value struct {
	Kind  ValueKind
	Float float64
	Int   int64
	Bool  bool
	Str   string
}

// FloatValue returns a float Value. NaN and infinities are refused so that
// such values never reach the buffer or the wire.
func FloatValue(f float64) (Value, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Value{}, fmt.Errorf("float value %v is not finite", f)
	}

	return Value{Kind: KindFloat, Float: f}, nil
}

// IntValue returns an integer Value.
func IntValue(i int64) Value { return Value{Kind: KindInt, Int: i} }

// BoolValue returns a boolean Value.
func BoolValue(b bool) Value { return Value{Kind: KindBool, Bool: b} }

// StringValue returns a string Value.
func StringValue(s string) Value { return Value{Kind: KindString, Str: s} }

// Validate reports whether the value has a known kind and, for floats, is finite.
func (v Value) Validate() error {
	switch v.Kind {
	case KindFloat:
		if math.IsNaN(v.Float) || math.IsInf(v.Float, 0) {
			return fmt.Errorf("float value %v is not finite", v.Float)
		}
	case KindInt, KindBool, KindString:
	default:
		return fmt.Errorf("invalid value kind %d", uint8(v.Kind))
	}

	return nil
}

// Sample is one reading of one signal of one device.
type Sample struct {
	Device   string
	Signal   string
	TS       time.Time
	Value    Value
	Quality  Quality
	Priority Priority
}

// Validate reports whether the sample can be put on the wire.
func (s Sample) Validate() error {
	if s.Device == "" {
		return errors.New("sample without device")
	}

	if s.Signal == "" {
		return errors.New("sample without signal")
	}

	if s.TS.IsZero() {
		return errors.New("sample without timestamp")
	}

	if err := s.Value.Validate(); err != nil {
		return fmt.Errorf("sample value: %w", err)
	}

	if !s.Quality.Valid() {
		return fmt.Errorf("sample quality: invalid value %d", uint8(s.Quality))
	}

	if !s.Priority.Valid() {
		return fmt.Errorf("sample priority: invalid value %d", uint8(s.Priority))
	}

	return nil
}

// Data types of a signal declared in a birth.
const (
	DataTypeNumber  = "number"
	DataTypeBoolean = "boolean"
	DataTypeString  = "string"
)

// SignalInfo describes one signal in a birth. An empty Unit is not written.
type SignalInfo struct {
	Signal   string
	DataType string
	Unit     string
}

// Validate reports whether the signal description is valid.
func (si SignalInfo) Validate() error {
	if si.Signal == "" {
		return errors.New("signal info without signal key")
	}

	switch si.DataType {
	case DataTypeNumber, DataTypeBoolean, DataTypeString:
		return nil
	default:
		return fmt.Errorf("signal %q: unknown data type %q", si.Signal, si.DataType)
	}
}

// EventKind is the kind of a device event. The zero value is invalid.
type EventKind uint8

// Device event kinds.
const (
	// Birth announces a device and lists its signals.
	Birth EventKind = iota + 1
	// Death says the driver lost the machine.
	Death
)

// String returns the wire string of the kind.
func (k EventKind) String() string {
	switch k {
	case Birth:
		return "birth"
	case Death:
		return "death"
	default:
		return fmt.Sprintf("invalid(%d)", uint8(k))
	}
}

// DeviceEvent is a device lifecycle event. Signals is set for Birth only.
type DeviceEvent struct {
	Kind    EventKind
	Device  string
	Signals []SignalInfo
	TS      time.Time
}

// Validate reports whether the event can be put on the wire.
func (e DeviceEvent) Validate() error {
	if e.Device == "" {
		return errors.New("device event without device")
	}

	switch e.Kind {
	case Birth:
		if len(e.Signals) == 0 {
			return fmt.Errorf("birth of %q lists no signals", e.Device)
		}

		for _, si := range e.Signals {
			if err := si.Validate(); err != nil {
				return fmt.Errorf("birth of %q: %w", e.Device, err)
			}
		}
	case Death:
		if len(e.Signals) != 0 {
			return fmt.Errorf("death of %q lists signals", e.Device)
		}
	default:
		return fmt.Errorf("device event of %q: invalid kind %d", e.Device, uint8(e.Kind))
	}

	return nil
}

// Record is what the buffer stores: exactly one of a sample or a device event.
type Record struct {
	Sample *Sample
	Event  *DeviceEvent
}

// SampleRecord wraps a copy of s in a Record.
func SampleRecord(s Sample) Record { return Record{Sample: &s} }

// EventRecord wraps a copy of e in a Record.
func EventRecord(e DeviceEvent) Record { return Record{Event: &e} }

// Device returns the device the record belongs to, or "" for an empty record.
func (r Record) Device() string {
	switch {
	case r.Sample != nil:
		return r.Sample.Device
	case r.Event != nil:
		return r.Event.Device
	default:
		return ""
	}
}

// Priority returns the record's class: the sample's own, and Critical for
// device events (birth and death are critical).
func (r Record) Priority() Priority {
	if r.Sample != nil {
		return r.Sample.Priority
	}

	return Critical
}

// Validate reports whether the record holds exactly one valid item.
func (r Record) Validate() error {
	switch {
	case r.Sample != nil && r.Event != nil:
		return errors.New("record holds both a sample and an event")
	case r.Sample != nil:
		return r.Sample.Validate()
	case r.Event != nil:
		return r.Event.Validate()
	default:
		return errors.New("empty record")
	}
}

// Loss reasons.
const (
	// LossBufferFull marks samples evicted because the buffer was full.
	LossBufferFull = "buffer_full"
	// LossRejectedInvalid marks samples the backend refused as invalid.
	LossRejectedInvalid = "rejected_invalid"
)

// Loss is a loss marker: a range of samples of one class that were dropped.
type Loss struct {
	Priority Priority
	FromTS   time.Time
	ToTS     time.Time
	Samples  int
	Reason   string
}

// Validate reports whether the marker can be put on the wire.
func (l Loss) Validate() error {
	if !l.Priority.Valid() {
		return fmt.Errorf("loss priority: invalid value %d", uint8(l.Priority))
	}

	if l.FromTS.IsZero() || l.ToTS.IsZero() {
		return errors.New("loss without timestamps")
	}

	if l.ToTS.Before(l.FromTS) {
		return errors.New("loss ends before it starts")
	}

	if l.Samples < 1 {
		return fmt.Errorf("loss counts %d samples (want at least 1)", l.Samples)
	}

	switch l.Reason {
	case LossBufferFull, LossRejectedInvalid:
		return nil
	default:
		return fmt.Errorf("unknown loss reason %q", l.Reason)
	}
}
