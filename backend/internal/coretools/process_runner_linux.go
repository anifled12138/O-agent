//go:build linux

package coretools

import (
	"context"
	"os/exec"

	"axiom.local/agent/internal/sandbox"
)

func runProcessTree(ctx context.Context, command *exec.Cmd, input []byte, policy processPolicy) (error, error) {
	return sandbox.Run(ctx, command, input, sandbox.Policy{
		Backend:                     policy.backend,
		ReadOnlyPaths:               policy.readOnlyPaths,
		WritePaths:                  policy.writePaths,
		NetworkAccess:               policy.networkAccess,
		NetworkAllowHosts:           append([]string(nil), policy.networkAllowHosts...),
		GitCredentials:              policy.gitCredentials,
		GitCredentialURLs:           append([]string(nil), policy.gitCredentialURLs...),
		Timeout:                     policy.timeout,
		PrivateTempWorkingDirectory: policy.privateTempWorkingDirectory,
	})
}

func runLongLivedProcessTree(ctx context.Context, command *exec.Cmd, input []byte, policy processPolicy) (error, error) {
	return sandbox.RunLongLived(ctx, command, input, sandbox.Policy{
		Backend:                     policy.backend,
		ReadOnlyPaths:               append([]string(nil), policy.readOnlyPaths...),
		WritePaths:                  append([]string(nil), policy.writePaths...),
		NetworkAccess:               policy.networkAccess,
		NetworkAllowHosts:           append([]string(nil), policy.networkAllowHosts...),
		GitCredentials:              policy.gitCredentials,
		GitCredentialURLs:           append([]string(nil), policy.gitCredentialURLs...),
		Timeout:                     policy.timeout,
		PrivateTempWorkingDirectory: policy.privateTempWorkingDirectory,
	})
}
