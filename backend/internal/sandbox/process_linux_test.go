//go:build linux

package sandbox

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBubblewrapArgsMapsWorkspaceBeforeHostMasksAndPreservesNestedReadOnlyGrant(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	cache := filepath.Join(project, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/usr/bin/true")
	command.Args = []string{"/usr/bin/true"}
	command.Dir = project
	args, err := bubblewrapArgs(command, Policy{
		Backend:       BackendBubblewrap,
		ReadOnlyPaths: []string{project, cache},
		WritePaths:    []string{project},
		Timeout:       time.Second,
	}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !hasFlagPair(args, "--bind", project, "/workspace") {
		t.Fatalf("project should be writable: %s", joined)
	}
	if hasFlagPair(args, "--ro-bind", project, "/workspace") {
		t.Fatalf("writable project root must not be rebound read-only: %s", joined)
	}
	if !hasFlagPair(args, "--ro-bind", cache, "/workspace/cache") {
		t.Fatalf("nested cache should be read-only: %s", joined)
	}
	if indexOfMount(args, "--bind", project) > indexOfArgPair(args, "--tmpfs", "/tmp") {
		t.Fatalf("workspace source must be bound before masking host /tmp: %s", joined)
	}
	if indexOfArgPair(args, "--tmpfs", "/home") < indexOfMount(args, "--bind", project) {
		t.Fatalf("workspace source must be bound before masking host /home: %s", joined)
	}
	if !containsPair(args, "--chdir", "/workspace") {
		t.Fatalf("working directory should use its sandbox path: %s", joined)
	}
	if !hasFlagPair(args, "--ro-bind-try", "/dev/null", "/etc/shadow") {
		t.Fatalf("host credential files must be hidden from sandbox commands: %s", joined)
	}
	if !containsPair(args, "--tmpfs", "/etc/o-agent") {
		t.Fatalf("host O service configuration and credentials must be hidden from sandbox commands: %s", joined)
	}
}

func TestBubblewrapArgsIsolatesNetworkUnlessPolicyAllowsIt(t *testing.T) {
	root := t.TempDir()
	command := exec.Command("/usr/bin/true")
	command.Dir = root
	args, err := bubblewrapArgs(command, Policy{Backend: BackendBubblewrap, Timeout: time.Second}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgPair(args, "--unshare-all") || containsArgPair(args, "--share-net") {
		t.Fatalf("network must be isolated by default: %q", args)
	}
	_hostsFile := filepath.Join(root, "egress-hosts")
	if err := os.WriteFile(_hostsFile, []byte("93.184.216.34 example.test\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	args, err = bubblewrapArgs(command, Policy{Backend: BackendBubblewrap, NetworkAccess: true, NetworkHostsFile: _hostsFile, NetworkAllowIPs: []netip.Addr{netip.MustParseAddr("93.184.216.34")}, Timeout: time.Second}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgPair(args, "--share-net") {
		t.Fatalf("explicit network permission should share host network: %q", args)
	}
	if !hasFlagPair(args, "--ro-bind", _hostsFile, "/etc/hosts") {
		t.Fatalf("networked sandbox must use its pinned hosts map: %q", args)
	}
}

func TestBubblewrapPrivateTmpfsHasNoProductMemoryCap(t *testing.T) {
	root := t.TempDir()
	command := exec.Command("/usr/bin/true")
	command.Dir = root
	args, err := bubblewrapArgs(command, Policy{Backend: BackendBubblewrap, Timeout: time.Second}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for index, arg := range args {
		if arg != "--tmpfs" {
			continue
		}
		count++
		if index >= 2 && args[index-2] == "--size" {
			t.Fatalf("tmpfs mount %q retains a product-imposed memory cap: %q", args[index+1], args)
		}
	}
	if count == 0 {
		t.Fatal("expected private tmpfs mounts")
	}
}

func TestBubblewrapRejectsHostRootAsAnAuthorizedPath(t *testing.T) {
	command := exec.Command("/usr/bin/true")
	command.Dir = "/usr"
	_, err := bubblewrapArgs(command, Policy{Backend: BackendBubblewrap, ReadOnlyPaths: []string{"/"}, Timeout: time.Second}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "host root") {
		t.Fatalf("expected host root grant to fail closed, got %v", err)
	}
}

func hasFlagPair(args []string, flag, first, second string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag && args[i+1] == first && args[i+2] == second {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func indexOfMount(args []string, flag, source string) int {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag && args[i+1] == source {
			return i
		}
	}
	return len(args)
}

func indexOfArgPair(args []string, flag, value string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return i
		}
	}
	return len(args)
}

// Fixtures test launcher/cancellation only; they do not prove namespace isolation.
func installBubblewrapFixture(t *testing.T, script string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "bwrap"), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory) // No systemd-run or systemctl.
}

func TestLinuxOfflineLauncherDoesNotRequireSystemd(t *testing.T) {
	installBubblewrapFixture(t, "exit 17\n")
	command := exec.Command("/usr/bin/true")
	command.Dir = "/usr"
	runErr, cleanupErr := Run(context.Background(), command, nil, Policy{Timeout: time.Second})
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 17 || cleanupErr != nil {
		t.Fatalf("run=%v cleanup=%v", runErr, cleanupErr)
	}
	if command.ProcessState == nil || command.ProcessState.ExitCode() != 17 {
		t.Fatal("child terminal state was not propagated")
	}
}

func TestLinuxMissingOrBrokenBubblewrapNeverRunsUnisolatedCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	command := exec.Command("/bin/sh", "-c", "touch \"$1\"", "fixture", marker)
	command.Dir = "/usr"
	t.Setenv("PATH", t.TempDir())
	runErr, cleanupErr := Run(context.Background(), command, nil, Policy{Timeout: time.Second})
	if runErr == nil || cleanupErr != nil {
		t.Fatalf("missing bwrap: run=%v cleanup=%v", runErr, cleanupErr)
	}
	installBubblewrapFixture(t, "exit 23\n")
	runErr, cleanupErr = Run(context.Background(), command, nil, Policy{Timeout: time.Second})
	if runErr == nil || cleanupErr != nil {
		t.Fatalf("broken bwrap: run=%v cleanup=%v", runErr, cleanupErr)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unisolated command ran: %v", err)
	}
}

func TestLinuxCancellationAndTimeoutTerminateLauncherProcessGroup(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "descendant.pid")
			installBubblewrapFixture(t, "/bin/sleep 60 &\nprintf '%s' \"$!\" > '"+pidFile+"'\nwait\n")
			command := exec.Command("/usr/bin/true")
			command.Dir = "/usr"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 3 * time.Second
			if mode == "timeout" {
				timeout = 500 * time.Millisecond
			}
			type result struct{ runErr, cleanupErr error }
			done := make(chan result, 1)
			go func() {
				runErr, cleanupErr := Run(ctx, command, nil, Policy{Timeout: timeout})
				done <- result{runErr, cleanupErr}
			}()
			deadline := time.Now().Add(2 * time.Second)
			var pid int
			for time.Now().Before(deadline) {
				if content, err := os.ReadFile(pidFile); err == nil {
					pid, err = strconv.Atoi(string(content))
					if err == nil && pid > 0 {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
			if pid == 0 {
				cancel()
				<-done
				t.Fatal("descendant did not start")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case r := <-done:
				want := context.Canceled
				if mode == "timeout" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(r.runErr, want) || r.cleanupErr != nil {
					t.Fatalf("run=%v cleanup=%v, want %v", r.runErr, r.cleanupErr, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not terminate launcher")
			}
			deadline = time.Now().Add(time.Second)
			for {
				content, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
				if errors.Is(err, os.ErrNotExist) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				_, tail, ok := strings.Cut(string(content), ") ")
				if ok && strings.HasPrefix(tail, "Z ") {
					break
				} // Dead, pending host PID-1 reaping.
				if time.Now().After(deadline) {
					t.Fatalf("descendant %d remains alive: %s", pid, content)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}
