//go:build !windows && !linux

package sandbox

import (
	"context"
	"os/exec"
	"path/filepath"
	"time"
)

// Run is intentionally unavailable until this platform has a native backend.
// Callers must not silently fall back to running an unconfined child process.
func Run(context.Context, *exec.Cmd, []byte, Policy) (error, error) {
	return ErrUnavailable, nil
}

func RecoverRunfiles(string) (RecoveryReport, error) { return RecoveryReport{}, nil }

func RecoverNativeJournals(string) (RecoveryReport, error) { return RecoveryReport{}, nil }

func NativeStatus(installDir, runnerPath string) NativeHealth {
	if runnerPath == "" {
		runnerPath = filepath.Join(installDir, "axiom-command-runner.exe")
	}
	return NativeHealth{Installation: "absent", Health: "unhealthy", Backend: string(BackendWindowsNative), Reason: "Windows native sandbox is unavailable on this platform", Runner: runnerPath, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

func LinuxStatus() NativeHealth {
	return NativeHealth{Installation: "absent", Health: "unhealthy", Backend: string(BackendBubblewrap), Reason: "Linux bubblewrap sandbox is unavailable on this platform", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

func WindowsSandboxSetupAvailable(helperPath, runnerSource string) bool { return false }

func WindowsSandboxSetupHelperAvailable(helperPath string) bool { return false }

func RunWindowsSandboxSetup(context.Context, string, string, string, string) error {
	return ErrUnavailable
}
