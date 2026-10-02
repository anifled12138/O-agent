//go:build linux

package projectquota

import (
	"testing"
)

func TestProbeFilesystemReadsCurrentMountWithoutClaimingTaskEnforcement(t *testing.T) {
	result, err := ProbeFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if result.HardLimitVerified {
		t.Fatal("mount preflight claimed a task hard limit without an applied quota")
	}
	if result.Reason == "" {
		t.Fatalf("mount preflight returned no reason: %+v", result)
	}
}
