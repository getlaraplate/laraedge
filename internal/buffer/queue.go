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

	"github.com/swolley/laraplate-edge/internal/atomicfile"
	"github.com/swolley/laraplate-edge/internal/model"
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

	oldest      time.Time // device ts of the record at the head, when oldestValid
	oldestValid bool
}

// openQueue recovers the queue in dir. Every segment is scanned; the
// incomplete tail of the last one is cut, and a bad record inside an earlier
// one drops the rest of that segment and is counted as corrupt. A new active
// segment is then created beyond any position handed out before the restart,
// so positions are never reused even when unsynced records were lost.
func openQueue(dir string, p model.Priority, ack Position, defaultSegmentSize int64, syncFile func(*os.File) error, log *slog.Logger) (*queue, uint64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, fmt.Errorf("create %s queue directory: %w", p, err)
	}

	segmentSize, err := pinSegmentSize(dir, defaultSegmentSize)
	if err != nil {
		return nil, 0, fmt.Errorf("%s queue: %w", p, err)
	}

	bases, err := listSegments(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("list %s segments: %w", p, err)
	}

	q := &queue{priority: p, dir: dir, segmentSize: segmentSize, syncFile: syncFile, log: log, ack: ack}

	var corrupt uint64
	for i, base := range bases {
		seg, bad, err := q.recover(base, i == len(bases)-1)
		if err != nil {
			return nil, 0, err
		}

		if bad {
			corrupt++
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
		return nil, 0, fmt.Errorf("%s queue: %w", p, err)
	}

	q.segments = append(q.segments, active)
	q.deleteWhere(func(s *segment) bool { return s.end() <= q.ack || s.size == 0 })

	return q, corrupt, nil
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

// walk calls fn for each frame from pos until fn returns false or the queue
// ends, crossing segments and the gaps between them. It returns the position
// after the last frame passed to fn.
func (q *queue) walk(pos Position, fn func(pos Position, frame []byte) bool) (Position, error) {
	for _, seg := range q.segments {
		if seg.end() <= pos {
			continue
		}

		pos = max(pos, seg.base)
		r, done, err := q.reader(seg)
		if err != nil {
			return pos, err
		}

		for pos < seg.end() {
			frame, err := readFrameAt(r, int64(pos-seg.base), seg.size, q.segmentSize)
			if err != nil {
				done()

				return pos, fmt.Errorf("position %d: %w", uint64(pos), err)
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

// read returns up to limit entries starting at from, or at the head when
// from precedes it.
func (q *queue) read(from Position, limit int) ([]Entry, error) {
	var out []Entry
	var decodeErr error

	_, err := q.walk(max(from, q.head()), func(pos Position, frame []byte) bool {
		r, err := decodePayload(frame[headerSize:])
		if err != nil {
			decodeErr = fmt.Errorf("position %d: %w", uint64(pos), err)

			return false
		}

		out = append(out, Entry{Pos: pos, Next: pos + Position(len(frame)), Record: r})

		return len(out) < limit
	})

	if err := errors.Join(err, decodeErr); err != nil {
		return nil, err
	}

	return out, nil
}

// count returns the number of records in [from, to); to must be a record
// boundary.
func (q *queue) count(from, to Position) (uint64, error) {
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
	lost     int    // records neither confirmed nor sealed
	from, to time.Time
}

// summarize scans seg for the records an eviction would lose.
func (q *queue) summarize(seg *segment) (eviction, error) {
	var ev eviction
	if seg.end() <= q.ack {
		return ev, nil
	}

	lostFrom := max(q.ack, q.sealed)
	r, done, err := q.reader(seg)
	if err != nil {
		return ev, err
	}

	defer done()

	res, err := scanEviction(io.NewSectionReader(r, 0, seg.size), seg.base, q.ack, lostFrom, q.segmentSize)
	if err != nil {
		return ev, fmt.Errorf("scan segment %d: %w", uint64(seg.base), err)
	}

	return res, nil
}

// scanEviction walks a segment sequentially, counting unconfirmed records
// and the ts range of the records lost by deleting it.
func scanEviction(r io.Reader, base, ack, lostFrom Position, maxFrame int64) (eviction, error) {
	var ev eviction
	br := bufio.NewReaderSize(r, 64<<10)
	var header [headerSize]byte
	var payload []byte
	pos := base

	for {
		if _, err := io.ReadFull(br, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return ev, nil
			}

			return ev, err
		}

		length, err := parseHeader(header[:], maxFrame)
		if err != nil {
			return ev, err
		}

		if cap(payload) < length {
			payload = make([]byte, length)
		}

		payload = payload[:length]
		if _, err := io.ReadFull(br, payload); err != nil {
			return ev, err
		}

		if pos >= ack {
			ev.unacked++
		}

		if pos >= lostFrom {
			ev.lost++
			if ts := payloadTS(payload); !ts.IsZero() {
				if ev.from.IsZero() || ts.Before(ev.from) {
					ev.from = ts
				}

				if ts.After(ev.to) {
					ev.to = ts
				}
			}
		}

		pos += Position(headerSize + length)
	}
}

// evictOldest deletes the oldest segment, rotating the active one first when
// it is the only one. record receives the loss marker, if any, before the
// segment is deleted.
func (q *queue) evictOldest(now time.Time, record func(model.Loss)) error {
	if len(q.segments) == 1 {
		if err := q.rotate(); err != nil {
			return fmt.Errorf("rotate before eviction: %w", err)
		}
	}

	seg := q.segments[0]
	ev, err := q.summarize(seg)
	if err != nil {
		return err
	}

	if ev.lost > 0 {
		if ev.from.IsZero() {
			ev.from, ev.to = now, now
		}

		record(model.Loss{Priority: q.priority, FromTS: ev.from, ToTS: ev.to, Samples: ev.lost, Reason: model.LossBufferFull})
	}

	q.log.Warn("buffer: evicted a segment", "queue", q.priority.String(), "segment", uint64(seg.base), "bytes", seg.size, "lost_records", ev.lost)
	q.unacked -= ev.unacked
	q.oldestValid = false
	q.deleteWhere(func(s *segment) bool { return s == seg })

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
