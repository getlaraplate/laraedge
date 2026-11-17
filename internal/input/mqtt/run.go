package mqtt

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

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
	// KindSubscribeRefused is a point, or an availability topic, whose
	// subscription the broker refused (SUBACK 0x80), counted once per
	// connection.
	KindSubscribeRefused = "subscribe_refused"
)

// Thresholds of the duplicate client id warning: flapCount connections in a
// row lost less than flapWindow after they were established.
const (
	flapWindow = 5 * time.Second
	flapCount  = 3
)

// msgClientIDTakenOver is the warning logged when the connections keep
// dropping right after they are established, which is what a broker does to
// two clients sharing a client id: each connection takes the other's over.
const msgClientIDTakenOver = "mqtt: the connection keeps dropping right after connecting; another client may be using the same client_id"

// flapDetector counts the connections lost less than flapWindow after they
// were established. It belongs to the goroutine owning the client.
type flapDetector struct {
	streak int
	warned bool
}

// lost records a connection lost after it lasted lasted, and reports
// whether to warn now: at the flapCount-th short one in a row, and again
// only after a connection that lasted flapWindow or more reset the streak.
func (f *flapDetector) lost(lasted time.Duration) (warn bool) {
	if lasted >= flapWindow {
		f.streak, f.warned = 0, false

		return false
	}

	f.streak++
	if f.streak >= flapCount && !f.warned {
		f.warned = true

		return true
	}

	return false
}

// msgSubscribeRefused is the warning logged once per connection for every
// topic the broker refused to subscribe.
const msgSubscribeRefused = "mqtt: the broker refused the subscription of a topic; fix the broker ACL, then reload the configuration or restart the agent"

// Events given to the observer of the deps, for the tests.
const (
	traceServing = "serving"
	traceDown    = "down"
)

// run is the goroutine owning the client: it connects, serves the
// connection until it is lost, and connects again after the backoff, until
// ctx is done. The backoff starts again from its minimum after a connection
// whose subscriptions were acknowledged.
func (in *Input) run(ctx context.Context, r *router) {
	backoff := driverkit.NewBackoff(in.clk, in.deps.jitter)
	var flaps flapDetector

	for {
		subscribed, err := in.session(ctx, r, &flaps)
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
// subscribed reports whether the broker answered the subscriptions; the
// input is connected from that answer on. A topic the broker refused keeps
// the connection: it is logged, counted, left out of the subscription set
// so the next connection subscribes it again, and a device whose points are
// all on refused topics is not kept alive.
func (in *Input) session(ctx context.Context, r *router, flaps *flapDetector) (subscribed bool, err error) {
	password, err := in.password(ctx)
	if err != nil {
		return false, err
	}

	c, err := in.deps.newClient(in.conn, in.clientID, password, !in.conn.PersistentSession, r.message)
	if err != nil {
		return false, err
	}

	// A new connection starts the seq tracking of every Sparkplug node
	// again: a gap across an outage is expected (decision S10).
	r.reset()

	if _, err := c.Connect(ctx); err != nil {
		return false, err
	}
	defer c.Disconnect()
	established := in.clk.Now()

	// A connection that ends with an error while the input still runs is a
	// loss for the flap detector, whether the client reported it through
	// Lost or it failed a pending SUBSCRIBE or UNSUBSCRIBE first.
	defer func() {
		if err != nil && ctx.Err() == nil && flaps.lost(in.clk.Now().Sub(established)) {
			in.log.Warn(msgClientIDTakenOver, "input", in.cfg.ID, "client_id", in.clientID)
		}
	}()

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
	previous, err := in.unsubscribeStale(connCtx, c, topics)
	if err != nil {
		return false, lostOr(lost, err)
	}
	refused, err := c.Subscribe(connCtx, topics, in.conn.QoS)
	if err != nil {
		return false, lostOr(lost, err)
	}

	in.health.SetConnected(true)
	defer in.health.SetConnected(false)

	in.refuse(refused, r.tracker)
	if err := writeTopics(in.opts.DataDir, in.cfg.ID, subscriptionSet(topics, refused, previous)); err != nil {
		in.log.Debug("mqtt: the subscription set cannot be written", "input", in.cfg.ID, "error", err.Error())
	}

	return true, in.keepAlive(connCtx, lost, r, c)
}

// refuse handles the topics the broker refused on this connection: each is
// logged at warn and counted subscribe_refused, once per point on it or once
// for an availability topic, and the devices whose points are all on refused
// topics are no longer kept alive (an empty refused list keeps them all).
func (in *Input) refuse(refused []string, tracker *Tracker) {
	for _, topic := range refused {
		in.log.Warn(msgSubscribeRefused, "input", in.cfg.ID, "topic", topic)

		count := 1
		if plan := in.plan[topic]; plan != nil {
			count = len(plan.Points)
		}
		for range count {
			in.health.CountError(KindSubscribeRefused)
		}
	}

	tracker.SetRefused(in.refusedDevices(refused))
}

// refusedDevices returns the devices of the input whose points are all on
// topics of refused.
func (in *Input) refusedDevices(refused []string) []string {
	if len(refused) == 0 {
		return nil
	}

	accepted := make(map[string]bool, len(in.devices))
	for topic, plan := range in.plan {
		if slices.Contains(refused, topic) {
			continue
		}
		for _, pp := range plan.Points {
			accepted[pp.Device] = true
		}
	}

	var devices []string
	for _, device := range in.devices {
		if !accepted[device] {
			devices = append(devices, device)
		}
	}

	return devices
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
// that are not in topics, and returns that set. A missing or unreadable set
// means nothing to unsubscribe (decision Q7).
func (in *Input) unsubscribeStale(ctx context.Context, c client, topics []string) (previous []string, err error) {
	previous, err = readTopics(in.opts.DataDir, in.cfg.ID)
	if err != nil {
		in.log.Debug("mqtt: no previous subscription set, nothing to unsubscribe", "input", in.cfg.ID, "error", err.Error())

		return nil, nil
	}

	return previous, c.Unsubscribe(ctx, staleTopics(previous, topics))
}

// subscriptionSet is the subscription set to write after a SUBACK: the
// topics the broker accepted, and the refused ones the previous set already
// held. A broker usually keeps in the session a subscription it granted
// earlier even when it refuses it again, so such a topic stays recorded and
// is still unsubscribed once the document drops it. A topic refused at its
// first subscription is left out.
func subscriptionSet(topics, refused, previous []string) []string {
	return slices.DeleteFunc(slices.Clone(topics), func(topic string) bool {
		return slices.Contains(refused, topic) && !slices.Contains(previous, topic)
	})
}

// keepAlive renews every device not held dead by its availability topic now,
// then every half death tolerance on the clock and once more when the
// connection is lost (decision Q2), until the connection is lost
// (errConnectionLost) or the input stops (ctx.Err()). Meanwhile it publishes
// on c the Sparkplug rebirth requests the router decided (decision S11).
func (in *Input) keepAlive(ctx context.Context, lost <-chan struct{}, r *router, c client) error {
	r.tracker.KeepAlive()

	every := in.keepAliveEvery()
	timer := in.clk.NewTimer(every)
	in.traceEvent(traceServing)

	for {
		select {
		case <-ctx.Done():
			timer.Stop()

			// A lost connection renews the devices once more: the last
			// renewal may be up to half a tolerance old, and only an outage
			// beyond the tolerance kills them (decision Q2). Close does not.
			select {
			case <-lost:
				r.tracker.KeepAlive()

				return errConnectionLost
			default:
				return ctx.Err()
			}
		case <-r.rebirthsDue:
			in.publishRebirths(ctx, c, r.takeRebirths())
		case <-timer.C():
			r.tracker.KeepAlive()
			timer = in.clk.NewTimer(every)
		}
	}
}

// publishRebirths publishes a rebirth request to every node of nodes: an
// NCMD with the metric Node Control/Rebirth = true, at QoS 0 and not
// retained (decision S11). A failed publish is logged at debug only; the
// session asks again at the next trigger after the interval.
func (in *Input) publishRebirths(ctx context.Context, c client, nodes []SparkplugTarget) {
	for _, node := range nodes {
		topic := SparkplugNamespace + "/" + node.Group + "/" + spNCMD + "/" + node.Node
		if err := c.Publish(ctx, topic, encodeRebirth(in.clk.Now()), 0); err != nil {
			in.log.Debug("mqtt: the Sparkplug rebirth request cannot be published", "input", in.cfg.ID, "node", node.Key(), "error", err.Error())
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
//
// Under sparkplug_b it owns the session, used under mu only. The rebirth
// requests it decides are queued in rebirths and signalled on rebirthsDue;
// the goroutine owning the client publishes them (keepAlive), so the
// delivery goroutine never waits on the network and the publish happens
// outside mu, on the current client.
type router struct {
	in      *Input
	emit    func(model.Record)
	tracker *Tracker
	// rebirthsDue holds one signal while rebirths is not empty.
	rebirthsDue chan struct{}

	mu      sync.Mutex
	stopped bool
	// session is the Sparkplug B session; nil under json.
	session *sparkplugSession
	// rebirths are the nodes due a rebirth request not yet published.
	rebirths []SparkplugTarget
}

// newRouter returns the router of in, which emits to emit and applies life
// and death through tracker.
func newRouter(in *Input, emit func(model.Record), tracker *Tracker) *router {
	r := &router{in: in, emit: emit, tracker: tracker, rebirthsDue: make(chan struct{}, 1)}
	if in.sparkplug != nil {
		r.session = newSparkplugSession(in.sparkplug, in.conn.Rebirth, in.clk)
	}

	return r
}

// message routes one message: the size check first, then the availability
// topics, then the topic plan, else the message is unmapped (spec sections
// 4 and 5). Every sample renews its device before it is emitted, so a birth
// comes before the sample. It reports whether the message was handled, which
// is every message but those arriving after stop: the client acknowledges
// only a handled one, so the broker redelivers the others from the
// persistent session.
func (r *router) message(topic string, payload []byte) (handled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return false
	}

	in := r.in
	if len(payload) > in.conn.MaxPayload {
		in.health.CountError(KindPayloadTooLarge)

		return true
	}

	if r.session != nil {
		r.sparkplug(topic, payload)

		return true
	}

	if availability, kind := r.tracker.Message(topic, payload); availability {
		if kind != "" {
			in.health.CountError(kind)
		}

		return true
	}

	plan := in.plan[topic]
	if plan == nil {
		in.health.CountError(KindUnmapped)

		return true
	}

	samples, kinds := Decode(plan, payload, in.clk.Now())
	for _, kind := range kinds {
		in.health.CountError(kind)
	}
	for _, s := range samples {
		r.tracker.Sample(s.Device)
		r.emit(model.SampleRecord(s))
	}

	return true
}

// sparkplug routes one message under sparkplug_b (decision S12), after the
// size check: NCMD, DCMD and STATE are dropped uncounted, a topic outside the
// Sparkplug shape or of a node not in the plan is unmapped. The births and
// deaths go to the tracker before the samples, so a birth comes before the
// sample of the birth, and a sample of a device held dead is dropped
// uncounted (decision S3). The rebirth requests due are queued for the
// goroutine owning the client. Called with mu held.
func (r *router) sparkplug(topic string, payload []byte) {
	in := r.in
	now := in.clk.Now()

	res := r.session.Handle(topic, payload, now)
	if res.Ignored {
		return
	}
	if !res.Mapped {
		in.health.CountError(KindUnmapped)

		return
	}

	for _, kind := range res.Kinds {
		in.health.CountError(kind)
	}
	for _, device := range res.Offline {
		r.tracker.Offline(device)
	}
	for _, device := range res.Online {
		r.tracker.Online(device)
	}
	for _, s := range res.Samples {
		if r.tracker.Seen(s.Device) {
			r.emit(model.SampleRecord(s))
		}
	}

	if due := r.session.RebirthNodes(now); len(due) > 0 {
		r.rebirths = append(r.rebirths, due...)
		select {
		case r.rebirthsDue <- struct{}{}:
		default:
		}
	}
}

// takeRebirths returns the nodes due a rebirth request and empties the
// queue.
func (r *router) takeRebirths() []SparkplugTarget {
	r.mu.Lock()
	defer r.mu.Unlock()

	due := r.rebirths
	r.rebirths = nil

	return due
}

// reset ends the seq tracking of the Sparkplug session at a new connection;
// a no-op under json.
func (r *router) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.session != nil {
		r.session.Reset()
	}
}

// stop drops every later message, unacknowledged; a message being routed
// finishes first.
func (r *router) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stopped = true
}
