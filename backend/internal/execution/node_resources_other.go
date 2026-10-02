//go:build !linux && !windows

package execution

import "axiom.local/agent/internal/storage"

// Platforms without an authoritative memory reader stay at one task. This is
// a conservative admission ceiling, not a project-size or task-size limit.
func hostNodeResources() (storage.ExecutionNodeResources, error) {
	return storage.ExecutionNodeResources{MaxConcurrentTasks: 1}, nil
}

func NodeResourceSnapshot() (storage.ExecutionNodeResources, error) { return hostNodeResources() }
