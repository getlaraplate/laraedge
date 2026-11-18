package mqtt

import (
	"reflect"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/mqtt/mqtttest"
	"github.com/getlaraplate/laraedge/internal/model"
)

var spStart = time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)

// sessionPlan: press is the node g/n1, oven the device g/n1/d1, saw the
// device g/n2/d2 (node n2 is known only through it, decision S1b).
func sessionPlan(t *testing.T) *SparkplugPlan {
	t.Helper()

	plan, err := PlanSparkplug(config.Input{Devices: []config.Device{
		{Device: "press", Points: []config.Point{
			{Signal: "temp", Type: "float64", Address: "g/n1|Temp", Priority: model.Critical},
			{Signal: "count", Type: "int64", Address: "g/n1|Count"},
		}},
		{Device: "oven", Points: []config.Point{{Signal: "speed", Type: "int32", Address: "g/n1/d1|Speed", Priority: model.Critical}}},
		{Device: "saw", Points: []config.Point{{Signal: "on", Type: "bool", Address: "g/n2/d2|On"}}},
	}})
	if err != nil {
		t.Fatalf("PlanSparkplug: %v", err)
	}

	return plan
}

func newTestSession(t *testing.T, rebirth bool) (*sparkplugSession, *clock.Fake) {
	t.Helper()

	clk := clock.NewFake(spStart)

	return newSparkplugSession(sessionPlan(t), rebirth, clk), clk
}

const (
	dtInt64   uint32 = 4
	dtDouble  uint32 = 10
	dtBoolean uint32 = 11
)

func named(name string, alias uint64, dt uint32) mqtttest.SparkplugMetric {
	return mqtttest.SparkplugMetric{Name: ptr(name), Alias: ptr(alias), Datatype: ptr(dt)}
}

func temp(alias uint64, v float64) mqtttest.SparkplugMetric {
	m := named("Temp", alias, dtDouble)
	m.Double = ptr(v)
	return m
}

func byAlias(alias uint64, v float64) mqtttest.SparkplugMetric {
	return mqtttest.SparkplugMetric{Alias: ptr(alias), Double: ptr(v)}
}

func payload(seq *uint64, metrics ...mqtttest.SparkplugMetric) []byte {
	return mqtttest.SparkplugPayload{Seq: seq, Metrics: metrics}.Encode()
}

// nbirth is an NBIRTH of g/n1 with bdSeq, seq and Temp under alias 1.
func nbirth(bdSeq, seq uint64) []byte {
	return payload(ptr(seq), mqtttest.BdSeq(bdSeq), temp(1, 20.5))
}

func ndeath(bdSeq *uint64) []byte {
	if bdSeq == nil {
		return payload(nil)
	}
	return payload(nil, mqtttest.BdSeq(*bdSeq))
}

const (
	tNBIRTH = "spBv1.0/g/NBIRTH/n1"
	tNDATA  = "spBv1.0/g/NDATA/n1"
	tNDEATH = "spBv1.0/g/NDEATH/n1"
	tDBIRTH = "spBv1.0/g/DBIRTH/n1/d1"
	tDDATA  = "spBv1.0/g/DDATA/n1/d1"
	tDDEATH = "spBv1.0/g/DDEATH/n1/d1"
)

func tempSample(v float64, ts time.Time) model.Sample {
	fv, _ := model.FloatValue(v)
	return model.Sample{Device: "press", Signal: "temp", TS: ts, Value: fv, Quality: model.Good, Priority: model.Critical}
}

func TestSessionNBIRTHGivesSamplesOnlineAndAliases(t *testing.T) {
	s, _ := newTestSession(t, false)

	r := s.Handle(tNBIRTH, nbirth(1, 0), spStart)
	if !r.Mapped || r.Ignored || r.Kinds != nil {
		t.Fatalf("result = %+v, want mapped, no kinds", r)
	}
	if want := []string{"press"}; !reflect.DeepEqual(r.Online, want) {
		t.Fatalf("Online = %q, want %q", r.Online, want)
	}
	if want := []model.Sample{tempSample(20.5, spStart)}; !reflect.DeepEqual(r.Samples, want) {
		t.Fatalf("Samples = %+v, want %+v", r.Samples, want)
	}

	r = s.Handle(tNDATA, payload(ptr(uint64(1)), byAlias(1, 21)), spStart)
	if want := []model.Sample{tempSample(21, spStart)}; !reflect.DeepEqual(r.Samples, want) || r.Kinds != nil {
		t.Fatalf("NDATA by learnt alias = %+v, want %+v", r, want)
	}
}

func TestSessionNDATAResolvesByAliasAndByName(t *testing.T) {
	tests := []struct {
		name   string
		metric mqtttest.SparkplugMetric
	}{
		{"by alias, datatype from the birth", byAlias(1, 22)},
		{"by name", mqtttest.SparkplugMetric{Name: ptr("Temp"), Double: ptr(22.0)}},
		{"by name and its own datatype", temp(9, 22)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestSession(t, false)
			s.Handle(tNBIRTH, nbirth(1, 0), spStart)

			r := s.Handle(tNDATA, payload(ptr(uint64(1)), tt.metric), spStart)
			if want := []model.Sample{tempSample(22, spStart)}; !reflect.DeepEqual(r.Samples, want) || r.Kinds != nil {
				t.Fatalf("result = %+v, want samples %+v", r, want)
			}
		})
	}
}

func TestSessionUnknownAlias(t *testing.T) {
	tests := []struct {
		name    string
		rebirth bool
		want    []SparkplugTarget
	}{
		{"rebirth off", false, nil},
		{"rebirth on", true, []SparkplugTarget{{Group: "g", Node: "n1"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, clk := newTestSession(t, tt.rebirth)

			r := s.Handle(tNDATA, payload(nil, byAlias(7, 1), byAlias(8, 2)), spStart)
			if r.Samples != nil || !r.Mapped {
				t.Fatalf("result = %+v, want mapped without samples", r)
			}
			if want := []string{KindUnknownAlias, KindUnknownAlias}; !reflect.DeepEqual(r.Kinds, want) {
				t.Fatalf("Kinds = %q, want %q", r.Kinds, want)
			}
			if got := s.RebirthNodes(clk.Now()); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("RebirthNodes = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSessionRebirthAtMostOnceEvery30s(t *testing.T) {
	s, clk := newTestSession(t, true)
	node := []SparkplugTarget{{Group: "g", Node: "n1"}}
	trigger := func() { s.Handle(tNDATA, payload(nil, byAlias(7, 1)), clk.Now()) }

	trigger()
	trigger()
	if got := s.RebirthNodes(clk.Now()); !reflect.DeepEqual(got, node) {
		t.Fatalf("first RebirthNodes = %+v, want %+v", got, node)
	}

	clk.Advance(10 * time.Second)
	trigger()
	if got := s.RebirthNodes(clk.Now()); got != nil {
		t.Fatalf("RebirthNodes within 30 s = %+v, want none", got)
	}

	clk.Advance(rebirthInterval)
	if got := s.RebirthNodes(clk.Now()); got != nil {
		t.Fatalf("RebirthNodes without a new trigger = %+v, want none", got)
	}
	trigger()
	if got := s.RebirthNodes(clk.Now()); !reflect.DeepEqual(got, node) {
		t.Fatalf("RebirthNodes after 30 s = %+v, want %+v", got, node)
	}
	if got := s.RebirthNodes(clk.Now()); got != nil {
		t.Fatalf("RebirthNodes twice = %+v, want none", got)
	}
}

func TestSessionSeq(t *testing.T) {
	tests := []struct {
		name  string
		birth uint64
		seqs  []uint64
		reset bool
		kinds []string
	}{
		{"wraps cleanly", 254, []uint64{255, 0, 1}, false, nil},
		{"a gap", 0, []uint64{1, 3, 4}, false, []string{KindSequenceGap}},
		{"after Reset any seq is clean", 0, []uint64{9, 10}, true, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, clk := newTestSession(t, true)
			s.Handle(tNBIRTH, nbirth(1, tt.birth), spStart)
			if tt.reset {
				s.Reset()
			}

			var kinds []string
			for _, seq := range tt.seqs {
				kinds = append(kinds, s.Handle(tNDATA, payload(ptr(seq), temp(1, 1)), spStart).Kinds...)
			}
			if !reflect.DeepEqual(kinds, tt.kinds) {
				t.Fatalf("Kinds = %q, want %q", kinds, tt.kinds)
			}
			if got := s.RebirthNodes(clk.Now()); (got != nil) != (tt.kinds != nil) {
				t.Fatalf("RebirthNodes = %+v, want a request: %v", got, tt.kinds != nil)
			}
		})
	}
}

func TestSessionSeqAcrossNodeAndDeviceMessages(t *testing.T) {
	s, _ := newTestSession(t, false)
	s.Handle(tNBIRTH, nbirth(1, 10), spStart)

	var kinds []string
	for i, topic := range []string{tDBIRTH, tDDATA, tNDATA, tDDEATH} {
		kinds = append(kinds, s.Handle(topic, payload(ptr(uint64(11+i))), spStart).Kinds...)
	}
	if kinds != nil {
		t.Fatalf("Kinds = %q, want none", kinds)
	}
}

func TestSessionNDEATH(t *testing.T) {
	tests := []struct {
		name        string
		birth       bool
		death       *uint64
		wantOffline []string
	}{
		{"matching bdSeq", true, ptr(uint64(5)), []string{"oven", "press"}},
		{"mismatched bdSeq is stale", true, ptr(uint64(4)), nil},
		{"no bdSeq is honoured", true, nil, []string{"oven", "press"}},
		{"no NBIRTH seen is honoured", false, ptr(uint64(4)), []string{"oven", "press"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestSession(t, false)
			if tt.birth {
				s.Handle(tNBIRTH, nbirth(5, 0), spStart)
			}

			r := s.Handle(tNDEATH, ndeath(tt.death), spStart)
			if !r.Mapped || r.Kinds != nil || r.Samples != nil {
				t.Fatalf("result = %+v, want mapped, no kinds, no samples", r)
			}
			if !reflect.DeepEqual(r.Offline, tt.wantOffline) {
				t.Fatalf("Offline = %q, want %q", r.Offline, tt.wantOffline)
			}
		})
	}
}

func TestSessionNDEATHEndsSeqTracking(t *testing.T) {
	s, _ := newTestSession(t, false)
	s.Handle(tNBIRTH, nbirth(5, 0), spStart)
	s.Handle(tNDEATH, ndeath(ptr(uint64(5))), spStart)

	// The data is a missed birth (ruling R11), never a sequence gap.
	if r := s.Handle(tNDATA, payload(ptr(uint64(42)), temp(1, 1)), spStart); !reflect.DeepEqual(r.Kinds, []string{KindMissedBirth}) {
		t.Fatalf("Kinds after NDEATH = %q, want only missed_birth", r.Kinds)
	}
}

func TestSessionNodeKnownOnlyThroughDevices(t *testing.T) {
	s, _ := newTestSession(t, false)

	r := s.Handle("spBv1.0/g/NBIRTH/n2", payload(ptr(uint64(0)), mqtttest.BdSeq(3), temp(1, 1)), spStart)
	if !r.Mapped || r.Samples != nil || r.Online != nil {
		t.Fatalf("NBIRTH = %+v, want mapped, no samples, nobody online", r)
	}
	r = s.Handle("spBv1.0/g/NDEATH/n2", ndeath(ptr(uint64(3))), spStart)
	if want := []string{"saw"}; !reflect.DeepEqual(r.Offline, want) {
		t.Fatalf("NDEATH Offline = %q, want %q", r.Offline, want)
	}
}

func TestSessionDDEATHPutsOnlyThatDeviceOffline(t *testing.T) {
	s, _ := newTestSession(t, false)
	s.Handle(tNBIRTH, nbirth(1, 0), spStart)

	r := s.Handle(tDDEATH, payload(ptr(uint64(1))), spStart)
	if want := []string{"oven"}; !reflect.DeepEqual(r.Offline, want) || r.Online != nil {
		t.Fatalf("result = %+v, want Offline %q", r, want)
	}
}

// TestSessionBirthRevivesAfterADeath: an NBIRTH after an NDEATH puts the
// node online, and its data is then no missed birth.
func TestSessionBirthRevivesAfterADeath(t *testing.T) {
	s, _ := newTestSession(t, false)
	s.Handle(tNBIRTH, nbirth(1, 0), spStart)
	s.Handle(tNDEATH, ndeath(ptr(uint64(1))), spStart)

	if r := s.Handle(tNBIRTH, nbirth(2, 0), spStart); !reflect.DeepEqual(r.Online, []string{"press"}) {
		t.Fatalf("NBIRTH Online = %q, want [press]", r.Online)
	}
	if r := s.Handle(tNDATA, payload(ptr(uint64(1)), temp(1, 1)), spStart); r.Online != nil || r.Kinds != nil || len(r.Samples) != 1 {
		t.Fatalf("NDATA after the NBIRTH = %+v, want one sample, nobody online, no kinds", r)
	}
}

// TestSessionReplayedNDEATHThenDataIsAMissedBirth is the persistent session
// replay of ruling R11: an agent started fresh receives the node's NDEATH
// queued while it was down, the NBIRTH and DBIRTH (QoS 0) were not queued,
// and the node publishes data by name. The data puts the node online, its
// samples are kept, the message counts missed_birth once and, with rebirth
// on, the node is due one rebirth.
func TestSessionReplayedNDEATHThenDataIsAMissedBirth(t *testing.T) {
	node := []SparkplugTarget{{Group: "g", Node: "n1"}}

	for _, rebirth := range []bool{true, false} {
		t.Run(map[bool]string{true: "rebirth on", false: "rebirth off"}[rebirth], func(t *testing.T) {
			s, clk := newTestSession(t, rebirth)

			if r := s.Handle(tNDEATH, ndeath(ptr(uint64(1))), spStart); !reflect.DeepEqual(r.Offline, []string{"oven", "press"}) {
				t.Fatalf("replayed NDEATH Offline = %q, want [oven press]", r.Offline)
			}

			r := s.Handle(tNDATA, payload(ptr(uint64(5)), temp(0, 21.5)), spStart)
			if !reflect.DeepEqual(r.Online, []string{"press"}) || !reflect.DeepEqual(r.Kinds, []string{KindMissedBirth}) {
				t.Fatalf("NDATA = %+v, want press online and one missed_birth", r)
			}
			if want := []model.Sample{tempSample(21.5, spStart)}; !reflect.DeepEqual(r.Samples, want) {
				t.Fatalf("NDATA Samples = %+v, want %+v", r.Samples, want)
			}
			got := s.RebirthNodes(clk.Now())
			if rebirth && !reflect.DeepEqual(got, node) {
				t.Fatalf("RebirthNodes = %+v, want %+v", got, node)
			}
			if !rebirth && got != nil {
				t.Fatalf("RebirthNodes = %+v, want none with rebirth off", got)
			}

			// The node is alive again: its next data is no missed birth.
			if r := s.Handle(tNDATA, payload(ptr(uint64(6)), temp(0, 22)), spStart); r.Online != nil || r.Kinds != nil {
				t.Fatalf("second NDATA = %+v, want nobody online, no kinds", r)
			}

			// The device, held dead by the NDEATH too, revives with its own
			// data; the rebirth is not asked again within 30 s.
			r = s.Handle(tDDATA, payload(ptr(uint64(7)), speed(0, 9)), spStart)
			if !reflect.DeepEqual(r.Online, []string{"oven"}) || !reflect.DeepEqual(r.Kinds, []string{KindMissedBirth}) || len(r.Samples) != 1 {
				t.Fatalf("DDATA = %+v, want oven online, one sample, one missed_birth", r)
			}
			if got := s.RebirthNodes(clk.Now()); got != nil {
				t.Fatalf("RebirthNodes within 30 s = %+v, want none", got)
			}
		})
	}
}

// TestSessionMissedBirthTargets: which agent devices data held dead by a
// death puts online, and that a stale NDEATH holds nothing.
func TestSessionMissedBirthTargets(t *testing.T) {
	tests := []struct {
		name   string
		deaths []string
		data   string
		online []string
		kinds  []string
	}{
		{"DDATA under a dead node revives the node and the device", []string{tNDEATH}, tDDATA, []string{"oven", "press"}, []string{KindMissedBirth}},
		{"NDATA under a dead node revives the node only", []string{tNDEATH}, tNDATA, []string{"press"}, []string{KindMissedBirth}},
		{"DDATA after a DDEATH revives the device", []string{tDDEATH}, tDDATA, []string{"oven"}, []string{KindMissedBirth}},
		{"NDATA after a DDEATH is no missed birth", []string{tDDEATH}, tNDATA, nil, nil},
		{"DDATA after the DBIRTH that followed a death", []string{tDDEATH, tDBIRTH}, tDDATA, nil, nil},
		{"DDATA after an NBIRTH still misses the DBIRTH", []string{tNDEATH, tNBIRTH}, tDDATA, []string{"oven"}, []string{KindMissedBirth}},
		{"an unplanned device under a dead node revives the node", []string{tNDEATH}, "spBv1.0/g/DDATA/n1/d9", []string{"press"}, []string{KindMissedBirth}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestSession(t, false)
			s.Handle(tNBIRTH, nbirth(1, 0), spStart)
			for _, topic := range tt.deaths {
				p := payload(nil, mqtttest.BdSeq(1))
				if topic == tNBIRTH {
					p = nbirth(2, 0)
				}
				s.Handle(topic, p, spStart)
			}

			r := s.Handle(tt.data, payload(nil, temp(0, 1), speed(0, 1)), spStart)
			if !reflect.DeepEqual(r.Online, tt.online) || !reflect.DeepEqual(r.Kinds, tt.kinds) {
				t.Fatalf("data = %+v, want Online %q, Kinds %q", r, tt.online, tt.kinds)
			}
		})
	}

	t.Run("a stale NDEATH holds nothing", func(t *testing.T) {
		s, _ := newTestSession(t, true)
		s.Handle(tNBIRTH, nbirth(5, 0), spStart)
		s.Handle(tNDEATH, ndeath(ptr(uint64(4))), spStart)

		if r := s.Handle(tNDATA, payload(nil, temp(1, 1)), spStart); r.Online != nil || r.Kinds != nil {
			t.Fatalf("NDATA = %+v, want nobody online, no kinds", r)
		}
	})
}

func TestSessionDBIRTHWithoutNBIRTH(t *testing.T) {
	s, _ := newTestSession(t, false)

	speed := named("Speed", 30, 3)
	speed.Int = ptr(uint32(1500))
	r := s.Handle(tDBIRTH, payload(ptr(uint64(7)), speed), spStart)
	if !r.Mapped || r.Kinds != nil || !reflect.DeepEqual(r.Online, []string{"oven"}) {
		t.Fatalf("DBIRTH = %+v, want mapped, oven online", r)
	}
	want := []model.Sample{{Device: "oven", Signal: "speed", TS: spStart, Value: model.IntValue(1500), Quality: model.Good, Priority: model.Critical}}
	if !reflect.DeepEqual(r.Samples, want) {
		t.Fatalf("DBIRTH Samples = %+v, want %+v", r.Samples, want)
	}

	r = s.Handle(tDDATA, payload(ptr(uint64(8)), mqtttest.SparkplugMetric{Alias: ptr(uint64(30)), Int: ptr(uint32(1600))}), spStart)
	if r.Kinds != nil || len(r.Samples) != 1 || r.Samples[0].Value != model.IntValue(1600) {
		t.Fatalf("DDATA by the DBIRTH alias = %+v", r)
	}
}

func TestSessionNBIRTHReplacesAliases(t *testing.T) {
	s, _ := newTestSession(t, false)
	s.Handle(tNBIRTH, nbirth(1, 0), spStart)
	s.Handle(tNBIRTH, payload(ptr(uint64(0)), mqtttest.BdSeq(2), temp(2, 1)), spStart)

	r := s.Handle(tNDATA, payload(nil, byAlias(1, 3)), spStart)
	if want := []string{KindUnknownAlias}; !reflect.DeepEqual(r.Kinds, want) {
		t.Fatalf("Kinds for the old alias = %q, want %q", r.Kinds, want)
	}
}

func TestSessionPayloads(t *testing.T) {
	s, _ := newTestSession(t, false)
	s.Handle(tNBIRTH, nbirth(1, 0), spStart)

	r := s.Handle(tNDATA, payload(nil, temp(1, 1), named("Other", 99, dtInt64), mqtttest.SparkplugMetric{Name: ptr("Unaddressed"), Long: ptr(uint64(1))}), spStart)
	if r.Kinds != nil || len(r.Samples) != 1 {
		t.Fatalf("unaddressed metrics = %+v, want one sample and no kinds", r)
	}

	r = s.Handle(tNDATA, []byte{0x0a, 0xff}, spStart)
	if want := []string{KindMalformedPayload}; !r.Mapped || r.Samples != nil || !reflect.DeepEqual(r.Kinds, want) {
		t.Fatalf("malformed = %+v, want Kinds %q", r, want)
	}
}

func TestSessionTopics(t *testing.T) {
	tests := []struct {
		topic   string
		mapped  bool
		ignored bool
	}{
		{"plant/n1/data", false, false},
		{"spBv1.0/g/NDATA", false, false},
		{"spBv1.0/g/NDATA/n9", false, false},
		{"spBv1.0/h/NDATA/n1", false, false},
		{"spBv1.0/g/DDATA/n1/d9", true, false},
		{"spBv1.0/g/DDATA/n9/d1", false, false},
		{"spBv1.0/g/NDATA/n1/d1", false, false},
		{"spBv1.0/g/DDATA/n1", false, false},
		{"spBv1.0/g/FOO/n1", false, false},
		{"spBv1.0/g/NDATA/n1", true, false},
		{"spBv1.0/g/NCMD/n1", true, true},
		{"spBv1.0/g/DCMD/n1/d1", true, true},
		{"spBv1.0/g/STATE/n1", true, true},
		{"spBv1.0/STATE/host", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			s, _ := newTestSession(t, false)

			r := s.Handle(tt.topic, payload(nil, temp(1, 1)), spStart)
			if r.Mapped != tt.mapped || r.Ignored != tt.ignored {
				t.Fatalf("Mapped, Ignored = %v, %v, want %v, %v", r.Mapped, r.Ignored, tt.mapped, tt.ignored)
			}
			if !tt.mapped || tt.ignored {
				if r.Samples != nil || r.Kinds != nil || r.Online != nil || r.Offline != nil {
					t.Fatalf("result = %+v, want empty", r)
				}
			}
		})
	}
}

func TestSessionNullAndTimestamps(t *testing.T) {
	metricTS := uint64(spStart.Add(-time.Hour).UnixMilli())
	payloadTS := uint64(spStart.Add(-time.Minute).UnixMilli())

	historical := temp(1, 5)
	historical.Historical = true
	historical.Timestamp = ptr(metricTS)

	null := mqtttest.SparkplugMetric{Alias: ptr(uint64(1)), Null: true}

	badTS := temp(1, 5)
	badTS.Timestamp = ptr(uint64(1) << 63)

	nullBadTS := null
	nullBadTS.Timestamp = ptr(uint64(1) << 63)

	tests := []struct {
		name      string
		p         mqtttest.SparkplugPayload
		want      model.Sample
		wantKinds []string
	}{
		{"is_null is bad without a kind", mqtttest.SparkplugPayload{Metrics: []mqtttest.SparkplugMetric{null}},
			badSample(PlannedPoint{Device: "press", Signal: "temp", Point: config.Point{Type: "float64", Priority: model.Critical}}, spStart), nil},
		{"historical keeps its timestamp", mqtttest.SparkplugPayload{Timestamp: ptr(payloadTS), Metrics: []mqtttest.SparkplugMetric{historical}},
			tempSample(5, spStart.Add(-time.Hour)), nil},
		{"payload timestamp", mqtttest.SparkplugPayload{Timestamp: ptr(payloadTS), Metrics: []mqtttest.SparkplugMetric{temp(1, 5)}},
			tempSample(5, spStart.Add(-time.Minute)), nil},
		{"implausible timestamp", mqtttest.SparkplugPayload{Metrics: []mqtttest.SparkplugMetric{badTS}},
			badSample(PlannedPoint{Device: "press", Signal: "temp", Point: config.Point{Type: "float64", Priority: model.Critical}}, spStart), []string{KindBadTimestamp}},
		{"is_null with an implausible timestamp is bad without a kind", mqtttest.SparkplugPayload{Metrics: []mqtttest.SparkplugMetric{nullBadTS}},
			badSample(PlannedPoint{Device: "press", Signal: "temp", Point: config.Point{Type: "float64", Priority: model.Critical}}, spStart), nil},
		{"is_null with an implausible payload timestamp is bad without a kind", mqtttest.SparkplugPayload{Timestamp: ptr(uint64(1) << 63), Metrics: []mqtttest.SparkplugMetric{null}},
			badSample(PlannedPoint{Device: "press", Signal: "temp", Point: config.Point{Type: "float64", Priority: model.Critical}}, spStart), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestSession(t, false)
			s.Handle(tNBIRTH, nbirth(1, 0), spStart)

			r := s.Handle(tNDATA, tt.p.Encode(), spStart)
			if want := []model.Sample{tt.want}; !reflect.DeepEqual(r.Samples, want) || !reflect.DeepEqual(r.Kinds, tt.wantKinds) {
				t.Fatalf("result = %+v, want samples %+v kinds %q", r, want, tt.wantKinds)
			}
		})
	}
}

func TestSessionConversionKind(t *testing.T) {
	s, _ := newTestSession(t, false)
	s.Handle(tNBIRTH, nbirth(1, 0), spStart)

	m := mqtttest.SparkplugMetric{Name: ptr("Count"), Datatype: ptr(dtBoolean), Bool: ptr(true)}
	r := s.Handle(tNDATA, payload(nil, m), spStart)
	if want := []string{KindConversion}; !reflect.DeepEqual(r.Kinds, want) || len(r.Samples) != 1 || r.Samples[0].Quality != model.Bad {
		t.Fatalf("result = %+v, want one bad sample and Kinds %q", r, want)
	}
}

// TestSessionUnplannedDeviceOfAPlannedNode: a device of a planned node that
// no point addresses takes part in seq checking and teaches its aliases, but
// gives no samples, no Online or Offline and no kind, and is not unmapped.
func TestSessionUnplannedDeviceOfAPlannedNode(t *testing.T) {
	s, _ := newTestSession(t, true)
	s.Handle(tNBIRTH, nbirth(1, 0), spStart)

	steps := []struct {
		topic string
		p     []byte
		// unaddressed marks a message of d2, which must give nothing.
		unaddressed bool
	}{
		{tDBIRTH, payload(ptr(uint64(1))), false},
		{"spBv1.0/g/DBIRTH/n1/d2", payload(ptr(uint64(2)), named("Level", 40, dtDouble)), true},
		{"spBv1.0/g/DDATA/n1/d2", payload(ptr(uint64(3)), byAlias(40, 1)), true},
		{tDDATA, payload(ptr(uint64(4))), false},
		{"spBv1.0/g/NDATA/n1", payload(ptr(uint64(5)), temp(1, 1)), false},
		{"spBv1.0/g/DDATA/n1/d2", payload(ptr(uint64(6)), mqtttest.SparkplugMetric{Name: ptr("Level"), Double: ptr(2.0)}), true},
		{"spBv1.0/g/DDEATH/n1/d2", payload(ptr(uint64(7))), true},
		{tNDATA, payload(ptr(uint64(8))), false},
	}
	for _, st := range steps {
		r := s.Handle(st.topic, st.p, spStart)
		if !r.Mapped || r.Ignored || r.Kinds != nil {
			t.Fatalf("%s: result = %+v, want mapped and no kinds", st.topic, r)
		}
		if st.unaddressed && (r.Samples != nil || r.Online != nil || r.Offline != nil) {
			t.Fatalf("%s: result = %+v, want nothing", st.topic, r)
		}
	}
	if got := s.RebirthNodes(spStart); got != nil {
		t.Fatalf("RebirthNodes = %+v, want none", got)
	}

	if r := s.Handle("spBv1.0/g/DDATA/n1/d2", payload(ptr(uint64(10))), spStart); !reflect.DeepEqual(r.Kinds, []string{KindSequenceGap}) {
		t.Fatalf("gap on d2: Kinds = %q, want [sequence_gap]", r.Kinds)
	}
}

// TestSessionUnplannedDeviceAliasesStayItsOwn: an alias a DBIRTH of an
// unplanned device teaches does not resolve for a planned device.
func TestSessionUnplannedDeviceAliasesStayItsOwn(t *testing.T) {
	s, _ := newTestSession(t, false)
	s.Handle("spBv1.0/g/DBIRTH/n1/d2", payload(nil, named("Speed", 40, 3)), spStart)

	r := s.Handle(tDDATA, payload(nil, mqtttest.SparkplugMetric{Alias: ptr(uint64(40)), Int: ptr(uint32(1))}), spStart)
	if r.Samples != nil || !reflect.DeepEqual(r.Kinds, []string{KindUnknownAlias}) {
		t.Fatalf("result = %+v, want no sample and unknown_alias", r)
	}
}

// TestSessionMalformedPayloadOfAnUnaddressedDevice: a malformed payload from
// a device no point addresses, under a planned node, counts
// malformed_payload (ruling R5).
func TestSessionMalformedPayloadOfAnUnaddressedDevice(t *testing.T) {
	s, _ := newTestSession(t, false)
	s.Handle(tNBIRTH, nbirth(1, 0), spStart)

	r := s.Handle("spBv1.0/g/DDATA/n1/d2", []byte{0x0a, 0xff}, spStart)
	if want := []string{KindMalformedPayload}; !r.Mapped || r.Samples != nil || r.Online != nil || !reflect.DeepEqual(r.Kinds, want) {
		t.Fatalf("result = %+v, want mapped, no samples, Kinds %q", r, want)
	}
}

// TestSessionMetricDatatypeWinsOverTheBirths: a data metric that carries its
// own datatype is converted by it, not by the different one its birth
// declared.
func TestSessionMetricDatatypeWinsOverTheBirths(t *testing.T) {
	s, _ := newTestSession(t, false)

	count := named("Count", 2, dtInt64)
	count.Long = ptr(uint64(1))
	s.Handle(tNBIRTH, payload(ptr(uint64(0)), mqtttest.BdSeq(1), count), spStart)

	asDouble := mqtttest.SparkplugMetric{Alias: ptr(uint64(2)), Datatype: ptr(dtDouble), Double: ptr(3.0)}
	r := s.Handle(tNDATA, payload(ptr(uint64(1)), asDouble), spStart)
	want := []model.Sample{{Device: "press", Signal: "count", TS: spStart, Value: model.IntValue(3), Quality: model.Good}}
	if !reflect.DeepEqual(r.Samples, want) || r.Kinds != nil {
		t.Fatalf("result = %+v, want samples %+v and no kinds", r, want)
	}

	// The birth's datatype still applies to a metric without its own.
	byBirth := mqtttest.SparkplugMetric{Alias: ptr(uint64(2)), Double: ptr(3.0)}
	if r := s.Handle(tNDATA, payload(ptr(uint64(2)), byBirth), spStart); !reflect.DeepEqual(r.Kinds, []string{KindConversion}) {
		t.Fatalf("Kinds by the birth's datatype = %q, want [conversion]", r.Kinds)
	}
}

// TestSessionDBIRTHUnderADeadNodeIsAMissedBirth: a DBIRTH under a node held
// dead by an NDEATH proves the node alive, its NBIRTH lost. It brings the
// node's agent device online with the device's, counts missed_birth once
// and, with rebirth on, makes the node due one rebirth, as data does
// (ruling R13, reversed on 2026-11-17).
func TestSessionDBIRTHUnderADeadNodeIsAMissedBirth(t *testing.T) {
	node := []SparkplugTarget{{Group: "g", Node: "n1"}}
	tests := []struct {
		name   string
		topic  string
		online []string
	}{
		{"a planned device", tDBIRTH, []string{"oven", "press"}},
		{"an unplanned device", "spBv1.0/g/DBIRTH/n1/d9", []string{"press"}},
	}

	for _, tt := range tests {
		for _, rebirth := range []bool{true, false} {
			t.Run(tt.name+map[bool]string{true: ", rebirth on", false: ", rebirth off"}[rebirth], func(t *testing.T) {
				s, clk := newTestSession(t, rebirth)
				s.Handle(tNBIRTH, nbirth(1, 0), spStart)
				s.Handle(tNDEATH, ndeath(ptr(uint64(1))), spStart)

				r := s.Handle(tt.topic, payload(ptr(uint64(0)), speed(30, 7)), spStart)
				if !reflect.DeepEqual(r.Online, tt.online) || !reflect.DeepEqual(r.Kinds, []string{KindMissedBirth}) {
					t.Fatalf("DBIRTH = %+v, want Online %q and one missed_birth", r, tt.online)
				}
				got := s.RebirthNodes(clk.Now())
				if rebirth && !reflect.DeepEqual(got, node) {
					t.Fatalf("RebirthNodes = %+v, want %+v", got, node)
				}
				if !rebirth && got != nil {
					t.Fatalf("RebirthNodes = %+v, want none with rebirth off", got)
				}

				// The node is alive: its data is no missed birth.
				if r := s.Handle(tNDATA, payload(ptr(uint64(1)), temp(1, 1)), spStart); r.Online != nil || r.Kinds != nil {
					t.Fatalf("NDATA after the DBIRTH = %+v, want nobody online, no kinds", r)
				}
			})
		}
	}

	t.Run("a DBIRTH after a DDEATH is no missed birth", func(t *testing.T) {
		s, clk := newTestSession(t, true)
		s.Handle(tNBIRTH, nbirth(1, 0), spStart)
		s.Handle(tDDEATH, payload(ptr(uint64(1))), spStart)

		r := s.Handle(tDBIRTH, payload(ptr(uint64(2)), speed(30, 7)), spStart)
		if !reflect.DeepEqual(r.Online, []string{"oven"}) || r.Kinds != nil {
			t.Fatalf("DBIRTH = %+v, want oven online, no kinds", r)
		}
		if got := s.RebirthNodes(clk.Now()); got != nil {
			t.Fatalf("RebirthNodes = %+v, want none", got)
		}
	})
}
