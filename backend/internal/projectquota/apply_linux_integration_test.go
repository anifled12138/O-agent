//go:build linux

package projectquota

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This test is deliberately opt-in because it creates a real kernel quota and
// writes to the filesystem. Run it on a disposable ext4/XFS mount with
// prjquota/pquota enabled and a root-owned test invocation:
// O_PROJECT_QUOTA_TEST_ROOT=/mnt/quota-test setpriv --bounding-set=-sys_resource go test ./internal/projectquota -run TestKernelProjectQuotaRejectsWritesBeyondLimit
func TestKernelProjectQuotaRejectsWritesBeyondLimit(t *testing.T) {
	if os.Getenv("O_PROJECT_QUOTA_TEST_ROOT") == "" {
		t.Skip("set O_PROJECT_QUOTA_TEST_ROOT to an isolated quota-enabled mount to run kernel enforcement acceptance")
	}
	if os.Geteuid() != 0 {
		t.Fatal("configured kernel project quota acceptance requires root")
	}
	// Administrative quota writes need CAP_SYS_ADMIN; the payload write must
	// not carry CAP_SYS_RESOURCE, which would bypass the ext4 hard limit.
	var capabilities [2]unix.CapUserData
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capget(&header, &capabilities[0]); err != nil {
		t.Fatalf("read quota acceptance capabilities: %v", err)
	}
	if capabilities[unix.CAP_SYS_RESOURCE/32].Effective&(uint32(1)<<(unix.CAP_SYS_RESOURCE%32)) != 0 {
		t.Fatal("run configured quota acceptance with setpriv --bounding-set=-sys_resource so payload writes cannot bypass the hard limit")
	}
	root, err := filepath.Abs(filepath.Clean(os.Getenv("O_PROJECT_QUOTA_TEST_ROOT")))
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve configured quota test root: %v", err)
	}
	root = resolvedRoot
	if root == string(filepath.Separator) || root == "." {
		t.Fatalf("refusing unsafe quota test root %q", root)
	}
	probe, err := ProbeFilesystem(root)
	if err != nil {
		t.Fatal(err)
	}
	if !probe.MountSupported || !probe.QuotaOptionFound {
		t.Fatalf("configured acceptance mount does not support project quotas: %s", probe.Reason)
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		t.Fatal(err)
	}
	if uint64(stat.Bavail)*uint64(stat.Bsize) < 128<<20 {
		t.Fatal("configured quota test mount needs at least 128 MiB free to distinguish quota rejection from filesystem exhaustion")
	}
	testRoot, err := os.MkdirTemp(root, ".o-projectquota-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(testRoot); err != nil {
			t.Errorf("remove quota acceptance workspace: %v", err)
		}
	})
	if err := os.Mkdir(filepath.Join(testRoot, ".o-projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(testRoot, ".o-projects", "acceptance"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Exercise the same high-bit namespace used by durable O workspace IDs.
	const projectID = uint32(0xe0000000)
	const limit = int64(32 << 20)
	quota, err := ApplyEmptyWorkspace(testRoot, ".o-projects/acceptance", projectID, limit, limit)
	if err != nil {
		t.Fatalf("apply kernel project quota: %v", err)
	}
	if !quota.Applied || quota.LimitBytes != limit {
		t.Fatalf("quota did not read back as applied: %+v", quota)
	}
	verified, err := InspectWorkspace(testRoot, ".o-projects/acceptance", projectID)
	if err != nil || !verified.Applied || verified.LimitBytes != limit {
		t.Fatalf("quota inspection failed: %+v, %v", verified, err)
	}

	file, err := os.OpenFile(filepath.Join(testRoot, ".o-projects", "acceptance", "large.bin"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	var written int64
	var writeErr error
	for written < 64<<20 {
		n, err := file.Write(chunk)
		written += int64(n)
		if err != nil {
			writeErr = err
			break
		}
		if n != len(chunk) {
			writeErr = syscall.EIO
			break
		}
	}
	if syncErr := file.Sync(); syncErr != nil && writeErr == nil {
		writeErr = syncErr
	}
	if closeErr := file.Close(); closeErr != nil && writeErr == nil {
		writeErr = closeErr
	}
	if written >= 64<<20 {
		t.Fatal("kernel accepted writes beyond the 32 MiB project hard limit")
	}
	if writeErr != nil && !errors.Is(writeErr, unix.EDQUOT) && !errors.Is(writeErr, unix.ENOSPC) {
		t.Fatalf("write stopped for a reason other than quota exhaustion: bytes=%d err=%v", written, writeErr)
	}
	verified, err = InspectWorkspace(testRoot, ".o-projects/acceptance", projectID)
	if err != nil || !verified.Applied || verified.LimitBytes != limit || verified.UsedBytes > limit {
		t.Fatalf("quota readback after denied write: %+v, %v", verified, err)
	}
	if err := os.WriteFile(filepath.Join(testRoot, ".o-projects", "unlimited-neighbor"), []byte("still writable"), 0o600); err != nil {
		t.Fatalf("quota unexpectedly blocked neighboring project: %v", err)
	}
	if err := os.Remove(filepath.Join(testRoot, ".o-projects", "acceptance", "large.bin")); err != nil {
		t.Fatal(err)
	}
	// An empty directory must not authorize clearing charges belonging to a
	// different inode with the same project ID, even outside the workspace.
	outsidePath := filepath.Join(testRoot, ".o-projects", "charged-neighbor")
	outside, err := os.OpenFile(outsidePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	chargeErr := func() error {
		attributes, err := readFSXAttr(int(outside.Fd()))
		if err != nil {
			return err
		}
		attributes.ProjectID = projectID
		if err := writeFSXAttr(int(outside.Fd()), attributes); err != nil {
			return err
		}
		if _, err := outside.Write(chunk); err != nil {
			return err
		}
		return outside.Sync()
	}()
	if err := errors.Join(chargeErr, outside.Close()); err != nil {
		t.Fatalf("create external project quota charge: %v", err)
	}
	if err := ReleaseEmptyWorkspace(testRoot, ".o-projects/acceptance", projectID); err == nil {
		t.Fatal("released quota while another inode still carried project usage")
	}
	verified, err = InspectWorkspace(testRoot, ".o-projects/acceptance", projectID)
	if err != nil || !verified.Applied || verified.LimitBytes != limit {
		t.Fatalf("rejected release changed the authoritative quota: %+v, %v", verified, err)
	}
	if err := os.Remove(outsidePath); err != nil {
		t.Fatal(err)
	}
	waitForUnlinkedQuotaCharges(t, testRoot, ".o-projects/acceptance", projectID)
	if err := ReleaseEmptyWorkspace(testRoot, ".o-projects/acceptance", projectID); err != nil {
		t.Fatalf("release empty workspace quota: %v", err)
	}
	quota, err = InspectWorkspace(testRoot, ".o-projects/acceptance", projectID)
	if err != nil || quota.Applied || quota.LimitBytes != 0 || quota.UsedBytes != 0 {
		t.Fatalf("released quota did not read back as cleared: %+v, %v", quota, err)
	}
	if err := ReleaseEmptyWorkspace(testRoot, ".o-projects/acceptance", projectID); err != nil {
		t.Fatalf("releasing the already-cleared quota was not idempotent: %v", err)
	}
}

// Unlink returns before XFS's deferred inode inactivation necessarily retires
// quota charges. Wait for the authoritative kernel usage before testing release;
// never relax the release guard or treat an empty directory as zero usage.
func waitForUnlinkedQuotaCharges(t *testing.T, root, relativePath string, projectID uint32) {
	t.Helper()
	directory, err := os.Open(filepath.Join(root, relativePath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := directory.Close(); err != nil {
			t.Errorf("close quota acceptance directory: %v", err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := unix.Syncfs(int(directory.Fd())); err != nil {
			t.Fatalf("flush quota acceptance filesystem after unlink: %v", err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
			t.Fatal(err)
		}
		quota, err := InspectWorkspace(root, relativePath, projectID)
		if err != nil {
			t.Fatal(err)
		}
		if quota.UsedBytes == stat.Blocks*512 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unlinked quota charges did not retire: project usage=%d bytes, empty directory=%d bytes", quota.UsedBytes, stat.Blocks*512)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
