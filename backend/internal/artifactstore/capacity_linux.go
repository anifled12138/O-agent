//go:build linux

package artifactstore

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func filesystemCapacity(path string) (totalBytes, freeBytes int64, measured bool, err error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, 0, false, err
	}
	if stats.Bsize <= 0 || stats.Blocks > math.MaxInt64/uint64(stats.Bsize) || stats.Bavail > math.MaxInt64/uint64(stats.Bsize) {
		return 0, 0, false, fmt.Errorf("filesystem byte capacity exceeds supported range")
	}
	totalBytes = int64(stats.Blocks * uint64(stats.Bsize))
	freeBytes = int64(stats.Bavail * uint64(stats.Bsize))
	if totalBytes < 0 || freeBytes < 0 {
		return 0, 0, false, unix.EOVERFLOW
	}
	return totalBytes, freeBytes, true, nil
}
