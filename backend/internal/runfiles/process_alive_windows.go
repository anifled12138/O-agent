//go:build windows

package runfiles

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func processAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err == nil {
		if closeErr := windows.CloseHandle(process); closeErr != nil {
			return true, fmt.Errorf("close owner process handle: %w", closeErr)
		}
		return true, nil
	}
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return true, nil
	}
	return true, fmt.Errorf("open owner process: %w", err)
}
