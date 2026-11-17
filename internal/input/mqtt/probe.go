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

type pointKey struct{ device, signal string }

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

// Probe subscribes to the topics of the points with the client id
// <client_id>-probe (clean session, QoS 0), decodes the messages like the
// running input does and keeps the first sample of every point. It returns
// as soon as every point has one, or, when ctx ends first, with the samples
// it has and no error (decision Q9); the samples are in configuration order.
// A connection failure, and a connection lost before the end, are errors.
// Retained messages count, so a point whose topic holds one is read at once.
func (in *Input) Probe(ctx context.Context) ([]model.Sample, error) {
	keys := make(map[pointKey]struct{})
	for _, dev := range in.cfg.Devices {
		for _, p := range dev.Points {
			keys[pointKey{dev.Device, p.Signal}] = struct{}{}
		}
	}
	need := len(keys)

	var (
		mu   sync.Mutex
		got  = make(map[pointKey]model.Sample, need)
		done = make(chan struct{})
	)

	onMessage := func(topic string, payload []byte, _ bool) {
		plan := in.plan[topic]
		if plan == nil || len(payload) > in.conn.MaxPayload {
			return
		}

		samples, _ := Decode(plan, payload, in.clk.Now())

		mu.Lock()
		before := len(got)
		for _, s := range samples {
			key := pointKey{s.Device, s.Signal}
			if _, ok := got[key]; !ok {
				got[key] = s
			}
		}
		if before < need && len(got) >= need {
			close(done)
		}
		mu.Unlock()

		in.traceEvent(traceProbeMessage)
	}

	c, _, err := in.commandClient(ctx, "-probe", slices.Sorted(maps.Keys(in.plan)), onMessage)
	if err != nil {
		return nil, fmt.Errorf("mqtt input %q: probe: %w", in.cfg.ID, err)
	}
	defer c.Disconnect()

	var lostErr error
	select {
	case <-done:
	case <-ctx.Done():
	case <-c.Lost():
		lostErr = errConnectionLost
	}

	mu.Lock()
	defer mu.Unlock()

	var samples []model.Sample
	for _, dev := range in.cfg.Devices {
		for _, p := range dev.Points {
			if s, ok := got[pointKey{dev.Device, p.Signal}]; ok {
				samples = append(samples, s)
			}
		}
	}

	if lostErr != nil && len(got) < need {
		return nil, fmt.Errorf("mqtt input %q: probe: %w", in.cfg.ID, lostErr)
	}

	return samples, nil
}
