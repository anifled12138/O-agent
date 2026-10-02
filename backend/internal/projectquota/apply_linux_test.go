//go:build linux

package projectquota

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestProjectQuotaKernelABIShapes(t *testing.T) {
	if got := unsafe.Sizeof(fsxattr{}); got != 28 {
		t.Fatalf("fsxattr ABI size = %d, want 28", got)
	}
	if got := unsafe.Sizeof(ifDqblk{}); got != 72 {
		t.Fatalf("if_dqblk ABI size = %d, want 72", got)
	}
	if got := unsafe.Sizeof(xfsDiskQuota{}); got != 112 {
		t.Fatalf("fs_disk_quota ABI size = %d, want 112", got)
	}
	if got := qcmd(qSetQuota, projectQuotaType); got != 0x80000802 {
		t.Fatalf("ext4 project quota command = %#x", got)
	}
}

func TestQuotaBlockLimitRoundsUpWithoutOverflow(t *testing.T) {
	cases := []struct {
		filesystem string
		bytes      int64
		want       uint64
	}{
		{filesystem: "ext4", bytes: 1, want: 1},
		{filesystem: "ext4", bytes: 1024, want: 1},
		{filesystem: "ext4", bytes: 1025, want: 2},
		{filesystem: "xfs", bytes: 511, want: 1},
		{filesystem: "xfs", bytes: 512, want: 1},
		{filesystem: "xfs", bytes: 513, want: 2},
	}
	for _, test := range cases {
		got, err := quotaBlockLimit(test.filesystem, test.bytes)
		if err != nil || got != test.want {
			t.Errorf("quotaBlockLimit(%s,%d) = %d, %v; want %d", test.filesystem, test.bytes, got, err, test.want)
		}
	}
	if got, err := quotaBlockLimit("ext4", 0); err != nil || got != 0 {
		t.Fatalf("zero limit for release = %d, %v", got, err)
	}
	if _, err := quotaBlockLimit("ext4", -1); err == nil {
		t.Fatal("negative quota limit was accepted")
	}
	if _, err := quotaBlockLimit("btrfs", 1024); err == nil {
		t.Fatal("unsupported filesystem was accepted")
	}
	if got, err := quotaBlockLimit("ext4", int64(^uint64(0)>>1)); err != nil || got == 0 {
		t.Fatalf("maximum signed quota limit overflowed: %d, %v", got, err)
	}
}

func TestQuotaRelativePathRejectsTraversalAndLinks(t *testing.T) {
	for _, value := range []string{"", ".", "../escape", "a/../escape", "/absolute", `a\b`, "a//b"} {
		if err := validateQuotaRelativePath(value); err == nil {
			t.Errorf("accepted invalid quota path %q", value)
		}
	}
	if err := validateQuotaRelativePath(".o-projects/scratch-0123456789abcdef"); err != nil {
		t.Fatalf("rejected canonical task workspace path: %v", err)
	}
}

func TestOpenWorkspaceDirectoryDoesNotFollowSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, ".o-projects")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := openWorkspaceDirectory(root, ".o-projects/task-dir"); err == nil {
		t.Fatal("workspace quota helper followed a symlink parent")
	}
}

func TestDirectoryIsEmptyReadsThroughPinnedDescriptor(t *testing.T) {
	root := t.TempDir()
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	empty, err := directoryIsEmpty(fd)
	if err != nil || !empty {
		t.Fatalf("empty directory state = %v, %v", empty, err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err = unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	empty, err = directoryIsEmpty(fd)
	if err != nil || empty {
		t.Fatalf("nonempty directory state = %v, %v", empty, err)
	}
}

func TestApplyEmptyWorkspaceRejectsInvalidQuotaBeforeFilesystemMutation(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, ".o-projects", "task")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEmptyWorkspace(root, ".o-projects/task", 0, 4096, 1<<20); err == nil {
		t.Fatal("accepted reserved project ID zero")
	}
	if _, err := ApplyEmptyWorkspace(root, ".o-projects/task", 1234, 2<<20, 1<<20); err == nil {
		t.Fatal("accepted a limit above the root-configured maximum")
	}
	entries, err := os.ReadDir(workspace)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid requests mutated the reserved workspace: entries=%v err=%v", entries, err)
	}
}
