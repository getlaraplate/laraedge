package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/config"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/transform"
)

// tpoint is a point of device a1 with its transform chain, each step a
// {name: params} object.
type tpoint struct {
	signal, typ string
	steps       []any
}

func scaleBy(factor float64) []any { return []any{map[string]any{"scale": factor}} }

func edgeCounting(mode string) []any { return []any{map[string]any{"edge_counter": mode}} }

// transformDocument builds a document with input a, reading device a1 with
// points, and output main; the variant changes input a without changing
// what it reads. extra adds inputs built as document does.
func (f *fixture) transformDocument(version int64, variant int, points []tpoint, extra ...inSpec) config.Document {
	f.t.Helper()

	return f.transformDocumentFor("a", version, variant, points, extra...)
}

// transformDocumentFor is transformDocument with the transformed input
// named id.
func (f *fixture) transformDocumentFor(id string, version int64, variant int, points []tpoint, extra ...inSpec) config.Document {
	f.t.Helper()

	var pts []any
	for _, p := range points {
		pt := map[string]any{"signal": p.signal, "address": p.signal, "read": "subscribe", "type": p.typ}
		if p.steps != nil {
			pt["transforms"] = p.steps
		}
		pts = append(pts, pt)
	}

	// The extra inputs and the output come from document; input a replaces
	// its first input.
	base := f.document(version, append([]inSpec{{id: id, devices: []string{"a1"}}}, extra...), []outSpec{{id: "main"}})

	tree := map[string]any{"schema": config.SchemaName, "version": version}

	ins := []any{map[string]any{
		"id": id, "driver": config.DriverSimulated,
		"connection": map[string]any{"variant": variant},
		"devices":    []any{map[string]any{"device": "a1", "points": pts}},
	}}
	for _, in := range base.Inputs[1:] {
		var raw any
		if err := json.Unmarshal(in.Raw, &raw); err != nil {
			f.t.Fatal(err)
		}
		ins = append(ins, raw)
	}
	tree["inputs"] = ins

	var outs []any
	for _, out := range base.Outputs {
		var raw any
		if err := json.Unmarshal(out.Raw, &raw); err != nil {
			f.t.Fatal(err)
		}
		outs = append(outs, raw)
	}
	tree["outputs"] = outs

	return f.parse(tree)
}

// counters returns the counter values of input a's state file, sorted.
func (f *fixture) counters() []int64 {
	f.t.Helper()

	return f.countersOf("a")
}

// countersOf returns the counter values of input id's state file, sorted.
func (f *fixture) countersOf(id string) []int64 {
	f.t.Helper()

	data, err := os.ReadFile(filepath.Join(f.dataDir, "inputs", id, "transform-state.json"))
	if err != nil {
		f.t.Fatalf("state file: %v", err)
	}

	var doc struct {
		Counters map[string]int64 `json:"counters"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		f.t.Fatalf("state file: %v", err)
	}

	values := make([]int64, 0, len(doc.Counters))
	for _, v := range doc.Counters {
		values = append(values, v)
	}
	slices.Sort(values)

	return values
}

// samplesOf returns the samples of a1/signal that main received, as detail
// writes them, in order.
func (f *fixture) samplesOf(signal string) []string {
	var out []string
	for _, item := range f.be.detailOf("main") {
		if strings.HasPrefix(item, "a1/"+signal+"=") {
			out = append(out, item)
		}
	}

	return out
}

// TestTransformsAppliedEndToEnd: a chain of the applied document transforms
// what the input emits before the outputs, and the birth carries the data
// type of what the chain produces.
func TestTransformsAppliedEndToEnd(t *testing.T) {
	f := newFixture(t)
	f.mustApply(f.transformDocument(1, 0, []tpoint{
		{signal: "temp", typ: "int16", steps: scaleBy(0.1)},
		{signal: "door", typ: "bool", steps: edgeCounting("rising")},
		{signal: "state", typ: "string"},
	}))

	f.be.waitDetail(t, "main", "birth a1 [temp:number door:number state:string]", 1)

	a := f.drv.instance(t, "a")
	a.sendValue("a1", "temp", model.IntValue(215))
	a.sendValue("a1", "state", model.StringValue("RUN"))

	f.be.waitDetail(t, "main", "a1/state=RUN good", 1)
	if got, want := f.samplesOf("temp"), []string{"a1/temp=21.5 good"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("main received temp %q, want the scaled value %q", got, want)
	}
}

// TestApplyRejectsAChainThatDoesNotTypeCheck: a chain CheckDocument refuses
// rejects the document with its message, the running configuration
// untouched (decision P1).
func TestApplyRejectsAChainThatDoesNotTypeCheck(t *testing.T) {
	f := newFixture(t)
	first := f.transformDocument(1, 0, []tpoint{{signal: "temp", typ: "int16", steps: scaleBy(0.1)}})
	f.mustApply(first)

	bad := f.transformDocument(2, 1, []tpoint{{signal: "temp", typ: "string", steps: scaleBy(0.1)}})
	want := transform.CheckDocument(bad)
	if len(want) != 1 || !strings.HasPrefix(want[0], "inputs.0.devices.0.points.0.transforms: input a, device a1, signal temp, step 1 (scale)") {
		t.Fatalf("CheckDocument() = %q, want one message for the scale of a string", want)
	}

	out := f.apply(bad)

	if out.Result != ResultRejected || !reflect.DeepEqual(out.Errors, want) {
		t.Fatalf("Apply() = %+v, want rejected with %q", out, want)
	}
	if n := f.drv.startCount("a"); n != 1 {
		t.Errorf("input a started %d times, want the running configuration untouched", n)
	}
	if got := f.drv.instance(t, "a").cfg.Raw; string(got) != string(first.Inputs[0].Raw) {
		t.Errorf("input a runs %s, want %s", got, first.Inputs[0].Raw)
	}
	if v := f.sup.AppliedVersion(); v != 1 {
		t.Errorf("AppliedVersion() = %d, want 1", v)
	}

	f.drv.instance(t, "a").sendValue("a1", "temp", model.IntValue(215))
	f.be.waitDetail(t, "main", "a1/temp=21.5 good", 1)
}

// TestRestartedInputClosesItsProcessor: a changed input closes its
// processor before the new one reads the state file, so the old one never
// writes it again (ruling R11), and an unchanged edge_counter step keeps its
// count while a changed one starts clean (decision P2).
func TestRestartedInputClosesItsProcessor(t *testing.T) {
	f := newFixture(t)
	f.mustApply(f.transformDocument(1, 0, []tpoint{
		{signal: "door", typ: "bool", steps: edgeCounting("rising")},
		{signal: "gate", typ: "bool", steps: edgeCounting("rising")},
	}))

	old := f.drv.instance(t, "a")
	old.sendValue("a1", "door", model.BoolValue(false))
	old.sendValue("a1", "door", model.BoolValue(true))
	old.sendValue("a1", "gate", model.BoolValue(false))
	old.sendValue("a1", "gate", model.BoolValue(true))
	f.be.waitDetail(t, "main", "a1/gate=1 good", 1)
	if got := f.counters(); !reflect.DeepEqual(got, []int64{1, 1}) {
		t.Fatalf("counters = %v, want [1 1]", got)
	}

	f.mustApply(f.transformDocument(2, 1, []tpoint{
		{signal: "door", typ: "bool", steps: edgeCounting("rising")},
		{signal: "gate", typ: "bool", steps: edgeCounting("both")},
	}))

	// The previous instance's emit reaches a closed processor: it neither
	// counts nor writes the state file back with the pruned gate key.
	old.sendValue("a1", "door", model.BoolValue(false))
	old.sendValue("a1", "door", model.BoolValue(true))
	if got := f.counters(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("counters after the restart = %v, want [1]: only door, untouched by the stopped input", got)
	}

	a := f.drv.instance(t, "a")
	a.sendValue("a1", "door", model.BoolValue(false))
	a.sendValue("a1", "door", model.BoolValue(true))
	a.sendValue("a1", "gate", model.BoolValue(true))
	a.sendValue("a1", "gate", model.BoolValue(false))
	f.be.waitDetail(t, "main", "a1/gate=1 good", 2)

	if got, want := f.samplesOf("door"), []string{"a1/door=0 good", "a1/door=1 good", "a1/door=1 good", "a1/door=2 good"}; !reflect.DeepEqual(got, want) {
		t.Errorf("main received door %q, want %q: the unchanged step keeps its count", got, want)
	}
	if got, want := f.samplesOf("gate"), []string{"a1/gate=0 good", "a1/gate=1 good", "a1/gate=0 good", "a1/gate=1 good"}; !reflect.DeepEqual(got, want) {
		t.Errorf("main received gate %q, want %q: the changed step starts clean", got, want)
	}
}

// TestRolledBackInputRebuildsItsProcessor: a rollback closes the processor
// of the failed document before it rebuilds the previous one from the state
// file, where an unchanged edge_counter step kept its count; the processor of
// an abandoned start is closed at once, its input still starting (ruling
// R11: never two live processors on one state file).
func TestRolledBackInputRebuildsItsProcessor(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*fixture, config.Document) Outcome
	}{
		{
			name: "another input does not start",
			apply: func(f *fixture, doc config.Document) Outcome {
				f.drv.failStart["b"] = errRefused

				return f.apply(doc)
			},
		},
		{
			name: "the changed input does not start in time",
			apply: func(f *fixture, doc config.Document) Outcome {
				var release sync.Once
				f.t.Cleanup(func() { release.Do(func() { close(f.drv.blockRelease) }) })
				f.drv.blockNth["a"] = 2

				done := make(chan Outcome, 1)
				go func() { done <- f.apply(doc) }()
				select {
				case <-f.drv.blockStarted:
				case <-time.After(guard):
					f.t.Fatal("the changed input a did not start")
				}
				f.clk.Advance(DefaultStartTimeout)
				select {
				case out := <-done:
					return out
				case <-time.After(guard):
					f.t.Fatal("Apply did not return after the start timeout")

					return Outcome{}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			door := []tpoint{{signal: "door", typ: "bool", steps: edgeCounting("rising")}}
			doc1 := f.transformDocument(1, 0, door)
			f.mustApply(doc1)

			a := f.drv.instance(t, "a")
			a.sendValue("a1", "door", model.BoolValue(false))
			a.sendValue("a1", "door", model.BoolValue(true))
			f.be.waitDetail(t, "main", "a1/door=1 good", 1)

			out := tt.apply(f, f.transformDocument(2, 1, door, inSpec{id: "b", devices: []string{"b1"}}))
			if out.Result != ResultRolledBack {
				t.Fatalf("Apply() = %+v, want rolled_back", out)
			}

			// The emit of the failed document's start of a reaches a closed
			// processor: it neither counts nor writes the state file.
			failed := f.drv.emitOf(t, "a", 2)
			for _, v := range []bool{false, true} {
				failed(model.SampleRecord(model.Sample{
					Device: "a1", Signal: "door", TS: start, Value: model.BoolValue(v),
					Quality: model.Good, Priority: model.Critical,
				}))
			}
			if got := f.counters(); !reflect.DeepEqual(got, []int64{1}) {
				t.Fatalf("counters = %v, want [1]: untouched by the failed document's processor", got)
			}

			a = f.drv.instance(t, "a")
			if got := a.cfg.Raw; string(got) != string(doc1.Inputs[0].Raw) {
				t.Fatalf("input a runs %s, want the previous configuration", got)
			}
			a.sendValue("a1", "door", model.BoolValue(false))
			a.sendValue("a1", "door", model.BoolValue(true))
			f.be.waitDetail(t, "main", "a1/door=2 good", 1)

			if got, want := f.samplesOf("door"), []string{"a1/door=0 good", "a1/door=1 good", "a1/door=1 good", "a1/door=2 good"}; !reflect.DeepEqual(got, want) {
				t.Errorf("main received door %q, want %q", got, want)
			}
		})
	}
}

// TestTransformErrorsCountedForTheAgentLifetime: the runtime errors of an
// input's chains are in its status, and the count survives the restart of
// the input, which replaces its processor.
func TestTransformErrorsCountedForTheAgentLifetime(t *testing.T) {
	f := newFixture(t)
	overflow := []tpoint{{signal: "temp", typ: "int16", steps: scaleBy(1e308)}}
	f.mustApply(f.transformDocument(1, 0, overflow))

	errorsOfA := func() uint64 {
		for _, in := range f.sup.Status().Inputs {
			if in.ID == "a" {
				return in.TransformErrors
			}
		}
		t.Fatal("input a missing from the status")

		return 0
	}

	f.drv.instance(t, "a").sendValue("a1", "temp", model.IntValue(215))
	f.be.waitDetail(t, "main", "a1/temp=0 bad", 1)
	if n := errorsOfA(); n != 1 {
		t.Fatalf("TransformErrors = %d, want 1", n)
	}

	f.mustApply(f.transformDocument(2, 1, overflow))
	if n := errorsOfA(); n != 1 {
		t.Fatalf("TransformErrors after the restart = %d, want 1", n)
	}

	f.drv.instance(t, "a").sendValue("a1", "temp", model.IntValue(215))
	f.be.waitDetail(t, "main", "a1/temp=0 bad", 2)
	if n := errorsOfA(); n != 2 {
		t.Fatalf("TransformErrors = %d, want 2", n)
	}
}

// door is the point of device a1 whose rising edges are counted.
var door = []tpoint{{signal: "door", typ: "bool", steps: edgeCounting("rising")}}

// countDoor sends the door's level false, then n rising edges, through
// emit; each count is saved before emit returns.
func countDoor(emit func(model.Record), n int) {
	send := func(v bool) {
		emit(model.SampleRecord(model.Sample{
			Device: "a1", Signal: "door", TS: start, Value: model.BoolValue(v),
			Quality: model.Good, Priority: model.Critical,
		}))
	}

	send(false)
	for range n {
		send(true)
		send(false)
	}
}

// emitOfRunning is the emit of the running instance of input id.
func (f *fixture) emitOfRunning(id string) func(model.Record) {
	f.t.Helper()

	in := f.drv.instance(f.t, id)

	return func(r model.Record) {
		f.drv.mu.Lock()
		emit := in.emit
		f.drv.mu.Unlock()
		emit(r)
	}
}

// stateDirExists reports whether the transform state directory of input id
// exists.
func (f *fixture) stateDirExists(id string) bool {
	f.t.Helper()

	_, err := os.Stat(filepath.Join(f.dataDir, "inputs", id))
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}

	return err == nil
}

// TestInputRenamedBackStartsItsCountsFromZero is ruling R15: input a counts,
// is renamed b, then renamed back. The backend keys counters by device and
// signal: a's old count resumed after b's would read as parts produced, so
// a's directory goes when the document without a is applied.
func TestInputRenamedBackStartsItsCountsFromZero(t *testing.T) {
	f := newFixture(t)

	f.mustApply(f.transformDocumentFor("a", 1, 0, door))
	countDoor(f.emitOfRunning("a"), 5)
	if got := f.countersOf("a"); !reflect.DeepEqual(got, []int64{5}) {
		t.Fatalf("counters of a = %v, want [5]", got)
	}

	f.mustApply(f.transformDocumentFor("b", 2, 0, door))
	if f.stateDirExists("a") {
		t.Fatal("the state directory of a outlived the document without a")
	}
	countDoor(f.emitOfRunning("b"), 2)

	f.mustApply(f.transformDocumentFor("a", 3, 0, door))
	if f.stateDirExists("b") {
		t.Fatal("the state directory of b outlived the document without b")
	}
	countDoor(f.emitOfRunning("a"), 1)
	if got := f.countersOf("a"); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("counters of a renamed back = %v, want [1]: counted from 0, not from the old 5", got)
	}
}

// TestInputRemovedAndAddedAgainStartsFromZero: a document without input a
// removes its state; the input added back counts from 0.
func TestInputRemovedAndAddedAgainStartsFromZero(t *testing.T) {
	f := newFixture(t)

	f.mustApply(f.transformDocument(1, 0, door))
	countDoor(f.emitOfRunning("a"), 3)

	f.mustApply(f.document(2, []inSpec{{id: "c", devices: []string{"c1"}}}, []outSpec{{id: "main"}}))
	if f.stateDirExists("a") {
		t.Fatal("the state directory of the removed input a is still there")
	}

	f.mustApply(f.transformDocument(3, 0, door))
	countDoor(f.emitOfRunning("a"), 1)
	if got := f.countersOf("a"); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("counters of a added back = %v, want [1]", got)
	}
}

// TestRollbackDoesNotResurrectARemovedCount: input a was removed by the
// applied document; a document adding it back is rolled back after a counted
// in it. The rollback leaves no state of a, so a later document with a
// counts from 0, not from the failed attempt's count.
func TestRollbackDoesNotResurrectARemovedCount(t *testing.T) {
	f := newFixture(t)

	f.mustApply(f.transformDocumentFor("a", 1, 0, door))
	countDoor(f.emitOfRunning("a"), 5)
	f.mustApply(f.transformDocumentFor("b", 2, 0, door))
	countDoor(f.emitOfRunning("b"), 2)

	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(f.drv.blockRelease) }) })
	f.drv.blockNth["c"] = 1

	done := make(chan Outcome, 1)
	go func() {
		done <- f.apply(f.transformDocumentFor("a", 3, 0, door, inSpec{id: "c", devices: []string{"c1"}}))
	}()
	select {
	case <-f.drv.blockStarted:
	case <-time.After(guard):
		t.Fatal("input c did not start")
	}

	// Input a started before c and counts while c hangs.
	countDoor(f.emitOfRunning("a"), 3)
	if got := f.countersOf("a"); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("counters of a during the attempt = %v, want [3]", got)
	}

	f.clk.Advance(DefaultStartTimeout)
	select {
	case out := <-done:
		if out.Result != ResultRolledBack {
			t.Fatalf("Apply() = %+v, want rolled_back", out)
		}
	case <-time.After(guard):
		t.Fatal("Apply did not return after the start timeout")
	}

	if f.stateDirExists("a") {
		t.Fatal("the rollback left the state of a, which the restored document does not have")
	}
	if !f.stateDirExists("b") {
		t.Fatal("the rollback removed the state of b, which the restored document has")
	}

	f.mustApply(f.transformDocumentFor("a", 4, 0, door))
	countDoor(f.emitOfRunning("a"), 1)
	if got := f.countersOf("a"); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("counters of a = %v, want [1]: neither the old 5 nor the attempt's 3", got)
	}
}

// TestFirstApplyRemovesStaleInputStateBeforeInputsStart: the first document
// of the agent's run removes the state of the inputs it does not have before
// it starts any input, even when it is rolled back; it touches nothing but
// the directories of data_dir/inputs.
func TestFirstApplyRemovesStaleInputStateBeforeInputsStart(t *testing.T) {
	f := newFixture(t)

	inputs := filepath.Join(f.dataDir, "inputs")
	for _, id := range []string{"a", "old"} {
		if err := os.MkdirAll(filepath.Join(inputs, id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inputs, id, "transform-state.json"), []byte(`{"version":1,"counters":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	notes := filepath.Join(inputs, "notes.txt")
	if err := os.WriteFile(notes, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}

	f.drv.failStart["a"] = errRefused
	if out := f.apply(f.transformDocument(1, 0, door)); out.Result != ResultRolledBack {
		t.Fatalf("Apply() = %+v, want rolled_back", out)
	}

	if f.stateDirExists("old") {
		t.Error("the state of input old, in no document, is still there")
	}
	if !f.stateDirExists("a") {
		t.Error("the state of input a, in the document, was removed")
	}
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("a file of data_dir/inputs was touched: %v", err)
	}
}
