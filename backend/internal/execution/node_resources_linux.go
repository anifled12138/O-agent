//go:build linux

package execution

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"axiom.local/agent/internal/resourceadmission"
	"axiom.local/agent/internal/storage"
)

func hostNodeResources() (storage.ExecutionNodeResources, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return storage.ExecutionNodeResources{}, fmt.Errorf("open /proc/meminfo for node capacity: %w", err)
	}
	defer file.Close()
	var totalKiB, availableKiB int64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[2] != "kB" {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKiB, err = strconv.ParseInt(fields[1], 10, 64)
		case "MemAvailable:":
			availableKiB, err = strconv.ParseInt(fields[1], 10, 64)
		}
		if err != nil {
			return storage.ExecutionNodeResources{}, fmt.Errorf("parse %s for node capacity: %w", fields[0], err)
		}
	}
	if err := scanner.Err(); err != nil {
		return storage.ExecutionNodeResources{}, fmt.Errorf("read /proc/meminfo for node capacity: %w", err)
	}
	if totalKiB <= 0 || availableKiB <= 0 || availableKiB > totalKiB {
		return storage.ExecutionNodeResources{}, fmt.Errorf("/proc/meminfo returned invalid node memory capacity")
	}
	totalBytes, availableBytes := totalKiB*1024, availableKiB*1024
	cgroupBytes, constrained, err := cgroupAvailableMemoryBytes()
	if err != nil {
		return storage.ExecutionNodeResources{}, fmt.Errorf("read node cgroup capacity: %w", err)
	}
	if constrained && cgroupBytes < availableBytes {
		availableBytes = cgroupBytes
	}
	reserved := resourceadmission.ReservedMemoryBytes()
	if reserved >= availableBytes {
		availableBytes = 0
	} else {
		availableBytes -= reserved
	}
	cpus := runtime.NumCPU()
	slots := nodeTaskSlotsForCapacity(availableBytes, cpus)
	return storage.ExecutionNodeResources{MemoryTotalBytes: totalBytes, MemoryAvailableBytes: availableBytes, LogicalCPUs: cpus, MaxConcurrentTasks: slots}, nil
}

// NodeResourceSnapshot returns the same live capacity estimate used by local
// and cloud task admission, for durable heartbeat reporting.
func NodeResourceSnapshot() (storage.ExecutionNodeResources, error) { return hostNodeResources() }
