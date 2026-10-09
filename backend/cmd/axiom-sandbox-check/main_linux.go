//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"axiom.local/agent/internal/projectquota"
	"axiom.local/agent/internal/sandbox"
)

type checkResult struct {
	Status              sandbox.NativeHealth     `json:"status"`
	WriteVerified       bool                     `json:"writeVerified"`
	WorkspaceQuotaMount projectquota.ProbeResult `json:"workspaceQuotaMount"`
	CheckedAt           time.Time                `json:"checkedAt"`
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, output io.Writer) error {
	status := sandbox.LinuxStatus()
	if status.Health != "healthy" {
		return fmt.Errorf("Linux sandbox health check failed: %s", status.Reason)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	workspaceRoot := os.Getenv("O_WORKSPACE_ROOT")
	if workspaceRoot == "" {
		workspaceRoot = "/var/lib/o-agent/workspaces"
	}
	quotaMount, err := projectquota.ProbeFilesystem(workspaceRoot)
	if err != nil {
		return fmt.Errorf("probe workspace quota mount: %w", err)
	}
	workspace, err := os.MkdirTemp("", "o-agent-sandbox-check-")
	if err != nil {
		return fmt.Errorf("create sandbox smoke-test workspace: %w", err)
	}
	writeVerified, smokeErr := runWriteSmokeTest(ctx, workspace)
	cleanupErr := cleanupSmokeWorkspace(workspace)
	if err := errors.Join(smokeErr, cleanupErr); err != nil {
		return err
	}
	result := checkResult{Status: status, WriteVerified: writeVerified, WorkspaceQuotaMount: quotaMount, CheckedAt: time.Now().UTC()}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return fmt.Errorf("write sandbox check result: %w", err)
	}
	return nil
}

func runWriteSmokeTest(ctx context.Context, workspace string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	command := exec.Command("/bin/sh", "-c", "printf 'o-agent-sandbox-ok\\n' > sandbox-check.txt")
	command.Dir = workspace
	runErr, cleanupErr := sandbox.Run(ctx, command, nil, sandbox.Policy{
		Backend:    sandbox.BackendBubblewrap,
		WritePaths: []string{workspace},
		Timeout:    15 * time.Second,
	})
	if runErr != nil || cleanupErr != nil {
		if runErr != nil {
			runErr = fmt.Errorf("execute Bubblewrap sandbox smoke test: %w", runErr)
		}
		return false, errors.Join(runErr, cleanupErr)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "sandbox-check.txt"))
	if err != nil {
		return false, fmt.Errorf("read back sandbox smoke-test output: %w", err)
	}
	if string(content) != "o-agent-sandbox-ok\n" {
		return false, errors.New("sandbox smoke-test output did not match the expected durable file contents")
	}
	return true, nil
}

func cleanupSmokeWorkspace(workspace string) error {
	removeErr := os.RemoveAll(workspace)
	if _, err := os.Lstat(workspace); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("sandbox smoke-test workspace remains after cleanup")
		}
		removeErr = errors.Join(removeErr, err)
	}
	if removeErr != nil {
		return fmt.Errorf("clean sandbox smoke-test workspace: %w", removeErr)
	}
	return nil
}
