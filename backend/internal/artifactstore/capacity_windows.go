//go:build windows

package artifactstore

import (
	"fmt"
	"math"

	"golang.org/x/sys/windows"
)

func filesystemCapacity(path string) (totalBytes, freeBytes int64, measured bool, err error) {
	wide, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(wide, &available, &total, &free); err != nil {
		return 0, 0, false, err
	}
	if total > math.MaxInt64 || available > math.MaxInt64 {
		return 0, 0, false, fmt.Errorf("filesystem byte capacity exceeds supported range")
	}
	return int64(total), int64(available), true, nil
}
