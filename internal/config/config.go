// Package config parses and validates the configuration document
// laraplate-edge-config/1 (JSON, or YAML for hand-written files) and keeps the
// last valid one in the data directory.
//
// Validation has two layers. Parse checks the document against the embedded
// JSON Schema, which describes the full format, plus the rules a schema cannot
// express (unique ids, durations and sizes in range, the URL rule). CheckBuild
// then refuses what this build does not provide. No message ever repeats a
// value of the document: a secret written in the wrong place must not reach a
// terminal or a log.
package config

import (
	"encoding/json"
	stdpath "path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

// SchemaName is the value of the document's schema key.
const SchemaName = "laraplate-edge-config/1"

// MaxDocumentSize bounds the size of a document, in bytes.
const MaxDocumentSize = 4 << 20

// Built-in names this build provides.
const (
	// DriverSimulated is the simulated input driver.
	DriverSimulated = "simulated"
	// DriverModbus is the Modbus input driver.
	DriverModbus = "modbus"
	// DriverOPCUA is the OPC UA input driver.
	DriverOPCUA = "opcua"
	// OutputLaraplateHTTP is the HTTP output to the Laraplate ingest route.
	OutputLaraplateHTTP = "laraplate-http"
)

// Read modes of a point.
const (
	// ReadSubscribe asks the source to push changes.
	ReadSubscribe = "subscribe"
	// ReadEvent reads values the source emits as events.
	ReadEvent = "event"
	// ReadPoll reads at a fixed interval.
	ReadPoll = "poll"
)

// Defaults and limits of the document. MaxSamples and MaxBody are the backend
// defaults of mes.machine.max_samples and mes.machine.max_body_kb.
const (
	DefaultDeathTolerance             = 30 * time.Second
	DefaultMaxSamples                 = 1000
	MaxSamples                        = 5000
	DefaultMaxBody            int64   = 1024 << 10
	MaxBody                   int64   = 1024 << 10
	DefaultMaxWait                    = 2 * time.Second
	DefaultBufferMaxSize      int64   = 1 << 30
	MinBufferMaxSize          int64   = 32 << 20
	DefaultMinFreePercent     float64 = 5
	DefaultFsyncInterval              = 200 * time.Millisecond
	defaultPriority                   = "critical"
	defaultMaxBodyText                = "1024KiB"
	defaultBufferSizeText             = "1GiB"
	minBufferSizeText                 = "32MiB"
	defaultDeathToleranceText         = "30s"
	defaultMaxWaitText                = "2s"
	defaultFsyncText                  = "200ms"
)

// Document is a parsed configuration document with defaults applied.
type Document struct {
	// Schema is always SchemaName.
	Schema string
	// Version is the document version, 0 when absent. It is int64 so that
	// every version the schema accepts (up to 2^53-1) fits on 32-bit
	// platforms too.
	Version int64
	// Inputs read machines.
	Inputs []Input
	// Outputs send machine messages.
	Outputs []Output
	// Plugins are external driver and output processes.
	Plugins []Plugin
	// Processors are plugin components deriving signals from observed devices.
	Processors []Processor
	// Raw is the whole document as compact canonical JSON, defaults applied.
	Raw json.RawMessage
}

// Input is one input of the document.
type Input struct {
	// ID is unique among the inputs.
	ID string
	// Driver is a built-in driver name or plugin:<id>.
	Driver string
	// Connection holds the driver settings as canonical JSON, {} when absent.
	Connection json.RawMessage
	// DeathTolerance is how long without a successful read before the
	// input's devices are reported dead.
	DeathTolerance time.Duration
	// Devices are the machines read through this input.
	Devices []Device
	// Raw is the input as compact canonical JSON, defaults applied, so that
	// two configurations can be compared input by input.
	Raw json.RawMessage
}

// Device is one machine of an input.
type Device struct {
	// Device is the device name used in messages.
	Device string
	// Points are the signals read from the machine.
	Points []Point
	// Derived are signals computed from the device's other signals.
	Derived []Derived
}

// Point is one signal read from a machine.
type Point struct {
	// Signal is the signal name, unique per device across all inputs.
	Signal string
	// Address is the driver-specific address of the value.
	Address string
	// Read says how the value is read.
	Read Read
	// Priority is the buffer class, Critical when not set.
	Priority model.Priority
	// Type is the decoding type of the address, empty when not set.
	Type string
	// ByteOrder is ABCD, CDAB, BADC or DCBA, empty when not set.
	ByteOrder string
	// Unit is the engineering unit, empty when not set.
	Unit string
	// Transforms is the transform chain, in order.
	Transforms []Transform
}

// DataType returns the birth data type of the point: boolean for bool,
// string for string, number otherwise.
func (p Point) DataType() string {
	switch p.Type {
	case "bool":
		return model.DataTypeBoolean
	case "string":
		return model.DataTypeString
	default:
		return model.DataTypeNumber
	}
}

// Read is how a point is read.
type Read struct {
	// Mode is ReadSubscribe, ReadEvent or ReadPoll.
	Mode string
	// Interval is the polling interval for ReadPoll.
	Interval time.Duration
	// Subscribe holds the subscription settings of a ReadSubscribe point; nil
	// for every other mode.
	Subscribe *Subscribe
}

// Subscribe is the subscription settings of a point read by subscribe: the
// `read: subscribe` string form (every field absent) or the long form
// `read: {subscribe: {sampling, queue, deadband}}`. Drivers that push changes
// by subscription use them; the others ignore them.
type Subscribe struct {
	// Sampling is how often the source samples the value; SamplingDefault when
	// the document gives none (the source's own publishing interval).
	Sampling time.Duration
	// Queue is the number of values the source queues between two
	// publications; 0 when the document gives none.
	Queue uint32
	// Deadband is the change a value needs to be reported: a non-negative
	// number (absolute) or "N%" (percent of the range); empty when the
	// document gives none.
	Deadband string
}

// SamplingDefault is Subscribe.Sampling when the document gives none.
const SamplingDefault time.Duration = -1

// Transform is one step of a point's transform chain.
type Transform struct {
	// Name is the transform of the catalogue.
	Name string
	// Params are its parameters as canonical JSON.
	Params json.RawMessage
}

// Derived is a signal computed by an expression.
type Derived struct {
	// Signal is the signal name.
	Signal string
	// Type is the declared result type.
	Type string
	// Priority is the buffer class, Critical when not set.
	Priority model.Priority
	// Expr is the expression.
	Expr string
	// Interval evaluates the expression at a fixed interval; zero means on
	// every change of an input it uses.
	Interval time.Duration
	// Transforms is the transform chain applied to the result, in order.
	Transforms []Transform
}

// Output is one output of the document.
type Output struct {
	// ID is unique among the outputs and names the output's buffer directory.
	ID string
	// Type is a built-in output type or plugin:<id>.
	Type string
	// URL is where the output sends.
	URL string
	// Token is a secret:// reference.
	Token string
	// Route selects the samples the output receives.
	Route Route
	// Batch bounds one sent batch.
	Batch Batch
	// Buffer bounds the output's disk buffer.
	Buffer Buffer
	// Config holds the plugin settings as canonical JSON, for plugin:<id>
	// types only; nil when absent.
	Config json.RawMessage
	// Raw is the output as compact canonical JSON, defaults applied.
	Raw json.RawMessage
}

// Route selects samples by device and signal.
type Route struct {
	// Include holds patterns matched with path.Match against device/signal
	// (device and signal names hold no /, so the split is unambiguous).
	// * alone matches everything; a pattern without / means pattern/*.
	Include []string
}

// Match reports whether the sample of signal on device is routed.
func (r Route) Match(device, signal string) bool {
	name := device + "/" + signal
	for _, pattern := range r.Include {
		if pattern == "*" {
			return true
		}
		if !strings.Contains(pattern, "/") {
			pattern += "/*"
		}
		if ok, _ := stdpath.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

// Batch bounds one sent batch.
type Batch struct {
	// MaxSamples is the most samples in a batch.
	MaxSamples int
	// MaxBody is the largest request body, in bytes.
	MaxBody int64
	// MaxWait is the longest a sample waits for its batch to fill.
	MaxWait time.Duration
}

// Buffer bounds an output's disk buffer.
type Buffer struct {
	// MaxSize is the most bytes on disk.
	MaxSize int64
	// MinFreePercent is the share of the disk kept free.
	MinFreePercent float64
	// FsyncInterval is the group sync period of written records.
	FsyncInterval time.Duration
}

// Plugin is one plugin of the document.
type Plugin struct {
	// ID is unique among the plugins.
	ID string
	// Path is the plugin binary.
	Path string
	// SHA256 is the expected hash of the binary.
	SHA256 string
	// ManifestSHA256 is the expected hash of the plugin.json file next to
	// the binary.
	ManifestSHA256 string
	// MaxMemory is the most bytes of memory the plugin process may use, 0
	// for no limit.
	MaxMemory int64
	// Raw is the plugin as compact canonical JSON.
	Raw json.RawMessage
}

// Processor is one processor of the document: a plugin component that
// observes devices of the inputs and derives signals on devices of its own.
type Processor struct {
	// ID is unique among the processors.
	ID string
	// Plugin is the id of the plugin providing the processor.
	Plugin string
	// Observe lists the device names of the inputs it receives.
	Observe []string
	// Devices are the derived devices.
	Devices []DerivedDevice
	// DeathTolerance is how long the processor may be down before its
	// derived devices are declared dead.
	DeathTolerance time.Duration
	// Config holds the plugin settings as canonical JSON, {} when absent.
	Config json.RawMessage
	// Raw is the processor as compact canonical JSON, defaults applied.
	Raw json.RawMessage
}

// DerivedDevice is a device a processor derives.
type DerivedDevice struct {
	// Device is the device name used in messages.
	Device string
	// Signals are the declared derived signals.
	Signals []DerivedSignal
}

// DerivedSignal is a signal a processor derives.
type DerivedSignal struct {
	// Signal is the signal name, unique per device.
	Signal string
	// Type is int, float, bool or string.
	Type string
	// Priority is critical or bulk, critical when not set.
	Priority string
	// Unit is the engineering unit, empty when not set.
	Unit string
}

// Parse reads a configuration document, JSON or YAML (a document whose first
// significant character is { or [ is JSON), and validates it against the
// embedded schema and the semantic rules. It returns the document, or the
// zero Document and one message per problem. The error is only for internal
// failures. Messages name the path of the problem, never its value.
func Parse(data []byte) (Document, []string, error) {
	if len(data) > MaxDocumentSize {
		return Document{}, []string{path{}.msg("larger than 4 MiB")}, nil
	}

	tree, msgs := decodeTree(data)
	if len(msgs) > 0 {
		return Document{}, msgs, nil
	}

	msgs, err := validateSchema(tree)
	if err != nil {
		return Document{}, nil, err
	}
	root, ok := tree.(map[string]any)
	if !ok {
		return Document{}, msgs, nil
	}

	// The semantic checks also run on a document the schema refused, so that
	// one run lists every problem; they then skip values the schema reported.
	applyDefaults(root)
	doc, semantic, err := build(root, len(msgs) > 0)
	if err != nil {
		return Document{}, nil, err
	}
	msgs = append(msgs, uncovered(semantic, msgs)...)
	if len(msgs) > 0 {
		return Document{}, msgs, nil
	}
	return doc, nil, nil
}

// uncovered drops the semantic messages about a path at, above or below one
// a schema message already names.
func uncovered(semantic, schemaMsgs []string) []string {
	covered := make([]string, 0, len(schemaMsgs))
	for _, m := range schemaMsgs {
		p, _, _ := strings.Cut(m, ": ")
		covered = append(covered, p)
	}
	var out []string
	for _, m := range semantic {
		p, _, _ := strings.Cut(m, ": ")
		related := slices.ContainsFunc(covered, func(c string) bool {
			return p == c || strings.HasPrefix(p, c+".") || strings.HasPrefix(c, p+".")
		})
		if !related {
			out = append(out, m)
		}
	}
	return out
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func setDefault(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok && m != nil {
		m[key] = value
	}
}

func ensureObject(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if o := object(m[key]); o != nil {
		return o
	}
	o := map[string]any{}
	m[key] = o
	return o
}

// applyDefaults fills the defaults the schema declares. On a tree the schema
// refused it only fills what has the expected shape (a misshapen batch or
// buffer is replaced; the document is discarded anyway).
func applyDefaults(root map[string]any) {
	for _, in := range list(root["inputs"]) {
		input := object(in)
		setDefault(input, "connection", map[string]any{})
		setDefault(input, "death_tolerance", defaultDeathToleranceText)
		for _, dev := range list(input["devices"]) {
			device := object(dev)
			for _, pt := range list(device["points"]) {
				setDefault(object(pt), "priority", defaultPriority)
			}
			for _, d := range list(device["derived"]) {
				setDefault(object(d), "priority", defaultPriority)
			}
		}
	}
	for _, pr := range list(root["processors"]) {
		processor := object(pr)
		setDefault(processor, "death_tolerance", defaultDeathToleranceText)
		setDefault(processor, "config", map[string]any{})
		for _, dev := range list(processor["devices"]) {
			for _, sig := range list(object(dev)["signals"]) {
				setDefault(object(sig), "priority", defaultPriority)
			}
		}
	}
	for _, out := range list(root["outputs"]) {
		output := object(out)
		setDefault(ensureObject(output, "route"), "include", []any{"*"})
		batch := ensureObject(output, "batch")
		setDefault(batch, "max_samples", json.Number(strconv.Itoa(DefaultMaxSamples)))
		setDefault(batch, "max_body", defaultMaxBodyText)
		setDefault(batch, "max_wait", defaultMaxWaitText)
		buffer := ensureObject(output, "buffer")
		setDefault(buffer, "max_size", defaultBufferSizeText)
		setDefault(buffer, "min_free_percent", json.Number(strconv.FormatFloat(DefaultMinFreePercent, 'g', -1, 64)))
		setDefault(buffer, "fsync_interval", defaultFsyncText)
	}
}
