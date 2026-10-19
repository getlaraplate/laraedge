// Package buffer is the durable disk buffer of one output: two append-only
// segmented logs, one per priority class, the confirmed position of each and
// the pending loss markers. Nothing confirmed is lost, and every record the
// buffer drops leaves a loss marker (spec 6.1, 6.2, 6.4).
//
// Layout of the output directory:
//
//	critical/, bulk/  segment files named by their first position (%020d.seg)
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
	"runtime"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/swolley/laraplate-edge/internal/clock"
	"github.com/swolley/laraplate-edge/internal/model"
)

// Defaults applied to zero Options fields.
const (
	DefaultMaxSize       int64         = 1 << 30
	DefaultMinFreeRatio  float64       = 0.05
	DefaultFsyncInterval time.Duration = 200 * time.Millisecond
	DefaultSegmentSize   int64         = 16 << 20
)

const (
	diskCheckInterval = time.Second
	ackedFileName     = "acked"
	lossesFileName    = "losses"
	tmpSuffix         = ".tmp"
	stateVersion      = 1
)

var (
	// ErrClosed is returned by every method called after Close.
	ErrClosed = errors.New("buffer: closed")
	// ErrDropped means a record could not be written even after an eviction;
	// it was dropped and a buffer_full loss marker records it.
	ErrDropped = errors.New("buffer: record dropped")
	// ErrTooLarge means a single record exceeds the segment size or MaxSize.
	ErrTooLarge = errors.New("buffer: record too large")
)

// Options configures a Buffer. Zero fields take the defaults.
type Options struct {
	// MaxSize bounds the bytes of both queues on disk (default 1 GiB).
	MaxSize int64
	// MinFreeRatio is the share of the disk kept free (default 0.05).
	MinFreeRatio float64
	// FsyncInterval is the group sync period of written records (default 200 ms).
	FsyncInterval time.Duration
	// SegmentSize is the size of one segment file (default 16 MiB).
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
	// CorruptRecords counts bad records found inside non-final segments at Open.
	CorruptRecords uint64
}

type writeFunc func(f *os.File, p []byte, off int64) (int, error)

// hooks are seams for in-package tests.
type hooks struct {
	write  writeFunc       // segment writes; nil means WriteAt
	armed  func()          // the sync loop armed its timer
	synced func(files int) // the sync loop finished a pass
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
	dropped     map[string]uint64
	corrupt     uint64

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
		dir:     dir,
		opts:    opts,
		log:     opts.Logger,
		hooks:   h,
		losses:  losses,
		dropped: map[string]uint64{},
		changed: make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	critical, corrupt, err := openQueue(filepath.Join(dir, model.Critical.String()), model.Critical, acked.Critical, opts.SegmentSize, b.log)
	if err != nil {
		return nil, fmt.Errorf("buffer: %w", err)
	}

	bulk, corruptBulk, err := openQueue(filepath.Join(dir, model.Bulk.String()), model.Bulk, acked.Bulk, opts.SegmentSize, b.log)
	if err != nil {
		_ = critical.close()

		return nil, fmt.Errorf("buffer: %w", err)
	}

	b.critical, b.bulk, b.corrupt = critical, bulk, corrupt+corruptBulk

	go b.syncLoop()

	return b, nil
}

func withDefaults(o Options) (Options, error) {
	switch {
	case o.MaxSize < 0:
		return o, fmt.Errorf("buffer: negative max size %d", o.MaxSize)
	case o.SegmentSize < 0:
		return o, fmt.Errorf("buffer: negative segment size %d", o.SegmentSize)
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
// MaxSize or the disk under its free floor, the oldest bulk segments are
// evicted first, critical ones only when bulk is empty; each eviction leaves
// a buffer_full loss marker. A failed write is cut back, followed by one
// eviction and one retry; if that fails too the record is dropped with its
// own marker and the error wraps ErrDropped. The record is readable at once
// and durable after the next group sync.
func (b *Buffer) Append(r model.Record) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("buffer: append: %w", err)
	}

	frame, err := encodeRecord(r)
	if err != nil {
		return fmt.Errorf("buffer: append: %w", err)
	}

	size := int64(len(frame))
	if size > b.opts.SegmentSize || size > b.opts.MaxSize {
		return fmt.Errorf("buffer: append: frame of %d bytes above the segment size %d or the buffer size %d: %w", size, b.opts.SegmentSize, b.opts.MaxSize, ErrTooLarge)
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

	if err := b.store(q, frame); err != nil {
		b.log.Warn("buffer: write failed, evicting and retrying once", "queue", q.priority.String(), "error", err)
		if _, evictErr := b.evictOne(); evictErr != nil {
			b.log.Error("buffer: eviction after a failed write failed", "error", evictErr)
		}

		if retryErr := b.store(q, frame); retryErr != nil {
			ts := recordTS(r)
			if ts.IsZero() {
				ts = b.opts.Clock.Now()
			}

			b.addLossLocked(model.Loss{Priority: q.priority, FromTS: ts, ToTS: ts, Samples: 1, Reason: model.LossBufferFull})
			b.log.Error("buffer: record dropped after a failed retry", "queue", q.priority.String(), "device", r.Device(), "error", retryErr)

			return fmt.Errorf("buffer: append to the %s queue: %w", q.priority, errors.Join(ErrDropped, retryErr))
		}
	}

	select {
	case b.changed <- struct{}{}:
	default:
	}

	return nil
}

// store rotates the active segment when the frame does not fit, makes room
// and writes the frame.
func (b *Buffer) store(q *queue, frame []byte) error {
	size := int64(len(frame))
	forceDiskCheck := false
	if q.needsRotation(size, b.opts.SegmentSize) {
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
// is above its free floor, or nothing is left to evict.
func (b *Buffer) makeRoom(size int64, forceDiskCheck bool) error {
	for {
		over := b.critical.bytes()+b.bulk.bytes()+size > b.opts.MaxSize
		low := b.diskIsLow(forceDiskCheck)
		if !over && !low {
			return nil
		}

		evicted, err := b.evictOne()
		if err != nil {
			return err
		}

		if !evicted {
			// Both queues are empty: the frame fits MaxSize (checked by
			// Append) and a low disk is left to the write itself.
			return nil
		}

		forceDiskCheck = low
	}
}

// evictOne evicts the oldest bulk segment, or the oldest critical one when
// bulk holds nothing. It reports false when both queues are empty.
func (b *Buffer) evictOne() (bool, error) {
	q := b.bulk
	if q.bytes() == 0 {
		q = b.critical
	}

	if q.bytes() == 0 {
		return false, nil
	}

	if err := q.evictOldest(b.opts.Clock.Now(), b.addLossLocked); err != nil {
		return false, fmt.Errorf("evict from the %s queue: %w", q.priority, err)
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
	q.unacked -= n
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
// it. A marker that cannot be written to disk stays pending in memory and
// is written again at every sync tick until it succeeds.
func (b *Buffer) AddLoss(l model.Loss) error {
	if err := l.Validate(); err != nil {
		return fmt.Errorf("buffer: add loss: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return ErrClosed
	}

	b.addLossLocked(l)

	return nil
}

func (b *Buffer) addLossLocked(l model.Loss) {
	b.losses = append(b.losses, l)
	b.dropped[l.Reason] += uint64(l.Samples)
	b.persistLossesLocked()
}

func (b *Buffer) persistLossesLocked() {
	if err := writeLosses(b.dir, b.losses); err != nil {
		b.lossesDirty = true
		b.log.Error("buffer: cannot write the loss markers, retrying at the next sync", "pending", len(b.losses), "error", err)

		return
	}

	b.lossesDirty = false
}

// PendingLosses returns a copy of the loss markers not yet acknowledged,
// oldest first.
func (b *Buffer) PendingLosses() []model.Loss {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.losses)
}

// AckLosses drops the first n pending markers. The batcher calls it only
// after the sealed batch carrying them is saved.
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
	b.persistLossesLocked()

	return nil
}

// Changed returns a channel signalled after every successful Append. It
// holds at most one pending signal.
func (b *Buffer) Changed() <-chan struct{} { return b.changed }

// Stats returns the current figures of the buffer.
func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := Stats{
		Critical:       b.critical.stats(),
		Bulk:           b.bulk.stats(),
		Dropped:        make(map[string]uint64, len(b.dropped)),
		CorruptRecords: b.corrupt,
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
		if err := writeLosses(b.dir, b.losses); err != nil {
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
		b.persistLossesLocked()
	}
	b.mu.Unlock()

	synced := 0
	for _, t := range targets {
		if err := t.file.Sync(); err != nil {
			// A segment rotated meanwhile was synced by the rotation; one
			// evicted meanwhile is gone.
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

// writeJSONAtomic replaces dir/name with the JSON of v: temporary file,
// fsync, rename, fsync of the directory where supported.
func writeJSONAtomic(dir, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}

	tmp := filepath.Join(dir, name+tmpSuffix)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}

	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}

	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("write %s: %w", name, err)
	}

	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("replace %s: %w", name, err)
	}

	if err := syncDir(dir); err != nil {
		return fmt.Errorf("replace %s: %w", name, err)
	}

	return nil
}

// syncDir makes the directory entries of dir durable. Windows has no
// directory sync, and some filesystems refuse it; both count as done.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}

	syncErr := d.Sync()
	closeErr := d.Close()
	if errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, errors.ErrUnsupported) {
		syncErr = nil
	}

	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}

	return nil
}
