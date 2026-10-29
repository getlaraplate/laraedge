package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/getlaraplate/laraedge/internal/bootstrap"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/modbus"
	"github.com/getlaraplate/laraedge/internal/input/opcua"
	"github.com/getlaraplate/laraedge/internal/input/simulated"
	"github.com/getlaraplate/laraedge/internal/logging"
	"github.com/getlaraplate/laraedge/internal/provider"
	"github.com/getlaraplate/laraedge/internal/secrets"
	"github.com/getlaraplate/laraedge/internal/status"
	"github.com/getlaraplate/laraedge/internal/supervisor"
	"github.com/getlaraplate/laraedge/internal/version"
	"github.com/getlaraplate/laraedge/internal/watchdog"
)

// agentDeps are what runAgent takes from its environment; tests replace them.
type agentDeps struct {
	// lookupEnv reads the environment for secret://env/ references and the
	// systemd notification variables.
	lookupEnv func(string) (string, bool)
	// clock drives every timer.
	clock clock.Clock
	// drivers replaces or adds to the built-in input drivers (tests).
	drivers map[string]input.Factory
	// logger receives the logs; nil means JSON on the logs writer of
	// runAgent at info.
	logger *slog.Logger
	// started, when set, receives the supervisor once it exists (tests).
	started func(*supervisor.Supervisor)
	// listening, when set, receives the address the status endpoint bound
	// (tests).
	listening func(addr string)
}

// productionDeps are the dependencies of the real agent.
func productionDeps() agentDeps {
	return agentDeps{
		lookupEnv: os.LookupEnv,
		clock:     clock.Real(),
		drivers:   map[string]input.Factory{},
	}
}

// runAgent runs the agent of the bootstrap file until ctx is done (SIGINT,
// SIGTERM), then stops the provider and the status endpoint and closes the
// supervisor, which syncs and closes every buffer. Each value received on
// reload (SIGHUP) reloads the bootstrap file: the outputs resolve their
// tokens again, the file provider rereads its file, and changed settings,
// which need a restart, are logged.
//
// Under systemd it sends READY=1 once a configuration is applied or the
// provider's first attempt ended, WATCHDOG=1 while it runs, until it
// returns, and STOPPING=1 when the shutdown starts. A status endpoint that
// cannot start (a non-loopback status_listen without status_token, a port
// in use) stops the agent at start. logs receives the JSON logs when
// deps.logger is nil; run passes stdout (spec 10.5).
func runAgent(ctx context.Context, bootstrapPath string, deps agentDeps, reload <-chan struct{}, logs io.Writer) (err error) {
	boot, err := bootstrap.Load(bootstrapPath)
	if err != nil {
		return err
	}

	log := deps.logger
	if log == nil {
		log = logging.New(logs, slog.LevelInfo)
	}

	if err := prepareDataDir(boot.DataDir); err != nil {
		return err
	}

	defer routeLibraryLogs(log)()

	// The watchdog pings until runAgent returns, closing the supervisor
	// included.
	wd := watchdog.New(watchdog.Config{LookupEnv: deps.lookupEnv, Clock: deps.clock, Logger: log})
	pingCtx, stopPings := context.WithCancel(context.Background())
	var pings sync.WaitGroup
	pings.Go(func() { _ = wd.Run(pingCtx) })
	defer func() {
		stopPings()
		pings.Wait()
	}()

	store := config.NewStore(boot.DataDir)
	resolver := secrets.NewResolver(secrets.WithLookupEnv(deps.lookupEnv))
	sup, err := supervisor.New(supervisor.Config{
		DataDir:    boot.DataDir,
		Store:      store,
		Drivers:    inputDrivers(boot, resolver, log, deps.drivers),
		Resolver:   resolver,
		ConfigDir:  configBaseDir(boot),
		Provider:   boot.Config.Provider,
		PluginsDir: boot.PluginsDir,
		Clock:      deps.clock,
		Logger:     log,
	})
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, sup.Close())
		log.Info("agent: stopped")
	}()

	fileReload := make(chan struct{}, 1)
	runProvider, err := newProvider(boot, sup, resolver, fileReload, deps.clock, log, wd.Ready)
	if err != nil {
		return err
	}

	srv, err := status.Listen(ctx, status.Config{
		Listen:   boot.StatusListen,
		TokenRef: boot.StatusToken,
		Resolver: resolver,
		AgentID:  boot.AgentID,
		Version:  version.String(),
		Source:   sup,
		Clock:    deps.clock,
		Logger:   log,
	})
	if err != nil {
		return err
	}
	serveCtx, stopServing := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := srv.Serve(serveCtx); err != nil {
			log.Error("agent: the status endpoint stopped", "error", err)
		}
	})
	if deps.listening != nil {
		deps.listening(srv.Addr().String())
	}

	if deps.started != nil {
		deps.started(sup)
	}
	log.Info("agent: starting", "agent_id", boot.AgentID, "version", version.String(), "provider", boot.Config.Provider, "data_dir", boot.DataDir, "status_listen", srv.Addr().String())

	provider.ApplyLastValid(ctx, store, sup, log)
	if sup.Status().Applied {
		wd.Ready()
	}

	reloadCtx, stopReloads := context.WithCancel(ctx)
	wg.Go(func() { handleReloads(reloadCtx, bootstrapPath, boot, sup, fileReload, reload, log) })

	err = runProvider(ctx)
	wd.Stopping()
	stopReloads()
	stopServing()
	wg.Wait()

	return err
}

// inputDrivers returns the input driver registry: the built-in drivers, then
// extra, which replaces a built-in of the same name. Drivers that read files
// named in the configuration resolve relative paths against the directory of
// the configuration file, which only the file provider has. The opcua driver
// keeps its PKI under the bootstrap's data_dir, forms its application URI
// from the agent id, resolves its passwords through resolver and warns
// through log (decision O13).
func inputDrivers(boot bootstrap.File, resolver opcua.SecretResolver, log *slog.Logger, extra map[string]input.Factory) map[string]input.Factory {
	drivers := map[string]input.Factory{
		config.DriverSimulated: simulated.Factory(configBaseDir(boot)),
		config.DriverModbus:    modbus.Factory(),
		config.DriverOPCUA: opcua.Factory(opcua.Options{
			DataDir:  boot.DataDir,
			AgentID:  boot.AgentID,
			Resolver: resolver,
			Logger:   log,
		}),
	}
	maps.Copy(drivers, extra)

	return drivers
}

// routeLibraryLogs sends the output of the standard log package, through
// which the OPC UA library logs, to l at debug with source=gopcua (decision
// O6), and returns the function that restores the previous output and
// flags.
func routeLibraryLogs(l *slog.Logger) (restore func()) {
	output, flags := stdlog.Writer(), stdlog.Flags()

	stdlog.SetFlags(0)
	stdlog.SetOutput(logging.StdlogBridge(l))

	return func() {
		stdlog.SetOutput(output)
		stdlog.SetFlags(flags)
	}
}

// configBaseDir returns the directory of the configuration file of the file
// provider, absolute, and "" for a provider that has no such file.
func configBaseDir(boot bootstrap.File) string {
	if boot.Config.Provider != bootstrap.ProviderFile {
		return ""
	}

	file := boot.Config.File
	if abs, err := filepath.Abs(file); err == nil {
		file = abs
	}

	return filepath.Dir(file)
}

// newProvider builds the provider the bootstrap names and returns its Run;
// attempted is called once the provider's first attempt ended.
func newProvider(boot bootstrap.File, sup *supervisor.Supervisor, resolver *secrets.Resolver, fileReload <-chan struct{}, clk clock.Clock, log *slog.Logger, attempted func()) (func(context.Context) error, error) {
	switch boot.Config.Provider {
	case bootstrap.ProviderFile:
		p, err := provider.NewFile(provider.FileConfig{
			Path:      boot.Config.File,
			Applier:   sup,
			Reload:    fileReload,
			Clock:     clk,
			Logger:    log,
			Attempted: attempted,
		})
		if err != nil {
			return nil, err
		}

		return p.Run, nil
	case bootstrap.ProviderLaraplate:
		p, err := provider.NewLaraplate(provider.LaraplateConfig{
			URL:          boot.Config.Laraplate.URL,
			Token:        boot.Config.Laraplate.Token,
			PollInterval: time.Duration(boot.Config.Laraplate.PollSeconds) * time.Second,
			Resolver:     resolver,
			Applier:      sup,
			DataDir:      boot.DataDir,
			Clock:        clk,
			Logger:       log,
			Attempted:    attempted,
		})
		if err != nil {
			return nil, err
		}

		return p.Run, nil
	default:
		return nil, fmt.Errorf("bootstrap: unknown provider %q", boot.Config.Provider)
	}
}

// handleReloads reloads the bootstrap file on every value of reload until
// ctx is done. A file that no longer loads is reported and changes nothing.
// Otherwise the outputs resolve their tokens again at once (token rotation,
// spec 10.6), the file provider rereads its document, and every changed
// setting is logged at warn: this build applies them at the next restart.
func handleReloads(ctx context.Context, path string, running bootstrap.File, sup *supervisor.Supervisor, fileReload chan<- struct{}, reload <-chan struct{}, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-reload:
		}

		next, err := bootstrap.Load(path)
		if err != nil {
			log.Error("agent: bootstrap reload refused; the running settings stay", "error", err)

			continue
		}

		for _, key := range changedSettings(running, next) {
			log.Warn("agent: bootstrap setting changed; it applies at the next restart", "key", key)
		}

		sup.ReloadCredentials()
		if running.Config.Provider == bootstrap.ProviderFile {
			select {
			case fileReload <- struct{}{}:
			default:
			}
		}
		log.Info("agent: bootstrap reloaded; credentials are resolved again")
	}
}

// changedSettings returns the bootstrap keys whose values differ. The values
// are never returned: a reference may be logged, a value must not.
func changedSettings(a, b bootstrap.File) []string {
	var keys []string
	check := func(key string, changed bool) {
		if changed {
			keys = append(keys, key)
		}
	}

	check("agent_id", a.AgentID != b.AgentID)
	check("data_dir", a.DataDir != b.DataDir)
	check("status_listen", a.StatusListen != b.StatusListen)
	check("status_token", a.StatusToken != b.StatusToken)
	check("plugins_dir", a.PluginsDir != b.PluginsDir)
	check("config.provider", a.Config.Provider != b.Config.Provider)
	check("config.file", a.Config.File != b.Config.File)
	check("config.laraplate.url", a.Config.Laraplate.URL != b.Config.Laraplate.URL)
	check("config.laraplate.token", a.Config.Laraplate.Token != b.Config.Laraplate.Token)
	check("config.laraplate.poll_seconds", a.Config.Laraplate.PollSeconds != b.Config.Laraplate.PollSeconds)

	return keys
}
