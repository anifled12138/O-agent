package execution

import "axiom.local/agent/internal/domain"

func nodeTaskSlotsForCapacity(availableBytes int64, cpuSlots int) int {
	return domain.LocalNodeTaskSlotsForCapacity(availableBytes, cpuSlots)
}
