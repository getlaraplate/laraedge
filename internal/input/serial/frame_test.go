package serial

import (
	"reflect"
	"testing"
	"time"
)

var frameT0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

type framerStep struct {
	at     time.Duration
	data   string
	tick   bool
	frames []string
	kinds  []string
}

func runFramer(t *testing.T, fr *framer, steps []framerStep) {
	t.Helper()
	for i, s := range steps {
		var frames [][]byte
		var kinds []string
		if s.tick {
			frames, kinds = fr.tick(frameT0.Add(s.at))
		} else {
			frames, kinds = fr.push([]byte(s.data), frameT0.Add(s.at))
		}
		got := make([]string, 0, len(frames))
		for _, f := range frames {
			got = append(got, string(f))
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
			{at: 71 * ms, tick: true, frames: []string{"ABCD"}},
			{at: 80 * ms, tick: true},
		}},
		{"gap push after silence completes previous frame", Framing{Gap: 50 * ms}, 1024, []framerStep{
			{data: "AB"}, {at: 100 * ms, data: "CD", frames: []string{"AB"}},
			{at: 200 * ms, tick: true, frames: []string{"CD"}},
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
		{"nothing pending tick", cr, 1024, []framerStep{{at: 5 * time.Second, tick: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runFramer(t, newFramer(tt.framing, tt.maxFrame), tt.steps)
		})
	}
}

func TestFramerReset(t *testing.T) {
	fr := newFramer(Framing{Terminator: []byte("\r")}, 1024)
	fr.push([]byte("12"), frameT0)
	fr.reset()
	runFramer(t, fr, []framerStep{{at: time.Millisecond, data: "9\r", frames: []string{"9"}}})
}

func TestFramerFramesDoNotAliasBuffer(t *testing.T) {
	fr := newFramer(Framing{Terminator: []byte("\r")}, 1024)
	frames, _ := fr.push([]byte("AB\r"), frameT0)
	fr.push([]byte("XY\r"), frameT0)
	if string(frames[0]) != "AB" {
		t.Fatalf("frame changed to %q", frames[0])
	}
}
