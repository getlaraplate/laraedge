package mqtt

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/getlaraplate/laraedge/internal/model"
)

// Events given to the observer of the deps by the command clients, for the
// tests.
const (
	traceProbeMessage  = "probe-message"
	traceBrowseMessage = "browse-message"
)

// commandQoS is the QoS of the probe and browse subscriptions (spec section 6).
const commandQoS = 0

// commandClient connects the client of a command (probe or browse) with the
// client id <client_id><suffix>, a clean session and the input's connection
// settings and password, and subscribes filters at QoS 0. It never touches
// the input's own client, lifecycle or topics file, so a running input does
// not see it. lost receives the loss of the connection; the caller must
// Disconnect the client. refused are the filters the broker refused, the
// others being subscribed. Errors are the client's fixed texts.
func (in *Input) commandClient(ctx context.Context, suffix string, filters []string, onMessage func(topic string, payload []byte, retained bool)) (c client, refused []string, err error) {
	password, err := in.password(ctx)
	if err != nil {
		return nil, nil, err
	}

	c, err = newPahoClientRetained(in.conn, in.clientID+suffix, password, true, onMessage)
	if err != nil {
		return nil, nil, err
	}

	if _, err := c.Connect(ctx); err != nil {
		c.Disconnect()

		return nil, nil, err
	}

	refused, err = c.Subscribe(ctx, filters, commandQoS)
	if err != nil {
		c.Disconnect()

		return nil, nil, err
	}

	return c, refused, nil
}

// PointRef names a point of an input: its device and signal.
type PointRef struct {
	Device string
	Signal string
}

// Probe subscribes to the topics of the points with the client id
// <client_id>-probe (clean session, QoS 0), decodes the messages like the
// running input does and keeps the first sample of every point. It returns
// as soon as every point has one, or, when ctx ends first, with the samples
// it has and no error (decision Q9); the samples are in configuration order.
// A connection failure, and a connection lost before the end, are errors.
// Retained messages count, so a point whose topic holds one is read at once.
func (in *Input) Probe(ctx context.Context) ([]model.Sample, error) {
	samples, _, err := in.ProbePoints(ctx)

	return samples, err
}

// ProbePoints is Probe that also says why a point has no good sample: kinds
// holds, for a point whose kept sample is bad, the read error kind of that
// sample (malformed_json, bad_timestamp, conversion) and, for a point without
// a sample, the reason of the last message of its topic that gave none
// (payload_too_large, NoValueAtPath), or subscribe_refused when the broker
// refused its topic. The points of a refused topic do not hold the probe:
// it returns once every other point has a sample.
func (in *Input) ProbePoints(ctx context.Context) (samples []model.Sample, kinds map[PointRef]string, err error) {
	keys := make(map[PointRef]struct{})
	for _, dev := range in.cfg.Devices {
		for _, p := range dev.Points {
			keys[PointRef{dev.Device, p.Signal}] = struct{}{}
		}
	}

	var (
		mu       sync.Mutex
		got      = make(map[PointRef]model.Sample, len(keys))
		why      = make(map[PointRef]string)
		need     = len(keys)
		finished bool
		done     = make(chan struct{})
	)

	// finish closes done once every point the probe waits for has a sample;
	// mu is held.
	finish := func() {
		if !finished && len(got) >= need {
			finished = true
			close(done)
		}
	}

	onMessage := func(topic string, payload []byte, _ bool) {
		defer in.traceEvent(traceProbeMessage)

		plan := in.plan[topic]
		if plan == nil {
			return
		}

		mu.Lock()
		defer mu.Unlock()

		if len(payload) > in.conn.MaxPayload {
			for _, pp := range plan.Points {
				if key := (PointRef{pp.Device, pp.Signal}); !hasSample(got, key) {
					why[key] = KindPayloadTooLarge
				}
			}

			return
		}

		decoded, _, pointKinds := decode(plan, payload, in.clk.Now())
		for i, pp := range plan.Points {
			key := PointRef{pp.Device, pp.Signal}
			if hasSample(got, key) {
				continue
			}
			if pointKinds[i] == NoValueAtPath {
				why[key] = NoValueAtPath

				continue
			}
			if s, ok := sampleOf(decoded, key); ok {
				got[key] = s
				delete(why, key)
				if pointKinds[i] != "" {
					why[key] = pointKinds[i]
				}
			}
		}
		finish()
	}

	c, refused, err := in.commandClient(ctx, "-probe", slices.Sorted(maps.Keys(in.plan)), onMessage)
	if err != nil {
		return nil, nil, fmt.Errorf("mqtt input %q: probe: %w", in.cfg.ID, err)
	}
	defer c.Disconnect()

	mu.Lock()
	for _, topic := range refused {
		for _, pp := range in.plan[topic].Points {
			key := PointRef{pp.Device, pp.Signal}
			why[key] = KindSubscribeRefused
			need--
		}
	}
	finish()
	mu.Unlock()

	var lostErr error
	select {
	case <-done:
	case <-ctx.Done():
	case <-c.Lost():
		lostErr = errConnectionLost
	}

	mu.Lock()
	defer mu.Unlock()

	if lostErr != nil && len(got) < need {
		return nil, nil, fmt.Errorf("mqtt input %q: probe: %w", in.cfg.ID, lostErr)
	}

	for _, dev := range in.cfg.Devices {
		for _, p := range dev.Points {
			if s, ok := got[PointRef{dev.Device, p.Signal}]; ok {
				samples = append(samples, s)
			}
		}
	}

	return samples, maps.Clone(why), nil
}

func hasSample(got map[PointRef]model.Sample, key PointRef) bool {
	_, ok := got[key]

	return ok
}

// sampleOf returns the sample of key among samples.
func sampleOf(samples []model.Sample, key PointRef) (model.Sample, bool) {
	for _, s := range samples {
		if s.Device == key.Device && s.Signal == key.Signal {
			return s, true
		}
	}

	return model.Sample{}, false
}
