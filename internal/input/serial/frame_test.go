package serial

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var frameT0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

type framerStep struct {
	at   time.Duration
	data string
	tick bool
	// command is a command written (commandSent).
	command bool
	frames  []string
	kinds   []string
	// stamps, when set, are the read times of the frames.
	stamps []time.Duration
}

func runFramer(t *testing.T, fr *framer, steps []framerStep) {
	t.Helper()
	for i, s := range steps {
		var frames []frame
		var kinds []string
		if s.command {
			fr.commandSent()
		} else if s.tick {
			frames, kinds = fr.tick(frameT0.Add(s.at))
		} else {
			frames, kinds = fr.push([]byte(s.data), frameT0.Add(s.at))
		}
		got := make([]string, 0, len(frames))
		stamps := make([]time.Duration, 0, len(frames))
		for _, f := range frames {
			got = append(got, string(f.data))
			stamps = append(stamps, f.at.Sub(frameT0))
		}
		want := s.frames
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("step %d: frames = %q, want %q", i, got, want)
		}
		if len(kinds) != len(s.kinds) || (len(kinds) > 0 && !reflect.DeepEqual(kinds, s.kinds)) {
			t.Fatalf("step %d: kinds = %v, want %v", i, kinds, s.kinds)
		}
		if s.stamps != nil && !reflect.DeepEqual(stamps, s.stamps) {
			t.Fatalf("step %d: frames read at %v, want %v", i, stamps, s.stamps)
		}
	}
}

func TestFramer(t *testing.T) {
	ms := time.Millisecond
	cr := Framing{Terminator: []byte("\r")}
	tests := []struct {
		name     string
		framing  Framing
		maxFrame int
		steps    []framerStep
	}{
		{"terminator two frames", cr, 1024, []framerStep{{data: "1.5\r2.5\r", frames: []string{"1.5", "2.5"}}}},
		{"gauge sends crlf under cr terminator", cr, 1024, []framerStep{{data: "1.5\r\n2.5\r\n", frames: []string{"1.5", "2.5"}}}},
		{"crlf then tick never counts the stray lf", cr, 1024, []framerStep{
			{data: "1.5\r\n", frames: []string{"1.5"}},
			{at: 3 * time.Second, tick: true},
		}},
		{"split across three pushes", cr, 1024, []framerStep{
			{data: "1."}, {at: ms, data: "5"}, {at: 2 * ms, data: "\r", frames: []string{"1.5"}},
		}},
		{"crlf terminator split", Framing{Terminator: []byte("\r\n")}, 1024, []framerStep{
			{data: "12\r"}, {at: ms, data: "\n", frames: []string{"12"}},
		}},
		{"length", Framing{Length: 4}, 1024, []framerStep{{data: "ABCDEFGH", frames: []string{"ABCD", "EFGH"}}}},
		{"gap", Framing{Gap: 50 * ms}, 1024, []framerStep{
			{data: "AB"}, {at: 20 * ms, data: "CD"},
			{at: 60 * ms, tick: true},
			{at: 71 * ms, tick: true, frames: []string{"ABCD"}, stamps: []time.Duration{20 * ms}},
			{at: 80 * ms, tick: true},
		}},
		{"gap push after silence completes previous frame", Framing{Gap: 50 * ms}, 1024, []framerStep{
			{data: "AB"}, {at: 100 * ms, data: "CD", frames: []string{"AB"}, stamps: []time.Duration{0}},
			{at: 200 * ms, tick: true, frames: []string{"CD"}, stamps: []time.Duration{100 * ms}},
		}},
		{"terminator frames read at the push", cr, 1024, []framerStep{
			{data: "1."}, {at: 30 * ms, data: "5\r2", frames: []string{"1.5"}, stamps: []time.Duration{30 * ms}},
		}},
		{"start", Framing{Terminator: []byte("\x03"), Start: 2, HasStart: true}, 1024, []framerStep{
			{data: "xx\x02AB\x03", frames: []string{"AB"}},
		}},
		{"start restarts a pending frame", Framing{Terminator: []byte("\x03"), Start: 2, HasStart: true}, 1024, []framerStep{
			{data: "\x02AB\x02CD\x03", frames: []string{"CD"}},
		}},
		{"too long resyncs at terminator", cr, 8, []framerStep{
			{data: "123456789", kinds: []string{"frame_too_long"}},
			{at: ms, data: "0123"},
			{at: 2 * ms, data: "\rOK\r", frames: []string{"OK"}},
		}},
		{"exactly max fits", cr, 8, []framerStep{{data: "12345678\r", frames: []string{"12345678"}}}},
		{"too long with crlf terminator counts once", Framing{Terminator: []byte("\r\n")}, 4, []framerStep{
			{data: "1234567890", kinds: []string{"frame_too_long"}},
			{at: ms, data: "\r"},
			{at: 2 * ms, data: "\nOK\r\n", frames: []string{"OK"}},
		}},
		{"too long with start resyncs at next start", Framing{Terminator: []byte("\x03"), Start: 2, HasStart: true}, 4, []framerStep{
			{data: "\x0212345\x03junk", kinds: []string{"frame_too_long"}},
			{at: ms, data: "\x02OK\x03", frames: []string{"OK"}},
		}},
		{"too long in gap mode resyncs after silence", Framing{Gap: 50 * ms}, 4, []framerStep{
			{data: "123456", kinds: []string{"frame_too_long"}},
			{at: 10 * ms, data: "78"},
			{at: 100 * ms, data: "OK"},
			{at: 200 * ms, tick: true, frames: []string{"OK"}},
		}},
		{"stale bytes dropped", cr, 1024, []framerStep{
			{data: "12"},
			{at: 1999 * ms, tick: true},
			{at: 2001 * ms, tick: true, kinds: []string{"incomplete_frame"}},
			{at: 2002 * ms, data: "3\r", frames: []string{"3"}},
		}},
		{"stale detected by push", cr, 1024, []framerStep{
			{data: "12"},
			{at: 3 * time.Second, data: "3\r", frames: []string{"3"}, kinds: []string{"incomplete_frame"}},
		}},
		{"stale in length mode", Framing{Length: 4}, 1024, []framerStep{
			{data: "12"},
			{at: 2001 * ms, tick: true, kinds: []string{"incomplete_frame"}},
			{at: 2002 * ms, data: "ABCD", frames: []string{"ABCD"}},
		}},
		{"slow bytes keep the frame alive", cr, 1024, []framerStep{
			{data: "1"}, {at: 1500 * ms, data: "2"}, {at: 3 * time.Second, data: "\r", frames: []string{"12"}},
		}},
		{"empty after trimming", cr, 1024, []framerStep{{data: "\r\r\n\x00\r", frames: nil}}},
		{"crlf gauge with values of exactly max frame", cr, 8, []framerStep{
			{data: "12345678\r\n12345678\r\n", frames: []string{"12345678", "12345678"}},
		}},
		{"crlf terminator too long by the carry is counted", Framing{Terminator: []byte("\r\n")}, 4, []framerStep{
			{data: "00000\r\n12\r\n", frames: []string{"12"}, kinds: []string{"frame_too_long"}},
		}},
		{"gap leading crlf not counted", Framing{Gap: 50 * ms}, 4, []framerStep{
			{data: "\r\nABCD"}, {at: 100 * ms, tick: true, frames: []string{"ABCD"}},
		}},
		{"gap trailing crlf not counted", Framing{Gap: 50 * ms}, 4, []framerStep{
			{data: "ABCD\r\n"}, {at: 100 * ms, tick: true, frames: []string{"ABCD"}},
		}},
		{"length with start trailing noise", Framing{Length: 4, Start: 2, HasStart: true}, 4, []framerStep{
			{data: "\x02ABCD\r\n", frames: []string{"ABCD"}},
		}},
		{"endless trailing noise is bounded in gap mode", Framing{Gap: 50 * ms}, 4, []framerStep{
			{data: "A" + strings.Repeat("\x00", 10000), kinds: []string{"frame_too_long"}},
			{at: 100 * ms, data: "OK"},
			{at: 200 * ms, tick: true, frames: []string{"OK"}},
		}},
		{"start then multi-byte terminator is an empty frame", Framing{Terminator: []byte("\r\n"), Start: 2, HasStart: true}, 1024, []framerStep{
			{data: "\x02\r\nXY"},
			{at: 3 * time.Second, tick: true},
		}},
		{"nothing pending tick", cr, 1024, []framerStep{{at: 5 * time.Second, tick: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runFramer(t, synced(newFramer(tt.framing, tt.maxFrame)), tt.steps)
		})
	}
}

// synced returns fr past its sync point, as after a quiet read cycle one
// second before frameT0, so that the steps start on a frame boundary.
func synced(fr *framer) *framer {
	fr.tick(frameT0.Add(-time.Second))
	return fr
}

func TestFramerSync(t *testing.T) {
	ms := time.Millisecond
	cr := Framing{Terminator: []byte("\r")}
	incomplete := []string{"incomplete_frame"}
	tests := []struct {
		name    string
		framing Framing
		steps   []framerStep
	}{
		{"terminator drops the half frame up to it", cr, []framerStep{
			{data: "5.250\r01A+1.000\r", frames: []string{"01A+1.000"}, kinds: incomplete},
		}},
		{"a terminator first drops nothing", cr, []framerStep{
			{data: "\r1.5\r", frames: []string{"1.5"}},
		}},
		{"only noise before the terminator is not counted", Framing{Terminator: []byte("\x03")}, []framerStep{
			{data: "\r\n\x00\x031.5\x03", frames: []string{"1.5"}},
		}},
		{"multi-byte terminator split across reads", Framing{Terminator: []byte("\r\n")}, []framerStep{
			{data: "x\r"},
			{at: ms, data: "\n2\r\n", frames: []string{"2"}, kinds: incomplete},
		}},
		{"a quiet cycle first drops nothing", cr, []framerStep{
			{tick: true},
			{at: 100 * ms, data: "1.5\r", frames: []string{"1.5"}},
		}},
		{"a quiet cycle after a half frame drops it once", cr, []framerStep{
			{data: "5.2"},
			{at: 100 * ms, tick: true, kinds: incomplete},
			{at: 200 * ms, data: "1.5\r", frames: []string{"1.5"}},
		}},
		{"a quiet cycle after noise counts nothing", cr, []framerStep{
			{data: "\n"},
			{at: 100 * ms, tick: true},
			{at: 200 * ms, data: "1.5\r", frames: []string{"1.5"}},
		}},
		{"two seconds of silence end sync like a quiet cycle", cr, []framerStep{
			{data: "12"},
			{at: 3 * time.Second, data: "3\r", frames: []string{"3"}, kinds: incomplete},
		}},
		{"a start character ends sync", Framing{Length: 2, Start: 2, HasStart: true}, []framerStep{
			{data: "XYZ\x02AB", frames: []string{"AB"}, kinds: incomplete},
		}},
		{"a start character first drops nothing", Framing{Terminator: []byte("\x03"), Start: 2, HasStart: true}, []framerStep{
			{data: "\x02AB\x03", frames: []string{"AB"}},
		}},
		{"length waits for a quiet cycle", Framing{Length: 4}, []framerStep{
			{data: "ABCDEFGH"},
			{at: 100 * ms, tick: true, kinds: incomplete},
			{at: 200 * ms, data: "1234", frames: []string{"1234"}},
		}},
		{"gap drops the bytes before the first silence", Framing{Gap: 50 * ms}, []framerStep{
			{data: "AB"}, {at: 20 * ms, data: "CD"},
			{at: 71 * ms, tick: true, kinds: incomplete},
			{at: 100 * ms, data: "EF"},
			{at: 200 * ms, tick: true, frames: []string{"EF"}, stamps: []time.Duration{100 * ms}},
		}},
		{"gap after a quiet start", Framing{Gap: 50 * ms}, []framerStep{
			{tick: true},
			{at: 60 * ms, data: "AB"},
			{at: 200 * ms, tick: true, frames: []string{"AB"}, stamps: []time.Duration{60 * ms}},
		}},
		{"gap needs a silence of gap, not one quiet cycle", Framing{Gap: 500 * ms}, []framerStep{
			{tick: true},
			{at: 100 * ms, data: "CD"},
			{at: 200 * ms, tick: true},
			{at: 700 * ms, tick: true, kinds: incomplete},
			{at: 800 * ms, data: "EF"},
			{at: 1400 * ms, tick: true, frames: []string{"EF"}},
		}},
		{"gap of one read timeout: the first quiet cycle ends sync", Framing{Gap: ReadTimeout}, []framerStep{
			{at: ReadTimeout, tick: true},
			{at: 150 * ms, data: "AB"},
			{at: 400 * ms, tick: true, frames: []string{"AB"}, stamps: []time.Duration{150 * ms}},
		}},
		{"gap counts the first silence from one read timeout back", Framing{Gap: 500 * ms}, []framerStep{
			{at: 100 * ms, tick: true},
			{at: 400 * ms, tick: true},
			{at: 500 * ms, tick: true},
			{at: 550 * ms, data: "EF"},
			{at: 1200 * ms, tick: true, frames: []string{"EF"}},
		}},
		{"a command ends sync in gap mode", Framing{Gap: 500 * ms}, []framerStep{
			{at: 100 * ms, tick: true},
			{at: 100 * ms, command: true},
			{at: 150 * ms, data: "AB"},
			{at: 800 * ms, tick: true, frames: []string{"AB"}, stamps: []time.Duration{150 * ms}},
		}},
		{"a command ends sync in terminator mode", cr, []framerStep{
			{command: true},
			{at: ms, data: "1.5\r", frames: []string{"1.5"}},
		}},
		{"a command after pending bytes leaves the fragment dropped", Framing{Gap: 500 * ms}, []framerStep{
			{data: "5.2"},
			{at: ms, command: true},
			{at: 20 * ms, data: "1"},
			{at: 700 * ms, tick: true, kinds: incomplete},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runFramer(t, newFramer(tt.framing, 1024), tt.steps)
		})
	}
}

func TestFramerReset(t *testing.T) {
	fr := synced(newFramer(Framing{Terminator: []byte("\r")}, 1024))
	fr.push([]byte("12"), frameT0)
	fr.reset()
	// The pending "12" is dropped without a count; after the reset the
	// framer syncs again, so the tail read first is not a frame.
	runFramer(t, fr, []framerStep{
		{at: time.Millisecond, data: "9\r", kinds: []string{"incomplete_frame"}},
		{at: 2 * time.Millisecond, data: "8\r", frames: []string{"8"}},
	})
}

func TestFramerFramesDoNotAliasBuffer(t *testing.T) {
	fr := synced(newFramer(Framing{Terminator: []byte("\r")}, 1024))
	frames, _ := fr.push([]byte("AB\r"), frameT0)
	fr.push([]byte("XY\r"), frameT0)
	if string(frames[0].data) != "AB" {
		t.Fatalf("frame changed to %q", frames[0].data)
	}
}

func TestFramerBufferStaysBounded(t *testing.T) {
	fr := synced(newFramer(Framing{Gap: 50 * time.Millisecond}, 4))
	fr.push([]byte("A"+strings.Repeat("\x00", 10000)), frameT0)
	if len(fr.buf) > fr.bufCap() {
		t.Fatalf("buffer holds %d bytes, cap %d", len(fr.buf), fr.bufCap())
	}
}
