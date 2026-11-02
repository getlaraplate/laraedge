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
// default does not fit the Unix socket limit it returns
// <os.TempDir()>/laraplate-edge-<12 hex of sha256(absolute dataDir)>/plugins/<id>
// with fallback true, the root created with mode 0700; an existing root must
// be a directory, not a symbolic link, with mode 0700 and owned by the
// effective user, else the error names it. An explicit runDir is never
// replaced: a path too long is left to the SDK to report.
func socketDir(dataDir, runDir, id string) (dir string, fallback bool, err error) {
	if runDir != "" {
		return filepath.Join(runDir, "plugins", id), false, nil
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", false, fmt.Errorf("data_dir: %w", err)
	}
	dir = filepath.Join(abs, "run", "plugins", id)
	if len(dir)+len(socketFileName) <= maxSocketPath {
		return dir, false, nil
	}

	sum := sha256.Sum256([]byte(abs))
	root := filepath.Join(os.TempDir(), "laraplate-edge-"+hex.EncodeToString(sum[:])[:12])
	if err := prepareFallbackRoot(root); err != nil {
		return "", false, err
	}

	return filepath.Join(root, "plugins", id), true, nil
}

// prepareFallbackRoot creates root with mode 0700, or checks an existing
// one without changing it: a directory, not a symbolic link, mode 0700,
// owned by the effective user.
func prepareFallbackRoot(root string) error {
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
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
