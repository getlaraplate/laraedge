package file

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/model"
)

// runEpoch is the fake clock's start in the input tests.
var runEpoch = time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)

// fileTime is the modification time of the test reports: an hour before the
// clock starts, so the watcher sees them unchanged from the first scan.
var fileTime = runEpoch.Add(-time.Hour)

const (
	// runInterval and runStable are the scan period and stable_for of the
	// test inputs: a file listed at the first scan is ready at the third.
	runInterval = 2 * time.Second
	runStable   = 4 * time.Second
	// runTolerance is the death tolerance of the test inputs.
	runTolerance = 10 * time.Second
	// idleTimers is the number of clock timers of a started input at rest:
	// the lifecycle's next check and the wait for the next scan.
	idleTimers = 2
	// report is a one-row wide CSV with a context column.
	report = "Serial,Bore\nS-1,12.5\n"
)

// runHarness runs one file input on a fake clock and records what it emits
// and which files it reads.
type runHarness struct {
	t       *testing.T
	clk     *clock.Fake
	dir     string
	dataDir string
	cfg     config.Input
	in      *Input

	mu        sync.Mutex
	records   []model.Record
	reads     []string
	renameErr error
	// renameErrs makes the rename of the named files of the watched
	// directory fail.
	renameErrs map[string]error
}

// harnessSetup says how a runHarness is built.
type harnessSetup struct {
	// conn are connection fields added to directory, format and timing.
	conn map[string]any
	// dir is the watched directory; empty: a new one, created.
	dir string
	// dataDir is the data directory; empty: a new one.
	dataDir string
}

// newRunHarness builds the input of setup with two devices, cmm (bore, from
// Bore) and gauge (serial, from Serial), and the context serial from Serial.
func newRunHarness(t *testing.T, setup harnessSetup) *runHarness {
	t.Helper()

	h := &runHarness{t: t, clk: clock.NewFake(runEpoch), dir: setup.dir, dataDir: setup.dataDir}
	if h.dir == "" {
		h.dir = filepath.Join(t.TempDir(), "watched")
		if err := os.Mkdir(h.dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if h.dataDir == "" {
		h.dataDir = t.TempDir()
	}

	fields := map[string]any{
		"directory":  h.dir,
		"format":     "csv",
		"interval":   runInterval.String(),
		"stable_for": runStable.String(),
		"context":    map[string]any{"serial": "Serial"},
	}
	for k, v := range setup.conn {
		fields[k] = v
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = config.Input{
		ID:             "cmm-line",
		Driver:         config.DriverFile,
		Connection:     raw,
		DeathTolerance: runTolerance,
		Devices: []config.Device{
			{Device: "cmm", Points: []config.Point{eventPoint("bore", "Bore", "float64")}},
			{Device: "gauge", Points: []config.Point{eventPoint("serial", "Serial", "string")}},
		},
	}

	in, err := newInput(h.cfg, h.clk, Options{DataDir: h.dataDir, Logger: slog.New(slog.DiscardHandler)}, deps{
		readFile: func(path string) ([]byte, error) {
			h.mu.Lock()
			h.reads = append(h.reads, filepath.Base(path))
			h.mu.Unlock()

			return os.ReadFile(path)
		},
		rename: func(oldPath, newPath string) error {
			h.mu.Lock()
			err := h.renameErr
			if named, ok := h.renameErrs[filepath.Base(oldPath)]; ok {
				err = named
			}
			h.mu.Unlock()
			if err != nil {
				return err
			}

			return os.Rename(oldPath, newPath)
		},
	})
	if err != nil {
		t.Fatalf("newInput: %v", err)
	}
	h.in = in

	return h
}

// eventPoint is an event point of type typ at the column address.
func eventPoint(signal, address, typ string) config.Point {
	return config.Point{Signal: signal, Address: address, Type: typ, Read: config.Read{Mode: config.ReadEvent}, Priority: model.Critical}
}

// start starts the input and waits until it rests.
func (h *runHarness) start() {
	h.t.Helper()

	if err := h.in.Start(h.t.Context(), h.emit); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	h.t.Cleanup(h.close)
	h.clk.BlockUntil(h.t, idleTimers)
}

func (h *runHarness) close() {
	if err := h.in.Close(); err != nil {
		h.t.Errorf("Close: %v", err)
	}
}

func (h *runHarness) emit(r model.Record) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, r)
}

// scan lets one scan run and waits until the input rests again.
func (h *runHarness) scan() {
	h.t.Helper()

	h.scanAfter(runInterval)
}

// scans runs n scans.
func (h *runHarness) scans(n int) {
	h.t.Helper()

	for range n {
		h.scan()
	}
}

// scanAfter moves the clock d forward, which runs one scan when d is at
// least the interval, and waits until the input rests again.
func (h *runHarness) scanAfter(d time.Duration) {
	h.t.Helper()

	h.clk.Advance(d)
	h.clk.BlockUntil(h.t, idleTimers)
}

// setRenameErr makes every rename fail with err; nil restores them.
func (h *runHarness) setRenameErr(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.renameErr = err
}

// setRenameErrFor makes the rename of the file name fail with err; nil
// restores it.
func (h *runHarness) setRenameErrFor(name string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.renameErrs == nil {
		h.renameErrs = map[string]error{}
	}
	if err == nil {
		delete(h.renameErrs, name)

		return
	}
	h.renameErrs[name] = err
}

// samples returns the samples emitted so far.
func (h *runHarness) samples() []model.Sample {
	h.mu.Lock()
	defer h.mu.Unlock()

	var out []model.Sample
	for _, r := range h.records {
		if r.Sample != nil {
			out = append(out, *r.Sample)
		}
	}

	return out
}

// events returns how many events of kind each device has.
func (h *runHarness) events(kind model.EventKind) map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := map[string]int{}
	for _, r := range h.records {
		if r.Event != nil && r.Event.Kind == kind {
			out[r.Event.Device]++
		}
	}

	return out
}

// readLog returns the names of the files read so far, in order.
func (h *runHarness) readLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.reads)
}

// write writes a file in the watched directory with the modification time mod.
func (h *runHarness) write(name, content string, mod time.Time) {
	h.t.Helper()

	writeAt(h.t, filepath.Join(h.dir, name), content, mod)
}

// writeAt writes path with content and the modification time mod.
func writeAt(t *testing.T, path, content string, mod time.Time) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// exists reports whether the path relative to the watched directory exists.
func (h *runHarness) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(h.dir, filepath.FromSlash(rel)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		h.t.Fatal(err)
	}

	return err == nil
}

// inLedger reports whether the stored ledger holds content at mod.
func (h *runHarness) inLedger(content string, mod time.Time) bool {
	h.t.Helper()

	l, err := loadLedger(h.dataDir, h.cfg.ID, slog.New(slog.DiscardHandler))
	if err != nil {
		h.t.Fatal(err)
	}

	return l.Has(ledgerKey{Hash: sha256.Sum256([]byte(content)), ModTime: mod})
}

// ledgerBytes returns the stored ledger, nil when there is none.
func (h *runHarness) ledgerBytes() []byte {
	h.t.Helper()

	dir, name := ledgerPath(h.dataDir, h.cfg.ID)
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		h.t.Fatal(err)
	}

	return data
}

// readErrors returns the read error count of kind.
func (h *runHarness) readErrors(kind string) uint64 {
	return h.in.Health().ReadErrors[kind]
}
