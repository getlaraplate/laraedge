// This file holds the plugin side of the supervisor: the running plugin, its host construction, start and stop, and the driver factory that resolves plugin:<id> inputs.

package supervisor

import (
	"context"
	"errors"
	"strings"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

// runningPlugin is a plugin with its host. Its spec is the one the host was
// built from: a document changing the entry (Raw) restarts it (decision H5).
type runningPlugin struct {
	spec plugin.Spec
	host *plugin.Host
}

// pluginOf returns the plugin id of a plugin driver, plugin:<id>.
func pluginOf(driver string) (string, bool) {
	return strings.CutPrefix(driver, plugin.DriverPrefix)
}

// newPlugin returns the plugin of spec, its host built and not started.
func (s *Supervisor) newPlugin(spec plugin.Spec) *runningPlugin {
	return &runningPlugin{spec: spec, host: plugin.NewHost(spec, plugin.HostOptions{
		DataDir: s.dataDir,
		RunDir:  s.runDir,
		Clock:   s.clk,
		Log:     s.log,
	})}
}

// pluginHosts returns the hosts of the running plugins, by id.
func (s *Supervisor) pluginHosts() map[string]*plugin.Host {
	s.mu.Lock()
	defer s.mu.Unlock()

	hosts := make(map[string]*plugin.Host, len(s.plugins))
	for id, rp := range s.plugins {
		hosts[id] = rp.host
	}

	return hosts
}

// factory returns the factory of driver: for plugin:<id> the input factory
// of the host of that id in hosts (decision H4), else the registry's. It
// returns a factory that refuses when there is none, which check prevents
// for the documents it accepts.
func (s *Supervisor) factory(driver string, hosts map[string]*plugin.Host) input.Factory {
	if id, ok := pluginOf(driver); ok {
		if h := hosts[id]; h != nil {
			return plugin.InputFactory(h, s.resolver)
		}

		return func(config.Input, clock.Clock) (input.Input, error) {
			return nil, errors.New("its plugin is not running")
		}
	}
	if f := s.drivers[driver]; f != nil {
		return f
	}

	return func(config.Input, clock.Clock) (input.Input, error) {
		return nil, errors.New("driver not available in this build")
	}
}

// startPlugin starts the host of rp and records it as running. A host that
// does not start is stopped, which removes what its launch left.
func (s *Supervisor) startPlugin(ctx context.Context, rp *runningPlugin) error {
	if err := rp.host.Start(ctx); err != nil {
		if serr := rp.host.Stop(context.Background()); serr != nil {
			s.log.Warn("supervisor: cannot stop a plugin that did not start", "plugin", rp.spec.ID, "error", serr)
		}

		return err
	}
	s.step("plugin " + rp.spec.ID)

	s.mu.Lock()
	s.plugins[rp.spec.ID] = rp
	delete(s.failedPlugins, rp.spec.ID)
	s.mu.Unlock()

	return nil
}

// stopPlugin stops the running plugin id and waits for its host's Stop,
// bounded by the stop deadline of spec section 6, and returns its spec. The
// inputs using it are stopped first.
func (s *Supervisor) stopPlugin(id string) plugin.Spec {
	s.mu.Lock()
	rp := s.plugins[id]
	delete(s.plugins, id)
	s.mu.Unlock()

	if err := rp.host.Stop(context.Background()); err != nil {
		s.log.Warn("supervisor: cannot stop a plugin", "plugin", id, "error", err)
	}

	return rp.spec
}
