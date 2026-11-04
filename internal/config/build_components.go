// This file holds the builder methods for inputs, outputs and processors, their secret and processor rules, and the transform chain.

package config

import (
	"encoding/json"
	"errors"
	stdpath "path"

	"github.com/getlaraplate/laraedge/internal/docrules"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/secrets"
)

func (b *builder) input(p path, v any) (Input, error) {
	var w wireInput
	raw, err := b.component(v, &w)
	if err != nil {
		return Input{}, err
	}
	in := Input{
		ID:             w.ID,
		Driver:         w.Driver,
		Connection:     w.Connection,
		DeathTolerance: b.duration(p.key("death_tolerance"), w.DeathTolerance),
		Raw:            raw,
	}
	walkSecrets(object(object(v)["connection"]), p.key("connection"), func(rp path, ref string) {
		if err := secrets.Check(ref); err != nil && !errors.Is(err, secrets.ErrUnavailable) {
			b.add(rp, secretMessage)
		}
	})
	for d, wd := range w.Devices {
		dp := p.key("devices").index(d)
		dev := Device{Device: wd.Device}
		for k, wp := range wd.Points {
			pp := dp.key("points").index(k)
			pt := Point{
				Signal:    wp.Signal,
				Address:   wp.Address,
				Read:      b.read(pp.key("read"), wp.Read),
				Priority:  priority(wp.Priority),
				Type:      wp.Type,
				ByteOrder: wp.ByteOrder,
				Unit:      wp.Unit,
			}
			pt.Transforms = transforms(wp.Transforms)
			dev.Points = append(dev.Points, pt)
		}
		for k, wdv := range wd.Derived {
			derived := Derived{
				Signal:     wdv.Signal,
				Type:       wdv.Type,
				Priority:   priority(wdv.Priority),
				Expr:       wdv.Expr,
				Transforms: transforms(wdv.Transforms),
			}
			if wdv.On.Interval != "" {
				derived.Interval = b.duration(dp.key("derived").index(k).key("on").key("interval"), wdv.On.Interval)
			}
			dev.Derived = append(dev.Derived, derived)
		}
		in.Devices = append(in.Devices, dev)
	}
	return in, nil
}

func (b *builder) output(p path, v any) (Output, error) {
	var w wireOutput
	raw, err := b.component(v, &w)
	if err != nil {
		return Output{}, err
	}
	out := Output{
		ID:    w.ID,
		Type:  w.Type,
		URL:   w.URL,
		Token: w.Token,
		Route: Route{Include: w.Route.Include},
		Batch: Batch{
			MaxSamples: w.Batch.MaxSamples,
			MaxBody:    b.size(p.key("batch").key("max_body"), w.Batch.MaxBody),
			MaxWait:    b.duration(p.key("batch").key("max_wait"), w.Batch.MaxWait),
		},
		Buffer: Buffer{
			MaxSize:        b.size(p.key("buffer").key("max_size"), w.Buffer.MaxSize),
			MinFreePercent: w.Buffer.MinFreePercent,
			FsyncInterval:  b.duration(p.key("buffer").key("fsync_interval"), w.Buffer.FsyncInterval),
		},
		Config: w.Config,
		Raw:    raw,
	}
	b.checkConfigSecrets(p.key("config"), object(v)["config"])
	if out.Batch.MaxBody > MaxBody {
		b.add(p.key("batch").key("max_body"), "must not exceed "+defaultMaxBodyText+" (the backend default of mes.machine.max_body_kb)")
	}
	if out.Buffer.MaxSize > 0 && out.Buffer.MaxSize < MinBufferMaxSize {
		b.add(p.key("buffer").key("max_size"), "must be at least "+minBufferSizeText+" (two segments of 16MiB)")
	}
	if out.Type == OutputLaraplateHTTP {
		if err := docrules.CheckURL(out.URL); err != nil {
			b.add(p.key("url"), err.Error())
		}
	}
	if out.Token != "" {
		if err := secrets.Check(out.Token); err != nil && !errors.Is(err, secrets.ErrUnavailable) {
			b.add(p.key("token"), secretMessage)
		}
	}
	for i, pattern := range out.Route.Include {
		if _, err := stdpath.Match(pattern, ""); err != nil {
			b.add(p.key("route").key("include").index(i), "invalid pattern")
		}
	}
	return out, nil
}

func (b *builder) processor(p path, v any) (Processor, error) {
	var w wireProcessor
	raw, err := b.component(v, &w)
	if err != nil {
		return Processor{}, err
	}
	pr := Processor{
		ID:             w.ID,
		Plugin:         w.Plugin,
		Observe:        w.Observe,
		DeathTolerance: b.duration(p.key("death_tolerance"), w.DeathTolerance),
		Config:         w.Config,
		Raw:            raw,
	}
	for _, wd := range w.Devices {
		dev := DerivedDevice{Device: wd.Device}
		for _, ws := range wd.Signals {
			dev.Signals = append(dev.Signals, DerivedSignal{Signal: ws.Signal, Type: ws.Type, Priority: ws.Priority, Unit: ws.Unit})
		}
		pr.Devices = append(pr.Devices, dev)
	}
	b.checkConfigSecrets(p.key("config"), object(v)["config"])
	return pr, nil
}

// checkConfigSecrets refuses, at its path, a value under a secret key of a
// plugin config object that is not a usable secret:// reference.
func (b *builder) checkConfigSecrets(p path, config any) {
	walkSecrets(config, p, func(rp path, ref string) {
		if err := secrets.Check(ref); err != nil && !errors.Is(err, secrets.ErrUnavailable) {
			b.add(rp, secretMessage)
		}
	})
}

// processorRules refuses observed devices no input has or a processor lists
// twice, and derived devices or signals that clash.
func (b *builder) processorRules(doc Document) {
	real := map[string]bool{}
	for _, in := range doc.Inputs {
		for _, dev := range in.Devices {
			real[dev.Device] = true
		}
	}
	derived := map[string]path{}
	for i, pr := range doc.Processors {
		pp := path{"processors"}.index(i)
		observed := map[string]int{}
		for k, name := range pr.Observe {
			switch j, dup := observed[name]; {
			case name == "":
			case dup:
				b.add(pp.key("observe").index(k), "duplicate of "+pp.key("observe").index(j).String())
			case !real[name]:
				b.add(pp.key("observe").index(k), "is not a device of any input")
				observed[name] = k
			default:
				observed[name] = k
			}
		}
		for d, dev := range pr.Devices {
			dp := pp.key("devices").index(d).key("device")
			switch q, dup := derived[dev.Device]; {
			case dev.Device == "":
			case real[dev.Device]:
				b.add(dp, "is the name of a device of an input")
			case dup:
				b.add(dp, "duplicate of "+q.String())
			default:
				derived[dev.Device] = dp
			}
			seen := map[string]int{}
			for s, sig := range dev.Signals {
				if j, dup := seen[sig.Signal]; dup && sig.Signal != "" {
					b.add(pp.key("devices").index(d).key("signals").index(s).key("signal"), "duplicate of "+pp.key("devices").index(d).key("signals").index(j).key("signal").String())
					continue
				}
				seen[sig.Signal] = s
			}
		}
	}
}

// transforms converts a chain of single-key objects.
func transforms(chain []map[string]json.RawMessage) []Transform {
	var out []Transform
	for _, step := range chain {
		for name, params := range step {
			out = append(out, Transform{Name: name, Params: params})
		}
	}
	return out
}

func priority(s string) model.Priority {
	if s == model.Bulk.String() {
		return model.Bulk
	}
	return model.Critical
}
