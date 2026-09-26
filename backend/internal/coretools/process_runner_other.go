//go:build !windows

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
		ReadOnlyPaths:               policy.readOnlyPaths,
		PrivateTempWorkingDirectory: policy.privateTempWorkingDirectory,
		JournalPath:                 policy.journalPath,
	})
}
