package config

import (
	"encoding/json"
	"strings"
)

const notInBuild = "not available in this build"

// keystorePrefix marks references to the OS keystore, which this build does
// not provide.
const keystorePrefix = "secret://keystore/"

// pluginPrefix starts the driver and output type names a plugin provides.
const pluginPrefix = "plugin:"

// CheckBuild is the second validation layer: it refuses, with "<path>: not
// available in this build", the parts of a valid document this build does
// not provide: drivers other than simulated, modbus, opcua and plugin:<id>,
// output types other than laraplate-http and plugin:<id>, secret://keystore/ references, and derived signals
// with their transforms. Plugins, their drivers, processors and outputs are provided. Point
// transforms are provided; their chains are type-checked by
// transform.CheckDocument, which config cannot call (import cycle). It
// returns nil when the build can apply doc.
func CheckBuild(doc Document) []string {
	var msgs []string
	add := func(p path) { msgs = append(msgs, p.msg(notInBuild)) }

	for i, in := range doc.Inputs {
		p := path{"inputs"}.index(i)
		if in.Driver != DriverSimulated && in.Driver != DriverModbus && in.Driver != DriverOPCUA && !strings.HasPrefix(in.Driver, pluginPrefix) {
			add(p.key("driver"))
		}
		checkKeystore(in.Connection, p.key("connection"), add)
		for d, dev := range in.Devices {
			dp := p.key("devices").index(d)
			if len(dev.Derived) > 0 {
				add(dp.key("derived"))
			}
			for k, dv := range dev.Derived {
				if len(dv.Transforms) > 0 {
					add(dp.key("derived").index(k).key("transforms"))
				}
			}
		}
	}
	for i, out := range doc.Outputs {
		p := path{"outputs"}.index(i)
		if out.Type != OutputLaraplateHTTP && !strings.HasPrefix(out.Type, pluginPrefix) {
			add(p.key("type"))
		}
		if strings.HasPrefix(out.Token, keystorePrefix) {
			add(p.key("token"))
		}
		checkKeystore(out.Config, p.key("config"), add)
	}
	for i, pr := range doc.Processors {
		checkKeystore(pr.Config, path{"processors"}.index(i).key("config"), add)
	}
	return msgs
}

// checkKeystore reports, through add, every keystore reference held by a
// secret key of a connection or config object.
func checkKeystore(raw json.RawMessage, p path, add func(path)) {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return
	}
	walkSecrets(v, p, func(rp path, ref string) {
		if strings.HasPrefix(ref, keystorePrefix) {
			add(rp)
		}
	})
}
