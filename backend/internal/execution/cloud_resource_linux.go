//go:build linux

package execution

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/resourceadmission"
)

// hostCloudTaskSlots returns the number of maximum-sized task sandboxes that
// fit in the currently available host memory and logical CPU count. A task is
// queued while no slot is available; it is never rejected for resource load.
func hostCloudTaskSlots() (int, error) {
	availableBytes, err := hostCloudAvailableMemoryBytes()
	if err != nil {
		return 0, err
	}
	availableBytes = subtractReservedMemory(availableBytes)
	return cloudSlotsForCapacity(availableBytes, runtime.NumCPU()), nil
}

func hostCloudTaskSlotsWithDisk(workspaceRoot string, taskDiskBudgetBytes, diskReserveBytes int64) (int, error) {
	if strings.TrimSpace(workspaceRoot) == "" || taskDiskBudgetBytes <= 0 || diskReserveBytes < 0 {
		return 0, fmt.Errorf("cloud disk admission requires a workspace root, positive task quota, and nonnegative reserve")
	}
	availableMemoryBytes, err := hostCloudAvailableMemoryBytes()
	if err != nil {
		return 0, err
	}
	availableMemoryBytes = subtractReservedMemory(availableMemoryBytes)
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(workspaceRoot, &filesystem); err != nil {
		return 0, fmt.Errorf("inspect task workspace filesystem capacity: %w", err)
	}
	if filesystem.Bsize <= 0 {
		return 0, fmt.Errorf("task workspace filesystem reported an invalid block size")
	}
	blockSize := uint64(filesystem.Bsize)
	if filesystem.Bavail > uint64(^uint64(0)>>1)/blockSize {
		return 0, fmt.Errorf("task workspace available disk capacity overflow")
	}
	availableDiskBytes := int64(filesystem.Bavail * blockSize)
	return domain.CloudWorkerTaskSlotsForResources(availableMemoryBytes, availableDiskBytes, taskDiskBudgetBytes, diskReserveBytes, runtime.NumCPU()), nil
}

func subtractReservedMemory(availableBytes int64) int64 {
	reserved := resourceadmission.ReservedMemoryBytes()
	if reserved >= availableBytes {
		return 0
	}
	return availableBytes - reserved
}

func hostCloudAvailableMemoryBytes() (int64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer file.Close()
	var availableKiB int64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			availableKiB, err = strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse MemAvailable: %w", err)
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	if availableKiB <= 0 {
		return 0, fmt.Errorf("MemAvailable is missing or invalid")
	}
	availableBytes := availableKiB * 1024
	cgroupBytes, constrained, err := cgroupAvailableMemoryBytes()
	if err != nil {
		return 0, err
	}
	if constrained && cgroupBytes < availableBytes {
		availableBytes = cgroupBytes
	}
	return availableBytes, nil
}

func hostCloudWorkerLoopLimit() int {
	return runtime.NumCPU()
}

func cgroupAvailableMemoryBytes() (int64, bool, error) {
	const cgroupRoot = "/sys/fs/cgroup"
	membership, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, false, fmt.Errorf("read current cgroup membership: %w", err)
	}
	var relative string
	for _, line := range strings.Split(strings.TrimSpace(string(membership)), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && fields[0] == "0" && fields[1] == "" {
			relative = filepath.Clean(filepath.FromSlash(fields[2]))
			break
		}
	}
	if relative == "" || !filepath.IsAbs(relative) || relative == string(filepath.Separator)+".." || strings.HasPrefix(relative, string(filepath.Separator)+".."+string(filepath.Separator)) {
		return 0, false, fmt.Errorf("current cgroup v2 membership is missing or invalid")
	}
	current := filepath.Join(cgroupRoot, strings.TrimLeft(relative, string(filepath.Separator)))
	root, err := filepath.Abs(cgroupRoot)
	if err != nil {
		return 0, false, err
	}
	available := int64(0)
	constrained := false
	for {
		if !strings.HasPrefix(current, root) {
			return 0, false, errors.New("current cgroup path escaped the cgroup v2 mount")
		}
		limitBytes, limited, readErr := cgroupMemoryLimit(current)
		if readErr != nil {
			return 0, false, readErr
		}
		if limited {
			usedBytes, err := readCgroupBytes(filepath.Join(current, "memory.current"))
			if err != nil {
				return 0, false, fmt.Errorf("read cgroup memory.current: %w", err)
			}
			remaining := cgroupRemainingBytes(limitBytes, usedBytes)
			if !constrained || remaining < available {
				available = remaining
			}
			constrained = true
		}
		if current == root {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return available, constrained, nil
}

func cgroupMemoryLimit(directory string) (int64, bool, error) {
	content, err := os.ReadFile(filepath.Join(directory, "memory.max"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read cgroup memory.max: %w", err)
	}
	value := strings.TrimSpace(string(content))
	if value == "max" {
		return 0, false, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit < 0 {
		return 0, false, fmt.Errorf("parse cgroup memory.max value %q", value)
	}
	return limit, true, nil
}

func readCgroupBytes(path string) (int64, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("parse byte count in %s", path)
	}
	return value, nil
}

func cgroupRemainingBytes(limitBytes, usedBytes int64) int64 {
	if usedBytes >= limitBytes {
		return 0
	}
	return limitBytes - usedBytes
}

func cloudSlotsForCapacity(availableBytes int64, cpuSlots int) int {
	return domain.CloudWorkerTaskSlotsForCapacity(availableBytes, cpuSlots)
}
