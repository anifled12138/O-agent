//go:build linux

package sandbox

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixedGitCredentialBroker struct {
	url      string
	username string
	password string
}

func (b fixedGitCredentialBroker) Lookup(repositoryURL string) (string, string, bool) {
	if repositoryURL != b.url {
		return "", "", false
	}
	return b.username, b.password, true
}

func TestLinuxGitCredentialHelperUsesExactScopedSocketBroker(t *testing.T) {
	const repository = "https://github.com/acme/o-agent.git"
	bridge, err := startLinuxGitCredentialBridge(fixedGitCredentialBroker{url: repository, username: "git-user", password: "private-token"}, []string{repository})
	if err != nil {
		t.Fatal(err)
	}
	dir := bridge.dir
	output := new(strings.Builder)
	input := strings.NewReader("protocol=https\nhost=github.com\npath=acme/o-agent.git\n\n")
	if err := RunGitCredentialHelper([]string{"--socket", bridge.socket, "get"}, input, output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "username=git-user\npassword=private-token\n\n" {
		t.Fatalf("credential helper output mismatch: %q", output.String())
	}
	otherRepoOutput := new(strings.Builder)
	otherRepoInput := strings.NewReader("protocol=https\nhost=github.com\npath=acme/other.git\n\n")
	if err := RunGitCredentialHelper([]string{"--socket", bridge.socket, "get"}, otherRepoInput, otherRepoOutput); err != nil {
		t.Fatal(err)
	}
	if otherRepoOutput.Len() != 0 {
		t.Fatalf("credential bridge disclosed credentials outside the exact repository scope: %q", otherRepoOutput.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != linuxCredentialSocketName {
		t.Fatalf("credential bridge wrote secret files to its private directory: entries=%v err=%v", entries, err)
	}
	if err := bridge.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("credential bridge directory remains after cleanup: %v", err)
	}
}

func TestLinuxCredentialBridgeRejectsUnsafeRepositoryScopes(t *testing.T) {
	for _, value := range []string{
		"http://github.com/acme/repo.git",
		"https://user:secret@github.com/acme/repo.git",
		"https://github.com:443/acme/repo.git",
		"https://github.com/acme/repo.git?token=secret",
		"https://github.com/acme/%2e%2e/other.git",
	} {
		if _, err := canonicalHTTPSRepositoryURL(value); err == nil {
			t.Errorf("unsafe repository URL was accepted: %q", value)
		}
	}
}

func TestBubblewrapCredentialBridgeIsMountedAfterHostMasksAndScopedToGit(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "o-agent")
	if err := os.WriteFile(helper, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(helper, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/usr/bin/true")
	command.Dir = dir
	bridge := &linuxGitCredentialBridge{dir: dir}
	hostsFile, allowedIPs, cleanup, err := prepareLinuxNetworkAllowlistWithResolver(context.Background(), []string{"github.com"}, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("140.82.112.4")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	args, err := bubblewrapArgs(command, Policy{
		Backend: BackendBubblewrap, NetworkAccess: true, NetworkHostsFile: hostsFile, NetworkAllowIPs: allowedIPs, Timeout: 5 * time.Second,
		GitCredentials: fixedGitCredentialBroker{}, GitCredentialURLs: []string{"https://github.com/acme/repo.git"},
	}, bridge, helper)
	if err != nil {
		t.Fatal(err)
	}
	maskIndex := indexOfArgPair(args, "--tmpfs", "/run")
	bridgeIndex := indexOfMount(args, "--bind", dir)
	if maskIndex < 0 || bridgeIndex <= maskIndex {
		t.Fatalf("credential socket must be mounted over a fresh /run after host masking: %q", args)
	}
	joined := strings.Join(args, " ")
	for _, expected := range []string{"GIT_TERMINAL_PROMPT", "credential.helper", "credential.useHttpPath", "credential.sock", "--git-credential-helper"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("sandbox Git credential configuration is missing %q: %s", expected, joined)
		}
	}
}
