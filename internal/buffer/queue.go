package buffer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/getlaraplate/laraedge/internal/atomicfile"
	"github.com/getlaraplate/laraedge/internal/model"
)

// queue is the segmented log of one priority class. The last segment is the
// active one; it always exists, possibly empty, and is the only one written.
type queue struct {
	priority    model.Priority
	dir         string
	segmentSize int64 // pinned per queue directory, also the frame size limit
	syncFile    func(*os.File) error
	log         *slog.Logger

	segments []*segment
	ack      Position // every record before it is confirmed (persisted in acked)
	sealed   Position // records before it belong to a sealed batch (not persisted)
	unacked  uint64   // records at or after the confirmed position
	corrupt  uint64   // corrupt records found inside segments, at Open or later

	oldest      time.Time // device ts of the record at the head, when oldestValid
	oldestValid bool
}

// openQueue recovers the queue in dir. Every segment is scanned; the
// incomplete tail of the last one is cut, and a bad record inside an earlier
// one drops the rest of that segment and is counted as corrupt. A new active
// segment is then created beyond any position handed out before the restart,
// so positions are never reused even when unsynced records were lost.
func openQueue(dir string, p model.Priority, ack Position, defaultSegmentSize int64, syncFile func(*os.File) error, log *slog.Logger) (*queue, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s queue directory: %w", p, err)
	}

	segmentSize, err := pinSegmentSize(dir, defaultSegmentSize)
	if err != nil {
		return nil, fmt.Errorf("%s queue: %w", p, err)
	}

	bases, err := listSegments(dir)
	if err != nil {
		return nil, fmt.Errorf("list %s segments: %w", p, err)
	}

	q := &queue{priority: p, dir: dir, segmentSize: segmentSize, syncFile: syncFile, log: log, ack: ack}

	for i, base := range bases {
		seg, bad, err := q.recover(base, i == len(bases)-1)
		if err != nil {
			return nil, err
		}

		if bad {
			q.corrupt++
		}

		q.segments = append(q.segments, seg)
	}

	// Records handed out before a crash end at most one segment size after
	// the last segment's start: a segment file is durable before it is written.
	next := ack
	if n := len(q.segments); n > 0 {
		last := q.segments[n-1]
		next = max(next, last.end(), last.base+Position(segmentSize))
	}

	active, err := createSegment(dir, next)
	if err != nil {
		return nil, fmt.Errorf("%s queue: %w", p, err)
	}

	q.segments = append(q.segments, active)
	q.deleteWhere(func(s *segment) bool { return s.end() <= q.ack || s.size == 0 })

	return q, nil
}

// recover scans one segment file and truncates it after its last valid frame.
func (q *queue) recover(base Position, last bool) (*segment, bool, error) {
	path := q.path(base)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, false, fmt.Errorf("open segment: %w", err)
	}

	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat segment: %w", err)
	}

	res, err := scanSegment(f, info.Size(), base, q.ack, q.segmentSize)
	if err != nil {
		return nil, false, fmt.Errorf("scan segment %s: %w", path, err)
	}

	bad := false
	if res.size < info.Size() {
		if err := f.Truncate(res.size); err != nil {
			return nil, false, fmt.Errorf("truncate segment %s: %w", path, err)
		}

		if err := f.Sync(); err != nil {
			return nil, false, fmt.Errorf("sync segment %s: %w", path, err)
		}

		attrs := []any{"queue", q.priority.String(), "segment", uint64(base), "offset", uint64(base) + uint64(res.size), "dropped_bytes", info.Size() - res.size}
		if last {
			q.log.Warn("buffer: cut an incomplete record at the end of the queue", attrs...)
		} else {
			bad = true
			q.log.Error("buffer: corrupt record inside a segment, dropped the rest of the segment", attrs...)
		}
	}

	q.unacked += res.records - res.beforeAck

	return &segment{base: base, size: res.size}, bad, nil
}

// segmentSizeState is the content of a queue's segment_size file.
type segmentSizeState struct {
	Version     int   `json:"version"`
	SegmentSize int64 `json:"segment_size"`
}

// pinSegmentSize returns the segment size recorded in a queue directory,
// recording fallback when there is none yet. A queue keeps the size it was
// created with: a smaller one would cut valid frames as corrupt and shorten
// the position skip that keeps positions unique after a crash.
func pinSegmentSize(dir string, fallback int64) (int64, error) {
	if err := os.Remove(filepath.Join(dir, segmentSizeFileName+tmpSuffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("remove a leftover temporary file: %w", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, segmentSizeFileName))
	if errors.Is(err, fs.ErrNotExist) {
		if err := writeJSONAtomic(dir, segmentSizeFileName, segmentSizeState{Version: stateVersion, SegmentSize: fallback}); err != nil {
			return 0, err
		}

		return fallback, nil
	}

	if err != nil {
		return 0, fmt.Errorf("read %s: %w", segmentSizeFileName, err)
	}

	var state segmentSizeState
	if err := json.Unmarshal(data, &state); err != nil {
		return 0, fmt.Errorf("parse %s: %w", segmentSizeFileName, err)
	}

	if state.Version != stateVersion || state.SegmentSize <= headerSize || state.SegmentSize > MaxSegmentSize {
		return 0, fmt.Errorf("%s: version %d, size %d not usable", segmentSizeFileName, state.Version, state.SegmentSize)
	}

	return state.SegmentSize, nil
}

func listSegments(dir string) ([]Position, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var bases []Position
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}

		if base, ok := parseSegmentName(e.Name()); ok {
			bases = append(bases, base)
		}
	}

	slices.Sort(bases)

	return bases, nil
}

func (q *queue) path(base Position) string { return filepath.Join(q.dir, segmentName(base)) }

func (q *queue) active() *segment { return q.segments[len(q.segments)-1] }

// head returns the position of the first unconfirmed record still on disk.
func (q *queue) head() Position { return max(q.ack, q.segments[0].base) }

// tail returns the position the next record will be written at, unless the
// active segment rotates first.
func (q *queue) tail() Position { return q.active().end() }

// bytes returns the size of the queue on disk, confirmed records included.
func (q *queue) bytes() int64 {
	var n int64
	for _, s := range q.segments {
		n += s.size
	}

	return n
}

func (q *queue) needsRotation(frame int64) bool {
	a := q.active()

	return a.size > 0 && a.size+frame > q.segmentSize
}

// rotate closes the active segment, synced, and starts a new one at its end.
// A rotated segment already wholly confirmed is deleted at once.
func (q *queue) rotate() error {
	old := q.active()
	if old.size == 0 {
		return nil
	}

	seg, err := createSegment(q.dir, old.end())
	if err != nil {
		return err
	}

	if err := old.close(q.syncFile); err != nil {
		q.log.Error("buffer: syncing and closing a rotated segment failed", "queue", q.priority.String(), "segment", uint64(old.base), "error", err)
	}

	q.segments = append(q.segments, seg)
	if old.end() <= q.ack {
		q.deleteWhere(func(s *segment) bool { return s == old })
	}

	return nil
}

// write appends a frame to the active segment through write. On failure the
// segment is cut back to its last good offset, so a partial frame never stays.
func (q *queue) write(frame []byte, write writeFunc) error {
	seg := q.active()
	n, err := write(seg.file, frame, seg.size)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}

	if err != nil {
		if terr := seg.file.Truncate(seg.size); terr != nil {
			err = errors.Join(err, fmt.Errorf("cut back: %w", terr))
		}

		return fmt.Errorf("write %s segment %d at offset %d: %w", q.priority, uint64(seg.base), seg.size, err)
	}

	seg.size += int64(len(frame))
	seg.dirty = true
	q.unacked++

	return nil
}

// reader returns a reader for seg: the active file, or a file opened for the
// call and closed by the returned func.
func (q *queue) reader(seg *segment) (io.ReaderAt, func(), error) {
	if seg.file != nil {
		return seg.file, func() {}, nil
	}

	f, err := os.Open(q.path(seg.base))
	if err != nil {
		return nil, nil, fmt.Errorf("open segment: %w", err)
	}

	return f, func() { _ = f.Close() }, nil
}

// corruptFrame is a frame of a queue that cannot be read back: a bad length,
// checksum, format version or payload, a read error, or a segment file that
// is gone. The frame and the rest of its segment are lost (ruling R20).
type corruptFrame struct {
	seg *segment
	pos Position
	err error
}

func (c *corruptFrame) Error() string {
	return fmt.Sprintf("position %d: %v", uint64(c.pos), c.err)
}

func (c *corruptFrame) Unwrap() error { return c.err }

// walk calls fn for each frame from pos until fn returns false or the queue
// ends, crossing segments and the gaps between them. It returns the position
// after the last frame passed to fn. A frame that cannot be read back is
// reported as a *corruptFrame; a start position inside a segment that is no
// record boundary is a plain error, never a corruption.
func (q *queue) walk(pos Position, fn func(pos Position, frame []byte) bool) (Position, error) {
	start := pos
	for _, seg := range q.segments {
		if seg.end() <= pos {
			continue
		}

		pos = max(pos, seg.base)
		r, done, err := q.reader(seg)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return pos, &corruptFrame{seg: seg, pos: seg.base, err: err}
			}

			return pos, err
		}

		for pos < seg.end() {
			frame, err := readFrameAt(r, int64(pos-seg.base), seg.size, q.segmentSize)
			if err != nil {
				defer done()

				if pos == start && pos != seg.base {
					return pos, q.classify(r, seg, pos, err)
				}

				return pos, &corruptFrame{seg: seg, pos: pos, err: err}
			}

			if !fn(pos, frame) {
				done()

				return pos, nil
			}

			pos += Position(len(frame))
		}

		done()
	}

	return pos, nil
}

// classify tells a bad frame at a caller's start position from a start
// position that is no record boundary, walking seg from its base: a frame
// that fails before pos is the corruption, reaching pos exactly makes the
// frame at pos corrupt, and passing over pos is the caller's error.
func (q *queue) classify(r io.ReaderAt, seg *segment, pos Position, cause error) error {
	at := seg.base
	for at < pos {
		frame, err := readFrameAt(r, int64(at-seg.base), seg.size, q.segmentSize)
		if err != nil {
			return &corruptFrame{seg: seg, pos: at, err: err}
		}

		at += Position(len(frame))
	}

	if at == pos {
		return &corruptFrame{seg: seg, pos: pos, err: cause}
	}

	return fmt.Errorf("position %d is not a record boundary: %w", uint64(pos), cause)
}

// segmentAt returns the segment holding pos, or nil.
func (q *queue) segmentAt(pos Position) *segment {
	for _, seg := range q.segments {
		if seg.base <= pos && pos < seg.end() {
			return seg
		}
	}

	return nil
}

// read returns up to limit entries starting at from, or at the head when
// from precedes it. A corrupt frame on the way is cut with the rest of its
// segment (dropCorrupt) and the read starts again, so a frame that rots
// while the buffer runs never stalls the output.
func (q *queue) read(from Position, limit int) ([]Entry, error) {
	for {
		out, err := q.readOnce(from, limit)
		if c, ok := errors.AsType[*corruptFrame](err); ok {
			if cerr := q.dropCorrupt(c); cerr != nil {
				return nil, errors.Join(err, cerr)
			}

			continue
		}

		if err != nil {
			return nil, err
		}

		return out, nil
	}
}

func (q *queue) readOnce(from Position, limit int) ([]Entry, error) {
	var out []Entry
	var decodeErr error

	_, err := q.walk(max(from, q.head()), func(pos Position, frame []byte) bool {
		r, err := decodePayload(frame[headerSize:])
		if err != nil {
			decodeErr = &corruptFrame{seg: q.segmentAt(pos), pos: pos, err: err}

			return false
		}

		out = append(out, Entry{Pos: pos, Next: pos + Position(len(frame)), Record: r})

		return len(out) < limit
	})

	if err != nil {
		return nil, err
	}

	if decodeErr != nil {
		return nil, decodeErr
	}

	return out, nil
}

// count returns the number of records in [from, to); to must be a record
// boundary. A corrupt frame on the way is cut as in read.
func (q *queue) count(from, to Position) (uint64, error) {
	for {
		n, err := q.countOnce(from, to)
		if c, ok := errors.AsType[*corruptFrame](err); ok {
			if cerr := q.dropCorrupt(c); cerr != nil {
				return 0, errors.Join(err, cerr)
			}

			continue
		}

		return n, err
	}
}

func (q *queue) countOnce(from, to Position) (uint64, error) {
	var n uint64
	reached := from
	_, err := q.walk(from, func(pos Position, frame []byte) bool {
		if pos >= to {
			return false
		}

		n++
		reached = pos + Position(len(frame))

		return true
	})
	if err != nil {
		return 0, err
	}

	if reached != to {
		return 0, fmt.Errorf("position %d is not a record boundary", uint64(to))
	}

	return n, nil
}

// dropCorrupt applies ruling R20 while the buffer runs: the corrupt frame
// and everything after it in its segment become unreadable, the segment
// file is cut there, the corruption is counted and logged without values,
// and the unconfirmed count is computed again. No loss marker is written:
// the protocol has no reason for it. Positions are never reused: a cut in
// the active segment first rotates it, so new records go after its old end.
func (q *queue) dropCorrupt(c *corruptFrame) error {
	cut, err := q.cut(c.seg, c.pos, c.err)
	if err != nil {
		return err
	}

	if !cut {
		return fmt.Errorf("the corrupt record at %d of the %s queue is outside its segments", uint64(c.pos), q.priority)
	}

	q.recount()

	return nil
}

// cut drops the frames of seg from pos on, in memory and on disk, and
// reports whether anything was cut. It fails only when the active segment
// cannot be rotated first.
func (q *queue) cut(seg *segment, pos Position, cause error) (bool, error) {
	if seg == nil || !slices.Contains(q.segments, seg) || pos >= seg.end() {
		return false, nil
	}

	if seg == q.active() {
		if err := q.rotate(); err != nil {
			return false, fmt.Errorf("rotate the %s segment %d before cutting a corrupt record: %w", q.priority, uint64(seg.base), err)
		}

		if !slices.Contains(q.segments, seg) {
			// Wholly confirmed: rotate deleted it.
			return true, nil
		}
	}

	pos = max(pos, seg.base)
	dropped := int64(seg.end() - pos)
	seg.size = int64(pos - seg.base)
	q.corrupt++
	q.oldestValid = false

	q.log.Error("buffer: corrupt record inside a segment, dropped the rest of the segment",
		"queue", q.priority.String(), "segment", uint64(seg.base), "offset", uint64(pos), "dropped_bytes", dropped, "error", cause)

	if !errors.Is(cause, fs.ErrNotExist) {
		if err := truncateSegment(q.path(seg.base), seg.size); err != nil {
			q.log.Error("buffer: cannot cut the corrupt segment on disk; it is cut again at the next start", "queue", q.priority.String(), "segment", uint64(seg.base), "error", err)
		}
	}

	q.deleteWhere(func(s *segment) bool { return s == seg && (s.size == 0 || s.end() <= q.ack) })

	return true, nil
}

// truncateSegment cuts a closed segment file to size and syncs it.
func truncateSegment(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}

	defer f.Close()

	if err := f.Truncate(size); err != nil {
		return err
	}

	return f.Sync()
}

// recount scans every segment again after a corruption, cutting any other
// corrupt frame it meets, and sets the unconfirmed count from what is left.
// It reads the whole queue: it runs only when a corruption was found.
func (q *queue) recount() {
	var n uint64
	for _, seg := range slices.Clone(q.segments) {
		if seg.size == 0 {
			continue
		}

		res, openErr, err := q.scan(seg)
		if openErr != nil && !errors.Is(openErr, fs.ErrNotExist) {
			// A failure to open, such as too many open files, is no
			// corruption: the segment stays, its records uncounted.
			q.log.Error("buffer: cannot open a segment to count its records", "queue", q.priority.String(), "segment", uint64(seg.base), "error", openErr)

			continue
		}

		if openErr != nil || err != nil || res.size < seg.size {
			cause := errors.Join(openErr, err)
			if cause == nil {
				cause = errCorruptFrame
			}

			if _, cerr := q.cut(seg, seg.base+Position(res.size), cause); cerr != nil {
				q.log.Error("buffer: cannot cut a corrupt record", "queue", q.priority.String(), "error", cerr)
			}
		}

		n += res.records - res.beforeAck
	}

	q.unacked = n
}

// scan verifies the frames of seg as the recovery at Open does. openErr
// reports a segment that cannot be opened, err a read failure.
func (q *queue) scan(seg *segment) (res scanResult, openErr, err error) {
	r, done, openErr := q.reader(seg)
	if openErr != nil {
		return scanResult{}, openErr, nil
	}

	defer done()

	res, err = scanSegment(io.NewSectionReader(r, 0, seg.size), seg.size, seg.base, q.ack, q.segmentSize)

	return res, nil, err
}

// oldestUnacked returns the device ts of the record at the head, or the zero
// time when the queue holds nothing unconfirmed.
func (q *queue) oldestUnacked() time.Time {
	if q.unacked == 0 {
		return time.Time{}
	}

	if !q.oldestValid {
		entries, err := q.read(q.head(), 1)
		if err != nil || len(entries) == 0 {
			return time.Time{}
		}

		q.oldest, q.oldestValid = recordTS(entries[0].Record), true
	}

	return q.oldest
}

// eviction is what evicting one segment loses.
type eviction struct {
	unacked  uint64 // records not yet confirmed
	lost     int    // samples neither confirmed nor sealed: device events are not samples
	from, to time.Time
	deaths   []string // devices whose death is lost, neither confirmed nor sealed
}

// summarize scans seg for the records an eviction would lose. On a corrupt
// frame it returns what it counted before it with a *corruptFrame error.
func (q *queue) summarize(seg *segment) (eviction, error) {
	var ev eviction
	if seg.end() <= q.ack {
		return ev, nil
	}

	lostFrom := max(q.ack, q.sealed)
	r, done, err := q.reader(seg)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ev, &corruptFrame{seg: seg, pos: seg.base, err: err}
		}

		return ev, err
	}

	defer done()

	ev, pos, err := scanEviction(io.NewSectionReader(r, 0, seg.size), seg.base, q.ack, lostFrom, q.segmentSize)
	if err != nil {
		return ev, &corruptFrame{seg: seg, pos: pos, err: err}
	}

	return ev, nil
}

// scanEviction walks a segment sequentially, counting unconfirmed records
// and the samples, their ts range and the deaths lost by deleting it. On a
// bad frame or a read error it returns what it counted before, the frame's
// position and the error.
func scanEviction(r io.Reader, base, ack, lostFrom Position, maxFrame int64) (eviction, Position, error) {
	var ev eviction
	br := bufio.NewReaderSize(r, 64<<10)
	var header [headerSize]byte
	var payload []byte
	pos := base

	for {
		if _, err := io.ReadFull(br, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return ev, pos, nil
			}

			return ev, pos, err
		}

		length, err := parseHeader(header[:], maxFrame)
		if err != nil {
			return ev, pos, err
		}

		if cap(payload) < length {
			payload = make([]byte, length)
		}

		payload = payload[:length]
		if _, err := io.ReadFull(br, payload); err != nil {
			return ev, pos, err
		}

		if err := verifyPayload(header[:], payload); err != nil {
			return ev, pos, err
		}

		sum, err := summarizePayload(payload)
		if err != nil {
			return ev, pos, err
		}

		if pos >= ack {
			ev.unacked++
		}

		if pos >= lostFrom {
			switch {
			case sum.sample:
				ev.lost++
				if !sum.ts.IsZero() {
					if ev.from.IsZero() || sum.ts.Before(ev.from) {
						ev.from = sum.ts
					}

					if sum.ts.After(ev.to) {
						ev.to = sum.ts
					}
				}
			case sum.death != "" && !slices.Contains(ev.deaths, sum.death):
				ev.deaths = append(ev.deaths, sum.death)
			}
		}

		pos += Position(headerSize + length)
	}
}

// evictOldest deletes the oldest segment, rotating the active one first when
// it is the only one. record receives the loss marker, if any, before the
// segment is deleted. A corrupt frame inside the segment does not stop the
// eviction: the samples before it are in the marker, the rest is counted as
// corrupt (ruling R20), and the unconfirmed count is computed again.
func (q *queue) evictOldest(now time.Time, record func(model.Loss)) error {
	if len(q.segments) == 1 {
		if err := q.rotate(); err != nil {
			return fmt.Errorf("rotate before eviction: %w", err)
		}
	}

	seg := q.segments[0]
	ev, err := q.summarize(seg)
	c, corrupt := errors.AsType[*corruptFrame](err)
	if err != nil && !corrupt {
		return err
	}

	if ev.lost > 0 {
		if ev.from.IsZero() {
			ev.from, ev.to = now, now
		}

		record(model.Loss{Priority: q.priority, FromTS: ev.from, ToTS: ev.to, Samples: ev.lost, Reason: model.LossBufferFull})
	}

	for _, device := range ev.deaths {
		q.log.Warn("buffer: evicted a device death; the backend will not see it", "queue", q.priority.String(), "device", device)
	}

	q.log.Warn("buffer: evicted a segment", "queue", q.priority.String(), "segment", uint64(seg.base), "bytes", seg.size, "lost_samples", ev.lost)
	if corrupt {
		q.corrupt++
		q.log.Error("buffer: corrupt record inside an evicted segment; the records after it are not in the loss marker",
			"queue", q.priority.String(), "segment", uint64(seg.base), "offset", uint64(c.pos), "error", c.err)
	} else {
		q.unacked -= min(ev.unacked, q.unacked)
	}

	q.oldestValid = false
	q.deleteWhere(func(s *segment) bool { return s == seg })
	if corrupt {
		q.recount()
	}

	return nil
}

// deleteWhere deletes every segment but the active one for which match is
// true, then syncs the directory. A file that cannot be deleted is logged
// and forgotten: it comes back at the next start as older data.
func (q *queue) deleteWhere(match func(*segment) bool) {
	active := q.active()
	deleted := false
	q.segments = slices.DeleteFunc(q.segments, func(s *segment) bool {
		if s == active || !match(s) {
			return false
		}

		if err := s.close(nil); err != nil {
			q.log.Error("buffer: closing a segment before deletion failed", "queue", q.priority.String(), "segment", uint64(s.base), "error", err)
		}

		if err := os.Remove(q.path(s.base)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			q.log.Error("buffer: deleting a segment failed", "queue", q.priority.String(), "segment", uint64(s.base), "error", err)
		}

		deleted = true

		return true
	})

	if deleted {
		if err := atomicfile.SyncDir(q.dir); err != nil {
			q.log.Error("buffer: syncing the queue directory failed", "queue", q.priority.String(), "error", err)
		}
	}
}

// close syncs and closes the active segment.
func (q *queue) close() error {
	if err := q.active().close(q.syncFile); err != nil {
		return fmt.Errorf("close %s queue: %w", q.priority, err)
	}

	return nil
}
