package file

import (
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func key(s string, mt time.Time) ledgerKey {
	return ledgerKey{Hash: sha256.Sum256([]byte(s)), ModTime: mt}
}

func TestLedgerSaveLoad(t *testing.T) {
	data := t.TempDir()
	mt := time.Date(2026, 11, 18, 9, 0, 0, 123456789, time.FixedZone("x", 3600))
	l, err := loadLedger(data, "in1", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	k := key("a", mt)
	if l.Has(k) {
		t.Fatal("empty ledger has key")
	}
	l.Add(k, "a.csv")
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "file", "seen", "in1.json")); err != nil {
		t.Fatal(err)
	}

	l2, err := loadLedger(data, "in1", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Same instant in another zone and without monotonic reading.
	if !l2.Has(key("a", mt.UTC())) {
		t.Fatal("reloaded ledger misses the key")
	}
	if l2.Has(key("a", mt.Add(time.Nanosecond))) || l2.Has(key("b", mt)) {
		t.Fatal("ledger matches a different key")
	}
}

func TestLedgerPrune(t *testing.T) {
	data := t.TempDir()
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	l, _ := loadLedger(data, "in1", testLogger())
	k := key("a", t0.Add(-1000*time.Hour))
	l.Add(k, "a.csv")
	ret := time.Hour

	// Present however old: kept, no gone_since.
	l.Prune(t0, map[string]time.Time{"a.csv": k.ModTime}, ret)
	if !l.Has(k) || l.entries[l.index(k)].GoneSince != nil {
		t.Fatal("present entry not kept clean")
	}

	// Disappears: gone_since set, kept.
	l.Prune(t0, map[string]time.Time{}, ret)
	if !l.Has(k) || l.entries[l.index(k)].GoneSince == nil {
		t.Fatal("gone_since not set")
	}

	// Comes back: cleared.
	l.Prune(t0.Add(time.Minute), map[string]time.Time{"a.csv": k.ModTime}, ret)
	if l.entries[l.index(k)].GoneSince != nil {
		t.Fatal("gone_since not cleared")
	}

	// Gone again, within retention: kept; beyond: dropped.
	l.Prune(t0.Add(2*time.Minute), map[string]time.Time{}, ret)
	l.Prune(t0.Add(2*time.Minute+ret), map[string]time.Time{}, ret)
	if !l.Has(k) {
		t.Fatal("dropped at exactly retention")
	}
	l.Prune(t0.Add(2*time.Minute+ret+time.Second), map[string]time.Time{}, ret)
	if l.Has(k) {
		t.Fatal("not dropped after retention")
	}
}

func TestLedgerCorrupt(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "file", "seen")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "in1.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := loadLedger(data, "in1", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.entries) != 0 {
		t.Fatal("corrupt ledger not empty")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "in1.json.corrupt-*"))
	if len(matches) != 1 || !strings.Contains(matches[0], "corrupt-") {
		t.Fatalf("corrupt file not renamed: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(dir, "in1.json")); !os.IsNotExist(err) {
		t.Fatal("corrupt file still in place")
	}
}

func TestLedgerPruneReusedNameWithOtherModTime(t *testing.T) {
	data := t.TempDir()
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	l, _ := loadLedger(data, "in1", testLogger())
	old := key("a", t0.Add(-time.Hour))
	l.Add(old, "result.csv")

	present := map[string]time.Time{"result.csv": t0}
	l.Prune(t0, present, time.Hour)
	if l.entries[l.index(old)].GoneSince == nil {
		t.Fatal("an entry whose modification time differs was kept as present")
	}
	l.Prune(t0.Add(time.Hour+time.Second), present, time.Hour)
	if l.Has(old) {
		t.Fatal("the older entry of a reused name never expires")
	}
}
