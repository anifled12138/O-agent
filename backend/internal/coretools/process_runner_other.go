//go:build !windows && !linux

package coretools

import (
	"context"
	"os/exec"

	"axiom.local/agent/internal/sandbox"
)

// Non-Windows command execution remains disabled until a native sandbox
// backend is available for the current platform.
func runProcessTree(ctx context.Context, command *exec.Cmd, input []byte, policy processPolicy) (error, error) {
	return sandbox.Run(ctx, command, input, sandbox.Policy{
		Backend:                     policy.backend,
		InstallDir:                  policy.installDir,
		RunnerPath:                  policy.runnerPath,
		ReadOnlyPaths:               policy.readOnlyPaths,
		WritePaths:                  policy.writePaths,
		NetworkAccess:               policy.networkAccess,
		NetworkAllowHosts:           append([]string(nil), policy.networkAllowHosts...),
		Timeout:                     policy.timeout,
		PowerShellExitWrapper:       policy.powershellExitWrapper,
		PrivateTempWorkingDirectory: policy.privateTempWorkingDirectory,
		JournalPath:                 policy.journalPath,
	})
}
