package buffer

// blockBytes returns blocks*blockSize, with a negative block count (FreeBSD
// reports Bavail as a signed value that goes below zero when the reserve is
// in use) clamped to zero.
func blockBytes[T ~int64 | ~uint64](blocks T, blockSize uint64) uint64 {
	if blocks <= 0 {
		return 0
	}

	return uint64(blocks) * blockSize
}
