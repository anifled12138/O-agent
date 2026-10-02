//go:build !linux

package execution

import "fmt"

func hostCloudWorkerLoopLimit() int { return 1 }

// Outside Linux, cloud admission stays conservative until that host platform
// has an authoritative memory-capacity reader matching the Linux sandbox.
func hostCloudTaskSlots() (int, error) { return 1, nil }

func hostCloudTaskSlotsWithDisk(_ string, taskDiskBudgetBytes, diskReserveBytes int64) (int, error) {
	if taskDiskBudgetBytes <= 0 || diskReserveBytes < 0 {
		return 0, fmt.Errorf("cloud disk admission requires a positive task quota and nonnegative reserve")
	}
	return 1, nil
}
