package file

import (
	"crypto/sha256"
	"os"
	"path/filepath"
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
