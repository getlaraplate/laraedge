// This file holds the cross-document checks (unique ids and signals) and the secret-key helpers.

package config

import (
	"slices"
	"strings"
)

func (b *builder) uniqueIDs(doc Document) {
	check := func(list string, ids []string) {
		first := map[string]int{}
		for i, id := range ids {
			if id == "" {
				continue
			}
			if j, dup := first[id]; dup {
				b.add(path{list}.index(i).key("id"), "duplicate of "+path{list}.index(j).key("id").String())
				continue
			}
			first[id] = i
		}
	}
	ids := func(n int, id func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = id(i)
		}
		return out
	}
	check("inputs", ids(len(doc.Inputs), func(i int) string { return doc.Inputs[i].ID }))
	check("outputs", ids(len(doc.Outputs), func(i int) string { return doc.Outputs[i].ID }))
	check("plugins", ids(len(doc.Plugins), func(i int) string { return doc.Plugins[i].ID }))
	check("processors", ids(len(doc.Processors), func(i int) string { return doc.Processors[i].ID }))
}

// uniqueSignals refuses a (device, signal) pair defined twice, by points or
// derived signals, across all inputs.
func (b *builder) uniqueSignals(doc Document) {
	type key struct{ device, signal string }
	first := map[key]path{}
	see := func(k key, p path) {
		if k.device == "" || k.signal == "" {
			return
		}
		if q, dup := first[k]; dup {
			b.add(p, "duplicate of "+q.String()+" on the same device")
			return
		}
		first[k] = p
	}
	for i, in := range doc.Inputs {
		for d, dev := range in.Devices {
			dp := path{"inputs"}.index(i).key("devices").index(d)
			for k, pt := range dev.Points {
				see(key{dev.Device, pt.Signal}, dp.key("points").index(k).key("signal"))
			}
			for k, dv := range dev.Derived {
				see(key{dev.Device, dv.Signal}, dp.key("derived").index(k).key("signal"))
			}
		}
	}
}

// secretKeySuffixes mark the connection keys holding secret references, at
// any depth: a key equal to or ending with one of them, in any letter case.
// The schema's connection patternProperties states the same rule.
var secretKeySuffixes = []string{"password", "token", "secret", "api_key"}

// IsSecretKey reports whether key names a secret reference (see secretKeySuffixes).
func IsSecretKey(key string) bool {
	lower := strings.ToLower(key)
	return slices.ContainsFunc(secretKeySuffixes, func(suffix string) bool { return strings.HasSuffix(lower, suffix) })
}

// walkSecrets calls fn for every string held by a secret key in v, in key
// order, with its path.
func walkSecrets(v any, p path, fn func(path, string)) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if s, ok := x[k].(string); ok && IsSecretKey(k) {
				fn(p.key(k), s)
				continue
			}
			walkSecrets(x[k], p.key(k), fn)
		}
	case []any:
		for i, item := range x {
			walkSecrets(item, p.index(i), fn)
		}
	}
}
