package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/clock"
	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/transform"
)

// maxFlushFires bounds the fires of the end-of-file flush of config test.
const maxFlushFires = 1_000_000

// errFlushBound is returned when the time-driven steps still have a deadline
// after maxFlushFires fires: a step that resends forever (heartbeat) never
// settles.
var errFlushBound = errors.New("the time-driven steps never settle: a step keeps emitting at its deadline (a heartbeat resends forever)")

// runConfigTest runs "config test <file> --input <samples.jsonl>" (decision
// P7): it replays the recorded samples through the transform chains of the
// document and writes the resulting samples to stdout, as JSON Lines, in
// emission order. Nothing is read or written besides the two files, so the
// same arguments always give the same output.
func runConfigTest(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("config test", stderr)
	inputFile := fs.String("input", "", "recorded samples, JSON Lines")
	positional, code, ok := parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	if *inputFile == "" {
		fmt.Fprintf(stderr, "laraplate-edge config test: --input <samples.jsonl> is required\n")
		return exitUsage
	}

	file := positional[0]
	doc, msgs, err := loadDocument(file)
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config test: %v\n", err)
		return exitFailure
	}
	if len(msgs) > 0 {
		printMessages(stderr, file, msgs)
		return exitFailure
	}

	samples, err := readSampleFile(*inputFile, pointKinds(doc))
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config test: %v\n", err)
		return exitFailure
	}

	out, _, err := replay(doc, samples, maxFlushFires)
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config test: %v\n", err)
		return exitFailure
	}

	var buf bytes.Buffer
	if err := transform.WriteSamples(&buf, out); err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config test: %v\n", err)
		return exitFailure
	}
	_, _ = stdout.Write(buf.Bytes())

	return exitOK
}

// readSampleFile reads a sample file; an error names the file.
func readSampleFile(file string, kindOf func(device, signal string) (model.ValueKind, bool)) ([]model.Sample, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	samples, err := transform.ReadSamples(f, kindOf)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}

	return samples, nil
}

// pointKey identifies a point of a document.
type pointKey struct{ device, signal string }

// pointKinds returns the kindOf of transform.ReadSamples for the points of
// doc: the input kind of each point, from its decoding type.
func pointKinds(doc config.Document) func(device, signal string) (model.ValueKind, bool) {
	kinds := map[pointKey]model.ValueKind{}

	for _, in := range doc.Inputs {
		for _, d := range in.Devices {
			for _, p := range d.Points {
				kinds[pointKey{d.Device, p.Signal}] = transform.KindOf(p.Type)
			}
		}
	}

	return func(device, signal string) (model.ValueKind, bool) {
		k, ok := kinds[pointKey{device, signal}]

		return k, ok
	}
}

// replayer holds the chains of a document and the virtual clock.
type replayer struct {
	clk    *clock.Fake
	order  []*transform.Chain
	chains map[pointKey]*transform.Chain
	out    []model.Sample
	errs   int
}

// replay runs samples through the chains of doc on a virtual clock and
// returns what comes out, in emission order, and the number of runtime errors
// of the steps (each also visible as a bad sample).
//
// The clock starts at the first sample's ts. A sample later than the clock
// moves it there and lets the chains with a due deadline fire, in document
// order, before the sample is applied; a sample earlier than the clock is
// applied at the current time, the clock never moves back (R4). After the last
// sample every remaining deadline fires, in time order, until none is pending;
// more than maxFires fires are errFlushBound. A sample of a point without a
// chain passes through. The chains have no persistence.
func replay(doc config.Document, samples []model.Sample, maxFires int) ([]model.Sample, int, error) {
	if len(samples) == 0 {
		return nil, 0, nil
	}

	r := &replayer{clk: clock.NewFake(samples[0].TS), chains: map[pointKey]*transform.Chain{}}

	for _, in := range doc.Inputs {
		for _, d := range in.Devices {
			for _, p := range d.Points {
				if len(p.Transforms) == 0 {
					continue
				}

				chain, err := transform.Compile(p.Transforms, transform.KindOf(p.Type), nil)
				if err != nil {
					return nil, 0, fmt.Errorf("input %s: %w", in.ID, err)
				}

				r.chains[pointKey{d.Device, p.Signal}] = chain
				r.order = append(r.order, chain)
			}
		}
	}

	for _, s := range samples {
		if now := r.clk.Now(); s.TS.After(now) {
			r.clk.Advance(s.TS.Sub(now))
			r.fireDue()
		}

		chain, ok := r.chains[pointKey{s.Device, s.Signal}]
		if !ok {
			r.out = append(r.out, s)

			continue
		}

		emitted, errs := chain.Apply(s)
		r.out = append(r.out, emitted...)
		r.errs += len(errs)
	}

	for fires := 0; ; fires++ {
		earliest, pending := r.earliest()
		if !pending {
			break
		}
		if fires >= maxFires {
			return nil, 0, errFlushBound
		}

		if now := r.clk.Now(); earliest.After(now) {
			r.clk.Advance(earliest.Sub(now))
		}
		r.fireDue()
	}

	return r.out, r.errs, nil
}

// earliest returns the earliest deadline of the chains.
func (r *replayer) earliest() (time.Time, bool) {
	var (
		first time.Time
		found bool
	)

	for _, c := range r.order {
		if d, ok := c.Deadline(); ok && (!found || d.Before(first)) {
			first, found = d, true
		}
	}

	return first, found
}

// fireDue fires the chains whose deadline the clock reached, in document
// order.
func (r *replayer) fireDue() {
	now := r.clk.Now()

	for _, c := range r.order {
		if d, ok := c.Deadline(); !ok || d.After(now) {
			continue
		}

		emitted, errs := c.Fire(now)
		r.out = append(r.out, emitted...)
		r.errs += len(errs)
	}
}
