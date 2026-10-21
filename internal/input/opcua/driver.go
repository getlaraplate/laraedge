package opcua

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopcua/opcua/ua"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Options are what the OPC UA inputs take from the agent (O13): the data
// directory holding the PKI, the agent id forming the application URI, the
// resolver of the password reference and the logger.
type Options struct {
	DataDir  string
	AgentID  string
	Resolver SecretResolver
	// Logger receives the warnings of the driver; nil discards.
	Logger *slog.Logger
}

// SecretResolver resolves a secret:// reference; *secrets.Resolver is one.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

var (
	errNoResolver = errors.New("no secret resolver")
	errNoDataDir  = errors.New("data_dir unknown (give the bootstrap): the agent certificate lives there")
	errLost       = errors.New("opcua: the session was lost")
)

// Factory returns the factory of OPC UA inputs. It validates the connection
// and every point and never touches the disk or the network (decision M1 of
// the Modbus plan, core R47).
func Factory(opts Options) input.Factory {
	return func(cfg config.Input, clk clock.Clock) (input.Input, error) {
		in, err := New(cfg, clk, opts)
		if err != nil {
			return nil, err
		}

		return in, nil
	}
}

// inputPoint is a point of the input with its parsed address.
type inputPoint struct {
	device string
	cfg    config.Point
	node   Node
}

// pollGroup is the points read together at one interval (O10), as indexes
// into Input.points.
type pollGroup struct {
	interval time.Duration
	points   []int
}

// dialFunc opens a session; dial in production.
type dialFunc func(ctx context.Context, conn Connection, deps dialDeps) (session, error)

// Input is one OPC UA input: one server, one session at a time, owned by one
// goroutine that connects with backoff, subscribes the subscribe points,
// reads the poll groups at their intervals and the watchdog (O3), and
// reconnects after any failure. It implements input.HealthReporter.
type Input struct {
	cfg  config.Input
	clk  clock.Clock
	opts Options
	log  *slog.Logger
	conn Connection

	// points are in configuration order; a point's index is the client
	// handle of its monitored item.
	points     []inputPoint
	devices    []string
	subscribed []int
	groups     []pollGroup
	tolerance  time.Duration

	// jitter draws the backoff jitter, in [0, 1). timing, newClient and
	// dial replace the production values in tests.
	jitter    func() float64
	timing    *timings
	newClient clientFactory
	dial      dialFunc

	health driverkit.Health
	// rejected deduplicates the untrusted certificate warning for the
	// input's lifetime (O1).
	rejected rejectedSet

	// mu guards the fields below.
	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc

	wg sync.WaitGroup
}

var _ input.HealthReporter = (*Input)(nil)

// New returns the OPC UA input of cfg. The errors are fixed texts naming the
// input and the position of the point (devices.<d>.points.<p>), never a
// value or a connection setting.
func New(cfg config.Input, clk clock.Clock, opts Options) (*Input, error) {
	conn, err := ParseConnection(cfg.Connection)
	if err != nil {
		return nil, fmt.Errorf("opcua input %q: %w", cfg.ID, err)
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
		tolerance: cfg.DeathTolerance,
		jitter:    rand.Float64,
		dial:      dial,
	}
	if in.tolerance <= 0 {
		in.tolerance = config.DefaultDeathTolerance
	}

	for d, dev := range cfg.Devices {
		if !slices.Contains(in.devices, dev.Device) {
			in.devices = append(in.devices, dev.Device)
		}

		for i, p := range dev.Points {
			node, err := checkPoint(p)
			if err != nil {
				return nil, fmt.Errorf("opcua input %q: devices.%d.points.%d: %w", cfg.ID, d, i, err)
			}

			in.addPoint(inputPoint{device: dev.Device, cfg: p, node: node})
		}
	}

	return in, nil
}

// checkPoint validates a point and parses its address.
func checkPoint(p config.Point) (Node, error) {
	if err := CheckPoint(p); err != nil {
		return Node{}, err
	}

	if p.Read.Mode == config.ReadPoll && p.Read.Interval <= 0 {
		return Node{}, errors.New("read.poll: must be greater than zero")
	}

	return ParseNode(p.Address)
}

// addPoint adds p to the subscribed points or to the poll group of its
// interval.
func (in *Input) addPoint(p inputPoint) {
	index := len(in.points)
	in.points = append(in.points, p)

	if p.cfg.Read.Mode == config.ReadSubscribe {
		in.subscribed = append(in.subscribed, index)

		return
	}

	g := slices.IndexFunc(in.groups, func(g pollGroup) bool { return g.interval == p.cfg.Read.Interval })
	if g < 0 {
		in.groups = append(in.groups, pollGroup{interval: p.cfg.Read.Interval})
		g = len(in.groups) - 1
	}
	in.groups[g].points = append(in.groups[g].points, index)
}

// Start loads or creates the agent's own certificate, a local resource whose
// failure fails Start (O2), warns when the connection is unprotected (O12),
// starts the device lifecycle and the goroutine owning the session, and
// returns: the server is connected in the background, so an unreachable or
// untrusted server is death after the death tolerance, never a failing
// Start (core R47). The goroutines stop when ctx is done or on Close.
func (in *Input) Start(ctx context.Context, emit func(model.Record)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("opcua input %q: start: %w", in.cfg.ID, err)
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	switch {
	case in.closed:
		return fmt.Errorf("opcua input %q: start after close", in.cfg.ID)
	case in.started:
		return fmt.Errorf("opcua input %q: already started", in.cfg.ID)
	}

	own, err := in.ownCertificate()
	if err != nil {
		return fmt.Errorf("opcua input %q: start: %w", in.cfg.ID, err)
	}

	if in.conn.SecurityPolicy == PolicyNone {
		in.log.Warn(fmt.Sprintf("input %s: OPC UA connection is unprotected (security_policy None)", in.cfg.ID), "input", in.cfg.ID)
	}

	runCtx, cancel := context.WithCancel(ctx)
	in.cancel = cancel
	in.started = true

	life := input.NewLifecycle(in.cfg, in.clk, emit)
	in.wg.Go(func() { life.Run(runCtx) })
	in.wg.Go(func() { in.run(runCtx, own, emit, life) })

	return nil
}

// Close stops the input and waits for its goroutines. A request in flight
// ends within the request timeout and the session's close within its close
// timeout (O3). A connection attempt is not waited for (Modbus R10): it ends
// on its own and the session it may open is closed unused. It is safe on an
// input never started and when called twice.
func (in *Input) Close() error {
	in.mu.Lock()
	in.closed = true
	cancel := in.cancel
	in.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	in.wg.Wait()

	return nil
}

// Health returns the connection state and the read errors of the input.
func (in *Input) Health() input.Health {
	return in.health.Snapshot()
}

// Probe connects once (the trust check included: an untrusted server is an
// error wrapping ErrUntrusted and naming its thumbprint), reads every point
// once with the Read service, subscribe points included, closes the session
// and returns one sample per point, in configuration order. It starts no
// loop and emits nothing.
func (in *Input) Probe(ctx context.Context) ([]model.Sample, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("opcua input %q: probe: %w", in.cfg.ID, err)
	}

	own, err := in.ownCertificate()
	if err != nil {
		return nil, fmt.Errorf("opcua input %q: probe: %w", in.cfg.ID, err)
	}

	s, err := in.dial(ctx, in.conn, in.dialDeps(own, nil))
	if err != nil {
		return nil, fmt.Errorf("opcua input %q: probe: %w", in.cfg.ID, err)
	}
	defer s.Close()

	var (
		ids     []*ua.NodeID
		indexes []int
		out     = make([]model.Sample, len(in.points))
		now     = in.clk.Now()
	)

	for i, p := range in.points {
		nodeID, err := s.Resolve(p.node)
		if err != nil {
			if errors.Is(err, errLibraryPanic) {
				return nil, fmt.Errorf("opcua input %q: probe: %w", in.cfg.ID, err)
			}

			out[i] = in.badSample(i, now)

			continue
		}

		ids = append(ids, nodeID)
		indexes = append(indexes, i)
	}

	values, err := s.Read(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("opcua input %q: probe: %w", in.cfg.ID, err)
	}

	for k, dv := range values {
		out[indexes[k]], _ = in.sample(indexes[k], dv)
	}

	return out, nil
}

// ownKeyPair is the agent's application certificate and key.
type ownKeyPair struct {
	cert []byte
	key  *rsa.PrivateKey
}

func (in *Input) ownCertificate() (ownKeyPair, error) {
	if in.opts.DataDir == "" {
		return ownKeyPair{}, errNoDataDir
	}

	cert, key, err := EnsureOwnCertificate(in.opts.DataDir, in.opts.AgentID, in.clk.Now())
	if err != nil {
		return ownKeyPair{}, err
	}

	return ownKeyPair{cert: cert, key: key}, nil
}

func (in *Input) timings() timings {
	if in.timing != nil {
		return *in.timing
	}

	return productionTimings
}

// dialDeps are the dependencies of one dial; onLost is bound to that dial.
func (in *Input) dialDeps(own ownKeyPair, onLost func()) dialDeps {
	deps := dialDeps{
		InputID:   in.cfg.ID,
		OwnCert:   own.cert,
		OwnKey:    own.key,
		Trust:     &TrustStore{Dir: PKIDir(in.opts.DataDir)},
		Rejected:  &in.rejected,
		OnLost:    onLost,
		Log:       in.log,
		timing:    in.timing,
		newClient: in.newClient,
	}
	if in.conn.User != "" {
		deps.Password = in.password
	}

	return deps
}

// password resolves the password reference, at every connect (O11).
func (in *Input) password(ctx context.Context) (string, error) {
	if in.opts.Resolver == nil {
		return "", errNoResolver
	}

	return in.opts.Resolver.Resolve(ctx, in.conn.Password)
}

// run is the goroutine owning the session: it connects, serves the session
// until it fails or is lost, closes it and connects again after the backoff,
// until ctx is done.
func (in *Input) run(ctx context.Context, own ownKeyPair, emit func(model.Record), life *input.Lifecycle) {
	backoff := driverkit.NewBackoff(in.clk, in.jitter)

	for {
		s, lost, err := in.connect(ctx, own)
		if err == nil {
			l := &link{life: life, emit: emit}
			err = in.serve(ctx, s, lost, l)
			in.health.SetConnected(false)

			// Only a session that exchanged something worked: one that
			// fails at its first request keeps the backoff growing, so the
			// server is not flooded with sessions (Modbus resets on an
			// answer too).
			if l.exchanged.Load() {
				backoff.Reset()
			}
		}
		if ctx.Err() != nil {
			return
		}

		in.health.CountError(errorKind(err))
		in.log.Debug("opcua: no session, retrying after the backoff", "input", in.cfg.ID, "error", err.Error())

		if backoff.Wait(ctx) != nil {
			return
		}
	}
}

// errorKind is the health kind of a session error: ErrTimeout is a timeout,
// everything else a connection failure.
func errorKind(err error) string {
	if errors.Is(err, ErrTimeout) {
		return input.ReadErrorTimeout
	}

	return input.ReadErrorConnection
}

// connect dials a session in a goroutine of its own and waits for it or for
// ctx (Modbus R10): Close must not wait for a dial. When ctx is done first,
// connect returns at once and the goroutine closes the session if its dial
// succeeds late; it never touches the health nor emits. The returned channel
// receives the session's loss: its OnLost is bound to this dial only, so a
// late call from a closed session reaches a channel nobody reads.
func (in *Input) connect(ctx context.Context, own ownKeyPair) (session, <-chan struct{}, error) {
	lost := make(chan struct{}, 1)
	deps := in.dialDeps(own, func() {
		select {
		case lost <- struct{}{}:
		default:
		}
	})

	type result struct {
		s   session
		err error
	}

	// Unbuffered: the session is handed over only to a waiting connect, so
	// exactly one side owns it.
	dialled := make(chan result)

	go func() {
		s, err := in.dial(ctx, in.conn, deps)
		select {
		case dialled <- result{s, err}:
		case <-ctx.Done():
			if err == nil {
				s.Close()
			}
		}
	}()

	select {
	case r := <-dialled:
		return r.s, lost, r.err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// link is what the owner and the notification reader share of one session.
type link struct {
	life *input.Lifecycle
	emit func(model.Record)

	// setup holds the bad samples of the session's setup (addresses that do
	// not resolve, monitored items the server refused). It is written
	// before the reader starts and emitted at the first successful
	// exchange, after the births.
	setup []model.Sample
	first sync.Once
	// exchanged is set at the first successful exchange.
	exchanged atomic.Bool
}

// serve runs one session until it fails, is lost or ctx is done, and closes
// it. The points are resolved (an nsu= address against this server's
// NamespaceArray), the subscribe points monitored, and the poll groups and
// the watchdog read at their times, earliest due first, on the clock. Only a
// successful exchange (a Read response, a watchdog answer, a data
// notification) keeps the devices alive (O4); the setup alone does not.
func (in *Input) serve(ctx context.Context, s session, lost <-chan struct{}, l *link) error {
	var reader sync.WaitGroup

	defer func() {
		s.Close()
		reader.Wait()
	}()

	in.health.SetConnected(true)

	ids, err := in.resolve(s, l)
	if err != nil {
		return err
	}

	if err := in.subscribe(ctx, s, ids, l, &reader); err != nil {
		return err
	}

	groups := make([][]*ua.NodeID, len(in.groups))
	members := make([][]int, len(in.groups))
	for g, group := range in.groups {
		for _, i := range group.points {
			if ids[i] != nil {
				groups[g] = append(groups[g], ids[i])
				members[g] = append(members[g], i)
			}
		}
	}

	now := in.clk.Now()
	every := in.timings().watchdog(in.tolerance)
	watchdogAt := now.Add(every)
	next := make([]time.Time, len(in.groups))
	for g := range next {
		next[g] = now
	}

	for {
		g, due := dueFirst(next, watchdogAt)
		if err := in.waitUntil(ctx, lost, due); err != nil {
			return err
		}

		if g < 0 {
			if err := s.CurrentTime(ctx); err != nil {
				return err
			}
			in.alive(l)
			watchdogAt = nextDue(watchdogAt, every, in.clk.Now())

			continue
		}

		if err := in.poll(ctx, s, groups[g], members[g], l); err != nil {
			return err
		}
		// Read after the exchange, which takes time: a group that fell
		// behind is due again one interval after it was actually read.
		next[g] = nextDue(next[g], in.groups[g].interval, in.clk.Now())
	}
}

// resolve returns the NodeId of every point, nil for a point whose address
// does not resolve on this server (an nsu= namespace it does not have): that
// point gets one bad sample, the others proceed, and kind exception counts
// once for the connect (ruling R8). A library panic fails the session.
func (in *Input) resolve(s session, l *link) ([]*ua.NodeID, error) {
	ids := make([]*ua.NodeID, len(in.points))
	now := in.clk.Now()
	unresolved := false

	for i, p := range in.points {
		nodeID, err := s.Resolve(p.node)
		switch {
		case err == nil:
			ids[i] = nodeID
		case errors.Is(err, errLibraryPanic):
			return nil, err
		default:
			unresolved = true
			l.setup = append(l.setup, in.badSample(i, now))
		}
	}

	if unresolved {
		in.health.CountError(input.ReadErrorException)
	}

	return ids, nil
}

// subscribe creates the session's subscription for the resolved subscribe
// points and starts the reader of its notifications. A monitored item the
// server refuses gets one bad sample, and kind exception counts once for the
// response (ruling R8); the item is created again at the next connect only.
func (in *Input) subscribe(ctx context.Context, s session, ids []*ua.NodeID, l *link, reader *sync.WaitGroup) error {
	var items []item

	for _, i := range in.subscribed {
		if ids[i] == nil {
			continue
		}

		sub := config.Subscribe{Sampling: config.SamplingDefault}
		if in.points[i].cfg.Read.Subscribe != nil {
			sub = *in.points[i].cfg.Read.Subscribe
		}

		items = append(items, item{Handle: uint32(i), Node: ids[i], Sub: sub})
	}

	if len(items) == 0 {
		return nil
	}

	notifications, statuses, err := s.Subscribe(ctx, in.conn.PublishingInterval, items)
	if err != nil {
		return err
	}

	now := in.clk.Now()
	refused := false

	for k, status := range statuses {
		if Quality(status) == model.Bad {
			refused = true
			l.setup = append(l.setup, in.badSample(int(items[k].Handle), now))
		}
	}

	if refused {
		in.health.CountError(input.ReadErrorException)
	}

	// Started after l.setup is complete. The reader never blocks on anything
	// but emit, so the session's drain never stalls the library's publish
	// loop on I/O (Task 4).
	reader.Go(func() {
		var counted uint64

		for n := range notifications {
			if ctx.Err() == nil {
				in.notify(n, l, &counted)
			}
		}
	})

	return nil
}

// notify emits the sample of one data change; a notification is a
// successful exchange (O4). A Bad status counts kind exception once per
// publish message (ruling R8): counted is the last message counted.
func (in *Input) notify(n notification, l *link, counted *uint64) {
	if int(n.Handle) >= len(in.points) {
		in.log.Debug("opcua: notification for an unknown handle ignored", "input", in.cfg.ID, "handle", n.Handle)

		return
	}

	in.alive(l)

	s, kind := in.sample(int(n.Handle), n.Value)
	switch {
	case kind == input.ReadErrorDecode:
		in.health.CountError(kind)
	case kind == input.ReadErrorException && (n.Message == 0 || n.Message != *counted):
		in.health.CountError(kind)
		*counted = n.Message
	}
	l.emit(model.SampleRecord(s))
}

// poll reads one poll group; no request without nodes. Decode counts per
// point, a Bad status once per Read (ruling R8).
func (in *Input) poll(ctx context.Context, s session, ids []*ua.NodeID, members []int, l *link) error {
	if len(ids) == 0 {
		return nil
	}

	values, err := s.Read(ctx, ids)
	if err != nil {
		return err
	}

	in.alive(l)

	bad := false

	for k, dv := range values {
		sample, kind := in.sample(members[k], dv)
		switch kind {
		case input.ReadErrorDecode:
			in.health.CountError(kind)
		case input.ReadErrorException:
			bad = true
		}
		l.emit(model.SampleRecord(sample))
	}

	if bad {
		in.health.CountError(input.ReadErrorException)
	}

	return nil
}

// alive records a successful exchange with the server for every device of
// the input: the devices sit behind the server, whose liveness is theirs
// (O4). The first one of the session also marks it as working and emits the
// bad samples of its setup, after the births.
func (in *Input) alive(l *link) {
	for _, d := range in.devices {
		l.life.Read(d)
	}

	l.first.Do(func() {
		l.exchanged.Store(true)

		for _, s := range l.setup {
			l.emit(model.SampleRecord(s))
		}
	})
}

// dueFirst returns the poll group due first and its time, or -1 and the
// watchdog's time when the watchdog read is due before every group.
func dueFirst(next []time.Time, watchdogAt time.Time) (int, time.Time) {
	group, due := -1, watchdogAt

	for g, t := range next {
		if t.Before(due) || (group < 0 && t.Equal(due)) {
			group, due = g, t
		}
	}

	return group, due
}

// waitUntil waits on the clock until t, or returns the reason it cannot:
// ctx done or the session lost.
func (in *Input) waitUntil(ctx context.Context, lost <-chan struct{}, t time.Time) error {
	d := t.Sub(in.clk.Now())
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-lost:
			return errLost
		default:
			return nil
		}
	}

	timer := in.clk.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-lost:
		return errLost
	case <-timer.C():
		return nil
	}
}

// nextDue returns the next time something done at due is due again: one
// interval later, or one interval after now when it fell behind.
func nextDue(due time.Time, interval time.Duration, now time.Time) time.Time {
	next := due.Add(interval)
	if !next.After(now) {
		next = now.Add(interval)
	}

	return next
}

// sample converts the data value of point i (O8) and returns the health kind
// to count: decode for a value that does not convert, exception for a Bad
// status from the server, "" otherwise.
func (in *Input) sample(i int, dv *ua.DataValue) (model.Sample, string) {
	p := in.points[i]

	v, quality, err := Convert(dv, p.cfg.Type)

	kind := ""
	switch {
	case err != nil:
		kind = input.ReadErrorDecode
	case quality == model.Bad:
		kind = input.ReadErrorException
	}

	return newSample(p, v, quality, Timestamp(dv, in.clk.Now())), kind
}

// badSample is a bad sample of point i carrying the zero value of its type's
// kind (Modbus R9).
func (in *Input) badSample(i int, ts time.Time) model.Sample {
	p := in.points[i]
	v, _ := zeroValue(p.cfg.Type)

	return newSample(p, v, model.Bad, ts.UTC())
}

func newSample(p inputPoint, v model.Value, q model.Quality, ts time.Time) model.Sample {
	priority := p.cfg.Priority
	if !priority.Valid() {
		priority = model.Critical
	}

	return model.Sample{Device: p.device, Signal: p.cfg.Signal, TS: ts, Value: v, Quality: q, Priority: priority}
}
