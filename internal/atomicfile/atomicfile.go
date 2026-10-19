// Package atomicfile replaces small state files so that after a crash they
// hold either the old or the new content, never a mix.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// TmpSuffix is the suffix of the temporary file Write uses next to the
// target; one left by a crash holds nothing of value and can be removed.
const TmpSuffix = ".tmp"

// Write replaces dir/name with data: temporary file, fsync, rename, fsync of
// the directory where supported.
func Write(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, name+TmpSuffix)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}

	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}

	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("write %s: %w", name, err)
	}

	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("replace %s: %w", name, err)
	}

	if err := SyncDir(dir); err != nil {
		return fmt.Errorf("replace %s: %w", name, err)
	}

	return nil
}

// SyncDir makes the directory entries of dir durable. Windows has no
// directory sync, and some filesystems refuse it; both count as done.
func SyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}

	syncErr := d.Sync()
	closeErr := d.Close()
	if errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, errors.ErrUnsupported) {
		syncErr = nil
	}

	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}

	return nil
}
