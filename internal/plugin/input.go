package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/getlaraplate/laraedge-sdk/pluginv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/secrets"
)

// Deadlines of the Input calls (spec section 6).
const (
	configureTimeout = 10 * time.Second
	probeTimeout     = 30 * time.Second

	// A Subscribe the plugin refuses with ALREADY_EXISTS (an older stream of
	// the instance is still unwinding) is retried every busyWait for up to
	// busyBound before it counts as an outage. busyBound matches the 10 s
	// the SDK allows the plugin's Stop before it releases the instance
	// (stopTimeout).
	busyWait  = 100 * time.Millisecond
	busyBound = 10 * time.Second
)

// SecretResolver resolves a secret:// reference; *secrets.Resolver is one.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Node is one node found by Browse.
type Node struct {
	// Address is usable as the address of a point.
	Address string
	// Name is the display name of the node.
	Name string
	// Type is the data type of the node as the plugin names it.
	Type string
	// Access is for example read or read_write.
	Access string
}

// Browser is implemented by the plugin inputs whose manifest declares browse.
type Browser interface {
	// Browse lists the nodes below node (empty for the root), depth levels
	// deep (zero lists the children only).
	Browse(ctx context.Context, node string, depth int) ([]Node, error)
}

// ConfigProblem is one problem of an input section, as the plugin or the
// secret resolution reported it. Path is relative to the input section.
type ConfigProblem struct {
	Path    string
	Message string
}

// ConfigRefusedError is the error of an input the plugin refused to
// configure, or whose secrets cannot be resolved. It names paths, never
// values.
type ConfigRefusedError struct {
	// Input is the id of the input.
	Input string
	// Problems are the problems found, never empty.
	Problems []ConfigProblem
}

// Messages renders the problems as "<prefix>.<path>: <message>", with the
// slice indexes of the path written as dotted numbers. The prefix is the
// dotted document path of the input, such as inputs.2.
func (e *ConfigRefusedError) Messages(prefix string) []string {
	out := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		path := prefix
		if p.Path != "" {
			path += "." + dotted(p.Path)
		}
		out = append(out, path+": "+p.Message)
	}

	return out
}

// Error lists the problems under the prefix inputs.<id>: the supervisor,
// which knows the position of the input in the document, can render them
// again with Messages.
func (e *ConfigRefusedError) Error() string {
	return strings.Join(e.Messages("inputs."+e.Input), "; ")
}

// InputFactory returns the factory of the inputs of the plugin h runs. It
// only validates and prepares: nothing starts or contacts the plugin before
// Start. The built input implements Browser exactly when the manifest of the
// plugin declares browse. Secrets are resolved with resolver at every
// Configure, and the resolved values go to the plugin only.
func InputFactory(h *Host, resolver SecretResolver) input.Factory {
	return func(cfg config.Input, clk clock.Clock) (input.Input, error) {
		kind := h.spec.Manifest.Kinds.Input
		if kind == nil {
			return nil, fmt.Errorf("plugin input %q: the plugin does not offer the input kind", cfg.ID)
		}
		if len(cfg.Raw) == 0 {
			return nil, fmt.Errorf("plugin input %q: the input has no section to configure the plugin with", cfg.ID)
		}
		if clk == nil {
			clk = clock.Real()
		}

		in := &pluginInput{
			h: h, resolver: resolver, cfg: cfg, clk: clk,
			log:   h.log.With("input", cfg.ID),
			known: map[string]bool{},
		}
		for _, d := range cfg.Devices {
			for _, p := range d.Points {
				in.known[d.Device+"/"+p.Signal] = true
			}
		}
		if kind.Browse {
			return &browsingInput{in}, nil
		}

		return in, nil
	}
}

// pluginInput is an input served by a plugin process. Its single run
// goroutine owns the connection to the plugin: it configures the instance,
// opens the one Subscribe stream, and after a plugin restart does both again
// (decision H6).
type pluginInput struct {
	h        *Host
	resolver SecretResolver
	cfg      config.Input
	clk      clock.Clock
	log      *slog.Logger
	known    map[string]bool // device/signal of the points
	health   driverkit.Health

	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// cfgMu serializes Configure; cfgGen is the host generation the instance
	// is configured for, 0 when it is not.
	cfgMu  sync.Mutex
	cfgGen uint64
}

var (
	_ input.Input          = (*pluginInput)(nil)
	_ input.HealthReporter = (*pluginInput)(nil)
	_ Browser              = (*browsingInput)(nil)
)

// browsingInput is a pluginInput of a plugin that declares browse.
type browsingInput struct{ *pluginInput }

// Start configures the instance when the plugin is up and starts the stream
// and the device lifecycle; it does not wait for a sample. It fails when the
// plugin refuses the configuration or a secret cannot be resolved
// (*ConfigRefusedError). A plugin that is down or fails the call is not a
// failure: the input then reports its devices dead and configures again when
// the plugin is back.
func (in *pluginInput) Start(ctx context.Context, emit func(model.Record)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("plugin input %q: start: %w", in.cfg.ID, err)
	}

	in.mu.Lock()
	switch {
	case in.closed:
		in.mu.Unlock()

		return fmt.Errorf("plugin input %q: start after close", in.cfg.ID)
	case in.started:
		in.mu.Unlock()

		return fmt.Errorf("plugin input %q: already started", in.cfg.ID)
	}
	runCtx, cancel := context.WithCancel(ctx)
	in.cancel = cancel
	in.started = true
	in.mu.Unlock()

	// fail undoes the start: the input can be started again.
	fail := func(err error) error {
		cancel()
		in.mu.Lock()
		in.started, in.cancel = false, nil
		in.mu.Unlock()

		return err
	}

	// Watch before reading the connection: a restart in between is seen.
	changes, unwatch := in.h.Watch()
	if conn, gen := in.h.Conn(); conn != nil {
		if err := in.ensureConfigured(runCtx, conn, gen); err != nil {
			var refused *ConfigRefusedError
			if errors.As(err, &refused) || runCtx.Err() != nil {
				unwatch()

				return fail(err)
			}
			in.log.Warn("plugin input could not be configured at start, retrying", "error", err.Error())
		}
	}

	// The goroutines start under mu, so a Close either sees them or comes
	// first and refuses the start: none starts after Close returned.
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		unwatch()
		cancel()

		return fmt.Errorf("plugin input %q: start after close", in.cfg.ID)
	}
	life := input.NewLifecycle(in.cfg, in.clk, emit)
	in.wg.Go(func() { life.Run(runCtx) })
	in.wg.Go(func() {
		defer unwatch()
		in.run(runCtx, emit, life, changes)
	})

	return nil
}

// Close ends the stream and waits for the goroutines of the input; nothing
// is emitted afterwards. It is safe on an input never started.
func (in *pluginInput) Close() error {
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

// Health returns the connection state and the read errors the plugin
// reported.
func (in *pluginInput) Health() input.Health { return in.health.Snapshot() }

// Probe reads every point once through the plugin. The instance is
// configured first when this input has not been (a probe needs no Start).
func (in *pluginInput) Probe(ctx context.Context) ([]model.Sample, error) {
	conn, err := in.ready(ctx)
	if err != nil {
		return nil, err
	}

	callCtx, release := in.h.deadline(ctx, probeTimeout)
	defer release()

	resp, err := pb.NewInputClient(conn).Probe(callCtx, &pb.ProbeRequest{InstanceId: in.cfg.ID})
	if err != nil {
		return nil, in.callError(ctx, callCtx, "probe", err)
	}
	out := make([]model.Sample, 0, len(resp.GetSamples()))
	for i, wire := range resp.GetSamples() {
		s, err := sampleFromWire(wire)
		if err != nil {
			return nil, fmt.Errorf("plugin input %q: probe: sample %d: %w", in.cfg.ID, i, err)
		}
		out = append(out, s)
	}

	return out, nil
}

// Browse lists the nodes below node through the plugin.
func (b *browsingInput) Browse(ctx context.Context, node string, depth int) ([]Node, error) {
	if depth < 0 {
		return nil, fmt.Errorf("plugin input %q: browse: depth must not be negative", b.cfg.ID)
	}
	if depth > math.MaxInt32 {
		return nil, fmt.Errorf("plugin input %q: browse: depth is too large", b.cfg.ID)
	}
	conn, err := b.ready(ctx)
	if err != nil {
		return nil, err
	}

	callCtx, release := b.h.deadline(ctx, probeTimeout)
	defer release()

	resp, err := pb.NewInputClient(conn).Browse(callCtx, &pb.BrowseRequest{InstanceId: b.cfg.ID, Node: node, Depth: uint32(depth)})
	if err != nil {
		return nil, b.callError(ctx, callCtx, "browse", err)
	}
	out := make([]Node, 0, len(resp.GetNodes()))
	for _, n := range resp.GetNodes() {
		out = append(out, Node{Address: n.GetAddress(), Name: n.GetName(), Type: n.GetType(), Access: n.GetAccess()})
	}

	return out, nil
}

// ready returns the connection of a plugin that is up and has the instance
// configured.
func (in *pluginInput) ready(ctx context.Context) (*grpc.ClientConn, error) {
	conn, gen := in.h.Conn()
	if conn == nil {
		return nil, fmt.Errorf("plugin input %q: the plugin %q is not running", in.cfg.ID, in.h.spec.ID)
	}
	if err := in.ensureConfigured(ctx, conn, gen); err != nil {
		return nil, err
	}

	return conn, nil
}

// callError turns an RPC failure into fixed text: a plugin's status message
// is its own words.
func (in *pluginInput) callError(ctx, callCtx context.Context, what string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("plugin input %q: %s: %w", in.cfg.ID, what, ctxErr)
	}
	if errors.Is(context.Cause(callCtx), context.DeadlineExceeded) {
		return fmt.Errorf("plugin input %q: %s: the plugin did not answer in time", in.cfg.ID, what)
	}

	return fmt.Errorf("plugin input %q: %s: the plugin answered %s", in.cfg.ID, what, status.Code(err))
}

// ensureConfigured configures the instance unless it is configured for the
// running generation of the plugin. A refusal leaves the instance
// unconfigured.
func (in *pluginInput) ensureConfigured(ctx context.Context, conn *grpc.ClientConn, gen uint64) error {
	in.cfgMu.Lock()
	defer in.cfgMu.Unlock()

	if in.cfgGen == gen {
		return nil
	}
	in.cfgGen = 0
	if err := in.configure(ctx, conn); err != nil {
		return err
	}
	in.cfgGen = gen

	return nil
}

// configure sends the input section with its secrets resolved now.
func (in *pluginInput) configure(ctx context.Context, conn *grpc.ClientConn) error {
	payload, resolved, err := in.payload(ctx)
	if err != nil {
		return err
	}

	callCtx, release := in.h.deadline(ctx, configureTimeout)
	defer release()

	resp, err := pb.NewInputClient(conn).Configure(callCtx, &pb.ConfigureRequest{InstanceId: in.cfg.ID, Config: payload})
	if err != nil {
		return in.callError(ctx, callCtx, "configure", err)
	}
	if len(resp.GetErrors()) == 0 {
		return nil
	}

	refused := &ConfigRefusedError{Input: in.cfg.ID}
	for _, e := range resp.GetErrors() {
		refused.Problems = append(refused.Problems, ConfigProblem{
			Path:    redact(e.GetPath(), resolved),
			Message: redact(e.GetMessage(), resolved),
		})
	}

	return refused
}

// payload returns the input section with every secret reference of the
// connection resolved, and the resolved values (for redaction).
func (in *pluginInput) payload(ctx context.Context) ([]byte, []string, error) {
	var section map[string]any
	dec := json.NewDecoder(bytes.NewReader(in.cfg.Raw))
	dec.UseNumber()
	if err := dec.Decode(&section); err != nil {
		return nil, nil, &ConfigRefusedError{Input: in.cfg.ID, Problems: []ConfigProblem{{Message: "the input section is not a JSON object"}}}
	}

	var (
		values   []string
		problems []ConfigProblem
	)
	if connection, ok := section["connection"]; ok {
		section["connection"] = in.resolveSecrets(ctx, connection, "connection", &values, &problems)
	}
	if len(problems) > 0 {
		return nil, nil, &ConfigRefusedError{Input: in.cfg.ID, Problems: problems}
	}

	out, err := json.Marshal(section)
	if err != nil {
		return nil, nil, &ConfigRefusedError{Input: in.cfg.ID, Problems: []ConfigProblem{{Message: "the input section cannot be encoded"}}}
	}

	return out, values, nil
}

// resolveSecrets walks v, resolving the references held by secret keys, and
// returns the new tree. A failure is a problem naming the path only.
func (in *pluginInput) resolveSecrets(ctx context.Context, v any, path string, values *[]string, problems *[]ConfigProblem) any {
	switch node := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := node[key]
			childPath := path + "." + key
			if ref, ok := child.(string); ok && config.IsSecretKey(key) && secrets.IsReference(ref) {
				node[key] = in.resolveOne(ctx, ref, childPath, values, problems)

				continue
			}
			node[key] = in.resolveSecrets(ctx, child, childPath, values, problems)
		}
	case []any:
		for i, child := range node {
			node[i] = in.resolveSecrets(ctx, child, fmt.Sprintf("%s.%d", path, i), values, problems)
		}
	}

	return v
}

func (in *pluginInput) resolveOne(ctx context.Context, ref, path string, values *[]string, problems *[]ConfigProblem) any {
	if in.resolver == nil {
		*problems = append(*problems, ConfigProblem{Path: path, Message: "the secret cannot be resolved: no secret resolver"})

		return ref
	}
	value, err := in.resolver.Resolve(ctx, ref)
	if err != nil {
		*problems = append(*problems, ConfigProblem{Path: path, Message: "the secret cannot be resolved"})

		return ref
	}
	if value != "" {
		*values = append(*values, value)
	}

	return value
}

// redact hides every resolved secret in text a plugin wrote.
func redact(text string, secretValues []string) string {
	for _, value := range secretValues {
		text = strings.ReplaceAll(text, value, "***")
	}

	return text
}

// run keeps the instance streaming until ctx ends: it opens the stream, and
// when the stream or the plugin goes away it reports the devices down and
// waits for the plugin to come back (a new generation) or, when the plugin
// is still up, for a backoff, then configures again if the plugin restarted.
func (in *pluginInput) run(ctx context.Context, emit func(model.Record), life *input.Lifecycle, changes <-chan uint64) {
	var (
		backoff = driverkit.NewBackoff(in.clk, rand.Float64)
		warned  = map[string]bool{}
		down    bool
		busyAt  time.Time // first refusal of the current streak, zero when none
		lastGen uint64
	)

	for ctx.Err() == nil {
		conn, gen := in.h.Conn()
		if gen != lastGen {
			lastGen, busyAt = gen, time.Time{}
		}
		if conn == nil {
			in.goDown(&down, emit)
			if !in.wait(ctx, changes, nil) {
				return
			}

			continue
		}

		if err := in.ensureConfigured(ctx, conn, gen); err != nil {
			if ctx.Err() != nil {
				return
			}
			in.goDown(&down, emit)

			var refused *ConfigRefusedError
			if errors.As(err, &refused) {
				// Decision H6: valid when applied, so no rollback; the input
				// stays down until the plugin restarts again.
				in.log.Warn("plugin refused the configuration after a restart, the input stays down",
					"problems", strings.Join(refused.Messages("inputs."+in.cfg.ID), "; "))
				if !in.wait(ctx, changes, nil) {
					return
				}

				continue
			}
			in.log.Warn("plugin input could not be configured, retrying", "error", err.Error())
			if !in.wait(ctx, changes, backoff) {
				return
			}

			continue
		}

		received, err := in.stream(ctx, conn, emit, life, &down, warned)
		if ctx.Err() != nil {
			return
		}
		if received {
			backoff.Reset()
			busyAt = time.Time{}
		}
		if !received && status.Code(err) == codes.AlreadyExists {
			// The previous stream of the instance is still unwinding in the
			// plugin: not an outage until busyBound has passed.
			if busyAt.IsZero() {
				busyAt = in.clk.Now()
			}
			if in.clk.Now().Sub(busyAt) < busyBound {
				if in.clk.Sleep(ctx, busyWait) != nil {
					return
				}

				continue
			}
		}
		in.goDown(&down, emit)
		in.log.Warn("plugin input stream ended", "code", status.Code(err).String())
		if !in.wait(ctx, changes, backoff) {
			return
		}
	}
}

// wait blocks until the plugin has a new generation, the backoff (when
// given) elapses, or ctx ends; it returns false when ctx ended.
func (in *pluginInput) wait(ctx context.Context, changes <-chan uint64, backoff *driverkit.Backoff) bool {
	var elapsed <-chan time.Time
	if backoff != nil {
		timer := in.clk.NewTimer(backoff.Next())
		defer timer.Stop()
		elapsed = timer.C()
	}

	select {
	case <-ctx.Done():
		return false
	case <-changes:
		return true
	case <-elapsed:
		return true
	}
}

// goDown reports the input down once per outage: not connected, and one bad
// sample per point with the zero value of its kind. The devices die after
// the death tolerance through the lifecycle.
func (in *pluginInput) goDown(down *bool, emit func(model.Record)) {
	if *down {
		return
	}
	*down = true
	in.health.SetConnected(false)

	now := in.clk.Now().UTC()
	for _, d := range in.cfg.Devices {
		for _, p := range d.Points {
			priority := p.Priority
			if !priority.Valid() {
				priority = model.Critical
			}
			emit(model.SampleRecord(model.Sample{
				Device: d.Device, Signal: p.Signal, TS: now,
				Value: zeroValue(p.Type), Quality: model.Bad, Priority: priority,
			}))
		}
	}
}

// zeroValue is the zero of the kind of a point type, the value of a bad
// sample; a point without a type is a float.
func zeroValue(pointType string) model.Value {
	switch pointType {
	case "bool":
		return model.BoolValue(false)
	case "int16", "uint16", "int32", "uint32", "int64", "uint64":
		return model.IntValue(0)
	case "string":
		return model.StringValue("")
	default:
		return model.Value{Kind: model.KindFloat}
	}
}

// stream opens the one Subscribe stream of the instance and handles its
// events until it ends; received tells whether it delivered any event.
func (in *pluginInput) stream(ctx context.Context, conn *grpc.ClientConn, emit func(model.Record), life *input.Lifecycle, down *bool, warned map[string]bool) (received bool, err error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := pb.NewInputClient(conn).Subscribe(streamCtx, &pb.SubscribeRequest{InstanceId: in.cfg.ID})
	if err != nil {
		return false, err
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			return received, err
		}
		received = true
		*down = false
		in.handle(event, emit, life, warned)
	}
}

// handle applies one event of the stream. A sample is a proof that its
// device answered unless its quality is bad; the lifecycle emits the birth
// of a device that was dead before the sample goes out.
func (in *pluginInput) handle(event *pb.InputEvent, emit func(model.Record), life *input.Lifecycle, warned map[string]bool) {
	switch e := event.GetEvent().(type) {
	case *pb.InputEvent_Sample:
		s, err := sampleFromWire(e.Sample)
		if err != nil {
			in.health.CountError(input.ReadErrorDecode)
			in.log.Warn("plugin sample dropped", "reason", err.Error())

			return
		}
		key := s.Device + "/" + s.Signal
		if !in.known[key] {
			if !warned[key] {
				warned[key] = true
				in.log.Warn("plugin sample dropped: the point is not configured", "device", s.Device, "signal", s.Signal)
			}

			return
		}
		if s.Quality != model.Bad {
			life.Read(s.Device)
		}
		emit(model.SampleRecord(s))
	case *pb.InputEvent_Answered:
		life.Read(e.Answered.GetDevice())
	case *pb.InputEvent_Health:
		in.health.SetConnected(e.Health.GetConnected())
		if kind := e.Health.GetReadError(); slices.Contains(input.ReadErrorKinds, kind) {
			in.health.CountError(kind)
		}
	}
}

// sampleFromWire converts and validates a sample of the plugin. The error
// text is fixed.
func sampleFromWire(wire *pb.Sample) (model.Sample, error) {
	if wire == nil {
		return model.Sample{}, errors.New("empty sample")
	}
	s := model.Sample{
		Device:   wire.GetDevice(),
		Signal:   wire.GetSignal(),
		Quality:  model.Quality(wire.GetQuality()),
		Priority: model.Priority(wire.GetPriority()),
	}
	if ts := wire.GetTs(); ts != nil {
		if ts.CheckValid() != nil {
			return model.Sample{}, errors.New("invalid timestamp")
		}
		s.TS = ts.AsTime()
	}

	switch v := wire.GetValue().GetKind().(type) {
	case *pb.Value_DoubleValue:
		s.Value = model.Value{Kind: model.KindFloat, Float: v.DoubleValue}
	case *pb.Value_IntValue:
		s.Value = model.IntValue(v.IntValue)
	case *pb.Value_BoolValue:
		s.Value = model.BoolValue(v.BoolValue)
	case *pb.Value_StringValue:
		s.Value = model.StringValue(v.StringValue)
	default:
		return model.Sample{}, errors.New("sample without value")
	}
	if err := s.Validate(); err != nil {
		return model.Sample{}, errors.New("invalid sample")
	}

	return s, nil
}
