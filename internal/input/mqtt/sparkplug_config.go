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
	// ByDevice maps an agent device to its target.
	ByDevice map[string]SparkplugTarget
	// Filters are the subscriptions, sorted and without duplicates: the node
	// of every target, devices included (decision S1b), and each device target.
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

// PlanSparkplug groups the points of cfg by target (decision S2). The errors
// are fixed texts naming positions, never an address.
func PlanSparkplug(cfg config.Input) (*SparkplugPlan, error) {
	plan := &SparkplugPlan{
		Targets:  make(map[SparkplugTarget]*TargetPlan),
		ByDevice: make(map[string]SparkplugTarget),
	}
	owner := make(map[SparkplugTarget]int)
	filters := make(map[string]bool)
	for di, dev := range cfg.Devices {
		dname := "devices[" + strconv.Itoa(di) + "]"
		var tp *TargetPlan
		first := make(map[string]int)
		for pi, p := range dev.Points {
			pname := dname + ".points[" + strconv.Itoa(pi) + "]"
			addr, err := ParseSparkplugAddress(p.Address)
			if err != nil {
				return nil, errors.New(pname + "." + err.Error())
			}
			target := SparkplugTarget{Group: addr.Group, Node: addr.Node, Device: addr.Device}
			if tp == nil {
				if other, taken := owner[target]; taken && dev.Device != cfg.Devices[other].Device {
					return nil, errors.New(dname + ": same Sparkplug target as devices[" + strconv.Itoa(other) + "]")
				}
				tp = &TargetPlan{Target: target, Device: dev.Device, Points: make(map[string]PlannedPoint)}
				plan.Targets[target] = tp
				plan.ByDevice[dev.Device] = target
				owner[target] = di
			} else if tp.Target != target {
				return nil, errors.New(dname + ": points address more than one Sparkplug target")
			}
			if prev, dup := first[addr.Metric]; dup {
				return nil, errors.New(pname + ": same metric as " + dname + ".points[" + strconv.Itoa(prev) + "]")
			}
			first[addr.Metric] = pi
			tp.Points[addr.Metric] = PlannedPoint{Device: dev.Device, Signal: p.Signal, Point: p}

			prefix := SparkplugNamespace + "/" + addr.Group + "/+/" + addr.Node
			filters[prefix] = true
			if addr.Device != "" {
				filters[prefix+"/"+addr.Device] = true
			}
		}
	}
	for f := range filters {
		plan.Filters = append(plan.Filters, f)
	}
	slices.Sort(plan.Filters)

	return plan, nil
}
