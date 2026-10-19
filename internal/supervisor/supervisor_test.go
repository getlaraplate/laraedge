package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/input"
	"github.com/swolley/laraplate-edge/internal/secrets"
)

var start = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

type fixture struct {
	t       *testing.T
	clk     *clock.Fake
	dataDir string
	env     *envStore
	drv     *fakeDriver
	be      *backend
	logs    *lockedBuffer
	sup     *Supervisor
}

type fixtureOption func(*Config)

func withoutDrivers(c *Config) { c.Drivers = nil }

func newFixture(t *testing.T, opts ...fixtureOption) *fixture {
	t.Helper()

	f := &fixture{
		t:       t,
		clk:     clock.NewFake(start),
		dataDir: t.TempDir(),
		env:     &envStore{vars: map[string]string{tokenEnv: testToken}},
		drv:     newFakeDriver(),
		be:      newBackend(t),
		logs:    &lockedBuffer{},
	}

	cfg := Config{
		DataDir:  f.dataDir,
		Drivers:  map[string]input.Factory{config.DriverSimulated: f.drv.factory},
		Resolver: secrets.NewResolver(secrets.WithLookupEnv(f.env.lookup)),
		Clock:    f.clk,
		Logger:   slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, o := range opts {
		o(&cfg)
	}

	sup, err := New(cfg)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	f.sup = sup

	// A poller reads the concurrent accessors for the whole test (-race).
	ctx, cancel := context.WithCancel(context.Background())
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for ctx.Err() == nil {
			_ = sup.Status()
			_ = sup.AppliedVersion()
			runtime.Gosched()
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-polled
		if err := sup.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
		if strings.Contains(f.logs.String(), testToken) {
			t.Errorf("the logs contain the token")
		}
		if strings.Contains(f.logs.String(), "918273") {
			t.Errorf("the logs contain a sample value")
		}
	})

	return f
}

type inSpec struct {
	id      string
	variant int
	devices []string
}

type outSpec struct {
	id      string
	variant int
	route   []string
}

// document builds and parses a configuration document. Every device has
// the points state (string) and count (uint32, pcs); the variant changes a
// component without changing what it reads or where it sends.
func (f *fixture) document(version int64, inputs []inSpec, outputs []outSpec) config.Document {
	f.t.Helper()

	tree := map[string]any{"schema": config.SchemaName, "inputs": []any{}, "outputs": []any{}}
	if version > 0 {
		tree["version"] = version
	}

	var ins []any
	for _, in := range inputs {
		var devices []any
		for _, d := range in.devices {
			devices = append(devices, map[string]any{"device": d, "points": []any{
				map[string]any{"signal": "state", "address": "state", "read": "subscribe", "type": "string"},
				map[string]any{"signal": "count", "address": "count", "read": "subscribe", "type": "uint32", "unit": "pcs"},
			}})
		}
		ins = append(ins, map[string]any{
			"id": in.id, "driver": config.DriverSimulated,
			"connection": map[string]any{"variant": in.variant},
			"devices":    devices,
		})
	}
	if ins != nil {
		tree["inputs"] = ins
	}

	var outs []any
	for _, o := range outputs {
		route := o.route
		if route == nil {
			route = []string{"*"}
		}
		outs = append(outs, map[string]any{
			"id": o.id, "type": config.OutputLaraplateHTTP,
			"url":   f.be.url(o.id),
			"token": "secret://env/" + tokenEnv,
			"route": map[string]any{"include": route},
			"batch": map[string]any{"max_samples": 1, "max_wait": fmt.Sprintf("%ds", o.variant+1)},
		})
	}
	if outs != nil {
		tree["outputs"] = outs
	}

	return f.parse(tree)
}

func (f *fixture) parse(tree map[string]any) config.Document {
	f.t.Helper()

	data, err := json.Marshal(tree)
	if err != nil {
		f.t.Fatal(err)
	}
	doc, msgs, err := config.Parse(data)
	if err != nil || len(msgs) > 0 {
		f.t.Fatalf("Parse() = %v, %q", err, msgs)
	}

	return doc
}

func (f *fixture) apply(doc config.Document) Outcome {
	f.t.Helper()

	out, err := f.sup.Apply(f.t.Context(), doc)
	if err != nil {
		f.t.Fatalf("Apply() error = %v", err)
	}

	return out
}

func (f *fixture) mustApply(doc config.Document) {
	f.t.Helper()

	if out := f.apply(doc); out.Result != ResultApplied || len(out.Errors) > 0 {
		f.t.Fatalf("Apply() = %+v, want applied", out)
	}
}

func (f *fixture) output(id string) *runningOutput {
	f.sup.mu.Lock()
	defer f.sup.mu.Unlock()

	return f.sup.outputs[id]
}

// waitStatus waits until cond holds on the status.
func (f *fixture) waitStatus(what string, cond func(Status) bool) {
	f.t.Helper()

	deadline := time.Now().Add(guard)
	for !cond(f.sup.Status()) {
		if time.Now().After(deadline) {
			f.t.Fatalf("status: %s not reached: %+v", what, f.sup.Status())
		}
		runtime.Gosched()
	}
}

func outputState(id, state string) func(Status) bool {
	return func(s Status) bool {
		for _, o := range s.Outputs {
			if o.ID == id {
				return o.State == state
			}
		}

		return false
	}
}

const (
	birthA1    = "birth a1 [state count]"
	birthB1    = "birth b1 [state count]"
	birthPress = "birth press-07 [state count]"
	birthOven  = "birth oven-02 [state count]"
)

func TestApplyStartsTheDocument(t *testing.T) {
	f := newFixture(t)
	doc := f.document(1, []inSpec{{id: "line", devices: []string{"press-07", "oven-02"}}}, []outSpec{{id: "main"}})

	f.mustApply(doc)

	if got := f.be.waitItem(t, "main", birthOven); !reflect.DeepEqual(got, []string{birthPress, birthOven}) {
		t.Fatalf("main received %q, want the births of both devices", got)
	}
	f.drv.instance(t, "line").send("press-07", "state")
	f.be.waitItem(t, "main", "press-07/state")

	st := f.sup.Status()
	if !st.Applied || st.Version != 1 {
		t.Fatalf("Status() applied=%v version=%d, want true 1", st.Applied, st.Version)
	}
	wantInputs := []InputStatus{{ID: "line", Driver: config.DriverSimulated, State: InputRunning}}
	if !reflect.DeepEqual(st.Inputs, wantInputs) {
		t.Fatalf("Status().Inputs = %+v, want %+v", st.Inputs, wantInputs)
	}
	if len(st.Outputs) != 1 || st.Outputs[0].ID != "main" || st.Outputs[0].Type != config.OutputLaraplateHTTP {
		t.Fatalf("Status().Outputs = %+v, want main", st.Outputs)
	}
	f.waitStatus("main sending", outputState("main", "sending"))

	saved, ok, err := config.NewStore(f.dataDir).LastValid()
	if err != nil || !ok || saved.Version != 1 {
		t.Fatalf("LastValid() = version %d, %v, %v; want the applied document", saved.Version, ok, err)
	}
}

func TestApplyByDifference(t *testing.T) {
	f := newFixture(t)
	f.be.refuse("gone", 503)

	doc1 := f.document(1,
		[]inSpec{{id: "a", devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}},
		[]outSpec{{id: "keep"}, {id: "change"}, {id: "gone"}})
	f.mustApply(doc1)
	for _, out := range []string{"keep", "change"} {
		f.be.waitItem(t, out, birthB1)
		f.be.waitItem(t, out, birthA1)
	}
	f.be.waitUntil(t, "gone refused once", func() bool { return f.be.requests("gone") == 1 })

	keepBuf, changeBuf := f.output("keep").buf, f.output("change").buf
	before := map[string]int{"keep": len(f.be.got("keep")), "change": len(f.be.got("change"))}

	doc2 := f.document(2,
		[]inSpec{{id: "a", variant: 1, devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}},
		[]outSpec{{id: "keep"}, {id: "change", variant: 1}, {id: "new"}})
	f.mustApply(doc2)

	if n := f.drv.startCount("b"); n != 1 {
		t.Errorf("unchanged input b started %d times, want 1", n)
	}
	if n := f.drv.startCount("a"); n != 2 {
		t.Errorf("changed input a started %d times, want 2", n)
	}
	if f.output("keep").buf != keepBuf {
		t.Error("the unchanged output keep has a new buffer")
	}
	if f.output("change").buf == changeBuf {
		t.Error("the changed output change kept its buffer")
	}
	if f.output("gone") != nil {
		t.Error("the removed output gone is still running")
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "outputs", "gone")); err != nil {
		t.Errorf("the directory of the removed output: %v, want it kept", err)
	}
	if !strings.Contains(f.logs.String(), `"output":"gone"`) || !strings.Contains(f.logs.String(), "unsent") {
		t.Errorf("no warning about the unsent data of gone in %s", f.logs.String())
	}

	f.drv.instance(t, "b").send("b1", "state")

	want := map[string][]string{
		// Only the changed input's device is born again on an unchanged output.
		"keep": {birthA1, "b1/state"},
		// A restarted or new output gets the births of the running inputs,
		// then those of the restarted input.
		"change": {birthB1, birthA1, "b1/state"},
		"new":    {birthB1, birthA1, "b1/state"},
	}
	for out, items := range want {
		got := f.be.waitItem(t, out, "b1/state")[before[out]:]
		if !reflect.DeepEqual(got, items) {
			t.Errorf("%s received %q after the change, want %q", out, got, items)
		}
	}

	for _, out := range []string{"keep", "change"} {
		seqs := f.be.seqsOf(out)
		for i := 1; i < len(seqs); i++ {
			if seqs[i] != seqs[i-1]+1 {
				t.Errorf("%s source_seq %v, want consecutive across the change", out, seqs)

				break
			}
		}
	}
}

func TestApplyRollsBackAFailingInput(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*fakeDriver)
		apply   func(*fixture, config.Document) Outcome
		wantErr string
	}{
		{
			name:    "start error",
			prepare: func(d *fakeDriver) { d.failStart["b"] = errRefused },
			apply:   func(f *fixture, doc config.Document) Outcome { return f.apply(doc) },
			wantErr: "inputs.1: " + errRefused.Error(),
		},
		{
			name:    "start timeout",
			prepare: func(d *fakeDriver) { d.hang["b"] = true },
			apply: func(f *fixture, doc config.Document) Outcome {
				done := make(chan Outcome, 1)
				go func() { done <- f.apply(doc) }()
				f.drv.waitCalled(f.t, "b")
				f.clk.Advance(DefaultStartTimeout)
				select {
				case out := <-done:
					return out
				case <-time.After(guard):
					f.t.Fatal("Apply did not return after the start timeout")

					return Outcome{}
				}
			},
			wantErr: "inputs.1: did not start within 30s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			doc1 := f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}})
			f.mustApply(doc1)
			f.be.waitItem(t, "main", birthA1)

			tt.prepare(f.drv)
			doc2 := f.document(2,
				[]inSpec{{id: "a", variant: 1, devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}},
				[]outSpec{{id: "main"}, {id: "extra"}})
			out := tt.apply(f, doc2)

			if out.Result != ResultRolledBack || !slices.Contains(out.Errors, tt.wantErr) {
				t.Fatalf("Apply() = %+v, want rolled_back with %q", out, tt.wantErr)
			}
			if got := f.drv.instance(t, "a").cfg.Raw; string(got) != string(doc1.Inputs[0].Raw) {
				t.Errorf("input a runs %s, want the previous configuration %s", got, doc1.Inputs[0].Raw)
			}
			if n := f.drv.startCount("a"); n != 3 {
				t.Errorf("input a started %d times, want 3 (first, changed, rolled back)", n)
			}
			if f.output("extra") != nil {
				t.Error("the output added by the failed document is still running")
			}

			st := f.sup.Status()
			wantInputs := []InputStatus{{ID: "a", Driver: config.DriverSimulated, State: InputRunning}}
			if st.Version != 1 || !reflect.DeepEqual(st.Inputs, wantInputs) {
				t.Errorf("Status() version %d inputs %+v, want version 1 with a running", st.Version, st.Inputs)
			}
			saved, _, _ := config.NewStore(f.dataDir).LastValid()
			if saved.Version != 1 {
				t.Errorf("LastValid().Version = %d, want 1", saved.Version)
			}

			// The rolled-back input is running and routed again.
			f.drv.instance(t, "a").send("a1", "count")
			f.be.waitItem(t, "main", "a1/count")
		})
	}
}

func TestApplyRefuses(t *testing.T) {
	tests := []struct {
		name    string
		opts    []fixtureOption
		first   func(*fixture) config.Document
		second  func(*fixture) config.Document
		prepare func(*fakeDriver)
		want    []string
	}{
		{
			name: "an older version",
			first: func(f *fixture) config.Document {
				return f.document(5, []inSpec{{id: "a", devices: []string{"a1"}}}, nil)
			},
			second: func(f *fixture) config.Document {
				return f.document(4, []inSpec{{id: "a", variant: 1, devices: []string{"a1"}}}, nil)
			},
			want: []string{"version: 4 is older than the applied version 5"},
		},
		{
			name: "a document without version after a versioned one",
			first: func(f *fixture) config.Document {
				return f.document(5, []inSpec{{id: "a", devices: []string{"a1"}}}, nil)
			},
			second: func(f *fixture) config.Document {
				return f.document(0, []inSpec{{id: "a", variant: 1, devices: []string{"a1"}}}, nil)
			},
			want: []string{"version: 0 is older than the applied version 5"},
		},
		{
			name: "what the build does not provide",
			first: func(f *fixture) config.Document {
				return f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, nil)
			},
			second: func(f *fixture) config.Document {
				return f.parse(map[string]any{
					"schema": config.SchemaName, "version": 2, "outputs": []any{},
					"inputs": []any{map[string]any{
						"id": "a", "driver": config.DriverSimulated,
						"devices": []any{map[string]any{"device": "a1", "points": []any{map[string]any{
							"signal": "temp", "address": "t", "read": "subscribe",
							"transforms": []any{map[string]any{"scale": 0.1}},
						}}}},
					}},
				})
			},
			want: []string{"inputs.0.devices.0.points.0.transforms: not available in this build"},
		},
		{
			name: "an input the driver cannot build",
			first: func(f *fixture) config.Document {
				return f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, nil)
			},
			second: func(f *fixture) config.Document {
				return f.document(2, []inSpec{{id: "a", devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}}, nil)
			},
			prepare: func(d *fakeDriver) { d.failBuild["b"] = errRefused },
			want:    []string{"inputs.1: " + errRefused.Error()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.opts...)
			first := tt.first(f)
			f.mustApply(first)
			if tt.prepare != nil {
				tt.prepare(f.drv)
			}

			out := f.apply(tt.second(f))

			if out.Result != ResultRejected || !reflect.DeepEqual(out.Errors, tt.want) {
				t.Fatalf("Apply() = %+v, want rejected with %q", out, tt.want)
			}
			if n := f.drv.startCount("a"); n != 1 {
				t.Errorf("input a started %d times, want the running configuration untouched", n)
			}
			if got := f.drv.instance(t, "a").cfg.Raw; string(got) != string(first.Inputs[0].Raw) {
				t.Errorf("input a runs %s, want %s", got, first.Inputs[0].Raw)
			}
			if v := f.sup.AppliedVersion(); v != first.Version {
				t.Errorf("AppliedVersion() = %d, want %d", v, first.Version)
			}
		})
	}
}

func TestApplyRefusesADriverMissingFromTheRegistry(t *testing.T) {
	f := newFixture(t, withoutDrivers)

	out := f.apply(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, nil))

	want := []string{"inputs.0.driver: not available in this build"}
	if out.Result != ResultRejected || !reflect.DeepEqual(out.Errors, want) {
		t.Fatalf("Apply() = %+v, want rejected with %q", out, want)
	}
	if st := f.sup.Status(); st.Applied {
		t.Fatalf("Status().Applied = true after a rejected first document")
	}
}

func TestApplyEqualVersionApplies(t *testing.T) {
	f := newFixture(t)
	f.mustApply(f.document(3, []inSpec{{id: "a", devices: []string{"a1"}}}, nil))

	f.mustApply(f.document(3, []inSpec{{id: "a", variant: 1, devices: []string{"a1"}}}, nil))

	if n := f.drv.startCount("a"); n != 2 {
		t.Fatalf("input a started %d times, want 2", n)
	}
}

func TestOutputBufferAndBatcherReadyBeforeTheRouterAppends(t *testing.T) {
	f := newFixture(t)
	var steps []string
	f.sup.trace = func(step string) { steps = append(steps, step) }

	f.mustApply(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}))

	want := []string{"buffer main", "batcher main", "route main", "input a"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("start steps = %q, want %q", steps, want)
	}
}

func TestBirthsAgainOnOutputReconnection(t *testing.T) {
	f := newFixture(t)
	f.be.refuse("main", 503)

	f.mustApply(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}))
	f.waitStatus("main backing off", outputState("main", "backoff"))

	// Move the clock until the backoff ends and the batch is resent.
	deadline := time.Now().Add(guard)
	for len(f.be.got("main")) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("main received %q, want the birth resent and the reconnection birth", f.be.got("main"))
		}
		f.clk.Advance(time.Minute)
		runtime.Gosched()
	}

	if got := f.be.got("main"); !reflect.DeepEqual(got, []string{birthA1, birthA1}) {
		t.Fatalf("main received %q, want the birth then the reconnection birth", got)
	}
}

func TestReloadCredentialsResumesASuspendedOutput(t *testing.T) {
	f := newFixture(t)
	f.env.set(tokenEnv, "")

	f.mustApply(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}))
	f.waitStatus("main suspended", outputState("main", "suspended_auth"))
	if n := f.be.requests("main"); n != 0 {
		t.Fatalf("the backend received %d requests while the token was unset", n)
	}

	f.env.set(tokenEnv, testToken)
	f.sup.ReloadCredentials()

	f.be.waitItem(t, "main", birthA1)
	if got := f.be.authOf("main"); got[0] != "Bearer "+testToken {
		t.Fatalf("Authorization = %q, want the token", got[0])
	}
}

func TestCloseStopsEverything(t *testing.T) {
	f := newFixture(t)
	f.mustApply(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}))

	if err := f.sup.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	if got := f.drv.closedIDs(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("closed inputs = %q, want a", got)
	}
	if st := f.sup.Status(); len(st.Inputs) != 0 || len(st.Outputs) != 0 {
		t.Fatalf("Status() after Close = %+v, want nothing running", st)
	}
	if _, err := f.sup.Apply(t.Context(), f.document(2, nil, nil)); err == nil {
		t.Fatal("Apply() after Close succeeded")
	}
}

func TestRollbackReportsAPreviousInputThatDoesNotStartAgain(t *testing.T) {
	f := newFixture(t)
	doc1 := f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}})
	f.mustApply(doc1)

	f.drv.failNth["a"] = 3
	f.drv.failStart["b"] = errRefused
	doc2 := f.document(2,
		[]inSpec{{id: "a", variant: 1, devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}},
		[]outSpec{{id: "main"}})
	out := f.apply(doc2)

	want := []string{"inputs.1: " + errRefused.Error(), "rollback: input a: " + errRefused.Error()}
	if out.Result != ResultRolledBack || !reflect.DeepEqual(out.Errors, want) {
		t.Fatalf("Apply() = %+v, want rolled_back with %q", out, want)
	}
	wantInputs := []InputStatus{{ID: "a", Driver: config.DriverSimulated, State: StateFailed, LastError: errRefused.Error()}}
	if st := f.sup.Status(); st.Version != 1 || !reflect.DeepEqual(st.Inputs, wantInputs) {
		t.Fatalf("Status() version %d inputs %+v, want version 1 with a failed", st.Version, st.Inputs)
	}

	// The next document starts the failed input again.
	f.mustApply(doc1)
	wantInputs = []InputStatus{{ID: "a", Driver: config.DriverSimulated, State: InputRunning}}
	if st := f.sup.Status(); !reflect.DeepEqual(st.Inputs, wantInputs) {
		t.Fatalf("Status().Inputs = %+v, want a running", st.Inputs)
	}
}

func TestChangedOutputKeepsRecordsEmittedWhileItRestarts(t *testing.T) {
	f := newFixture(t)
	doc1 := f.document(1, []inSpec{{id: "b", devices: []string{"b1"}}}, []outSpec{{id: "change"}})
	f.mustApply(doc1)
	f.be.waitItem(t, "change", birthB1)

	// The unchanged input emits between the stop of the previous output and
	// the start of the new one.
	f.sup.trace = func(step string) {
		if step == "buffer change" {
			f.drv.instance(t, "b").send("b1", "count")
		}
	}
	f.mustApply(f.document(2, []inSpec{{id: "b", devices: []string{"b1"}}}, []outSpec{{id: "change", variant: 1}}))

	got := f.be.waitItem(t, "change", "b1/count")
	if want := []string{birthB1, birthB1, "b1/count"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("change received %q, want the restart birth then the held sample %q", got, want)
	}
}

func TestRollbackOfAChangedOutputKeepsItsBufferAndSeq(t *testing.T) {
	f := newFixture(t)
	doc1 := f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}})
	f.mustApply(doc1)
	f.be.waitItem(t, "main", birthA1)

	// A record waits in the buffer while the backend refuses.
	f.be.refuse("main", 503, 503, 503)
	f.drv.instance(t, "a").send("a1", "state")
	f.waitStatus("main backing off", outputState("main", "backoff"))

	f.drv.failStart["b"] = errRefused
	out := f.apply(f.document(2,
		[]inSpec{{id: "a", devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}},
		[]outSpec{{id: "main", variant: 1}}))
	if out.Result != ResultRolledBack {
		t.Fatalf("Apply() = %+v, want rolled_back", out)
	}
	if got := f.output("main").cfg.Raw; string(got) != string(doc1.Outputs[0].Raw) {
		t.Fatalf("main runs %s, want the previous configuration", got)
	}

	deadline := time.Now().Add(guard)
	for !slices.Contains(f.be.got("main"), "a1/state") {
		if time.Now().After(deadline) {
			t.Fatalf("main received %q, want the buffered sample after the rollback", f.be.got("main"))
		}
		f.clk.Advance(time.Minute)
		runtime.Gosched()
	}
	seqs := f.be.seqsOf("main")
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("main source_seq %v, want consecutive across the rollback", seqs)
		}
	}
}

func TestBirthsOnlyForLiveInputs(t *testing.T) {
	f := newFixture(t)
	f.mustApply(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}))
	f.be.waitItem(t, "main", birthA1)

	f.drv.hang["b"] = true
	done := make(chan Outcome, 1)
	go func() {
		done <- f.apply(f.document(2, []inSpec{{id: "a", devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}}, []outSpec{{id: "main"}}))
	}()
	f.drv.waitCalled(t, "b")

	// An output reconnecting while b is still starting.
	f.sup.router.BirthTo("main")

	f.clk.Advance(DefaultStartTimeout)
	select {
	case out := <-done:
		if out.Result != ResultRolledBack {
			t.Fatalf("Apply() = %+v, want rolled_back", out)
		}
	case <-time.After(guard):
		t.Fatal("Apply did not return")
	}

	f.drv.instance(t, "a").send("a1", "count")
	if got := f.be.waitItem(t, "main", "a1/count"); slices.Contains(got, birthB1) {
		t.Fatalf("main received %q, with a birth for an input that never ran", got)
	}
}

func TestApplyCancelledIsNotAnOutcome(t *testing.T) {
	f := newFixture(t)
	doc1 := f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, nil)
	f.mustApply(doc1)

	f.drv.hang["b"] = true
	ctx, cancel := context.WithCancel(t.Context())
	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := f.sup.Apply(ctx, f.document(2, []inSpec{{id: "a", variant: 1, devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}}, nil))
		done <- result{out, err}
	}()
	f.drv.waitCalled(t, "b")
	cancel()

	var res result
	select {
	case res = <-done:
	case <-time.After(guard):
		t.Fatal("Apply did not return after cancel")
	}
	if res.out.Result != "" || !errors.Is(res.err, context.Canceled) {
		t.Fatalf("Apply() = %+v, %v; want no outcome and context.Canceled", res.out, res.err)
	}
	if got := f.drv.instance(t, "a").cfg.Raw; string(got) != string(doc1.Inputs[0].Raw) {
		t.Fatalf("input a runs %s, want the previous configuration", got)
	}
	if v := f.sup.AppliedVersion(); v != 1 {
		t.Fatalf("AppliedVersion() = %d, want 1", v)
	}
}

// TestFailedOutputHoldsItsRecords: an output whose rollback restart fails
// keeps the records routed to it in a hold until a later document starts it
// (flushed) or removes it (discarded with a warning) (ruling R48).
func TestFailedOutputHoldsItsRecords(t *testing.T) {
	tests := []struct {
		name  string
		later func(f *fixture) config.Document
		check func(t *testing.T, f *fixture)
	}{
		{
			name: "a later document starts it",
			later: func(f *fixture) config.Document {
				return f.document(3, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main", variant: 2}})
			},
			check: func(t *testing.T, f *fixture) {
				got := f.be.waitItem(t, "main", "a1/state")
				if want := []string{birthA1, birthA1, "a1/state"}; !reflect.DeepEqual(got, want) {
					t.Fatalf("main received %q, want the restart birth then the held sample %q", got, want)
				}
			},
		},
		{
			name: "a later document removes it",
			later: func(f *fixture) config.Document {
				return f.document(3, []inSpec{{id: "a", devices: []string{"a1"}}}, nil)
			},
			check: func(t *testing.T, f *fixture) {
				if !strings.Contains(f.logs.String(), "held records of an output discarded") || !strings.Contains(f.logs.String(), `"records":1`) {
					t.Fatalf("no warning about the discarded held record in %s", f.logs.String())
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, restore := failMain(t)
			f.drv.instance(t, "a").send("a1", "state")

			restore()
			f.mustApply(tt.later(f))
			tt.check(t, f)
		})
	}
}

// failMain applies a document with input a and output main, then a change
// of main that fails, and whose rollback fails too, through a corrupt
// inflight file: main is left failed and held. restore repairs the file.
func failMain(t *testing.T) (f *fixture, restore func()) {
	t.Helper()

	f = newFixture(t)
	f.mustApply(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}))
	f.be.waitItem(t, "main", birthA1)
	f.waitStatus("main confirmed", func(s Status) bool {
		return len(s.Outputs) == 1 && !s.Outputs[0].LastConfirmation.IsZero()
	})

	inflight := filepath.Join(f.dataDir, "outputs", "main", "inflight")
	saved, err := os.ReadFile(inflight)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inflight, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := f.apply(f.document(2, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main", variant: 1}}))
	if out.Result != ResultRolledBack || len(out.Errors) != 2 || !strings.HasPrefix(out.Errors[1], "rollback: output main: ") {
		t.Fatalf("Apply() = %+v, want rolled_back with the failed restart of main", out)
	}
	st := f.sup.Status()
	if len(st.Outputs) != 1 || st.Outputs[0].State != StateFailed {
		t.Fatalf("Status().Outputs = %+v, want main failed", st.Outputs)
	}

	return f, func() {
		if err := os.WriteFile(inflight, saved, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFailedOutputStartedByARolledBackDocumentStaysHeld: a document that
// starts the failed output but fails on another component leaves the output
// failed with its previous error and its records held, routed by the
// configuration being restored.
func TestFailedOutputStartedByARolledBackDocumentStaysHeld(t *testing.T) {
	f, restore := failMain(t)
	failedErr := f.sup.Status().Outputs[0].LastError
	f.drv.instance(t, "a").send("a1", "state")
	restore()

	f.drv.failStart["b"] = errRefused
	out := f.apply(f.document(3,
		[]inSpec{{id: "a", devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}},
		[]outSpec{{id: "main", variant: 2}}))
	if out.Result != ResultRolledBack {
		t.Fatalf("Apply() = %+v, want rolled_back", out)
	}
	st := f.sup.Status()
	if len(st.Outputs) != 1 || st.Outputs[0].State != StateFailed || st.Outputs[0].LastError != failedErr {
		t.Fatalf("Status().Outputs = %+v, want main failed with %q", st.Outputs, failedErr)
	}

	f.drv.instance(t, "a").send("a1", "count")
	f.mustApply(f.document(4, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main", variant: 2}}))

	f.be.waitItem(t, "main", "a1/count")
	if got := f.be.got("main"); !slices.Contains(got, "a1/state") {
		t.Fatalf("main received %q, want the sample held before the rolled back document", got)
	}
}

func TestCloseStopsInputsBeforeDiscardingFailedOutputs(t *testing.T) {
	f, _ := failMain(t)
	f.drv.instance(t, "a").send("a1", "state")
	var steps []string
	f.sup.trace = func(step string) { steps = append(steps, step) }

	if err := f.sup.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	if want := []string{"stop input a", "discard output main"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("Close steps = %q, want %q", steps, want)
	}
	for _, want := range []string{"held records of an output discarded", `"reason":"agent shutting down"`, `"priority":"critical","records":1`, `"from_ts"`, `"to_ts"`} {
		if !strings.Contains(f.logs.String(), want) {
			t.Errorf("no %s in the shutdown warning: %s", want, f.logs.String())
		}
	}
}

// TestInputEmittingAfterAFailedStartRegistersNothing: an input that emits
// after its start failed neither registers its devices nor sends a record.
func TestInputEmittingAfterAFailedStartRegistersNothing(t *testing.T) {
	f := newFixture(t)
	f.mustApply(f.document(1, []inSpec{{id: "a", devices: []string{"a1"}}}, []outSpec{{id: "main"}}))
	f.be.waitItem(t, "main", birthA1)

	release := make(chan struct{})
	f.drv.lateEmit["b"] = release
	f.drv.failStart["b"] = errRefused
	out := f.apply(f.document(2, []inSpec{{id: "a", devices: []string{"a1"}}, {id: "b", devices: []string{"b1"}}}, []outSpec{{id: "main"}}))
	if out.Result != ResultRolledBack {
		t.Fatalf("Apply() = %+v, want rolled_back", out)
	}

	close(release)
	select {
	case <-f.drv.lateDone:
	case <-time.After(guard):
		t.Fatal("the late emit did not run")
	}
	f.sup.router.BirthTo("main")
	f.drv.instance(t, "a").send("a1", "count")

	got := f.be.waitItem(t, "main", "a1/count")
	if slices.Contains(got, birthB1) || slices.Contains(got, "b1/state") {
		t.Fatalf("main received %q, with records of an input whose start failed", got)
	}
}
