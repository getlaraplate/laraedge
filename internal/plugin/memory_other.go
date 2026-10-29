//go:build !linux && !windows

package plugin

// processMemory cannot read the memory of a process on this platform.
func processMemory(int) (int64, error) {
	return 0, errMemoryUnsupported
}
