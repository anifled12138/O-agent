//go:build windows

package execution

import (
	"fmt"
	"runtime"
	"unsafe"

	"axiom.local/agent/internal/storage"
	"golang.org/x/sys/windows"
)

type nodeMemoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var kernel32NodeResources = windows.NewLazySystemDLL("kernel32.dll")

func hostNodeResources() (storage.ExecutionNodeResources, error) {
	status := nodeMemoryStatusEx{Length: uint32(unsafe.Sizeof(nodeMemoryStatusEx{}))}
	proc := kernel32NodeResources.NewProc("GlobalMemoryStatusEx")
	result, _, callErr := proc.Call(uintptr(unsafe.Pointer(&status)))
	if result == 0 {
		if callErr == nil {
			return storage.ExecutionNodeResources{}, fmt.Errorf("read Windows memory capacity: GlobalMemoryStatusEx returned zero without an error")
		}
		return storage.ExecutionNodeResources{}, fmt.Errorf("read Windows memory capacity: %w", callErr)
	}
	if status.TotalPhys == 0 || status.AvailPhys > status.TotalPhys {
		return storage.ExecutionNodeResources{}, fmt.Errorf("Windows returned invalid node memory capacity")
	}
	cpus := runtime.NumCPU()
	slots := nodeTaskSlotsForCapacity(int64(status.AvailPhys), cpus)
	return storage.ExecutionNodeResources{MemoryTotalBytes: int64(status.TotalPhys), MemoryAvailableBytes: int64(status.AvailPhys), LogicalCPUs: cpus, MaxConcurrentTasks: slots}, nil
}

func NodeResourceSnapshot() (storage.ExecutionNodeResources, error) { return hostNodeResources() }
