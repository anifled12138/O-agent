//go:build linux

package projectquota

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	ext4Magic = 0xEF53
	xfsMagic  = 0x58465342
)

// ProbeFilesystem reports mount-level prerequisites only. It deliberately
// never claims a task hard limit: that requires the privileged quota backend
// to assign a project ID, write a limit, and verify it from the kernel.
func ProbeFilesystem(path string) (ProbeResult, error) {
	if strings.TrimSpace(path) == "" {
		return ProbeResult{}, fmt.Errorf("workspace path is required")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return ProbeResult{}, fmt.Errorf("resolve workspace path: %w", err)
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs(resolved, &filesystem); err != nil {
		return ProbeResult{}, fmt.Errorf("read workspace filesystem: %w", err)
	}
	var filesystemName string
	switch uint64(filesystem.Type) {
	case ext4Magic:
		filesystemName = "ext4"
	case xfsMagic:
		filesystemName = "xfs"
	default:
		return ProbeResult{Reason: fmt.Sprintf("filesystem type %#x does not support the configured ext4/XFS project quota backend", uint64(filesystem.Type))}, nil
	}
	contents, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ProbeResult{}, fmt.Errorf("read Linux mount table: %w", err)
	}
	mounts, err := parseMountInfo(string(contents))
	if err != nil {
		return ProbeResult{}, err
	}
	var selected *Mount
	for index := range mounts {
		mount := &mounts[index]
		if !mountContainsPath(mount.MountPoint, resolved) {
			continue
		}
		if selected == nil || len(mount.MountPoint) > len(selected.MountPoint) {
			selected = mount
		}
	}
	if selected == nil {
		return ProbeResult{}, fmt.Errorf("no mount contains workspace path %s", resolved)
	}
	result := ProbeResult{Filesystem: filesystemName, MountPoint: selected.MountPoint, MountSupported: true}
	options := append(append([]string(nil), selected.MountOptions...), selected.SuperOptions...)
	for _, option := range options {
		if option == "prjquota" || option == "pquota" {
			result.QuotaOptionFound = true
			break
		}
	}
	if !result.QuotaOptionFound {
		result.Reason = "project quota mount option prjquota/pquota is not active"
		return result, nil
	}
	result.Reason = "mount supports project quotas; no per-workspace kernel limit has been applied or verified"
	return result, nil
}
