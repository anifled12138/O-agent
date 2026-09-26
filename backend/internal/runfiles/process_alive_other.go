//go:build !windows

package runfiles

import (
	"errors"
	"fmt"
	"syscall"
)

func processAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, fmt.Errorf("probe process %d: %w", pid, err)
}
