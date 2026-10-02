//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procOpenMutex = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenMutexW")

const mutexModifyState = 0x0001

func nativeCommandLeaseName(ownerSID, commandID string) string {
	return "Global\\OAgentSandboxCommand_" + wfpDigest(ownerSID+"\x00"+commandID)
}

func acquireNativeCommandLease(ownerSID, commandID string) (windows.Handle, error) {
	runtime.LockOSThread()
	threadPinned := true
	unlockThread := func() {
		if threadPinned {
			threadPinned = false
			runtime.UnlockOSThread()
		}
	}
	name16, err := windows.UTF16PtrFromString(nativeCommandLeaseName(ownerSID, commandID))
	if err != nil {
		unlockThread()
		return 0, err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + ownerSID + ")")
	if err != nil {
		unlockThread()
		return 0, fmt.Errorf("build native command lease ACL: %w", err)
	}
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	r1, _, callErr := procCreateMutex.Call(uintptr(unsafe.Pointer(&security)), 0, uintptr(unsafe.Pointer(name16)))
	runtime.KeepAlive(descriptor)
	if r1 == 0 {
		unlockThread()
		return 0, fmt.Errorf("create native command lease: %w", callErr)
	}
	handle := windows.Handle(r1)
	wait, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		closeErr := windows.CloseHandle(handle)
		unlockThread()
		return 0, errors.Join(err, closeErr)
	}
	if wait != windows.WAIT_OBJECT_0 && wait != windows.WAIT_ABANDONED {
		closeErr := windows.CloseHandle(handle)
		unlockThread()
		return 0, errors.Join(fmt.Errorf("new native command lease was already held (wait=%d)", wait), closeErr)
	}
	return handle, nil
}

func releaseNativeCommandLease(handle windows.Handle) error {
	if handle == 0 {
		return nil
	}
	result, _, callErr := procReleaseMutex.Call(uintptr(handle))
	var releaseErr error
	if result == 0 {
		releaseErr = fmt.Errorf("release native command lease: %w", callErr)
	}
	closeErr := windows.CloseHandle(handle)
	runtime.UnlockOSThread()
	return errors.Join(releaseErr, closeErr)
}

// nativeCommandLeaseActive is called while the per-user setup mutex is held,
// so no command can create its journal/lease between this check and recovery.
func nativeCommandLeaseActive(ownerSID, commandID string) (active bool, resultErr error) {
	name16, err := windows.UTF16PtrFromString(nativeCommandLeaseName(ownerSID, commandID))
	if err != nil {
		return false, err
	}
	r1, _, callErr := procOpenMutex.Call(windows.SYNCHRONIZE|mutexModifyState, 0, uintptr(unsafe.Pointer(name16)))
	runtime.KeepAlive(name16)
	if r1 == 0 {
		if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(callErr, windows.ERROR_INVALID_NAME) {
			return false, nil
		}
		return false, fmt.Errorf("open native command lease: %w", callErr)
	}
	handle := windows.Handle(r1)
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	wait, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, err
	}
	switch wait {
	case uint32(windows.WAIT_TIMEOUT):
		return true, nil
	case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
		result, _, releaseErr := procReleaseMutex.Call(uintptr(handle))
		if result == 0 {
			return false, fmt.Errorf("release recovered native command lease: %w", releaseErr)
		}
		return false, nil
	default:
		return false, fmt.Errorf("unexpected native command lease wait result %d", wait)
	}
}
