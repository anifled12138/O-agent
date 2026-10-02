//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func nativeFileIdentity(path string) (volumeSerial uint32, fileIndex uint64, resultErr error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return 0, 0, err
	}
	if strings.HasPrefix(absolute, `\\`) {
		return 0, 0, fmt.Errorf("UNC and device paths are unsupported by the Windows native sandbox: %s", absolute)
	}
	volume := filepath.VolumeName(absolute)
	if len(volume) != 2 || volume[1] != ':' {
		return 0, 0, fmt.Errorf("unsupported Windows filesystem path: %s", absolute)
	}
	root := volume + string(filepath.Separator)
	root16, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, err
	}
	if driveType := windows.GetDriveType(root16); driveType != windows.DRIVE_FIXED {
		return 0, 0, fmt.Errorf("Windows native sandbox requires a local fixed drive: %s", root)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return 0, 0, fmt.Errorf("filesystem path escapes its local volume: %s", absolute)
	}
	components := []string{root}
	current := root
	if relative != "." {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			if component == "" || component == "." {
				continue
			}
			current = filepath.Join(current, component)
			components = append(components, current)
		}
	}
	var final windows.Handle
	defer func() {
		if final == 0 {
			return
		}
		if err := windows.CloseHandle(final); err != nil {
			volumeSerial = 0
			fileIndex = 0
			resultErr = errors.Join(resultErr, fmt.Errorf("close filesystem identity handle: %w", err))
		}
	}()
	for _, component := range components {
		component16, err := windows.UTF16PtrFromString(component)
		if err != nil {
			return 0, 0, err
		}
		handle, err := windows.CreateFile(component16, windows.FILE_READ_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return 0, 0, fmt.Errorf("open filesystem path component %s: %w", component, err)
		}
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			return 0, 0, errors.Join(fmt.Errorf("read filesystem identity for %s: %w", component, err), windows.CloseHandle(handle))
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return 0, 0, errors.Join(fmt.Errorf("reparse points are unsupported in sandbox paths: %s", component), windows.CloseHandle(handle))
		}
		if final != 0 {
			if err := windows.CloseHandle(final); err != nil {
				final = 0
				return 0, 0, errors.Join(fmt.Errorf("close prior filesystem identity handle: %w", err), windows.CloseHandle(handle))
			}
		}
		final = handle
	}
	var fileSystem [32]uint16
	if err := windows.GetVolumeInformationByHandle(final, nil, 0, nil, nil, nil, &fileSystem[0], uint32(len(fileSystem))); err != nil {
		return 0, 0, fmt.Errorf("read filesystem type for %s: %w", absolute, err)
	}
	if !strings.EqualFold(windows.UTF16ToString(fileSystem[:]), "NTFS") {
		return 0, 0, fmt.Errorf("Windows native sandbox requires NTFS; %s is %q", absolute, windows.UTF16ToString(fileSystem[:]))
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(final, &info); err != nil {
		return 0, 0, fmt.Errorf("read filesystem identity for %s: %w", absolute, err)
	}
	index := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	if info.VolumeSerialNumber == 0 || index == 0 {
		return 0, 0, fmt.Errorf("filesystem identity for %s is incomplete", absolute)
	}
	return info.VolumeSerialNumber, index, nil
}

func canonicalNativeDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if _, _, err := nativeFileIdentity(absolute); err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return filepath.Clean(absolute), nil
}

func nativeEnvironmentRuntimePaths(environment map[string]string) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, entry := range filepath.SplitList(environment["PATH"]) {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		resolved, err := canonicalNativeDirectory(entry)
		if err != nil || isSensitiveRuntimePath(resolved) || isBroadRuntimePath(resolved) || isSystemRuntimePath(resolved) {
			continue
		}
		key := strings.ToLower(resolved)
		if !seen[key] {
			seen[key] = true
			paths = append(paths, resolved)
		}
	}
	return paths
}

// setNativeAccess binds the ACL change to the filesystem object opened by the
// host and compares that object with the durable journal identity. This keeps
// cleanup from changing a different object after a rename or reparse attack.
func setNativeAccess(path string, sid *windows.SID, accessMode windows.ACCESS_MODE, write, deny bool, volume uint32, index uint64) (resultErr error) {
	aclMutationMu.Lock()
	defer aclMutationMu.Unlock()

	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(path16, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("open filesystem object for ACL update: %w", err)
	}
	defer func() {
		if err := windows.CloseHandle(handle); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close filesystem object after ACL update: %w", err))
		}
	}()

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("read filesystem identity before ACL update: %w", err)
	}
	actualIndex := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.VolumeSerialNumber != volume || actualIndex != index {
		return errors.New("filesystem object identity changed before ACL update")
	}
	current, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read object DACL: %w", err)
	}
	if current == nil {
		return errors.New("filesystem object has no security descriptor")
	}
	permissions := windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE)
	if deny {
		permissions = 0
	}
	if write {
		// Grant DELETE on each object so files can be removed or renamed, but
		// never grant FILE_DELETE_CHILD on the parent. The latter would let a
		// workspace write grant bypass a deny ACE on a protected Git config file.
		permissions |= windows.ACCESS_MASK(windows.FILE_GENERIC_WRITE | windows.DELETE)
	}
	if accessMode == windows.SET_ACCESS && deny {
		accessMode = windows.DENY_ACCESS
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: permissions,
		AccessMode:        accessMode,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	updated, err := windows.BuildSecurityDescriptor(nil, nil, []windows.EXPLICIT_ACCESS{entry}, nil, current)
	if err != nil {
		return fmt.Errorf("build updated object DACL: %w", err)
	}
	dacl, _, err := updated.DACL()
	if err != nil {
		return fmt.Errorf("read updated object DACL: %w", err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("write object DACL: %w", err)
	}
	readBack, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read back object DACL: %w", err)
	}
	if readBack == nil {
		return errors.New("filesystem object ACL read-back returned no security descriptor")
	}
	actualDACL, _, err := readBack.DACL()
	if err != nil {
		return fmt.Errorf("read back filesystem object DACL: %w", err)
	}
	if accessMode == windows.REVOKE_ACCESS {
		for _, denyACE := range []bool{false, true} {
			hasSID, err := explicitACLHasSID(actualDACL, sid, denyACE)
			if err != nil {
				return fmt.Errorf("verify filesystem object ACL read-back: %w", err)
			}
			if hasSID {
				return errors.New("filesystem object DACL still contains an explicit command logon SID entry")
			}
		}
	} else {
		if hasSID, err := explicitACLHasSID(actualDACL, sid, deny); err != nil {
			return fmt.Errorf("verify filesystem object ACL read-back: %w", err)
		} else if !hasSID {
			return errors.New("filesystem object DACL read-back did not confirm the requested logon SID entry")
		}
	}
	return nil
}

func explicitACLHasSID(dacl *windows.ACL, sid *windows.SID, deny bool) (bool, error) {
	if dacl == nil {
		return false, nil
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return false, err
		}
		if ace == nil || ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			continue
		}
		if deny && ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE || !deny && ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if aceSID.IsValid() && aceSID.Equals(sid) {
			return true, nil
		}
	}
	return false, nil
}

func containsSID(descriptor, sid string) bool {
	return strings.Contains(strings.ToUpper(descriptor), strings.ToUpper(sid))
}

func verifyNativeSIDAbsentTree(root string, sid *windows.SID) error {
	root = filepath.Clean(root)
	var visit func(string) error
	visit = func(path string) error {
		if _, _, err := nativeFileIdentity(path); err != nil {
			return fmt.Errorf("verify command ACL cleanup path %s: %w", path, err)
		}
		descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("read back command ACL cleanup for %s: %w", path, err)
		}
		if descriptor == nil || containsSID(descriptor.String(), sid.String()) {
			return fmt.Errorf("command logon SID remains on filesystem object %s", path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect command ACL cleanup object %s: %w", path, err)
		}
		if !info.IsDir() {
			return nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("list command ACL cleanup directory %s: %w", path, err)
		}
		for _, entry := range entries {
			if err := visit(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(root)
}

func verifyNativeSIDAbsentRoots(records []nativeACLRecord, sid *windows.SID) error {
	roots := make([]string, 0, len(records))
	for _, record := range records {
		if !record.Write {
			continue
		}
		covered := false
		for _, root := range roots {
			if pathWithin(root, record.Path) {
				covered = true
				break
			}
		}
		if !covered {
			roots = append(roots, filepath.Clean(record.Path))
		}
	}
	for _, root := range roots {
		if err := verifyNativeSIDAbsentTree(root, sid); err != nil {
			return err
		}
	}
	return nil
}

func nativeIdentityRecord(path string, write, deny bool) (nativeACLRecord, error) {
	path = filepath.Clean(path)
	volume, index, err := nativeFileIdentity(path)
	if err != nil {
		return nativeACLRecord{}, fmt.Errorf("read journal identity for %s: %w", path, err)
	}
	return nativeACLRecord{Path: path, VolumeSerial: volume, FileIndex: index, Write: write, Deny: deny}, nil
}
