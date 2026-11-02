//go:build unix

package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// fallbackRoot is the fallback root the Global Constraints name for dataDir
// under os.TempDir().
func fallbackRoot(t *testing.T, dataDir string) string {
	t.Helper()

	return fallbackRootIn(t, os.TempDir(), dataDir)
}

// fallbackRootIn is the fallback root of dataDir under temp.
func fallbackRootIn(t *testing.T, temp, dataDir string) string {
	t.Helper()

	abs, err := filepath.Abs(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(abs))

	return filepath.Join(temp, "laraplate-edge-"+hex.EncodeToString(sum[:])[:12])
}

// assertPrivateDir fails unless dir is a directory with mode 0700.
func assertPrivateDir(t *testing.T, dir string) {
	t.Helper()

	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s mode = %v, want a 0700 directory", dir, info.Mode())
	}
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
		assertPrivateDir(t, root)
	}
}

// TestSocketDirFallbackIgnoresTheUmask: a new root gets mode 0700 whatever
// the umask, which would otherwise make it 0500 and refused.
func TestSocketDirFallbackIgnoresTheUmask(t *testing.T) {
	shortTMPDIR(t)
	old := syscall.Umask(0o277)
	t.Cleanup(func() { syscall.Umask(old) })

	if _, _, err := socketDir(deepDataDir(), "", "demo"); err != nil {
		t.Fatalf("socketDir() error = %v", err)
	}
	assertPrivateDir(t, fallbackRoot(t, deepDataDir()))
}

// TestSocketDirFallsBackToTmpWhenTempIsDeep: a temporary directory too deep
// for the socket (the per-user TMPDIR of macOS) moves the root to /tmp.
func TestSocketDirFallsBackToTmpWhenTempIsDeep(t *testing.T) {
	short := shortTMPDIR(t)
	deepTemp := filepath.Join(short, strings.Repeat("t", 80))
	if err := os.Mkdir(deepTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", deepTemp)
	// A data directory unique to this run, so its root under /tmp is too.
	base := filepath.Base(short)
	dataDir := "/" + base + strings.Repeat("d", 119-len(base))
	root := fallbackRootIn(t, "/tmp", dataDir)
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	dir, fallback, err := socketDir(dataDir, "", "demo")
	if err != nil {
		t.Fatalf("socketDir() error = %v", err)
	}
	if want := filepath.Join(root, "plugins", "demo"); dir != want || !fallback {
		t.Fatalf("socketDir() = %q, %v; want %q, true", dir, fallback, want)
	}
	assertPrivateDir(t, root)
	if _, err := os.Lstat(fallbackRootIn(t, deepTemp, dataDir)); err == nil {
		t.Fatal("a root was created under the deep temporary directory")
	}
}

func TestSocketDirRefusesWhenNoFallbackFits(t *testing.T) {
	deep := "/" + strings.Repeat("t", 100)

	_, _, err := socketDirIn(deepDataDir(), "", "demo", []string{deep, deep})
	if err == nil || !strings.Contains(err.Error(), fallbackRootIn(t, deep, deepDataDir())) || !strings.Contains(err.Error(), "run_dir") {
		t.Fatalf("socketDirIn() error = %v, want one naming the path tried and run_dir", err)
	}
}

func TestSocketDirRefusesAnUnsafeFallback(t *testing.T) {
	tests := []struct {
		name string
		// prepare makes root unsafe and returns a directory that must stay
		// empty, or "" when there is none.
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
		{
			name: "regular file",
			prepare: func(t *testing.T, root string) string {
				if err := os.WriteFile(root, nil, 0o600); err != nil {
					t.Fatal(err)
				}

				return ""
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
			if target == "" {
				return
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
