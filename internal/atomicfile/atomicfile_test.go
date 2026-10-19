package atomicfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/swolley/laraplate-edge/internal/atomicfile"
)

func TestWrite(t *testing.T) {
	tests := []struct {
		name     string
		existing string // content before the write; empty means no file
		blocked  bool   // a directory sits where the temporary file goes
		wantErr  bool
		want     string
	}{
		{name: "creates the file", want: "new"},
		{name: "replaces the file", existing: "old", want: "new"},
		{name: "a failed write keeps the old content", existing: "old", blocked: true, wantErr: true, want: "old"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "state")
			if tc.existing != "" {
				if err := os.WriteFile(target, []byte(tc.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if tc.blocked {
				if err := os.Mkdir(target+atomicfile.TmpSuffix, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			err := atomicfile.Write(dir, "state", []byte("new"))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Write() = %v, want error %v", err, tc.wantErr)
			}

			got, err := os.ReadFile(target)
			if err != nil || string(got) != tc.want {
				t.Fatalf("content %q, %v; want %q", got, err, tc.want)
			}

			if !tc.blocked {
				if _, err := os.Stat(target + atomicfile.TmpSuffix); !os.IsNotExist(err) {
					t.Fatalf("temporary file left behind: %v", err)
				}
			}
		})
	}
}

func TestSyncDir(t *testing.T) {
	if err := atomicfile.SyncDir(t.TempDir()); err != nil {
		t.Fatalf("SyncDir(existing) = %v", err)
	}

	if err := atomicfile.SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("SyncDir(missing) = nil, want an error")
	}
}
