package mqtt

import (
	"slices"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/model"
)

// Read error kinds of Sparkplug B (spec section 7).
const (
	// KindUnknownAlias is a data metric with only an alias that no birth of
	// its node taught (decision S9).
	KindUnknownAlias = "unknown_alias"
	// KindSequenceGap is a message whose seq does not follow the last one of
	// its node (decision S10).
	KindSequenceGap = "sequence_gap"
)

const (
	// rebirthInterval is the shortest time between two rebirth requests to
	// one node (decision S11).
	rebirthInterval = 30 * time.Second
	// metricRebirth is the metric name of a rebirth request.
	metricRebirth = "Node Control/Rebirth"
	// metricBdSeq is the metric of births and deaths that pairs an NDEATH
	// with its NBIRTH (decision S5).
	metricBdSeq = "bdSeq"
)

// Sparkplug B message types (the third topic level).
const (
	spNBIRTH = "NBIRTH"
	spNDEATH = "NDEATH"
	spNDATA  = "NDATA"
	spDBIRTH = "DBIRTH"
	spDDEATH = "DDEATH"
	spDDATA  = "DDATA"
	spNCMD   = "NCMD"
	spDCMD   = "DCMD"
	spSTATE  = "STATE"
)

// spResult is what one Sparkplug message gives the input.
type spResult struct {
	// Samples are the samples of the addressed metrics.
	Samples []model.Sample
	// Online and Offline are the agent devices a birth or a death puts
	// online or offline, sorted.
	Online, Offline []string
	// Kinds are the read error kinds, one per occurrence.
	Kinds []string
	// Mapped is false for a topic outside the Sparkplug shape or a node or
	// device not in the plan: the input counts it unmapped.
	Mapped bool
	// Ignored is true for NCMD, DCMD and STATE, dropped without a count.
	Ignored bool
}

// spMetricKey names a metric of a node (device "") or of one of its devices.
type spMetricKey struct {
	device string
	name   string
}

// spNode is the state the session keeps of one known edge node.
type spNode struct {
	// aliases are learnt from the births of the node and its devices:
	// Sparkplug aliases are unique per edge node. Only an NBIRTH replaces them.
	aliases map[uint64]spMetricKey
	// datatypes are the datatypes the births declared, by metric.
	datatypes map[spMetricKey]uint32

	bdSeq    uint64
	hasBdSeq bool

	seq    uint64
	hasSeq bool

	lastRebirth time.Time
	rebirthDue  bool
}

// sparkplugSession is the Sparkplug B state machine of one input: births,
// deaths, aliases, seq and bdSeq (decisions S3 to S5 and S8 to S11). It never
// touches the network or the device lifecycle: Handle returns what a message
// means and the input applies it.
//
// It is used from the router's single goroutine under the router's mutex, so
// it has no lock of its own and is not safe for concurrent use.
type sparkplugSession struct {
	plan    *SparkplugPlan
	rebirth bool
	clk     clock.Clock

	// nodes are the known nodes (Device ""): the node of every target,
	// devices included (decision S1b).
	nodes map[SparkplugTarget]*spNode
	// devicesOf are the agent devices of the Sparkplug devices under a node,
	// sorted.
	devicesOf map[SparkplugTarget][]string
}

// newSparkplugSession returns the session of plan. With rebirth false,
// RebirthNodes never returns a node.
func newSparkplugSession(plan *SparkplugPlan, rebirth bool, clk clock.Clock) *sparkplugSession {
	s := &sparkplugSession{
		plan:      plan,
		rebirth:   rebirth,
		clk:       clk,
		nodes:     make(map[SparkplugTarget]*spNode),
		devicesOf: make(map[SparkplugTarget][]string),
	}
	for target, tp := range plan.Targets {
		node := SparkplugTarget{Group: target.Group, Node: target.Node}
		if s.nodes[node] == nil {
			s.nodes[node] = &spNode{}
		}
		if target.Device != "" {
			s.devicesOf[node] = append(s.devicesOf[node], tp.Device)
		}
	}
	for node := range s.devicesOf {
		slices.Sort(s.devicesOf[node])
	}

	return s
}

// Handle interprets one message received at received. Its payload is
// decoded only when the topic is a handled message of a node or device in
// the plan.
func (s *sparkplugSession) Handle(topic string, payload []byte, received time.Time) spResult {
	levels := strings.Split(topic, "/")
	if len(levels) < 3 || levels[0] != SparkplugNamespace {
		return spResult{}
	}
	if len(levels) == 3 && levels[1] == spSTATE {
		return spResult{Mapped: true, Ignored: true}
	}
	if len(levels) < 4 || len(levels) > 5 {
		return spResult{}
	}

	typ := levels[2]
	switch typ {
	case spNCMD, spDCMD, spSTATE:
		return spResult{Mapped: true, Ignored: true}
	case spNBIRTH, spNDEATH, spNDATA:
		if len(levels) != 4 {
			return spResult{}
		}
	case spDBIRTH, spDDEATH, spDDATA:
		if len(levels) != 5 {
			return spResult{}
		}
	default:
		return spResult{}
	}

	nodeKey := SparkplugTarget{Group: levels[1], Node: levels[3]}
	node := s.nodes[nodeKey]
	if node == nil {
		return spResult{}
	}
	target := nodeKey
	if len(levels) == 5 {
		target.Device = levels[4]
		if s.plan.Targets[target] == nil {
			return spResult{}
		}
	}

	p, err := decodePayload(payload)
	if err != nil {
		return spResult{Mapped: true, Kinds: []string{KindMalformedPayload}}
	}

	r := spResult{Mapped: true}
	switch typ {
	case spNBIRTH:
		node.aliases = make(map[uint64]spMetricKey)
		node.datatypes = make(map[spMetricKey]uint32)
		node.bdSeq, node.hasBdSeq = bdSeqOf(p)
		node.seq, node.hasSeq = p.Seq, p.HasSeq
		node.learn("", p)
		s.samples(&r, node, target, p, received)
		if tp := s.plan.Targets[target]; tp != nil {
			r.Online = []string{tp.Device}
		}
	case spDBIRTH:
		s.checkSeq(&r, node, p)
		node.learn(target.Device, p)
		s.samples(&r, node, target, p, received)
		r.Online = []string{s.plan.Targets[target].Device}
	case spNDATA, spDDATA:
		s.checkSeq(&r, node, p)
		s.samples(&r, node, target, p, received)
	case spNDEATH:
		if bdSeq, ok := bdSeqOf(p); ok && node.hasBdSeq && bdSeq != node.bdSeq {
			return r
		}
		node.hasSeq = false
		if tp := s.plan.Targets[target]; tp != nil {
			r.Offline = append(r.Offline, tp.Device)
		}
		r.Offline = append(r.Offline, s.devicesOf[nodeKey]...)
		slices.Sort(r.Offline)
	case spDDEATH:
		s.checkSeq(&r, node, p)
		r.Offline = []string{s.plan.Targets[target].Device}
	}

	return r
}

// Reset ends every node's seq tracking, at a reconnection: a gap across an
// outage is expected (decision S10). Aliases and bdSeq survive.
func (s *sparkplugSession) Reset() {
	for _, node := range s.nodes {
		node.hasSeq = false
	}
}

// RebirthNodes returns the nodes due a rebirth request, sorted by key, and
// marks them requested at now. It returns none when rebirth is off.
func (s *sparkplugSession) RebirthNodes(now time.Time) []SparkplugTarget {
	if !s.rebirth {
		return nil
	}
	var due []SparkplugTarget
	for key, node := range s.nodes {
		if node.rebirthDue {
			node.rebirthDue = false
			node.lastRebirth = now
			due = append(due, key)
		}
	}
	slices.SortFunc(due, func(a, b SparkplugTarget) int { return strings.Compare(a.Key(), b.Key()) })

	return due
}

// trigger asks for a rebirth of the node, at most once every
// rebirthInterval of the injected clock (decision S11). A trigger within the
// interval of the last request is dropped.
func (s *sparkplugSession) trigger(node *spNode) {
	if !s.rebirth || node.rebirthDue {
		return
	}
	if !node.lastRebirth.IsZero() && s.clk.Now().Sub(node.lastRebirth) < rebirthInterval {
		return
	}
	node.rebirthDue = true
}

// checkSeq applies decision S10 to NDATA, DBIRTH, DDATA and DDEATH. A
// message without seq is not checked and ends the tracking, so the next one
// sets the last without a check.
func (s *sparkplugSession) checkSeq(r *spResult, node *spNode, p spPayload) {
	if !p.HasSeq {
		node.hasSeq = false
		return
	}
	if node.hasSeq && p.Seq != (node.seq+1)%256 {
		r.Kinds = append(r.Kinds, KindSequenceGap)
		s.trigger(node)
	}
	node.seq, node.hasSeq = p.Seq, true
}

// samples appends the samples of the metrics of target that points address.
// Metrics no point addresses are ignored without a count (decision S9).
func (s *sparkplugSession) samples(r *spResult, node *spNode, target SparkplugTarget, p spPayload, received time.Time) {
	tp := s.plan.Targets[target]
	if tp == nil {
		return
	}
	for _, m := range p.Metrics {
		var key spMetricKey
		switch {
		case m.HasName:
			key = spMetricKey{device: target.Device, name: m.Name}
		case m.HasAlias:
			known, ok := node.aliases[m.Alias]
			if !ok || known.device != target.Device {
				r.Kinds = append(r.Kinds, KindUnknownAlias)
				s.trigger(node)
				continue
			}
			key = known
		default:
			continue
		}
		pp, ok := tp.Points[key.name]
		if !ok {
			continue
		}

		ts, plausible := metricTime(m, p, received)
		if !plausible {
			r.Samples = append(r.Samples, badSample(pp, received.UTC()))
			r.Kinds = append(r.Kinds, KindBadTimestamp)
			continue
		}
		if m.Null {
			r.Samples = append(r.Samples, badSample(pp, ts))
			continue
		}
		v, kind := metricValue(m, node.datatypes[key], pp.Point.Type)
		if kind != "" {
			r.Samples = append(r.Samples, badSample(pp, ts))
			r.Kinds = append(r.Kinds, kind)
			continue
		}
		r.Samples = append(r.Samples, model.Sample{Device: pp.Device, Signal: pp.Signal, TS: ts, Value: v, Quality: metricQuality(m), Priority: pp.Point.Priority})
	}
}

// learn records the aliases and datatypes of a birth of the node (device "")
// or of one of its devices (decision S4).
func (n *spNode) learn(device string, p spPayload) {
	if n.aliases == nil {
		n.aliases = make(map[uint64]spMetricKey)
		n.datatypes = make(map[spMetricKey]uint32)
	}
	for _, m := range p.Metrics {
		if !m.HasName {
			continue
		}
		key := spMetricKey{device: device, name: m.Name}
		if m.Datatype != 0 {
			n.datatypes[key] = m.Datatype
		}
		if m.HasAlias {
			n.aliases[m.Alias] = key
		}
	}
}

// bdSeqOf reads the bdSeq metric of a birth or death, from int_value or
// long_value.
func bdSeqOf(p spPayload) (uint64, bool) {
	for _, m := range p.Metrics {
		if m.HasName && m.Name == metricBdSeq && (m.Value.Field == spFieldInt || m.Value.Field == spFieldLong) {
			return m.Value.Uint, true
		}
	}

	return 0, false
}
