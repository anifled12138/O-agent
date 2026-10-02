//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
)

var procShellExecuteEx = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

type shellExecuteInfo struct {
	Size       uint32
	Mask       uint32
	Window     windows.Handle
	Verb       *uint16
	File       *uint16
	Parameters *uint16
	Directory  *uint16
	Show       int32
	Instance   uintptr
	IDList     uintptr
	Class      *uint16
	Key        windows.Handle
	HotKey     uint32
	Icon       uintptr
	Process    windows.Handle
}

func WindowsSandboxSetupAvailable(helperPath, runnerSource string) bool {
	for _, path := range []string{helperPath, runnerSource} {
		if path == "" {
			return false
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func WindowsSandboxSetupHelperAvailable(helperPath string) bool {
	info, err := os.Stat(helperPath)
	return helperPath != "" && err == nil && info.Mode().IsRegular()
}

// RunWindowsSandboxSetup starts the setup helper only after an explicit Host
// request. Windows performs the UAC consent flow for the `runas` verb.
func RunWindowsSandboxSetup(ctx context.Context, operation, helperPath, installDir, runnerSource string) (resultErr error) {
	if operation != "install" && operation != "repair" && operation != "uninstall" {
		return fmt.Errorf("unsupported sandbox maintenance operation %q", operation)
	}
	if !WindowsSandboxSetupHelperAvailable(helperPath) || (operation != "uninstall" && !WindowsSandboxSetupAvailable(helperPath, runnerSource)) {
		return errors.New("sandbox setup helper or trusted runner source is missing")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current Windows user SID for sandbox setup: %w", err)
	}
	installDir, err = checkedInstallDirectory(installDir)
	if err != nil {
		return err
	}
	helperPath, err = canonicalExecutable(helperPath)
	if err != nil {
		return fmt.Errorf("resolve sandbox setup helper: %w", err)
	}
	args := []string{"--operation", operation, "--install-dir", installDir, "--owner-sid", owner.User.Sid.String()}
	if operation != "uninstall" {
		runnerSource, err = canonicalExecutable(runnerSource)
		if err != nil {
			return fmt.Errorf("resolve trusted command runner: %w", err)
		}
		args = append(args, "--runner", runnerSource)
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	helper16, err := windows.UTF16PtrFromString(helperPath)
	if err != nil {
		return err
	}
	parameters16, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return err
	}
	directory16, err := windows.UTF16PtrFromString(filepath.Dir(helperPath))
	if err != nil {
		return err
	}
	info := shellExecuteInfo{
		Size: uint32(unsafe.Sizeof(shellExecuteInfo{})), Mask: seeMaskNoCloseProcess | seeMaskNoAsync,
		Verb: verb, File: helper16, Parameters: parameters16, Directory: directory16,
		Show: windows.SW_HIDE,
	}
	result, _, callErr := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info)))
	runtimeKeepAliveWFP(verb, helper16, parameters16, directory16, info)
	if result == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == windows.ERROR_CANCELLED {
			return errors.New("Windows administrator consent was cancelled")
		}
		if callErr != nil && callErr != syscall.Errno(0) {
			return fmt.Errorf("start elevated sandbox setup: %w", callErr)
		}
		return errors.New("start elevated sandbox setup returned failure without a Windows error")
	}
	if info.Process == 0 {
		return errors.New("elevated sandbox setup did not return a process handle")
	}
	defer func() {
		if err := windows.CloseHandle(info.Process); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close elevated sandbox setup process handle: %w", err))
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox setup continues in the elevated helper after the request ended: %w", ctx.Err())
		default:
		}
		wait, err := windows.WaitForSingleObject(info.Process, 250)
		if err != nil {
			return fmt.Errorf("wait for elevated sandbox setup: %w", err)
		}
		if wait == uint32(windows.WAIT_TIMEOUT) {
			continue
		}
		if wait != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("unexpected elevated setup wait result %d", wait)
		}
		var exitCode uint32
		if err := windows.GetExitCodeProcess(info.Process, &exitCode); err != nil {
			return fmt.Errorf("read elevated sandbox setup exit status: %w", err)
		}
		status := NativeStatus(installDir, "")
		if exitCode != 0 {
			return fmt.Errorf("elevated sandbox setup exited with code %d; installation=%s health=%s: %s", exitCode, status.Installation, status.Health, status.Reason)
		}
		switch operation {
		case "install", "repair":
			if status.Health != "healthy" {
				return fmt.Errorf("setup helper exited successfully but authoritative sandbox Probe is %s: %s", status.Health, status.Reason)
			}
		case "uninstall":
			if status.Installation != "absent" {
				return fmt.Errorf("setup helper exited successfully but sandbox installation remains %s", status.Installation)
			}
		}
		return nil
	}
}
