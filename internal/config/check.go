package config

import (
	"encoding/json"
	"strings"
)

const notInBuild = "not available in this build"

// keystorePrefix marks references to the OS keystore, which this build does
// not provide.
const keystorePrefix = "secret://keystore/"

// CheckBuild is the second validation layer: it refuses, with "<path>: not
// available in this build", the parts of a valid document this build does not
// provide: drivers other than simulated (plugin drivers included), output
// types other than laraplate-http, secret://keystore/ references, transforms
// (of points and of derived signals), derived signals and plugins. It returns nil when the build can apply doc.
func CheckBuild(doc Document) []string {
	var msgs []string
	add := func(p path) { msgs = append(msgs, p.msg(notInBuild)) }

	for i, in := range doc.Inputs {
		p := path{"inputs"}.index(i)
		if in.Driver != DriverSimulated {
			add(p.key("driver"))
		}
		var conn any
		if err := json.Unmarshal(in.Connection, &conn); err == nil {
			walkSecrets(conn, p.key("connection"), func(rp path, ref string) {
				if strings.HasPrefix(ref, keystorePrefix) {
					add(rp)
				}
			})
		}
		for d, dev := range in.Devices {
			dp := p.key("devices").index(d)
			for k, pt := range dev.Points {
				if len(pt.Transforms) > 0 {
					add(dp.key("points").index(k).key("transforms"))
				}
			}
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
		if out.Type != OutputLaraplateHTTP {
			add(p.key("type"))
		}
		if strings.HasPrefix(out.Token, keystorePrefix) {
			add(p.key("token"))
		}
	}
	for i := range doc.Plugins {
		add(path{"plugins"}.index(i))
	}
	return msgs
}
