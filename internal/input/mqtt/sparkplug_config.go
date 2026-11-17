package mqtt

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/getlaraplate/laraedge/internal/config"
)

// SparkplugNamespace is the first topic level of Sparkplug B (spec section 7).
const SparkplugNamespace = "spBv1.0"

const sparkplugAddressShape = "address: want <group>/<edge_node>[/<device>]|<metric>"

// SparkplugAddress is a parsed point address under format sparkplug_b:
// <group>/<edge_node>[/<device>]|<metric>.
type SparkplugAddress struct {
	Group string
	Node  string
	// Device is the Sparkplug device; empty for a metric of the node.
	Device string
	// Metric is the metric name; it may contain / and |.
	Metric string
}

// ParseSparkplugAddress parses an address, split at the first |. The left part
// has two or three levels, each non-empty and free of +, # and /; the metric
// is any non-empty string. The errors are fixed texts, never the address.
func ParseSparkplugAddress(s string) (SparkplugAddress, error) {
	if s == "" {
		return SparkplugAddress{}, errors.New("address: required")
	}
	left, metric, ok := strings.Cut(s, "|")
	if !ok || metric == "" {
		return SparkplugAddress{}, errors.New(sparkplugAddressShape)
	}
	levels := strings.Split(left, "/")
	if len(levels) < 2 || len(levels) > 3 {
		return SparkplugAddress{}, errors.New(sparkplugAddressShape)
	}
	for _, level := range levels {
		if level == "" || strings.ContainsAny(level, "+#") {
			return SparkplugAddress{}, errors.New(sparkplugAddressShape)
		}
	}
	a := SparkplugAddress{Group: levels[0], Node: levels[1], Metric: metric}
	if len(levels) == 3 {
		a.Device = levels[2]
	}

	return a, nil
}

// SparkplugTarget is a node (group and edge node) or a Sparkplug device
// (group, edge node and device).
type SparkplugTarget struct {
	Group  string
	Node   string
	Device string
}

// Key is group/node[/device].
func (t SparkplugTarget) Key() string {
	if t.Device == "" {
		return t.Group + "/" + t.Node
	}

	return t.Group + "/" + t.Node + "/" + t.Device
}

// SparkplugPlan is what the Sparkplug router needs of the document.
type SparkplugPlan struct {
	// Targets holds the plan of every target points address.
	Targets map[SparkplugTarget]*TargetPlan
	// ByDevice maps an agent device to its target. A device with no points
	// has no entry.
	ByDevice map[string]SparkplugTarget
	// Filters are the subscriptions, sorted and without duplicates: one
	// spBv1.0/<group>/+/<edge_node>/# per node of every target, devices
	// included (decision S1b as amended). It matches the node's own topics and
	// those of every device under it, planned or not: seq is one counter per
	// node and its devices, so the session must see them all.
	Filters []string
}

// TargetPlan is the points of one agent device on one target.
type TargetPlan struct {
	Target SparkplugTarget
	// Device is the agent device.
	Device string
	// Points are the points by metric name.
	Points map[string]PlannedPoint
}

// PlanSparkplug groups the points of cfg by target (decision S2). Entries of
// one device name are one agent device: all their points together address one
// target and no metric twice. The errors are fixed texts naming positions,
// never an address.
func PlanSparkplug(cfg config.Input) (*SparkplugPlan, error) {
	type metricPos struct{ entry, point int }

	plan := &SparkplugPlan{
		Targets:  make(map[SparkplugTarget]*TargetPlan),
		ByDevice: make(map[string]SparkplugTarget),
	}
	owner := make(map[SparkplugTarget]int)
	metrics := make(map[string]map[string]metricPos)
	filters := make(map[string]bool)
	for di, dev := range cfg.Devices {
		dname := "devices[" + strconv.Itoa(di) + "]"
		for pi, p := range dev.Points {
			pname := dname + ".points[" + strconv.Itoa(pi) + "]"
			addr, err := ParseSparkplugAddress(p.Address)
			if err != nil {
				return nil, errors.New(pname + "." + err.Error())
			}
			target := SparkplugTarget{Group: addr.Group, Node: addr.Node, Device: addr.Device}
			tp := plan.Targets[target]
			switch have, known := plan.ByDevice[dev.Device]; {
			case !known:
				if other, taken := owner[target]; taken {
					return nil, errors.New(dname + ": same Sparkplug target as devices[" + strconv.Itoa(other) + "]")
				}
				tp = &TargetPlan{Target: target, Device: dev.Device, Points: make(map[string]PlannedPoint)}
				plan.Targets[target] = tp
				plan.ByDevice[dev.Device] = target
				owner[target] = di
				metrics[dev.Device] = make(map[string]metricPos)
			case have != target:
				return nil, errors.New(dname + ": points address more than one Sparkplug target")
			}
			if prev, dup := metrics[dev.Device][addr.Metric]; dup {
				return nil, errors.New(pname + ": same metric as devices[" + strconv.Itoa(prev.entry) + "].points[" + strconv.Itoa(prev.point) + "]")
			}
			metrics[dev.Device][addr.Metric] = metricPos{di, pi}
			tp.Points[addr.Metric] = PlannedPoint{Device: dev.Device, Signal: p.Signal, Point: p}

			filters[SparkplugNamespace+"/"+addr.Group+"/+/"+addr.Node+"/#"] = true
		}
	}
	for f := range filters {
		plan.Filters = append(plan.Filters, f)
	}
	slices.Sort(plan.Filters)

	return plan, nil
}
