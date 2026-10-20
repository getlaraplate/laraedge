// Package buffer is the durable disk buffer of one output: two append-only
// segmented logs, one per priority class, the confirmed position of each and
// the pending loss markers. Nothing confirmed is lost, and every sample the
// buffer drops leaves a loss marker (spec 6.1, 6.2, 6.4); a device event is
// not a sample, and a lost death is logged with its device.
//
// Layout of the output directory:
//
//	critical/, bulk/  segment files named by their first position (%020d.seg)
//	                  and segment_size, the size pinned when the queue was created
//	acked             the confirmed position of each queue (atomic write)
//	losses            the pending loss markers (atomic write)
package buffer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/getlaraplate/laraplate-edge/internal/atomicfile"
	"github.com/getlaraplate/laraplate-edge/internal/clock"
	"github.com/getlaraplate/laraplate-edge/internal/model"
)

// Defaults applied to zero Options fields.
const (
	DefaultMaxSize       int64         = 1 << 30
	DefaultMinFreeRatio  float64       = 0.05
	DefaultFsyncInterval time.Duration = 200 * time.Millisecond
	DefaultSegmentSize   int64         = 16 << 20
)

// MaxSegmentSize bounds Options.SegmentSize and a pinned queue segment size.
const MaxSegmentSize int64 = 1 << 30

const (
	diskCheckInterval   = time.Second
	ackedFileName       = "acked"
	lossesFileName      = "losses"
	segmentSizeFileName = "segment_size"
	tmpSuffix           = atomicfile.TmpSuffix
	stateVersion        = 1
)

var (
	// ErrClosed is returned by every method called after Close.
	ErrClosed = errors.New("buffer: closed")
	// ErrDropped means a record could not be written even after an eviction;
	// it was dropped and a buffer_full loss marker records it.
	ErrDropped = errors.New("buffer: record dropped")
	// ErrTooLarge means a single record exceeds the segment size or MaxSize;
	// it was dropped and a buffer_full loss marker records it.
	ErrTooLarge = errors.New("buffer: record too large")
	// ErrNotDurable means a change to the loss markers is applied in memory
	// but not yet written to disk; it is written again at every sync tick.
	ErrNotDurable = errors.New("buffer: loss markers not yet durable")

	// errFloorBreached means the free disk floor is breached and only the
	// active segments are left: the incoming record is dropped (ruling R37).
	errFloorBreached = errors.New("free disk floor breached with only active segments left")
)

// Options configures a Buffer. Zero fields take the defaults.
type Options struct {
	// MaxSize bounds the bytes of both queues on disk (default 1 GiB).
	MaxSize int64
	// MinFreeRatio is the share of the disk kept free (default 0.05).
	MinFreeRatio float64
	// FsyncInterval is the group sync period of written records (default 200 ms).
	FsyncInterval time.Duration
	// SegmentSize is the size of one segment file (default 16 MiB). It applies
	// to queues created by this Open; an existing queue keeps its own.
	SegmentSize int64
	// Clock drives the sync timer and the disk check rate (default real).
	Clock clock.Clock
	// DiskUsage reports the free and total bytes of the disk holding dir.
	// Nil means the operating system's figures.
	DiskUsage func(dir string) (free, total uint64, err error)
	// Logger receives recovery, eviction and I/O reports (default slog.Default()).
	// Sample values are never logged.
	Logger *slog.Logger
}

// Position is a byte offset in one queue. Positions grow across segments
// and are never reused, also after segments are deleted or a crash.
type Position uint64

// Entry is one record read from a queue, with its position and the position
// just after it.
type Entry struct {
	Pos    Position
	Next   Position
	Record model.Record
}

// QueueStats describes one queue.
type QueueStats struct {
	// Records counts the records not yet confirmed.
	Records uint64
	// Bytes is the size of the queue on disk.
	Bytes int64
	// OldestUnacked is the device ts of the oldest unconfirmed record, zero
	// when there is none.
	OldestUnacked time.Time
}

// Stats describes a Buffer.
type Stats struct {
	Critical QueueStats
	Bulk     QueueStats
	// Records and Bytes are the totals of both queues.
	Records uint64
	Bytes   int64
	// Dropped counts the samples of every loss marker added since Open, by reason.
	Dropped map[string]uint64
	// CorruptRecords counts the corrupt records found since Open: inside a
	// non-final segment at Open, or anywhere while the buffer runs. Each one
	// dropped the rest of its segment without a loss marker (ruling R20).
	CorruptRecords uint64
}

type writeFunc func(f *os.File, p []byte, off int64) (int, error)

// hooks are seams for in-package tests.
type hooks struct {
	write       writeFunc                                   // segment writes; nil means WriteAt
	sync        func(f *os.File) error                      // segment fsyncs; nil means f.Sync
	writeLosses func(dir string, losses []model.Loss) error // losses file writes; nil means writeLosses
	armed       func()                                      // the sync loop armed its timer
	beforeSync  func()                                      // a sync pass took its files and released the lock
	synced      func(files int)                             // the sync loop finished a pass
}

// Buffer is the disk buffer of one output. Append may run in one goroutine
// while Read, Ack and the loss methods run in another.
type Buffer struct {
	dir   string
	opts  Options
	log   *slog.Logger
	hooks hooks

	mu          sync.Mutex
	closed      bool
	critical    *queue
	bulk        *queue
	losses      []model.Loss
	lossesDirty bool
	frozen      int // leading markers handed out by PendingLosses: never changed again
	dropped     map[string]uint64
	failures    *failureLog

	diskChecked   bool
	diskCheckedAt time.Time
	diskLow       bool
	diskErrLogged bool

	changed chan struct{}
	stop    chan struct{}
	done    chan struct{}
}

// Open opens or creates the buffer in dir (data_dir/outputs/{id}), recovers
// both queues and starts the group sync.
func Open(dir string, opts Options) (*Buffer, error) {
	return open(dir, opts, hooks{})
}

func open(dir string, opts Options, h hooks) (*Buffer, error) {
	opts, err := withDefaults(opts)
	if err != nil {
		return nil, err
	}

	if h.write == nil {
		h.write = func(f *os.File, p []byte, off int64) (int, error) { return f.WriteAt(p, off) }
	}

	if h.sync == nil {
		h.sync = func(f *os.File) error { return f.Sync() }
	}

	if h.writeLosses == nil {
		h.writeLosses = writeLosses
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("buffer: create %s: %w", dir, err)
	}

	for _, name := range []string{ackedFileName, lossesFileName} {
		if err := os.Remove(filepath.Join(dir, name+tmpSuffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("buffer: remove a leftover temporary file: %w", err)
		}
	}

	acked, err := readAcked(dir)
	if err != nil {
		return nil, err
	}

	losses, err := readLosses(dir)
	if err != nil {
		return nil, err
	}

	if err := createStateFiles(dir, losses); err != nil {
		return nil, err
	}

	b := &Buffer{
		dir:      dir,
		opts:     opts,
		log:      opts.Logger,
		hooks:    h,
		losses:   losses,
		dropped:  map[string]uint64{},
		failures: newFailureLog(opts.Logger),
		changed:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	critical, err := openQueue(filepath.Join(dir, model.Critical.String()), model.Critical, acked.Critical, opts.SegmentSize, h.sync, b.log)
	if err != nil {
		return nil, fmt.Errorf("buffer: %w", err)
	}

	bulk, err := openQueue(filepath.Join(dir, model.Bulk.String()), model.Bulk, acked.Bulk, opts.SegmentSize, h.sync, b.log)
	if err != nil {
		_ = critical.close()

		return nil, fmt.Errorf("buffer: %w", err)
	}

	b.critical, b.bulk = critical, bulk

	go b.syncLoop()

	return b, nil
}

func withDefaults(o Options) (Options, error) {
	switch {
	case o.MaxSize < 0:
		return o, fmt.Errorf("buffer: negative max size %d", o.MaxSize)
	case o.SegmentSize < 0 || o.SegmentSize > MaxSegmentSize:
		return o, fmt.Errorf("buffer: segment size %d outside [0, %d]", o.SegmentSize, MaxSegmentSize)
	case o.FsyncInterval < 0:
		return o, fmt.Errorf("buffer: negative fsync interval %v", o.FsyncInterval)
	case o.MinFreeRatio < 0 || o.MinFreeRatio >= 1:
		return o, fmt.Errorf("buffer: minimum free ratio %v outside [0, 1)", o.MinFreeRatio)
	}

	if o.MaxSize == 0 {
		o.MaxSize = DefaultMaxSize
	}

	if o.SegmentSize == 0 {
		o.SegmentSize = DefaultSegmentSize
	}

	if o.FsyncInterval == 0 {
		o.FsyncInterval = DefaultFsyncInterval
	}

	if o.MinFreeRatio == 0 {
		o.MinFreeRatio = DefaultMinFreeRatio
	}

	if o.Clock == nil {
		o.Clock = clock.Real()
	}

	if o.DiskUsage == nil {
		o.DiskUsage = diskUsage
	}

	if o.Logger == nil {
		o.Logger = slog.Default()
	}

	return o, nil
}

func (b *Buffer) queue(p model.Priority) (*queue, error) {
	switch p {
	case model.Critical:
		return b.critical, nil
	case model.Bulk:
		return b.bulk, nil
	default:
		return nil, fmt.Errorf("buffer: invalid priority %d", uint8(p))
	}
}

// Append writes r to the queue of its priority. When the buffer is over
// MaxSize, the oldest bulk segments are evicted first, critical ones only
// when bulk is empty; each eviction leaves a buffer_full loss marker. When
// the disk is under its free floor only whole older segments are evicted;
// with nothing but the active segments left the record is dropped and
// counted in the newest pending buffer_full marker of its class (ruling R37).
// A failed write is cut back, followed by one eviction and one retry; if
// that fails too the record is dropped with its own marker. Every drop
// returns an error wrapping ErrDropped (ErrTooLarge for an oversized record).
// The record is readable at once and durable after the next group sync.
func (b *Buffer) Append(r model.Record) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("buffer: append: %w", err)
	}

	frame, err := encodeRecord(r)
	if err != nil {
		return fmt.Errorf("buffer: append: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrClosed
	}

	q, err := b.queue(r.Priority())
	if err != nil {
		return err
	}

	size := int64(len(frame))
	if size > q.segmentSize || size > b.opts.MaxSize {
		b.dropRecord(q.priority, r, "too_large", "record larger than a segment or the buffer", nil)

		return fmt.Errorf("buffer: append: frame of %d bytes above the segment size %d or the buffer size %d: %w", size, q.segmentSize, b.opts.MaxSize, ErrTooLarge)
	}

	err = b.store(q, frame)
	if err != nil && !errors.Is(err, errFloorBreached) {
		b.failures.report(b.opts.Clock.Now(), q.priority.String()+".write_retry", slog.LevelWarn, "buffer: write failed, evicting and retrying once", "queue", q.priority.String(), "error", err)
		if _, evictErr := b.evictOne(); evictErr != nil {
			b.failures.report(b.opts.Clock.Now(), q.priority.String()+".evict_failed", slog.LevelError, "buffer: eviction after a failed write failed", "queue", q.priority.String(), "error", evictErr)
		}

		if err = b.store(q, frame); err != nil && !errors.Is(err, errFloorBreached) {
			b.dropRecord(q.priority, r, "write_failed", "write failed after an eviction and a retry", err)

			return fmt.Errorf("buffer: append to the %s queue: %w", q.priority, errors.Join(ErrDropped, err))
		}
	}

	if errors.Is(err, errFloorBreached) {
		b.dropRecord(q.priority, r, "floor_breached", "free disk floor breached by other data", nil)

		return fmt.Errorf("buffer: append to the %s queue: %w", q.priority, errors.Join(ErrDropped, err))
	}

	select {
	case b.changed <- struct{}{}:
	default:
	}

	return nil
}

// markerTS returns the ts a loss marker of r carries.
func (b *Buffer) markerTS(r model.Record) time.Time {
	if ts := recordTS(r); !ts.IsZero() {
		return ts
	}

	return b.opts.Clock.Now()
}

// dropRecord records the drop of r. The log goes through the failure log
// (kind is "<queue>.<kind>"): the first drop of a kind in full, then counted
// in a summary at most once a minute. A device event is not a sample: it
// leaves no marker, and a lost death is logged with its device. A sample is
// added to the newest pending buffer_full marker of the same class when that
// marker is the last one and was never handed out, so a lasting condition
// (a breached floor, a disk that refuses every write) leaves one growing
// marker instead of one per record; that change is written at the next sync
// tick, the declared durability limit of records themselves. Otherwise a new
// marker is written at once.
func (b *Buffer) dropRecord(p model.Priority, r model.Record, kind, why string, cause error) {
	now := b.opts.Clock.Now()
	level := slog.LevelError
	if kind == "floor_breached" {
		level = slog.LevelWarn
	}

	attrs := []any{"queue", p.String(), "reason", why, "device", r.Device()}
	if cause != nil {
		attrs = append(attrs, "error", cause)
	}

	b.failures.report(now, p.String()+"."+kind, level, "buffer: record dropped", attrs...)

	if r.Event != nil {
		if r.Event.Kind == model.Death {
			b.log.Warn("buffer: a device death was dropped; the backend will not see it", "queue", p.String(), "device", r.Event.Device)
		}

		return
	}

	ts := b.markerTS(r)
	if n := len(b.losses); n > b.frozen {
		last := &b.losses[n-1]
		if last.Priority == p && last.Reason == model.LossBufferFull {
			last.Samples++
			if ts.Before(last.FromTS) {
				last.FromTS = ts
			}

			if ts.After(last.ToTS) {
				last.ToTS = ts
			}

			b.dropped[model.LossBufferFull]++
			b.lossesDirty = true

			return
		}
	}

	if err := b.addLossLocked(model.Loss{Priority: p, FromTS: ts, ToTS: ts, Samples: 1, Reason: model.LossBufferFull}); err != nil {
		b.failures.report(now, "losses_not_written", slog.LevelError, "buffer: cannot write the loss markers, retrying at the next sync", "reason", why, "error", err)
	}
}

// store rotates the active segment when the frame does not fit, makes room
// and writes the frame.
func (b *Buffer) store(q *queue, frame []byte) error {
	size := int64(len(frame))
	forceDiskCheck := false
	if q.needsRotation(size) {
		if err := q.rotate(); err != nil {
			return err
		}

		forceDiskCheck = true
	}

	if err := b.makeRoom(size, forceDiskCheck); err != nil {
		return err
	}

	return q.write(frame, b.hooks.write)
}

// makeRoom evicts segments until a frame of size fits MaxSize and the disk
// is above its free floor. Over MaxSize any segment may go, active ones
// rotated first. Under the floor alone the order is evictOlder's: the breach
// comes from other data on the disk, and rotating the critical active
// segment on every record would churn files without freeing space; with
// nothing of that left it returns errFloorBreached.
func (b *Buffer) makeRoom(size int64, forceDiskCheck bool) error {
	for {
		over := b.critical.bytes()+b.bulk.bytes()+size > b.opts.MaxSize
		low := b.diskIsLow(forceDiskCheck)
		if !over && !low {
			return nil
		}

		var evicted bool
		var err error
		if over {
			evicted, err = b.evictOne()
		} else {
			evicted, err = b.evictOlder()
		}

		if err != nil {
			return err
		}

		if !evicted {
			if over {
				// Both queues are empty: the frame fits MaxSize (checked by Append).
				return nil
			}

			return errFloorBreached
		}

		forceDiskCheck = low
	}
}

// evictOne evicts the oldest bulk segment, or the oldest critical one when
// bulk holds nothing, rotating an active segment that is the only one. It
// reports false when both queues are empty.
func (b *Buffer) evictOne() (bool, error) {
	q := b.bulk
	if q.bytes() == 0 {
		q = b.critical
	}

	if q.bytes() == 0 {
		return false, nil
	}

	return b.evict(q)
}

// evictOlder is the eviction order under a floor-only breach: older bulk
// segments, then the bulk active segment when it holds data (rotated and
// evicted once: dropped records are not written, so it stays empty), then
// older critical segments. Critical active segments are never rotated here.
// It reports false when nothing of that is left.
func (b *Buffer) evictOlder() (bool, error) {
	if b.bulk.bytes() > 0 {
		return b.evict(b.bulk)
	}

	if len(b.critical.segments) > 1 {
		return b.evict(b.critical)
	}

	return false, nil
}

// evict deletes the oldest segment of q. Its marker is written before the
// deletion when the disk allows it, and again right after, once space is
// freed. Nothing is evicted while an earlier marker is not durable, so at
// most one marker is ever at risk.
func (b *Buffer) evict(q *queue) (bool, error) {
	if b.lossesDirty {
		if err := b.persistLossesLocked(); err != nil {
			return false, fmt.Errorf("evict from the %s queue: loss markers not durable: %w", q.priority, err)
		}
	}

	err := q.evictOldest(b.opts.Clock.Now(), func(l model.Loss) {
		b.queueLossLocked(l)
		_ = b.persistLossesLocked()
	})
	if err != nil {
		return false, fmt.Errorf("evict from the %s queue: %w", q.priority, err)
	}

	if b.lossesDirty {
		if err := b.persistLossesLocked(); err != nil {
			b.log.Error("buffer: cannot write the loss marker of an evicted segment, retrying at the next sync", "queue", q.priority.String(), "error", err)

			return true, fmt.Errorf("evict from the %s queue: loss marker not durable: %w", q.priority, err)
		}
	}

	return true, nil
}

// diskIsLow reports whether free disk space is under the floor. The figures
// are read at most once per diskCheckInterval unless force is set. When they
// cannot be read the floor is not enforced: data is never dropped for a
// measurement failure.
func (b *Buffer) diskIsLow(force bool) bool {
	now := b.opts.Clock.Now()
	if !force && b.diskChecked && now.Sub(b.diskCheckedAt) < diskCheckInterval {
		return b.diskLow
	}

	b.diskChecked, b.diskCheckedAt = true, now

	free, total, err := b.opts.DiskUsage(b.dir)
	if err != nil || total == 0 {
		if !b.diskErrLogged {
			b.log.Warn("buffer: cannot read free disk space, the floor is not enforced", "dir", b.dir, "error", err)
			b.diskErrLogged = true
		}

		b.diskLow = false

		return false
	}

	b.diskErrLogged = false
	b.diskLow = float64(free) < b.opts.MinFreeRatio*float64(total)

	return b.diskLow
}

// Read returns up to limit records of queue p starting at from, or at the
// queue head when from precedes it (confirmed or evicted records). Records
// appended but not yet synced are included.
func (b *Buffer) Read(p model.Priority, from Position, limit int) ([]Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, ErrClosed
	}

	q, err := b.queue(p)
	if err != nil {
		return nil, err
	}

	if limit <= 0 {
		return nil, nil
	}

	entries, err := q.read(from, limit)
	if err != nil {
		return nil, fmt.Errorf("buffer: read the %s queue from %d: %w", p, uint64(from), err)
	}

	return entries, nil
}

// Ack confirms every record of queue p before upTo, persists the position
// and deletes the segments wholly before it. upTo at or before the current
// confirmed position or the head is a no-op; beyond the tail, or not at a
// record boundary, it is an error.
func (b *Buffer) Ack(p model.Priority, upTo Position) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrClosed
	}

	q, err := b.queue(p)
	if err != nil {
		return err
	}

	head := q.head()
	if upTo <= q.ack || upTo <= head {
		return nil
	}

	if tail := q.tail(); upTo > tail {
		return fmt.Errorf("buffer: ack the %s queue at %d beyond its tail %d", p, uint64(upTo), uint64(tail))
	}

	n, err := q.count(head, upTo)
	if err != nil {
		return fmt.Errorf("buffer: ack the %s queue: %w", p, err)
	}

	state := ackedState{Version: stateVersion, Critical: b.critical.ack, Bulk: b.bulk.ack}
	if p == model.Critical {
		state.Critical = upTo
	} else {
		state.Bulk = upTo
	}

	if err := writeJSONAtomic(b.dir, ackedFileName, state); err != nil {
		return fmt.Errorf("buffer: ack the %s queue: %w", p, err)
	}

	q.ack = upTo
	q.unacked -= min(n, q.unacked)
	q.oldestValid = false
	q.deleteWhere(func(s *segment) bool { return s.end() <= q.ack })

	return nil
}

// MarkSealed sets (not raises) the sealed mark of queue p: records before
// upTo belong to a sealed batch that carries its own copy, so an eviction
// deletes them without counting them as lost. It is not persisted; the
// batcher sets it again after loading its inflight batch.
func (b *Buffer) MarkSealed(p model.Priority, upTo Position) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrClosed
	}

	q, err := b.queue(p)
	if err != nil {
		return err
	}

	q.sealed = upTo

	return nil
}

// AddLoss records a loss marker (rejected_invalid from outputs; buffer_full
// markers are added by the buffer itself). It is kept until AckLosses drops
// it. When the marker cannot be written to disk it stays pending in memory,
// is written again at every sync tick, and the error wraps ErrNotDurable so
// that the caller can hold back whatever depends on it.
func (b *Buffer) AddLoss(l model.Loss) error {
	if err := l.Validate(); err != nil {
		return fmt.Errorf("buffer: add loss: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrClosed
	}

	if err := b.addLossLocked(l); err != nil {
		return fmt.Errorf("buffer: add loss: %w", errors.Join(ErrNotDurable, err))
	}

	return nil
}

// addLossLocked adds a marker and writes the markers, returning the write error.
func (b *Buffer) addLossLocked(l model.Loss) error {
	b.queueLossLocked(l)

	return b.persistLossesLocked()
}

// queueLossLocked adds a marker in memory only.
func (b *Buffer) queueLossLocked(l model.Loss) {
	b.losses = append(b.losses, l)
	b.dropped[l.Reason] += uint64(l.Samples)
	b.lossesDirty = true
}

// persistLossesLocked writes the pending markers; on failure they stay dirty.
func (b *Buffer) persistLossesLocked() error {
	if err := b.hooks.writeLosses(b.dir, b.losses); err != nil {
		b.lossesDirty = true

		return err
	}

	b.lossesDirty = false

	return nil
}

// PendingLosses returns a copy of the loss markers not yet acknowledged,
// oldest first. The markers returned are never changed afterwards, so a
// batch that carries them carries them exactly.
func (b *Buffer) PendingLosses() []model.Loss {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.frozen = len(b.losses)

	return slices.Clone(b.losses)
}

// AckLosses drops the first n pending markers. The batcher calls it only
// after the sealed batch carrying them is saved. When the change cannot be
// written it stands in memory, is written at the next sync tick, and the
// error wraps ErrNotDurable.
func (b *Buffer) AckLosses(n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrClosed
	}

	if n < 0 || n > len(b.losses) {
		return fmt.Errorf("buffer: acknowledge %d loss markers, %d pending", n, len(b.losses))
	}

	if n == 0 {
		return nil
	}

	b.losses = slices.Clone(b.losses[n:])
	b.frozen = max(0, b.frozen-n)
	if err := b.persistLossesLocked(); err != nil {
		return fmt.Errorf("buffer: acknowledge loss markers: %w", errors.Join(ErrNotDurable, err))
	}

	return nil
}

// SyncLosses writes the pending loss markers now when an earlier write
// failed, instead of waiting for the next sync tick, so that a caller can
// make a marker durable before acknowledging the records it stands for. It
// returns nil when nothing is waiting to be written, and an error wrapping
// ErrNotDurable when the write fails again.
func (b *Buffer) SyncLosses() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrClosed
	}

	if !b.lossesDirty {
		return nil
	}

	if err := b.persistLossesLocked(); err != nil {
		return fmt.Errorf("buffer: sync loss markers: %w", errors.Join(ErrNotDurable, err))
	}

	return nil
}

// Head returns the oldest readable position of queue p: the first record
// neither confirmed nor evicted. It returns 0 for an invalid priority.
func (b *Buffer) Head(p model.Priority) Position {
	b.mu.Lock()
	defer b.mu.Unlock()

	q, err := b.queue(p)
	if err != nil {
		return 0
	}

	return q.head()
}

// Changed returns a channel signalled after every successful Append. It
// holds at most one pending signal.
func (b *Buffer) Changed() <-chan struct{} { return b.changed }

// Records returns the number of records not yet confirmed, both queues
// together. It reads only counters kept in memory, never a frame, so the
// batcher may call it after every append.
func (b *Buffer) Records() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.critical.unacked + b.bulk.unacked
}

// Stats returns the current figures of the buffer. It may read the frame
// at the head of each queue (for OldestUnacked).
func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := Stats{
		Critical:       b.critical.stats(),
		Bulk:           b.bulk.stats(),
		Dropped:        make(map[string]uint64, len(b.dropped)),
		CorruptRecords: b.critical.corrupt + b.bulk.corrupt,
	}

	s.Records = s.Critical.Records + s.Bulk.Records
	s.Bytes = s.Critical.Bytes + s.Bulk.Bytes
	for reason, n := range b.dropped {
		s.Dropped[reason] = n
	}

	return s
}

func (q *queue) stats() QueueStats {
	return QueueStats{Records: q.unacked, Bytes: q.bytes(), OldestUnacked: q.oldestUnacked()}
}

// Close stops the group sync, syncs and closes the files. Later calls return
// nil; other methods return ErrClosed.
func (b *Buffer) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()

		return nil
	}

	b.closed = true
	b.mu.Unlock()

	close(b.stop)
	<-b.done

	b.mu.Lock()
	defer b.mu.Unlock()

	errs := []error{b.critical.close(), b.bulk.close()}
	if b.lossesDirty {
		if err := b.persistLossesLocked(); err != nil {
			errs = append(errs, fmt.Errorf("write loss markers: %w", err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("buffer: close: %w", err)
	}

	return nil
}

// syncLoop syncs the written active segments every FsyncInterval until Close.
func (b *Buffer) syncLoop() {
	defer close(b.done)

	for {
		timer := b.opts.Clock.NewTimer(b.opts.FsyncInterval)
		if b.hooks.armed != nil {
			b.hooks.armed()
		}

		select {
		case <-b.stop:
			timer.Stop()

			return
		case <-timer.C():
			b.syncPass()
		}
	}
}

// syncPass syncs dirty active segments outside the lock, so appends are not
// held up by a slow disk, and retries unwritten loss markers.
func (b *Buffer) syncPass() {
	type target struct {
		q    *queue
		file *os.File
	}

	b.mu.Lock()
	var targets []target
	for _, q := range []*queue{b.critical, b.bulk} {
		if seg := q.active(); seg.dirty && seg.file != nil {
			targets = append(targets, target{q: q, file: seg.file})
			seg.dirty = false
		}
	}

	if b.lossesDirty {
		if err := b.persistLossesLocked(); err != nil {
			b.failures.report(b.opts.Clock.Now(), "losses_not_written", slog.LevelError, "buffer: cannot write the loss markers, retrying at the next sync", "pending", len(b.losses), "error", err)
		}
	}

	b.failures.flush(b.opts.Clock.Now())
	b.mu.Unlock()

	if b.hooks.beforeSync != nil {
		b.hooks.beforeSync()
	}

	synced := 0
	for _, t := range targets {
		if err := b.hooks.sync(t.file); err != nil {
			// A segment rotated meanwhile was synced by the rotation itself,
			// which syncs unconditionally because this pass already took its
			// dirty flag; one evicted meanwhile is gone.
			if errors.Is(err, os.ErrClosed) {
				continue
			}

			b.log.Error("buffer: group sync failed", "queue", t.q.priority.String(), "error", err)
			b.mu.Lock()
			if seg := t.q.active(); seg.file == t.file {
				seg.dirty = true
			}
			b.mu.Unlock()

			continue
		}

		synced++
	}

	if b.hooks.synced != nil {
		b.hooks.synced(synced)
	}
}

// createStateFiles writes the acked and losses files when they do not exist
// yet, so the layout is complete from the first start.
func createStateFiles(dir string, losses []model.Loss) error {
	if _, err := os.Stat(filepath.Join(dir, ackedFileName)); errors.Is(err, fs.ErrNotExist) {
		if err := writeJSONAtomic(dir, ackedFileName, ackedState{Version: stateVersion}); err != nil {
			return fmt.Errorf("buffer: %w", err)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, lossesFileName)); errors.Is(err, fs.ErrNotExist) {
		if err := writeLosses(dir, losses); err != nil {
			return fmt.Errorf("buffer: %w", err)
		}
	}

	return nil
}

// ackedState is the content of the acked file.
type ackedState struct {
	Version  int      `json:"version"`
	Critical Position `json:"critical"`
	Bulk     Position `json:"bulk"`
}

func readAcked(dir string) (ackedState, error) {
	var state ackedState
	data, err := os.ReadFile(filepath.Join(dir, ackedFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}

	if err != nil {
		return state, fmt.Errorf("buffer: read acked: %w", err)
	}

	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("buffer: parse acked: %w", err)
	}

	if state.Version != stateVersion {
		return state, fmt.Errorf("buffer: acked has version %d, want %d", state.Version, stateVersion)
	}

	return state, nil
}

// lossesState is the content of the losses file.
type lossesState struct {
	Version int          `json:"version"`
	Losses  []lossRecord `json:"losses"`
}

type lossRecord struct {
	Priority string    `json:"priority"`
	FromTS   time.Time `json:"from_ts"`
	ToTS     time.Time `json:"to_ts"`
	Samples  int       `json:"samples"`
	Reason   string    `json:"reason"`
}

func writeLosses(dir string, losses []model.Loss) error {
	state := lossesState{Version: stateVersion, Losses: make([]lossRecord, 0, len(losses))}
	for _, l := range losses {
		state.Losses = append(state.Losses, lossRecord{Priority: l.Priority.String(), FromTS: l.FromTS, ToTS: l.ToTS, Samples: l.Samples, Reason: l.Reason})
	}

	return writeJSONAtomic(dir, lossesFileName, state)
}

func readLosses(dir string) ([]model.Loss, error) {
	data, err := os.ReadFile(filepath.Join(dir, lossesFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("buffer: read losses: %w", err)
	}

	var state lossesState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("buffer: parse losses: %w", err)
	}

	if state.Version != stateVersion {
		return nil, fmt.Errorf("buffer: losses has version %d, want %d", state.Version, stateVersion)
	}

	var losses []model.Loss
	for i, r := range state.Losses {
		p, err := model.ParsePriority(r.Priority)
		if err != nil {
			return nil, fmt.Errorf("buffer: losses entry %d: %w", i, err)
		}

		l := model.Loss{Priority: p, FromTS: r.FromTS, ToTS: r.ToTS, Samples: r.Samples, Reason: r.Reason}
		if err := l.Validate(); err != nil {
			return nil, fmt.Errorf("buffer: losses entry %d: %w", i, err)
		}

		losses = append(losses, l)
	}

	return losses, nil
}

// writeJSONAtomic replaces dir/name with the JSON of v atomically.
func writeJSONAtomic(dir, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}

	return atomicfile.Write(dir, name, data)
}
