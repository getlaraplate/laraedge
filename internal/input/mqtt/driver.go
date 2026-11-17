package mqtt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Options are what the MQTT inputs take from the agent: the data directory
// holding the subscription set (decision Q7), the agent id forming the
// default client id, the resolver of the password reference and the logger.
type Options struct {
	DataDir  string
	AgentID  string
	Resolver SecretResolver
	// Logger receives the debug messages of the driver; nil discards.
	Logger *slog.Logger
}

// SecretResolver resolves a secret:// reference; *secrets.Resolver is one.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// clientIDPrefix starts the default client id,
// laraplate-edge-<agent_id>-<input_id> (spec section 3).
const clientIDPrefix = "laraplate-edge-"

var (
	errNoResolver = errors.New("mqtt: no secret resolver for the password")
	errPassword   = errors.New("mqtt: the password reference cannot be resolved")
)

// Factory returns the factory of MQTT inputs. It validates the connection
// and every point and never touches the disk or the network (decision Q1,
// core R47).
func Factory(opts Options) edgeinput.Factory {
	return func(cfg config.Input, clk clock.Clock) (edgeinput.Input, error) {
		in, err := New(cfg, clk, opts)
		if err != nil {
			return nil, err
		}

		return in, nil
	}
}

// Input is one MQTT input: one broker, one client at a time, owned by one
// goroutine that connects with backoff, unsubscribes the topics the input no
// longer uses, subscribes its own, keeps its devices alive while connected
// (decision Q2) and connects again after any failure. The messages are
// routed on the client's delivery goroutine. It implements
// edgeinput.HealthReporter.
type Input struct {
	cfg  config.Input
	clk  clock.Clock
	opts Options
	log  *slog.Logger
	conn Connection

	// plan is the decoding plan of every subscribed topic.
	plan      map[string]*TopicPlan
	devices   []string
	clientID  string
	tolerance time.Duration

	// deps are fixed by the constructor and never change afterwards.
	deps deps

	health driverkit.Health

	// mu guards the fields below.
	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	router  *router

	wg sync.WaitGroup
}

var _ edgeinput.HealthReporter = (*Input)(nil)

// deps are what an input takes besides its configuration: the draw of the
// backoff jitter, in [0, 1), the constructor of the input's client, and an
// observer told the steps of the connection and of the commands (the trace
// events), nil for none. New gives the production ones; the tests build
// their inputs through newInput.
type deps struct {
	jitter  func() float64
	observe func(event string)
	// newClient builds the client of a connection; nil means newPahoClient.
	newClient func(conn Connection, clientID, password string, cleanSession bool, onMessage func(topic string, payload []byte) (handled bool)) (client, error)
}

// New returns the MQTT input of cfg. The errors are fixed texts naming the
// input and the field path or the position of the point
// (devices.<d>.points.<p>), never a value or a connection setting.
func New(cfg config.Input, clk clock.Clock, opts Options) (*Input, error) {
	return newInput(cfg, clk, opts, deps{jitter: rand.Float64})
}

// newInput is New with the given deps.
func newInput(cfg config.Input, clk clock.Clock, opts Options, d deps) (*Input, error) {
	conn, err := ParseConnection(cfg.Connection, cfg)
	if err != nil {
		return nil, fmt.Errorf("mqtt input %q: %w", cfg.ID, err)
	}

	for d, dev := range cfg.Devices {
		for i, p := range dev.Points {
			if err := checkPoint(p); err != nil {
				return nil, fmt.Errorf("mqtt input %q: devices.%d.points.%d: %w", cfg.ID, d, i, err)
			}
		}
	}

	plan, err := Plan(cfg, conn)
	if err != nil {
		return nil, fmt.Errorf("mqtt input %q: %w", cfg.ID, err)
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	in := &Input{
		cfg:       cfg,
		clk:       clk,
		opts:      opts,
		log:       log,
		conn:      conn,
		plan:      plan,
		clientID:  clientID(conn, opts.AgentID, cfg.ID),
		tolerance: cfg.DeathTolerance,
		deps:      d,
	}
	if in.deps.jitter == nil {
		in.deps.jitter = rand.Float64
	}
	if in.deps.newClient == nil {
		in.deps.newClient = newPahoClient
	}
	if in.tolerance <= 0 {
		in.tolerance = config.DefaultDeathTolerance
	}
	for _, dev := range cfg.Devices {
		if !slices.Contains(in.devices, dev.Device) {
			in.devices = append(in.devices, dev.Device)
		}
	}

	return in, nil
}

// clientID is the effective client id of input id: the document's
// client_id, else laraplate-edge-<agent_id>-<input_id>.
func clientID(conn Connection, agentID, id string) string {
	if conn.ClientID != "" {
		return conn.ClientID
	}

	return clientIDPrefix + agentID + "-" + id
}

// CheckClientIDs refuses two mqtt inputs of doc that would connect to the
// same broker (scheme, host and port, the default port filled in, the host
// in any letter case) with the same effective client id: the broker would
// drop one connection whenever the other connects. agentID forms the default
// ids. An input whose connection does not parse is left to its factory. The
// messages name the input ids and their positions, never a client id or a
// broker.
func CheckClientIDs(doc config.Document, agentID string) []string {
	type identity struct{ scheme, host, clientID string }

	first := map[identity]int{}
	var msgs []string
	for i, cfg := range doc.Inputs {
		if cfg.Driver != config.DriverMQTT {
			continue
		}
		conn, err := ParseConnection(cfg.Connection, cfg)
		if err != nil {
			continue
		}

		key := identity{conn.Broker.Scheme, strings.ToLower(conn.Broker.Host), clientID(conn, agentID, cfg.ID)}
		if j, dup := first[key]; dup {
			msgs = append(msgs, fmt.Sprintf("inputs.%d.connection.client_id: input %q has the same broker and client id as input %q (inputs.%d); every mqtt input needs its own client id on a broker", i, cfg.ID, doc.Inputs[j].ID, j))

			continue
		}
		first[key] = i
	}

	return msgs
}

// checkPoint validates a point and its address.
func checkPoint(p config.Point) error {
	if err := CheckPoint(p); err != nil {
		return err
	}

	_, err := ParseAddress(p.Address)

	return err
}

// Start reads the TLS files, a local resource whose failure fails Start,
// starts the device lifecycle and the goroutine owning the client, and
// returns: the broker is connected in the background, so an unreachable
// broker is death after the death tolerance, never a failing Start (core
// R47). The goroutines stop when ctx is done or on Close.
func (in *Input) Start(ctx context.Context, emit func(model.Record)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mqtt input %q: start: %w", in.cfg.ID, err)
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	switch {
	case in.closed:
		return fmt.Errorf("mqtt input %q: start after close", in.cfg.ID)
	case in.started:
		return fmt.Errorf("mqtt input %q: already started", in.cfg.ID)
	}

	if in.conn.Broker != nil && in.conn.Broker.Scheme == SchemeTLS {
		if _, err := tlsConfig(in.conn.TLS); err != nil {
			return fmt.Errorf("mqtt input %q: start: %w", in.cfg.ID, err)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	in.cancel = cancel
	in.started = true

	life := edgeinput.NewLifecycle(in.cfg, in.clk, emit)
	in.router = &router{
		in:      in,
		emit:    emit,
		tracker: NewTracker(life, in.conn.Availability, in.devices),
	}

	in.wg.Go(func() { life.Run(runCtx) })
	in.wg.Go(func() { in.run(runCtx, in.router) })

	return nil
}

// Close stops the input: the client is disconnected without unsubscribing,
// so a persistent session keeps the messages published meanwhile (spec
// section 5), and the goroutines are waited for. A connection attempt in
// flight is abandoned and closed by the library in the background; a
// message it may still deliver is dropped without being acknowledged, so a
// QoS 1 one stays in the persistent session and the broker sends it again to
// the next connection. After Close returns emit is never called. It is safe
// on an input never started and when called twice.
func (in *Input) Close() error {
	in.mu.Lock()
	in.closed = true
	cancel := in.cancel
	r := in.router
	in.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	in.wg.Wait()

	if r != nil {
		r.stop()
	}

	return nil
}

// Health returns the connection state and the read errors of the input.
func (in *Input) Health() edgeinput.Health {
	return in.health.Snapshot()
}

// topics returns the subscription set of the input: the topics of the plan
// and the availability topics, sorted and without duplicates.
func (in *Input) topics(tracker *Tracker) []string {
	topics := slices.Collect(maps.Keys(in.plan))
	topics = append(topics, tracker.Topics()...)
	slices.Sort(topics)

	return slices.Compact(topics)
}

// keepAliveEvery is how often the devices are renewed while connected:
// half the death tolerance (decision Q2).
func (in *Input) keepAliveEvery() time.Duration {
	return max(in.tolerance/2, time.Millisecond)
}

// traceEvent tells the observer of the deps, if any, of event.
func (in *Input) traceEvent(event string) {
	if in.deps.observe != nil {
		in.deps.observe(event)
	}
}
