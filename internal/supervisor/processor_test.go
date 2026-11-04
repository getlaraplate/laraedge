package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
	"github.com/getlaraplate/laraedge/internal/plugin"
)

const (
	// sinkBehaviour serves the output kind and keeps every batch.
	sinkBehaviour = `{"output":true,"trace_file":"trace","batches_file":"batches"}`
	// procBehaviour serves the processor kind and echoes every observed
	// sample as line-1/count.
	procBehaviour = `{"processor":true,"trace_file":"trace"}`
)

// pluginOutput is output id of type plugin:<pluginID> routing everything,
// sealing a batch per sample, with the config object cfg.
func pluginOutput(id, pluginID string, cfg map[string]any) map[string]any {
	if cfg == nil {
		cfg = map[string]any{}
	}

	return map[string]any{
		"id": id, "type": plugin.DriverPrefix + pluginID,
		"route":  map[string]any{"include": []any{"*"}},
		"batch":  map[string]any{"max_samples": 1, "max_wait": "1s"},
		"config": cfg,
	}
}

// processorSection is processor id of plugin pluginID observing observe and
// deriving line-1/count (int).
func processorSection(id, pluginID string, cfg map[string]any, observe ...string) map[string]any {
	if cfg == nil {
		cfg = map[string]any{}
	}

	return map[string]any{
		"id": id, "plugin": pluginID, "observe": observe,
		"devices": []any{map[string]any{"device": "line-1", "signals": []any{
			map[string]any{"signal": "count", "type": "int"},
		}}},
		"config": cfg,
	}
}

// read returns the file name of the plugin's directory, "" when missing.
func (p installedPlugin) read(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return string(data)
}

// waitFile waits until cond holds on the file name of the plugin.
func (p installedPlugin) waitFile(t *testing.T, name, what string, cond func(string) bool) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for !cond(p.read(t, name)) {
		if time.Now().After(deadline) {
			t.Fatalf("%s of plugin %s: %s not reached:\n%s", name, p.id, what, p.read(t, name))
		}
		time.Sleep(time.Millisecond)
	}
}

// waitFor polls cond every millisecond until it holds; guard bounds the
// wait.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(guard)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s not reached", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// restartClock is the fake clock as the supervisor sees it, counting the
// restart waits of plugin hosts once their timer is pending: a test that
// kills plugins advances the clock exactly when every host waits on it,
// and only then, so no deadline of a running call expires meanwhile.
type restartClock struct {
	*clock.Fake

	restarts atomic.Int64
}

// withRestartClock makes the supervisor use c over the fixture's fake clock.
func withRestartClock(c *restartClock) fixtureOption {
	return func(cfg *Config) {
		c.Fake = cfg.Clock.(*clock.Fake)
		cfg.Clock = c
	}
}

// Sleep is clock.Fake's Sleep, counting a host restart wait once its timer
// is pending. It mirrors clock.Fake.Sleep. A restart wait is the host's
// driverkit.Backoff, which draws at least BackoffMin/2; the only other
// sleepers in these tests are the plugin busy waits of busyWait (100 ms;
// plugin.TestBusyWaitStaysBelowBackoffFloor guards the gap), so
// the duration tells them apart and no stack frame is read. The count cannot
// come from BlockUntil: the hosts' health, memory and deadline timers are
// pending too.
func (c *restartClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	timer := c.NewTimer(d)
	defer timer.Stop()
	if d >= driverkit.BackoffMin/2 {
		c.restarts.Add(1)
	}

	select {
	case <-timer.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// batchItems returns the records of a batches file as device/signal=int
// for a sample and "birth device" for a birth.
func batchItems(batches string) []string {
	var items []string
	for line := range strings.SplitSeq(strings.TrimSpace(batches), "\n") {
		var b struct {
			Records []struct {
				Sample *struct {
					Device, Signal string
					Value          struct{ Int int64 }
				}
				Event *struct {
					Device  string
					Signals []struct{ Signal string }
				}
			}
		}
		if json.Unmarshal([]byte(line), &b) != nil {
			continue
		}
		for _, r := range b.Records {
			switch {
			case r.Sample != nil:
				items = append(items, fmt.Sprintf("%s/%s=%d", r.Sample.Device, r.Sample.Signal, r.Sample.Value.Int))
			case r.Event != nil && len(r.Event.Signals) > 0:
				items = append(items, "birth "+r.Event.Device)
			}
		}
	}

	return items
}

// batchHolds reports whether the batches file holds device/count=value.
func batchHolds(device string, value int) func(string) bool {
	want := fmt.Sprintf("%s/count=%d", device, value)

	return func(batches string) bool { return slices.Contains(batchItems(batches), want) }
}

// processorDocument is a document with input a (device a1), output main
// (HTTP), plugin output out on sink and processor line on proc observing
// a1.
func (f *fixture) processorDocument(version int64, proc, sink installedPlugin, outCfg, procCfg map[string]any) config.Document {
	f.t.Helper()

	return f.extendedDocument(f.document(version, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}), extras{
		plugins:    []any{proc.entry(nil), sink.entry(nil)},
		outputs:    []any{pluginOutput("out", "sink", outCfg)},
		processors: []any{processorSection("line", "proc", procCfg, "a1")},
	})
}

// TestProcessorAndPluginOutputRunEndToEnd: machine data and the data the
// processor derives from it reach both the HTTP output and the plugin
// output, the derived device with its birth.
func TestProcessorAndPluginOutputRunEndToEnd(t *testing.T) {
	f, _ := newPluginFixture(t)
	proc := installPlugin(t, "proc", procBehaviour)
	sink := installPlugin(t, "sink", sinkBehaviour)

	f.mustApply(f.processorDocument(1, proc, sink, nil, nil))
	proc.waitFile(t, "trace", "the stream open", func(s string) bool { return strings.Contains(s, "process line") })
	f.drv.instance(t, "a").sendValue("a1", "count", model.IntValue(7))

	f.be.waitDetail(t, "main", "a1/count=7 good", 1)
	f.be.waitDetail(t, "main", "birth line-1 [count:number]", 1)
	f.be.waitDetail(t, "main", "line-1/count=7 good", 1)
	sink.waitFile(t, "batches", "machine and derived data", func(s string) bool {
		return batchHolds("a1", 7)(s) && batchHolds("line-1", 7)(s) && slices.Contains(batchItems(s), "birth line-1")
	})

	st := f.sup.Status()
	want := []ProcessorStatus{{ID: "line", Plugin: "proc", State: ProcessorUp}}
	if !reflect.DeepEqual(st.Processors, want) {
		t.Fatalf("Status().Processors = %+v, want %+v", st.Processors, want)
	}
	i := slices.IndexFunc(st.Outputs, func(o OutputStatus) bool { return o.ID == "out" })
	if i < 0 || st.Outputs[i].Type != "plugin:sink" || st.Outputs[i].State != "sending" || st.Outputs[i].LastConfirmation.IsZero() {
		t.Fatalf("Status().Outputs = %+v, want out of type plugin:sink sending with a confirmation", st.Outputs)
	}
	if len(st.Plugins) != 2 {
		t.Fatalf("Status().Plugins = %+v, want proc and sink", st.Plugins)
	}
}

// TestProcessorOrPluginOutputRefusedRollsBack: a processor or a plugin
// output whose plugin refuses its section rolls the document back, the
// problems named at the component's document path (ruling R6), and the
// plugins the document started have exited.
func TestProcessorOrPluginOutputRefusedRollsBack(t *testing.T) {
	tests := []struct {
		name       string
		procExtra  string
		sinkExtra  string
		wantErrors []string
	}{
		{"processor", `,"refuse_config":true`, "", []string{"processors.0.config.port: must be an integer"}},
		{"plugin output", "", `,"refuse_config":true`, []string{"outputs.1.config.port: must be an integer"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newPluginFixture(t)
			proc := installPlugin(t, "proc", strings.TrimSuffix(procBehaviour, "}")+tc.procExtra+`,"pid_file":"pid"}`)
			sink := installPlugin(t, "sink", strings.TrimSuffix(sinkBehaviour, "}")+tc.sinkExtra+`,"pid_file":"pid"}`)
			refusing := proc
			if tc.sinkExtra != "" {
				refusing = sink
			}
			for _, p := range []installedPlugin{proc, sink} {
				// installPlugin ran the binary for its manifest.
				if err := os.Remove(filepath.Join(p.dir, "pid")); err != nil {
					t.Fatal(err)
				}
			}
			f.mustApply(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}))

			out := f.apply(f.processorDocument(2, proc, sink, nil, nil))

			if out.Result != ResultRolledBack || !slices.Equal(out.Errors, tc.wantErrors) {
				t.Fatalf("Apply() = %+v, want rolled_back with %q", out, tc.wantErrors)
			}
			st := f.sup.Status()
			if st.Version != 1 || len(st.Processors) != 0 || len(st.Plugins) != 0 || len(st.Outputs) != 1 {
				t.Fatalf("Status() = %+v, want version 1 with main only", st)
			}
			if refusing.read(t, "pid") == "" {
				t.Fatalf("plugin %s wrote no pid: it was never started", refusing.id)
			}
			for _, p := range []installedPlugin{proc, sink} {
				data := p.read(t, "pid")
				if data == "" {
					continue
				}
				pid, err := strconv.Atoi(data)
				if err != nil {
					t.Fatalf("pid file of %s = %q", p.id, data)
				}
				if alive(pid) {
					t.Errorf("plugin %s (pid %d) still runs after the rollback, want it exited", p.id, pid)
				}
			}
			f.drv.instance(t, "a").send("a1", "state")
			f.be.waitItem(t, "main", "a1/state")
		})
	}
}

// sentBatch is one batch a plugin output received: its source_seq and its
// records as batchItems renders them.
type sentBatch struct {
	seq   uint64
	items []string
}

// sentBatches returns the batches output id of plugin p received, in the
// order of its sends, each message_id once: the trace names the sends of
// each output, the batches file holds their content.
func (p installedPlugin) sentBatches(t *testing.T, id string) []sentBatch {
	t.Helper()

	byID := map[string]sentBatch{}
	for line := range strings.SplitSeq(strings.TrimSpace(p.read(t, "batches")), "\n") {
		var b struct {
			MessageID string
			SourceSeq uint64
		}
		if json.Unmarshal([]byte(line), &b) != nil {
			continue
		}
		if _, ok := byID[b.MessageID]; !ok {
			byID[b.MessageID] = sentBatch{seq: b.SourceSeq, items: batchItems(line)}
		}
	}

	var out []sentBatch
	seen := map[string]bool{}
	for line := range strings.SplitSeq(p.read(t, "trace"), "\n") {
		messageID, ok := strings.CutPrefix(line, "send "+id+" ")
		if !ok || seen[messageID] {
			continue
		}
		seen[messageID] = true
		if b, ok := byID[messageID]; ok {
			out = append(out, b)
		}
	}

	return out
}

// TestReloadOfOnePluginOutputConfigKeepsThePluginProcess is the review
// focus: a document changing only one plugin output's config keeps the
// plugin's pid; only that output is stopped and started, configured again,
// and its buffer keeps what it holds: the records o1 held at the reload,
// never sent (its batch waits for 100 records or an hour of the fake
// clock), reach the plugin after it, in order, with consecutive source_seq.
func TestReloadOfOnePluginOutputConfigKeepsThePluginProcess(t *testing.T) {
	f, _ := newPluginFixture(t)
	sink := installPlugin(t, "sink", sinkBehaviour)
	doc := func(version int64, o1, o1Batch map[string]any) config.Document {
		held := pluginOutput("o1", "sink", o1)
		held["batch"] = o1Batch

		return f.extendedDocument(f.document(version, []inSpec{{id: "a", devices: []string{"a1"}}}, nil), extras{
			plugins: []any{sink.entry(nil)},
			outputs: []any{held, pluginOutput("o2", "sink", nil)},
		})
	}

	f.mustApply(doc(1, map[string]any{"topic": "x"}, map[string]any{"max_samples": 100, "max_wait": "1h"}))
	pid := f.runningPluginPid("sink")
	o1Buf, o2Buf := f.output("o1").buf, f.output("o2").buf
	a := f.drv.instance(t, "a")
	for v := int64(5); v <= 7; v++ {
		a.sendValue("a1", "count", model.IntValue(v))
	}
	sink.waitFile(t, "batches", "o2 sending", func(s string) bool { return countItems(batchItems(s), "a1/count=7") == 1 })
	waitFor(t, "o1 holding the three samples", func() bool { return o1Buf.Records() >= 3 })
	if strings.Contains(sink.read(t, "trace"), "send o1 ") {
		t.Fatalf("o1 sent before the reload, want its records held:\n%s", sink.read(t, "trace"))
	}

	f.mustApply(doc(2, map[string]any{"topic": "y"}, map[string]any{"max_samples": 1, "max_wait": "1s"}))
	a.sendValue("a1", "count", model.IntValue(8))

	if got := f.runningPluginPid("sink"); got != pid {
		t.Fatalf("plugin pid = %d after the reload, want %d kept", got, pid)
	}
	if got := sink.traceCount(t, "configure o1"); got != 2 {
		t.Errorf("o1 configured %d times, want 2", got)
	}
	if got := sink.traceCount(t, "configure o2"); got != 1 {
		t.Errorf("o2 configured %d times, want 1 (unchanged)", got)
	}
	if f.output("o2").buf != o2Buf {
		t.Error("o2 was restarted, want it untouched")
	}
	if f.output("o1").buf == o1Buf {
		t.Error("o1 kept its running instance, want it stopped and started")
	}
	sink.waitFile(t, "batches", "o1 and o2 sending after the reload", func(s string) bool {
		return countItems(batchItems(s), "a1/count=8") == 2
	})

	batches := sink.sentBatches(t, "o1")
	var samples []string
	for i, b := range batches {
		if i > 0 && b.seq != batches[i-1].seq+1 {
			t.Errorf("o1 source_seq %d follows %d, want consecutive", b.seq, batches[i-1].seq)
		}
		for _, item := range b.items {
			if strings.HasPrefix(item, "a1/") {
				samples = append(samples, item)
			}
		}
	}
	if want := []string{"a1/count=5", "a1/count=6", "a1/count=7", "a1/count=8"}; !slices.Equal(samples, want) {
		t.Errorf("o1 received %v in %d batches, want %v (the held records first, in order)", samples, len(batches), want)
	}
}

// TestPluginCrashRestartsItsProcessorsAndOutputs: the plugins of a
// processor and of an output are killed; machine data still reaches the
// HTTP output, the derived device goes bad, and once the plugins are back
// the processor streams again and the output, configured again, delivers
// everything its buffer kept meanwhile.
func TestPluginCrashRestartsItsProcessorsAndOutputs(t *testing.T) {
	rc := &restartClock{}
	f, _ := newPluginFixture(t, withRestartClock(rc))
	proc := installPlugin(t, "proc", procBehaviour)
	sink := installPlugin(t, "sink", sinkBehaviour)
	f.mustApply(f.processorDocument(1, proc, sink, nil, nil))
	proc.waitFile(t, "trace", "the stream open", func(s string) bool { return strings.Contains(s, "process line") })
	a := f.drv.instance(t, "a")
	a.sendValue("a1", "count", model.IntValue(1))
	sink.waitFile(t, "batches", "the derived sample", batchHolds("line-1", 1))

	for _, id := range []string{"proc", "sink"} {
		p, err := os.FindProcess(f.runningPluginPid(id))
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Kill(); err != nil {
			t.Fatal(err)
		}
	}
	f.waitStatus("both plugins down", func(st Status) bool {
		return len(st.Plugins) == 2 && st.Plugins[0].Restarts == 1 && st.Plugins[1].Restarts == 1 &&
			st.Plugins[0].State != string(plugin.StateUp) && st.Plugins[1].State != string(plugin.StateUp)
	})

	a.sendValue("a1", "count", model.IntValue(2))
	a.sendValue("a1", "count", model.IntValue(3))
	f.be.waitDetail(t, "main", "a1/count=3 good", 1)
	f.be.waitDetail(t, "main", "line-1/count=0 bad", 1)

	// Both hosts wait their first restart, at most 1 s: one step of the
	// clock starts them again. Then nothing waits on the clock: the
	// processor streams again and the output, woken by its plugin, delivers
	// what its buffer kept.
	waitFor(t, "both plugins waiting to restart", func() bool { return rc.restarts.Load() >= 2 })
	f.clk.Advance(time.Second)
	f.waitStatus("both plugins back", func(st Status) bool {
		return len(st.Plugins) == 2 && st.Plugins[0].State == string(plugin.StateUp) && st.Plugins[1].State == string(plugin.StateUp)
	})
	waitFor(t, "the processor streaming again and the output delivering what it kept", func() bool {
		s := sink.read(t, "batches")
		return batchHolds("a1", 2)(s) && batchHolds("a1", 3)(s) && proc.traceCount(t, "process line") >= 2
	})

	if got := sink.traceCount(t, "configure out"); got != 2 {
		t.Errorf("out configured %d times, want 2 (start, plugin restart)", got)
	}
	if got := proc.traceCount(t, "configure line"); got != 2 {
		t.Errorf("line configured %d times, want 2 (start, plugin restart)", got)
	}
	a.sendValue("a1", "count", model.IntValue(4))
	f.be.waitDetail(t, "main", "line-1/count=4 good", 1)
}

// TestRemovingTheLastProcessorAndOutputStopsTheirPlugins: plugins used only
// by a processor and a plugin output stop when the document removes them.
func TestRemovingTheLastProcessorAndOutputStopsTheirPlugins(t *testing.T) {
	f, _ := newPluginFixture(t)
	proc := installPlugin(t, "proc", procBehaviour)
	sink := installPlugin(t, "sink", sinkBehaviour)
	f.mustApply(f.processorDocument(1, proc, sink, nil, nil))
	f.runningPluginPid("proc")
	f.runningPluginPid("sink")

	f.mustApply(f.extendedDocument(f.document(2, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}),
		extras{plugins: []any{proc.entry(nil), sink.entry(nil)}}))

	st := f.sup.Status()
	if len(st.Plugins) != 0 || len(st.Processors) != 0 || len(st.Outputs) != 1 {
		t.Fatalf("Status() = %+v, want main only, no plugin", st)
	}
	if got := proc.traceCount(t, "process end line"); got != 1 {
		t.Errorf("the stream of line ended %d times, want 1", got)
	}
}

// TestChangedPluginEntryRestartsItsProcessorsAndOutputs is decision Q6: a
// changed plugins entry restarts the plugin with every component on it.
func TestChangedPluginEntryRestartsItsProcessorsAndOutputs(t *testing.T) {
	f, _ := newPluginFixture(t)
	proc := installPlugin(t, "proc", procBehaviour)
	sink := installPlugin(t, "sink", sinkBehaviour)
	f.mustApply(f.processorDocument(1, proc, sink, nil, nil))
	outBuf := f.output("out").buf

	f.mustApply(f.extendedDocument(f.document(2, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}), extras{
		plugins:    []any{proc.entry(map[string]any{"max_memory": "1GiB"}), sink.entry(map[string]any{"max_memory": "1GiB"})},
		outputs:    []any{pluginOutput("out", "sink", nil)},
		processors: []any{processorSection("line", "proc", nil, "a1")},
	}))

	if f.output("out").buf == outBuf {
		t.Error("out kept its running instance, want it restarted with its plugin")
	}
	if got := sink.traceCount(t, "configure out"); got != 2 {
		t.Errorf("out configured %d times, want 2", got)
	}
	if got := proc.traceCount(t, "configure line"); got != 2 {
		t.Errorf("line configured %d times, want 2", got)
	}
}

// TestCloseStopsProcessorsBeforeTheirPlugin: Close stops the inputs, then
// the processors, then the plugins, then the outputs.
func TestCloseStopsProcessorsBeforeTheirPlugin(t *testing.T) {
	f, _ := newPluginFixture(t)
	proc := installPlugin(t, "proc", procBehaviour)
	sink := installPlugin(t, "sink", sinkBehaviour)
	f.mustApply(f.processorDocument(1, proc, sink, nil, nil))
	var steps []string
	f.sup.trace = func(step string) { steps = append(steps, step) }

	if err := f.sup.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	got := steps
	want := []string{"stop input a", "stop processor line", "stop plugin proc", "stop plugin sink", "stop output main", "stop output out"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Close steps = %q, want %q", got, want)
	}
}

// TestProcessorObservesRecordsAfterTheTransforms: the processor receives
// what the input's point transforms emit (the plugin's 21 scaled by 2),
// not what the plugin read.
func TestProcessorObservesRecordsAfterTheTransforms(t *testing.T) {
	f, _ := newPluginFixture(t)
	demo := installPlugin(t, "demo", `{"script":[{"sample":{"device":"d1","signal":"count","value":21,"ts":"2026-10-19T08:00:00Z","quality":"good"}}]}`)
	proc := installPlugin(t, "proc", `{"processor":true,"records_file":"records"}`)

	in := pluginInput("p1", "demo", "d1", 0, "count")
	f.mustApply(f.extendedDocument(f.document(1, nil, []outSpec{{id: "main"}}), extras{
		plugins:    []any{demo.entry(nil), proc.entry(nil)},
		inputs:     []any{in},
		processors: []any{processorSection("line", "proc", nil, "d1")},
	}))

	f.be.waitDetail(t, "main", "d1/count=42 good", 1)
	// The scale made the plugin's int 21 the float 42.
	proc.waitFile(t, "records", "the transformed sample", func(s string) bool {
		return strings.Contains(s, `"Device":"d1","Signal":"count"`) && strings.Contains(s, `"Float":42,`) && !strings.Contains(s, `"Int":21`)
	})
}

// TestProcessorWithTheIDOfAnInput is ruling R8: a processor whose id is
// that of an input keeps both registrations apart, so an output starting
// again receives the births of the input's devices and of the derived
// ones, and removing the processor leaves the input's devices known.
func TestProcessorWithTheIDOfAnInput(t *testing.T) {
	f, _ := newPluginFixture(t)
	proc := installPlugin(t, "proc", procBehaviour)
	doc := func(version int64, variant int, withProcessor bool) config.Document {
		x := extras{plugins: []any{proc.entry(nil)}}
		if withProcessor {
			x.processors = []any{processorSection("a", "proc", nil, "a1")}
		}

		return f.extendedDocument(f.document(version, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main", variant: variant}}), x)
	}

	f.mustApply(doc(1, 0, true))
	f.be.waitDetail(t, "main", "birth a1 [state:string count:number]", 1)
	f.be.waitDetail(t, "main", "birth line-1 [count:number]", 1)

	f.mustApply(doc(2, 1, true))
	f.be.waitDetail(t, "main", "birth a1 [state:string count:number]", 2)
	f.be.waitDetail(t, "main", "birth line-1 [count:number]", 2)

	f.mustApply(doc(3, 2, false))
	f.be.waitDetail(t, "main", "birth a1 [state:string count:number]", 3)
	if got := countItems(f.be.detailOf("main"), "birth line-1 [count:number]"); got != 2 {
		t.Fatalf("line-1 born %d times, want 2: the removed processor's devices are forgotten", got)
	}
}

// TestProcessorReceivesTheFirstBirthsOfItsObservedDevices is decision Q4
// for births: a processor receives the birth an input sends at its start,
// whether the input starts with it or was already running when a later
// document added the processor.
func TestProcessorReceivesTheFirstBirthsOfItsObservedDevices(t *testing.T) {
	const birthOfA1 = `"Event":{"Kind":1,"Device":"a1"`
	tests := []struct {
		name  string
		apply func(f *fixture, proc installedPlugin, processorDoc func(int64) config.Document)
	}{
		{"input started with the processor", func(f *fixture, _ installedPlugin, processorDoc func(int64) config.Document) {
			f.mustApply(processorDoc(1))
		}},
		{"processor added to a running input", func(f *fixture, proc installedPlugin, processorDoc func(int64) config.Document) {
			f.mustApply(f.extendedDocument(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}),
				extras{plugins: []any{proc.entry(nil)}}))
			f.be.waitDetail(f.t, "main", "birth a1 [state:string count:number]", 1)
			f.mustApply(processorDoc(2))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newPluginFixture(t)
			proc := installPlugin(t, "proc", `{"processor":true,"trace_file":"trace","records_file":"records"}`)
			processorDoc := func(version int64) config.Document {
				return f.extendedDocument(f.document(version, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}), extras{
					plugins:    []any{proc.entry(nil)},
					processors: []any{processorSection("line", "proc", nil, "a1")},
				})
			}

			tc.apply(f, proc, processorDoc)

			proc.waitFile(t, "records", "the birth of a1", func(s string) bool { return strings.Contains(s, birthOfA1) })
		})
	}
}
