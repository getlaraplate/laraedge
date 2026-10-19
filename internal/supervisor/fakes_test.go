package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/config"
	"github.com/swolley/laraplate-edge/internal/input"
	"github.com/swolley/laraplate-edge/internal/model"
)

// guard bounds every wait for a goroutine; it is a deadlock guard, never a
// timing assertion.
const guard = 10 * time.Second

const (
	testToken = "tok-77c1-SECRET-never-logged"
	tokenEnv  = "EDGE_TOKEN"
)

// fakeDriver builds fake inputs and lets the test drive them.
type fakeDriver struct {
	mu        sync.Mutex
	failStart map[string]error
	hang      map[string]bool
	failBuild map[string]error
	failNth   map[string]int // fail the nth Start of an input
	// lateEmit makes Start leave a goroutine that emits b1/state once the
	// channel is closed, then closes lateDone.
	lateEmit map[string]chan struct{}
	lateDone chan struct{}
	// blockNth makes the nth Start of an input wait, ignoring its context,
	// until blockRelease is closed; blockStarted is closed when it begins
	// and blockClosed when that instance is closed.
	blockNth     map[string]int
	blockStarted chan struct{}
	blockRelease chan struct{}
	blockClosed  chan struct{}
	// closeEmit makes Close of a started input emit a sample of its first
	// device, as a goroutine of the input emitting while it stops.
	closeEmit map[string]bool
	running   map[string]*fakeInput
	starts    []string // "id raw" of every Start called
	closed    []string // ids of every Close of a started input
	called    chan string
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{
		failStart:    map[string]error{},
		hang:         map[string]bool{},
		failBuild:    map[string]error{},
		failNth:      map[string]int{},
		lateEmit:     map[string]chan struct{}{},
		lateDone:     make(chan struct{}),
		blockNth:     map[string]int{},
		blockStarted: make(chan struct{}),
		blockRelease: make(chan struct{}),
		blockClosed:  make(chan struct{}),
		closeEmit:    map[string]bool{},
		running:      map[string]*fakeInput{},
		called:       make(chan string, 64),
	}
}

func (d *fakeDriver) factory(cfg config.Input, _ clock.Clock) (input.Input, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.failBuild[cfg.ID]; err != nil {
		return nil, err
	}

	return &fakeInput{d: d, cfg: cfg}, nil
}

func (d *fakeDriver) startCount(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.countStarts(id)
}

func (d *fakeDriver) countStarts(id string) int {
	n := 0
	for _, s := range d.starts {
		if strings.HasPrefix(s, id+" ") {
			n++
		}
	}

	return n
}

// instance returns the running instance of input id.
func (d *fakeDriver) instance(t *testing.T, id string) *fakeInput {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	in := d.running[id]
	if in == nil {
		t.Fatalf("input %s is not running", id)
	}

	return in
}

func (d *fakeDriver) closedIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]string(nil), d.closed...)
}

func (d *fakeDriver) waitCalled(t *testing.T, id string) {
	t.Helper()

	for {
		select {
		case got := <-d.called:
			if got == id {
				return
			}
		case <-time.After(guard):
			t.Fatalf("Start of %s was not called", id)
		}
	}
}

type fakeInput struct {
	d       *fakeDriver
	cfg     config.Input
	emit    func(model.Record)
	started bool
	blocked bool
}

func (in *fakeInput) Start(ctx context.Context, emit func(model.Record)) error {
	d := in.d
	d.mu.Lock()
	d.starts = append(d.starts, in.cfg.ID+" "+string(in.cfg.Raw))
	hang, err := d.hang[in.cfg.ID], d.failStart[in.cfg.ID]
	if n := d.failNth[in.cfg.ID]; n > 0 && d.countStarts(in.cfg.ID) == n {
		err = errRefused
	}
	late := d.lateEmit[in.cfg.ID]
	block := d.blockNth[in.cfg.ID] > 0 && d.countStarts(in.cfg.ID) == d.blockNth[in.cfg.ID]
	in.blocked = block
	d.mu.Unlock()
	d.called <- in.cfg.ID

	if block {
		close(d.blockStarted)
		<-d.blockRelease

		return ctx.Err()
	}

	if late != nil {
		go func() {
			defer close(d.lateDone)
			<-late
			emit(model.SampleRecord(model.Sample{
				Device: "b1", Signal: "state", TS: time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC),
				Value: model.IntValue(918273), Quality: model.Good, Priority: model.Critical,
			}))
		}()
	}

	if hang {
		<-ctx.Done()

		return ctx.Err()
	}
	if err != nil {
		return err
	}

	d.mu.Lock()
	in.emit, in.started = emit, true
	d.running[in.cfg.ID] = in
	d.mu.Unlock()

	return nil
}

func (in *fakeInput) Close() error {
	d := in.d
	d.mu.Lock()
	emit := in.emit
	closeEmit := in.started && d.closeEmit[in.cfg.ID]
	d.mu.Unlock()
	if closeEmit {
		emit(model.SampleRecord(model.Sample{
			Device: in.cfg.Devices[0].Device, Signal: "state", TS: time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC),
			Value: model.IntValue(918273), Quality: model.Good, Priority: model.Critical,
		}))
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if in.blocked {
		in.blocked = false
		close(d.blockClosed)
	}
	if in.started {
		d.closed = append(d.closed, in.cfg.ID)
		in.started = false
	}
	if d.running[in.cfg.ID] == in {
		delete(d.running, in.cfg.ID)
	}

	return nil
}

func (in *fakeInput) Probe(context.Context) ([]model.Sample, error) { return nil, nil }

// send emits a sample of device/signal with a recognisable value.
func (in *fakeInput) send(device, signal string) {
	in.d.mu.Lock()
	emit := in.emit
	in.d.mu.Unlock()

	emit(model.SampleRecord(model.Sample{
		Device: device, Signal: signal, TS: time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC),
		Value: model.IntValue(918273), Quality: model.Good, Priority: model.Critical,
	}))
}

// sendDeath emits the death of device.
func (in *fakeInput) sendDeath(device string) {
	in.d.mu.Lock()
	emit := in.emit
	in.d.mu.Unlock()

	emit(model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: device, TS: time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)}))
}

// backend is a fake ingest route serving every output under /<output id>/.
type backend struct {
	srv *httptest.Server

	mu      sync.Mutex
	script  map[string][]int // per output, statuses answered first, then 202
	items   map[string][]string
	seqs    map[string][]uint64
	auth    map[string][]string
	changed chan struct{}
}

func newBackend(t *testing.T) *backend {
	t.Helper()

	be := &backend{
		script:  map[string][]int{},
		items:   map[string][]string{},
		seqs:    map[string][]uint64{},
		auth:    map[string][]string{},
		changed: make(chan struct{}, 1),
	}
	be.srv = httptest.NewUnstartedServer(http.HandlerFunc(be.serve))
	be.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	be.srv.Start()
	t.Cleanup(be.srv.Close)

	return be
}

// refuse makes output out answer statuses first.
func (be *backend) refuse(out string, statuses ...int) {
	be.mu.Lock()
	defer be.mu.Unlock()

	be.script[out] = statuses
}

func (be *backend) url(output string) string { return be.srv.URL + "/" + output }

func (be *backend) serve(w http.ResponseWriter, r *http.Request) {
	out, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	body, _ := io.ReadAll(r.Body)

	var env struct {
		SourceSeq uint64 `json:"source_seq"`
		Devices   []struct {
			Device  string `json:"device"`
			Type    string `json:"type"`
			Samples []struct {
				Signal string `json:"signal"`
			} `json:"samples"`
			Signals []struct {
				Signal string `json:"signal"`
			} `json:"signals"`
		} `json:"devices"`
	}
	_ = json.Unmarshal(body, &env)

	be.mu.Lock()
	status := http.StatusAccepted
	if s := be.script[out]; len(s) > 0 {
		status, be.script[out] = s[0], s[1:]
	}
	be.auth[out] = append(be.auth[out], r.Header.Get("Authorization"))
	if status == http.StatusAccepted {
		be.seqs[out] = append(be.seqs[out], env.SourceSeq)
		for _, d := range env.Devices {
			switch d.Type {
			case "birth":
				var names []string
				for _, s := range d.Signals {
					names = append(names, s.Signal)
				}
				be.items[out] = append(be.items[out], fmt.Sprintf("birth %s %v", d.Device, names))
			case "death":
				be.items[out] = append(be.items[out], "death "+d.Device)
			default:
				for _, s := range d.Samples {
					be.items[out] = append(be.items[out], d.Device+"/"+s.Signal)
				}
			}
		}
	}
	be.mu.Unlock()

	select {
	case be.changed <- struct{}{}:
	default:
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"status":"accepted"}`)
}

func (be *backend) got(out string) []string {
	be.mu.Lock()
	defer be.mu.Unlock()

	return append([]string(nil), be.items[out]...)
}

func (be *backend) seqsOf(out string) []uint64 {
	be.mu.Lock()
	defer be.mu.Unlock()

	return append([]uint64(nil), be.seqs[out]...)
}

func (be *backend) requests(out string) int {
	be.mu.Lock()
	defer be.mu.Unlock()

	return len(be.auth[out])
}

func (be *backend) authOf(out string) []string {
	be.mu.Lock()
	defer be.mu.Unlock()

	return append([]string(nil), be.auth[out]...)
}

// waitUntil waits until cond holds, re-checking after every request.
func (be *backend) waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.After(guard)
	for !cond() {
		select {
		case <-be.changed:
		case <-deadline:
			t.Fatalf("backend: %s not reached", what)
		}
	}
}

// waitItem waits until output out received item and returns everything it
// received up to and including it.
func (be *backend) waitItem(t *testing.T, out, item string) []string {
	t.Helper()

	var upTo []string
	be.waitUntil(t, out+" receiving "+item, func() bool {
		got := be.got(out)
		for i, it := range got {
			if it == item {
				upTo = got[:i+1]

				return true
			}
		}

		return false
	})

	return upTo
}

// envStore is a fake environment for secrets.WithLookupEnv.
type envStore struct {
	mu   sync.Mutex
	vars map[string]string
}

func (e *envStore) lookup(name string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	v, ok := e.vars[name]

	return v, ok
}

func (e *envStore) set(name, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.vars[name] = value
}

// lockedBuffer is a log sink safe for concurrent use.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

var errRefused = errors.New("connection refused by the fake machine")
