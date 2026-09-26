//go:build windows

package coretools

import (
	"context"
	"os/exec"

	"axiom.local/agent/internal/sandbox"
)

func runProcessTree(ctx context.Context, command *exec.Cmd, input []byte, policy processPolicy) (error, error) {
	return sandbox.Run(ctx, command, input, sandbox.Policy{
		ReadOnlyPaths:               policy.readOnlyPaths,
		PrivateTempWorkingDirectory: policy.privateTempWorkingDirectory,
		JournalPath:                 policy.journalPath,
	})
}
