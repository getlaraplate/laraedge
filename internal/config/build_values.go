// This file holds the builder methods that read single values: reads, subscriptions, deadbands, durations and sizes.

package config

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

func (b *builder) read(p path, raw json.RawMessage) Read {
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		r := Read{Mode: mode}
		if mode == ReadSubscribe {
			r.Subscribe = &Subscribe{Sampling: SamplingDefault}
		}
		return r
	}
	var long struct {
		Poll      string          `json:"poll"`
		Subscribe json.RawMessage `json:"subscribe"`
	}
	_ = json.Unmarshal(raw, &long)
	if long.Subscribe != nil {
		return Read{Mode: ReadSubscribe, Subscribe: b.subscribe(p.key("subscribe"), long.Subscribe)}
	}
	return Read{Mode: ReadPoll, Interval: b.duration(p.key("poll"), long.Poll)}
}

// subscribe converts the settings of the long subscribe form. The schema
// accepted their shape; the percentage cap is checked here.
func (b *builder) subscribe(p path, raw json.RawMessage) *Subscribe {
	var wire struct {
		Sampling *string         `json:"sampling"`
		Queue    uint32          `json:"queue"`
		Deadband json.RawMessage `json:"deadband"`
	}
	_ = json.Unmarshal(raw, &wire)

	sub := &Subscribe{Sampling: SamplingDefault, Queue: wire.Queue}
	if wire.Sampling != nil {
		sub.Sampling = b.duration(p.key("sampling"), *wire.Sampling)
	}
	if len(wire.Deadband) != 0 {
		sub.Deadband = b.deadband(p.key("deadband"), wire.Deadband)
	}
	return sub
}

// deadband returns the text of a deadband and refuses a percentage above 100.
// A text the schema refused is left to the schema's message. A JSON number is
// written in plain decimal, never with an exponent: YAML 0.0000001 reaches
// here as 1e-07, which drivers read as decimal text only.
func (b *builder) deadband(p path, raw json.RawMessage) string {
	var text string
	isNumber := false
	if err := json.Unmarshal(raw, &text); err != nil {
		text = strings.TrimSpace(string(raw))
		isNumber = true
	}
	number, percent := strings.CutSuffix(text, "%")
	value, err := strconv.ParseFloat(number, 64)
	switch {
	case errors.Is(err, strconv.ErrRange):
		b.add(p, "is too large")
	case err == nil && percent && value > 100:
		b.add(p, "must be at most 100%")
	case err == nil && isNumber:
		text = strconv.FormatFloat(value, 'f', -1, 64)
	}
	return text
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
