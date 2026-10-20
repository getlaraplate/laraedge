package buffer

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/clock"
	"github.com/getlaraplate/laraplate-edge/internal/model"
)

var epoch = time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

var errNoSpace = errors.New("no space left on device")

// sampleAt returns a sample of class p; i stays in 10..99 so that every
// frame has the same size.
func sampleAt(p model.Priority, i int) model.Record {
	return model.SampleRecord(model.Sample{
		Device:   "press-01",
		Signal:   "temp",
		TS:       epoch.Add(time.Duration(i) * time.Second),
		Value:    model.IntValue(int64(i)),
		Quality:  model.Good,
		Priority: p,
	})
}

func bulk(i int) model.Record { return sampleAt(model.Bulk, i+10) }

func crit(i int) model.Record { return sampleAt(model.Critical, i+10) }

func bulkRange(from, to int) []model.Record {
	var out []model.Record
	for i := from; i <= to; i++ {
		out = append(out, bulk(i))
	}

	return out
}

func critRange(from, to int) []model.Record {
	var out []model.Record
	for i := from; i <= to; i++ {
		out = append(out, crit(i))
	}

	return out
}

func tsOf(r model.Record) time.Time { return r.Sample.TS }

func loss(p model.Priority, from, to model.Record, samples int) model.Loss {
	return model.Loss{Priority: p, FromTS: tsOf(from), ToTS: tsOf(to), Samples: samples, Reason: model.LossBufferFull}
}

// frameSize returns the size of one frame of bulk(i) or crit(i): they are
// equal by construction.
func frameSize(t *testing.T) int64 {
	t.Helper()
	b, err := encodeRecord(bulk(0))
	if err != nil {
		t.Fatal(err)
	}

	c, err := encodeRecord(crit(89))
	if err != nil {
		t.Fatal(err)
	}

	if len(b) != len(c) {
		t.Fatalf("frame sizes differ: %d and %d", len(b), len(c))
	}

	return int64(len(b))
}

// fakeDisk reports free space from a list consumed one value per call; the
// last value repeats. The total is always 100.
type fakeDisk struct {
	mu    sync.Mutex
	free  []uint64
	err   error
	calls int
}

func newFakeDisk(free ...uint64) *fakeDisk { return &fakeDisk{free: free} }

func (d *fakeDisk) usage(string) (uint64, uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.calls++
	if d.err != nil {
		return 0, 0, d.err
	}

	v := d.free[0]
	if len(d.free) > 1 {
		d.free = d.free[1:]
	}

	return v, 100, nil
}

func (d *fakeDisk) reset(err error, free ...uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.free, d.err, d.calls = free, err, 0
}

func (d *fakeDisk) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.calls
}

// testOptions holds two frames per segment and a large size limit.
func testOptions(t *testing.T, clk clock.Clock) Options {
	f := frameSize(t)

	return Options{
		MaxSize:       1 << 20,
		SegmentSize:   2*f + f/2,
		FsyncInterval: 200 * time.Millisecond,
		Clock:         clk,
		DiskUsage:     newFakeDisk(100).usage,
		Logger:        slog.New(slog.DiscardHandler),
	}
}

func openBuffer(t *testing.T, dir string, opts Options, h hooks) *Buffer {
	t.Helper()
	b, err := open(dir, opts, h)
	if err != nil {
		t.Fatalf("open(%s) = %v", dir, err)
	}

	t.Cleanup(func() { _ = b.Close() })

	return b
}

func mustAppend(t *testing.T, b *Buffer, recs ...model.Record) {
	t.Helper()
	for _, r := range recs {
		if err := b.Append(r); err != nil {
			t.Fatalf("Append(%v) = %v", tsOf(r), err)
		}
	}
}

func mustClose(t *testing.T, b *Buffer) {
	t.Helper()
	if err := b.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

func readAll(t *testing.T, b *Buffer, p model.Priority) []Entry {
	t.Helper()
	var all []Entry
	var from Position
	for {
		entries, err := b.Read(p, from, 3)
		if err != nil {
			t.Fatalf("Read(%v, %d) = %v", p, from, err)
		}

		if len(entries) == 0 {
			return all
		}

		all = append(all, entries...)
		from = entries[len(entries)-1].Next
	}
}

func recordsOf(entries []Entry) []model.Record {
	var out []model.Record
	for _, e := range entries {
		out = append(out, e.Record)
	}

	return out
}

func assertRecords(t *testing.T, b *Buffer, p model.Priority, want []model.Record) {
	t.Helper()
	if got := recordsOf(readAll(t, b, p)); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v queue holds %d records %v, want %d %v", p, len(got), timestamps(got), len(want), timestamps(want))
	}
}

func timestamps(recs []model.Record) []string {
	var out []string
	for _, r := range recs {
		out = append(out, tsOf(r).Format("15:04:05"))
	}

	return out
}

func assertLosses(t *testing.T, b *Buffer, want []model.Loss) {
	t.Helper()
	got := b.PendingLosses()
	if len(got) != len(want) {
		t.Fatalf("PendingLosses() = %+v, want %+v", got, want)
	}

	for i := range got {
		g, w := got[i], want[i]
		if g.Priority != w.Priority || !g.FromTS.Equal(w.FromTS) || !g.ToTS.Equal(w.ToTS) || g.Samples != w.Samples || g.Reason != w.Reason {
			t.Fatalf("PendingLosses()[%d] = %+v, want %+v", i, g, w)
		}
	}
}

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".seg") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}

	slices.Sort(out)

	return out
}

// nonEmptySegments returns the segment files of a queue that hold data.
func nonEmptySegments(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, path := range segmentFiles(t, dir) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		if info.Size() > 0 {
			out = append(out, path)
		}
	}

	return out
}

func TestRoundTripAcrossSegmentsAndReopen(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(t, clock.NewFake(epoch))
	b := openBuffer(t, dir, opts, hooks{})

	var wantCritical, wantBulk []model.Record
	for _, r := range seedRecords() {
		mustAppend(t, b, r)
		if r.Priority() == model.Critical {
			wantCritical = append(wantCritical, r)
		} else {
			wantBulk = append(wantBulk, r)
		}
	}

	extra := bulkRange(0, 6)
	mustAppend(t, b, extra...)
	wantBulk = append(wantBulk, extra...)

	assertRecords(t, b, model.Critical, wantCritical)
	assertRecords(t, b, model.Bulk, wantBulk)

	entries := readAll(t, b, model.Bulk)
	for i := 1; i < len(entries); i++ {
		if entries[i].Pos < entries[i-1].Next {
			t.Fatalf("entry %d starts at %d before the previous end %d", i, entries[i].Pos, entries[i-1].Next)
		}
	}

	if n := len(segmentFiles(t, filepath.Join(dir, "bulk"))); n < 3 {
		t.Fatalf("bulk queue has %d segment files, want several (rotation)", n)
	}

	mustClose(t, b)
	b = openBuffer(t, dir, opts, hooks{})
	assertRecords(t, b, model.Critical, wantCritical)
	assertRecords(t, b, model.Bulk, wantBulk)
}

func TestReadLimitsAndErrors(t *testing.T) {
	b := openBuffer(t, t.TempDir(), testOptions(t, clock.NewFake(epoch)), hooks{})
	mustAppend(t, b, bulkRange(0, 4)...)
	all := readAll(t, b, model.Bulk)

	tests := []struct {
		name    string
		p       model.Priority
		from    Position
		limit   int
		want    []model.Record
		wantErr bool
	}{
		{"first two", model.Bulk, 0, 2, bulkRange(0, 1), false},
		{"from the third", model.Bulk, all[2].Pos, 10, bulkRange(2, 4), false},
		{"at the tail", model.Bulk, all[4].Next, 10, nil, false},
		{"zero limit", model.Bulk, 0, 0, nil, false},
		{"empty queue", model.Critical, 0, 10, nil, false},
		{"not a record boundary", model.Bulk, all[1].Pos + 1, 10, nil, true},
		{"invalid priority", model.Priority(0), 0, 10, nil, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := b.Read(tc.p, tc.from, tc.limit)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Read() error = %v, wantErr %v", err, tc.wantErr)
			}

			if got := recordsOf(entries); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Read() = %v, want %v", timestamps(got), timestamps(tc.want))
			}
		})
	}

	// A start position that is no record boundary is the caller's error,
	// never a corruption: nothing is cut.
	if s := b.Stats(); s.CorruptRecords != 0 || s.Bulk.Records != 5 {
		t.Fatalf("Stats() after the failed reads = %+v, want no corruption and 5 bulk records", s)
	}

	assertRecords(t, b, model.Bulk, bulkRange(0, 4))
}

func TestAppendRefuses(t *testing.T) {
	f := frameSize(t)
	tests := []struct {
		name       string
		mutate     func(*Options)
		record     model.Record
		wantErr    error
		wantLosses []model.Loss
	}{
		{"record larger than a segment", func(o *Options) { o.SegmentSize = f - 1 }, bulk(0), ErrTooLarge, []model.Loss{loss(model.Bulk, bulk(0), bulk(0), 1)}},
		{"record larger than the buffer", func(o *Options) { o.MaxSize = f - 1 }, crit(0), ErrTooLarge, []model.Loss{loss(model.Critical, crit(0), crit(0), 1)}},
		{"invalid record", func(*Options) {}, model.Record{}, nil, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions(t, clock.NewFake(epoch))
			tc.mutate(&opts)
			b := openBuffer(t, t.TempDir(), opts, hooks{})

			err := b.Append(tc.record)
			if err == nil {
				t.Fatal("Append() = nil, want error")
			}

			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Append() = %v, want %v", err, tc.wantErr)
			}

			if s := b.Stats(); s.Records != 0 || s.Bytes != 0 {
				t.Fatalf("Stats() after a refused record = %+v", s)
			}

			assertLosses(t, b, tc.wantLosses)
		})
	}
}

func TestRecoveryTruncatesTheIncompleteTail(t *testing.T) {
	f := frameSize(t)
	next, _ := encodeRecord(bulk(3))

	tests := []struct {
		name     string
		damage   func(t *testing.T, path string)
		wantSize int64
		want     []model.Record
	}{
		{
			name:     "half a header",
			damage:   func(t *testing.T, path string) { appendBytes(t, path, next[:4]) },
			wantSize: 3 * f,
			want:     bulkRange(1, 2),
		},
		{
			name:     "header and part of the payload",
			damage:   func(t *testing.T, path string) { appendBytes(t, path, next[:len(next)-5]) },
			wantSize: 3 * f,
			want:     bulkRange(1, 2),
		},
		{
			name:     "bad checksum in the last record",
			damage:   func(t *testing.T, path string) { flipByte(t, path, 3*f-3) },
			wantSize: 2 * f,
			want:     bulkRange(1, 1),
		},
		{
			name: "length above the segment size",
			damage: func(t *testing.T, path string) {
				appendBytes(t, path, []byte{0, 0, 0, 0x40, 1, 2, 3, 4, 5, 6, 7, 8, 9})
			},
			wantSize: 3 * f,
			want:     bulkRange(1, 2),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(t, clock.NewFake(epoch))
			opts.SegmentSize = 10 * f
			b := openBuffer(t, dir, opts, hooks{})
			mustAppend(t, b, bulkRange(0, 2)...)
			first := readAll(t, b, model.Bulk)[0]
			if err := b.Ack(model.Bulk, first.Next); err != nil {
				t.Fatalf("Ack() = %v", err)
			}

			mustClose(t, b)

			segs := nonEmptySegments(t, filepath.Join(dir, "bulk"))
			if len(segs) != 1 {
				t.Fatalf("want one segment with data, got %v", segs)
			}

			tc.damage(t, segs[0])
			damaged, _ := os.Stat(segs[0])
			base, _ := parseSegmentName(filepath.Base(segs[0]))
			damagedTail := base + Position(damaged.Size())

			b = openBuffer(t, dir, opts, hooks{})
			assertRecords(t, b, model.Bulk, tc.want)

			info, err := os.Stat(segs[0])
			if err != nil {
				t.Fatal(err)
			}

			if info.Size() != tc.wantSize {
				t.Fatalf("segment size after recovery = %d, want %d", info.Size(), tc.wantSize)
			}

			if s := b.Stats(); s.CorruptRecords != 0 || len(b.PendingLosses()) != 0 {
				t.Fatalf("an incomplete tail counted as corruption or loss: %+v", s)
			}

			mustAppend(t, b, bulk(4))
			entries := readAll(t, b, model.Bulk)
			if last := entries[len(entries)-1]; last.Pos < damagedTail {
				t.Fatalf("new record at %d reuses a position below the pre-crash tail %d", last.Pos, damagedTail)
			}
		})
	}
}

func TestRecoveryDropsTheRestOfACorruptInnerSegment(t *testing.T) {
	f := frameSize(t)
	tests := []struct {
		name   string
		record int64
		want   []model.Record
	}{
		{"first record of a middle segment", 0, []model.Record{bulk(0), bulk(1), bulk(4), bulk(5)}},
		{"second record of a middle segment", 1, []model.Record{bulk(0), bulk(1), bulk(2), bulk(4), bulk(5)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(t, clock.NewFake(epoch))
			b := openBuffer(t, dir, opts, hooks{})
			mustAppend(t, b, bulkRange(0, 5)...)
			mustClose(t, b)

			segs := nonEmptySegments(t, filepath.Join(dir, "bulk"))
			if len(segs) != 3 {
				t.Fatalf("want three segments with data, got %v", segs)
			}

			flipByte(t, segs[1], (tc.record+1)*f-3)

			b = openBuffer(t, dir, opts, hooks{})
			assertRecords(t, b, model.Bulk, tc.want)

			s := b.Stats()
			if s.CorruptRecords != 1 {
				t.Fatalf("CorruptRecords = %d, want 1", s.CorruptRecords)
			}

			if s.Bulk.Records != uint64(len(tc.want)) {
				t.Fatalf("Bulk.Records = %d, want %d", s.Bulk.Records, len(tc.want))
			}

			if len(b.PendingLosses()) != 0 {
				t.Fatalf("corruption produced loss markers: %+v", b.PendingLosses())
			}
		})
	}
}

func appendBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	data[off] ^= 0x01
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAckDeletesFullyConfirmedSegments(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(t, clock.NewFake(epoch))
	b := openBuffer(t, dir, opts, hooks{})
	mustAppend(t, b, bulkRange(0, 5)...)

	bulkDir := filepath.Join(dir, "bulk")
	before := nonEmptySegments(t, bulkDir)
	if len(before) != 3 {
		t.Fatalf("want three segments with data, got %v", before)
	}

	entries := readAll(t, b, model.Bulk)
	if err := b.Ack(model.Bulk, entries[3].Next); err != nil {
		t.Fatalf("Ack() = %v", err)
	}

	if after := segmentFiles(t, bulkDir); !reflect.DeepEqual(after, before[2:]) {
		t.Fatalf("segment files after ack = %v, want only %v", after, before[2:])
	}

	if got := b.Stats().Bulk.Records; got != 2 {
		t.Fatalf("Bulk.Records = %d, want 2", got)
	}

	assertRecords(t, b, model.Bulk, bulkRange(4, 5))

	mustClose(t, b)
	b = openBuffer(t, dir, opts, hooks{})
	assertRecords(t, b, model.Bulk, bulkRange(4, 5))
}

func TestAckRules(t *testing.T) {
	tests := []struct {
		name        string
		p           model.Priority
		upTo        func(entries []Entry) Position
		wantErr     bool
		wantRecords uint64
	}{
		{"at the current ack is a no-op", model.Bulk, func(e []Entry) Position { return e[1].Next }, false, 1},
		{"before the current ack is a no-op", model.Bulk, func(e []Entry) Position { return e[0].Next }, false, 1},
		{"zero is a no-op", model.Bulk, func([]Entry) Position { return 0 }, false, 1},
		{"the tail confirms everything", model.Bulk, func(e []Entry) Position { return e[2].Next }, false, 0},
		{"beyond the tail is an error", model.Bulk, func(e []Entry) Position { return e[2].Next + 1 }, true, 1},
		{"not a record boundary is an error", model.Bulk, func(e []Entry) Position { return e[2].Pos + 3 }, true, 1},
		{"invalid priority is an error", model.Priority(9), func(e []Entry) Position { return e[2].Next }, true, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(t, clock.NewFake(epoch))
			opts.SegmentSize = 1 << 16
			b := openBuffer(t, dir, opts, hooks{})
			mustAppend(t, b, bulkRange(0, 2)...)
			entries := readAll(t, b, model.Bulk)
			if err := b.Ack(model.Bulk, entries[1].Next); err != nil {
				t.Fatalf("first Ack() = %v", err)
			}

			err := b.Ack(tc.p, tc.upTo(entries))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Ack() error = %v, wantErr %v", err, tc.wantErr)
			}

			if got := b.Stats().Bulk.Records; got != tc.wantRecords {
				t.Fatalf("Bulk.Records = %d, want %d", got, tc.wantRecords)
			}

			mustClose(t, b)
			b = openBuffer(t, dir, opts, hooks{})
			if got := uint64(len(readAll(t, b, model.Bulk))); got != tc.wantRecords {
				t.Fatalf("after reopen %d records unconfirmed, want %d", got, tc.wantRecords)
			}
		})
	}
}

func TestPositionsAreNeverReused(t *testing.T) {
	tests := []struct {
		name string
		ack  bool
	}{
		{"after every segment was confirmed and deleted", true},
		{"after a clean close", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(t, clock.NewFake(epoch))
			b := openBuffer(t, dir, opts, hooks{})
			mustAppend(t, b, bulkRange(0, 5)...)
			entries := readAll(t, b, model.Bulk)
			tail := entries[len(entries)-1].Next
			if tc.ack {
				if err := b.Ack(model.Bulk, tail); err != nil {
					t.Fatalf("Ack() = %v", err)
				}
			}

			mustClose(t, b)
			b = openBuffer(t, dir, opts, hooks{})
			mustAppend(t, b, bulk(6))

			entries = readAll(t, b, model.Bulk)
			last := entries[len(entries)-1]
			if last.Pos < tail {
				t.Fatalf("new record at %d reuses positions below the old tail %d", last.Pos, tail)
			}

			if err := b.Ack(model.Bulk, last.Next); err != nil {
				t.Fatalf("Ack() across the reopen gap = %v", err)
			}

			if got := b.Stats().Bulk.Records; got != 0 {
				t.Fatalf("Bulk.Records after confirming everything = %d", got)
			}
		})
	}
}

func TestEvictionAtMaxSize(t *testing.T) {
	f := frameSize(t)
	limit := func(frames int64) int64 { return frames*f + f/2 }

	tests := []struct {
		name          string
		critical      int
		bulk          int
		ackBulk       int
		sealBulk      int
		maxSize       int64
		reopenMaxSize int64
		trigger       model.Record
		wantLosses    []model.Loss
		wantCritical  []model.Record
		wantBulk      []model.Record
	}{
		{
			name: "the oldest bulk segment goes first", critical: 2, bulk: 6, maxSize: limit(8), trigger: bulk(6),
			wantLosses:   []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2)},
			wantCritical: critRange(0, 1), wantBulk: bulkRange(2, 6),
		},
		{
			name: "a critical record also evicts bulk first", critical: 2, bulk: 6, maxSize: limit(8), trigger: crit(2),
			wantLosses:   []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2)},
			wantCritical: critRange(0, 2), wantBulk: bulkRange(2, 5),
		},
		{
			name: "critical goes only when it fills the buffer alone", critical: 4, maxSize: limit(4), trigger: crit(4),
			wantLosses:   []model.Loss{loss(model.Critical, crit(0), crit(1), 2)},
			wantCritical: critRange(2, 4),
		},
		{
			name: "the only bulk segment is rotated and evicted", critical: 4, bulk: 1, maxSize: limit(5), trigger: crit(4),
			wantLosses:   []model.Loss{loss(model.Bulk, bulk(0), bulk(0), 1)},
			wantCritical: critRange(0, 4),
		},
		{
			name: "confirmed records are not counted", bulk: 6, ackBulk: 1, maxSize: limit(6), trigger: bulk(6),
			wantLosses: []model.Loss{loss(model.Bulk, bulk(1), bulk(1), 1)},
			wantBulk:   bulkRange(2, 6),
		},
		{
			name: "sealed records are not counted and leave no marker", bulk: 6, sealBulk: 2, maxSize: limit(6), trigger: bulk(6),
			wantBulk: bulkRange(2, 6),
		},
		{
			name: "a smaller limit evicts until the record fits", bulk: 6, maxSize: limit(8), reopenMaxSize: limit(4), trigger: bulk(6),
			wantLosses: []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2), loss(model.Bulk, bulk(2), bulk(3), 2)},
			wantBulk:   bulkRange(4, 6),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(t, clock.NewFake(epoch))
			opts.MaxSize = tc.maxSize
			b := openBuffer(t, dir, opts, hooks{})
			mustAppend(t, b, critRange(0, tc.critical-1)...)
			mustAppend(t, b, bulkRange(0, tc.bulk-1)...)

			entries := readAll(t, b, model.Bulk)
			if tc.ackBulk > 0 {
				if err := b.Ack(model.Bulk, entries[tc.ackBulk-1].Next); err != nil {
					t.Fatalf("Ack() = %v", err)
				}
			}

			if tc.sealBulk > 0 {
				if err := b.MarkSealed(model.Bulk, entries[tc.sealBulk-1].Next); err != nil {
					t.Fatalf("MarkSealed() = %v", err)
				}
			}

			if tc.reopenMaxSize > 0 {
				mustClose(t, b)
				opts.MaxSize = tc.reopenMaxSize
				b = openBuffer(t, dir, opts, hooks{})
			}

			if len(b.PendingLosses()) != 0 {
				t.Fatalf("losses before the trigger: %+v", b.PendingLosses())
			}

			mustAppend(t, b, tc.trigger)
			assertLosses(t, b, tc.wantLosses)
			assertRecords(t, b, model.Critical, tc.wantCritical)
			assertRecords(t, b, model.Bulk, tc.wantBulk)

			s := b.Stats()
			if s.Bytes > opts.MaxSize {
				t.Fatalf("Stats().Bytes = %d above MaxSize %d", s.Bytes, opts.MaxSize)
			}

			var dropped uint64
			for _, l := range tc.wantLosses {
				dropped += uint64(l.Samples)
			}

			if s.Dropped[model.LossBufferFull] != dropped {
				t.Fatalf("Stats().Dropped = %v, want %d buffer_full", s.Dropped, dropped)
			}

			mustClose(t, b)
			b = openBuffer(t, dir, opts, hooks{})
			assertLosses(t, b, tc.wantLosses)
		})
	}
}

func TestFreeDiskFloor(t *testing.T) {
	f := frameSize(t)
	tests := []struct {
		name          string
		segmentFrames int64
		bulk          int
		advance       time.Duration
		usageErr      error
		free          []uint64
		trigger       model.Record
		wantCalls     int
		wantDropped   bool
		wantLosses    []model.Loss
		wantBulk      []model.Record
	}{
		{
			name: "a rotation reads the floor at once", segmentFrames: 2, bulk: 4,
			free: []uint64{4, 50}, trigger: bulk(4), wantCalls: 2,
			wantLosses: []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2)},
			wantBulk:   bulkRange(2, 4),
		},
		{
			name: "the floor is read at most once per second", segmentFrames: 10, bulk: 2,
			free: []uint64{4, 50}, trigger: bulk(2), wantCalls: 0,
			wantBulk: bulkRange(0, 2),
		},
		{
			name: "a second later the floor evicts the bulk active segment", segmentFrames: 10, bulk: 2, advance: time.Second,
			free: []uint64{4, 50}, trigger: bulk(2), wantCalls: 2,
			wantLosses: []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2)},
			wantBulk:   bulkRange(2, 2),
		},
		{
			name: "unreadable disk statistics drop nothing", segmentFrames: 10, bulk: 2, advance: time.Second,
			usageErr: errors.New("statfs failed"), trigger: bulk(2), wantCalls: 1,
			wantBulk: bulkRange(0, 2),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock.NewFake(epoch)
			disk := newFakeDisk(100)
			opts := testOptions(t, clk)
			opts.SegmentSize = tc.segmentFrames*f + f/2
			opts.DiskUsage = disk.usage
			b := openBuffer(t, t.TempDir(), opts, hooks{})
			mustAppend(t, b, bulkRange(0, tc.bulk-1)...)

			disk.reset(tc.usageErr, tc.free...)
			clk.Advance(tc.advance)
			if err := b.Append(tc.trigger); tc.wantDropped != errors.Is(err, ErrDropped) || (!tc.wantDropped && err != nil) {
				t.Fatalf("Append() = %v, wantDropped %v", err, tc.wantDropped)
			}

			if got := disk.callCount(); got != tc.wantCalls {
				t.Fatalf("disk usage read %d times, want %d", got, tc.wantCalls)
			}

			assertLosses(t, b, tc.wantLosses)
			assertRecords(t, b, model.Bulk, tc.wantBulk)
		})
	}
}

// failingWriter fails the next failures segment writes, optionally after
// writing half of the frame, like a disk that fills up mid-write.
type failingWriter struct {
	failures int
	partial  bool
}

func (w *failingWriter) write(f *os.File, p []byte, off int64) (int, error) {
	if w.failures == 0 {
		return f.WriteAt(p, off)
	}

	w.failures--
	if w.partial {
		n, _ := f.WriteAt(p[:len(p)/2], off)
		return n, errNoSpace
	}

	return 0, errNoSpace
}

func TestDiskFullWhileWriting(t *testing.T) {
	tests := []struct {
		name         string
		bulk         int
		failures     int
		partial      bool
		trigger      model.Record
		wantDropped  bool
		wantLosses   []model.Loss
		wantCritical []model.Record
		wantBulk     []model.Record
	}{
		{
			name: "the retry succeeds after evicting bulk", bulk: 4, failures: 1, trigger: crit(0),
			wantLosses:   []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2)},
			wantCritical: []model.Record{crit(0), crit(1)}, wantBulk: bulkRange(2, 3),
		},
		{
			name: "a partial write is cut back before the retry", bulk: 4, failures: 1, partial: true, trigger: crit(0),
			wantLosses:   []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2)},
			wantCritical: []model.Record{crit(0), crit(1)}, wantBulk: bulkRange(2, 3),
		},
		{
			name: "a failed retry drops the record with its own marker", bulk: 4, failures: 2, partial: true, trigger: crit(0),
			wantDropped:  true,
			wantLosses:   []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2), loss(model.Critical, crit(0), crit(0), 1)},
			wantCritical: []model.Record{crit(1)}, wantBulk: bulkRange(2, 3),
		},
		{
			name: "with nothing to evict the record is dropped with a marker", failures: 2, trigger: bulk(0),
			wantDropped:  true,
			wantLosses:   []model.Loss{loss(model.Bulk, bulk(0), bulk(0), 1)},
			wantCritical: []model.Record{crit(1)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(t, clock.NewFake(epoch))
			writer := &failingWriter{}
			b := openBuffer(t, dir, opts, hooks{write: writer.write})
			mustAppend(t, b, bulkRange(0, tc.bulk-1)...)

			writer.failures, writer.partial = tc.failures, tc.partial
			err := b.Append(tc.trigger)
			if tc.wantDropped {
				if !errors.Is(err, ErrDropped) {
					t.Fatalf("Append() = %v, want ErrDropped", err)
				}
			} else if err != nil {
				t.Fatalf("Append() = %v, want nil after the retry", err)
			}

			if onDisk, counted := diskBytes(t, dir), b.Stats().Bytes; onDisk != counted {
				t.Fatalf("segment files hold %d bytes, the buffer counts %d: a failed write was not cut back", onDisk, counted)
			}

			mustAppend(t, b, crit(1))
			assertLosses(t, b, tc.wantLosses)
			assertRecords(t, b, model.Critical, tc.wantCritical)
			assertRecords(t, b, model.Bulk, tc.wantBulk)

			mustClose(t, b)
			b = openBuffer(t, dir, opts, hooks{})
			assertRecords(t, b, model.Critical, tc.wantCritical)
			assertRecords(t, b, model.Bulk, tc.wantBulk)
			assertLosses(t, b, tc.wantLosses)
		})
	}
}

// diskBytes sums the sizes of the segment files of both queues.
func diskBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	for _, q := range []string{"critical", "bulk"} {
		for _, path := range segmentFiles(t, filepath.Join(dir, q)) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			n += info.Size()
		}
	}

	return n
}

func TestLossMarkersAreDurable(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(t, clock.NewFake(epoch))
	b := openBuffer(t, dir, opts, hooks{})

	first := model.Loss{Priority: model.Bulk, FromTS: epoch, ToTS: epoch.Add(time.Minute), Samples: 10, Reason: model.LossBufferFull}
	second := model.Loss{Priority: model.Critical, FromTS: epoch, ToTS: epoch, Samples: 1, Reason: model.LossRejectedInvalid}

	for _, l := range []model.Loss{first, second} {
		if err := b.AddLoss(l); err != nil {
			t.Fatalf("AddLoss() = %v", err)
		}
	}

	if err := b.AddLoss(model.Loss{Priority: model.Bulk, Reason: "other"}); err == nil {
		t.Fatal("AddLoss(invalid) = nil, want error")
	}

	assertLosses(t, b, []model.Loss{first, second})
	if got := b.Stats().Dropped; got[model.LossBufferFull] != 10 || got[model.LossRejectedInvalid] != 1 {
		t.Fatalf("Stats().Dropped = %v", got)
	}

	mustClose(t, b)
	b = openBuffer(t, dir, opts, hooks{})
	assertLosses(t, b, []model.Loss{first, second})

	for _, n := range []int{-1, 3} {
		if err := b.AckLosses(n); err == nil {
			t.Fatalf("AckLosses(%d) = nil, want error", n)
		}
	}

	if err := b.AckLosses(1); err != nil {
		t.Fatalf("AckLosses(1) = %v", err)
	}

	assertLosses(t, b, []model.Loss{second})
	mustClose(t, b)
	b = openBuffer(t, dir, opts, hooks{})
	assertLosses(t, b, []model.Loss{second})

	if err := b.AckLosses(1); err != nil {
		t.Fatalf("AckLosses(1) = %v", err)
	}

	mustClose(t, b)
	b = openBuffer(t, dir, opts, hooks{})
	assertLosses(t, b, nil)

	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestGroupFsyncAndClose(t *testing.T) {
	clk := clock.NewFake(epoch)
	armed := make(chan struct{}, 8)
	synced := make(chan int, 8)
	h := hooks{
		armed:  func() { armed <- struct{}{} },
		synced: func(files int) { synced <- files },
	}

	opts := testOptions(t, clk)
	b := openBuffer(t, t.TempDir(), opts, h)
	<-armed

	mustAppend(t, b, bulk(0), crit(0))
	clk.Advance(opts.FsyncInterval - time.Millisecond)
	select {
	case n := <-synced:
		t.Fatalf("synced %d files before the interval elapsed", n)
	default:
	}

	clk.Advance(time.Millisecond)
	if n := <-synced; n != 2 {
		t.Fatalf("first sync covered %d files, want 2 (both active segments)", n)
	}

	<-armed
	clk.Advance(opts.FsyncInterval)
	if n := <-synced; n != 0 {
		t.Fatalf("sync without new writes covered %d files, want 0", n)
	}

	<-armed
	mustClose(t, b)
	if n := clk.PendingTimers(); n != 0 {
		t.Fatalf("PendingTimers() after Close = %d, want 0 (sync goroutine still waiting)", n)
	}

	if err := b.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}

	calls := []struct {
		name string
		call func() error
	}{
		{"Append", func() error { return b.Append(bulk(1)) }},
		{"Read", func() error { _, err := b.Read(model.Bulk, 0, 1); return err }},
		{"Ack", func() error { return b.Ack(model.Bulk, 1) }},
		{"MarkSealed", func() error { return b.MarkSealed(model.Bulk, 1) }},
		{"AddLoss", func() error { return b.AddLoss(loss(model.Bulk, bulk(0), bulk(0), 1)) }},
		{"AckLosses", func() error { return b.AckLosses(0) }},
	}
	for _, c := range calls {
		if err := c.call(); !errors.Is(err, ErrClosed) {
			t.Fatalf("%s after Close = %v, want ErrClosed", c.name, err)
		}
	}
}

func TestChangedSignalsWithoutBlocking(t *testing.T) {
	b := openBuffer(t, t.TempDir(), testOptions(t, clock.NewFake(epoch)), hooks{})

	select {
	case <-b.Changed():
		t.Fatal("Changed() signalled before any Append")
	default:
	}

	mustAppend(t, b, bulk(0), bulk(1), crit(0))
	select {
	case <-b.Changed():
	default:
		t.Fatal("Changed() not signalled after Append")
	}

	select {
	case <-b.Changed():
		t.Fatal("Changed() holds more than one pending signal")
	default:
	}
}

func TestStats(t *testing.T) {
	f := frameSize(t)
	b := openBuffer(t, t.TempDir(), testOptions(t, clock.NewFake(epoch)), hooks{})

	if s := b.Stats(); s.Records != 0 || s.Bytes != 0 || !s.Critical.OldestUnacked.IsZero() || s.Dropped == nil {
		t.Fatalf("Stats() of an empty buffer = %+v", s)
	}

	mustAppend(t, b, crit(0), crit(1))
	mustAppend(t, b, bulkRange(0, 2)...)
	if err := b.Ack(model.Bulk, readAll(t, b, model.Bulk)[0].Next); err != nil {
		t.Fatal(err)
	}

	s := b.Stats()
	want := Stats{
		Critical: QueueStats{Records: 2, Bytes: 2 * f, OldestUnacked: tsOf(crit(0))},
		Bulk:     QueueStats{Records: 2, Bytes: 3 * f, OldestUnacked: tsOf(bulk(1))},
		Records:  4,
		Bytes:    5 * f,
		Dropped:  map[string]uint64{},
	}

	if !reflect.DeepEqual(s, want) {
		t.Fatalf("Stats() = %+v, want %+v", s, want)
	}
}

func TestConcurrentAppendReadAck(t *testing.T) {
	const total = 300
	b := openBuffer(t, t.TempDir(), testOptions(t, clock.Real()), hooks{})

	errc := make(chan error, 1)
	go func() {
		from := map[model.Priority]Position{}
		last := map[model.Priority]int64{}
		seen := 0
		for seen < total {
			<-b.Changed()
			for _, p := range []model.Priority{model.Critical, model.Bulk} {
				for {
					entries, err := b.Read(p, from[p], 7)
					if err != nil {
						errc <- err
						return
					}

					if len(entries) == 0 {
						break
					}

					for _, e := range entries {
						if v := e.Record.Sample.Value.Int; v <= last[p] {
							errc <- errors.New("records out of order")
							return
						}

						last[p] = e.Record.Sample.Value.Int
					}

					seen += len(entries)
					from[p] = entries[len(entries)-1].Next
					if err := b.Ack(p, from[p]); err != nil {
						errc <- err
						return
					}
				}
			}
		}

		errc <- nil
	}()

	for i := range total {
		p := model.Bulk
		if i%3 == 0 {
			p = model.Critical
		}

		mustAppend(t, b, sampleAt(p, 10+i))
	}

	if err := <-errc; err != nil {
		t.Fatalf("consumer: %v", err)
	}

	if s := b.Stats(); s.Records != 0 {
		t.Fatalf("Stats().Records after consuming everything = %d", s.Records)
	}
}

func TestOpenOptions(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"negative max size", Options{MaxSize: -1}, true},
		{"negative segment size", Options{SegmentSize: -1}, true},
		{"segment size above 1 GiB", Options{SegmentSize: 1<<30 + 1}, true},
		{"negative fsync interval", Options{FsyncInterval: -time.Second}, true},
		{"negative free ratio", Options{MinFreeRatio: -0.1}, true},
		{"free ratio of one", Options{MinFreeRatio: 1}, true},
		{"zero values take the defaults", Options{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Open(t.TempDir(), tc.opts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Open() error = %v, wantErr %v", err, tc.wantErr)
			}

			if err != nil {
				return
			}

			defer b.Close()
			o := b.opts
			if o.MaxSize != 1<<30 || o.MinFreeRatio != 0.05 || o.FsyncInterval != 200*time.Millisecond || o.SegmentSize != 16<<20 || o.Clock == nil || o.DiskUsage == nil || o.Logger == nil {
				t.Fatalf("defaults = %+v", o)
			}
		})
	}
}

func TestOpenLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outputs", "laraplate")
	openBuffer(t, dir, testOptions(t, clock.NewFake(epoch)), hooks{})

	for _, name := range []string{"critical", "bulk", "acked", "losses", "critical/segment_size", "bulk/segment_size"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestRealDiskUsage(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "windows":
	default:
		t.Skipf("no disk usage implementation on %s", runtime.GOOS)
	}

	free, total, err := diskUsage(t.TempDir())
	if err != nil {
		t.Fatalf("diskUsage() = %v", err)
	}

	if total == 0 || free > total {
		t.Fatalf("diskUsage() = free %d, total %d", free, total)
	}
}

// TestRotationSyncsTheSegmentAPendingSyncPassTook holds the first sync pass
// between taking its files and syncing them while an Append rotates.
func TestRotationSyncsTheSegmentAPendingSyncPassTook(t *testing.T) {
	clk := clock.NewFake(epoch)
	armed := make(chan struct{}, 8)
	synced := make(chan int, 8)
	captured := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var syncedFiles []string

	h := hooks{
		sync: func(f *os.File) error {
			err := f.Sync()
			if err == nil {
				mu.Lock()
				syncedFiles = append(syncedFiles, f.Name())
				mu.Unlock()
			}

			return err
		},
		armed:      func() { armed <- struct{}{} },
		beforeSync: func() { once.Do(func() { captured <- struct{}{}; <-release }) },
		synced:     func(n int) { synced <- n },
	}

	dir := t.TempDir()
	opts := testOptions(t, clk)
	b := openBuffer(t, dir, opts, h)
	<-armed

	mustAppend(t, b, bulk(0), bulk(1)) // the segment is full: the next record rotates it
	old := segmentFiles(t, filepath.Join(dir, "bulk"))[0]
	clk.Advance(opts.FsyncInterval)
	<-captured // the pass holds the active bulk file and has cleared its dirty flag

	mustAppend(t, b, bulk(2)) // rotates the segment the pass holds, writing nothing more to it
	if n := len(segmentFiles(t, filepath.Join(dir, "bulk"))); n != 2 {
		t.Fatalf("bulk queue has %d segment files, want 2 after the rotation", n)
	}

	close(release)
	<-synced

	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(syncedFiles, old) {
		t.Fatalf("rotated segment %s never synced; synced files: %v", old, syncedFiles)
	}
}

// failingLosses fails the next failures writes of the losses file; a
// negative count fails every write.
type failingLosses struct {
	mu       sync.Mutex
	failures int
}

func (w *failingLosses) set(failures int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.failures = failures
}

func (w *failingLosses) write(dir string, losses []model.Loss) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.failures != 0 {
		if w.failures > 0 {
			w.failures--
		}

		return errNoSpace
	}

	return writeLosses(dir, losses)
}

func durableLosses(t *testing.T, dir string) []model.Loss {
	t.Helper()
	losses, err := readLosses(dir)
	if err != nil {
		t.Fatalf("readLosses() = %v", err)
	}

	return losses
}

func assertSameLosses(t *testing.T, got, want []model.Loss) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("losses = %+v, want %+v", got, want)
	}

	for i := range got {
		g, w := got[i], want[i]
		if g.Priority != w.Priority || !g.FromTS.Equal(w.FromTS) || !g.ToTS.Equal(w.ToTS) || g.Samples != w.Samples || g.Reason != w.Reason {
			t.Fatalf("losses[%d] = %+v, want %+v", i, g, w)
		}
	}
}

func TestAddLossReportsAMarkerNotYetDurable(t *testing.T) {
	dir := t.TempDir()
	writer := &failingLosses{}
	b := openBuffer(t, dir, testOptions(t, clock.NewFake(epoch)), hooks{writeLosses: writer.write})
	marker := loss(model.Bulk, bulk(0), bulk(0), 1)

	writer.set(1)
	if err := b.AddLoss(marker); !errors.Is(err, ErrNotDurable) {
		t.Fatalf("AddLoss() = %v, want ErrNotDurable", err)
	}

	assertLosses(t, b, []model.Loss{marker})
	assertSameLosses(t, durableLosses(t, dir), nil)

	b.syncPass()
	assertSameLosses(t, durableLosses(t, dir), []model.Loss{marker})

	writer.set(1)
	if err := b.AckLosses(1); !errors.Is(err, ErrNotDurable) {
		t.Fatalf("AckLosses() = %v, want ErrNotDurable", err)
	}

	assertLosses(t, b, nil)
	b.syncPass()
	assertSameLosses(t, durableLosses(t, dir), nil)
}

func TestSyncLossesWritesPendingMarkersNow(t *testing.T) {
	tests := []struct {
		name        string
		addFailures int // failed writes when the marker is added
		syncFailure bool
		wantErr     error
		wantDurable bool
	}{
		{name: "nothing waiting", wantDurable: true},
		{name: "a marker left by a failed write is written", addFailures: 1, wantDurable: true},
		{name: "the write fails again", addFailures: 1, syncFailure: true, wantErr: ErrNotDurable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writer := &failingLosses{}
			b := openBuffer(t, dir, testOptions(t, clock.NewFake(epoch)), hooks{writeLosses: writer.write})
			marker := loss(model.Bulk, bulk(0), bulk(0), 1)

			writer.set(tc.addFailures)
			_ = b.AddLoss(marker)
			if tc.syncFailure {
				writer.set(1)
			}

			if err := b.SyncLosses(); !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("SyncLosses() = %v, want %v", err, tc.wantErr)
			}

			want := []model.Loss(nil)
			if tc.wantDurable {
				want = []model.Loss{marker}
			}

			assertSameLosses(t, durableLosses(t, dir), want)
			assertLosses(t, b, []model.Loss{marker})
		})
	}

	b := openBuffer(t, t.TempDir(), testOptions(t, clock.NewFake(epoch)), hooks{})
	mustClose(t, b)
	if err := b.SyncLosses(); !errors.Is(err, ErrClosed) {
		t.Fatalf("SyncLosses() after Close = %v, want ErrClosed", err)
	}
}

func TestEvictionWaitsForDurableMarkers(t *testing.T) {
	f := frameSize(t)
	limit := func(frames int64) int64 { return frames*f + f/2 }

	tests := []struct {
		name        string
		frames      int64
		appends     int
		failures    int
		wantDropped bool
		wantLosses  []model.Loss
		wantBulk    []model.Record
		wantDurable []model.Loss
	}{
		{
			name: "the marker is written right after the deletion freed space", frames: 5, appends: 1, failures: 1,
			wantLosses:  []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2)},
			wantBulk:    bulkRange(2, 6),
			wantDurable: []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2)},
		},
		{
			name: "the write after the deletion succeeds and eviction goes on", frames: 4, appends: 1, failures: 1,
			wantLosses:  []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2), loss(model.Bulk, bulk(2), bulk(3), 2)},
			wantBulk:    bulkRange(4, 6),
			wantDurable: []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2), loss(model.Bulk, bulk(2), bulk(3), 2)},
		},
		{
			name: "a marker that stays unwritten stops eviction and drops the record into it", frames: 4, appends: 1, failures: -1, wantDropped: true,
			wantLosses: []model.Loss{loss(model.Bulk, bulk(0), bulk(6), 3)},
			wantBulk:   bulkRange(2, 5),
		},
		{
			name: "a lasting write failure keeps one marker in memory", frames: 4, appends: 40, failures: -1, wantDropped: true,
			wantLosses: []model.Loss{loss(model.Bulk, bulk(0), bulk(45), 42)},
			wantBulk:   bulkRange(2, 5),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writer := &failingLosses{}
			opts := testOptions(t, clock.NewFake(epoch))
			opts.MaxSize = limit(8)
			b := openBuffer(t, dir, opts, hooks{writeLosses: writer.write})
			mustAppend(t, b, bulkRange(0, 5)...)
			mustClose(t, b)

			opts.MaxSize = limit(tc.frames)
			b = openBuffer(t, dir, opts, hooks{writeLosses: writer.write})
			writer.set(tc.failures)
			for i := range tc.appends {
				if err := b.Append(bulk(6 + i)); tc.wantDropped != errors.Is(err, ErrDropped) || (!tc.wantDropped && err != nil) {
					t.Fatalf("Append(bulk(%d)) = %v, wantDropped %v", 6+i, err, tc.wantDropped)
				}
			}

			assertLosses(t, b, tc.wantLosses)
			assertRecords(t, b, model.Bulk, tc.wantBulk)
			assertSameLosses(t, durableLosses(t, dir), tc.wantDurable)

			writer.set(0)
			mustClose(t, b)
			b = openBuffer(t, dir, opts, hooks{})
			assertLosses(t, b, tc.wantLosses)
			assertRecords(t, b, model.Bulk, tc.wantBulk)
		})
	}
}

func TestFloorBreachCoalescesDrops(t *testing.T) {
	type step struct {
		record  model.Record
		handOut bool // call PendingLosses before this record
	}

	bulkSteps := func(from, to int) []step {
		var out []step
		for i := from; i <= to; i++ {
			out = append(out, step{record: bulk(i)})
		}

		return out
	}

	tests := []struct {
		name  string
		steps []step
		want  []model.Loss
	}{
		{
			name:  "a lasting breach leaves one growing marker",
			steps: bulkSteps(2, 61),
			want:  []model.Loss{loss(model.Bulk, bulk(2), bulk(61), 60)},
		},
		{
			name:  "another class in between starts new markers",
			steps: []step{{record: bulk(2)}, {record: bulk(3)}, {record: crit(0)}, {record: bulk(4)}},
			want:  []model.Loss{loss(model.Bulk, bulk(2), bulk(3), 2), loss(model.Critical, crit(0), crit(0), 1), loss(model.Bulk, bulk(4), bulk(4), 1)},
		},
		{
			name:  "a marker handed out is never changed",
			steps: []step{{record: bulk(2)}, {record: bulk(3), handOut: true}, {record: bulk(4)}},
			want:  []model.Loss{loss(model.Bulk, bulk(2), bulk(2), 1), loss(model.Bulk, bulk(3), bulk(4), 2)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := frameSize(t)
			dir := t.TempDir()
			clk := clock.NewFake(epoch)
			disk := newFakeDisk(100)
			opts := testOptions(t, clk)
			opts.SegmentSize = 10*f + f/2
			opts.DiskUsage = disk.usage
			b := openBuffer(t, dir, opts, hooks{})

			before := append(segmentFiles(t, filepath.Join(dir, "critical")), segmentFiles(t, filepath.Join(dir, "bulk"))...)
			disk.reset(nil, 1)
			var samples uint64
			for _, st := range tc.steps {
				clk.Advance(time.Second)
				if st.handOut {
					b.PendingLosses()
				}

				if err := b.Append(st.record); !errors.Is(err, ErrDropped) {
					t.Fatalf("Append() under a breached floor = %v, want ErrDropped", err)
				}

				samples++
			}

			after := append(segmentFiles(t, filepath.Join(dir, "critical")), segmentFiles(t, filepath.Join(dir, "bulk"))...)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("segment files changed under the floor: %v, before %v", after, before)
			}

			assertLosses(t, b, tc.want)
			assertRecords(t, b, model.Bulk, nil)
			if got := b.Stats().Dropped[model.LossBufferFull]; got != samples {
				t.Fatalf("Stats().Dropped = %d, want %d", got, samples)
			}

			b.syncPass()
			assertSameLosses(t, durableLosses(t, dir), tc.want)

			info, err := os.Stat(filepath.Join(dir, "losses"))
			if err != nil {
				t.Fatal(err)
			}

			if info.Size() > 1024 {
				t.Fatalf("losses file grew to %d bytes", info.Size())
			}
		})
	}
}

func TestSegmentSizeIsPinnedPerQueue(t *testing.T) {
	f := frameSize(t)
	dir := t.TempDir()
	opts := testOptions(t, clock.NewFake(epoch))
	opts.SegmentSize = 10 * f
	b := openBuffer(t, dir, opts, hooks{})
	mustAppend(t, b, bulkRange(0, 2)...)
	entries := readAll(t, b, model.Bulk)
	oldTail := entries[0].Pos + Position(10*f)
	mustClose(t, b)

	opts.SegmentSize = f - 1
	b = openBuffer(t, dir, opts, hooks{})
	assertRecords(t, b, model.Bulk, bulkRange(0, 2))
	if s := b.Stats(); s.CorruptRecords != 0 {
		t.Fatalf("CorruptRecords = %d after reopening with a smaller segment size", s.CorruptRecords)
	}

	mustAppend(t, b, bulk(3), crit(0))
	entries = readAll(t, b, model.Bulk)
	if last := entries[len(entries)-1]; last.Pos < oldTail {
		t.Fatalf("new record at %d below the old segment end %d", last.Pos, oldTail)
	}
}

func TestHead(t *testing.T) {
	f := frameSize(t)
	limit := func(frames int64) int64 { return frames*f + f/2 }

	tests := []struct {
		name    string
		p       model.Priority
		prepare func(t *testing.T, b *Buffer, bulkEntries []Entry) Position
	}{
		{"an untouched queue starts at its first record", model.Bulk, func(_ *testing.T, _ *Buffer, e []Entry) Position { return e[0].Pos }},
		{"after an ack", model.Bulk, func(t *testing.T, b *Buffer, e []Entry) Position {
			if err := b.Ack(model.Bulk, e[2].Next); err != nil {
				t.Fatal(err)
			}

			return e[2].Next
		}},
		{"after an eviction", model.Bulk, func(t *testing.T, b *Buffer, e []Entry) Position {
			mustAppend(t, b, bulkRange(4, 6)...)

			return e[2].Pos
		}},
		{"an invalid priority", model.Priority(0), func(*testing.T, *Buffer, []Entry) Position { return 0 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions(t, clock.NewFake(epoch))
			opts.MaxSize = limit(6)
			b := openBuffer(t, t.TempDir(), opts, hooks{})
			mustAppend(t, b, bulkRange(0, 3)...)
			want := tc.prepare(t, b, readAll(t, b, model.Bulk))
			if got := b.Head(tc.p); got != want {
				t.Fatalf("Head() = %d, want %d", got, want)
			}
		})
	}
}

func TestBlockBytesClampsNegativeCounts(t *testing.T) {
	tests := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"signed negative", blockBytes(int64(-5), 4096), 0},
		{"signed zero", blockBytes(int64(0), 4096), 0},
		{"signed positive", blockBytes(int64(3), 4096), 3 * 4096},
		{"unsigned", blockBytes(uint64(7), 512), 7 * 512},
	}

	for _, tc := range tests {
		if tc.got != tc.want {
			t.Fatalf("%s: blockBytes = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

func TestFloorEvictionOrder(t *testing.T) {
	tests := []struct {
		name         string
		bulk         int
		free         []uint64
		trigger      model.Record
		wantDropped  bool
		wantLosses   []model.Loss
		wantCritical []model.Record
		wantBulk     []model.Record
	}{
		{
			name: "older bulk segments go first", bulk: 3, free: []uint64{1, 50}, trigger: bulk(3),
			wantLosses:   []model.Loss{loss(model.Bulk, bulk(0), bulk(1), 2)},
			wantCritical: critRange(0, 3), wantBulk: bulkRange(2, 3),
		},
		{
			name: "the bulk active segment goes before older critical segments", bulk: 1, free: []uint64{1, 50}, trigger: bulk(1),
			wantLosses:   []model.Loss{loss(model.Bulk, bulk(0), bulk(0), 1)},
			wantCritical: critRange(0, 3), wantBulk: bulkRange(1, 1),
		},
		{
			name: "older critical segments go once bulk is empty, then records drop", bulk: 1, free: []uint64{1}, trigger: bulk(1),
			wantDropped: true,
			wantLosses: []model.Loss{
				loss(model.Bulk, bulk(0), bulk(0), 1),
				loss(model.Critical, crit(0), crit(1), 2),
				loss(model.Bulk, bulk(1), bulk(1), 1),
			},
			wantCritical: critRange(2, 3),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := clock.NewFake(epoch)
			disk := newFakeDisk(100)
			opts := testOptions(t, clk)
			opts.DiskUsage = disk.usage
			b := openBuffer(t, t.TempDir(), opts, hooks{})
			mustAppend(t, b, critRange(0, 3)...)
			mustAppend(t, b, bulkRange(0, tc.bulk-1)...)

			disk.reset(nil, tc.free...)
			clk.Advance(time.Second)
			if err := b.Append(tc.trigger); tc.wantDropped != errors.Is(err, ErrDropped) || (!tc.wantDropped && err != nil) {
				t.Fatalf("Append() = %v, wantDropped %v", err, tc.wantDropped)
			}

			assertLosses(t, b, tc.wantLosses)
			assertRecords(t, b, model.Critical, tc.wantCritical)
			assertRecords(t, b, model.Bulk, tc.wantBulk)
		})
	}
}

func TestPinnedSegmentSizeIsValidated(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"one GiB is accepted", `{"version":1,"segment_size":1073741824}`, false},
		{"above one GiB", `{"version":1,"segment_size":1073741825}`, true},
		{"absurd size", `{"version":1,"segment_size":9223372036854775807}`, true},
		{"not larger than a header", `{"version":1,"segment_size":8}`, true},
		{"unknown version", `{"version":2,"segment_size":4096}`, true},
		{"not JSON", `16777216`, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "bulk"), 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(dir, "bulk", "segment_size"), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			b, err := open(dir, testOptions(t, clock.NewFake(epoch)), hooks{})
			if (err != nil) != tc.wantErr {
				t.Fatalf("open() error = %v, wantErr %v", err, tc.wantErr)
			}

			if err == nil {
				mustClose(t, b)
			}
		})
	}
}
