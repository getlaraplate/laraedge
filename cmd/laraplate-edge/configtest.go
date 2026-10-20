package main

import (
	"bytes"
	"encoding/json"
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

// maxReplayFires is the defensive bound on the fires of one replay. The replay
// ends by itself (it stops at a horizon); the bound only stops a document
// whose steps fire at an absurd rate over a long gap.
const maxReplayFires = 1_000_000

// errFlushBound is returned when a replay needs more than its bound of fires.
var errFlushBound = errors.New("too many time-driven firings: a step with a very short period over a long gap in the samples")

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

	out, errs, err := replay(doc, samples, maxReplayFires)
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config test: %v\n", err)
		return exitFailure
	}

	var buf bytes.Buffer
	if err := transform.WriteSamples(&buf, out); err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config test: %v\n", err)
		return exitFailure
	}
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config test: write the samples: %v\n", err)
		return exitFailure
	}
	if errs > 0 {
		fmt.Fprintf(stderr, "laraplate-edge config test: transform errors: %d (the samples are marked bad)\n", errs)
	}

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
	clk      *clock.Fake
	order    []*transform.Chain
	chains   map[pointKey]*transform.Chain
	out      []model.Sample
	errs     int
	fires    int
	maxFires int
}

// replay runs samples through the chains of doc on a virtual clock and
// returns what comes out, in emission order, and the number of runtime errors
// of the steps (each also visible as a bad sample).
//
// The clock starts at the first sample's ts. Before a sample later than the
// clock, every deadline strictly before its ts fires in time order, the clock
// moving to each deadline (ruling R12: a heartbeat repeats across a gap;
// chains due at the same instant fire in document order); a deadline equal to
// the sample's ts is handled after the sample is applied, as the live
// processor does (a heartbeat the sample re-arms resends nothing). A sample earlier than the clock
// is applied at the current time, the clock never moves back (R4). After the
// last sample the deadlines fire up to a horizon: the later of the last
// sample's ts and the clock, plus the longest duration among the heartbeat,
// min_interval and debounce steps of the document; a deadline past it stays
// pending, so a heartbeat does not resend forever. More than maxFires fires
// is errFlushBound, a safeguard only. A sample of a point without a chain
// passes through. The chains have no persistence.
func replay(doc config.Document, samples []model.Sample, maxFires int) ([]model.Sample, int, error) {
	if len(samples) == 0 {
		return nil, 0, nil
	}

	r := &replayer{clk: clock.NewFake(samples[0].TS), chains: map[pointKey]*transform.Chain{}, maxFires: maxFires}

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
		if err := r.advanceTo(s.TS, false); err != nil {
			return nil, 0, err
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

	horizon := samples[len(samples)-1].TS
	if now := r.clk.Now(); now.After(horizon) {
		horizon = now
	}

	if err := r.advanceTo(horizon.Add(longestPeriod(doc)), true); err != nil {
		return nil, 0, err
	}

	return r.out, r.errs, nil
}

// longestPeriod returns the longest duration parameter among the
// time-driven steps (heartbeat, min_interval, debounce) of doc, 0 when there
// is none. Parameters were checked by CheckDocument.
func longestPeriod(doc config.Document) time.Duration {
	var longest time.Duration

	for _, in := range doc.Inputs {
		for _, d := range in.Devices {
			for _, p := range d.Points {
				for _, t := range p.Transforms {
					if t.Name != "heartbeat" && t.Name != "min_interval" && t.Name != "debounce" {
						continue
					}

					var text string
					if json.Unmarshal(t.Params, &text) != nil {
						continue
					}

					if period, err := time.ParseDuration(text); err == nil {
						longest = max(longest, period)
					}
				}
			}
		}
	}

	return longest
}

// advanceTo moves the virtual clock to t, firing on the way every deadline
// before t in time order, and a deadline at t too when inclusive. It never
// moves the clock back.
func (r *replayer) advanceTo(t time.Time, inclusive bool) error {
	for {
		earliest, pending := r.earliest()
		if !pending || earliest.After(t) || (!inclusive && earliest.Equal(t)) {
			break
		}

		if r.fires >= r.maxFires {
			return errFlushBound
		}
		r.fires++

		if now := r.clk.Now(); earliest.After(now) {
			r.clk.Advance(earliest.Sub(now))
		}
		r.fireDue()
	}

	if now := r.clk.Now(); t.After(now) {
		r.clk.Advance(t.Sub(now))
	}

	return nil
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
