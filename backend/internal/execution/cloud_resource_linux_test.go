//go:build linux

package execution

import (
	"os"
	"path/filepath"
	"testing"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/resourceadmission"
)

func TestCloudSlotsUseAvailableMemoryAndCPUHeadroom(t *testing.T) {
	const mib = int64(1 << 20)
	tests := []struct {
		name      string
		available int64
		cpus      int
		want      int
	}{
		{name: "4 GiB host admits two maximum sandboxes", available: 4 * 1024 * mib, cpus: 4, want: 2},
		{name: "memory pressure lowers admission", available: 2 * 1024 * mib, cpus: 4, want: 1},
		{name: "CPU count caps high-memory host", available: 32 * 1024 * mib, cpus: 4, want: 4},
		{name: "preserve control plane reserve", available: domain.LocalNodeControlPlaneReserveBytes, cpus: 4, want: 0},
		{name: "no CPU capacity", available: 8 * 1024 * mib, cpus: 0, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cloudSlotsForCapacity(test.available, test.cpus); got != test.want {
				t.Fatalf("cloudSlotsForCapacity(%d, %d) = %d, want %d", test.available, test.cpus, got, test.want)
			}
		})
	}
}

func TestBrowserMemoryReservationReducesCloudTaskAdmission(t *testing.T) {
	release := resourceadmission.ReserveMemory(domain.CloudTaskSandboxMemoryLimitBytes)
	defer release()
	availableAfterReservation := subtractReservedMemory(2 << 30)
	if got := cloudSlotsForCapacity(availableAfterReservation, 4); got != 0 {
		t.Fatalf("cloud slots with browser reservation = %d, want 0", got)
	}
}

func TestCgroupMemoryLimitAndUsageConstrainAdmission(t *testing.T) {
	directory := t.TempDir()
	limitPath := filepath.Join(directory, "memory.max")
	if err := os.WriteFile(limitPath, []byte("2147483648\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	limit, constrained, err := cgroupMemoryLimit(directory)
	if err != nil || !constrained || limit != 2147483648 {
		t.Fatalf("cgroup memory limit = %d, constrained=%v, err=%v", limit, constrained, err)
	}
	currentPath := filepath.Join(directory, "memory.current")
	if err := os.WriteFile(currentPath, []byte("536870912\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	used, err := readCgroupBytes(currentPath)
	if err != nil || used != 536870912 {
		t.Fatalf("cgroup memory usage = %d, err=%v", used, err)
	}
	if got := cgroupRemainingBytes(limit, used); got != 1610612736 {
		t.Fatalf("cgroup available memory = %d, want 1610612736", got)
	}
	if err := os.WriteFile(limitPath, []byte("max\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, constrained, err := cgroupMemoryLimit(directory); err != nil || constrained {
		t.Fatalf("unlimited cgroup memory.max should not constrain admission: constrained=%v err=%v", constrained, err)
	}
}
