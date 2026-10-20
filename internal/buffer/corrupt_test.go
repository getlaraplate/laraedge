package buffer

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/model"
)

// corruptFrameAt flips a byte inside the payload of the record'th frame of
// the segment file at path, while the buffer is open: bit rot or a disk
// fault after Open.
func corruptFrameAt(t *testing.T, path string, record int) {
	t.Helper()
	f := frameSize(t)
	flipByte(t, path, int64(record)*f+f/2)
}

// TestRuntimeCorruptFrameSkipsTheRestOfItsSegment applies ruling R20 while
// the buffer runs: a frame found corrupt after Open drops the rest of its
// segment, is counted, and reading goes on at the next segment instead of
// failing for ever.
func TestRuntimeCorruptFrameSkipsTheRestOfItsSegment(t *testing.T) {
	tests := []struct {
		name    string
		segment int // index among the three segments holding bulk 0..5
		record  int // frame index inside that segment
		want    []model.Record
	}{
		{"first record of an older segment", 1, 0, []model.Record{bulk(0), bulk(1), bulk(4), bulk(5)}},
		{"second record of an older segment", 1, 1, []model.Record{bulk(0), bulk(1), bulk(2), bulk(4), bulk(5)}},
		{"record of the segment being written", 2, 0, []model.Record{bulk(0), bulk(1), bulk(2), bulk(3)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(t, clock.NewFake(epoch))
			b := openBuffer(t, dir, opts, hooks{})
			mustAppend(t, b, bulkRange(0, 5)...)

			segs := nonEmptySegments(t, filepath.Join(dir, "bulk"))
			if len(segs) != 3 {
				t.Fatalf("want three segments with data, got %v", segs)
			}

			tail := b.bulk.tail()
			corruptFrameAt(t, segs[tc.segment], tc.record)

			assertRecords(t, b, model.Bulk, tc.want)

			s := b.Stats()
			if s.CorruptRecords != 1 {
				t.Fatalf("CorruptRecords = %d, want 1", s.CorruptRecords)
			}

			if s.Bulk.Records != uint64(len(tc.want)) {
				t.Fatalf("Bulk.Records = %d, want %d", s.Bulk.Records, len(tc.want))
			}

			if pending := b.PendingLosses(); len(pending) != 0 {
				t.Fatalf("corruption produced loss markers: %+v", pending)
			}

			mustAppend(t, b, bulk(6))
			entries := readAll(t, b, model.Bulk)
			if last := entries[len(entries)-1]; last.Record.Sample.TS != tsOf(bulk(6)) || last.Pos < tail {
				t.Fatalf("record appended after the cut at %d, want its own record at or after the old tail %d", last.Pos, tail)
			}

			want := append(tc.want, bulk(6))
			mustClose(t, b)

			b = openBuffer(t, dir, opts, hooks{})
			assertRecords(t, b, model.Bulk, want)
			if s := b.Stats(); s.CorruptRecords != 0 {
				t.Fatalf("CorruptRecords after a restart = %d, want 0: the cut was not written to disk", s.CorruptRecords)
			}

			entries = readAll(t, b, model.Bulk)
			if err := b.Ack(model.Bulk, entries[len(entries)-1].Next); err != nil {
				t.Fatalf("Ack() across the cut = %v", err)
			}

			if s := b.Stats(); s.Records != 0 {
				t.Fatalf("Records after acknowledging everything = %d, want 0", s.Records)
			}
		})
	}
}

// TestAckAcrossARuntimeCorruptFrame: a frame corrupted after the batcher
// read past it is cut when Ack counts the records, and Ack succeeds.
func TestAckAcrossARuntimeCorruptFrame(t *testing.T) {
	dir := t.TempDir()
	b := openBuffer(t, dir, testOptions(t, clock.NewFake(epoch)), hooks{})
	mustAppend(t, b, bulkRange(0, 5)...)
	entries := readAll(t, b, model.Bulk)

	corruptFrameAt(t, nonEmptySegments(t, filepath.Join(dir, "bulk"))[1], 1)
	if err := b.Ack(model.Bulk, entries[len(entries)-1].Next); err != nil {
		t.Fatalf("Ack() = %v, want nil", err)
	}

	if s := b.Stats(); s.Records != 0 || s.CorruptRecords != 1 {
		t.Fatalf("Stats() = %+v, want no records and one corrupt", s)
	}
}

// TestEvictionDeletesASegmentWithARuntimeCorruptFrame: eviction treats a
// segment holding a corrupt frame normally. The marker counts the samples
// read before the corruption; the rest is counted as corrupt.
func TestEvictionDeletesASegmentWithARuntimeCorruptFrame(t *testing.T) {
	f := frameSize(t)
	dir := t.TempDir()
	opts := testOptions(t, clock.NewFake(epoch))
	opts.MaxSize = 6*f + f/2
	b := openBuffer(t, dir, opts, hooks{})
	mustAppend(t, b, bulkRange(0, 5)...)

	corruptFrameAt(t, nonEmptySegments(t, filepath.Join(dir, "bulk"))[0], 1)
	if err := b.Append(bulk(6)); err != nil {
		t.Fatalf("Append() over max_size with a corrupt oldest segment = %v, want nil", err)
	}

	assertLosses(t, b, []model.Loss{loss(model.Bulk, bulk(0), bulk(0), 1)})
	assertRecords(t, b, model.Bulk, bulkRange(2, 6))

	s := b.Stats()
	if s.CorruptRecords != 1 || s.Bulk.Records != 5 {
		t.Fatalf("Stats() = %+v, want 1 corrupt and 5 bulk records", s)
	}

	mustAppend(t, b, bulk(7))
	if errors.Is(b.Append(bulk(8)), ErrDropped) {
		t.Fatal("eviction stays wedged after the corrupt segment")
	}
}
