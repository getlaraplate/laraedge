//go:build unix

package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// maxSocketPath is the longest Unix socket path the SDK accepts: sun_path
// less its terminating NUL.
const maxSocketPath = len(syscall.RawSockaddrUnix{}.Path) - 1

// socketFileName has the length of the socket file name the SDK binds in
// the socket directory, p<8 hex>.sock, with its separator.
const socketFileName = "/p00000000.sock"

// socketDir returns the socket directory of plugin id: runDir/plugins/<id>
// when runDir is set, else <absolute dataDir>/run/plugins/<id>. When that
// default does not fit the Unix socket limit it returns, with fallback true,
// <temp>/laraplate-edge-<12 hex of sha256(absolute dataDir)>/plugins/<id>,
// <temp> being os.TempDir(), or /tmp when the path does not fit there
// (os.TempDir() is a deep per-user directory on macOS). The root is created
// with mode 0700; an existing root must be a directory, not a symbolic link,
// with mode 0700 and owned by the effective user, else the error names it.
// When no candidate fits, the error names the last path tried. An explicit
// runDir is never replaced: a path too long is left to the SDK to report.
func socketDir(dataDir, runDir, id string) (dir string, fallback bool, err error) {
	return socketDirIn(dataDir, runDir, id, []string{os.TempDir(), "/tmp"})
}

// socketDirIn is socketDir with the candidate parents of the fallback root,
// tried in order.
func socketDirIn(dataDir, runDir, id string, temps []string) (dir string, fallback bool, err error) {
	if runDir != "" {
		return filepath.Join(runDir, "plugins", id), false, nil
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", false, fmt.Errorf("data_dir: %w", err)
	}
	dir = filepath.Join(abs, "run", "plugins", id)
	if fits(dir) {
		return dir, false, nil
	}

	sum := sha256.Sum256([]byte(abs))
	name := "laraplate-edge-" + hex.EncodeToString(sum[:])[:12]
	for _, temp := range temps {
		root := filepath.Join(temp, name)
		dir = filepath.Join(root, "plugins", id)
		if !fits(dir) {
			continue
		}
		if err := prepareFallbackRoot(root); err != nil {
			return "", false, err
		}

		return dir, true, nil
	}

	return "", false, fmt.Errorf("plugin socket directory %s is too long for a Unix socket (at most %d bytes with the socket file): set a short absolute run_dir in the bootstrap", dir, maxSocketPath)
}

// fits reports whether a socket file of the SDK in dir fits the Unix limit.
func fits(dir string) bool {
	return len(dir)+len(socketFileName) <= maxSocketPath
}

// prepareFallbackRoot creates root with mode 0700, or checks an existing
// one without changing it: a directory, not a symbolic link, mode 0700,
// owned by the effective user.
func prepareFallbackRoot(root string) error {
	err := os.Mkdir(root, 0o700)
	switch {
	case err == nil:
		// The umask may have narrowed the mode of the new directory.
		if err := os.Chmod(root, 0o700); err != nil {
			return fmt.Errorf("plugin socket directory %s: %w", root, err)
		}
	case !errors.Is(err, fs.ErrExist):
		return fmt.Errorf("plugin socket directory %s: %w", root, err)
	}

	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("plugin socket directory %s: %w", root, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("plugin socket directory %s is a symbolic link", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("plugin socket directory %s is not a directory", root)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("plugin socket directory %s has mode %#o, want 0700", root, perm)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("plugin socket directory %s is not owned by the agent's user", root)
	}

	return nil
}
