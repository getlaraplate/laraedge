//go:build !linux && !darwin && !freebsd && !windows

package buffer

import (
	"errors"
	"runtime"
)

// diskUsage is not implemented on this system; the buffer then does not
// enforce the free disk floor.
func diskUsage(string) (free, total uint64, err error) {
	return 0, 0, errors.New("disk usage not available on " + runtime.GOOS)
}
