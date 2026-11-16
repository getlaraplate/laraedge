// This file holds the dial path of the OPC UA input: endpoint discovery, security options, the trust check and the connection to the server.

package opcua

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uapolicy"
)

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
