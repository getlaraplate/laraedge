package file

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	edgeinput "github.com/getlaraplate/laraedge/internal/input"
)

// snapshot is everything probe and browse must leave alone: the names and
// modification times under the watched directory (subdirectories included)
// and the bytes of the ledger.
func (h *runHarness) snapshot() string {
	h.t.Helper()

	var b strings.Builder
	err := filepath.WalkDir(h.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b.WriteString(p + " " + info.ModTime().UTC().Format(time.RFC3339Nano) + "\n")

		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	b.Write(h.ledgerBytes())

	return b.String()
}

// seedLedger records content at mod as read, through the ledger the input
// itself writes.
func (h *runHarness) seedLedger(name, content string, mod time.Time) {
	h.t.Helper()

	l := peekLedger(h.dataDir, h.cfg.ID)
	l.Add(ledgerKey{Hash: sha256Of(content), ModTime: mod}, name)
	if err := l.Save(); err != nil {
		h.t.Fatal(err)
	}
}

func TestProbeReadsTheNewestReadyUnseenFile(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("old.csv", "Serial,Bore\nS-OLD,1\n", fileTime.Add(-time.Hour))
	h.write("new.csv", "Serial,Bore\nS-NEW,2.5\n", fileTime)
	h.seedLedger("old.csv", "Serial,Bore\nS-OLD,1\n", fileTime.Add(-time.Hour))

	samples, kinds, err := h.in.ProbePoints(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || len(kinds) != 0 {
		t.Fatalf("samples = %v, kinds = %v", samples, kinds)
	}
	if got := h.in.ProbeSource(); got != "new.csv" {
		t.Errorf("source = %q", got)
	}
	for _, s := range samples {
		if s.Signal == "bore" && s.Value.Float != 2.5 {
			t.Errorf("bore = %v", s.Value)
		}
	}
}

func TestProbeSkipsASeenFileAndTheYoungOne(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("old.csv", "Serial,Bore\nS-OLD,1\n", fileTime)
	h.write("young.csv", "Serial,Bore\nS-Y,3\n", runEpoch.Add(-time.Second))

	samples, _, err := h.in.ProbePoints(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if h.in.ProbeSource() != "old.csv" || len(samples) != 2 {
		t.Errorf("source = %q, samples = %v", h.in.ProbeSource(), samples)
	}
}

func TestProbeWithoutAQualifyingFileSaysNoFile(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("young.csv", report, runEpoch.Add(-time.Second))
	h.write("seen.csv", report, fileTime)
	h.seedLedger("seen.csv", report, fileTime)
	h.write("x.tmp", report, fileTime)

	samples, kinds, err := h.in.ProbePoints(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 || h.in.ProbeSource() != "" {
		t.Errorf("samples = %v, source = %q", samples, h.in.ProbeSource())
	}
	want := map[edgeinput.PointRef]string{
		{Device: "cmm", Signal: "bore"}:     ProbeNoFile,
		{Device: "gauge", Signal: "serial"}: ProbeNoFile,
	}
	if !equalKinds(kinds, want) {
		t.Errorf("kinds = %v", kinds)
	}
}

func TestProbeOfAnEmptyDirectorySaysNoFile(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	_, kinds, err := h.in.ProbePoints(t.Context())
	if err != nil || len(kinds) != 2 || kinds[edgeinput.PointRef{Device: "cmm", Signal: "bore"}] != ProbeNoFile {
		t.Fatalf("empty: kinds = %v, err = %v", kinds, err)
	}
}

// TestProbeOfAnUnlistableDirectoryFails: a directory that cannot be listed
// is an error with a fixed text, as for browse, not "no file".
func TestProbeOfAnUnlistableDirectoryFails(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	if err := os.RemoveAll(h.dir); err != nil {
		t.Fatal(err)
	}
	samples, kinds, err := h.in.ProbePoints(t.Context())
	if err == nil || err.Error() != "the directory cannot be listed" || samples != nil || kinds != nil {
		t.Fatalf("samples = %v, kinds = %v, err = %v", samples, kinds, err)
	}
	if strings.Contains(err.Error(), h.dir) {
		t.Error("the error names the directory")
	}
	if h.readErrors(KindListing) != 1 {
		t.Errorf("listing errors = %d", h.readErrors(KindListing))
	}
}

func TestProbeKinds(t *testing.T) {
	tests := []struct {
		name    string
		conn    map[string]any
		content string
		want    map[edgeinput.PointRef]string
		samples int
	}{
		{
			name:    "missing column",
			content: "Serial,Other\nS-1,1\n",
			want:    map[edgeinput.PointRef]string{{Device: "cmm", Signal: "bore"}: KindMissingField},
			samples: 1,
		},
		{
			name:    "bad value",
			content: "Serial,Bore\nS-1,abc\n",
			want:    map[edgeinput.PointRef]string{{Device: "cmm", Signal: "bore"}: KindConversion},
			samples: 2,
		},
		{
			name:    "malformed file",
			content: "Serial,Serial,Bore\nS-1,S-1,1\n",
			want: map[edgeinput.PointRef]string{
				{Device: "cmm", Signal: "bore"}:     KindMalformedFile,
				{Device: "gauge", Signal: "serial"}: KindMalformedFile,
			},
		},
		{
			name:    "too large",
			conn:    map[string]any{"max_file_size": 10},
			content: report,
			want: map[edgeinput.PointRef]string{
				{Device: "cmm", Signal: "bore"}:     KindFileTooLarge,
				{Device: "gauge", Signal: "serial"}: KindFileTooLarge,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRunHarness(t, harnessSetup{conn: tt.conn})
			h.write("r.csv", tt.content, fileTime)

			samples, kinds, err := h.in.ProbePoints(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(samples) != tt.samples || !equalKinds(kinds, tt.want) {
				t.Errorf("samples = %d, kinds = %v", len(samples), kinds)
			}
			if h.in.ProbeSource() != "r.csv" {
				t.Errorf("source = %q", h.in.ProbeSource())
			}
		})
	}
}

func TestProbeAndBrowseWriteNothing(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("a.csv", report, fileTime)
	h.write("b.csv", "Serial,Bore\nS-2,3\n", fileTime.Add(time.Minute))
	h.write("c.csv", "Serial,Bore\nS-3,4\n", runEpoch.Add(-time.Second))
	h.seedLedger("a.csv", report, fileTime)
	before := h.snapshot()

	if _, _, err := h.in.ProbePoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.in.Browse(DefaultBrowseLimit); err != nil {
		t.Fatal(err)
	}
	if after := h.snapshot(); after != before {
		t.Errorf("changed:\nbefore %s\nafter %s", before, after)
	}
}

func TestProbeAndBrowseLeaveACorruptLedgerAlone(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("a.csv", report, fileTime)
	dir, name := ledgerPath(h.dataDir, h.cfg.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := h.snapshot()

	samples, _, err := h.in.ProbePoints(t.Context())
	if err != nil || len(samples) != 2 {
		t.Fatalf("samples = %v, err = %v", samples, err)
	}
	if _, err := h.in.Browse(DefaultBrowseLimit); err != nil {
		t.Fatal(err)
	}
	if after := h.snapshot(); after != before {
		t.Errorf("changed:\nbefore %s\nafter %s", before, after)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("ledger dir = %v, err = %v", entries, err)
	}
}

func TestBrowseStatesAndOrder(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("seen.csv", "Serial,Bore\nS-1,1\n", fileTime.Add(-2*time.Hour))
	h.write("ready.csv", "Serial,Bore\nS-2,2\n", fileTime.Add(-time.Hour))
	h.write("waiting.csv", "Serial,Bore\nS-3,3\n", runEpoch.Add(-3*time.Second))
	h.write("skip.tmp", report, fileTime)
	h.seedLedger("seen.csv", "Serial,Bore\nS-1,1\n", fileTime.Add(-2*time.Hour))

	b, err := h.in.Browse(DefaultBrowseLimit)
	if err != nil {
		t.Fatal(err)
	}
	want := []BrowseFile{
		{Name: "waiting.csv", Size: 18, Age: 3 * time.Second, State: StateWaiting},
		{Name: "ready.csv", Size: 18, Age: 2 * time.Hour, State: StateReady},
		{Name: "seen.csv", Size: 18, Age: 3 * time.Hour, State: StateSeen},
	}
	if !slices.Equal(b.Files, want) {
		t.Errorf("files = %+v", b.Files)
	}
}

func TestBrowseLimitKeepsTheNewest(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	for i, n := range []string{"a.csv", "b.csv", "c.csv"} {
		h.write(n, report, fileTime.Add(time.Duration(i)*time.Minute))
	}

	b, err := h.in.Browse(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) != 2 || b.Files[0].Name != "c.csv" || b.Files[1].Name != "b.csv" || b.Source != "c.csv" {
		t.Errorf("browse = %+v", b)
	}
	if h.in.ProbeSource() != "" {
		t.Errorf("browse set the probe source: %q", h.in.ProbeSource())
	}
}

func TestBrowseWideAddressesWithPreviews(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	h.write("r.csv", "Serial, Bore ,Note\nS-1,12.5,\""+strings.Repeat("é", 50)+"\"\nS-2,9,x\n", fileTime)

	b, err := h.in.Browse(DefaultBrowseLimit)
	if err != nil {
		t.Fatal(err)
	}
	want := []BrowseAddress{
		{Address: "Serial", Preview: `"S-1"`},
		{Address: "Bore", Preview: `"12.5"`},
		{Address: "Note", Preview: `"` + strings.Repeat("é", 40) + `"`},
	}
	if b.Source != "r.csv" || !slices.Equal(b.Addresses, want) {
		t.Errorf("source = %q, addresses = %+v", b.Source, b.Addresses)
	}
}

func TestBrowseLongAddressesWithPreviews(t *testing.T) {
	h := newRunHarness(t, harnessSetup{conn: map[string]any{
		"csv": map[string]any{"layout": "long", "key_column": "Key", "value_column": "Value"},
	}})
	h.write("r.csv", "Key,Value\nBore,12.5\nSerial,S-1\nBore,13\n", fileTime)

	b, err := h.in.Browse(DefaultBrowseLimit)
	if err != nil {
		t.Fatal(err)
	}
	want := []BrowseAddress{{Address: "Bore", Preview: `"12.5"`}, {Address: "Serial", Preview: `"S-1"`}}
	if !slices.Equal(b.Addresses, want) {
		t.Errorf("addresses = %+v", b.Addresses)
	}
}

func TestBrowseSourceIsTheNewestReadableFile(t *testing.T) {
	h := newRunHarness(t, harnessSetup{conn: map[string]any{"max_file_size": 30}})
	h.write("big.csv", "Serial,Bore\n"+strings.Repeat("S-1,1\n", 20), fileTime.Add(time.Minute))
	h.write("ok.csv", report, fileTime)

	b, err := h.in.Browse(DefaultBrowseLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) != 2 || b.Source != "ok.csv" {
		t.Errorf("browse = %+v", b)
	}

	empty := newRunHarness(t, harnessSetup{})
	b, err = empty.in.Browse(DefaultBrowseLimit)
	if err != nil || b.Source != "" || len(b.Files) != 0 || len(b.Addresses) != 0 {
		t.Errorf("empty browse = %+v, err = %v", b, err)
	}
}

func TestBrowseOfAMissingDirectoryFails(t *testing.T) {
	h := newRunHarness(t, harnessSetup{})
	if err := os.RemoveAll(h.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := h.in.Browse(DefaultBrowseLimit); err == nil {
		t.Error("want an error")
	}
}

func equalKinds(got, want map[edgeinput.PointRef]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}

	return true
}

func sha256Of(content string) [32]byte {
	return sha256.Sum256([]byte(content))
}

// qdasBrowseReport describes three characteristics: a full one, one without
// K2001 and an attributive one, with context fields shared, positional and
// coded per characteristic.
var qdasBrowseReport = qdasLines(
	"K0100 3",
	"K2001/1 Bore", "K2002/1 Diameter", "K2101/1 12.5", "K2110/1 12.4", "K2111/1 12.6", "K2142/1 mm",
	"K2002/3 Flag", "K2004/3 1",
	"12.5"+qf+qf+qf+"EV"+qs+"7"+qs+"1",
	"K0014/0 P-1", "K0053/0 O-1", "K0006/2 B9",
)

func TestProbeQDASReadsAFileAndAPair(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.write("a.dfq", qdasReport, fileTime)
	samples, kinds, err := h.in.ProbePoints(t.Context())
	if err != nil || len(kinds) != 0 || h.in.ProbeSource() != "a.dfq" {
		t.Fatalf("kinds = %v, source = %q, err = %v", kinds, h.in.ProbeSource(), err)
	}
	checkQDASSamples(t, samples, fileTime)

	p := newQDASHarness(t, nil)
	p.writePair("a", fileTime)
	samples, kinds, err = p.in.ProbePoints(t.Context())
	if err != nil || len(kinds) != 0 || p.in.ProbeSource() != "a.dfd + a.dfx" {
		t.Fatalf("pair: kinds = %v, source = %q, err = %v", kinds, p.in.ProbeSource(), err)
	}
	checkQDASSamples(t, samples, fileTime)
}

func TestProbeQDASSkipsASeenPair(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.writePair("a", fileTime)
	l := peekLedger(h.dataDir, h.cfg.ID)
	l.Add(pairKey(qdasDescription, qdasValues, fileTime), "a.dfx")
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}

	samples, kinds, err := h.in.ProbePoints(t.Context())
	if err != nil || len(samples) != 0 || len(kinds) != 2 || h.in.ProbeSource() != "" {
		t.Fatalf("samples = %v, kinds = %v, source = %q, err = %v", samples, kinds, h.in.ProbeSource(), err)
	}
}

func TestProbeQDASDfxAloneIsNoFile(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.write("a.dfx", qdasValues, fileTime)
	h.write("b.dfd", qdasDescription, fileTime)
	samples, kinds, err := h.in.ProbePoints(t.Context())
	want := map[edgeinput.PointRef]string{
		{Device: "cmm", Signal: "bore"}:     ProbeNoFile,
		{Device: "gauge", Signal: "serial"}: ProbeNoFile,
	}
	if err != nil || len(samples) != 0 || !equalKinds(kinds, want) || h.in.ProbeSource() != "" {
		t.Fatalf("samples = %v, kinds = %v, source = %q, err = %v", samples, kinds, h.in.ProbeSource(), err)
	}
}

func TestBrowseQDASStates(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.writePair("seen", fileTime.Add(-3*time.Hour))
	h.write("lonely.dfx", qdasValues, fileTime.Add(-2*time.Hour))
	h.write("old.dfd", qdasDescription, fileTime.Add(-time.Hour))
	h.write("young.dfd", qdasDescription, runEpoch.Add(-time.Second))
	h.write("fresh.dfq", qdasReport, fileTime)
	h.write("note.txt", "x", fileTime)
	l := peekLedger(h.dataDir, h.cfg.ID)
	l.Add(pairKey(qdasDescription, qdasValues, fileTime.Add(-3*time.Hour)), "seen.dfx")
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}

	b, err := h.in.Browse(DefaultBrowseLimit)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range b.Files {
		got[f.Name] = f.State
	}
	want := map[string]string{
		"young.dfd":  StateWaiting,
		"fresh.dfq":  StateReady,
		"old.dfd":    StateReady,
		"lonely.dfx": StateIncomplete,
		"seen.dfd":   StateSeen,
		"seen.dfx":   StateSeen,
	}
	if !reflect.DeepEqual(got, want) || StateIncomplete != "incomplete" {
		t.Errorf("states = %v", got)
	}
}

func TestBrowseQDASAddressesAndContext(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.write("a.dfq", qdasBrowseReport, fileTime)

	b, err := h.in.Browse(DefaultBrowseLimit)
	if err != nil {
		t.Fatal(err)
	}
	wantAddrs := []BrowseAddress{
		{Address: "Bore", Preview: `"12.5"`, Detail: "Diameter  nominal 12.5  limits 12.4..12.6  mm"},
		{Address: "#2", Preview: `"7"`},
		{Address: "#3", Preview: `"1"`, Detail: "Flag  attributive, not read"},
	}
	wantContext := []BrowseAddress{
		{Address: "K0014", Preview: `"P-1"`},
		{Address: "K0053", Preview: `"O-1"`},
		{Address: "K0005", Preview: `"EV"`},
		{Address: "K0006", Preview: `"B9"`},
	}
	if b.Source != "a.dfq" || !slices.Equal(b.Addresses, wantAddrs) || !slices.Equal(b.Context, wantContext) {
		t.Errorf("source = %q, addresses = %+v, context = %+v", b.Source, b.Addresses, b.Context)
	}
}

func TestBrowseQDASSourceOfAPair(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.write("p.dfd", qdasLines("K0100 1", "K2001/1 Bore"), fileTime)
	h.write("p.dfx", qdasLines("5", "K0014/0 SN"), fileTime)

	b, err := h.in.Browse(DefaultBrowseLimit)
	if err != nil {
		t.Fatal(err)
	}
	if b.Source != "p.dfd + p.dfx" || len(b.Addresses) != 1 || b.Addresses[0].Preview != `"5"` || len(b.Context) != 1 {
		t.Errorf("browse = %+v", b)
	}
}

func TestProbeAndBrowseQDASWriteNothing(t *testing.T) {
	h := newQDASHarness(t, nil)
	h.writePair("a", fileTime)
	h.write("b.dfq", qdasReport, fileTime.Add(time.Minute))
	before := h.snapshot()

	if _, _, err := h.in.ProbePoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.in.Browse(DefaultBrowseLimit); err != nil {
		t.Fatal(err)
	}
	if after := h.snapshot(); after != before {
		t.Errorf("changed:\n%s\n---\n%s", before, after)
	}
}
