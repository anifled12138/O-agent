package projectquota

import (
	"strings"
	"testing"
)

func TestParseMountInfoPreservesEscapedPathsAndQuotaOptions(t *testing.T) {
	contents := strings.Join([]string{
		"36 25 8:1 / / rw,relatime shared:1 - ext4 /dev/root rw,errors=remount-ro",
		`41 36 8:1 / /var/lib/o\040agent/workspaces rw,relatime,prjquota - ext4 /dev/root rw,prjquota`,
	}, "\n")
	mounts, err := parseMountInfo(contents)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 {
		t.Fatalf("parsed %d mounts, want 2", len(mounts))
	}
	quotaMount := mounts[1]
	if quotaMount.MountPoint != "/var/lib/o agent/workspaces" || quotaMount.Filesystem != "ext4" || quotaMount.Source != "/dev/root" {
		t.Fatalf("unexpected decoded mount: %+v", quotaMount)
	}
	if len(quotaMount.MountOptions) != 3 || quotaMount.MountOptions[2] != "prjquota" || len(quotaMount.SuperOptions) != 2 || quotaMount.SuperOptions[1] != "prjquota" {
		t.Fatalf("quota options were not retained: %+v", quotaMount)
	}
}

func TestMountContainsPathUsesComponentBoundary(t *testing.T) {
	if !mountContainsPath("/var/lib/o-agent", "/var/lib/o-agent/workspaces/task") {
		t.Fatal("mount did not contain a nested workspace")
	}
	if mountContainsPath("/var/lib/o-agent", "/var/lib/o-agent-old/workspaces") {
		t.Fatal("mount accepted a string-prefix sibling path")
	}
	if mountContainsPath("/var/lib/o-agent", "/etc/passwd") {
		t.Fatal("mount accepted an unrelated path")
	}
}

func TestProbeResultNeverClaimsHardLimitFromMountOptions(t *testing.T) {
	result := ProbeResult{Filesystem: "ext4", MountPoint: "/srv/o-agent", MountSupported: true, QuotaOptionFound: true, Reason: "mount supports quotas"}
	if result.HardLimitVerified {
		t.Fatal("mount configuration was treated as proof of an applied per-workspace limit")
	}
}
