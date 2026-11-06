package mqtt

import (
	"context"
	"fmt"
	"sync"

	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Read error kinds the input decides itself, before the decoder (spec
// section 4).
const (
	// KindPayloadTooLarge is a message above max_payload, dropped unread.
	KindPayloadTooLarge = "payload_too_large"
	// KindUnmapped is a message on a topic no point subscribes to.
	KindUnmapped = "unmapped"
)

// Events given to Input.trace, for the tests.
const (
	traceServing = "serving"
	traceDown    = "down"
)

// run is the goroutine owning the client: it connects, serves the
// connection until it is lost, and connects again after the backoff, until
// ctx is done. The backoff starts again from its minimum after a connection
// whose subscriptions were acknowledged.
func (in *Input) run(ctx context.Context, r *router) {
	backoff := driverkit.NewBackoff(in.clk, in.jitter)

	for {
		subscribed, err := in.session(ctx, r)
		if subscribed {
			backoff.Reset()
		}
		if ctx.Err() != nil {
			return
		}

		in.health.CountError(edgeinput.ReadErrorConnection)
		in.log.Debug("mqtt: no connection, retrying after the backoff", "input", in.cfg.ID, "error", err.Error())
		in.traceEvent(traceDown)

		if backoff.Wait(ctx) != nil {
			return
		}
	}
}

// session runs one connection with a new client (a client whose connect was
// abandoned stays busy in the background): it resolves the password,
// connects, unsubscribes the topics the input no longer uses, subscribes its
// own, writes the subscription set and keeps the devices alive until the
// connection is lost or ctx is done. It disconnects without unsubscribing.
// subscribed reports whether the broker acknowledged the subscriptions.
func (in *Input) session(ctx context.Context, r *router) (subscribed bool, err error) {
	password, err := in.password(ctx)
	if err != nil {
		return false, err
	}

	c, err := newPahoClient(in.conn, in.clientID, password, !in.conn.PersistentSession, r.message)
	if err != nil {
		return false, err
	}

	if _, err := c.Connect(ctx); err != nil {
		return false, err
	}
	defer c.Disconnect()

	in.health.SetConnected(true)
	defer in.health.SetConnected(false)

	connCtx, cancel := context.WithCancel(ctx)
	lost := make(chan struct{})
	var watch sync.WaitGroup
	watch.Go(func() {
		select {
		case <-c.Lost():
			close(lost)
			cancel()
		case <-connCtx.Done():
		}
	})
	defer func() {
		cancel()
		watch.Wait()
	}()

	topics := in.topics(r.tracker)
	if err := in.unsubscribeStale(connCtx, c, topics); err != nil {
		return false, lostOr(lost, err)
	}
	if err := c.Subscribe(connCtx, topics, in.conn.QoS); err != nil {
		return false, lostOr(lost, err)
	}
	if err := writeTopics(in.opts.DataDir, in.cfg.ID, topics); err != nil {
		in.log.Debug("mqtt: the subscription set cannot be written", "input", in.cfg.ID, "error", err.Error())
	}

	return true, in.keepAlive(connCtx, lost, r.tracker)
}

// password resolves the password reference of the connection, at every
// connection so a rotated secret is picked up; "" without auth.
func (in *Input) password(ctx context.Context) (string, error) {
	if in.conn.Auth == nil {
		return "", nil
	}
	if in.opts.Resolver == nil {
		return "", errNoResolver
	}

	value, err := in.opts.Resolver.Resolve(ctx, in.conn.Auth.Password)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		return "", fmt.Errorf("%w: %w", errPassword, err)
	}

	return value, nil
}

// unsubscribeStale unsubscribes the topics of the last subscription set
// that are not in topics. A missing or unreadable set means nothing to
// unsubscribe (decision Q7).
func (in *Input) unsubscribeStale(ctx context.Context, c client, topics []string) error {
	old, err := readTopics(in.opts.DataDir, in.cfg.ID)
	if err != nil {
		in.log.Debug("mqtt: no previous subscription set, nothing to unsubscribe", "input", in.cfg.ID, "error", err.Error())

		return nil
	}

	return c.Unsubscribe(ctx, staleTopics(old, topics))
}

// keepAlive renews every device not held dead by its availability topic now,
// then every half death tolerance on the clock and once more when the
// connection is lost (decision Q2), until the connection is lost
// (errConnectionLost) or the input stops (ctx.Err()).
func (in *Input) keepAlive(ctx context.Context, lost <-chan struct{}, tracker *Tracker) error {
	tracker.KeepAlive()

	every := in.keepAliveEvery()
	first := true

	for {
		timer := in.clk.NewTimer(every)
		if first {
			first = false
			in.traceEvent(traceServing)
		}

		select {
		case <-ctx.Done():
			timer.Stop()

			// A lost connection renews the devices once more: the last
			// renewal may be up to half a tolerance old, and only an outage
			// beyond the tolerance kills them (decision Q2). Close does not.
			select {
			case <-lost:
				tracker.KeepAlive()

				return errConnectionLost
			default:
				return ctx.Err()
			}
		case <-timer.C():
			tracker.KeepAlive()
		}
	}
}

// lostOr returns errConnectionLost when the connection was lost, err
// otherwise: a subscribe cut short by the loss reports the loss.
func lostOr(lost <-chan struct{}, err error) error {
	select {
	case <-lost:
		return errConnectionLost
	default:
		return err
	}
}

// router routes the messages of the input's clients. It is called on the
// client's delivery goroutine, one message at a time; stop makes it drop
// every later message, so nothing is emitted after Close.
type router struct {
	in      *Input
	emit    func(model.Record)
	tracker *Tracker

	mu      sync.Mutex
	stopped bool
}

// message routes one message: the size check first, then the availability
// topics, then the topic plan, else the message is unmapped (spec sections
// 4 and 5). Every sample renews its device before it is emitted, so a birth
// comes before the sample.
func (r *router) message(topic string, payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return
	}

	in := r.in
	if len(payload) > in.conn.MaxPayload {
		in.health.CountError(KindPayloadTooLarge)

		return
	}

	if handled, kind := r.tracker.Message(topic, payload); handled {
		if kind != "" {
			in.health.CountError(kind)
		}

		return
	}

	plan := in.plan[topic]
	if plan == nil {
		in.health.CountError(KindUnmapped)

		return
	}

	samples, kinds := Decode(plan, payload, in.clk.Now())
	for _, kind := range kinds {
		in.health.CountError(kind)
	}
	for _, s := range samples {
		r.tracker.Sample(s.Device)
		r.emit(model.SampleRecord(s))
	}
}

// stop drops every later message; a message being routed finishes first.
func (r *router) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stopped = true
}
