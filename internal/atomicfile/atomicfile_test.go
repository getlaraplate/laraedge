package atomicfile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/getlaraplate/laraplate-edge/internal/atomicfile"
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
	tests := []struct {
		name    string
		goos    string
		missing bool
		wantErr bool
	}{
		{name: "existing directory", goos: runtime.GOOS},
		{name: "missing directory", goos: runtime.GOOS, missing: true, wantErr: true},
		{name: "existing directory on windows", goos: "windows"},
		{name: "missing directory on windows", goos: "windows", missing: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.missing {
				dir = filepath.Join(dir, "missing")
			}

			err := atomicfile.SyncDirOn(dir, tc.goos)
			if (err != nil) != tc.wantErr {
				t.Fatalf("SyncDir(%s) on %s = %v, want error %v", tc.name, tc.goos, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("SyncDir(%s) on %s = %v, want fs.ErrNotExist", tc.name, tc.goos, err)
			}
		})
	}

	// The exported function runs the branch of this system.
	if err := atomicfile.SyncDir(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SyncDir(missing) = %v, want fs.ErrNotExist", err)
	}
}
