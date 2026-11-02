//go:build unix

package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortTMPDIR points TMPDIR at a new short directory under /tmp for the
// test: t.TempDir() may itself be too deep for a socket path.
func shortTMPDIR(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "lps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)

	return dir
}

// fallbackRoot is the fallback root the Global Constraints name for dataDir.
func fallbackRoot(t *testing.T, dataDir string) string {
	t.Helper()

	abs, err := filepath.Abs(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(abs))

	return filepath.Join(os.TempDir(), "laraplate-edge-"+hex.EncodeToString(sum[:])[:12])
}

// deepDataDir returns an absolute data directory of exactly 120 bytes.
func deepDataDir() string {
	return "/" + strings.Repeat("d", 119)
}

func TestSocketDirDefault(t *testing.T) {
	dir, fallback, err := socketDir("/srv/edge", "", "demo")
	if err != nil {
		t.Fatalf("socketDir() error = %v", err)
	}
	if want := "/srv/edge/run/plugins/demo"; dir != want || fallback {
		t.Fatalf("socketDir() = %q, %v; want %q, false", dir, fallback, want)
	}
}

func TestSocketDirExplicitRunDirWins(t *testing.T) {
	runDir := "/" + strings.Repeat("r", 150)

	dir, fallback, err := socketDir("/srv/edge", runDir, "demo")
	if err != nil {
		t.Fatalf("socketDir() error = %v", err)
	}
	if want := runDir + "/plugins/demo"; dir != want || fallback {
		t.Fatalf("socketDir() = %q, %v; want %q, false", dir, fallback, want)
	}
}

func TestSocketDirFallsBackWhenTooLong(t *testing.T) {
	shortTMPDIR(t)
	dataDir := deepDataDir()
	if len(dataDir) != 120 {
		t.Fatalf("data dir is %d bytes, want 120", len(dataDir))
	}
	root := fallbackRoot(t, dataDir)

	for range 2 {
		dir, fallback, err := socketDir(dataDir, "", "demo")
		if err != nil {
			t.Fatalf("socketDir() error = %v", err)
		}
		if want := filepath.Join(root, "plugins", "demo"); dir != want || !fallback {
			t.Fatalf("socketDir() = %q, %v; want %q, true", dir, fallback, want)
		}
		info, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("root %s mode = %v, want a 0700 directory", root, info.Mode())
		}
	}
}

func TestSocketDirRefusesAnUnsafeFallback(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, root string) (target string)
	}{
		{
			name: "symbolic link",
			prepare: func(t *testing.T, root string) string {
				target := filepath.Join(os.TempDir(), "target")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, root); err != nil {
					t.Fatal(err)
				}

				return target
			},
		},
		{
			name: "directory open to others",
			prepare: func(t *testing.T, root string) string {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(root, 0o755); err != nil {
					t.Fatal(err)
				}

				return root
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shortTMPDIR(t)
			dataDir := deepDataDir()
			root := fallbackRoot(t, dataDir)
			target := tt.prepare(t, root)

			_, _, err := socketDir(dataDir, "", "demo")
			if err == nil || !strings.Contains(err.Error(), root) {
				t.Fatalf("socketDir() error = %v, want an error naming %s", err, root)
			}
			entries, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("%s holds %d entries, want none", target, len(entries))
			}
		})
	}
}
