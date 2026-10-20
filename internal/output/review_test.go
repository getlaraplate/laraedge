package output_test

import (
	"context"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/buffer"
	"github.com/getlaraplate/laraplate-edge/internal/model"
	"github.com/getlaraplate/laraplate-edge/internal/output"
)

func death(device string) model.Record {
	return model.EventRecord(model.DeviceEvent{Kind: model.Death, Device: device, TS: epoch})
}

// TestBatcherKeepsSealingAfterARuntimeCorruptFrame: a frame that rots in
// an older segment while the agent runs drops the rest of that segment
// (ruling R20); the batcher seals the records around it instead of failing
// every Next until a restart.
func TestBatcherKeepsSealingAfterARuntimeCorruptFrame(t *testing.T) {
	f := bulkFrameSize(t)
	h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 1 << 20})
	h.opts.SegmentSize = 3*f + f/2
	h.open()
	h.append(bulk(0), bulk(1), bulk(2), bulk(3), bulk(4), bulk(5))

	segments, err := filepath.Glob(filepath.Join(h.dir, "bulk", "*.seg"))
	if err != nil || len(segments) < 2 {
		t.Fatalf("bulk segments %v, %v; want at least two", segments, err)
	}

	sort.Strings(segments)
	corruptByte(t, segments[0], f+f/2) // inside the payload of bulk(1), the buffer open

	b := h.next()
	if got, want := bodySamples(t, b.Body), []string{"temp=10", "temp=13", "temp=14", "temp=15"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batch samples %v, want %v", got, want)
	}

	h.confirm(b)
	if s := h.buf.Stats(); s.CorruptRecords != 1 || s.Records != 0 {
		t.Fatalf("Stats() = %+v, want one corrupt record and nothing left", s)
	}

	h.append(bulk(6))
	if got := bodySamples(t, h.next().Body); !reflect.DeepEqual(got, []string{"temp=16"}) {
		t.Fatalf("next batch samples %v, want temp=16", got)
	}
}

// statsCounter counts the calls of Stats, which reads frames.
type statsCounter struct {
	*buffer.Buffer
	calls atomic.Int64
}

func (s *statsCounter) Stats() buffer.Stats {
	s.calls.Add(1)

	return s.Buffer.Stats()
}

// TestNextWaitsOnTheRecordCount: the batcher wakes on every append and asks
// the buffer only for its record count, never for the full Stats.
func TestNextWaitsOnTheRecordCount(t *testing.T) {
	h := newHarness(t, output.BatchLimits{MaxSamples: 3, MaxBody: 1 << 20, MaxWait: time.Hour})
	counter := &statsCounter{}
	h.wrap = func(b *buffer.Buffer) output.Buffer { counter.Buffer = b; return counter }
	h.open()

	res := h.nextAsync(context.Background())
	h.append(bulk(0), bulk(1), bulk(2))
	if r := <-res; r.err != nil || r.batch.Len() != 3 {
		t.Fatalf("Next() = %v, %v; want the three records", r.batch, r.err)
	}

	if n := counter.calls.Load(); n != 0 {
		t.Fatalf("the batcher called Stats %d times, want 0", n)
	}
}

// TestLossMarkersCountSamplesOnly: a dropped batch's markers count its
// samples; a device event is not a sample, and a drop of events alone
// leaves no marker (the protocol requires samples >= 1).
func TestLossMarkersCountSamplesOnly(t *testing.T) {
	tests := []struct {
		name    string
		records []model.Record
		want    []model.Loss
	}{
		{
			name:    "samples and a death",
			records: []model.Record{death("press-07"), crit(0), bulk(0)},
			want: []model.Loss{
				markerOf(model.Critical, crit(0), crit(0), 1, model.LossRejectedInvalid),
				markerOf(model.Bulk, bulk(0), bulk(0), 1, model.LossRejectedInvalid),
			},
		},
		{name: "a death alone", records: []model.Record{death("press-07")}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 1 << 20}).open()
			h.append(tc.records...)

			if err := h.bt.Drop(h.next(), model.LossRejectedInvalid); err != nil {
				t.Fatalf("Drop() = %v", err)
			}

			if pending := h.buf.PendingLosses(); !sameLosses(pending, tc.want) {
				t.Fatalf("pending losses %+v, want %+v", pending, tc.want)
			}

			if n := h.buf.Records(); n != 0 {
				t.Fatalf("buffer holds %d records after Drop, want 0", n)
			}
		})
	}
}

// TestRejectOfAnOversizedEventLeavesNoMarker: a birth whose envelope alone
// exceeds max_body is dropped and acknowledged without a marker.
func TestRejectOfAnOversizedEventLeavesNoMarker(t *testing.T) {
	signals := make([]model.SignalInfo, 40)
	for i := range signals {
		signals[i] = model.SignalInfo{Signal: strings.Repeat("s", 100) + string(rune('a'+i%26)) + string(rune('a'+i/26)), DataType: model.DataTypeNumber}
	}

	big := model.EventRecord(model.DeviceEvent{Kind: model.Birth, Device: "press-07", TS: epoch, Signals: signals})
	h := newHarness(t, output.BatchLimits{MaxSamples: 10, MaxBody: 1})
	h.limits.MaxBody = envelopeSize(t, h, crit(0)) + 512
	h.open()
	h.append(big, crit(0))

	b := h.next()
	if got := bodySamples(t, b.Body); !reflect.DeepEqual(got, []string{"state=10"}) {
		t.Fatalf("batch %v, want the sample after the dropped birth", got)
	}

	if len(b.Losses) != 0 {
		t.Fatalf("batch carries markers %+v for a dropped birth, want none", b.Losses)
	}
}

// TestHTTPIngestPathIgnoresQueryAndFragment: whatever query or fragment the
// base URL carries, a bare "?" included, the agent posts to the ingest route.
func TestHTTPIngestPathIgnoresQueryAndFragment(t *testing.T) {
	for _, suffix := range []string{"?", "/?", "?a=b", "#", "#frag", "/?#"} {
		t.Run(suffix, func(t *testing.T) {
			f := newFixture(t, 1, reply{status: http.StatusAccepted, body: acceptedBody})
			f.h.open()
			f.h.append(bulk(0))

			cfg := f.config()
			cfg.URL = f.be.srv.URL + suffix
			out, err := output.NewHTTP(cfg)
			if err != nil {
				t.Fatalf("NewHTTP() = %v", err)
			}

			f.out = out
			f.run = startRun(t, out)

			req := f.be.next(t)
			if req.path != ingestPath || req.query != "" {
				t.Fatalf("request to %q?%q, want %q without a query", req.path, req.query, ingestPath)
			}
		})
	}
}
