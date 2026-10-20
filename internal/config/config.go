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
	"errors"
	"math"
	stdpath "path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/docrules"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/secrets"
)

// SchemaName is the value of the document's schema key.
const SchemaName = "laraplate-edge-config/1"

// MaxDocumentSize bounds the size of a document, in bytes.
const MaxDocumentSize = 4 << 20

// Built-in names this build provides.
const (
	// DriverSimulated is the simulated input driver.
	DriverSimulated = "simulated"
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
}

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
	// Raw is the plugin as compact canonical JSON.
	Raw json.RawMessage
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

type wireDocument struct {
	Schema  string `json:"schema"`
	Version int64  `json:"version"`
}

type wireInput struct {
	ID             string          `json:"id"`
	Driver         string          `json:"driver"`
	Connection     json.RawMessage `json:"connection"`
	DeathTolerance string          `json:"death_tolerance"`
	Devices        []struct {
		Device string `json:"device"`
		Points []struct {
			Signal     string                       `json:"signal"`
			Address    string                       `json:"address"`
			Read       json.RawMessage              `json:"read"`
			Priority   string                       `json:"priority"`
			Type       string                       `json:"type"`
			ByteOrder  string                       `json:"byte_order"`
			Unit       string                       `json:"unit"`
			Transforms []map[string]json.RawMessage `json:"transforms"`
		} `json:"points"`
		Derived []struct {
			Signal   string `json:"signal"`
			Type     string `json:"type"`
			Priority string `json:"priority"`
			Expr     string `json:"expr"`
			On       struct {
				Interval string `json:"interval"`
			} `json:"on"`
			Transforms []map[string]json.RawMessage `json:"transforms"`
		} `json:"derived"`
	} `json:"devices"`
}

type wireOutput struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	Token string `json:"token"`
	Route struct {
		Include []string `json:"include"`
	} `json:"route"`
	Batch struct {
		MaxSamples int    `json:"max_samples"`
		MaxBody    string `json:"max_body"`
		MaxWait    string `json:"max_wait"`
	} `json:"batch"`
	Buffer struct {
		MaxSize        string  `json:"max_size"`
		MinFreePercent float64 `json:"min_free_percent"`
		FsyncInterval  string  `json:"fsync_interval"`
	} `json:"buffer"`
}

type wirePlugin struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// builder turns a defaulted tree into a Document and collects the semantic
// problems the schema cannot express. A lenient builder works on a tree the
// schema refused: values of the wrong type are left zero.
type builder struct {
	msgs    []string
	lenient bool
}

func (b *builder) add(p path, text string) { b.msgs = append(b.msgs, p.msg(text)) }

// component renders one component and decodes it into dst. The error is a
// fixed text: the json error would quote a value of the document.
func (b *builder) component(v any, dst any) (json.RawMessage, error) {
	raw, err := canonical(v)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, dst); err != nil && !b.lenient {
		return nil, errDecodeComponent
	}
	return raw, nil
}

// errDecodeComponent reports a validated component that does not fit its Go
// type: a mismatch between the schema and the code, never a user error.
var errDecodeComponent = errors.New("config: internal error: a validated component does not fit its Go type")

func build(root map[string]any, lenient bool) (Document, []string, error) {
	b := &builder{lenient: lenient}
	var top wireDocument
	raw, err := b.component(root, &top)
	if err != nil {
		return Document{}, nil, err
	}
	doc := Document{Schema: top.Schema, Version: top.Version, Raw: raw}

	for i, v := range list(root["inputs"]) {
		in, err := b.input(path{"inputs"}.index(i), v)
		if err != nil {
			return Document{}, nil, err
		}
		doc.Inputs = append(doc.Inputs, in)
	}
	for i, v := range list(root["outputs"]) {
		out, err := b.output(path{"outputs"}.index(i), v)
		if err != nil {
			return Document{}, nil, err
		}
		doc.Outputs = append(doc.Outputs, out)
	}
	for _, v := range list(root["plugins"]) {
		var w wirePlugin
		raw, err := b.component(v, &w)
		if err != nil {
			return Document{}, nil, err
		}
		doc.Plugins = append(doc.Plugins, Plugin{ID: w.ID, Path: w.Path, SHA256: w.SHA256, Raw: raw})
	}

	b.uniqueIDs(doc)
	b.uniqueSignals(doc)
	return doc, b.msgs, nil
}

func (b *builder) input(p path, v any) (Input, error) {
	var w wireInput
	raw, err := b.component(v, &w)
	if err != nil {
		return Input{}, err
	}
	in := Input{
		ID:             w.ID,
		Driver:         w.Driver,
		Connection:     w.Connection,
		DeathTolerance: b.duration(p.key("death_tolerance"), w.DeathTolerance),
		Raw:            raw,
	}
	walkSecrets(object(object(v)["connection"]), p.key("connection"), func(rp path, ref string) {
		if err := secrets.Check(ref); err != nil && !errors.Is(err, secrets.ErrUnavailable) {
			b.add(rp, secretMessage)
		}
	})
	for d, wd := range w.Devices {
		dp := p.key("devices").index(d)
		dev := Device{Device: wd.Device}
		for k, wp := range wd.Points {
			pp := dp.key("points").index(k)
			pt := Point{
				Signal:    wp.Signal,
				Address:   wp.Address,
				Read:      b.read(pp.key("read"), wp.Read),
				Priority:  priority(wp.Priority),
				Type:      wp.Type,
				ByteOrder: wp.ByteOrder,
				Unit:      wp.Unit,
			}
			pt.Transforms = transforms(wp.Transforms)
			dev.Points = append(dev.Points, pt)
		}
		for k, wdv := range wd.Derived {
			derived := Derived{
				Signal:     wdv.Signal,
				Type:       wdv.Type,
				Priority:   priority(wdv.Priority),
				Expr:       wdv.Expr,
				Transforms: transforms(wdv.Transforms),
			}
			if wdv.On.Interval != "" {
				derived.Interval = b.duration(dp.key("derived").index(k).key("on").key("interval"), wdv.On.Interval)
			}
			dev.Derived = append(dev.Derived, derived)
		}
		in.Devices = append(in.Devices, dev)
	}
	return in, nil
}

func (b *builder) output(p path, v any) (Output, error) {
	var w wireOutput
	raw, err := b.component(v, &w)
	if err != nil {
		return Output{}, err
	}
	out := Output{
		ID:    w.ID,
		Type:  w.Type,
		URL:   w.URL,
		Token: w.Token,
		Route: Route{Include: w.Route.Include},
		Batch: Batch{
			MaxSamples: w.Batch.MaxSamples,
			MaxBody:    b.size(p.key("batch").key("max_body"), w.Batch.MaxBody),
			MaxWait:    b.duration(p.key("batch").key("max_wait"), w.Batch.MaxWait),
		},
		Buffer: Buffer{
			MaxSize:        b.size(p.key("buffer").key("max_size"), w.Buffer.MaxSize),
			MinFreePercent: w.Buffer.MinFreePercent,
			FsyncInterval:  b.duration(p.key("buffer").key("fsync_interval"), w.Buffer.FsyncInterval),
		},
		Raw: raw,
	}
	if out.Batch.MaxBody > MaxBody {
		b.add(p.key("batch").key("max_body"), "must not exceed "+defaultMaxBodyText+" (the backend default of mes.machine.max_body_kb)")
	}
	if out.Buffer.MaxSize > 0 && out.Buffer.MaxSize < MinBufferMaxSize {
		b.add(p.key("buffer").key("max_size"), "must be at least "+minBufferSizeText+" (two segments of 16MiB)")
	}
	if out.Type == OutputLaraplateHTTP {
		if err := docrules.CheckURL(out.URL); err != nil {
			b.add(p.key("url"), err.Error())
		}
	}
	if out.Token != "" {
		if err := secrets.Check(out.Token); err != nil && !errors.Is(err, secrets.ErrUnavailable) {
			b.add(p.key("token"), secretMessage)
		}
	}
	for i, pattern := range out.Route.Include {
		if _, err := stdpath.Match(pattern, ""); err != nil {
			b.add(p.key("route").key("include").index(i), "invalid pattern")
		}
	}
	return out, nil
}

// transforms converts a chain of single-key objects.
func transforms(chain []map[string]json.RawMessage) []Transform {
	var out []Transform
	for _, step := range chain {
		for name, params := range step {
			out = append(out, Transform{Name: name, Params: params})
		}
	}
	return out
}

func priority(s string) model.Priority {
	if s == model.Bulk.String() {
		return model.Bulk
	}
	return model.Critical
}

func (b *builder) read(p path, raw json.RawMessage) Read {
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		return Read{Mode: mode}
	}
	var poll struct {
		Poll string `json:"poll"`
	}
	_ = json.Unmarshal(raw, &poll)
	return Read{Mode: ReadPoll, Interval: b.duration(p.key("poll"), poll.Poll)}
}

// duration parses a duration the schema pattern accepted; it must be
// positive and fit a time.Duration.
func (b *builder) duration(p path, text string) time.Duration {
	d, err := time.ParseDuration(text)
	switch {
	case err != nil:
		b.add(p, "is too large")
	case d <= 0:
		b.add(p, "must be greater than zero")
	}
	return d
}

var sizeUnits = []struct {
	suffix string
	factor int64
}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"B", 1}}

// size parses a size the schema pattern accepted; it must be positive and
// fit an int64.
func (b *builder) size(p path, text string) int64 {
	for _, unit := range sizeUnits {
		digits, ok := strings.CutSuffix(text, unit.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		switch {
		case err != nil || n > math.MaxInt64/unit.factor:
			b.add(p, "is too large")
			return 0
		case n == 0:
			b.add(p, "must be greater than zero")
			return 0
		}
		return n * unit.factor
	}
	b.add(p, "must be a size such as 512KiB, 16MiB or 1GiB")
	return 0
}

func (b *builder) uniqueIDs(doc Document) {
	check := func(list string, ids []string) {
		first := map[string]int{}
		for i, id := range ids {
			if id == "" {
				continue
			}
			if j, dup := first[id]; dup {
				b.add(path{list}.index(i).key("id"), "duplicate of "+path{list}.index(j).key("id").String())
				continue
			}
			first[id] = i
		}
	}
	ids := func(n int, id func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = id(i)
		}
		return out
	}
	check("inputs", ids(len(doc.Inputs), func(i int) string { return doc.Inputs[i].ID }))
	check("outputs", ids(len(doc.Outputs), func(i int) string { return doc.Outputs[i].ID }))
	check("plugins", ids(len(doc.Plugins), func(i int) string { return doc.Plugins[i].ID }))
}

// uniqueSignals refuses a (device, signal) pair defined twice, by points or
// derived signals, across all inputs.
func (b *builder) uniqueSignals(doc Document) {
	type key struct{ device, signal string }
	first := map[key]path{}
	see := func(k key, p path) {
		if k.device == "" || k.signal == "" {
			return
		}
		if q, dup := first[k]; dup {
			b.add(p, "duplicate of "+q.String()+" on the same device")
			return
		}
		first[k] = p
	}
	for i, in := range doc.Inputs {
		for d, dev := range in.Devices {
			dp := path{"inputs"}.index(i).key("devices").index(d)
			for k, pt := range dev.Points {
				see(key{dev.Device, pt.Signal}, dp.key("points").index(k).key("signal"))
			}
			for k, dv := range dev.Derived {
				see(key{dev.Device, dv.Signal}, dp.key("derived").index(k).key("signal"))
			}
		}
	}
}

// secretKeySuffixes mark the connection keys holding secret references, at
// any depth: a key equal to or ending with one of them, in any letter case.
// The schema's connection patternProperties states the same rule.
var secretKeySuffixes = []string{"password", "token", "secret", "api_key"}

func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	return slices.ContainsFunc(secretKeySuffixes, func(suffix string) bool { return strings.HasSuffix(lower, suffix) })
}

// walkSecrets calls fn for every string held by a secret key in v, in key
// order, with its path.
func walkSecrets(v any, p path, fn func(path, string)) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if s, ok := x[k].(string); ok && isSecretKey(k) {
				fn(p.key(k), s)
				continue
			}
			walkSecrets(x[k], p.key(k), fn)
		}
	case []any:
		for i, item := range x {
			walkSecrets(item, p.index(i), fn)
		}
	}
}
