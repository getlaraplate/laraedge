package opcua

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uapolicy"

	"github.com/getlaraplate/laraedge/internal/config"
)

// The session layer sits between the driver and the library (gopcua v0.9.1).
// It keeps the library's traps out of the agent: it validates the server
// certificate itself (O1: the library validates none), never sends a request
// that can fault (a ServiceFault tears the library client down: Reads and
// monitored item creation are chunked at the server's limits, and no request
// is ever empty, O5), uses each library client once (AutoReconnect off: the
// driver dials a new session after a loss), reports a loss through OnLost,
// turns every panic of a library call made from our goroutines into an error
// (O7) and never puts an endpoint, a user name or a password in an error.
// Values, endpoints and library error texts are logged at debug only.

// Error kinds of the session layer. The driver counts ErrTimeout as kind
// timeout and every other error as kind connection.
var (
	// ErrUntrusted is a server certificate missing from trusted/; the error is
	// an *UntrustedError carrying its thumbprint.
	ErrUntrusted = errors.New("server certificate not trusted")
	// ErrTimeout is a request or a connection attempt the server did not
	// answer in time.
	ErrTimeout = errors.New("timed out")
	// ErrUnknownNamespace is an nsu= address whose namespace URI is not in the
	// server's NamespaceArray.
	ErrUnknownNamespace = errors.New("namespace URI not on the server")
)

var (
	errConnection          = errors.New("connection failed")
	errPassword            = errors.New("password not resolved")
	errLibraryPanic        = errors.New("the OPC UA library panicked")
	errClosed              = errors.New("session closed")
	errMalformed           = errors.New("malformed response")
	errNoEndpoint          = errors.New("the server offers no endpoint with the configured security policy and mode")
	errNoServerCertificate = errors.New("the server endpoint carries no certificate")
	errNoTokenPolicy       = errors.New("the server endpoint does not offer the configured authentication")
	errNoOwnCertificate    = errors.New("own certificate or key missing")
	errUserWithoutSecurity = errors.New("user name authentication needs a security policy other than None")
	errNoItems             = errors.New("subscribe needs at least one item")
	errSubscribed          = errors.New("the session is already subscribed")
	errIdentifier          = errors.New("address: identifier: invalid")
)

// UntrustedError is a server certificate that is not in trusted/.
type UntrustedError struct {
	// Thumbprint is the lowercase hex SHA-1 of the certificate.
	Thumbprint string
}

func (e *UntrustedError) Error() string {
	return "opcua: server certificate not trusted, thumbprint " + e.Thumbprint
}

// Is makes errors.Is(err, ErrUntrusted) true.
func (e *UntrustedError) Is(target error) bool { return target == ErrUntrusted }

// timings are the fixed connection timings of decision O3; tests shorten them
// through dialDeps.timing (ruling R3).
type timings struct {
	// Dial bounds the TCP connection; Request each request. A connection
	// attempt (discovery, then connect and session) is bounded by Dial plus
	// Request per phase.
	Dial    time.Duration
	Request time.Duration
	// Session is the requested session timeout.
	Session time.Duration
	// MaxKeepAliveCount is the subscription's keep-alive count.
	MaxKeepAliveCount uint32
	// LifetimeCount is the subscription's lifetime count; 0 derives it from
	// the publishing interval (subscriptionLifetime). Tests set a huge value:
	// see sharedTestServer.
	LifetimeCount uint32
	// WatchdogMax and WatchdogMin bound the watchdog read interval.
	WatchdogMax time.Duration
	WatchdogMin time.Duration
	// Close bounds the CloseSession request of Close.
	Close time.Duration
}

var productionTimings = timings{
	Dial:              5 * time.Second,
	Request:           5 * time.Second,
	Session:           60 * time.Second,
	MaxKeepAliveCount: 10,
	WatchdogMax:       5 * time.Second,
	WatchdogMin:       time.Second,
	Close:             time.Second,
}

// lifetimeWindow is how long a server keeps a subscription whose session
// was lost: past it, the server deletes the subscription instead of keeping
// it for a client that is not coming back.
const lifetimeWindow = time.Minute

// lifetime is the subscription's lifetime count for interval: LifetimeCount
// when set, otherwise subscriptionLifetime.
func (t timings) lifetime(interval time.Duration) uint32 {
	if t.LifetimeCount != 0 {
		return t.LifetimeCount
	}

	return subscriptionLifetime(interval, t.MaxKeepAliveCount)
}

// subscriptionLifetime is max(3 x maxKeepAlive, ceil(lifetimeWindow /
// interval)): OPC UA requires a lifetime count of at least three keep-alive
// counts, and a lost session's subscription then expires within about
// lifetimeWindow. The library's default (10000 publishing intervals) keeps
// it 2.8 hours at one second.
func subscriptionLifetime(interval time.Duration, maxKeepAlive uint32) uint32 {
	floor := uint64(3) * uint64(maxKeepAlive)
	count := uint64(1)

	if interval > 0 {
		count = uint64((lifetimeWindow + interval - 1) / interval)
	}

	return uint32(min(max(floor, count), math.MaxUint32))
}

// watchdog is the interval of the CurrentTime read: min(WatchdogMax,
// deathTolerance/3), at least WatchdogMin (O3).
func (t timings) watchdog(deathTolerance time.Duration) time.Duration {
	return max(min(t.WatchdogMax, deathTolerance/3), t.WatchdogMin)
}

// Limits of O5: limitUnknown when the server's limit is unreadable,
// limitCap for "no limit" and as the upper bound of any limit (ruling R6: a
// larger request can exceed the server's maximum message size, which is a
// ServiceFault).
const (
	limitUnknown = 100
	limitCap     = 1000
)

// notifyBuffer is the size of the library's notification channel and of the
// session's own.
const notifyBuffer = 1024

// session is what the driver needs of a connected server.
type session interface {
	// Limits are the request sizes the session chunks at (O5).
	Limits() (maxRead, maxMonitored int)
	// Resolve returns the NodeId of an address, looking an nsu= URI up in the
	// NamespaceArray read at this connect.
	Resolve(n Node) (*ua.NodeID, error)
	// Read reads the Value of ids with both timestamps, in chunks; the
	// results are in the order of ids. No request is sent for no ids.
	Read(ctx context.Context, ids []*ua.NodeID) ([]*ua.DataValue, error)
	// Subscribe creates the session's one subscription at the publishing
	// interval and its monitored items, in chunks. It returns the channel of
	// data changes (closed by Close; the driver must keep reading it) and the
	// status of each item in the order of items.
	Subscribe(ctx context.Context, interval time.Duration, items []item) (<-chan notification, []ua.StatusCode, error)
	// CurrentTime reads Server_ServerStatus_CurrentTime, the watchdog: any
	// answer is a live server.
	CurrentTime(ctx context.Context) error
	// Browse lists the nodes a node references through forward hierarchical
	// references, all node classes, following continuation points (task 6,
	// ruling R1). It is the one walk step of the browse command.
	Browse(ctx context.Context, node *ua.NodeID) ([]*ua.ReferenceDescription, error)
	// VariableAttributes reads the DataType and AccessLevel attributes of
	// ids, in chunks, in the order of ids. No request is sent for no ids.
	VariableAttributes(ctx context.Context, ids []*ua.NodeID) ([]variableAttributes, error)
	// Namespaces is the NamespaceArray read at this connect.
	Namespaces() []string
	// Close ends the session. It is idempotent and safe while a call is in
	// flight; it never calls OnLost.
	Close()
}

// item is a point to monitor.
type item struct {
	// Handle comes back in its notifications.
	Handle uint32
	Node   *ua.NodeID
	Sub    config.Subscribe
}

// notification is a data change of a monitored item.
type notification struct {
	Handle uint32
	Value  *ua.DataValue
	// Message numbers the publish message that carried the change, from 1 per
	// session: the changes of one message share it (ruling R8 counts a Bad
	// status once per message).
	Message uint64
}

// dialDeps is what dial needs besides the connection.
type dialDeps struct {
	// InputID names the input in the untrusted certificate warning.
	InputID string
	// OwnCert and OwnKey are the agent's application certificate (DER) and key.
	OwnCert []byte
	OwnKey  *rsa.PrivateKey
	// Trust is the trust store; nil trusts nothing.
	Trust *TrustStore
	// Rejected deduplicates the rejected/ write and the warning of an
	// untrusted certificate; the driver keeps one for the input's lifetime.
	// Nil warns at every dial.
	Rejected *rejectedSet
	// Password resolves the password reference; called at every dial with a
	// user name.
	Password func(ctx context.Context) (string, error)
	// OnLost is called at most once per session, from its own goroutine,
	// when a dialled session loses its connection or its subscription; never
	// for a dial that failed, and never for a loss detected after Close began.
	// A loss detected just before or while Close runs may still call OnLost
	// shortly after Close returned (Close does not wait for it: OnLost may
	// itself call Close). So OnLost must not block, and the caller binds it to
	// the session it dialled (a per-dial closure carrying a generation, say)
	// and ignores a call for a session it already replaced.
	OnLost func()
	// Log receives the warning of O1 and debug details; nil discards.
	Log *slog.Logger

	// timing replaces productionTimings; newClient replaces the library
	// client. Both are for tests.
	timing    *timings
	newClient clientFactory
}

func (d dialDeps) timings() timings {
	if d.timing != nil {
		return *d.timing
	}

	return productionTimings
}

func (d dialDeps) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}

	return slog.New(slog.DiscardHandler)
}

// rejectedSet holds the thumbprints already rejected and warned about.
type rejectedSet struct {
	mu   sync.Mutex
	seen map[string]bool
}

// first reports whether thumb is met for the first time, and records it. A
// nil set has no memory.
func (r *rejectedSet) first(thumb string) bool {
	if r == nil {
		return true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.seen[thumb] {
		return false
	}
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	r.seen[thumb] = true

	return true
}

// uaClient is the part of the library client the session uses: no Write,
// Call or node management (parent 9.1).
type uaClient interface {
	Connect(ctx context.Context) error
	Close(ctx context.Context) error
	Read(ctx context.Context, req *ua.ReadRequest) (*ua.ReadResponse, error)
	Browse(ctx context.Context, req *ua.BrowseRequest) (*ua.BrowseResponse, error)
	BrowseNext(ctx context.Context, req *ua.BrowseNextRequest) (*ua.BrowseNextResponse, error)
	Subscribe(ctx context.Context, params *opcua.SubscriptionParameters, notifyCh chan<- *opcua.PublishNotificationData) (uaSubscription, error)
	Namespaces() []string
}

// uaSubscription is the part of a library subscription the session uses.
type uaSubscription interface {
	Monitor(ctx context.Context, ts ua.TimestampsToReturn, items ...*ua.MonitoredItemCreateRequest) (*ua.CreateMonitoredItemsResponse, error)
}

// clientFactory builds a client for endpoint; onState receives its states.
type clientFactory func(endpoint string, opts []opcua.Option, onState func(opcua.ConnState)) (uaClient, error)

type libraryClient struct {
	*opcua.Client
}

func (c libraryClient) Subscribe(ctx context.Context, params *opcua.SubscriptionParameters, ch chan<- *opcua.PublishNotificationData) (uaSubscription, error) {
	sub, err := c.Client.Subscribe(ctx, params, ch)
	if err != nil {
		return nil, err
	}

	return sub, nil
}

func newLibraryClient(endpoint string, opts []opcua.Option, onState func(opcua.ConnState)) (uaClient, error) {
	c, err := opcua.NewClient(endpoint, append(slices.Clone(opts), opcua.StateChangedFunc(onState))...)
	if err != nil {
		return nil, err
	}

	return libraryClient{Client: c}, nil
}

// panicError is a recovered panic of a library call.
type panicError struct {
	value any
}

func (e *panicError) Error() string        { return errLibraryPanic.Error() }
func (e *panicError) Is(target error) bool { return target == errLibraryPanic }

// contain runs a library call, turning a panic into a *panicError (O7).
func contain(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{value: r}
		}
	}()

	return f()
}

// classify turns a library error into a session error with a fixed text:
// ErrTimeout, errLibraryPanic, the caller's context error, or errConnection.
// A status code is kept (its text is the library's fixed table); anything
// else from the library may name the endpoint and is logged at debug only.
func classify(ctx context.Context, log *slog.Logger, inputID, op string, err error) error {
	attrs := []any{"input", inputID, "op", op, "error", err.Error()}

	var p *panicError
	if errors.As(err, &p) {
		attrs = append(attrs, "panic", fmt.Sprint(p.value))
	}

	log.Debug("opcua: library call failed", attrs...)

	var (
		status ua.StatusCode
		netErr net.Error
	)

	switch {
	case p != nil:
		return fmt.Errorf("opcua %s: %w", op, errLibraryPanic)
	case ctx.Err() != nil:
		return fmt.Errorf("opcua %s: %w", op, ctx.Err())
	case errors.Is(err, ua.StatusBadTimeout):
		return fmt.Errorf("opcua %s: %w: %w", op, ErrTimeout, ua.StatusBadTimeout)
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Errorf("opcua %s: %w", op, ErrTimeout)
	case errors.As(err, &status):
		return fmt.Errorf("opcua %s: %w: %w", op, errConnection, status)
	default:
		return fmt.Errorf("opcua %s: %w", op, errConnection)
	}
}

// securityMode is the library's mode of conn.
func securityMode(conn Connection) ua.MessageSecurityMode {
	switch conn.SecurityMode {
	case ModeSign:
		return ua.MessageSecurityModeSign
	case ModeSignAndEncrypt:
		return ua.MessageSecurityModeSignAndEncrypt
	default:
		return ua.MessageSecurityModeNone
	}
}

// discover asks the server for its endpoints over a None channel and returns
// the one matching the configured policy and mode (ruling R1). Discovery is
// not authenticated: the trust check of the returned certificate is what
// authenticates the server afterwards.
func discover(ctx context.Context, conn Connection) (*ua.EndpointDescription, error) {
	return discoverWith(ctx, conn, productionTimings, slog.New(slog.DiscardHandler), "")
}

func discoverWith(ctx context.Context, conn Connection, t timings, log *slog.Logger, inputID string) (*ua.EndpointDescription, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("opcua discover endpoints: %w", err)
	}

	dctx, cancel := context.WithTimeout(ctx, t.Dial+t.Request)
	defer cancel()

	var eps []*ua.EndpointDescription

	err := contain(func() (err error) {
		eps, err = opcua.GetEndpoints(dctx, conn.Endpoint, opcua.DialTimeout(t.Dial), opcua.RequestTimeout(t.Request))

		return err
	})
	if err != nil {
		return nil, classify(ctx, log, inputID, "discover endpoints", err)
	}

	var ep *ua.EndpointDescription

	policy := conn.SecurityPolicy
	if policy == PolicyNone {
		policy = ua.SecurityPolicyURINone
	}

	err = contain(func() (err error) {
		ep, err = opcua.SelectEndpoint(slices.Clone(eps), policy, securityMode(conn))

		return err
	})
	if err != nil || ep == nil {
		return nil, fmt.Errorf("opcua discover endpoints: %w", errNoEndpoint)
	}

	return ep, nil
}

// lossWatcher turns the library's states into one OnLost call. A loss counts
// once the client was Connected and is reported once dial armed the watcher,
// so a dial that fails reports nothing; Close sets closing first, so a
// deliberate close reports nothing either. A loss reported before closing was
// set may still reach OnLost after Close returned (see dialDeps.OnLost).
type lossWatcher struct {
	onLost func()

	connected atomic.Bool
	armed     atomic.Bool
	closing   atomic.Bool
	lost      atomic.Bool
	fired     atomic.Bool
}

func (w *lossWatcher) state(s opcua.ConnState) {
	switch s {
	case opcua.Connected:
		w.connected.Store(true)
	case opcua.Disconnected, opcua.Closed:
		if w.connected.Load() && !w.closing.Load() {
			w.signal()
		}
	}
}

// signal records a loss and reports it when armed.
func (w *lossWatcher) signal() {
	w.lost.Store(true)
	w.fire()
}

func (w *lossWatcher) arm() {
	w.armed.Store(true)
	if w.lost.Load() {
		w.fire()
	}
}

// fire calls OnLost on its own goroutine, at most once: the library calls
// the state function from its own goroutines, which must never block on the
// driver. The goroutine checks closing again, which narrows, but cannot
// close, the window in which OnLost runs after Close.
func (w *lossWatcher) fire() {
	if !w.armed.Load() || w.closing.Load() || !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.onLost != nil {
		go func() {
			if !w.closing.Load() {
				w.onLost()
			}
		}()
	}
}

// uaSession is the session over one library client.
type uaSession struct {
	inputID      string
	client       uaClient
	watch        *lossWatcher
	log          *slog.Logger
	t            timings
	maxRead      int
	maxMonitored int

	done chan struct{}
	wg   sync.WaitGroup
	// messages counts the data change messages; only the drain goroutine
	// touches it.
	messages uint64

	mu         sync.Mutex
	closed     bool
	subscribed bool
}

// dial connects to the server of conn: with a policy other than None it
// discovers the endpoint, accepts it only when its certificate is trusted
// (O1) and pins it; it resolves the password (O11), connects with
// AutoReconnect off and the O3 timings, and reads the limits (O5). It returns
// an *UntrustedError for an untrusted certificate, after writing it to
// rejected/ and warning once per thumbprint (deps.Rejected).
func dial(ctx context.Context, conn Connection, deps dialDeps) (session, error) {
	t := deps.timings()
	log := deps.logger()

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("opcua connect: %w", err)
	}

	appURI, err := applicationURI(deps.OwnCert)
	if err != nil {
		return nil, err
	}

	opts := []opcua.Option{
		opcua.AutoReconnect(false),
		opcua.DialTimeout(t.Dial),
		opcua.RequestTimeout(t.Request),
		opcua.SessionTimeout(t.Session),
		opcua.ApplicationName("laraplate-edge"),
	}

	if conn.SecurityPolicy == PolicyNone {
		if conn.User != "" {
			return nil, fmt.Errorf("opcua connect: %w", errUserWithoutSecurity)
		}

		opts = append(opts, opcua.SecurityPolicy(ua.SecurityPolicyURINone), opcua.SecurityMode(ua.MessageSecurityModeNone))
	} else {
		secure, err := secureOptions(ctx, conn, deps, t, log)
		if err != nil {
			return nil, err
		}

		opts = append(opts, secure...)
	}

	// After Certificate, which sets it from the certificate too: equal values.
	opts = append(opts, opcua.ApplicationURI(appURI))

	if conn.User != "" {
		if deps.Password == nil {
			return nil, fmt.Errorf("opcua connect: %w", errPassword)
		}

		password, err := deps.Password(ctx)
		if err != nil {
			return nil, fmt.Errorf("opcua connect: %w: %w", errPassword, err)
		}

		opts = append(opts, opcua.AuthUsername(conn.User, password))
	}

	return connect(ctx, conn, deps, opts, t, log)
}

// secureOptions discovers the endpoint, checks its certificate against the
// trust store and returns the options pinning it.
func secureOptions(ctx context.Context, conn Connection, deps dialDeps, t timings, log *slog.Logger) ([]opcua.Option, error) {
	if len(deps.OwnCert) == 0 || deps.OwnKey == nil {
		return nil, fmt.Errorf("opcua connect: %w", errNoOwnCertificate)
	}

	ep, err := discoverWith(ctx, conn, t, log, deps.InputID)
	if err != nil {
		return nil, err
	}

	if err := checkTrust(ep, deps, log); err != nil {
		return nil, err
	}

	tokenType := ua.UserTokenTypeAnonymous
	if conn.User != "" {
		tokenType = ua.UserTokenTypeUserName
	}

	if !slices.ContainsFunc(ep.UserIdentityTokens, func(p *ua.UserTokenPolicy) bool { return p != nil && p.TokenType == tokenType }) {
		return nil, fmt.Errorf("opcua connect: %w", errNoTokenPolicy)
	}

	return []opcua.Option{
		opcua.SecurityFromEndpoint(ep, tokenType),
		opcua.Certificate(deps.OwnCert),
		opcua.PrivateKey(deps.OwnKey),
	}, nil
}

// checkTrust accepts the endpoint's certificate only when trusted/ holds its
// thumbprint (O1). Otherwise, the first time this thumbprint is met, it
// writes the certificate to rejected/ and warns with the command that trusts
// it; never with the endpoint.
func checkTrust(ep *ua.EndpointDescription, deps dialDeps, log *slog.Logger) error {
	if len(ep.ServerCertificate) == 0 {
		return fmt.Errorf("opcua connect: %w", errNoServerCertificate)
	}

	thumb := Thumbprint(ep.ServerCertificate)
	if deps.Trust != nil && deps.Trust.Trusted(thumb) {
		return nil
	}

	if deps.Rejected.first(thumb) {
		if deps.Trust != nil {
			if err := deps.Trust.Reject(ep.ServerCertificate); err != nil {
				log.Warn("opcua: cannot store a rejected server certificate", "input", deps.InputID, "thumbprint", thumb, "error", err)
			}
		}

		log.Warn(fmt.Sprintf("input %s: server certificate not trusted, thumbprint %s; trust it with: laraplate-edge opcua trust %s", deps.InputID, thumb, deps.InputID),
			"input", deps.InputID, "thumbprint", thumb)
	}

	return &UntrustedError{Thumbprint: thumb}
}

// applicationURI is the first URI of the own certificate, the agent's
// application URI (O2).
func applicationURI(certDER []byte) (string, error) {
	if len(certDER) == 0 {
		return "", fmt.Errorf("opcua connect: %w", errNoOwnCertificate)
	}

	cert, err := uapolicy.ParseCertificate(certDER)
	if err != nil || len(cert.URIs) == 0 {
		return "", fmt.Errorf("opcua connect: %w", errOwnCertInvalid)
	}

	return cert.URIs[0].String(), nil
}

// connect builds the client, connects it and reads the limits.
func connect(ctx context.Context, conn Connection, deps dialDeps, opts []opcua.Option, t timings, log *slog.Logger) (session, error) {
	newClient := deps.newClient
	if newClient == nil {
		newClient = newLibraryClient
	}

	watch := &lossWatcher{onLost: deps.OnLost}

	var client uaClient

	err := contain(func() (err error) {
		client, err = newClient(conn.Endpoint, opts, watch.state)

		return err
	})
	if err != nil {
		return nil, classify(ctx, log, deps.InputID, "create the client", err)
	}

	s := &uaSession{
		inputID: deps.InputID,
		client:  client,
		watch:   watch,
		log:     log,
		t:       t,
		done:    make(chan struct{}),
	}

	cctx, cancel := context.WithTimeout(ctx, t.Dial+t.Request)
	defer cancel()

	if err := contain(func() error { return client.Connect(cctx) }); err != nil {
		s.Close()

		return nil, classify(ctx, log, deps.InputID, "connect", err)
	}

	if err := s.readLimits(cctx); err != nil {
		s.Close()

		if ctx.Err() != nil {
			return nil, fmt.Errorf("opcua connect: %w", ctx.Err())
		}

		return nil, err
	}

	watch.arm()

	return s, nil
}

// readLimits reads MaxNodesPerRead and MaxMonitoredItemsPerCall with one
// single-node Read each (ruling R6: a server whose MaxNodesPerRead is 1 would
// fault a two-node Read on every connect).
func (s *uaSession) readLimits(ctx context.Context) error {
	limits := make([]int, 2)

	for i, nodeID := range []uint32{
		id.Server_ServerCapabilities_OperationLimits_MaxNodesPerRead,
		id.Server_ServerCapabilities_OperationLimits_MaxMonitoredItemsPerCall,
	} {
		res, err := s.read(ctx, "read the limits", &ua.ReadRequest{
			TimestampsToReturn: ua.TimestampsToReturnNeither,
			NodesToRead:        []*ua.ReadValueID{{NodeID: ua.NewNumericNodeID(0, nodeID), AttributeID: ua.AttributeIDValue}},
		})
		if err != nil {
			return err
		}

		// read checked that there is one result per node.
		limits[i] = limitValue(res.Results[0])
	}

	s.maxRead, s.maxMonitored = limits[0], limits[1]

	return nil
}

// limitValue is an operation limit (O5): a Good non-negative integer, 0
// meaning no limit; limitCap bounds it; anything else is limitUnknown.
func limitValue(dv *ua.DataValue) int {
	if dv == nil || uint32(dv.Status)>>30 != 0 || dv.Value == nil {
		return limitUnknown
	}

	var n int64

	switch v := dv.Value.Value().(type) {
	case uint8:
		n = int64(v)
	case uint16:
		n = int64(v)
	case uint32:
		n = int64(v)
	case uint64:
		n = int64(min(v, math.MaxInt64))
	case int8:
		n = int64(v)
	case int16:
		n = int64(v)
	case int32:
		n = int64(v)
	case int64:
		n = v
	default:
		return limitUnknown
	}

	switch {
	case n < 0:
		return limitUnknown
	case n == 0, n > limitCap:
		return limitCap
	default:
		return int(n)
	}
}

func (s *uaSession) Limits() (maxRead, maxMonitored int) {
	return s.maxRead, s.maxMonitored
}

func (s *uaSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closed
}

// read sends one Read request.
func (s *uaSession) read(ctx context.Context, op string, req *ua.ReadRequest) (*ua.ReadResponse, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("opcua %s: %w", op, errClosed)
	}

	var res *ua.ReadResponse

	err := contain(func() (err error) {
		res, err = s.client.Read(ctx, req)

		return err
	})
	if err != nil {
		if s.isClosed() {
			return nil, fmt.Errorf("opcua %s: %w", op, errClosed)
		}

		return nil, classify(ctx, s.log, s.inputID, op, err)
	}

	if res == nil || len(res.Results) != len(req.NodesToRead) {
		return nil, fmt.Errorf("opcua %s: %w", op, errMalformed)
	}

	return res, nil
}

func (s *uaSession) Read(ctx context.Context, ids []*ua.NodeID) ([]*ua.DataValue, error) {
	out := make([]*ua.DataValue, 0, len(ids))

	for chunk := range slices.Chunk(ids, s.maxRead) {
		nodes := make([]*ua.ReadValueID, len(chunk))
		for i, nodeID := range chunk {
			nodes[i] = &ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue}
		}

		res, err := s.read(ctx, "read", &ua.ReadRequest{TimestampsToReturn: ua.TimestampsToReturnBoth, NodesToRead: nodes})
		if err != nil {
			return nil, err
		}

		out = append(out, res.Results...)
	}

	return out, nil
}

// variableAttributes are what browse reads of a variable besides its
// reference.
type variableAttributes struct {
	DataType    *ua.DataValue
	AccessLevel *ua.DataValue
}

// VariableAttributes reads the DataType and AccessLevel attributes of ids in
// one batch, chunked at the server's MaxNodesPerRead (two nodes per id),
// never an empty request.
func (s *uaSession) VariableAttributes(ctx context.Context, ids []*ua.NodeID) ([]variableAttributes, error) {
	nodes := make([]*ua.ReadValueID, 0, 2*len(ids))
	for _, nodeID := range ids {
		nodes = append(nodes,
			&ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDDataType},
			&ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDAccessLevel},
		)
	}

	values := make([]*ua.DataValue, 0, len(nodes))

	for chunk := range slices.Chunk(nodes, s.maxRead) {
		res, err := s.read(ctx, "read the variable attributes", &ua.ReadRequest{TimestampsToReturn: ua.TimestampsToReturnNeither, NodesToRead: chunk})
		if err != nil {
			return nil, err
		}

		values = append(values, res.Results...)
	}

	out := make([]variableAttributes, len(ids))
	for i := range out {
		out[i] = variableAttributes{DataType: values[2*i], AccessLevel: values[2*i+1]}
	}

	return out, nil
}

func (s *uaSession) Namespaces() []string {
	var namespaces []string

	if err := contain(func() error { namespaces = slices.Clone(s.client.Namespaces()); return nil }); err != nil {
		return nil
	}

	return namespaces
}

func (s *uaSession) CurrentTime(ctx context.Context) error {
	_, err := s.read(ctx, "watchdog read", &ua.ReadRequest{
		TimestampsToReturn: ua.TimestampsToReturnNeither,
		NodesToRead: []*ua.ReadValueID{
			{NodeID: ua.NewNumericNodeID(0, id.Server_ServerStatus_CurrentTime), AttributeID: ua.AttributeIDValue},
		},
	})

	return err
}

func (s *uaSession) Resolve(n Node) (*ua.NodeID, error) {
	if n.ID != nil {
		return n.ID, nil
	}

	var namespaces []string

	if err := contain(func() error { namespaces = s.client.Namespaces(); return nil }); err != nil {
		return nil, fmt.Errorf("opcua resolve: %w", errLibraryPanic)
	}

	index := slices.Index(namespaces, n.NamespaceURI)
	if index < 0 || index > math.MaxUint16 {
		return nil, fmt.Errorf("opcua resolve: %w", ErrUnknownNamespace)
	}

	nodeID, err := ua.ParseNodeID("ns=" + strconv.Itoa(index) + ";" + n.Rest)
	if err != nil {
		return nil, fmt.Errorf("opcua resolve: %w", errIdentifier)
	}

	return nodeID, nil
}

// monitorRequest is the monitored item of it (O9): Value attribute, reporting,
// sampling in milliseconds (-1: the publishing interval), queue (0: 10),
// discard oldest, a DataChangeFilter when a deadband is set.
func monitorRequest(it item) (*ua.MonitoredItemCreateRequest, error) {
	kind, value, err := ParseDeadband(it.Sub.Deadband)
	if err != nil {
		return nil, err
	}

	req := opcua.NewMonitoredItemCreateRequestWithDefaults(it.Node, ua.AttributeIDValue, it.Handle)
	params := req.RequestedParameters

	params.SamplingInterval = -1
	if it.Sub.Sampling != config.SamplingDefault {
		params.SamplingInterval = float64(it.Sub.Sampling) / float64(time.Millisecond)
	}

	params.QueueSize = 10
	if it.Sub.Queue != 0 {
		params.QueueSize = it.Sub.Queue
	}

	params.DiscardOldest = true

	if kind != uint32(ua.DeadbandTypeNone) {
		params.Filter = ua.NewExtensionObject(&ua.DataChangeFilter{
			Trigger:       ua.DataChangeTriggerStatusValue,
			DeadbandType:  kind,
			DeadbandValue: value,
		})
	}

	return req, nil
}

func (s *uaSession) Subscribe(ctx context.Context, interval time.Duration, items []item) (<-chan notification, []ua.StatusCode, error) {
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("opcua subscribe: %w", errNoItems)
	}

	reqs := make([]*ua.MonitoredItemCreateRequest, len(items))
	for i, it := range items {
		req, err := monitorRequest(it)
		if err != nil {
			return nil, nil, fmt.Errorf("opcua subscribe: %w", err)
		}
		reqs[i] = req
	}

	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()

		return nil, nil, fmt.Errorf("opcua subscribe: %w", errClosed)
	case s.subscribed:
		s.mu.Unlock()

		return nil, nil, fmt.Errorf("opcua subscribe: %w", errSubscribed)
	}
	s.subscribed = true
	s.wg.Add(1)
	s.mu.Unlock()

	in := make(chan *opcua.PublishNotificationData, notifyBuffer)
	out := make(chan notification, notifyBuffer)

	go s.drain(in, out)

	var sub uaSubscription

	err := contain(func() (err error) {
		sub, err = s.client.Subscribe(ctx, &opcua.SubscriptionParameters{Interval: interval, MaxKeepAliveCount: s.t.MaxKeepAliveCount, LifetimeCount: s.t.lifetime(interval)}, in)

		return err
	})
	if err != nil {
		return nil, nil, classify(ctx, s.log, s.inputID, "subscribe", err)
	}

	statuses := make([]ua.StatusCode, 0, len(items))

	for chunk := range slices.Chunk(reqs, s.maxMonitored) {
		var res *ua.CreateMonitoredItemsResponse

		err := contain(func() (err error) {
			res, err = sub.Monitor(ctx, ua.TimestampsToReturnBoth, chunk...)

			return err
		})
		if err != nil {
			return nil, nil, classify(ctx, s.log, s.inputID, "create monitored items", err)
		}
		if res == nil || len(res.Results) != len(chunk) {
			return nil, nil, fmt.Errorf("opcua create monitored items: %w", errMalformed)
		}

		for _, r := range res.Results {
			if r == nil {
				statuses = append(statuses, ua.StatusBad)

				continue
			}
			statuses = append(statuses, r.StatusCode)
		}
	}

	return out, statuses, nil
}

// drain empties the library's channel until Close, so the library's publish
// loop never blocks on it, and forwards the data changes. A status change or
// a publish fault means the subscription is gone: OnLost.
func (s *uaSession) drain(in <-chan *opcua.PublishNotificationData, out chan<- notification) {
	defer s.wg.Done()
	defer close(out)

	for {
		select {
		case <-s.done:
			return
		case msg := <-in:
			if !s.dispatch(msg, out) {
				return
			}
		}
	}
}

// dispatch handles one message of the library; false when Close interrupted.
func (s *uaSession) dispatch(msg *opcua.PublishNotificationData, out chan<- notification) bool {
	if msg == nil {
		return true
	}

	if msg.Error != nil {
		var status ua.StatusCode
		if errors.As(msg.Error, &status) && uint32(status)>>30 != 0 {
			s.log.Debug("opcua: subscription lost", "input", s.inputID, "error", msg.Error.Error())
			s.watch.signal()

			return true
		}

		s.log.Debug("opcua: publish message ignored", "input", s.inputID, "error", msg.Error.Error())

		return true
	}

	switch v := msg.Value.(type) {
	case *ua.DataChangeNotification:
		s.messages++

		for _, mi := range v.MonitoredItems {
			if mi == nil {
				continue
			}

			select {
			case out <- notification{Handle: mi.ClientHandle, Value: mi.Value, Message: s.messages}:
			case <-s.done:
				return false
			}
		}
	case *ua.StatusChangeNotification:
		s.log.Debug("opcua: subscription status changed", "input", s.inputID, "status", v.Status.Error())
		s.watch.signal()
	}

	return true
}

func (s *uaSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return
	}
	s.closed = true
	s.mu.Unlock()

	s.watch.closing.Store(true)
	close(s.done)

	ctx, cancel := context.WithTimeout(context.Background(), s.t.Close)
	defer cancel()

	if err := contain(func() error { return s.client.Close(ctx) }); err != nil {
		s.log.Debug("opcua: close failed", "input", s.inputID, "error", err.Error())
	}

	s.wg.Wait()
}
