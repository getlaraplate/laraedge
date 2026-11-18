package file

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func names(files []fileInfo) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Name)
	}

	return out
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}

func TestWatcherStability(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	mt := t0.Add(-time.Hour)
	w := newWatcher(Connection{Pattern: "*", StableFor: 5 * time.Second})
	f := fileInfo{Name: "a.csv", Size: 10, ModTime: mt}

	if got := w.Ready(t0, []fileInfo{f}); len(got) != 0 {
		t.Fatalf("ready at first sight: %v", got)
	}
	if got := w.Ready(t0.Add(5*time.Second-time.Nanosecond), []fileInfo{f}); len(got) != 0 {
		t.Fatalf("ready one tick early: %v", got)
	}
	if got := w.Ready(t0.Add(5*time.Second), []fileInfo{f}); !equalNames(names(got), []string{"a.csv"}) {
		t.Fatalf("not ready at StableFor: %v", got)
	}

	// Modification time alone restarts the wait.
	f2 := f
	f2.ModTime = mt.Add(time.Second)
	if got := w.Ready(t0.Add(6*time.Second), []fileInfo{f2}); len(got) != 0 {
		t.Fatalf("ready after mtime change: %v", got)
	}
	if got := w.Ready(t0.Add(11*time.Second), []fileInfo{f2}); len(got) != 1 {
		t.Fatalf("not ready 5s after mtime change: %v", got)
	}

	// Removed then listed again restarts the wait.
	w.Ready(t0.Add(12*time.Second), nil)
	if got := w.Ready(t0.Add(13*time.Second), []fileInfo{f2}); len(got) != 0 {
		t.Fatalf("ready right after reappearing: %v", got)
	}
	if got := w.Ready(t0.Add(18*time.Second), []fileInfo{f2}); len(got) != 1 {
		t.Fatalf("not ready 5s after reappearing: %v", got)
	}
}

func TestWatcherGrowingNeverReady(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	w := newWatcher(Connection{Pattern: "*", StableFor: 5 * time.Second})
	for i := range 20 {
		f := fileInfo{Name: "a.csv", Size: int64(i + 1), ModTime: t0}
		if got := w.Ready(t0.Add(time.Duration(i)*3*time.Second), []fileInfo{f}); len(got) != 0 {
			t.Fatalf("growing file ready at call %d", i)
		}
	}
}

func TestWatcherSkipAndPattern(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		pattern string
		want    bool
	}{
		{".hidden", "*", false},
		{"a.tmp", "*", false},
		{"a.part", "*", false},
		{"a.csv~", "*", false},
		{"a.txt", "*.csv", false},
		{"REPORT.CSV", "*.csv", true},
		{"a.csv", "*.CSV", true},
		{"a.csv", "*", true},
	}
	for _, tc := range tests {
		t.Run(tc.name+"/"+tc.pattern, func(t *testing.T) {
			w := newWatcher(Connection{Pattern: tc.pattern, StableFor: time.Second})
			files := []fileInfo{{Name: tc.name, Size: 1, ModTime: t0}}
			w.Ready(t0, files)
			got := w.Ready(t0.Add(time.Second), files)
			if (len(got) == 1) != tc.want {
				t.Fatalf("ready = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWatcherOrder(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	w := newWatcher(Connection{Pattern: "*", StableFor: time.Second})
	files := []fileInfo{
		{Name: "c.csv", Size: 1, ModTime: t0},
		{Name: "b.csv", Size: 1, ModTime: t0},
		{Name: "z.csv", Size: 1, ModTime: t0.Add(-time.Minute)},
	}
	w.Ready(t0, files)
	got := names(w.Ready(t0.Add(time.Second), files))
	if want := []string{"z.csv", "b.csv", "c.csv"}; !equalNames(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestListDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.csv"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "done"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "done", "b.csv"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := listDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a.csv" || got[0].Size != 3 {
		t.Fatalf("listDir = %+v", got)
	}

	if _, err := listDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory listed without error")
	}
}
