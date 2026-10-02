package domain

import "testing"

func TestCloudAndLocalTaskSlotPoliciesFollowLiveCapacity(t *testing.T) {
	availableForFour := LocalNodeControlPlaneReserveBytes + 4*CloudTaskSandboxMemoryLimitBytes
	if got := CloudWorkerTaskSlotsForCapacity(availableForFour, 8); got != 4 {
		t.Fatalf("cloud slots = %d, want 4 when memory and CPU permit four tasks", got)
	}
	if got := LocalNodeTaskSlotsForCapacity(availableForFour, 8); got != 4 {
		t.Fatalf("local node slots = %d, want 4 when memory and CPU permit four tasks", got)
	}
	if got := LocalNodeTaskSlotsForCapacity(LocalNodeControlPlaneReserveBytes+CloudTaskSandboxMemoryLimitBytes-1, 8); got != 0 {
		t.Fatalf("local node admitted %d tasks below one full task's memory headroom", got)
	}
	if got := LocalNodeTaskSlotsForCapacity(availableForFour, 1); got != 1 {
		t.Fatalf("local node ignored CPU capacity: got %d slots, want 1", got)
	}
	availableForHundred := LocalNodeControlPlaneReserveBytes + 100*CloudTaskSandboxMemoryLimitBytes
	if got := LocalNodeTaskSlotsForCapacity(availableForHundred, 128); got != MaxLocalNodeWorkerConcurrency {
		t.Fatalf("local node scheduler ceiling = %d, want %d", got, MaxLocalNodeWorkerConcurrency)
	}
}

func TestCloudTaskAdmissionAlsoReservesConfiguredWorkspaceDiskBudgets(t *testing.T) {
	const gib = int64(1 << 30)
	const taskDisk = 8 * gib
	const reserve = 4 * gib
	if got := CloudWorkerTaskSlotsForResources(4*gib, 40*gib, taskDisk, reserve, 4); got != 2 {
		t.Fatalf("4 GiB memory / 40 GiB disk host admitted %d tasks, want memory-limited 2", got)
	}
	if got := CloudWorkerTaskSlotsForResources(16*gib, 19*gib, taskDisk, reserve, 8); got != 1 {
		t.Fatalf("disk headroom admitted %d tasks, want one quota-sized task", got)
	}
	if got := CloudWorkerTaskSlotsForResources(16*gib, reserve+taskDisk-1, taskDisk, reserve, 8); got != 0 {
		t.Fatalf("admitted %d tasks below one configured disk budget", got)
	}
	if got := CloudWorkerTaskSlotsForResources(16*gib, 64*gib, 0, reserve, 8); got != 0 {
		t.Fatalf("admitted %d cloud tasks without a positive disk budget", got)
	}
}

func TestSafeAgentContinuationStopReasons(t *testing.T) {
	for _, reason := range []string{"step_limit", "model_call_limit", "stalled", "handoff_requested"} {
		if !SafeAgentContinuationStopReason(reason) {
			t.Errorf("safe continuation stop reason %q rejected", reason)
		}
	}
	for _, reason := range []string{"", "cancelled", "tool_effect_unknown", "failed", "interrupted"} {
		if SafeAgentContinuationStopReason(reason) {
			t.Errorf("unsafe continuation stop reason %q accepted", reason)
		}
	}
}
