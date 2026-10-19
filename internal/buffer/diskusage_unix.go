//go:build linux || darwin || freebsd

package buffer

import (
	"fmt"
	"syscall"
)

// diskUsage reports the bytes available to unprivileged users and the size
// of the filesystem holding dir.
func diskUsage(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", dir, err)
	}

	bsize := uint64(st.Bsize)

	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, nil
}
