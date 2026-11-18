package file

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/model"
)

// Q-DAS reports of the scanner tests: the description defines the two
// characteristics the harness points address, Bore and Serial; the values are
// one record without K0004, so its samples take the modification time, with
// the part ID K0014 the harness maps to the serial context.
var (
	qdasDescription = qdasLines("K0100 2", "K2001/1 Bore", "K2001/2 Serial")
	qdasValues      = qdasLines("12.5"+qs+"S-1", "K0014/0 P-1")
	qdasReport      = qdasDescription + qdasValues
)

// newQDASHarness is newRunHarness with format qdas, the serial context from
// K0014 and the connection fields conn.
func newQDASHarness(t *testing.T, conn map[string]any) *runHarness {
	t.Helper()

	fields := map[string]any{"format": "qdas", "context": map[string]any{"serial": "K0014"}}
	for k, v := range conn {
		fields[k] = v
	}

	return newRunHarness(t, harnessSetup{conn: fields})
}

// writePair writes name.dfd and name.dfx with the test description and values.
func (h *runHarness) writePair(name string, mod time.Time) {
	h.t.Helper()

	h.write(name+".dfd", qdasDescription, mod)
	h.write(name+".dfx", qdasValues, mod)
}

// pairKey is the ledger key of a pair (Q8): the SHA-256 of the 8-byte
// big-endian length of the .dfd, the .dfd, then the .dfx, at the .dfx's
// modification time.
func pairKey(dfd, dfx string, mod time.Time) ledgerKey {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(dfd)))
	sum := sha256.New()
	sum.Write(length[:])
	sum.Write([]byte(dfd))
	sum.Write([]byte(dfx))

	var key ledgerKey
	copy(key.Hash[:], sum.Sum(nil))
	key.ModTime = mod

	return key
}

// storedLedger returns the stored ledger.
func (h *runHarness) storedLedger() *ledger {
	h.t.Helper()

	l, err := loadLedger(h.dataDir, h.cfg.ID, slog.New(slog.DiscardHandler))
	if err != nil {
		h.t.Fatal(err)
	}

	return l
}

// checkQDASSamples checks the samples of one test report read at mod.
func checkQDASSamples(t *testing.T, samples []model.Sample, mod time.Time) {
	t.Helper()

	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2 (bore and serial)", len(samples))
	}
	for _, s := range samples {
		if s.Quality != model.Good || !s.TS.Equal(mod) || !reflect.DeepEqual(s.Context, &model.Context{Serial: "P-1"}) {
			t.Errorf("sample %s: quality %v ts %v context %+v, want good at %v with serial P-1", s.Signal, s.Quality, s.TS, s.Context, mod)
		}
	}
}

// TestQDASSingleFileIsReadOnceIntoDone: a .dfq is a report alone, read once
// and moved to done/ one scan later.
func TestQDASSingleFileIsReadOnceIntoDone(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.write("a.dfq", qdasReport, fileTime)
	h.start()
	h.scans(3)
	checkQDASSamples(t, h.samples(), fileTime)

	h.scans(5)
	if h.exists("a.dfq") || !h.exists("done/a.dfq") {
		t.Fatal("the .dfq is not in done/")
	}
	if got := h.readLog(); !slices.Equal(got, []string{"a.dfq"}) {
		t.Fatalf("reads = %v, want a.dfq once", got)
	}
	if !h.inLedger(qdasReport, fileTime) {
		t.Fatal("the .dfq is not in the ledger under its own hash")
	}
}

// TestQDASDescriptionOnlyFileIsReadWithoutSamples (Review Focus): a .dfq
// holding only the description is read, gives no sample and no read error,
// and goes to done/.
func TestQDASDescriptionOnlyFileIsReadWithoutSamples(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.write("a.dfq", qdasDescription, fileTime)
	h.start()
	h.scans(4)

	if got := len(h.samples()); got != 0 {
		t.Fatalf("samples = %d, want 0", got)
	}
	if got := h.in.Health().ReadErrors; len(got) != 0 {
		t.Fatalf("read errors = %v, want none", got)
	}
	if h.exists("a.dfq") || !h.exists("done/a.dfq") || h.exists("error") {
		t.Fatal("the description-only .dfq is not in done/")
	}
}

// TestQDASPairWrittenDfxFirstIsReadOnceBothStable (Review Focus): an exporter
// writes the .dfx, then the .dfd five seconds later. Nothing is read until
// both are stable; the pair is then read once, with the samples of the joined
// text, and both files end in done/.
func TestQDASPairWrittenDfxFirstIsReadOnceBothStable(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.write("a.dfx", qdasValues, runEpoch)
	h.start()
	h.scans(2)

	// The .dfx is stable at the next scan, the .dfd written now is first seen
	// there and stable two scans later.
	dfdTime := runEpoch.Add(5 * time.Second)
	h.scanAfter(time.Second)
	h.write("a.dfd", qdasDescription, dfdTime)
	h.scanAfter(runInterval - time.Second)
	if got := h.readLog(); len(got) != 0 {
		t.Fatalf("read with the .dfd just written: %v", got)
	}

	h.scan()
	if got := h.readLog(); len(got) != 0 {
		t.Fatalf("read before the .dfd was stable: %v", got)
	}
	h.scan()
	if got := h.readLog(); !slices.Equal(got, []string{"a.dfd", "a.dfx"}) {
		t.Fatalf("reads = %v, want the pair once both are stable", got)
	}
	checkQDASSamples(t, h.samples(), runEpoch)

	h.scans(5)
	if got := len(h.readLog()); got != 2 {
		t.Fatalf("reads = %d, want the pair read once", got)
	}
	for _, name := range []string{"a.dfd", "a.dfx"} {
		if h.exists(name) || !h.exists("done/"+name) {
			t.Fatalf("%s is not in done/", name)
		}
	}
}

// TestQDASPairMovesUnderOneSuffix: the two files of a pair take one suffix
// free for both, also when only one of the names is taken.
func TestQDASPairMovesUnderOneSuffix(t *testing.T) {
	for name, taken := range map[string][]string{
		"both taken":     {"a.dfd", "a.dfx"},
		"only dfx taken": {"a.dfx"},
		"only dfd taken": {"a.dfd"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newQDASHarness(t, nil)
			if err := os.Mkdir(filepath.Join(h.dir, "done"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, n := range taken {
				writeAt(t, filepath.Join(h.dir, "done", n), "older", fileTime)
			}
			h.writePair("a", fileTime)
			h.start()
			h.scans(4)

			for n, want := range map[string]string{"a-1.dfd": qdasDescription, "a-1.dfx": qdasValues} {
				got, err := os.ReadFile(filepath.Join(h.dir, "done", n))
				if err != nil || string(got) != want {
					t.Fatalf("done/%s = %q, %v; want the moved file", n, got, err)
				}
			}
			for _, n := range taken {
				if older, _ := os.ReadFile(filepath.Join(h.dir, "done", n)); string(older) != "older" {
					t.Fatalf("done/%s = %q, want it untouched", n, older)
				}
			}
		})
	}
}

// TestQDASFilesThatAreNotReportsAreNeverRead: a .dfd alone, a .dfx alone and,
// under qdas, a .csv are never read and never moved.
func TestQDASFilesThatAreNotReportsAreNeverRead(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.write("a.dfd", qdasDescription, fileTime)
	h.write("b.dfx", qdasValues, fileTime)
	h.write("c.csv", report, fileTime)
	h.start()
	h.scans(10)

	if got := h.readLog(); len(got) != 0 {
		t.Fatalf("reads = %v, want none", got)
	}
	if got := len(h.samples()); got != 0 {
		t.Fatalf("samples = %d, want 0", got)
	}
	for _, name := range []string{"a.dfd", "b.dfx", "c.csv"} {
		if !h.exists(name) {
			t.Fatalf("%s moved", name)
		}
	}
	if h.exists("done") || h.exists("error") {
		t.Fatal("a file that is not a report was moved")
	}
}

// TestQDASPairLedgerEntryAndDuplicate: the pair's ledger entry is named after
// the .dfx and keyed on both files; a copy of both with the same modification
// times is a duplicate, moved at once without samples.
func TestQDASPairLedgerEntryAndDuplicate(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.writePair("a", fileTime)
	h.start()
	h.scans(4)

	l := h.storedLedger()
	key := pairKey(qdasDescription, qdasValues, fileTime)
	if !l.Has(key) || len(l.entries) != 1 || l.entries[0].Name != "a.dfx" {
		t.Fatalf("ledger = %+v, want one entry for the pair named a.dfx", l.entries)
	}

	h.writePair("b", fileTime)
	h.scans(3)
	if got := len(h.samples()); got != 2 {
		t.Fatalf("samples after the copy = %d, want still 2", got)
	}
	for _, name := range []string{"b.dfd", "b.dfx"} {
		if h.exists(name) || !h.exists("done/"+name) {
			t.Fatalf("the duplicate %s was not moved to done/ in the scan that found it", name)
		}
	}
}

// TestQDASKeepReadsAPairOnceAndAChangedDfdAgain: with keep the pair is read
// once over ten scans; a change to its .dfd makes it a new candidate.
func TestQDASKeepReadsAPairOnceAndAChangedDfdAgain(t *testing.T) {
	h := newQDASHarness(t, map[string]any{"after_read": "keep"})
	h.writePair("a", fileTime)
	h.start()
	h.scans(3)
	h.scans(10)
	if got := h.readLog(); !slices.Equal(got, []string{"a.dfd", "a.dfx"}) {
		t.Fatalf("reads = %v, want the pair once", got)
	}
	if h.exists("done") {
		t.Fatal("keep moved the pair")
	}

	changed := qdasLines("K0100 2", "K2001/1 Bore", "K2001/2 Serial", "K2002/1 Bore diameter")
	h.write("a.dfd", changed, fileTime.Add(time.Minute))
	h.scans(3)
	if got := h.readLog(); !slices.Equal(got, []string{"a.dfd", "a.dfx", "a.dfd", "a.dfx"}) {
		t.Fatalf("reads = %v, want the pair read again after the .dfd changed", got)
	}
	h.scans(10)
	if got := len(h.readLog()); got != 4 {
		t.Fatalf("reads = %d, want the changed pair read once", got)
	}
	if got := len(h.samples()); got != 4 {
		t.Fatalf("samples = %d, want 4", got)
	}
	if !h.storedLedger().Has(pairKey(changed, qdasValues, fileTime)) {
		t.Fatal("the changed pair is not in the ledger")
	}
}

// TestQDASPairMoveFailingForTheDfdOnly: the .dfx is moved, the .dfd counts
// one move_failed over many scans and is moved, under the .dfx's suffix, once
// renames work again.
func TestQDASPairMoveFailingForTheDfdOnly(t *testing.T) {
	h := newQDASHarness(t, nil)
	if err := os.Mkdir(filepath.Join(h.dir, "done"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAt(t, filepath.Join(h.dir, "done", "a.dfx"), "older", fileTime)
	h.setRenameErrFor("a.dfd", errors.New("permission denied"))
	h.writePair("a", fileTime)
	h.start()
	h.scans(4)
	if h.exists("a.dfx") || !h.exists("done/a-1.dfx") || !h.exists("a.dfd") {
		t.Fatal("the .dfx was not moved, or the .dfd was")
	}

	h.scans(10)
	if got := h.readErrors(KindMoveFailed); got != 1 {
		t.Fatalf("move_failed = %d, want 1", got)
	}
	if got := len(h.readLog()); got != 2 {
		t.Fatalf("reads = %d, want the pair read once", got)
	}

	h.setRenameErrFor("a.dfd", nil)
	h.scan()
	if h.exists("a.dfd") || !h.exists("done/a-1.dfd") || h.exists("done/a.dfd") {
		t.Fatal("the .dfd was not moved under the pair's suffix once renames work again")
	}
	if got := h.readErrors(KindMoveFailed); got != 1 {
		t.Fatalf("move_failed after the retry = %d, want 1", got)
	}
}

// TestQDASMaxFilesPerScanCountsReports: with a cap of one, two pairs are read
// one per scan, oldest first.
func TestQDASMaxFilesPerScanCountsReports(t *testing.T) {
	h := newQDASHarness(t, map[string]any{"max_files_per_scan": 1})
	h.writePair("b", fileTime.Add(time.Minute))
	h.writePair("a", fileTime)
	h.start()
	h.scans(2)

	for _, want := range [][]string{{"a.dfd", "a.dfx"}, {"a.dfd", "a.dfx", "b.dfd", "b.dfx"}} {
		h.scan()
		if got := h.readLog(); !slices.Equal(got, want) {
			t.Fatalf("reads = %v, want %v", got, want)
		}
	}
}

// TestQDASRefusedPairGoesToErrorTogether: a pair above max_file_size or
// malformed is counted once and both files go to error/.
func TestQDASRefusedPairGoesToErrorTogether(t *testing.T) {
	for name, tt := range map[string]struct {
		conn  map[string]any
		dfd   string
		kind  string
		reads int
	}{
		"too large": {conn: map[string]any{"max_file_size": len(qdasDescription) - 1}, dfd: qdasDescription, kind: KindFileTooLarge, reads: 0},
		"malformed": {dfd: qdasLines("K0100 2", "K2001/1 Bore", "Kxyz"), kind: KindMalformedFile, reads: 2},
	} {
		t.Run(name, func(t *testing.T) {
			h := newQDASHarness(t, tt.conn)
			h.write("a.dfd", tt.dfd, fileTime)
			h.write("a.dfx", qdasValues, fileTime)
			h.start()
			h.scans(4)

			if got := h.readErrors(tt.kind); got != 1 {
				t.Fatalf("%s = %d, want 1", tt.kind, got)
			}
			if got := len(h.samples()); got != 0 {
				t.Fatalf("samples = %d, want 0", got)
			}
			for _, n := range []string{"a.dfd", "a.dfx"} {
				if h.exists(n) || !h.exists("error/"+n) {
					t.Fatalf("%s is not in error/", n)
				}
			}
			if got := len(h.readLog()); got != tt.reads {
				t.Fatalf("reads = %d, want %d", got, tt.reads)
			}
		})
	}
}

// TestReportsOf groups the ready files into reports (Q8, Q9).
func TestReportsOf(t *testing.T) {
	at := func(name string, minutes int) fileInfo {
		return fileInfo{Name: name, Size: 1, ModTime: fileTime.Add(time.Duration(minutes) * time.Minute)}
	}
	pair := func(dfx, dfd fileInfo) fileReport {
		return fileReport{Primary: dfx, Companion: &dfd}
	}
	csv := Connection{Format: FormatCSV, Pattern: DefaultPattern}
	qdas := Connection{Format: FormatQDAS, Pattern: DefaultPattern}

	tests := []struct {
		name       string
		conn       Connection
		ready      []fileInfo
		listed     []fileInfo
		reports    []fileReport
		incomplete []fileInfo
	}{
		{
			name:    "csv: one report per ready file",
			conn:    csv,
			ready:   []fileInfo{at("a.csv", 0), at("b.dfd", 1)},
			listed:  []fileInfo{at("a.csv", 0), at("b.dfd", 1)},
			reports: []fileReport{{Primary: at("a.csv", 0)}, {Primary: at("b.dfd", 1)}},
		},
		{
			name:    "qdas: a .dfq alone, a pair, ordered by the primary",
			conn:    qdas,
			ready:   []fileInfo{at("b.dfd", 0), at("a.DFQ", 1), at("b.dfx", 2)},
			listed:  []fileInfo{at("b.dfd", 0), at("a.DFQ", 1), at("b.dfx", 2)},
			reports: []fileReport{{Primary: at("a.DFQ", 1)}, pair(at("b.dfx", 2), at("b.dfd", 0))},
		},
		{
			name:    "qdas: the extension pairs ignoring case",
			conn:    qdas,
			ready:   []fileInfo{at("A.DFX", 0), at("A.dfd", 0)},
			listed:  []fileInfo{at("A.DFX", 0), at("A.dfd", 0)},
			reports: []fileReport{pair(at("A.DFX", 0), at("A.dfd", 0))},
		},
		{
			name:       "qdas: the stem pairs exactly",
			conn:       qdas,
			ready:      []fileInfo{at("A.dfx", 0), at("a.dfd", 0)},
			listed:     []fileInfo{at("A.dfx", 0), at("a.dfd", 0)},
			incomplete: []fileInfo{at("A.dfx", 0)},
		},
		{
			name:   "qdas: a .dfx whose .dfd is listed but not stable waits",
			conn:   qdas,
			ready:  []fileInfo{at("a.dfx", 0)},
			listed: []fileInfo{at("a.dfx", 0), at("a.dfd", 5)},
		},
		{
			name:       "qdas: a .dfx without its .dfd is incomplete",
			conn:       qdas,
			ready:      []fileInfo{at("a.dfx", 0), at("b.dfd", 0)},
			listed:     []fileInfo{at("a.dfx", 0), at("b.dfd", 0)},
			incomplete: []fileInfo{at("a.dfx", 0)},
		},
		{
			name:       "qdas: the pattern must match the .dfd too",
			conn:       Connection{Format: FormatQDAS, Pattern: "*.dfx"},
			ready:      []fileInfo{at("a.dfx", 0)},
			listed:     []fileInfo{at("a.dfx", 0), at("a.dfd", 0)},
			incomplete: []fileInfo{at("a.dfx", 0)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reports, incomplete := reportsOf(tt.conn, tt.ready, tt.listed)
			if !reflect.DeepEqual(reports, tt.reports) || !reflect.DeepEqual(incomplete, tt.incomplete) {
				t.Fatalf("reportsOf = %+v, %+v; want %+v, %+v", reports, incomplete, tt.reports, tt.incomplete)
			}
		})
	}
}

// TestConsideredFiltersTheExtensionsUnderQDAS: under qdas only .dfq, .dfd and
// .dfx names, in any case, are considered; under csv the extension does not
// matter.
func TestConsideredFiltersTheExtensionsUnderQDAS(t *testing.T) {
	qdas := Connection{Format: FormatQDAS, Pattern: DefaultPattern}
	csv := Connection{Format: FormatCSV, Pattern: DefaultPattern}
	for name, want := range map[string]bool{"a.dfq": true, "a.DFD": true, "a.Dfx": true, "a.csv": false, "a.dfq.tmp": false, "dfq": false} {
		if got := considered(qdas, name); got != want {
			t.Errorf("considered(qdas, %q) = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]bool{"a.dfq": true, "a.csv": true, "a.txt": true, ".a.csv": false} {
		if got := considered(csv, name); got != want {
			t.Errorf("considered(csv, %q) = %v, want %v", name, got, want)
		}
	}
}

// TestFreeNamesTakesOneSuffixForAll: the suffix is the first free for every
// name.
func TestFreeNamesTakesOneSuffixForAll(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.dfx", "a-1.dfd", "b.dfd"} {
		writeAt(t, filepath.Join(dir, name), "x", fileTime)
	}
	for _, tt := range []struct{ names, want []string }{
		{[]string{"a.dfd", "a.dfx"}, []string{"a-2.dfd", "a-2.dfx"}},
		{[]string{"b.dfd", "b.dfx"}, []string{"b-1.dfd", "b-1.dfx"}},
		{[]string{"c.dfd", "c.dfx"}, []string{"c.dfd", "c.dfx"}},
		{[]string{"a.dfx"}, []string{"a-1.dfx"}},
	} {
		got, err := freeNames(dir, tt.names)
		want := make([]string, len(tt.want))
		for i, n := range tt.want {
			want[i] = filepath.Join(dir, n)
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("freeNames(%v) = %v, %v; want %v", tt.names, got, err, want)
		}
	}
}

// TestReadReport reads a single file and a pair, and refuses one above the
// size limit before or after the read.
func TestReadReport(t *testing.T) {
	dir := t.TempDir()
	writeAt(t, filepath.Join(dir, "a.dfd"), "\xef\xbb\xbfD", fileTime)
	writeAt(t, filepath.Join(dir, "a.dfx"), "\xef\xbb\xbfX", fileTime.Add(time.Minute))
	writeAt(t, filepath.Join(dir, "s.dfq"), "Q", fileTime)
	d := deps{readFile: os.ReadFile}
	dfd := fileInfo{Name: "a.dfd", Size: 4, ModTime: fileTime}
	dfx := fileInfo{Name: "a.dfx", Size: 4, ModTime: fileTime.Add(time.Minute)}
	single := fileReport{Primary: fileInfo{Name: "s.dfq", Size: 1, ModTime: fileTime}}

	data, key, tooLarge, err := readReport(d, dir, single, 10)
	if err != nil || tooLarge || string(data) != "Q" || key != (ledgerKey{Hash: sha256.Sum256([]byte("Q")), ModTime: fileTime}) {
		t.Fatalf("single = %q, %+v, %v, %v", data, key, tooLarge, err)
	}

	data, key, tooLarge, err = readReport(d, dir, fileReport{Primary: dfx, Companion: &dfd}, 10)
	if err != nil || tooLarge || string(data) != "\xef\xbb\xbfD\nX" || key != pairKey("\xef\xbb\xbfD", "\xef\xbb\xbfX", dfx.ModTime) {
		t.Fatalf("pair = %q, %+v, %v, %v", data, key, tooLarge, err)
	}

	if _, _, tooLarge, _ := readReport(d, dir, fileReport{Primary: dfx, Companion: &dfd}, 3); !tooLarge {
		t.Fatal("a pair with a file above the limit is not too large")
	}
	lyingDFD, lyingDFX := dfd, dfx
	lyingDFD.Size, lyingDFX.Size = 1, 1
	if _, _, tooLarge, _ := readReport(d, dir, fileReport{Primary: lyingDFX, Companion: &lyingDFD}, 3); !tooLarge {
		t.Fatal("a .dfd grown above the limit after the listing is not too large")
	}
	if _, _, _, err := readReport(d, dir, fileReport{Primary: fileInfo{Name: "gone.dfx"}, Companion: &dfd}, 10); err == nil {
		t.Fatal("a missing .dfx is not an error")
	}
}
