package domain

// MaxCloudWorkerConcurrency is the configurable ceiling for cloud task
// workers. Host resource admission may select a lower active count at runtime.
const MaxCloudWorkerConcurrency = 64

// MaxLocalNodeWorkerConcurrency bounds scheduler workers. Reported active
// capacity is still reduced by the node's live memory and CPU snapshot.
const MaxLocalNodeWorkerConcurrency = 64

// CloudTaskSandboxMemoryLimitBytes is a legacy name for the estimated task
// admission budget. It does not impose a Linux command memory hard limit.
const CloudTaskSandboxMemoryLimitBytes int64 = 1536 << 20

// CloudWorkspaceDiskReserveBytes keeps space available for the control plane,
// logs, backups, and artifact staging on the shared workspace filesystem.
const CloudWorkspaceDiskReserveBytes int64 = 4 << 30

// LocalNodeControlPlaneReserveBytes protects the node process and desktop
// from admitting work against memory already needed by the operating system.
const LocalNodeControlPlaneReserveBytes int64 = 512 << 20

// SafeAgentContinuationStopReason identifies stops whose durable checkpoint
// may be resumed without replaying an in-flight side effect.
func SafeAgentContinuationStopReason(reason string) bool {
	switch reason {
	case "step_limit", "model_call_limit", "stalled", "handoff_requested":
		return true
	default:
		return false
	}
}

// CloudWorkerTaskSlotsForCapacity admits as many full-sized cloud sandboxes
// as available memory and logical CPU capacity allow.
func CloudWorkerTaskSlotsForCapacity(availableBytes int64, logicalCPUs int) int {
	if availableBytes <= LocalNodeControlPlaneReserveBytes || logicalCPUs < 1 {
		return 0
	}
	slots := int((availableBytes - LocalNodeControlPlaneReserveBytes) / CloudTaskSandboxMemoryLimitBytes)
	if slots > logicalCPUs {
		slots = logicalCPUs
	}
	return slots
}

// CloudWorkerTaskSlotsForResources admits a task only when both its maximum
// memory envelope and its configured workspace quota fit the live host. Disk
// capacity is a scheduling signal: callers keep tasks queued when it is low.
func CloudWorkerTaskSlotsForResources(availableMemoryBytes, availableDiskBytes, taskDiskBudgetBytes, diskReserveBytes int64, logicalCPUs int) int {
	memorySlots := CloudWorkerTaskSlotsForCapacity(availableMemoryBytes, logicalCPUs)
	if memorySlots == 0 || taskDiskBudgetBytes <= 0 || diskReserveBytes < 0 || availableDiskBytes <= diskReserveBytes {
		return 0
	}
	diskSlots := (availableDiskBytes - diskReserveBytes) / taskDiskBudgetBytes
	if int64(memorySlots) > diskSlots {
		return int(diskSlots)
	}
	return memorySlots
}

// LocalNodeTaskSlotsForCapacity estimates local admission from live capacity.
// It deliberately has no fixed two-task cap: high-capacity computers can run
// more tasks while smaller hosts remain bounded by memory and CPU.
func LocalNodeTaskSlotsForCapacity(availableBytes int64, logicalCPUs int) int {
	slots := CloudWorkerTaskSlotsForCapacity(availableBytes, logicalCPUs)
	if slots > MaxLocalNodeWorkerConcurrency {
		slots = MaxLocalNodeWorkerConcurrency
	}
	return slots
}
