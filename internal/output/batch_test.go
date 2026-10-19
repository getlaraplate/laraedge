package output_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swolley/laraplate-edge/internal/buffer"
	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/envelope"
	"github.com/swolley/laraplate-edge/internal/model"
	"github.com/swolley/laraplate-edge/internal/output"
)

var epoch = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

// sample returns a sample of class p whose ts and value are i; i in 10..99
// keeps every frame of one signal the same size.
func sample(p model.Priority, signal string, i int) model.Record {
	return model.SampleRecord(model.Sample{
		Device:   "press-01",
		Signal:   signal,
		TS:       epoch.Add(time.Duration(i) * time.Second),
		Value:    model.IntValue(int64(i)),
		Quality:  model.Good,
		Priority: p,
	})
}

func crit(i int) model.Record { return sample(model.Critical, "state", i+10) }

func bulk(i int) model.Record { return sample(model.Bulk, "temp", i+10) }

func recordTS(r model.Record) time.Time {
	if r.Sample != nil {
		return r.Sample.TS
	}

	return r.Event.TS
}

// bufferFull returns a buffer_full marker of class p from ts i to ts j.
func bufferFull(p model.Priority, i, j, samples int) model.Loss {
	return model.Loss{
		Priority: p,
		FromTS:   epoch.Add(time.Duration(i) * time.Second),
		ToTS:     epoch.Add(time.Duration(j) * time.Second),
		Samples:  samples,
		Reason:   model.LossBufferFull,
	}
}

func markerOf(p model.Priority, from, to model.Record, samples int, reason string) model.Loss {
	return model.Loss{Priority: p, FromTS: recordTS(from), ToTS: recordTS(to), Samples: samples, Reason: reason}
}

func sameLosses(a, b []model.Loss) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		x, y := a[i], b[i]
		if x.Priority != y.Priority || !x.FromTS.Equal(y.FromTS) || !x.ToTS.Equal(y.ToTS) || x.Samples != y.Samples || x.Reason != y.Reason {
			return false
		}
	}

	return true
}

// fault is how faultyBuffer answers one loss method.
type fault int

const (
	faultNone       fault = iota
	faultNotDurable       // applied, reported as not yet durable
	faultFailing          // not applied, reported as an error
	faultLost             // not applied, reported as done: a crash before the call
)

func (f fault) apply(call func() error) error {
	switch f {
	case faultNotDurable:
		if err := call(); err != nil {
			return err
		}

		return fmt.Errorf("faulty disk: %w", buffer.ErrNotDurable)
	case faultFailing:
		return errors.New("faulty: refused")
	case faultLost:
		return nil
	default:
		return call()
	}
}

// faultyBuffer is the real buffer with scripted answers of the loss
// methods, counting the AckLosses calls.
type faultyBuffer struct {
	*buffer.Buffer
	addLoss        fault
	ackLosses      fault
	syncLosses     fault
	ackLossesCalls int
}

func (f *faultyBuffer) AddLoss(l model.Loss) error {
	return f.addLoss.apply(func() error { return f.Buffer.AddLoss(l) })
}

func (f *faultyBuffer) AckLosses(n int) error {
	f.ackLossesCalls++

	return f.ackLosses.apply(func() error { return f.Buffer.AckLosses(n) })
}

func (f *faultyBuffer) SyncLosses() error {
	return f.syncLosses.apply(f.Buffer.SyncLosses)
}

// harness is one output directory with its buffer, inflight and batcher;
// restart closes the buffer and opens everything again, as after a crash
// with the inflight file as last written.
type harness struct {
	t      *testing.T
	dir    string
	limits output.BatchLimits
	opts   buffer.Options
	clk    *clock.Fake // the batcher's clock; the buffer has its own
	wrap   func(*buffer.Buffer) output.Buffer

	buf *buffer.Buffer
	inf *output.Inflight
	bt  *output.Batcher
}

func newHarness(t *testing.T, limits output.BatchLimits) *harness {
	t.Helper()

	h := &harness{
		t:      t,
		dir:    filepath.Join(t.TempDir(), "outputs", "laraplate"),
		limits: limits,
		opts: buffer.Options{
			Clock:     clock.NewFake(epoch),
			DiskUsage: func(string) (uint64, uint64, error) { return 100, 100, nil },
			Logger:    slog.New(slog.DiscardHandler),
		},
		clk: clock.NewFake(epoch),
	}
	t.Cleanup(func() {
		if h.buf != nil {
			_ = h.buf.Close()
		}
	})

	return h
}

func (h *harness) open() *harness {
	h.t.Helper()

	buf, err := buffer.Open(h.dir, h.opts)
	if err != nil {
		h.t.Fatalf("buffer.Open() = %v", err)
	}

	h.buf = buf
	var src output.Buffer = buf
	if h.wrap != nil {
		src = h.wrap(buf)
	}

	h.inf = output.NewInflight(h.dir)
	h.bt, err = output.NewBatcher(src, h.inf, envelope.NewIDGenerator(h.clk), h.clk, h.limits, slog.New(slog.DiscardHandler))
	if err != nil {
		h.t.Fatalf("NewBatcher() = %v", err)
	}

	return h
}

func (h *harness) restart() {
	h.t.Helper()

	if err := h.buf.Close(); err != nil {
		h.t.Fatalf("buffer.Close() = %v", err)
	}

	h.buf = nil
	h.open()
}

func (h *harness) append(records ...model.Record) {
	h.t.Helper()

	for _, r := range records {
		if err := h.buf.Append(r); err != nil {
			h.t.Fatalf("Append() = %v", err)
		}
	}
}

// next calls Next where it must not wait: enough records or MaxWait zero.
func (h *harness) next() *output.Batch {
	h.t.Helper()

	b, err := h.bt.Next(context.Background())
	if err != nil {
		h.t.Fatalf("Next() = %v", err)
	}

	return b
}

func (h *harness) confirm(b *output.Batch) {
	h.t.Helper()

	if err := h.bt.Confirm(b); err != nil {
		h.t.Fatalf("Confirm() = %v", err)
	}
}

type nextResult struct {
	batch *output.Batch
	err   error
}

func (h *harness) nextAsync(ctx context.Context) <-chan nextResult {
	out := make(chan nextResult, 1)
	go func() {
		b, err := h.bt.Next(ctx)
		out <- nextResult{b, err}
	}()

	return out
}

// waitTimers yields until n timers wait on clk: the batcher is then blocked
// in its select.
func waitTimers(t *testing.T, clk *clock.Fake, n int) {
	t.Helper()

	for i := 0; clk.PendingTimers() != n; i++ {
		if i > 50_000_000 {
			t.Fatalf("PendingTimers() = %d, want %d", clk.PendingTimers(), n)
		}

		runtime.Gosched()
	}
}

// bodySamples returns "signal=value" for every sample of the envelope, in
// order, and "type:device" for every device event.
func bodySamples(t *testing.T, body []byte) []string {
	t.Helper()

	var env struct {
		Devices []struct {
			Device  string `json:"device"`
			Type    string `json:"type"`
			Samples []struct {
				Signal string          `json:"signal"`
				Value  json.RawMessage `json:"value"`
			} `json:"samples"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not an envelope: %v", err)
	}

	var out []string
	for _, d := range env.Devices {
		if d.Type != "data" {
			out = append(out, d.Type+":"+d.Device)

			continue
		}

		for _, s := range d.Samples {
			out = append(out, s.Signal+"="+string(s.Value))
		}
	}

	return out
}

// envelopeSize is the body size of records under a message id of the
// generator's length, at seq 0 and the batcher clock's time.
func envelopeSize(t *testing.T, h *harness, records ...model.Record) int {
	t.Helper()

	body, err := envelope.Build("00000000-0000-7000-8000-000000000000", 0, h.clk.Now(), records, nil)
	if err != nil {
		t.Fatal(err)
	}

	return len(body)
}

func TestNextLimitsAndPriorityOrder(t *testing.T) {
	tests := []struct {
		name        string
		limits      output.BatchLimits
		appended    []model.Record
		wantSamples []string
	}{
		{
			name:        "max_samples counts records",
			limits:      output.BatchLimits{MaxSamples: 3, MaxBody: 1 << 20, MaxWait: time.Hour},
			appended:    []model.Record{bulk(0), bulk(1), bulk(2), bulk(3), bulk(4)},
			wantSamples: []string{"temp=10", "temp=11", "temp=12"},
		},
		{
			name:        "critical first, then bulk",
			limits:      output.BatchLimits{MaxSamples: 3, MaxBody: 1 << 20, MaxWait: time.Hour},
			appended:    []model.Record{bulk(0), bulk(1), crit(0), crit(1)},
			wantSamples: []string{"state=10", "state=11", "temp=10"},
		},
		{
			name:   "device events count as records and travel critical",
			limits: output.BatchLimits{MaxSamples: 2, MaxBody: 1 << 20, MaxWait: time.Hour},
			appended: []model.Record{
				bulk(0),
				model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: "press-02", TS: epoch}),
			},
			wantSamples: []string{"death:press-02", "temp=10"},
		},
		{
			name:        "max_wait zero seals what is there",
			limits:      output.BatchLimits{MaxSamples: 10, MaxBody: 1 << 20},
			appended:    []model.Record{bulk(0), crit(0)},
			wantSamples: []string{"state=10", "temp=10"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.limits).open()
			h.append(tc.appended...)

			b := h.next()
			if got := bodySamples(t, b.Body); !reflect.DeepEqual(got, tc.wantSamples) {
				t.Fatalf("body samples %v, want %v", got, tc.wantSamples)
			}

			if b.Seq != 0 || b.Len() != len(tc.wantSamples) {
				t.Fatalf("batch seq %d with %d records, want seq 0 with %d", b.Seq, b.Len(), len(tc.wantSamples))
			}

			if n := h.clk.PendingTimers(); n != 0 {
				t.Fatalf("PendingTimers() after Next = %d, want 0", n)
			}
		})
	}
}

func TestNextWaitsMaxWaitForMoreRecords(t *testing.T) {
	const maxWait = 2 * time.Second
	h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 1 << 20, MaxWait: maxWait}).open()

	res := h.nextAsync(context.Background())
	h.append(bulk(0))
	waitTimers(t, h.clk, 1)

	h.clk.Advance(maxWait - time.Millisecond)
	select {
	case r := <-res:
		t.Fatalf("Next() returned before max_wait: %v, %v", r.batch, r.err)
	default:
	}

	h.append(bulk(1), crit(0))
	h.clk.Advance(time.Millisecond)

	r := <-res
	if r.err != nil {
		t.Fatalf("Next() = %v", r.err)
	}

	if got, want := bodySamples(t, r.batch.Body), []string{"state=10", "temp=10", "temp=11"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body samples %v, want %v", got, want)
	}
}

func TestNextSealsAtOnceWhenMaxSamplesArrive(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 3, MaxBody: 1 << 20, MaxWait: time.Hour}).open()

	res := h.nextAsync(context.Background())
	h.append(bulk(0))
	waitTimers(t, h.clk, 1)
	h.append(bulk(1), bulk(2))

	r := <-res
	if r.err != nil || r.batch.Len() != 3 {
		t.Fatalf("Next() = %v records, %v; want 3 records", r.batch, r.err)
	}

	if n := h.clk.PendingTimers(); n != 0 {
		t.Fatalf("PendingTimers() after Next = %d, want 0 (max_wait timer left behind)", n)
	}
}

func TestNextStopsOnCancel(t *testing.T) {
	tests := []struct {
		name     string
		appended []model.Record
		timers   int
	}{
		{name: "no record yet"},
		{name: "waiting for max_wait", appended: []model.Record{bulk(0)}, timers: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 1 << 20, MaxWait: time.Hour}).open()
			h.append(tc.appended...)

			ctx, cancel := context.WithCancel(context.Background())
			res := h.nextAsync(ctx)
			waitTimers(t, h.clk, tc.timers)
			cancel()

			if r := <-res; !errors.Is(r.err, context.Canceled) {
				t.Fatalf("Next() after cancel = %v, %v; want context.Canceled", r.batch, r.err)
			}

			if n := h.clk.PendingTimers(); n != 0 {
				t.Fatalf("PendingTimers() after cancel = %d, want 0", n)
			}
		})
	}
}

func TestNextFitsMaxBody(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 1 << 20}).open()
	records := []model.Record{bulk(0), bulk(1), bulk(2), bulk(3), bulk(4), bulk(5)}
	h.limits.MaxBody = envelopeSize(t, h, records[:4]...) + 5
	if envelopeSize(t, h, records[:5]...) <= h.limits.MaxBody {
		t.Fatal("test setup: five records fit max_body")
	}

	h.restart()
	h.append(records...)

	first := h.next()
	if first.Len() != 4 || len(first.Body) > h.limits.MaxBody {
		t.Fatalf("first batch: %d records, %d bytes; want 4 records within %d bytes", first.Len(), len(first.Body), h.limits.MaxBody)
	}

	h.confirm(first)
	second := h.next()
	if got, want := bodySamples(t, second.Body), []string{"temp=14", "temp=15"}; !reflect.DeepEqual(got, want) || second.Seq != 1 {
		t.Fatalf("second batch seq %d samples %v, want seq 1 samples %v", second.Seq, got, want)
	}
}

func TestNextDropsARecordLargerThanMaxBody(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 600}).open()
	big := model.SampleRecord(model.Sample{
		Device: "press-01", Signal: "note", TS: epoch.Add(time.Second),
		Value: model.StringValue(strings.Repeat("x", 1000)), Quality: model.Good, Priority: model.Bulk,
	})
	h.append(big, bulk(0))

	b := h.next()
	if got, want := bodySamples(t, b.Body), []string{"temp=10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body samples %v, want %v", got, want)
	}

	if want := []model.Loss{markerOf(model.Bulk, big, big, 1, model.LossRejectedInvalid)}; !sameLosses(b.Losses, want) {
		t.Fatalf("batch losses %v, want %v", b.Losses, want)
	}

	if len(b.Body) > 600 {
		t.Fatalf("body of %d bytes above max_body", len(b.Body))
	}
}

func TestSealedBatchIsResentIdenticalAfterRestart(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 2, MaxBody: 1 << 20}).open()
	carried := bufferFull(model.Bulk, 1, 2, 2)
	if err := h.buf.AddLoss(carried); err != nil {
		t.Fatal(err)
	}

	h.append(crit(0), bulk(0), bulk(1))

	sealed := h.next()
	if !sameLosses(sealed.Losses, []model.Loss{carried}) {
		t.Fatalf("sealed losses %v, want %v", sealed.Losses, []model.Loss{carried})
	}

	if again := h.next(); again != sealed {
		t.Fatal("a second Next before Confirm returned another batch")
	}

	h.restart()
	resent := h.next()
	assertSameBatch(t, resent, sealed)

	if pending := h.buf.PendingLosses(); len(pending) != 0 {
		t.Fatalf("pending losses after the seal: %v, want none (they travel with the batch)", pending)
	}

	h.confirm(resent)
	after := h.next()
	if after.Seq != 1 || len(after.Losses) != 0 {
		t.Fatalf("batch after confirm: seq %d losses %v, want seq 1 and no losses", after.Seq, after.Losses)
	}

	if got, want := bodySamples(t, after.Body), []string{"temp=11"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body samples %v, want %v", got, want)
	}
}

// TestSeqGrowsAcrossRestarts covers the Review Focus "crash between sealing
// a batch and writing inflight": the save fails, nothing is durable, and
// after a restart the same records and loss markers are sealed with the seq
// that was never sent.
func TestSeqGrowsAcrossRestarts(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 1 << 20}).open()

	var sent []uint64
	for i := range 3 {
		h.append(bulk(i))
		b := h.next()
		sent = append(sent, b.Seq)
		h.confirm(b)
		h.restart()
	}

	carried := bufferFull(model.Bulk, 1, 2, 2)
	if err := h.buf.AddLoss(carried); err != nil {
		t.Fatal(err)
	}

	h.append(bulk(3))
	blocker := filepath.Join(h.dir, "inflight.tmp")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}

	if b, err := h.bt.Next(context.Background()); err == nil {
		t.Fatalf("Next() with a failing inflight save = batch seq %d, want an error", b.Seq)
	}

	h.restart()
	resealed := h.next()
	if got, want := bodySamples(t, resealed.Body), []string{"temp=13"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resealed samples %v, want %v", got, want)
	}

	if !sameLosses(resealed.Losses, []model.Loss{carried}) {
		t.Fatalf("resealed losses %v, want %v: a failed save acknowledges no marker", resealed.Losses, []model.Loss{carried})
	}

	sent = append(sent, resealed.Seq)
	h.confirm(resealed)
	h.restart()
	h.append(bulk(4))
	sent = append(sent, h.next().Seq)

	if want := []uint64{0, 1, 2, 3, 4}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("seqs sent %v, want %v", sent, want)
	}
}

func TestShrinkKeepsSeqAndLossesInTheFirstPart(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 6, MaxBody: 1 << 20}).open()
	carried := bufferFull(model.Critical, 1, 1, 1)
	if err := h.buf.AddLoss(carried); err != nil {
		t.Fatal(err)
	}

	h.append(bulk(0), bulk(1), bulk(2), bulk(3), crit(0), crit(1))
	refused := h.next()

	first, err := h.bt.Shrink(refused, 3)
	if err != nil {
		t.Fatalf("Shrink() = %v", err)
	}

	if first.Seq != refused.Seq || first.MessageID == refused.MessageID {
		t.Fatalf("shrunk batch seq %d id %s; want seq %d and a new id (refused id %s)", first.Seq, first.MessageID, refused.Seq, refused.MessageID)
	}

	if got, want := bodySamples(t, first.Body), []string{"state=10", "state=11", "temp=10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shrunk samples %v, want %v", got, want)
	}

	if !sameLosses(first.Losses, []model.Loss{carried}) {
		t.Fatalf("shrunk losses %v, want %v", first.Losses, []model.Loss{carried})
	}

	if again := h.next(); again != first {
		t.Fatal("Next after Shrink returned another batch than the shrunk one")
	}

	h.restart()
	resent := h.next()
	assertSameBatch(t, resent, first)

	h.confirm(resent)
	rest := h.next()
	if rest.Seq != refused.Seq+1 || len(rest.Losses) != 0 {
		t.Fatalf("remainder seq %d losses %v, want seq %d and no losses", rest.Seq, rest.Losses, refused.Seq+1)
	}

	if got, want := bodySamples(t, rest.Body), []string{"temp=11", "temp=12", "temp=13"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remainder samples %v, want %v", got, want)
	}

	if pending := h.buf.PendingLosses(); len(pending) != 0 {
		t.Fatalf("pending losses %v, want none: nothing was lost", pending)
	}
}

func TestShrinkRefusesBadSizes(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 2, MaxBody: 1 << 20}).open()
	h.append(bulk(0), bulk(1))
	b := h.next()

	for _, n := range []int{0, 2, -1} {
		if _, err := h.bt.Shrink(b, n); err == nil {
			t.Errorf("Shrink(b, %d) = nil, want an error", n)
		}
	}
}

// bulkFrameSize returns the frame size of bulk(i), equal for i in 0..89.
func bulkFrameSize(t *testing.T) int64 {
	t.Helper()

	b, err := buffer.Open(t.TempDir(), buffer.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}

	defer b.Close()

	if err := b.Append(bulk(0)); err != nil {
		t.Fatal(err)
	}

	e, err := b.Read(model.Bulk, 0, 1)
	if err != nil || len(e) != 1 {
		t.Fatalf("Read() = %v, %v", e, err)
	}

	return int64(e[0].Next - e[0].Pos)
}

// TestEvictionSparesSealedRecordsAndShrinkCountsThem: a sealed batch carries
// its records, so evicting them is no loss, also after a restart; once a
// split hands part of them back to the buffer, the part already evicted is
// counted in a buffer_full marker.
func TestEvictionSparesSealedRecordsAndShrinkCountsThem(t *testing.T) {
	f := bulkFrameSize(t)
	h := newHarness(t, output.BatchLimits{MaxSamples: 3, MaxBody: 1 << 20})
	h.opts.SegmentSize = 3*f + f/2
	h.opts.MaxSize = 6*f + f/2
	h.open()

	h.append(bulk(0), bulk(1), bulk(2))
	sealed := h.next()

	h.restart()
	h.append(bulk(3), bulk(4), bulk(5), bulk(6))
	if head := h.buf.Head(model.Bulk); head < sealed.Positions[model.Bulk] {
		t.Fatalf("test setup: the sealed segment was not evicted (head %d)", head)
	}

	if pending := h.buf.PendingLosses(); len(pending) != 0 {
		t.Fatalf("pending losses after evicting sealed records: %v, want none", pending)
	}

	resent := h.next()
	if _, err := h.bt.Shrink(resent, 1); err != nil {
		t.Fatalf("Shrink() = %v", err)
	}

	want := []model.Loss{markerOf(model.Bulk, bulk(1), bulk(2), 2, model.LossBufferFull)}
	if pending := h.buf.PendingLosses(); !sameLosses(pending, want) {
		t.Fatalf("pending losses after Shrink: %v, want %v", pending, want)
	}
}

func TestDropReusesSeqAndLeavesMarkers(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 3, MaxBody: 1 << 20}).open()
	carried := bufferFull(model.Bulk, 1, 2, 2)
	if err := h.buf.AddLoss(carried); err != nil {
		t.Fatal(err)
	}

	h.append(bulk(0), bulk(1), crit(0))
	refused := h.next()

	if err := h.bt.Drop(refused, model.LossRejectedInvalid); err != nil {
		t.Fatalf("Drop() = %v", err)
	}

	want := []model.Loss{
		carried,
		markerOf(model.Critical, crit(0), crit(0), 1, model.LossRejectedInvalid),
		markerOf(model.Bulk, bulk(0), bulk(1), 2, model.LossRejectedInvalid),
	}

	if s := h.buf.Stats(); s.Records != 0 {
		t.Fatalf("buffer holds %d records after Drop, want 0", s.Records)
	}

	if got := h.inf.NextSeq(); got != refused.Seq {
		t.Fatalf("next seq after Drop = %d, want the refused %d", got, refused.Seq)
	}

	h.restart()
	h.append(bulk(5))
	next := h.next()
	if next.Seq != refused.Seq {
		t.Fatalf("batch after Drop has seq %d, want the refused %d", next.Seq, refused.Seq)
	}

	if !sameLosses(next.Losses, want) {
		t.Fatalf("batch after Drop carries %v, want %v", next.Losses, want)
	}

	if got, want := bodySamples(t, next.Body), []string{"temp=15"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body samples %v, want %v", got, want)
	}
}

func TestLossMethodsNotDurable(t *testing.T) {
	carried := bufferFull(model.Bulk, 1, 2, 2)
	rejected := markerOf(model.Bulk, bulk(0), bulk(0), 1, model.LossRejectedInvalid)
	tests := []struct {
		name       string
		addLoss    fault
		ackLosses  fault
		syncLosses fault
		drop       bool
		dropErr    bool
		pending    []model.Loss // after the seal and the optional Drop
	}{
		{
			name:      "AckLosses applied but not durable: never repeated",
			ackLosses: faultNotDurable,
		},
		{
			name:    "AddLoss not durable, SyncLosses writes it: Drop goes on, nothing doubled",
			addLoss: faultNotDurable,
			drop:    true,
			pending: []model.Loss{carried, rejected},
		},
		{
			name:       "AddLoss not durable and SyncLosses fails: Drop acknowledges nothing",
			addLoss:    faultNotDurable,
			syncLosses: faultFailing,
			drop:       true,
			dropErr:    true,
			pending:    []model.Loss{carried, rejected},
		},
		{
			name:    "AddLoss refused: Drop fails and keeps the batch",
			addLoss: faultFailing,
			drop:    true,
			dropErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, output.BatchLimits{MaxSamples: 1, MaxBody: 1 << 20})
			fb := &faultyBuffer{ackLosses: tc.ackLosses, syncLosses: tc.syncLosses}
			h.wrap = func(b *buffer.Buffer) output.Buffer { fb.Buffer = b; return fb }
			h.open()
			if err := h.buf.AddLoss(carried); err != nil {
				t.Fatal(err)
			}

			h.append(bulk(0))
			b := h.next()
			if !sameLosses(b.Losses, []model.Loss{carried}) {
				t.Fatalf("sealed losses %v, want %v", b.Losses, []model.Loss{carried})
			}

			if !tc.drop {
				h.confirm(b)
				h.append(bulk(1))
				if after := h.next(); len(after.Losses) != 0 {
					t.Fatalf("the next batch carries %v again", after.Losses)
				}

				if fb.ackLossesCalls != 1 {
					t.Fatalf("AckLosses called %d times, want once", fb.ackLossesCalls)
				}
			}

			if tc.drop {
				fb.addLoss = tc.addLoss
				err := h.bt.Drop(b, model.LossRejectedInvalid)
				if (err != nil) != tc.dropErr {
					t.Fatalf("Drop() = %v, want error %v", err, tc.dropErr)
				}
			}

			if pending := h.buf.PendingLosses(); !sameLosses(pending, tc.pending) {
				t.Fatalf("pending losses %v, want %v", pending, tc.pending)
			}

			if tc.dropErr {
				_, stored, next := loadInflight(t, h.dir)
				if stored == nil || stored.Seq != b.Seq || next != b.Seq+1 {
					t.Fatalf("inflight after a failed Drop: %v next %d, want the batch kept", stored, next)
				}

				if s := h.buf.Stats(); s.Records != 1 {
					t.Fatalf("buffer holds %d records after a failed Drop, want 1 (nothing acknowledged)", s.Records)
				}

				if again := h.next(); again != b {
					t.Fatal("Next after a failed Drop returned another batch")
				}
			}
		})
	}
}

// TestRejectWaitsForADurableMarker: a record larger than max_body is
// acknowledged only once its rejected_invalid marker is durable (ruling R41).
func TestRejectWaitsForADurableMarker(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 600})
	fb := &faultyBuffer{addLoss: faultNotDurable, syncLosses: faultFailing}
	h.wrap = func(b *buffer.Buffer) output.Buffer { fb.Buffer = b; return fb }
	h.open()
	big := model.SampleRecord(model.Sample{
		Device: "press-01", Signal: "note", TS: epoch.Add(time.Second),
		Value: model.StringValue(strings.Repeat("x", 1000)), Quality: model.Good, Priority: model.Bulk,
	})
	h.append(big, bulk(0))

	if b, err := h.bt.Next(context.Background()); err == nil {
		t.Fatalf("Next() with a marker that cannot be written = batch seq %d, want an error", b.Seq)
	}

	if s := h.buf.Stats(); s.Records != 2 {
		t.Fatalf("buffer holds %d records, want 2: the oversized record is not acknowledged", s.Records)
	}

	fb.addLoss, fb.syncLosses = faultNone, faultNone
	b := h.next()
	marker := markerOf(model.Bulk, big, big, 1, model.LossRejectedInvalid)
	if got, want := bodySamples(t, b.Body), []string{"temp=10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body samples %v, want %v", got, want)
	}

	// The failed attempt left its marker applied in memory; the retry adds
	// it again: reported twice, never lost.
	if !sameLosses(b.Losses, []model.Loss{marker, marker}) {
		t.Fatalf("batch losses %v, want the marker twice", b.Losses)
	}
}

func TestBatcherRefusesForeignBatches(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 2, MaxBody: 1 << 20}).open()
	h.append(bulk(0), bulk(1))
	b := h.next()
	other := *b

	if err := h.bt.Confirm(&other); err == nil {
		t.Error("Confirm(copy) = nil, want an error")
	}

	if err := h.bt.Drop(&other, model.LossRejectedInvalid); err == nil {
		t.Error("Drop(copy) = nil, want an error")
	}

	if _, err := h.bt.Shrink(&other, 1); err == nil {
		t.Error("Shrink(copy) = nil, want an error")
	}

	if !bytes.Equal(h.next().Body, b.Body) {
		t.Fatal("the current batch changed")
	}
}

func TestNewBatcherRefusesBadLimits(t *testing.T) {
	tests := []output.BatchLimits{
		{MaxSamples: 0, MaxBody: 1},
		{MaxSamples: 1, MaxBody: 0},
		{MaxSamples: 1, MaxBody: 1, MaxWait: -time.Second},
	}
	for _, limits := range tests {
		dir := t.TempDir()
		buf, err := buffer.Open(dir, buffer.Options{Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}

		clk := clock.NewFake(epoch)
		if _, err := output.NewBatcher(buf, output.NewInflight(dir), envelope.NewIDGenerator(clk), clk, limits, nil); err == nil {
			t.Errorf("NewBatcher(%+v) = nil error", limits)
		}

		_ = buf.Close()
	}
}

// TestSealedRecordsCutByRecovery: records are durable only at the next group
// sync, the inflight batch at once. After a crash the buffer cuts the
// records it had not synced while the batch still carries them: settling
// the batch must not wedge on positions the buffer no longer has, and a
// split must count the records it cannot hand back.
func TestSealedRecordsCutByRecovery(t *testing.T) {
	tests := []struct {
		name    string
		settle  func(t *testing.T, h *harness, b *output.Batch)
		pending []model.Loss
	}{
		{
			name:   "confirm",
			settle: func(t *testing.T, h *harness, b *output.Batch) { h.confirm(b) },
		},
		{
			name: "drop",
			settle: func(t *testing.T, h *harness, b *output.Batch) {
				if err := h.bt.Drop(b, model.LossRejectedInvalid); err != nil {
					t.Fatalf("Drop() = %v", err)
				}
			},
			pending: []model.Loss{markerOf(model.Bulk, bulk(0), bulk(2), 3, model.LossRejectedInvalid)},
		},
		{
			name: "shrink then confirm",
			settle: func(t *testing.T, h *harness, b *output.Batch) {
				first, err := h.bt.Shrink(b, 1)
				if err != nil {
					t.Fatalf("Shrink() = %v", err)
				}

				h.confirm(first)
			},
			pending: []model.Loss{markerOf(model.Bulk, bulk(1), bulk(2), 2, model.LossBufferFull)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := bulkFrameSize(t)
			h := newHarness(t, output.BatchLimits{MaxSamples: 3, MaxBody: 1 << 20}).open()
			h.append(bulk(0), bulk(1), bulk(2))
			sealed := h.next()

			if err := h.buf.Close(); err != nil {
				t.Fatal(err)
			}

			h.buf = nil
			segments, err := filepath.Glob(filepath.Join(h.dir, "bulk", "*.seg"))
			if err != nil || len(segments) != 1 {
				t.Fatalf("bulk segments %v, %v; want one", segments, err)
			}

			if err := os.Truncate(segments[0], f); err != nil {
				t.Fatal(err)
			}

			h.open()
			resent := h.next()
			assertSameBatch(t, resent, sealed)
			tc.settle(t, h, resent)

			if pending := h.buf.PendingLosses(); !sameLosses(pending, tc.pending) {
				t.Fatalf("pending losses %v, want %v", pending, tc.pending)
			}

			h.append(bulk(5))
			after := h.next()
			if got, want := bodySamples(t, after.Body), []string{"temp=15"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("samples after the settled batch %v, want %v", got, want)
			}
		})
	}
}

// TestCrashBetweenSaveAndAckLossesReportsMarkersTwice: the batch carrying
// the markers is saved but the markers are still pending in the buffer. They
// travel with the resent batch and again with the next one: reported twice,
// never lost (ruling R40).
func TestCrashBetweenSaveAndAckLossesReportsMarkersTwice(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 1, MaxBody: 1 << 20})
	h.wrap = func(b *buffer.Buffer) output.Buffer { return &faultyBuffer{Buffer: b, ackLosses: faultLost} }
	h.open()
	carried := bufferFull(model.Bulk, 1, 2, 2)
	if err := h.buf.AddLoss(carried); err != nil {
		t.Fatal(err)
	}

	h.append(bulk(0), bulk(1))
	sealed := h.next()

	h.wrap = nil
	h.restart()
	resent := h.next()
	assertSameBatch(t, resent, sealed)

	h.confirm(resent)
	if after := h.next(); !sameLosses(after.Losses, []model.Loss{carried}) {
		t.Fatalf("the next batch carries %v, want %v again", after.Losses, []model.Loss{carried})
	}
}

// TestCrashBetweenConfirmAckAndClear: the records are acknowledged but the
// inflight batch is not cleared. After a restart the identical batch is
// resent (the backend answers duplicate), its acknowledgement is a no-op,
// and nothing appended meanwhile is lost.
func TestCrashBetweenConfirmAckAndClear(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 2, MaxBody: 1 << 20}).open()
	h.append(bulk(0), bulk(1), bulk(2))
	sealed := h.next()

	if err := os.Mkdir(filepath.Join(h.dir, "inflight.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := h.bt.Confirm(sealed); err == nil {
		t.Fatal("Confirm() with a failing inflight write = nil, want an error")
	}

	if s := h.buf.Stats(); s.Records != 1 {
		t.Fatalf("buffer holds %d records after the acknowledgement, want 1", s.Records)
	}

	h.restart()
	resent := h.next()
	assertSameBatch(t, resent, sealed)
	h.confirm(resent)

	h.append(bulk(3))
	after := h.next()
	if got, want := bodySamples(t, after.Body), []string{"temp=12", "temp=13"}; !reflect.DeepEqual(got, want) || after.Seq != sealed.Seq+1 {
		t.Fatalf("next batch seq %d samples %v, want seq %d samples %v", after.Seq, got, sealed.Seq+1, want)
	}
}

// TestShrinkCountsAnInteriorGap: a corrupt record in an older segment drops
// the rest of that segment at recovery, leaving a gap inside the batch's
// records while those after it survive. A split counts the gap, also when
// held records of the remainder lie on both sides of it.
func TestShrinkCountsAnInteriorGap(t *testing.T) {
	tests := []struct {
		name     string
		corrupt  int // index of the bulk record corrupted in the older segment
		gone     [2]int
		wantRest []string
	}{
		{name: "gap at the start of the remainder", corrupt: 1, gone: [2]int{1, 2}, wantRest: []string{"temp=13", "temp=14", "temp=15"}},
		{name: "held records on both sides of the gap", corrupt: 2, gone: [2]int{2, 2}, wantRest: []string{"temp=11", "temp=13", "temp=14", "temp=15"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := bulkFrameSize(t)
			h := newHarness(t, output.BatchLimits{MaxSamples: 6, MaxBody: 1 << 20})
			h.opts.SegmentSize = 3*f + f/2
			h.open()
			h.append(bulk(0), bulk(1), bulk(2), bulk(3), bulk(4), bulk(5))
			sealed := h.next()

			if err := h.buf.Close(); err != nil {
				t.Fatal(err)
			}

			h.buf = nil
			segments, err := filepath.Glob(filepath.Join(h.dir, "bulk", "*.seg"))
			if err != nil || len(segments) < 2 {
				t.Fatalf("bulk segments %v, %v; want at least two", segments, err)
			}

			sort.Strings(segments)
			corruptByte(t, segments[0], int64(tc.corrupt)*f+f/2) // inside the payload of bulk(corrupt)

			h.open()
			resent := h.next()
			assertSameBatch(t, resent, sealed)
			first, err := h.bt.Shrink(resent, 1)
			if err != nil {
				t.Fatalf("Shrink() = %v", err)
			}

			from, to := bulk(tc.gone[0]), bulk(tc.gone[1])
			want := []model.Loss{markerOf(model.Bulk, from, to, tc.gone[1]-tc.gone[0]+1, model.LossBufferFull)}
			if pending := h.buf.PendingLosses(); !sameLosses(pending, want) {
				t.Fatalf("pending losses after Shrink: %v, want %v", pending, want)
			}

			h.confirm(first)
			rest := h.next()
			if got := bodySamples(t, rest.Body); !reflect.DeepEqual(got, tc.wantRest) {
				t.Fatalf("remainder samples %v, want %v", got, tc.wantRest)
			}
		})
	}
}

// TestShrinkHandsTheRestBackToEviction: after a split the batch no longer
// carries the rest, so evicting it later is a loss the buffer counts.
func TestShrinkHandsTheRestBackToEviction(t *testing.T) {
	f := bulkFrameSize(t)
	h := newHarness(t, output.BatchLimits{MaxSamples: 3, MaxBody: 1 << 20})
	h.opts.SegmentSize = 3*f + f/2
	h.opts.MaxSize = 6*f + f/2
	h.open()

	h.append(bulk(0), bulk(1), bulk(2))
	if _, err := h.bt.Shrink(h.next(), 1); err != nil {
		t.Fatalf("Shrink() = %v", err)
	}

	h.append(bulk(3), bulk(4), bulk(5), bulk(6))

	want := []model.Loss{markerOf(model.Bulk, bulk(1), bulk(2), 2, model.LossBufferFull)}
	if pending := h.buf.PendingLosses(); !sameLosses(pending, want) {
		t.Fatalf("pending losses after evicting the handed back records: %v, want %v", pending, want)
	}
}

func corruptByte(t *testing.T, path string, off int64) {
	t.Helper()

	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}

	defer file.Close()

	b := make([]byte, 1)
	if _, err := file.ReadAt(b, off); err != nil {
		t.Fatal(err)
	}

	b[0] ^= 0xff
	if _, err := file.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

// phantomBuffer counts one record more than its queues hold, its appends
// are signalled by the test, and it closes spun at the 100th read: a batcher
// that waits for appends reads twice per signal.
type phantomBuffer struct {
	*buffer.Buffer
	changed chan struct{}
	spun    chan struct{}
	reads   atomic.Int64
}

func (p *phantomBuffer) Stats() buffer.Stats {
	s := p.Buffer.Stats()
	s.Records++

	return s
}

func (p *phantomBuffer) Changed() <-chan struct{} { return p.changed }

func (p *phantomBuffer) Read(pr model.Priority, from buffer.Position, limit int) ([]buffer.Entry, error) {
	if p.reads.Add(1) == 100 {
		close(p.spun)
	}

	return p.Buffer.Read(pr, from, limit)
}

// TestNextNeverSpinsOnACountWithoutRecords: when the record count promises
// more than the queues hold, Next waits for an append instead of reading
// again and again.
func TestNextNeverSpinsOnACountWithoutRecords(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 1 << 20})
	pb := &phantomBuffer{changed: make(chan struct{}), spun: make(chan struct{})}
	h.wrap = func(b *buffer.Buffer) output.Buffer { pb.Buffer = b; return pb }
	h.open()

	res := h.nextAsync(context.Background())
	signal := func() {
		t.Helper()

		select {
		case pb.changed <- struct{}{}:
		case <-pb.spun:
			t.Fatal("Next kept reading an empty buffer instead of waiting for an append")
		}
	}

	signal() // received only while Next waits for an append
	h.append(bulk(0))
	signal()

	r := <-res
	if r.err != nil || r.batch.Len() != 1 {
		t.Fatalf("Next() = %v, %v; want the appended record", r.batch, r.err)
	}
}
