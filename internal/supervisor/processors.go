package supervisor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"

	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

// processorKeyPrefix keys the virtual input of a processor with the router.
// A colon is not allowed in an input id, so a processor's derived devices
// can never replace, or be removed with, the registration of an input that
// has the same id (ruling R8).
const processorKeyPrefix = "processor:"

// runningProcessor is a started processor. Its derived devices are a
// virtual input registered with the router through inst, keyed apart from
// the real inputs.
type runningProcessor struct {
	cfg    config.Processor
	proc   *plugin.Processor
	inst   *inputInstance
	cancel context.CancelFunc
}

// usedPlugins returns the ids of the plugins a component of doc uses: the
// plugin:<id> drivers of the inputs, the plugin of the processors and the
// plugin:<id> types of the outputs (decision Q6).
func usedPlugins(doc config.Document) map[string]bool {
	used := map[string]bool{}
	for _, in := range doc.Inputs {
		if id, ok := pluginOf(in.Driver); ok {
			used[id] = true
		}
	}
	for _, pr := range doc.Processors {
		used[pr.Plugin] = true
	}
	for _, out := range doc.Outputs {
		if id, ok := pluginOf(out.Type); ok {
			used[id] = true
		}
	}

	return used
}

// diffProcessors adds to p the processors of doc to start and the running
// ones to stop: a processor is kept when its section is unchanged and its
// plugin does not restart. The caller holds s.mu.
func (s *Supervisor) diffProcessors(doc config.Document, p *plan, restarted map[string]bool) {
	inDoc := map[string]bool{}
	for i, pr := range doc.Processors {
		inDoc[pr.ID] = true
		running := s.processors[pr.ID]
		if running != nil && bytes.Equal(running.cfg.Raw, pr.Raw) && !restarted[pr.Plugin] {
			p.kept++

			continue
		}
		p.startProcessors = append(p.startProcessors, i)
		if running != nil {
			p.stopProcessors = append(p.stopProcessors, pr.ID)
		}
	}
	for id := range s.processors {
		if !inDoc[id] {
			p.stopProcessors = append(p.stopProcessors, id)
		}
	}
}

// startProcessor starts processor cfg on its running plugin, within the
// start timeout. Its derived devices are registered with the router, and
// their births sent, on its first record or once Start returned. Only then
// is it offered the records of the inputs. A plugin refusing the section
// fails the start with a *plugin.ConfigRefusedError.
func (s *Supervisor) startProcessor(ctx context.Context, cfg config.Processor) error {
	h := s.pluginHosts()[cfg.Plugin]
	if h == nil {
		return errors.New("its plugin is not running")
	}

	proc := plugin.NewProcessor(h, cfg, s.resolver, s.clk, s.log)
	virtual := proc.Input()
	virtual.ID = processorKeyPrefix + cfg.ID
	inst := &inputInstance{router: s.router, cfg: virtual, live: true}

	lifeCtx, cancel := context.WithCancel(s.ctx)
	err := s.startBounded(ctx, cancel, func() error { return proc.Start(lifeCtx, inst.emit) }, func() {
		inst.retire()
		if err := proc.Close(); err != nil {
			s.log.Warn("supervisor: cannot close a processor that did not start", "processor", cfg.ID, "error", err)
		}
	})
	if err != nil {
		inst.retire()
		cancel()

		return err
	}

	inst.register()
	s.step("processor " + cfg.ID)

	s.mu.Lock()
	s.processors[cfg.ID] = &runningProcessor{cfg: cfg, proc: proc, inst: inst, cancel: cancel}
	delete(s.failedProc, cfg.ID)
	s.setTapsLocked()
	s.mu.Unlock()

	// The inputs registering from now on offer it their births; those
	// already running are offered theirs here, the supervisor's lock
	// released. A device registering in between may be offered twice.
	for _, ev := range s.router.Births() {
		proc.Offer(model.EventRecord(ev))
	}

	return nil
}

// stopProcessor stops offering records to processor id, retires its derived
// devices, then closes it, and returns its configuration.
func (s *Supervisor) stopProcessor(id string) config.Processor {
	s.mu.Lock()
	rp := s.processors[id]
	delete(s.processors, id)
	s.setTapsLocked()
	s.mu.Unlock()

	rp.inst.retire()
	rp.cancel()
	if err := rp.proc.Close(); err != nil {
		s.log.Warn("supervisor: cannot close a processor", "processor", id, "error", err)
	}

	return rp.cfg
}

// setTapsLocked publishes the running processors to the input instances.
// The caller holds s.mu.
func (s *Supervisor) setTapsLocked() {
	taps := make([]*plugin.Processor, 0, len(s.processors))
	for _, id := range sortedKeys(s.processors) {
		taps = append(taps, s.processors[id].proc)
	}
	s.taps.Store(&taps)
}

// newOutputSender returns the sender of plugin output cfg through its
// running plugin, its instance configured (ruling R6). A refusal or a
// secret that cannot be resolved is an error, a *plugin.ConfigRefusedError;
// a plugin that is down or does not answer is logged, and the output's
// Loop configures the instance at its first Send.
func (s *Supervisor) newOutputSender(ctx context.Context, pluginID string, cfg config.Output, log *slog.Logger) (*plugin.OutputSender, error) {
	h := s.pluginHosts()[pluginID]
	if h == nil {
		return nil, errors.New("its plugin is not running")
	}

	sender, err := plugin.NewOutputSender(h, cfg, s.resolver, s.clk, nil)
	if err != nil {
		return nil, err
	}
	if err := sender.Configure(ctx); err != nil {
		var refused *plugin.ConfigRefusedError
		if errors.As(err, &refused) || ctx.Err() != nil {
			sender.Close()

			return nil, err
		}
		log.Warn("supervisor: plugin output could not be configured at start, retrying", "error", err.Error())
	}

	return sender, nil
}
