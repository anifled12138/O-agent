//go:build linux

package projectquota

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	fsIOCFSGetXAttr    = 0x801c581f
	fsIOCFSSetXAttr    = 0x401c5820
	fsXFlagProjInherit = 0x00000200
	qGetQuota          = 0x800007
	qSetQuota          = 0x800008
	projectQuotaType   = 2
	qifBlockLimits     = 1
	xqmGetQuota        = 0x5803
	xqmSetQuotaLimit   = 0x5804
	xfsProjectQuota    = 2
	xfsHardBlockLimit  = 1 << 3
	xfsSoftBlockLimit  = 1 << 2
)

type fsxattr struct {
	XFlags     uint32
	ExtSize    uint32
	Nextents   uint32
	ProjectID  uint32
	CowExtSize uint32
	Padding    [8]byte
}

type ifDqblk struct {
	BlockHardLimit uint64
	BlockSoftLimit uint64
	CurrentSpace   uint64
	InodeHardLimit uint64
	InodeSoftLimit uint64
	CurrentInodes  uint64
	BlockTime      uint64
	InodeTime      uint64
	Valid          uint32
	Padding        uint32
}

type xfsDiskQuota struct {
	Version         int8
	Flags           int8
	FieldMask       uint16
	ID              uint32
	BlockHardLimit  uint64
	BlockSoftLimit  uint64
	InodeHardLimit  uint64
	InodeSoftLimit  uint64
	BlockCount      uint64
	InodeCount      uint64
	InodeTimer      int32
	BlockTimer      int32
	InodeWarnings   uint16
	BlockWarnings   uint16
	InodeTimerHigh  int8
	BlockTimerHigh  int8
	RTBlockTimerHi  int8
	Padding2        int8
	RTBlockHard     uint64
	RTBlockSoft     uint64
	RTBlockCount    uint64
	RTBlockTimer    int32
	RTBlockWarnings uint16
	Padding3        int16
	Padding4        [8]byte
}

// ApplyEmptyWorkspace applies a kernel-enforced project block quota to an
// already-created empty task directory. It must be called by the narrow,
// privileged host helper; model-controlled commands must never call it.
func ApplyEmptyWorkspace(workspaceRoot, relativePath string, projectID uint32, limitBytes, maximumBytes int64) (WorkspaceQuota, error) {
	if projectID == 0 || limitBytes <= 0 || maximumBytes <= 0 || limitBytes > maximumBytes {
		return WorkspaceQuota{}, errors.New("project ID and workspace limit are outside the configured range")
	}
	root, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if err := validateQuotaRelativePath(relativePath); err != nil {
		return WorkspaceQuota{}, err
	}
	directoryFD, err := openWorkspaceDirectory(root, relativePath)
	if err != nil {
		return WorkspaceQuota{}, err
	}
	directory := os.NewFile(uintptr(directoryFD), "task-workspace")
	if directory == nil {
		unix.Close(directoryFD)
		return WorkspaceQuota{}, errors.New("open task workspace directory stream")
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if len(entries) != 0 {
		return WorkspaceQuota{}, errors.New("project quota must be applied before workspace files are created")
	}
	probe, err := ProbeFilesystem(fmt.Sprintf("/proc/self/fd/%d", directoryFD))
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if !probe.MountSupported || !probe.QuotaOptionFound {
		return WorkspaceQuota{}, fmt.Errorf("project quota mount is unavailable: %s", probe.Reason)
	}
	previous, err := readFSXAttr(directoryFD)
	if err != nil {
		return WorkspaceQuota{}, fmt.Errorf("read workspace project attributes: %w", err)
	}
	if previous.ProjectID != 0 && previous.ProjectID != projectID {
		return WorkspaceQuota{}, errors.New("empty workspace already belongs to a different project quota")
	}
	if err := setProjectQuota(probe, projectID, limitBytes); err != nil {
		return WorkspaceQuota{}, fmt.Errorf("apply filesystem project quota: %w", err)
	}
	updated := previous
	updated.ProjectID = projectID
	updated.XFlags |= fsXFlagProjInherit
	if err := writeFSXAttr(directoryFD, updated); err != nil {
		return WorkspaceQuota{}, rollbackQuota(probe, projectID, previous, directoryFD, fmt.Errorf("set workspace project ID and inheritance: %w", err))
	}
	verified, err := InspectWorkspace(workspaceRoot, relativePath, projectID)
	if err != nil {
		return WorkspaceQuota{}, rollbackQuota(probe, projectID, previous, directoryFD, fmt.Errorf("read back applied workspace quota: %w", err))
	}
	if !verified.Applied || verified.LimitBytes != limitBytes {
		return WorkspaceQuota{}, rollbackQuota(probe, projectID, previous, directoryFD, errors.New("filesystem quota did not read back the requested hard limit"))
	}
	return verified, nil
}

// InspectWorkspace reads both directory project attributes and the filesystem
// quota record. A successful command or mount option alone is never reported
// as an applied workspace hard limit.
func InspectWorkspace(workspaceRoot, relativePath string, projectID uint32) (WorkspaceQuota, error) {
	if projectID == 0 {
		return WorkspaceQuota{}, errors.New("project ID must be nonzero")
	}
	root, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if err := validateQuotaRelativePath(relativePath); err != nil {
		return WorkspaceQuota{}, err
	}
	directoryFD, err := openWorkspaceDirectory(root, relativePath)
	if err != nil {
		return WorkspaceQuota{}, err
	}
	defer unix.Close(directoryFD)
	probe, err := ProbeFilesystem(fmt.Sprintf("/proc/self/fd/%d", directoryFD))
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if !probe.MountSupported || !probe.QuotaOptionFound {
		return WorkspaceQuota{}, fmt.Errorf("project quota mount is unavailable: %s", probe.Reason)
	}
	attributes, err := readFSXAttr(directoryFD)
	if err != nil {
		return WorkspaceQuota{}, fmt.Errorf("read workspace project attributes: %w", err)
	}
	quota, err := getProjectQuota(probe, projectID)
	if err != nil {
		return WorkspaceQuota{}, fmt.Errorf("read kernel project quota: %w", err)
	}
	limitBytes, usedBytes := quotaBytes(probe.Filesystem, quota)
	result := WorkspaceQuota{Filesystem: probe.Filesystem, MountPoint: probe.MountPoint, ProjectID: projectID, LimitBytes: limitBytes, UsedBytes: usedBytes}
	result.Applied = attributes.ProjectID == projectID && attributes.XFlags&fsXFlagProjInherit != 0 && limitBytes > 0
	return result, nil
}

// ReleaseEmptyWorkspace clears an already-verified quota only when its task
// directory is empty. Callers must additionally verify durable task terminal
// state, lease expiry, and absence of checkpoint/outbox references.
func ReleaseEmptyWorkspace(workspaceRoot, relativePath string, projectID uint32) error {
	_, err := ReleaseTaskWorkspace(workspaceRoot, relativePath, projectID)
	return err
}

// ReleaseTaskWorkspace clears a quota after its empty task directory has been
// removed, or clears an empty existing directory. A missing directory is
// accepted only after the kernel reports zero project usage for the ID. An
// existing empty directory may carry only its own allocated blocks; any other
// charge prevents release, and final quota usage must read back as zero.
func ReleaseTaskWorkspace(workspaceRoot, relativePath string, projectID uint32) (WorkspaceQuota, error) {
	if projectID == 0 {
		return WorkspaceQuota{}, errors.New("project ID must be nonzero")
	}
	root, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if err := validateQuotaRelativePath(relativePath); err != nil {
		return WorkspaceQuota{}, err
	}
	directoryFD, err := openWorkspaceDirectory(root, relativePath)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, os.ErrNotExist) {
		return clearQuotaWithoutWorkspace(root, projectID)
	}
	if err != nil {
		return WorkspaceQuota{}, err
	}
	defer unix.Close(directoryFD)
	empty, err := directoryIsEmpty(directoryFD)
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if !empty {
		return WorkspaceQuota{}, errors.New("cannot release a workspace quota while files remain in the task directory")
	}
	probe, err := ProbeFilesystem(fmt.Sprintf("/proc/self/fd/%d", directoryFD))
	if err != nil {
		return WorkspaceQuota{}, err
	}
	attributes, err := readFSXAttr(directoryFD)
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if attributes.ProjectID != 0 && attributes.ProjectID != projectID {
		return WorkspaceQuota{}, errors.New("empty workspace belongs to a different quota project ID")
	}
	quota, err := getProjectQuota(probe, projectID)
	if err != nil {
		return WorkspaceQuota{}, fmt.Errorf("read project quota before release: %w", err)
	}
	currentLimit, currentUsage := quotaBytes(probe.Filesystem, quota)
	if currentUsage != 0 {
		// ext4 charges the empty directory's own allocated blocks to its
		// project. Clearing its project ID transfers those blocks back to
		// project zero; any charge beyond this directory must remain protected.
		var stat unix.Stat_t
		if err := unix.Fstat(directoryFD, &stat); err != nil {
			return WorkspaceQuota{}, fmt.Errorf("inspect empty workspace block usage before release: %w", err)
		}
		if attributes.ProjectID != projectID || currentUsage != stat.Blocks*512 {
			return WorkspaceQuota{}, errors.New("cannot release a task project quota while usage outside the empty directory remains")
		}
	}
	if currentLimit == 0 && attributes.ProjectID == 0 && attributes.XFlags&fsXFlagProjInherit == 0 {
		return WorkspaceQuota{Filesystem: probe.Filesystem, MountPoint: probe.MountPoint, ProjectID: projectID}, nil
	}
	if err := setProjectQuota(probe, projectID, 0); err != nil {
		return WorkspaceQuota{}, fmt.Errorf("clear filesystem project quota: %w", err)
	}
	cleared := attributes
	if attributes.ProjectID == projectID {
		cleared.ProjectID = 0
		cleared.XFlags &^= fsXFlagProjInherit
		if err := writeFSXAttr(directoryFD, cleared); err != nil {
			rollbackErr := errors.Join(setProjectQuota(probe, projectID, currentLimit), writeFSXAttr(directoryFD, attributes))
			return WorkspaceQuota{}, errors.Join(fmt.Errorf("clear workspace project attributes: %w", err), rollbackErr)
		}
	}
	quota, err = getProjectQuota(probe, projectID)
	if err != nil {
		rollbackErr := setProjectQuota(probe, projectID, currentLimit)
		if attributes.ProjectID == projectID {
			rollbackErr = errors.Join(rollbackErr, writeFSXAttr(directoryFD, attributes))
		}
		return WorkspaceQuota{}, errors.Join(fmt.Errorf("verify released filesystem project quota: %w", err), rollbackErr)
	}
	limitBytes, usedBytes := quotaBytes(probe.Filesystem, quota)
	if limitBytes != 0 || usedBytes != 0 {
		rollbackErr := setProjectQuota(probe, projectID, currentLimit)
		if attributes.ProjectID == projectID {
			rollbackErr = errors.Join(rollbackErr, writeFSXAttr(directoryFD, attributes))
		}
		return WorkspaceQuota{}, errors.Join(errors.New("workspace quota release did not verify from filesystem read-back"), rollbackErr)
	}
	if attributes.ProjectID == projectID {
		readBack, readErr := readFSXAttr(directoryFD)
		if readErr != nil || readBack.ProjectID != 0 || readBack.XFlags&fsXFlagProjInherit != 0 {
			rollbackErr := errors.Join(setProjectQuota(probe, projectID, currentLimit), writeFSXAttr(directoryFD, attributes))
			return WorkspaceQuota{}, errors.Join(errors.New("workspace project attributes failed release read-back"), readErr, rollbackErr)
		}
	}
	return WorkspaceQuota{Filesystem: probe.Filesystem, MountPoint: probe.MountPoint, ProjectID: projectID, UsedBytes: usedBytes}, nil
}

func clearQuotaWithoutWorkspace(root string, projectID uint32) (WorkspaceQuota, error) {
	probe, err := ProbeFilesystem(root)
	if err != nil {
		return WorkspaceQuota{}, err
	}
	if !probe.MountSupported || !probe.QuotaOptionFound {
		return WorkspaceQuota{}, fmt.Errorf("project quota mount is unavailable: %s", probe.Reason)
	}
	quota, err := getProjectQuota(probe, projectID)
	if err != nil {
		return WorkspaceQuota{}, fmt.Errorf("read orphaned task quota before release: %w", err)
	}
	limitBytes, usedBytes := quotaBytes(probe.Filesystem, quota)
	if usedBytes != 0 {
		return WorkspaceQuota{}, fmt.Errorf("cannot release removed workspace project %d with %d bytes still charged", projectID, usedBytes)
	}
	originalLimit := limitBytes
	if originalLimit != 0 {
		if err := setProjectQuota(probe, projectID, 0); err != nil {
			return WorkspaceQuota{}, fmt.Errorf("clear orphaned task quota: %w", err)
		}
	}
	quota, err = getProjectQuota(probe, projectID)
	if err != nil {
		rollbackErr := setProjectQuota(probe, projectID, originalLimit)
		return WorkspaceQuota{}, errors.Join(fmt.Errorf("read back orphaned task quota release: %w", err), rollbackErr)
	}
	limitBytes, usedBytes = quotaBytes(probe.Filesystem, quota)
	if limitBytes != 0 || usedBytes != 0 {
		rollbackErr := setProjectQuota(probe, projectID, originalLimit)
		return WorkspaceQuota{}, errors.Join(errors.New("orphaned task quota did not read back as cleared"), rollbackErr)
	}
	return WorkspaceQuota{Filesystem: probe.Filesystem, MountPoint: probe.MountPoint, ProjectID: projectID}, nil
}

func validateQuotaRelativePath(relativePath string) error {
	if relativePath == "" || strings.Contains(relativePath, "\\") || filepath.IsAbs(relativePath) {
		return errors.New("workspace quota path must be a clean relative POSIX path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relativePath)))
	if clean != relativePath || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return errors.New("workspace quota path escapes or is not canonical")
	}
	return nil
}

func directoryIsEmpty(directoryFD int) (bool, error) {
	buffer := make([]byte, 8192)
	for {
		n, err := unix.ReadDirent(directoryFD, buffer)
		if err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
		_, _, names := unix.ParseDirent(buffer[:n], -1, nil)
		for _, name := range names {
			if name != "." && name != ".." {
				return false, nil
			}
		}
	}
}

func openWorkspaceDirectory(root, relativePath string) (int, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("open trusted workspace root: %w", err)
	}
	currentFD := rootFD
	components := strings.Split(relativePath, "/")
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			unix.Close(currentFD)
			return -1, errors.New("workspace quota path contains an invalid component")
		}
		flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		nextFD, openErr := unix.Openat(currentFD, component, flags, 0)
		unix.Close(currentFD)
		if openErr != nil {
			return -1, fmt.Errorf("open workspace path component %q without following links: %w", component, openErr)
		}
		currentFD = nextFD
		if index == len(components)-1 {
			return currentFD, nil
		}
	}
	unix.Close(currentFD)
	return -1, errors.New("workspace quota path has no final directory")
}

func readFSXAttr(fd int) (fsxattr, error) {
	var attributes fsxattr
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), fsIOCFSGetXAttr, uintptr(unsafe.Pointer(&attributes)))
	if errno != 0 {
		return fsxattr{}, errno
	}
	return attributes, nil
}

func writeFSXAttr(fd int, attributes fsxattr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), fsIOCFSSetXAttr, uintptr(unsafe.Pointer(&attributes)))
	if errno != 0 {
		return errno
	}
	readBack, err := readFSXAttr(fd)
	if err != nil {
		return err
	}
	if readBack.ProjectID != attributes.ProjectID || readBack.XFlags&fsXFlagProjInherit != attributes.XFlags&fsXFlagProjInherit {
		return errors.New("filesystem project attributes failed read-back verification")
	}
	return nil
}

func setProjectQuota(probe ProbeResult, projectID uint32, limitBytes int64) error {
	blocks, err := quotaBlockLimit(probe.Filesystem, limitBytes)
	if err != nil {
		return err
	}
	switch probe.Filesystem {
	case "ext4":
		quota := ifDqblk{BlockHardLimit: blocks, BlockSoftLimit: blocks, Valid: qifBlockLimits}
		return quotaControlAtMount(probe.MountPoint, qcmd(qSetQuota, projectQuotaType), projectID, unsafe.Pointer(&quota))
	case "xfs":
		quota := xfsDiskQuota{Version: 1, Flags: xfsProjectQuota, FieldMask: xfsHardBlockLimit | xfsSoftBlockLimit, ID: projectID, BlockHardLimit: blocks, BlockSoftLimit: blocks}
		return quotaControlAtMount(probe.MountPoint, qcmd(xqmSetQuotaLimit, projectQuotaType), projectID, unsafe.Pointer(&quota))
	default:
		return fmt.Errorf("unsupported project quota filesystem %q", probe.Filesystem)
	}
}

type projectQuotaRecord struct {
	ext4 *ifDqblk
	xfs  *xfsDiskQuota
}

func getProjectQuota(probe ProbeResult, projectID uint32) (projectQuotaRecord, error) {
	switch probe.Filesystem {
	case "ext4":
		var quota ifDqblk
		if err := quotaControlAtMount(probe.MountPoint, qcmd(qGetQuota, projectQuotaType), projectID, unsafe.Pointer(&quota)); err != nil {
			return projectQuotaRecord{}, err
		}
		return projectQuotaRecord{ext4: &quota}, nil
	case "xfs":
		quota := xfsDiskQuota{Version: 1, Flags: xfsProjectQuota, ID: projectID}
		if err := quotaControlAtMount(probe.MountPoint, qcmd(xqmGetQuota, projectQuotaType), projectID, unsafe.Pointer(&quota)); err != nil {
			return projectQuotaRecord{}, err
		}
		return projectQuotaRecord{xfs: &quota}, nil
	default:
		return projectQuotaRecord{}, fmt.Errorf("unsupported project quota filesystem %q", probe.Filesystem)
	}
}

// quotactl requires a block-device path, not a mount directory. Linux 5.14+
// quotactl_fd addresses the mounted filesystem directly without resolving its
// backing device, and uses QCMD encoding for both ext4 and XFS operations.
func quotaControlAtMount(mountPoint string, command uintptr, projectID uint32, record unsafe.Pointer) error {
	fd, err := unix.Open(mountPoint, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open quota mount: %w", err)
	}
	_, _, errno := unix.Syscall6(unix.SYS_QUOTACTL_FD, uintptr(fd), command, uintptr(projectID), uintptr(record), 0, 0)
	closeErr := unix.Close(fd)
	if errno != 0 {
		return errors.Join(errno, closeErr)
	}
	return closeErr
}
func quotaBytes(filesystem string, quota projectQuotaRecord) (int64, int64) {
	switch filesystem {
	case "ext4":
		if quota.ext4 == nil {
			return 0, 0
		}
		value := quota.ext4
		if value.BlockHardLimit > uint64(math.MaxInt64/1024) {
			return math.MaxInt64, int64(value.CurrentSpace)
		}
		return int64(value.BlockHardLimit) * 1024, int64(value.CurrentSpace)
	case "xfs":
		if quota.xfs == nil {
			return 0, 0
		}
		value := quota.xfs
		if value.BlockHardLimit > uint64(math.MaxInt64/512) || value.BlockCount > uint64(math.MaxInt64/512) {
			return math.MaxInt64, math.MaxInt64
		}
		return int64(value.BlockHardLimit) * 512, int64(value.BlockCount) * 512
	default:
		return 0, 0
	}
}

func quotaBlockLimit(filesystem string, limitBytes int64) (uint64, error) {
	if limitBytes < 0 {
		return 0, errors.New("quota limit cannot be negative")
	}
	if limitBytes == 0 {
		return 0, nil
	}
	blockSize := int64(1024)
	if filesystem == "xfs" {
		blockSize = 512
	} else if filesystem != "ext4" {
		return 0, fmt.Errorf("unsupported project quota filesystem %q", filesystem)
	}
	blocks := limitBytes / blockSize
	if limitBytes%blockSize != 0 {
		blocks++
	}
	return uint64(blocks), nil
}

func qcmd(command, quotaType int) uintptr {
	return uintptr((command << 8) | (quotaType & 0xff))
}

func rollbackQuota(probe ProbeResult, projectID uint32, previous fsxattr, fd int, cause error) error {
	var rollbackErrors []error
	if err := setProjectQuota(probe, projectID, 0); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("remove partial quota limit: %w", err))
	}
	if err := writeFSXAttr(fd, previous); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("restore original project attributes: %w", err))
	}
	return errors.Join(cause, errors.Join(rollbackErrors...))
}
