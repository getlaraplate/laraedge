package serial

import (
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	edgeinput "github.com/getlaraplate/laraedge/internal/input"
	"github.com/getlaraplate/laraedge/internal/input/driverkit"
	"github.com/getlaraplate/laraedge/internal/model"
)

func TestFactoryRefusesABadPointAndOpensNothing(t *testing.T) {
	h := newRunHarness(t, nil)
	poll := gaugePoint("w", `^01A(.+)$`, "float64")
	poll.Read = config.Read{Mode: config.ReadPoll}
	bad := h.cfg
	bad.Devices = []config.Device{{Device: "a", Points: []config.Point{poll}}}
	if _, err := Factory(Options{})(bad, clock.Real()); err == nil || !strings.HasPrefix(err.Error(), `serial input "gauges": devices.0.points.0`) {
		t.Fatalf("Factory = %v, want an error naming the input and the point", err)
	}

	in, err := Factory(Options{})(h.cfg, clock.Real())
	if err != nil {
		t.Fatalf("Factory: %v", err)
	}
	if _, ok := in.(edgeinput.HealthReporter); !ok {
		t.Fatal("the serial input reports no health")
	}
	if err := in.Close(); err != nil {
		t.Fatalf("Close unstarted: %v", err)
	}
	if h.opener.count() != 0 {
		t.Fatalf("opener called %d times by the factory", h.opener.count())
	}
}

func TestStartReturnsWithoutPort(t *testing.T) {
	h := newRunHarness(t, nil)
	h.opener.then(ErrPortMissing, ErrPortMissing, ErrPortBusy, ErrPortPermission, ErrPortPermission, ErrPortMissing, ErrPortMissing)
	h.start()

	if h.in.Health().Connected {
		t.Fatal("connected without a port")
	}
	if got := h.readErrors(KindOpen); got != 1 {
		t.Fatalf("open errors after start = %d, want 1", got)
	}
	// Waits of 0.5 s, 1 s, 2 s, 4 s, 8 s, 16 s: seven failed attempts by 31.5 s.
	for i, wait := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
		h.advance(wait)
		if got := h.readErrors(KindOpen); got != uint64(i+2) {
			t.Fatalf("open errors after attempt %d = %d, want %d", i+2, got, i+2)
		}
	}
	if h.in.Health().Connected {
		t.Fatal("connected without a port")
	}

	want := map[string]int{msgOpenMissing: 1, msgOpenBusy: 1, msgOpenPermission: 1}
	for msg, n := range want {
		lines := h.logs.at(t, "WARN", msg)
		if len(lines) != n {
			t.Errorf("%q logged %d times, want %d", msg, len(lines), n)
		}
		for _, l := range lines {
			if l["input"] != "gauges" {
				t.Errorf("%q without the input: %v", msg, l)
			}
		}
	}
	if perm := h.logs.at(t, "WARN", msgOpenPermission); runtime.GOOS == "linux" && (len(perm) != 1 || !strings.Contains(perm[0]["hint"].(string), "dialout")) {
		t.Errorf("permission warning on Linux does not name dialout: %v", perm)
	}

	// The next attempt opens the port.
	h.advance(32 * time.Second)
	if !h.in.Health().Connected || !h.port.isOpen() {
		t.Fatal("not connected after the port became free")
	}
	if got := h.readErrors(KindOpen); got != 7 {
		t.Fatalf("open errors = %d, want 7", got)
	}
}

func TestFramesBecomeSamples(t *testing.T) {
	h := newRunHarness(t, nil)
	h.start()
	if !h.in.Health().Connected {
		t.Fatal("not connected with the port open")
	}
	// A quiet read cycle syncs the framer on a frame boundary.
	h.step()

	h.port.feed("SN7\r01A1.5\r")
	h.step()
	readAt := h.clk.Now()

	got := h.samples()
	if len(got) != 1 {
		t.Fatalf("samples = %+v, want one", got)
	}
	s := got[0]
	if s.Device != "a" || s.Signal != "w" || s.Value.Float != 1.5 || s.Quality != model.Good || !s.TS.Equal(readAt) || s.Priority != model.Critical {
		t.Fatalf("sample = %+v", s)
	}
	if s.Context == nil || s.Context.Serial != "7" {
		t.Fatalf("context = %+v, want serial 7", s.Context)
	}
	set := h.logs.at(t, "INFO", msgContextSet)
	if len(set) != 1 || set[0]["key"] != "serial" || set[0]["value"] != "7" || set[0]["input"] != "gauges" {
		t.Fatalf("context change log = %v", set)
	}

	// The same scan again changes nothing and is not logged again.
	h.port.feed("SN7\r")
	h.step()
	if set := h.logs.at(t, "INFO", msgContextSet); len(set) != 1 {
		t.Fatalf("context change logged %d times for the same scan, want 1", len(set))
	}

	h.port.feed("01Axyz\r99\r")
	h.step()
	if h.readErrors(KindConversion) != 1 || h.readErrors(KindUnmatched) != 1 {
		t.Fatalf("read errors = %v", h.in.Health().ReadErrors)
	}
	unmatched := h.logs.at(t, "DEBUG", msgUnmatched)
	if len(unmatched) != 1 || unmatched[0]["frame"] != `"99"` {
		t.Fatalf("unmatched log = %v", unmatched)
	}

	h.close()
	h.close()
	dropped := h.logs.at(t, "INFO", msgContextDropped)
	if len(dropped) != 1 || dropped[0]["value"] != nil {
		t.Fatalf("context dropped log = %v", dropped)
	}
	fed := []string{"SN7", "01A1.5", "01Axyz", "99"}
	for _, l := range h.logs.lines(t) {
		if l["level"] == "DEBUG" || l["msg"] == msgContextSet {
			continue
		}
		text := strings.Join(values(l), " ")
		for _, frame := range fed {
			if strings.Contains(text, frame) {
				t.Fatalf("frame %q in a log line above debug: %v", frame, l)
			}
		}
	}
}

// values returns the string attributes of a log line but its time.
func values(m map[string]any) []string {
	var out []string
	for k, v := range m {
		if s, ok := v.(string); ok && k != "time" {
			out = append(out, s)
		}
	}
	return out
}

func TestUnmatchedFrameLogTruncated(t *testing.T) {
	h := newRunHarness(t, nil)
	h.start()
	h.step()
	h.port.feed(strings.Repeat("x", 100) + "\r")
	h.step()
	lines := h.logs.at(t, "DEBUG", msgUnmatched)
	if len(lines) != 1 || lines[0]["frame"] != `"`+strings.Repeat("x", 80)+`"` {
		t.Fatalf("unmatched log = %v", lines)
	}
}

func TestUnpluggedMidFrame(t *testing.T) {
	h := newRunHarness(t, nil)
	h.start()
	h.step()

	h.port.feed("SN7\r")
	h.step()
	h.port.feed("01A1")
	h.step()
	h.port.fail()
	h.step()
	if h.readErrors(KindPortLost) != 1 || h.in.Health().Connected || h.port.isOpen() {
		t.Fatalf("after the loss: errors %v connected %v open %v", h.in.Health().ReadErrors, h.in.Health().Connected, h.port.isOpen())
	}

	// Closed 1.5 s: a failed attempt after 0.5 s, the open after 1 s more.
	h.opener.then(ErrPortMissing)
	h.advance(1500 * time.Millisecond)
	if !h.port.isOpen() || !h.in.Health().Connected {
		t.Fatal("port not open again after the backoff")
	}
	if h.readErrors(KindOpen) != 1 {
		t.Fatalf("open errors = %d, want 1", h.readErrors(KindOpen))
	}

	// The first read after reopening starts in the middle of a frame: its
	// tail is dropped up to the terminator and counted once.
	h.port.feed(".5\r01A2.0\r")
	h.step()
	got := h.samples()
	if len(got) != 1 || got[0].Value.Float != 2.0 {
		t.Fatalf("samples = %+v, want only 2.0", got)
	}
	if h.readErrors(kindIncompleteFrame) != 1 || h.readErrors(KindUnmatched) != 0 {
		t.Fatalf("read errors = %v, want one incomplete_frame and no unmatched", h.in.Health().ReadErrors)
	}
	if got[0].Context == nil || got[0].Context.Serial != "7" {
		t.Fatalf("context = %+v, want serial 7 kept across the loss", got[0].Context)
	}
	if n := len(h.events(model.Death)); n != 0 {
		t.Fatalf("%d deaths with the port closed 1.5 s", n)
	}

	// Closed 7.5 s: three failed attempts, then the open.
	h.step()
	lastRead := h.clk.Now()
	h.port.fail()
	h.step()
	h.opener.then(ErrPortMissing, ErrPortMissing, ErrPortMissing)
	h.stepUntil(100, func() bool { return len(h.events(model.Death)) == 2 })
	deaths := h.events(model.Death)
	for _, d := range deaths {
		if !d.TS.Equal(lastRead.Add(runTolerance)) {
			t.Fatalf("death %+v, want at %v", d, lastRead.Add(runTolerance))
		}
	}
	if h.port.isOpen() {
		t.Fatal("port open before the backoff ended")
	}
	h.stepUntil(100, func() bool { return len(h.events(model.Birth)) == 2 })
	births := h.events(model.Birth)
	if names := []string{births[0].Device, births[1].Device}; !slices.Contains(names, "a") || !slices.Contains(names, "b") {
		t.Fatalf("births = %+v", births)
	}
	if h.readErrors(KindPortLost) != 2 || h.readErrors(KindOpen) != 4 {
		t.Fatalf("read errors = %v", h.in.Health().ReadErrors)
	}
}

func TestSilenceDoesNotKill(t *testing.T) {
	h := newRunHarness(t, nil)
	h.start()
	h.advance(10 * runTolerance)
	if n := len(h.events(model.Death)); n != 0 {
		t.Fatalf("%d deaths of a silent instrument on an open port", n)
	}
}

func TestSilenceKillsWithRequests(t *testing.T) {
	h := newRunHarness(t, map[string]any{"requests": []map[string]any{{"command": "?\r", "every": "1s"}}})
	h.start()
	h.advance(runTolerance)
	if n := len(h.events(model.Death)); n != 2 {
		t.Fatalf("%d deaths after %v without a frame, want 2", n, runTolerance)
	}
	if h.readErrors(KindNoResponse) == 0 {
		t.Fatal("no no_response counted")
	}
	h.port.feed("01A1.0\r")
	h.step()
	if n := len(h.events(model.Birth)); n != 2 {
		t.Fatalf("%d births after a frame, want 2", n)
	}
	if len(h.samples()) != 1 {
		t.Fatalf("samples = %+v", h.samples())
	}
}

func TestRequestsWritten(t *testing.T) {
	h := newRunHarness(t, map[string]any{"requests": []map[string]any{
		{"command": "A\r", "every": "1s"},
		{"command": "B\r", "every": "1s"},
	}})
	h.start()

	h.step()
	if got := h.port.written(); !slices.Equal(got, []string{"A\r"}) {
		t.Fatalf("writes = %q", got)
	}
	h.advance(time.Second)
	if got := h.port.written(); !slices.Equal(got, []string{"A\r", "B\r"}) {
		t.Fatalf("writes = %q", got)
	}
	if h.readErrors(KindNoResponse) != 1 {
		t.Fatalf("no_response = %d, want 1", h.readErrors(KindNoResponse))
	}
	h.port.feed("02A3\r")
	h.step()
	if got := h.port.written(); !slices.Equal(got, []string{"A\r", "B\r", "A\r"}) {
		t.Fatalf("writes = %q", got)
	}
	if h.readErrors(KindNoResponse) != 1 {
		t.Fatalf("no_response = %d, want 1", h.readErrors(KindNoResponse))
	}

	h.port.fail()
	h.step()
	h.opener.then(ErrPortMissing, ErrPortMissing)
	for range 100 {
		if h.port.isOpen() {
			break
		}
		if n := len(h.port.written()); n != 3 {
			t.Fatalf("%d writes with the port closed, want 3", n)
		}
		h.step()
	}
	if !h.port.isOpen() {
		t.Fatal("port not open again")
	}
	h.stepUntil(2, func() bool { return len(h.port.written()) == 4 })
	if got := h.port.written(); got[3] != "A\r" {
		t.Fatalf("first write after reopening = %q, want A", got[3])
	}
}

func TestWriteErrorLosesThePort(t *testing.T) {
	h := newRunHarness(t, map[string]any{"requests": []map[string]any{{"command": "?\r", "every": "1s"}}})
	h.start()
	h.port.failWrite()
	h.step()
	if h.readErrors(KindPortLost) != 1 || h.in.Health().Connected {
		t.Fatalf("read errors %v", h.in.Health().ReadErrors)
	}
}

func TestCloseStopsPromptly(t *testing.T) {
	h := newRunHarness(t, nil)
	h.start()
	h.port.feed("01A1.0\r")

	done := make(chan struct{})
	go func() {
		_ = h.in.Close()
		close(done)
	}()
	for closed := false; !closed; {
		select {
		case <-done:
			closed = true
		default:
			h.clk.Advance(ReadTimeout)
			runtime.Gosched()
		}
	}
	n := h.recordCount()
	h.port.feed("01A2.0\r")
	h.clk.Advance(time.Minute)
	if h.recordCount() != n {
		t.Fatal("emit called after Close returned")
	}
	if h.port.isOpen() {
		t.Fatal("port left open after Close")
	}
	if err := h.in.Start(t.Context(), h.emit); err == nil {
		t.Fatal("Start after Close succeeded")
	}
}

func TestStartTwiceRefused(t *testing.T) {
	h := newRunHarness(t, nil)
	h.start()
	if err := h.in.Start(t.Context(), h.emit); err == nil || !strings.Contains(err.Error(), `serial input "gauges"`) {
		t.Fatalf("second Start = %v", err)
	}
}

func TestDuplicateAddressWarned(t *testing.T) {
	cfg := config.Input{
		ID:             "gauges",
		Driver:         config.DriverSerial,
		Connection:     gaugeConnection(nil),
		DeathTolerance: runTolerance,
		Devices: []config.Device{
			{Device: "a", Points: []config.Point{gaugePoint("w", `^01A(.+)$`, "float64")}},
			{Device: "b", Points: []config.Point{gaugePoint("w", `^01A(.+)$`, "float64")}},
		},
	}
	h := newRunHarnessFor(t, cfg)
	lines := h.logs.at(t, "WARN", msgDuplicateAddress)
	if len(lines) != 1 || lines[0]["point"] != "devices.1.points.0" || lines[0]["input"] != "gauges" {
		t.Fatalf("duplicate address warnings = %v", lines)
	}
}

func TestCloseLogsNoDropOfAnExpiredContext(t *testing.T) {
	h := newRunHarness(t, map[string]any{"context_ttl": "1s"})
	h.start()
	h.step()
	h.port.feed("SN7\r")
	h.step()
	h.advance(2 * time.Second)
	h.close()
	if dropped := h.logs.at(t, "INFO", msgContextDropped); len(dropped) != 0 {
		t.Fatalf("context dropped logged with every key expired: %v", dropped)
	}
}

func TestSteadyUnmatchedFramesStayAtDebug(t *testing.T) {
	h := newRunHarness(t, nil)
	h.start()
	h.step()
	for range 3 {
		h.port.feed("99\r98\r")
		h.step()
		h.clk.Advance(unmatchedWindow)
		h.clk.BlockUntil(t, idleTimers)
	}
	h.port.feed("97\r")
	h.close()

	summaries := h.logs.at(t, "DEBUG", msgUnmatchedSummary)
	if len(summaries) < 3 {
		t.Fatalf("%d summaries at debug over three windows, want at least 3", len(summaries))
	}
	for _, l := range h.logs.lines(t) {
		if l["level"] != "DEBUG" && l["msg"] != msgContextDropped {
			t.Fatalf("a record above debug for unmatched frames: %v", l)
		}
	}
}

func TestFlappingPortBacksOff(t *testing.T) {
	h := newRunHarness(t, nil)
	h.opener.loseNext(5)
	h.start()

	// Every open succeeds and the first read fails: the waits still grow,
	// 0.5 s, 1 s, 2 s, 4 s, 8 s, as for an open that fails.
	if h.opener.count() != 1 || h.readErrors(KindPortLost) != 1 {
		t.Fatalf("after start: opens %d, port_lost %d", h.opener.count(), h.readErrors(KindPortLost))
	}
	for i, wait := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second} {
		h.advance(wait - ReadTimeout)
		if h.opener.count() != i+1 {
			t.Fatalf("open %d came before its wait of %v", i+2, wait)
		}
		h.step()
		if h.opener.count() != i+2 {
			t.Fatalf("opens = %d after a wait of %v, want %d", h.opener.count(), wait, i+2)
		}
	}
	if got := h.readErrors(KindPortLost); got != 5 {
		t.Fatalf("port_lost = %d, want 5", got)
	}
	if lost := h.logs.at(t, "WARN", msgPortLost); len(lost) != 1 {
		t.Fatalf("port lost warned %d times in one episode, want 1", len(lost))
	}

	// The sixth open holds; a frame ends the episode and resets the backoff.
	h.advance(8 * time.Second)
	if h.opener.count() != 6 || !h.port.isOpen() {
		t.Fatalf("opens = %d, open %v", h.opener.count(), h.port.isOpen())
	}
	h.step()
	h.port.feed("01A1.0\r")
	h.step()
	h.port.fail()
	h.step()
	if lost := h.logs.at(t, "WARN", msgPortLost); len(lost) != 2 {
		t.Fatalf("port lost warned %d times after a frame, want 2", len(lost))
	}
	h.advance(500 * time.Millisecond)
	if h.opener.count() != 7 {
		t.Fatalf("opens = %d, want 7 after a reset backoff of 0.5 s", h.opener.count())
	}
}

func TestAPortOpenForBackoffMaxResetsTheBackoff(t *testing.T) {
	h := newRunHarness(t, nil)
	h.opener.loseNext(3)
	h.start()
	h.advance(500*time.Millisecond + time.Second)
	if h.opener.count() != 3 {
		t.Fatalf("opens = %d, want 3", h.opener.count())
	}
	// The fourth open, after 2 s, holds for BackoffMax without a frame.
	h.advance(2 * time.Second)
	if h.opener.count() != 4 || !h.port.isOpen() {
		t.Fatalf("opens = %d, open %v", h.opener.count(), h.port.isOpen())
	}
	h.advance(driverkit.BackoffMax)
	h.port.fail()
	h.step()
	h.advance(500 * time.Millisecond)
	if h.opener.count() != 5 {
		t.Fatalf("opens = %d, want 5 after a reset backoff of 0.5 s", h.opener.count())
	}
}

func TestGapFrameStampedAtItsLastByte(t *testing.T) {
	h := newRunHarness(t, map[string]any{"framing": map[string]any{"gap": "50ms"}})
	h.start()
	h.step()
	h.step()
	h.port.feed("01A1.5")
	h.step()
	lastByte := h.clk.Now()
	h.stepUntil(5, func() bool { return len(h.samples()) == 1 })
	if s := h.samples()[0]; s.Value.Float != 1.5 || !s.TS.Equal(lastByte) {
		t.Fatalf("sample = %+v, want 1.5 at %v", s, lastByte)
	}
}

func TestAGaugeStreamingAcrossTheOpenGivesNoHalfFrame(t *testing.T) {
	h := newRunHarness(t, nil)
	h.start()
	h.port.feed("5.250\r01A+1.000\r")
	h.step()
	got := h.samples()
	if len(got) != 1 || got[0].Value.Float != 1.0 {
		t.Fatalf("samples = %+v, want only 1.0", got)
	}
	if h.readErrors(kindIncompleteFrame) != 1 || h.readErrors(KindUnmatched) != 0 {
		t.Fatalf("read errors = %v", h.in.Health().ReadErrors)
	}
}
