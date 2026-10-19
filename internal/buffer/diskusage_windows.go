//go:build windows

package buffer

import (
	"fmt"
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskUsage reports the bytes available to the calling user and the size of
// the volume holding dir.
func diskUsage(dir string) (free, total uint64, err error) {
	path, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, fmt.Errorf("disk usage of %s: %w", dir, err)
	}

	r, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)),
		0,
	)
	if r == 0 {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceExW %s: %w", dir, callErr)
	}

	return free, total, nil
}
