package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/atomicfile"
	"github.com/getlaraplate/laraplate-edge/internal/bootstrap"
	"github.com/getlaraplate/laraplate-edge/internal/clock"
	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/transform"
)

// probeTimeout bounds the one read of every point.
const probeTimeout = 30 * time.Second

// runProbe runs "probe <input>" (decision P8): it builds the input the
// document names through the driver registry of run, reads every point once
// and prints the raw driver samples, before any transform. It never starts
// the input, an output or the status endpoint, and writes nothing but the
// --record file. The samples are printed on purpose: probe is an operator
// tool, and prints samples only, never the document.
func runProbe(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("probe", stderr)
	bootstrapPath := fs.String("bootstrap", "", "path to the bootstrap file; its data_dir holds the last valid document")
	configFile := fs.String("config", "", "configuration document to use instead of the last valid one")
	record := fs.String("record", "", "write the samples to this file as JSON Lines")
	positional, code, ok := parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	if *configFile == "" && *bootstrapPath == "" {
		fmt.Fprintf(stderr, "laraplate-edge probe: --config <file> or --bootstrap <path> is required\n")
		return exitUsage
	}

	doc, boot, msgs, err := probeDocument(*configFile, *bootstrapPath)
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge probe: %v\n", err)
		return exitFailure
	}
	if len(msgs) > 0 {
		printMessages(stderr, *configFile, msgs)
		return exitFailure
	}

	samples, elapsed, err := probeInput(doc, boot, positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge probe: %v\n", err)
		return exitFailure
	}

	if *record != "" {
		if err := recordSamples(*record, samples); err != nil {
			fmt.Fprintf(stderr, "laraplate-edge probe: --record %v\n", err)
			return exitFailure
		}
	}

	var out bytes.Buffer
	for _, s := range samples {
		fmt.Fprintf(&out, "%s\t%s\t%s\t%s\t%s\n", s.Device, s.Signal, formatValue(s.Value), kindName(s.Value.Kind), s.Quality)
	}
	fmt.Fprintf(&out, "response time: %s\n", elapsed.Round(time.Microsecond))
	if _, err := stdout.Write(out.Bytes()); err != nil {
		fmt.Fprintf(stderr, "laraplate-edge probe: write the samples: %v\n", err)
		return exitFailure
	}

	return exitOK
}

// probeDocument returns the document to probe and the bootstrap the driver
// registry is built from. With a configuration file the document is checked
// like config validate does, the messages being returned, and relative paths
// of the connection resolve against that file's directory, as the file
// provider of run does. Without one it is the last valid document of the
// bootstrap's data_dir.
func probeDocument(configFile, bootstrapPath string) (doc config.Document, boot bootstrap.File, msgs []string, err error) {
	if configFile != "" {
		doc, msgs, err = loadDocument(configFile)
		boot = bootstrap.File{Config: bootstrap.Config{Provider: bootstrap.ProviderFile, File: configFile}}

		return doc, boot, msgs, err
	}

	boot, err = bootstrap.Load(bootstrapPath)
	if err != nil {
		return doc, boot, nil, err
	}

	doc, found, err := config.NewStore(boot.DataDir).LastValid()
	if err != nil {
		return doc, boot, nil, err
	}
	if !found {
		return doc, boot, nil, fmt.Errorf("no configuration has been applied yet under data_dir %s: use --config <file>", boot.DataDir)
	}

	return doc, boot, nil, nil
}

// probeInput builds the input id of doc and reads every point once. The
// elapsed time is the response time of that read.
func probeInput(doc config.Document, boot bootstrap.File, id string) ([]model.Sample, time.Duration, error) {
	var cfg *config.Input
	for i := range doc.Inputs {
		if doc.Inputs[i].ID == id {
			cfg = &doc.Inputs[i]

			break
		}
	}
	if cfg == nil {
		return nil, 0, fmt.Errorf("the configuration has no input %q (inputs: %s)", id, inputIDs(doc))
	}

	factory := inputDrivers(boot, nil)[cfg.Driver]
	if factory == nil {
		return nil, 0, fmt.Errorf("input %q: driver %s is not available in this build", id, cfg.Driver)
	}

	clk := clock.Real()

	in, err := factory(*cfg, clk)
	if err != nil {
		return nil, 0, fmt.Errorf("input %q: %w", id, err)
	}
	defer func() { _ = in.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	start := clk.Now()

	samples, err := in.Probe(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("input %q: %w", id, err)
	}

	return samples, clk.Now().Sub(start), nil
}

// inputIDs lists the input ids of doc for a message.
func inputIDs(doc config.Document) string {
	if len(doc.Inputs) == 0 {
		return "none"
	}

	var out []byte
	for i, in := range doc.Inputs {
		if i > 0 {
			out = append(out, ", "...)
		}
		out = append(out, in.ID...)
	}

	return string(out)
}

// recordSamples writes samples to file as JSON Lines, atomically.
func recordSamples(file string, samples []model.Sample) error {
	var buf bytes.Buffer
	if err := transform.WriteSamples(&buf, samples); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}

	if err := atomicfile.Write(filepath.Dir(file), filepath.Base(file), buf.Bytes()); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}

	return nil
}

// formatValue renders a value on one line: a string is quoted, so that a
// control character from a machine never reaches the terminal.
func formatValue(v model.Value) string {
	switch v.Kind {
	case model.KindBool:
		return strconv.FormatBool(v.Bool)
	case model.KindInt:
		return strconv.FormatInt(v.Int, 10)
	case model.KindFloat:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case model.KindString:
		return strconv.Quote(v.Str)
	default:
		return "?"
	}
}

// kindName names the kind of a value.
func kindName(k model.ValueKind) string {
	switch k {
	case model.KindBool:
		return "boolean"
	case model.KindInt:
		return "integer"
	case model.KindFloat:
		return "float"
	case model.KindString:
		return "string"
	default:
		return "unknown"
	}
}
