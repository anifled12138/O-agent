//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"axiom.local/agent/internal/sandbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	operation := flag.String("operation", "install", "install, repair, uninstall, or status")
	installDir := flag.String("install-dir", "", "per-user sandbox installation directory")
	runnerPath := flag.String("runner", "", "trusted axiom-command-runner.exe path")
	ownerSID := flag.String("owner-sid", "", "Windows SID that owns this O Agent installation")
	flag.Parse()
	if *installDir == "" {
		return errors.New("-install-dir is required")
	}
	switch *operation {
	case "status":
		status := sandbox.NativeStatus(*installDir, "")
		if err := json.NewEncoder(os.Stdout).Encode(status); err != nil {
			return err
		}
		if status.Health != "healthy" {
			return fmt.Errorf("native sandbox is %s: %s", status.Health, status.Reason)
		}
		return nil
	case "install", "repair":
		if *runnerPath == "" {
			return errors.New("-runner is required for installation and repair")
		}
	case "uninstall":
		if err := sandbox.RemoveWindowsNative(*installDir, *ownerSID); err != nil {
			return err
		}
		status := sandbox.NativeStatus(*installDir, "")
		if status.Installation != "absent" {
			return fmt.Errorf("native sandbox uninstall did not remove installation: %s", status.Reason)
		}
		return json.NewEncoder(os.Stdout).Encode(status)
	default:
		return fmt.Errorf("unsupported setup operation %q", *operation)
	}
	if err := sandbox.InstallWindowsNative(*installDir, *runnerPath, *ownerSID); err != nil {
		return err
	}
	status := sandbox.NativeStatus(*installDir, "")
	if status.Health != "healthy" {
		return fmt.Errorf("native sandbox setup did not pass final read-back Probe: %s", status.Reason)
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}
