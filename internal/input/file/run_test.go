package file

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

// TestStableFileIsEmittedThenCommittedOneScanLater: a report is read once
// it has been unchanged for stable_for, its samples carry its context and
// its modification time, and it reaches done/ and the ledger only at the
// next scan.
func TestStableFileIsEmittedThenCommittedOneScanLater(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("report.csv", report, fileTime)
	h.start()

	h.scans(2)
	if got := h.samples(); len(got) != 0 {
		t.Fatalf("samples before stable_for = %d, want 0", len(got))
	}

	h.scan()
	samples := h.samples()
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2 (bore and serial)", len(samples))
	}
	for _, s := range samples {
		if !s.TS.Equal(fileTime) || !reflect.DeepEqual(s.Context, &model.Context{Serial: "S-1"}) {
			t.Errorf("sample %s: ts %v context %+v, want %v and serial S-1", s.Signal, s.TS, s.Context, fileTime)
		}
	}
	if !h.exists("report.csv") || h.exists("done/report.csv") || h.inLedger(report, fileTime) {
		t.Fatal("the file was committed in the scan that emitted it")
	}

	h.scan()
	if h.exists("report.csv") || !h.exists("done/report.csv") {
		t.Fatal("the file is not in done/ one scan after the emit")
	}
	if !h.inLedger(report, fileTime) {
		t.Fatal("the file is not in the ledger one scan after the emit")
	}

	h.scans(3)
	if got := h.readLog(); !slices.Equal(got, []string{"report.csv"}) {
		t.Fatalf("reads = %v, want one", got)
	}
}

// TestRestartBeforeTheCommitReadsTheFileAgain: an input closed between the
// emit and the commit leaves the file uncommitted; the next input on the same
// data dir emits the same samples and commits them.
func TestRestartBeforeTheCommitReadsTheFileAgain(t *testing.T) {
	first := newRunHarness(t, harnessSetup{})
	first.write("report.csv", report, fileTime)
	first.start()
	first.scans(3)
	before := first.samples()
	if len(before) != 2 {
		t.Fatalf("first input samples = %d, want 2", len(before))
	}
	first.close()
	if !first.exists("report.csv") || first.inLedger(report, fileTime) {
		t.Fatal("the closed input committed its pending file")
	}

	second := newRunHarness(t, harnessSetup{dir: first.dir, dataDir: first.dataDir})
	second.start()
	second.scans(3)
	if after := second.samples(); !reflect.DeepEqual(after, before) {
		t.Fatalf("samples after the restart =\n%+v\nwant\n%+v", after, before)
	}
	second.scan()
	if !second.exists("done/report.csv") || !second.inLedger(report, fileTime) {
		t.Fatal("the second input did not commit the file")
	}
}

// TestDuplicateCopyGoesToDoneWithoutSamples: a byte-identical copy with the
// same modification time is a duplicate, moved at once; the same bytes with
// another modification time are a new report.
func TestDuplicateCopyGoesToDoneWithoutSamples(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("report.csv", report, fileTime)
	h.start()
	h.scans(4)
	if !h.inLedger(report, fileTime) {
		t.Fatal("the first report was not committed")
	}

	h.write("copy.csv", report, fileTime)
	h.scans(3)
	if got := len(h.samples()); got != 2 {
		t.Fatalf("samples after the copy = %d, want still 2", got)
	}
	if h.exists("copy.csv") || !h.exists("done/copy.csv") {
		t.Fatal("the duplicate was not moved to done/ in the scan that found it")
	}

	later := fileTime.Add(time.Minute)
	h.write("again.csv", report, later)
	h.scans(3)
	samples := h.samples()
	if len(samples) != 4 || !samples[3].TS.Equal(later) {
		t.Fatalf("samples after a new modification time = %d, want 4 with the new time", len(samples))
	}
}

// TestKeepReadsAFileOnceAndKeepsItsEntryWhilePresent: with keep the file is
// never read again while unchanged, its ledger entry outlives the retention
// while the file is on disk and expires once it is gone longer than that.
func TestKeepReadsAFileOnceAndKeepsItsEntryWhilePresent(t *testing.T) {
	h := newRunHarness(t, harnessSetup{conn: map[string]any{"after_read": "keep", "seen_retention": "1h"}})
	h.write("report.csv", report, fileTime)
	h.start()
	h.scans(4)
	h.scans(10)
	if got := h.readLog(); !slices.Equal(got, []string{"report.csv"}) {
		t.Fatalf("reads = %v, want one", got)
	}
	if !h.exists("report.csv") || h.exists("done") {
		t.Fatal("keep moved the file")
	}

	h.scanAfter(2 * time.Hour)
	if !h.inLedger(report, fileTime) {
		t.Fatal("the entry expired while the file is present")
	}

	if err := os.Remove(filepath.Join(h.dir, "report.csv")); err != nil {
		t.Fatal(err)
	}
	h.scan()
	h.scanAfter(30 * time.Minute)
	if !h.inLedger(report, fileTime) {
		t.Fatal("the entry expired before the retention")
	}
	h.scanAfter(31 * time.Minute)
	if h.inLedger(report, fileTime) {
		t.Fatal("the entry outlived the retention after the file was gone")
	}
}

// TestCommittedKeepFilesDoNotStarveANewOne: 150 kept files do not count
// towards max_files_per_scan, so a new file is read as soon as it is ready.
func TestCommittedKeepFilesDoNotStarveANewOne(t *testing.T) {
	h := newRunHarness(t, harnessSetup{conn: map[string]any{"after_read": "keep", "max_files_per_scan": 100}})
	for i := range 150 {
		h.write(fmt.Sprintf("old-%03d.csv", i), fmt.Sprintf("Serial,Bore\nS-%d,1\n", i), fileTime.Add(time.Duration(i)*time.Second))
	}
	h.start()
	h.scans(5)
	if got := len(h.readLog()); got != 150 {
		t.Fatalf("reads of the old files = %d, want 150", got)
	}

	h.write("new.csv", report, runEpoch)
	h.scans(2)
	if got := len(h.readLog()); got != 150 {
		t.Fatalf("new file read before stable_for: reads = %d", got)
	}
	h.scan()
	if got := h.readLog(); len(got) != 151 || got[150] != "new.csv" {
		t.Fatalf("the new file was not read in the scan it became ready: last reads %v", got[140:])
	}
}

// TestMaxFilesPerScanReadsOldestFirst: five ready files with a cap of two are
// read two, two and one, oldest first.
func TestMaxFilesPerScanReadsOldestFirst(t *testing.T) {
	h := newRunHarness(t, harnessSetup{conn: map[string]any{"max_files_per_scan": 2}})
	for i, name := range []string{"e.csv", "d.csv", "c.csv", "b.csv", "a.csv"} {
		h.write(name, fmt.Sprintf("Serial,Bore\nS-%d,1\n", i), fileTime.Add(time.Duration(i)*time.Minute))
	}
	h.start()
	h.scans(2)

	for _, want := range [][]string{{"e.csv", "d.csv"}, {"e.csv", "d.csv", "c.csv", "b.csv"}, {"e.csv", "d.csv", "c.csv", "b.csv", "a.csv"}} {
		h.scan()
		if got := h.readLog(); !slices.Equal(got, want) {
			t.Fatalf("reads = %v, want %v", got, want)
		}
	}
}

// TestMoveToATakenNameAddsASuffix: done/report.csv taken gives
// done/report-1.csv and leaves the other file alone.
func TestMoveToATakenNameAddsASuffix(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	if err := os.Mkdir(filepath.Join(h.dir, "done"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAt(t, filepath.Join(h.dir, "done", "report.csv"), "older", fileTime)
	h.write("report.csv", report, fileTime)
	h.start()
	h.scans(4)

	got, err := os.ReadFile(filepath.Join(h.dir, "done", "report-1.csv"))
	if err != nil || string(got) != report {
		t.Fatalf("done/report-1.csv = %q, %v; want the report", got, err)
	}
	if older, _ := os.ReadFile(filepath.Join(h.dir, "done", "report.csv")); string(older) != "older" {
		t.Fatalf("done/report.csv = %q, want it untouched", older)
	}
}

// TestFreeNameCountsUp: the suffix counts up past every taken name, before
// the extension.
func TestFreeNameCountsUp(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"report.csv", "report-1.csv", "report-2.csv", "plain"} {
		writeAt(t, filepath.Join(dir, name), "x", fileTime)
	}
	for name, want := range map[string]string{"report.csv": "report-3.csv", "plain": "plain-1", "new.csv": "new.csv"} {
		got, err := freeName(dir, name)
		if err != nil || got != filepath.Join(dir, want) {
			t.Errorf("freeName(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}

// TestMoveFailureIsCountedOnceAndTheFileReadOnce: a file that cannot be
// moved counts move_failed once over many scans and is never read again.
func TestMoveFailureIsCountedOnceAndTheFileReadOnce(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.setRenameErr(errors.New("read-only file system"))
	h.write("report.csv", report, fileTime)
	h.start()
	h.scans(4)
	h.scans(10)

	if got := h.readErrors(KindMoveFailed); got != 1 {
		t.Fatalf("move_failed = %d, want 1", got)
	}
	if got := h.readLog(); !slices.Equal(got, []string{"report.csv"}) {
		t.Fatalf("reads = %v, want one", got)
	}
	if got := len(h.samples()); got != 2 {
		t.Fatalf("samples = %d, want 2", got)
	}
	if !h.exists("report.csv") || !h.inLedger(report, fileTime) {
		t.Fatal("the unmovable file is not in place and in the ledger")
	}

	h.setRenameErr(nil)
	h.scan()
	if h.exists("report.csv") || !h.exists("done/report.csv") {
		t.Fatal("the move was not retried once renames work again")
	}
	if got := h.readErrors(KindMoveFailed); got != 1 {
		t.Fatalf("move_failed after the retry = %d, want 1", got)
	}
}

// TestMissingDirectoryIsDeathThenBirth: Start succeeds without the directory,
// every device dies after the tolerance, and the first listing that works is
// their birth.
func TestMissingDirectoryIsDeathThenBirth(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "share")
	h := newRunHarness(t, harnessSetup{dir: dir})
	h.start()

	h.scans(4)
	if got := h.events(model.Death); len(got) != 0 {
		t.Fatalf("deaths before the tolerance = %v", got)
	}
	h.scan()
	if got := h.events(model.Death); got["cmm"] != 1 || got["gauge"] != 1 {
		t.Fatalf("deaths = %v, want one per device", got)
	}
	if hl := h.in.Health(); hl.Connected || hl.ReadErrors[KindListing] == 0 {
		t.Fatalf("health = %+v, want disconnected with listing errors", hl)
	}

	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.scan()
	if got := h.events(model.Birth); got["cmm"] != 1 || got["gauge"] != 1 {
		t.Fatalf("births = %v, want one per device", got)
	}
	if !h.in.Health().Connected {
		t.Fatal("not connected after a successful listing")
	}
}

// TestRefusedFilesGoToError: a file above max_file_size and a malformed file
// are counted by kind, give no samples and are moved to error/.
func TestRefusedFilesGoToError(t *testing.T) {
	for name, tt := range map[string]struct {
		conn    map[string]any
		content string
		kind    string
		reads   int
	}{
		"too large": {conn: map[string]any{"max_file_size": 10}, content: report, kind: KindFileTooLarge, reads: 0},
		"malformed": {content: "Bore\n1\n2\n", kind: KindMalformedFile, reads: 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRunHarness(t, harnessSetup{conn: tt.conn})
			h.write("report.csv", tt.content, fileTime)
			h.start()
			h.scans(4)

			if got := h.readErrors(tt.kind); got != 1 {
				t.Fatalf("%s = %d, want 1", tt.kind, got)
			}
			if got := len(h.samples()); got != 0 {
				t.Fatalf("samples = %d, want 0", got)
			}
			if h.exists("report.csv") || !h.exists("error/report.csv") {
				t.Fatal("the refused file is not in error/")
			}
			if got := len(h.readLog()); got != tt.reads {
				t.Fatalf("reads = %d, want %d", got, tt.reads)
			}
		})
	}
}

// TestTemporaryNameRenamedIsReadOnce (Review Focus): report.tmp is never
// read; renamed to report.csv with its modification time it is read once,
// after stable_for, and that time is its timestamp.
func TestTemporaryNameRenamedIsReadOnce(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("report.tmp", report, fileTime)
	h.start()
	h.scans(5)
	if got := h.readLog(); len(got) != 0 {
		t.Fatalf("reads of the temporary name = %v", got)
	}

	if err := os.Rename(filepath.Join(h.dir, "report.tmp"), filepath.Join(h.dir, "report.csv")); err != nil {
		t.Fatal(err)
	}
	h.scans(2)
	if got := h.readLog(); len(got) != 0 {
		t.Fatalf("read before stable_for: %v", got)
	}
	h.scan()
	h.scans(5)

	if got := h.readLog(); !slices.Equal(got, []string{"report.csv"}) {
		t.Fatalf("reads = %v, want report.csv once", got)
	}
	for _, s := range h.samples() {
		if !s.TS.Equal(fileTime) {
			t.Errorf("sample %s ts = %v, want the modification time %v", s.Signal, s.TS, fileTime)
		}
	}
}

// TestShareRemovedAndRestored (Review Focus): while the directory is gone no
// sample is emitted, no file moves and the ledger does not change; the
// devices die after the tolerance, are born at the first listing, and the
// files present are then handled normally.
func TestShareRemovedAndRestored(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("first.csv", report, fileTime)
	h.start()
	h.scans(4)
	if !h.inLedger(report, fileTime) {
		t.Fatal("first.csv was not committed")
	}

	second := "Serial,Bore\nS-2,13\n"
	h.write("second.csv", second, fileTime)
	h.scans(3)
	if got := len(h.samples()); got != 4 {
		t.Fatalf("samples = %d, want 4 with second.csv pending", got)
	}
	ledger := h.ledgerBytes()

	away := h.dir + "-away"
	if err := os.Rename(h.dir, away); err != nil {
		t.Fatal(err)
	}
	writeAt(t, filepath.Join(away, "third.csv"), "Serial,Bore\nS-3,14\n", fileTime)
	h.scans(6)
	if got := h.events(model.Death); got["cmm"] != 1 || got["gauge"] != 1 {
		t.Fatalf("deaths = %v, want one per device", got)
	}
	if got := len(h.samples()); got != 4 {
		t.Fatalf("samples while gone = %d, want 4", got)
	}
	if got := h.ledgerBytes(); string(got) != string(ledger) {
		t.Fatal("the ledger changed while the share was gone")
	}
	if got := h.readErrors(KindMoveFailed); got != 0 {
		t.Fatalf("move_failed while gone = %d", got)
	}
	if _, err := os.Stat(filepath.Join(away, "second.csv")); err != nil {
		t.Fatalf("second.csv moved while the share was gone: %v", err)
	}

	if err := os.Rename(away, h.dir); err != nil {
		t.Fatal(err)
	}
	h.scan()
	if got := h.events(model.Birth); got["cmm"] != 1 || got["gauge"] != 1 {
		t.Fatalf("births = %v, want one per device", got)
	}
	if !h.exists("done/second.csv") || !h.inLedger(second, fileTime) {
		t.Fatal("second.csv was not committed after the share came back")
	}
	h.scans(3)
	if got := h.readLog(); !slices.Equal(got, []string{"first.csv", "second.csv", "third.csv"}) {
		t.Fatalf("reads = %v, want each file once", got)
	}
}

// TestPendingFileGoneOrChangedIsNotMoved: a pending file deleted before its
// commit is recorded without a move failure; one changed after its read is
// not moved and is read again in its new state.
func TestPendingFileGoneOrChangedIsNotMoved(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("gone.csv", report, fileTime)
	h.write("changed.csv", "Serial,Bore\nS-2,13\n", fileTime)
	h.start()
	h.scans(3)
	if got := len(h.samples()); got != 4 {
		t.Fatalf("samples = %d, want 4", got)
	}

	if err := os.Remove(filepath.Join(h.dir, "gone.csv")); err != nil {
		t.Fatal(err)
	}
	changed := "Serial,Bore\nS-2,13.5\n"
	h.write("changed.csv", changed, fileTime.Add(time.Minute))
	h.scan()
	if got := h.readErrors(KindMoveFailed); got != 0 {
		t.Fatalf("move_failed = %d, want 0", got)
	}
	if !h.inLedger(report, fileTime) {
		t.Fatal("the deleted file was not recorded")
	}
	if !h.exists("changed.csv") || h.exists("done/changed.csv") {
		t.Fatal("the changed file was moved without being read in its new state")
	}

	h.scans(3)
	if got := h.readLog(); !slices.Equal(got, []string{"changed.csv", "gone.csv", "changed.csv"}) {
		t.Fatalf("reads = %v, want changed.csv read again", got)
	}
	if !h.exists("done/changed.csv") || !h.inLedger(changed, fileTime.Add(time.Minute)) {
		t.Fatal("the changed file was not committed after its second read")
	}
}

// TestReusedFileNameDoesNotKeepOldEntriesAlive: an exporter that writes the
// same name every time must not keep the ledger entries of its older reports
// alive; an entry is present only while a file of that name has its
// modification time.
func TestReusedFileNameDoesNotKeepOldEntriesAlive(t *testing.T) {
	h := newRunHarness(t, harnessSetup{conn: map[string]any{"after_read": "keep", "seen_retention": "1h"}})
	h.write("result.csv", report, fileTime)
	h.start()
	h.scans(4)
	if !h.inLedger(report, fileTime) {
		t.Fatal("the first report was not committed")
	}

	newer := fileTime.Add(time.Minute)
	next := report + "\n"
	h.write("result.csv", next, newer)
	h.scans(4)
	if !h.inLedger(next, newer) {
		t.Fatal("the second report was not committed")
	}

	h.scanAfter(30 * time.Minute)
	if !h.inLedger(report, fileTime) {
		t.Fatal("the older entry expired before the retention")
	}
	h.scanAfter(31 * time.Minute)
	if h.inLedger(report, fileTime) {
		t.Fatal("the older entry outlived the retention while a newer file reuses its name")
	}
	if !h.inLedger(next, newer) {
		t.Fatal("the entry of the file on disk expired")
	}
}
