package mqtt

import (
	"reflect"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/config"
)

func TestParseSparkplugAddress(t *testing.T) {
	ok := []struct {
		in   string
		want SparkplugAddress
	}{
		{"g/n|m", SparkplugAddress{Group: "g", Node: "n", Metric: "m"}},
		{"g/n/d|Node Control/Rebirth", SparkplugAddress{Group: "g", Node: "n", Device: "d", Metric: "Node Control/Rebirth"}},
		{"g/n/d|a|b", SparkplugAddress{Group: "g", Node: "n", Device: "d", Metric: "a|b"}},
	}
	for _, tt := range ok {
		got, err := ParseSparkplugAddress(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("%q: got %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"g|m", "g/n/d/x|m", "g//n|m", "g/+|m", "g/n#|m", "g/n|", "g/n", "", "|m"} {
		if _, err := ParseSparkplugAddress(in); err == nil {
			t.Errorf("%q: want an error", in)
		}
	}
}

func TestParseSparkplugAddressErrorsNeverEchoValues(t *testing.T) {
	const secret = "sekret-name"
	for _, in := range []string{secret + "|m", secret + "/+|m", secret + "/n/d/x|m", secret + "/n|", secret} {
		_, err := ParseSparkplugAddress(in)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("%q: error %v", in, err)
		}
	}
}

func TestSparkplugTargetKey(t *testing.T) {
	if got := (SparkplugTarget{Group: "g", Node: "n"}).Key(); got != "g/n" {
		t.Errorf("node key = %q", got)
	}
	if got := (SparkplugTarget{Group: "g", Node: "n", Device: "d"}).Key(); got != "g/n/d" {
		t.Errorf("device key = %q", got)
	}
}

func spInput(devices ...config.Device) config.Input {
	return config.Input{ID: "broker", Driver: config.DriverMQTT, Devices: devices}
}

func spDevice(name string, addrs ...string) config.Device {
	d := config.Device{Device: name}
	for i, a := range addrs {
		d.Points = append(d.Points, config.Point{Signal: "s" + string(rune('a'+i)), Address: a, Type: "float64", Read: config.Read{Mode: config.ReadSubscribe}})
	}
	return d
}

func TestPlanSparkplug(t *testing.T) {
	plan, err := PlanSparkplug(spInput(spDevice("n1", "g/n|a", "g/n|b"), spDevice("d1", "g/n/d|x")))
	if err != nil {
		t.Fatal(err)
	}
	wantFilters := []string{"spBv1.0/g/+/n", "spBv1.0/g/+/n/d"}
	if !reflect.DeepEqual(plan.Filters, wantFilters) {
		t.Errorf("filters = %v, want %v", plan.Filters, wantFilters)
	}
	node := SparkplugTarget{Group: "g", Node: "n"}
	dev := SparkplugTarget{Group: "g", Node: "n", Device: "d"}
	if plan.ByDevice["n1"] != node || plan.ByDevice["d1"] != dev {
		t.Errorf("ByDevice = %v", plan.ByDevice)
	}
	tp := plan.Targets[node]
	if tp == nil || tp.Device != "n1" || tp.Target != node || len(tp.Points) != 2 {
		t.Fatalf("node plan = %+v", tp)
	}
	if pp := tp.Points["b"]; pp.Device != "n1" || pp.Signal != "sb" || pp.Point.Address != "g/n|b" {
		t.Errorf("point b = %+v", pp)
	}
	if tp := plan.Targets[dev]; tp == nil || tp.Device != "d1" || len(tp.Points) != 1 {
		t.Errorf("device plan = %+v", tp)
	}
}

func TestPlanSparkplugDeviceAloneSubscribesItsNode(t *testing.T) {
	plan, err := PlanSparkplug(spInput(spDevice("d1", "g/n/d|x")))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"spBv1.0/g/+/n", "spBv1.0/g/+/n/d"}
	if !reflect.DeepEqual(plan.Filters, want) {
		t.Errorf("filters = %v, want %v", plan.Filters, want)
	}
	if _, ok := plan.Targets[SparkplugTarget{Group: "g", Node: "n"}]; ok {
		t.Error("the node is subscribed but must not be a target")
	}
}

func TestPlanSparkplugFiltersSortedAndDeduplicated(t *testing.T) {
	plan, err := PlanSparkplug(spInput(spDevice("a", "h/z/d1|x"), spDevice("b", "g/n/d2|x"), spDevice("c", "g/n/d1|x")))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"spBv1.0/g/+/n", "spBv1.0/g/+/n/d1", "spBv1.0/g/+/n/d2", "spBv1.0/h/+/z", "spBv1.0/h/+/z/d1"}
	if !reflect.DeepEqual(plan.Filters, want) {
		t.Errorf("filters = %v, want %v", plan.Filters, want)
	}
}

func TestPlanSparkplugRefusals(t *testing.T) {
	tests := []struct {
		name string
		in   config.Input
		want string
	}{
		{"device on two targets", spInput(spDevice("d", "g/n|a", "g/n/d|b")), "devices[0]: points address more than one Sparkplug target"},
		{"two devices one target", spInput(spDevice("d", "g/n/x|a"), spDevice("e", "g/n/x|b")), "devices[1]: same Sparkplug target as devices[0]"},
		{"same metric twice", spInput(spDevice("d", "g/n|a", "g/n|a")), "devices[0].points[1]: same metric as devices[0].points[0]"},
		{"same name two targets", spInput(spDevice("d", "g/n|a"), spDevice("d", "g/n/x|b")), "devices[1]: points address more than one Sparkplug target"},
		{"same name same metric", spInput(spDevice("d", "g/n|a"), spDevice("d", "g/n|a")), "devices[1].points[0]: same metric as devices[0].points[0]"},
		{"bad address", spInput(spDevice("d", "g|a")), "devices[0].points[0].address: want <group>/<edge_node>[/<device>]|<metric>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PlanSparkplug(tt.in)
			if err == nil || err.Error() != tt.want {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNewInputSparkplugBuildsItsPlan(t *testing.T) {
	cfg := spInput(spDevice("n1", "g/n|a"))
	cfg.Connection = []byte(`{"broker":"mqtt://b","format":"sparkplug_b"}`)
	in, err := newInput(cfg, nil, Options{}, deps{})
	if err != nil {
		t.Fatal(err)
	}
	if in.sparkplug == nil || in.plan != nil {
		t.Errorf("sparkplug %v plan %v", in.sparkplug, in.plan)
	}
	cfg.Devices[0].Points[0].Address = "g|a"
	if _, err := newInput(cfg, nil, Options{}, deps{}); err == nil {
		t.Error("a bad Sparkplug address must be refused")
	}
}

func TestPlanSparkplugSameNameEntriesMerge(t *testing.T) {
	plan, err := PlanSparkplug(spInput(spDevice("d", "g/n|a"), spDevice("d", "g/n|b")))
	if err != nil {
		t.Fatal(err)
	}
	node := SparkplugTarget{Group: "g", Node: "n"}
	if len(plan.ByDevice) != 1 || len(plan.Targets) != 1 {
		t.Fatalf("ByDevice %v Targets %v", plan.ByDevice, plan.Targets)
	}
	if tp := plan.Targets[node]; tp == nil || tp.Device != "d" || len(tp.Points) != 2 {
		t.Errorf("merged plan = %+v", tp)
	}
}
