//go:build linux

package sandbox

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
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

func TestBubblewrapTmpfsUsesTheTaskMemoryBudget(t *testing.T) {
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
		if index < 2 || args[index-2] != "--size" || args[index-1] != fmt.Sprintf("%d", linuxSandboxMemoryMaxBytes) {
			t.Fatalf("tmpfs mount %q does not use the task memory budget: %q", args[index+1], args)
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
