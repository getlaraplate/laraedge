package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getlaraplate/laraedge-sdk/handshake"
	"github.com/getlaraplate/laraedge-sdk/manifest"
	pb "github.com/getlaraplate/laraedge-sdk/pluginv1"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
)

// Restart reasons, the reason label of the restart counters: why the
// previous run or start attempt of the plugin ended.
const (
	// ReasonExit: the process exited, or a start attempt failed.
	ReasonExit = "exit"
	// ReasonHealth: three health checks in a row failed.
	ReasonHealth = "health"
	// ReasonMemory: the process used more than max_memory.
	ReasonMemory = "memory"
	// ReasonHash: the binary or plugin.json on disk no longer matches its hash.
	ReasonHash = "hash"
)

// State is the life state of a plugin host.
type State string

// States of a plugin host.
const (
	StateStarting State = "starting"
	StateUp       State = "up"
	StateDown     State = "down"
	StateStopped  State = "stopped"
)

// Deadlines and periods of spec section 6.
const (
	handshakeTimeout = 10 * time.Second
	infoTimeout      = 5 * time.Second
	healthEvery      = 5 * time.Second
	healthTimeout    = 2 * time.Second
	healthFailures   = 3
	memoryEvery      = 10 * time.Second
	stableAfter      = time.Minute
	shutdownDeadline = 5 * time.Second
	killAfter        = 5 * time.Second
)

// protocols are the plugin protocol versions the agent offers.
var protocols = []string{"v1"}

var (
	errHash         = errors.New("the plugin files on disk do not match their hashes")
	errExited       = errors.New("the plugin process exited")
	errUnhealthy    = errors.New("the plugin failed three health checks in a row")
	errMemory       = errors.New("the plugin used more memory than max_memory")
	errNotServing   = errors.New("the plugin health check did not answer SERVING")
	errInfoMismatch = errors.New("the plugin Info does not match plugin.json")
	errStopped      = errors.New("the plugin host is stopped")
	// errMemoryUnsupported is returned by processMemory where memory cannot be read.
	errMemoryUnsupported = errors.New("reading the memory of a process is not supported on this platform")
)

// HostOptions are the dependencies of a Host.
type HostOptions struct {
	// DataDir is the agent data directory; the plugin socket lives in
	// DataDir/run/plugins/<id>/.
	DataDir string
	// Clock times every deadline, check and backoff of the host.
	Clock clock.Clock
	// Log receives the host events and the plugin output, with plugin=<id>.
	Log *slog.Logger
	// Rand draws the backoff jitter in [0, 1]; nil uses math/rand/v2.
	Rand func() float64
}

// Status is a snapshot of a host for /status and the metrics.
type Status struct {
	State State
	// Pid is the plugin process id while it is up, else 0.
	Pid int
	// Restarts counts every restart; RestartsByReason splits it by reason.
	Restarts         uint64
	RestartsByReason map[string]uint64
	// LastError is the last reason the plugin went down or failed to start.
	LastError string
}

// Host starts and supervises the process of one plugin: it verifies the
// hashes before every launch, completes the handshake, checks Info against
// plugin.json, polls health and memory, restarts with backoff and stops the
// process in the order Shutdown, SIGTERM, kill.
type Host struct {
	spec      Spec
	clk       clock.Clock
	log       *slog.Logger
	socketDir string
	limiter   *outputLimiter
	backoff   *driverkit.Backoff // owned by Start, then by the supervise goroutine
	changes   chan uint64

	mu         sync.Mutex
	state      State
	run        *running
	generation uint64
	// watchers are the subscriptions of Watch, notified like changes.
	watchers  map[chan uint64]struct{}
	restarts  map[string]uint64
	lastError string
	binarySum string
	started   bool
	// cancelStart cancels the launch of a Start in flight; startDone is
	// closed when that Start returns.
	cancelStart context.CancelFunc
	startDone   chan struct{}
	// cancel ends the supervision; done is closed when it has ended.
	cancel context.CancelFunc
	done   chan struct{}
	// stopping is closed when the one Stop that does the work has finished,
	// with stopErr as its result.
	stopping chan struct{}
	stopErr  error
}

// running is one launched plugin process and its connection.
type running struct {
	proc           *handshake.Process
	conn           *grpc.ClientConn
	stdout, stderr *lineWriter
	exited         chan struct{}
	binarySum      string
	upSince        time.Time
}

// NewHost returns a host for spec; it starts nothing until Start.
func NewHost(spec Spec, opts HostOptions) *Host {
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real()
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("plugin", spec.ID)
	draw := opts.Rand
	if draw == nil {
		draw = rand.Float64
	}

	return &Host{
		spec:      spec,
		clk:       clk,
		log:       log,
		socketDir: filepath.Join(opts.DataDir, "run", "plugins", spec.ID),
		limiter:   newOutputLimiter(clk, log),
		backoff:   driverkit.NewBackoff(clk, draw),
		changes:   make(chan uint64, 1),
		state:     StateDown,
		restarts:  map[string]uint64{},
	}
}

// Start launches the plugin once and, when it is up, supervises it until
// Stop. An error means the plugin could not be started (a document start
// failure); nothing keeps running then. A Stop during Start cancels the
// launch and waits for Start; Start after Stop is refused.
func (h *Host) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.stopping != nil {
		h.mu.Unlock()

		return errStopped
	}
	if h.started {
		h.mu.Unlock()

		return errors.New("plugin host already started")
	}
	launchCtx, cancelLaunch := context.WithCancel(ctx)
	startDone := make(chan struct{})
	h.started = true
	h.state = StateStarting
	h.cancelStart, h.startDone = cancelLaunch, startDone
	h.mu.Unlock()
	defer close(startDone)
	defer cancelLaunch()

	run, err := h.launch(launchCtx)

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.stopping != nil {
		// Stop waits for this Start and then stops run in its sequence.
		h.run = run

		return errStopped
	}
	if err != nil {
		h.state = StateDown
		h.lastError = err.Error()

		return err
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	h.cancel, h.done = cancel, done
	h.upLocked(run)
	go h.supervise(loopCtx, run, done)

	return nil
}

// Conn returns the connection to the plugin and its generation; the
// connection is nil while the plugin is not up.
func (h *Host) Conn() (*grpc.ClientConn, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.state != StateUp || h.run == nil {
		return nil, h.generation
	}

	return h.run.conn, h.generation
}

// Changes delivers the new generation after every successful start or
// restart; it holds only the latest one.
func (h *Host) Changes() <-chan uint64 { return h.changes }

// Watch returns a channel of its own that, like Changes, delivers the latest
// generation after every successful start or restart, and a function that
// ends the subscription. It lets every instance of the plugin observe a
// restart, which the single Changes channel cannot. Subscribe before reading
// Conn: a generation announced before Watch is not delivered.
func (h *Host) Watch() (<-chan uint64, func()) {
	ch := make(chan uint64, 1)

	h.mu.Lock()
	if h.watchers == nil {
		h.watchers = map[chan uint64]struct{}{}
	}
	h.watchers[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(h.watchers, ch)
		h.mu.Unlock()
	}
}

// BinarySHA256 returns the hash of the binary verified at the last
// successful launch, empty before the first.
func (h *Host) BinarySHA256() string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.binarySum
}

// Status returns a snapshot of the host.
func (h *Host) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()

	s := Status{State: h.state, LastError: h.lastError, RestartsByReason: make(map[string]uint64, len(h.restarts))}
	if h.run != nil && h.state == StateUp {
		s.Pid = h.run.proc.Pid
	}
	for reason, n := range h.restarts {
		s.RestartsByReason[reason] = n
		s.Restarts += n
	}

	return s
}

// Stop ends the supervision and stops the process: Shutdown with a 5 s
// deadline, then SIGTERM, then kill 5 s later (on Windows kill right after
// the deadline), and removes the socket directory. When ctx ends first the
// process is killed at once and ctx.Err() is returned. Exactly one call does
// the work; every other call waits for it and returns its result.
func (h *Host) Stop(ctx context.Context) error {
	h.mu.Lock()
	if stopping := h.stopping; stopping != nil {
		h.mu.Unlock()
		<-stopping

		h.mu.Lock()
		defer h.mu.Unlock()

		return h.stopErr
	}
	stopping := make(chan struct{})
	h.stopping = stopping
	cancelStart, startDone := h.cancelStart, h.startDone
	h.mu.Unlock()

	if cancelStart != nil {
		cancelStart()
		<-startDone
	}

	h.mu.Lock()
	cancel, done, started := h.cancel, h.done, h.started
	h.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}

	h.mu.Lock()
	run := h.run
	h.run = nil
	h.mu.Unlock()

	if run != nil {
		h.shutdown(ctx, run)
	}
	h.limiter.report(true)
	if started {
		_ = os.RemoveAll(h.socketDir)
	}

	h.mu.Lock()
	h.state = StateStopped
	h.stopErr = ctx.Err()
	err := h.stopErr
	h.mu.Unlock()
	close(stopping)

	return err
}

// supervise watches the running plugin and restarts it until ctx ends.
func (h *Host) supervise(ctx context.Context, run *running, done chan<- struct{}) {
	defer close(done)

	for {
		reason, cause := h.watch(ctx, run)
		if reason == "" {
			return
		}
		if h.clk.Now().Sub(run.upSince) >= stableAfter {
			h.backoff.Reset()
		}
		h.down(reason, cause)
		h.teardown(run)

		if run = h.restart(ctx); run == nil {
			return
		}
	}
}

// restart waits the backoff and launches until a launch succeeds; it
// returns nil when ctx ends first.
func (h *Host) restart(ctx context.Context) *running {
	for {
		if err := h.backoff.Wait(ctx); err != nil {
			return nil
		}
		h.setState(StateStarting)

		run, err := h.launch(ctx)
		if err == nil {
			h.up(run)

			return run
		}
		if ctx.Err() != nil {
			return nil
		}
		reason := ReasonExit
		if errors.Is(err, errHash) {
			reason = ReasonHash
		}
		h.down(reason, err)
	}
}

// watch waits until the running plugin must be restarted and returns why,
// or "" when ctx ends.
func (h *Host) watch(ctx context.Context, run *running) (string, error) {
	health := h.clk.NewTimer(healthEvery)
	var (
		memory  clock.Timer
		memoryC <-chan time.Time
	)
	if h.spec.MaxMemory > 0 {
		memory = h.clk.NewTimer(memoryEvery)
		memoryC = memory.C()
	}
	defer func() {
		health.Stop()
		if memory != nil {
			memory.Stop()
		}
	}()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return "", nil
		case <-run.exited:
			return ReasonExit, errExited
		case <-health.C():
			h.limiter.report(false)
			if err := h.checkHealth(ctx, run.conn); err != nil {
				if ctx.Err() != nil {
					return "", nil
				}
				failures++
				h.log.Warn("plugin health check failed", "failures", failures)
				if failures >= healthFailures {
					return ReasonHealth, errUnhealthy
				}
			} else {
				failures = 0
				h.log.Debug("plugin health check passed")
			}
			health = h.clk.NewTimer(healthEvery)
		case <-memoryC:
			used, err := processMemory(run.proc.Pid)
			switch {
			case errors.Is(err, errMemoryUnsupported):
				h.log.Warn("max_memory is not enforced: " + err.Error())
				memoryC = nil

				continue
			case err == nil && used > h.spec.MaxMemory:
				h.log.Warn("plugin memory above max_memory", "memory_bytes", used)

				return ReasonMemory, errMemory
			case err == nil:
				h.log.Debug("plugin memory checked", "memory_bytes", used)
			}
			memory = h.clk.NewTimer(memoryEvery)
			memoryC = memory.C()
		}
	}
}

// launch verifies the files, starts the process and completes the start
// sequence: handshake, connection, Info against plugin.json, a health check.
func (h *Host) launch(ctx context.Context) (*running, error) {
	binarySum, manifestRaw, err := h.verifyFiles()
	if err != nil {
		return nil, err
	}

	stdout := newLineWriter("stdout", h.limiter, h.log)
	stderr := newLineWriter("stderr", h.limiter, h.log)
	proc, err := handshake.Launch(ctx, handshake.LaunchConfig{
		Path:      h.spec.Path,
		Dir:       h.spec.Dir,
		SocketDir: h.socketDir,
		Protocols: protocols,
		Env:       minimalEnv(),
		Stdout:    stdout,
		Stderr:    stderr,
		Timeout:   handshakeTimeout,
	})
	if err != nil {
		stderr.flush()

		return nil, launchError(ctx, err)
	}

	run := &running{proc: proc, stdout: stdout, stderr: stderr, exited: make(chan struct{}), binarySum: binarySum}
	go func() {
		_ = proc.Wait()
		close(run.exited)
	}()
	if err := h.connect(ctx, run, manifestRaw); err != nil {
		h.teardown(run)

		return nil, err
	}
	run.upSince = h.clk.Now()

	return run, nil
}

// verifyFiles hashes the binary that will run and plugin.json, immediately
// before the launch, and returns the binary hash and the plugin.json bytes.
func (h *Host) verifyFiles() (string, []byte, error) {
	binarySum, err := HashFile(h.spec.Path)
	if err != nil {
		return "", nil, fmt.Errorf("%w: path: the plugin binary cannot be read", errHash)
	}
	if !strings.EqualFold(binarySum, h.spec.SHA256) {
		return "", nil, fmt.Errorf("%w: sha256: does not match the plugin binary", errHash)
	}
	raw, err := os.ReadFile(filepath.Join(h.spec.Dir, ManifestFile))
	if err != nil {
		return "", nil, fmt.Errorf("%w: plugin.json cannot be read", errHash)
	}
	if !strings.EqualFold(SumHex(raw), h.spec.ManifestSHA256) {
		return "", nil, fmt.Errorf("%w: manifest_sha256: does not match plugin.json", errHash)
	}

	return binarySum, raw, nil
}

// connect dials the plugin, checks Info and a first health check.
func (h *Host) connect(ctx context.Context, run *running, manifestRaw []byte) error {
	dialCtx, release := h.deadline(ctx, handshakeTimeout)
	conn, err := run.proc.Dial(dialCtx)
	release()
	if err != nil {
		return errors.New("the plugin did not accept the gRPC connection")
	}
	run.conn = conn

	infoCtx, release := h.deadline(ctx, infoTimeout)
	info, err := pb.NewPluginClient(conn).Info(infoCtx, &pb.InfoRequest{})
	release()
	if err != nil {
		return errors.New("the plugin did not answer Info")
	}
	want, err := manifest.Canonical(manifestRaw)
	if err != nil {
		return errInfoMismatch
	}
	got, err := manifest.Canonical(info.GetManifest())
	if err != nil || !bytes.Equal(got, want) ||
		info.GetName() != h.spec.Manifest.Name || info.GetVersion() != h.spec.Manifest.Version {
		return errInfoMismatch
	}

	return h.checkHealth(ctx, conn)
}

// checkHealth calls the standard health Check with its 2 s deadline.
func (h *Host) checkHealth(ctx context.Context, conn *grpc.ClientConn) error {
	checkCtx, release := h.deadline(ctx, healthTimeout)
	defer release()

	resp, err := healthpb.NewHealthClient(conn).Check(checkCtx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return fmt.Errorf("%w: no answer", errNotServing)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return errNotServing
	}

	return nil
}

// shutdown stops a running plugin: Shutdown and 5 s to exit, then SIGTERM
// and 5 s more (not on Windows), then kill.
func (h *Host) shutdown(ctx context.Context, run *running) {
	timer := h.clk.NewTimer(shutdownDeadline)
	h.log.Info("plugin stopping")
	rpcCtx, cancelRPC := context.WithCancel(ctx)
	rpcDone := make(chan struct{})
	go func() {
		defer close(rpcDone)
		// The answer does not matter: UNAVAILABLE or EOF also mean the plugin heard it.
		_, _ = pb.NewPluginClient(run.conn).Shutdown(rpcCtx, &pb.ShutdownRequest{Deadline: durationpb.New(shutdownDeadline)})
	}()
	exited := h.waitExit(ctx, run, timer)
	cancelRPC()
	<-rpcDone

	if !exited && ctx.Err() == nil && runtime.GOOS != "windows" {
		h.log.Warn("plugin did not stop after Shutdown, sending SIGTERM")
		_ = run.proc.Signal(syscall.SIGTERM)
		exited = h.waitExit(ctx, run, h.clk.NewTimer(killAfter))
	}
	if !exited {
		h.log.Warn("plugin did not stop in time, killing it")
	}
	h.teardown(run)
}

// waitExit reports whether the process exits before timer fires or ctx ends.
func (h *Host) waitExit(ctx context.Context, run *running, timer clock.Timer) bool {
	defer timer.Stop()

	select {
	case <-run.exited:
		return true
	case <-timer.C():
		return false
	case <-ctx.Done():
		return false
	}
}

// teardown closes the connection, kills the process if it still runs and
// waits until it is reaped and its output logged.
func (h *Host) teardown(run *running) {
	if run.conn != nil {
		_ = run.conn.Close()
	}
	_ = run.proc.Kill()
	<-run.exited
	run.stdout.flush()
	run.stderr.flush()
}

// up records a running plugin and announces its new generation.
func (h *Host) up(run *running) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.upLocked(run)
}

func (h *Host) upLocked(run *running) {
	h.run = run
	h.state = StateUp
	h.binarySum = run.binarySum
	h.generation++
	select {
	case <-h.changes:
	default:
	}
	h.changes <- h.generation
	for ch := range h.watchers {
		select {
		case <-ch:
		default:
		}
		ch <- h.generation
	}
	h.log.Info("plugin started", "pid", run.proc.Pid)
}

// down records that the plugin is down and will be restarted for reason.
func (h *Host) down(reason string, cause error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.run = nil
	h.state = StateDown
	h.restarts[reason]++
	h.lastError = cause.Error()
	h.log.Warn("plugin down, restarting", "reason", reason, "error", cause.Error())
}

func (h *Host) setState(s State) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.state = s
}

// deadline returns a context cancelled after d on the host clock.
func (h *Host) deadline(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	timer := h.clk.NewTimer(d)
	released := make(chan struct{})
	go func() {
		select {
		case <-timer.C():
			cancel(context.DeadlineExceeded)
		case <-released:
		}
	}()

	var once sync.Once

	return ctx, func() {
		once.Do(func() {
			timer.Stop()
			close(released)
			cancel(context.Canceled)
		})
	}
}

// minimalEnv is the whole environment of a plugin besides the handshake
// variables: PATH, TZ and, on Windows, SystemRoot, when set. It never
// carries secrets.
func minimalEnv() []string {
	keys := []string{"PATH", "TZ"}
	if runtime.GOOS == "windows" {
		keys = append(keys, "SystemRoot")
	}
	env := []string{}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}

	return env
}

// launchError turns a handshake failure into fixed text: the SDK errors may
// quote the plugin's output.
func launchError(ctx context.Context, err error) error {
	var exit *handshake.ExitError
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &exit):
		return fmt.Errorf("the plugin exited before the handshake with exit code %d", exit.ExitCode)
	case errors.Is(err, handshake.ErrHandshakeTimeout):
		return errors.New("the plugin printed no handshake line within 10s")
	case errors.Is(err, handshake.ErrBadHandshakeLine):
		return errors.New("the plugin printed a bad handshake line")
	default:
		return fmt.Errorf("the plugin could not be started: %w", err)
	}
}
