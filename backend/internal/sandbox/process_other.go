//go:build !windows

package sandbox

import (
	"context"
	"os/exec"
)

// Run is intentionally unavailable until this platform has a native backend.
// Callers must not silently fall back to running an unconfined child process.
func Run(context.Context, *exec.Cmd, []byte, Policy) (error, error) {
	return ErrUnavailable, nil
}

func RecoverRunfiles(string) error { return nil }
