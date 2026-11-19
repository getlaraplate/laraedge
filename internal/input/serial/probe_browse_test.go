package serial

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/model"
)

// lineSync is a terminator fed first so that the probe's framer, which
// drops bytes until a sync point, starts on a frame boundary.
const lineSync = "\r"

// probeResult is what a Probe call returned.
type probeResult struct {
	samples []model.Sample
	kinds   map[PointRef]string
	err     error
}

// probe runs ProbePoints on the harness until every point has a sample, or
// until the port has been read dry, when it cancels the probe.
func (h *runHarness) probe(t *testing.T, cancelWhenIdle bool) probeResult {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan probeResult, 1)
	go func() {
		s, k, err := h.in.ProbePoints(ctx)
		done <- probeResult{s, k, err}
	}()
	if cancelWhenIdle {
		h.clk.BlockUntil(t, 1)
		cancel()
		h.stop()
	}
	return <-done
}

func TestProbeReadsOneSampleForEveryPoint(t *testing.T) {
	h := newRunHarness(t, nil)
	h.port.feed(lineSync)
	h.port.feed("SN9\r01A1.5\r02A2.5\r01A9\r")

	got := h.probe(t, false)
	if got.err != nil {
		t.Fatalf("probe: %v", got.err)
	}
	if len(got.samples) != 2 || got.samples[0].Device != "a" || got.samples[0].Value.Float != 1.5 || got.samples[1].Device != "b" || got.samples[1].Value.Float != 2.5 {
		t.Fatalf("samples = %+v", got.samples)
	}
	if got.samples[0].Context == nil || got.samples[0].Context.Serial != "9" {
		t.Errorf("context = %+v", got.samples[0].Context)
	}
	if len(got.kinds) != 0 {
		t.Errorf("kinds = %v", got.kinds)
	}
	if want := []string{"context: serial=9"}; !slices.Equal(h.in.ProbeNotes(), want) {
		t.Errorf("notes = %q, want %q", h.in.ProbeNotes(), want)
	}
	if h.recordCount() != 0 || h.port.isOpen() || h.opener.count() != 1 {
		t.Errorf("records = %d, open = %v, opens = %d", h.recordCount(), h.port.isOpen(), h.opener.count())
	}
}

func TestProbeSaysNoFrameForASilentPointAndKeepsUnmatchedFrames(t *testing.T) {
	h := newRunHarness(t, nil)
	h.port.feed(lineSync)
	h.port.feed("01A1.5\r")
	for i := range 12 {
		h.port.feed(fmt.Sprintf("junk%d\r", i))
	}
	h.port.feed("ZZ" + strings.Repeat("x", 100) + "\r")

	got := h.probe(t, true)
	if got.err != nil {
		t.Fatalf("probe: %v", got.err)
	}
	if len(got.samples) != 1 || got.samples[0].Device != "a" {
		t.Fatalf("samples = %+v", got.samples)
	}
	if got.kinds[PointRef{Device: "b", Signal: "w"}] != KindNoFrame {
		t.Errorf("kinds = %v", got.kinds)
	}
	notes := h.in.ProbeNotes()
	if len(notes) != maxProbeUnmatched {
		t.Fatalf("notes = %q", notes)
	}
	if notes[0] != `unmatched: "junk0"` {
		t.Errorf("notes[0] = %q", notes[0])
	}
}

func TestProbeTruncatesAnUnmatchedFrameTo80Bytes(t *testing.T) {
	h := newRunHarness(t, nil)
	h.port.feed(lineSync)
	h.port.feed("ZZ" + strings.Repeat("x", 100) + "\r")
	h.probe(t, true)

	notes := h.in.ProbeNotes()
	want := `unmatched: "ZZ` + strings.Repeat("x", 78) + `"`
	if len(notes) != 1 || notes[0] != want {
		t.Errorf("notes = %q", notes)
	}
}

func TestProbeNotesListContextKeysInFixedOrder(t *testing.T) {
	h := newRunHarness(t, map[string]any{"context": []map[string]any{
		{"key": "lot", "match": `^L(\d+)$`},
		{"key": "serial", "match": `^SN(\d+)$`},
		{"key": "order_ref", "match": `^O(\d+)$`},
	}})
	h.port.feed(lineSync)
	h.port.feed("L7\rO5\rSN9\r")
	h.probe(t, true)

	want := []string{"context: serial=9", "context: order_ref=5", "context: lot=7"}
	if !slices.Equal(h.in.ProbeNotes(), want) {
		t.Errorf("notes = %q, want %q", h.in.ProbeNotes(), want)
	}
}

func TestProbeOfABusyPortSaysSo(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"busy", fmt.Errorf("%w: /dev/secret", ErrPortBusy), "port busy: the input may be running; stop it before probing"},
		{"missing", ErrPortMissing, "the port does not exist"},
		{"permission", ErrPortPermission, "permission denied opening the port"},
		{"other", errors.New("boom /dev/secret"), "the port cannot be opened"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunHarness(t, nil)
			h.opener.then(tc.err)
			_, err := h.in.Probe(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"gauges"`) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("err = %v", err)
			}
			if h.opener.count() != 1 {
				t.Errorf("opens = %d", h.opener.count())
			}
		})
	}
}

func TestProbeSendsTheRequestsOnTheSchedule(t *testing.T) {
	h := newRunHarness(t, map[string]any{"requests": []map[string]any{{"command": "W\r", "every": "1s"}}})
	h.port.feed(lineSync)
	h.port.feed("01A1\r02A2\r")
	got := h.probe(t, false)
	if got.err != nil || len(got.samples) != 2 {
		t.Fatalf("probe = %+v", got)
	}
	if w := h.port.written(); len(w) == 0 || w[0] != "W\r" {
		t.Errorf("written = %q", w)
	}
}

func TestBrowseMarksTheConfiguredPort(t *testing.T) {
	h := newRunHarness(t, nil)
	list := func() ([]PortInfo, error) {
		return []PortInfo{
			{Name: "/dev/ttyUSB1", StablePath: "/dev/serial/by-id/usb-B", VID: "0403", PID: "6001", Product: "FT232R", SerialNumber: "A1"},
			{Name: "/dev/ttyUSB0"},
		}, nil
	}
	var buf bytes.Buffer
	if err := Browse(h.cfg, &buf, list); err != nil {
		t.Fatal(err)
	}
	want := "* /dev/ttyUSB0  -  -  -  -\n" +
		"  /dev/ttyUSB1  /dev/serial/by-id/usb-B  0403:6001  FT232R  A1\n"
	if buf.String() != want {
		t.Errorf("got\n%q\nwant\n%q", buf.String(), want)
	}
}

func TestBrowseMatchesTheStablePathAndReportsAMissingDevice(t *testing.T) {
	list := func() ([]PortInfo, error) {
		return []PortInfo{{Name: "/dev/ttyUSB1", StablePath: "/dev/ttyUSB0"}}, nil
	}
	h := newRunHarness(t, nil)
	var buf bytes.Buffer
	if err := Browse(h.cfg, &buf, list); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "* /dev/ttyUSB1") || strings.Contains(buf.String(), "not found") {
		t.Errorf("stable path: %q", buf.String())
	}

	buf.Reset()
	if err := Browse(h.cfg, &buf, func() ([]PortInfo, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "device /dev/ttyUSB0: not found\n" {
		t.Errorf("missing: %q", buf.String())
	}

	buf.Reset()
	boom := errors.New("enumerate failed")
	if err := Browse(h.cfg, &buf, func() ([]PortInfo, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

func TestProbeNotesEscapeTheContextValue(t *testing.T) {
	h := newRunHarness(t, map[string]any{"context": []map[string]any{{"key": "serial", "match": `^SN(.+)$`}}})
	h.port.feed(lineSync)
	h.port.feed("SNA\x1b[2JB\r")
	h.probe(t, true)

	want := []string{`context: serial=A\x1b[2JB`}
	if !slices.Equal(h.in.ProbeNotes(), want) {
		t.Errorf("notes = %q, want %q", h.in.ProbeNotes(), want)
	}
}

func TestBrowseEscapesTheUSBIDs(t *testing.T) {
	h := newRunHarness(t, nil)
	list := func() ([]PortInfo, error) {
		return []PortInfo{{Name: "/dev/ttyUSB0", VID: "04\n03", PID: "\x1b[2J"}}, nil
	}
	var buf bytes.Buffer
	if err := Browse(h.cfg, &buf, list); err != nil {
		t.Fatal(err)
	}
	if want := `* /dev/ttyUSB0  -  04\n03:\x1b[2J  -  -` + "\n"; buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestProbeDropsTheHalfFrameReadAtTheOpen(t *testing.T) {
	h := newRunHarness(t, nil)
	h.port.feed("5.250\r01A+1.000\r02A2\r")
	got := h.probe(t, false)
	if got.err != nil || len(got.samples) != 2 || got.samples[0].Value.Float != 1.0 || got.samples[1].Value.Float != 2 {
		t.Fatalf("probe = %+v", got)
	}
	if notes := h.in.ProbeNotes(); len(notes) != 0 {
		t.Fatalf("notes = %q, want none (the half frame is not unmatched)", notes)
	}
}
