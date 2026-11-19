package serial

import (
	"bytes"
	"time"
)

// Kinds counted by the framer (spec section 5).
const (
	kindFrameTooLong    = "frame_too_long"
	kindIncompleteFrame = "incomplete_frame"
)

// staleAfter is how long bytes of an unfinished frame survive without news
// (terminator and length framing).
const staleAfter = 2 * time.Second

// frame is one frame cut by the framer and the time it was read.
type frame struct {
	data []byte
	at   time.Time
}

// framer cuts the byte stream of a port into frames. It holds no clock: the
// only time it knows is the one given to push and tick.
//
// A port may be opened in the middle of a frame, so a new framer, and one
// reset after a port loss, starts syncing: it drops bytes until a sync point,
// which is a quiet read cycle (tick), a start character (with start), a
// terminator (with terminator; the bytes up to and including it are dropped),
// or, with gap, the first silence of gap, or a command written to the port. Dropped bytes other than CR, LF and
// NUL count incomplete_frame once per sync.
type framer struct {
	framing  Framing
	maxFrame int

	// syncing is true until the first sync point.
	syncing bool
	// syncDropped counts the bytes other than CR, LF and NUL dropped while
	// syncing.
	syncDropped int

	// buf holds the pending frame (without the start byte). While skipping in
	// terminator mode it only holds the tail needed to spot the terminator.
	buf []byte
	// inFrame is true once a start byte was seen (HasStart only).
	inFrame bool
	// skipping is true after a too long frame, until the resync point.
	skipping bool
	// noiseRun is the length of the CR, LF and NUL run ending buf.
	noiseRun int
	// head holds the last bytes of leading noise dropped before a frame, to
	// spot a terminator made of noise bytes.
	head []byte

	last    time.Time
	hasLast bool
}

func newFramer(f Framing, maxFrame int) *framer {
	return &framer{framing: f, maxFrame: maxFrame, syncing: true}
}

// reset drops what is pending, without counting it (port lost), and starts
// syncing again.
func (f *framer) reset() {
	f.clear()
	f.hasLast = false
	f.syncing, f.syncDropped = true, 0
}

// push takes the bytes read at at and returns the frames they complete and the
// kinds to count.
func (f *framer) push(b []byte, at time.Time) ([]frame, []string) {
	var frames []frame
	var kinds []string
	if len(b) == 0 {
		return nil, nil
	}
	frames, kinds = f.expire(at)
	for _, c := range b {
		if f.syncing {
			var dropped bool
			if dropped, kinds = f.sync(c, kinds); dropped {
				continue
			}
		}
		frames, kinds = f.feed(c, at, frames, kinds)
	}
	f.last, f.hasLast = at, true
	return frames, kinds
}

// tick is called for a read cycle that brought no byte: the line was quiet for
// one read timeout. It ends a gap frame after the silence, drops stale bytes,
// and is a sync point (with gap, once the silence lasts gap: it counts from
// the last byte, or from one read timeout before the first tick when no byte
// came yet), and so is a command written (commandSent).
func (f *framer) tick(at time.Time) ([]frame, []string) {
	if f.syncing {
		if f.framing.Gap == 0 {
			return nil, f.endSync(nil)
		}
		if !f.hasLast {
			// The input buffer was flushed at open, so the line was quiet
			// for at least this read timeout: count the silence from then.
			f.last, f.hasLast = at.Add(-ReadTimeout), true
		}
	}
	return f.expire(at)
}

// commandSent is called right after a command was written to the port. While
// syncing with no byte dropped, what follows is the answer to it, so the
// command is a sync point; bytes already dropped are still a fragment and the
// sync goes on.
func (f *framer) commandSent() {
	if f.syncing && f.syncDropped == 0 {
		f.endSync(nil)
	}
}

// expire ends a gap frame after the silence and drops stale bytes; while
// syncing, the same silence is a sync point.
func (f *framer) expire(at time.Time) ([]frame, []string) {
	if !f.hasLast {
		return nil, nil
	}
	silence := at.Sub(f.last)
	if f.framing.Gap > 0 {
		if silence < f.framing.Gap {
			return nil, nil
		}
		if f.syncing {
			return nil, f.endSync(nil)
		}
		var frames []frame
		if !f.skipping {
			// A gap frame was read when its last byte was (spec S10).
			frames = f.emit(nil, f.last)
		}
		f.clear()
		return frames, nil
	}
	if silence < staleAfter {
		return nil, nil
	}
	if f.syncing {
		return nil, f.endSync(nil)
	}
	var kinds []string
	if !f.skipping && len(trimFrame(f.buf)) > 0 {
		kinds = append(kinds, kindIncompleteFrame)
	}
	f.clear()
	return nil, kinds
}

// sync takes c while syncing and reports whether it was dropped; a start
// character ends the sync and is not dropped, a terminator ends it and is.
func (f *framer) sync(c byte, kinds []string) (bool, []string) {
	fr := f.framing
	if fr.HasStart && c == fr.Start {
		return false, f.endSync(kinds)
	}
	if !isFrameNoise(c) {
		f.syncDropped++
	}
	if term := fr.Terminator; len(term) > 0 {
		f.buf = append(f.buf, c)
		if len(f.buf) > len(term) {
			f.buf = f.buf[len(f.buf)-len(term):]
		}
		if bytes.Equal(f.buf, term) {
			f.syncDropped -= countContent(term)
			return true, f.endSync(kinds)
		}
	}
	return true, kinds
}

// endSync ends the sync, counting incomplete_frame when something other than
// CR, LF and NUL was dropped.
func (f *framer) endSync(kinds []string) []string {
	if f.syncDropped > 0 {
		kinds = append(kinds, kindIncompleteFrame)
	}
	f.syncing, f.syncDropped = false, 0
	f.buf = f.buf[:0]
	return kinds
}

func (f *framer) clear() {
	f.buf = f.buf[:0]
	f.noiseRun = 0
	f.head = f.head[:0]
	f.inFrame = false
	f.skipping = false
}

// emit appends the trimmed pending frame read at at, when not empty, and
// clears it.
func (f *framer) emit(frames []frame, at time.Time) []frame {
	if t := trimFrame(f.buf); len(t) > 0 {
		frames = append(frames, frame{data: bytes.Clone(t), at: at})
	}
	f.buf = f.buf[:0]
	f.noiseRun = 0
	return frames
}

func (f *framer) feed(c byte, at time.Time, frames []frame, kinds []string) ([]frame, []string) {
	fr := f.framing
	if fr.HasStart {
		if c == fr.Start {
			f.buf = f.buf[:0]
			f.noiseRun = 0
			f.head = f.head[:0]
			f.inFrame = true
			f.skipping = false
			return frames, kinds
		}
		if !f.inFrame {
			return frames, kinds
		}
	}
	term := fr.Terminator
	if f.skipping {
		if len(term) > 0 {
			f.buf = append(f.buf, c)
			if len(f.buf) > len(term) {
				f.buf = f.buf[len(f.buf)-len(term):]
			}
			if bytes.Equal(f.buf, term) {
				f.buf = f.buf[:0]
				f.skipping = false
			}
		}
		return frames, kinds
	}
	if len(f.buf) == 0 && fr.Length == 0 && isFrameNoise(c) {
		// Leading CR, LF and NUL are trimmed from the frame, so they do not
		// count toward max_frame; never buffering them keeps it that way.
		if len(term) > 0 {
			f.head = append(f.head, c)
			if len(f.head) > len(term) {
				f.head = f.head[len(f.head)-len(term):]
			}
			if bytes.Equal(f.head, term) {
				f.head = f.head[:0]
				f.inFrame = false
			}
		}
		return frames, kinds
	}
	f.head = f.head[:0]
	f.buf = append(f.buf, c)
	if isFrameNoise(c) {
		f.noiseRun++
	} else {
		f.noiseRun = 0
	}
	switch {
	case len(term) > 0:
		if bytes.HasSuffix(f.buf, term) {
			f.buf = f.buf[:len(f.buf)-len(term)]
			f.inFrame = false
			if len(trimFrame(f.buf)) > f.maxFrame {
				f.buf = f.buf[:0]
				f.noiseRun = 0
				return frames, append(kinds, kindFrameTooLong)
			}
			return f.emit(frames, at), kinds
		}
		if f.contentLen() > f.maxFrame+len(term)-1 || len(f.buf) > f.bufCap() {
			return frames, f.tooLong(kinds)
		}
	case fr.Length > 0:
		if len(f.buf) >= fr.Length {
			frames = f.emit(frames, at)
			f.inFrame = false
		}
	default:
		if f.contentLen() > f.maxFrame || len(f.buf) > f.bufCap() {
			return frames, f.tooLong(kinds)
		}
	}
	return frames, kinds
}

// tooLong counts the frame and starts skipping to the resync point.
func (f *framer) tooLong(kinds []string) []string {
	f.noiseRun = 0
	term := f.framing.Terminator
	if len(term) > 1 && !f.framing.HasStart {
		f.buf = f.buf[len(f.buf)-(len(term)-1):]
	} else {
		f.buf = f.buf[:0]
	}
	if f.framing.HasStart {
		f.inFrame = false
		f.buf = f.buf[:0]
	} else {
		f.skipping = true
	}
	return append(kinds, kindFrameTooLong)
}

func trimFrame(b []byte) []byte {
	return bytes.Trim(b, "\r\n\x00")
}

// countContent counts the bytes of b other than CR, LF and NUL.
func countContent(b []byte) int {
	n := 0
	for _, c := range b {
		if !isFrameNoise(c) {
			n++
		}
	}
	return n
}

func isFrameNoise(c byte) bool {
	return c == '\r' || c == '\n' || c == 0
}

// contentLen is the pending size without trailing CR, LF and NUL, which are
// trimmed from the frame and so never count toward max_frame.
func (f *framer) contentLen() int {
	return len(f.buf) - f.noiseRun
}

// bufCap is the hard limit of buffered bytes: a frame of max_frame bytes plus
// its terminator and a short CRLF tail still fits, endless noise does not.
func (f *framer) bufCap() int {
	return f.maxFrame + len(f.framing.Terminator) + 2
}
