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

// framer cuts the byte stream of a port into frames. It holds no clock: the
// only time it knows is the one given to push and tick.
type framer struct {
	framing  Framing
	maxFrame int

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
	return &framer{framing: f, maxFrame: maxFrame}
}

// reset drops what is pending, without counting it (port lost).
func (f *framer) reset() {
	f.clear()
	f.hasLast = false
}

// push takes the bytes read at at and returns the frames they complete and the
// kinds to count.
func (f *framer) push(b []byte, at time.Time) ([][]byte, []string) {
	var frames [][]byte
	var kinds []string
	if len(b) == 0 {
		return nil, nil
	}
	frames, kinds = f.tick(at)
	for _, c := range b {
		frames, kinds = f.feed(c, frames, kinds)
	}
	f.last, f.hasLast = at, true
	return frames, kinds
}

// tick ends a gap frame after the silence and drops stale bytes.
func (f *framer) tick(at time.Time) ([][]byte, []string) {
	if !f.hasLast {
		return nil, nil
	}
	silence := at.Sub(f.last)
	if f.framing.Gap > 0 {
		if silence < f.framing.Gap {
			return nil, nil
		}
		var frames [][]byte
		if !f.skipping {
			frames = f.emit(nil)
		}
		f.clear()
		return frames, nil
	}
	if silence < staleAfter {
		return nil, nil
	}
	var kinds []string
	if !f.skipping && len(trimFrame(f.buf)) > 0 {
		kinds = append(kinds, kindIncompleteFrame)
	}
	f.clear()
	return nil, kinds
}

func (f *framer) clear() {
	f.buf = f.buf[:0]
	f.noiseRun = 0
	f.head = f.head[:0]
	f.inFrame = false
	f.skipping = false
}

// emit appends the trimmed pending frame, when not empty, and clears it.
func (f *framer) emit(frames [][]byte) [][]byte {
	if t := trimFrame(f.buf); len(t) > 0 {
		frames = append(frames, bytes.Clone(t))
	}
	f.buf = f.buf[:0]
	f.noiseRun = 0
	return frames
}

func (f *framer) feed(c byte, frames [][]byte, kinds []string) ([][]byte, []string) {
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
			return f.emit(frames), kinds
		}
		if f.contentLen() > f.maxFrame+len(term)-1 || len(f.buf) > f.bufCap() {
			return frames, f.tooLong(kinds)
		}
	case fr.Length > 0:
		if len(f.buf) >= fr.Length {
			frames = f.emit(frames)
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
