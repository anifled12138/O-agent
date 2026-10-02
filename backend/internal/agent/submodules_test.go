package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/coretools"
)

func TestParseGitSubmoduleConfig(t *testing.T) {
	got, err := parseGitSubmoduleConfig("submodule.Lib.Path\nlibs/core\x00submodule.Lib.Url\n../core.git\x00")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Lib" || got[0].Path != "libs/core" || got[0].URL != "../core.git" {
		t.Fatalf("unexpected modules: %#v", got)
	}
}

func TestParseGitSubmoduleConfigMatchesGitOutput(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is unavailable")
	}
	manifest := filepath.Join(t.TempDir(), ".gitmodules")
	contents := "[submodule \"lib.core\"]\n\tpath = libs/core\n\turl = ../core.git\n"
	if err := os.WriteFile(manifest, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(context.Background(), gitPath, "config", "--null", "--file", manifest, "--get-regexp", `^submodule\..*\.(path|url)$`)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git config output: %v", err)
	}
	modules, err := parseGitSubmoduleConfig(string(output))
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || modules[0].Name != "lib.core" || modules[0].Path != "libs/core" || modules[0].URL != "../core.git" {
		t.Fatalf("parsed Git output = %#v", modules)
	}
	repo := filepath.Dir(manifest)
	runGit := func(args ...string) []byte {
		t.Helper()
		command := exec.CommandContext(context.Background(), gitPath, append([]string{"-C", repo}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return output
	}
	runGit("init")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.invalid")
	runGit("add", ".gitmodules")
	runGit("commit", "-m", "add submodule manifest")
	output, err = exec.CommandContext(context.Background(), gitPath, "-C", repo, "config", "--null", "--blob", "HEAD:.gitmodules", "--get-regexp", `^submodule\..*\.(path|url)$`).Output()
	if err != nil {
		t.Fatalf("git config --blob output: %v", err)
	}
	modules, err = parseGitSubmoduleConfig(string(output))
	if err != nil || len(modules) != 1 || modules[0].Name != "lib.core" || modules[0].URL != "../core.git" {
		t.Fatalf("parsed committed Git config = %#v err=%v", modules, err)
	}
}

func TestParseGitSubmoduleConfigRejectsEscapingOrIncompleteEntries(t *testing.T) {
	for _, input := range []string{
		"submodule.x.path\n../outside\x00submodule.x.url\nhttps://github.com/acme/lib.git\x00",
		"submodule.x.path\nlib\x00",
		"submodule.x.path\nlib\x00submodule.x.url\nhttps://github.com/acme/a.git\x00submodule.y.path\nlib\x00submodule.y.url\nhttps://github.com/acme/b.git\x00",
		"submodule.x.path\nlib\x00unexpected-record\x00",
	} {
		if _, err := parseGitSubmoduleConfig(input); err == nil {
			t.Fatalf("parseGitSubmoduleConfig(%q) unexpectedly succeeded", input)
		}
	}
}

func TestResolveGitSubmoduleURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		url  string
		want string
		bad  bool
	}{
		{name: "sibling relative", base: "https://github.com/acme/app.git", url: "../shared.git", want: "https://github.com/acme/shared.git"},
		{name: "same organization", base: "https://gitee.com/acme/app.git", url: "https://gitee.com/acme/lib", want: "https://gitee.com/acme/lib.git"},
		{name: "foreign host", base: "https://github.com/acme/app.git", url: "https://example.com/acme/lib.git", bad: true},
		{name: "ssh transport", base: "https://github.com/acme/app.git", url: "git@github.com:acme/lib.git", bad: true},
		{name: "path traversal", base: "https://github.com/acme/app.git", url: "../../evil/repo.git", bad: true},
		{name: "query", base: "https://github.com/acme/app.git", url: "lib.git?token=secret", bad: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveGitSubmoduleURL(test.base, test.url)
			if test.bad {
				if err == nil {
					t.Fatalf("resolveGitSubmoduleURL() = %q, want rejection", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("resolveGitSubmoduleURL() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestGitOutputHasSubmoduleLink(t *testing.T) {
	if !gitOutputHasSubmoduleLink("160000 commit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tlibs/core\x00", "libs/core") {
		t.Fatal("expected gitlink was not recognized")
	}
	for _, output := range []string{
		"100644 blob aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tlibs/core\x00",
		"160000 commit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tlibs/core-evil\x00",
	} {
		if gitOutputHasSubmoduleLink(output, "libs/core") {
			t.Fatalf("unexpectedly recognized non-matching record %q", output)
		}
	}
}

func TestParseGitBlobEntry(t *testing.T) {
	commit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	got, exists, err := parseGitBlobEntry("100644 blob "+commit+"\t.gitmodules\x00", ".gitmodules")
	if err != nil || !exists || got != commit {
		t.Fatalf("parseGitBlobEntry() = %q %t %v", got, exists, err)
	}
	if _, exists, err := parseGitBlobEntry("", ".gitmodules"); err != nil || exists {
		t.Fatalf("missing Git blob = exists:%t err:%v", exists, err)
	}
	for _, output := range []string{
		"120000 blob " + commit + "\t.gitmodules\x00",
		"160000 commit " + commit + "\t.gitmodules\x00",
		"100644 blob bad\t.gitmodules\x00",
		"100644 blob " + commit + "\tother\x00",
	} {
		if _, _, err := parseGitBlobEntry(output, ".gitmodules"); err == nil {
			t.Fatalf("parseGitBlobEntry(%q) unexpectedly succeeded", output)
		}
	}
}

func TestExecutionConfigSnapshotScopesEveryValidatedRepository(t *testing.T) {
	service := &Service{}
	got := service.executionConfigSnapshot(
		"https://github.com/acme/main",
		"https://github.com/acme/submodule.git",
		"https://example.com/attacker/repo.git",
	)
	if len(got.GitCredentialURLs) != 2 || got.GitCredentialURLs[0] != "https://github.com/acme/main.git" || got.GitCredentialURLs[1] != "https://github.com/acme/submodule.git" {
		t.Fatalf("credential scope = %#v, want both normalized GitHub repositories only", got.GitCredentialURLs)
	}
}

func TestInitializeGitSubmodulesRejectsUntrustedTransportBeforeNetwork(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is unavailable")
	}
	root := t.TempDir()
	runGitCommand(t, gitPath, root, "init")
	runGitCommand(t, gitPath, root, "config", "user.name", "Test User")
	runGitCommand(t, gitPath, root, "config", "user.email", "test@example.invalid")
	runGitCommand(t, gitPath, root, "remote", "add", "origin", "https://github.com/acme/main.git")
	manifest := "[submodule \"unsafe\"]\n\tpath = deps/unsafe\n\turl = file:///tmp/untrusted.git\n"
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, gitPath, root, "add", ".gitmodules")
	runGitCommand(t, gitPath, root, "update-index", "--add", "--cacheinfo", "160000", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "deps/unsafe")
	runGitCommand(t, gitPath, root, "commit", "-m", "add unsafe submodule")

	networkCalls := 0
	service := &Service{sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, workdir string, _, _ []string, network bool, _ time.Duration) (string, string, error, error) {
		if network {
			networkCalls++
		}
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = workdir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err, nil
	}}
	if err := service.initializeGitSubmodules(context.Background(), root, "https://github.com/acme/main.git"); err == nil {
		t.Fatal("untrusted file transport in committed .gitmodules was accepted")
	}
	if networkCalls != 0 {
		t.Fatalf("submodule network commands ran before URL validation: %d", networkCalls)
	}
	if _, err := os.Lstat(filepath.Join(root, "deps", "unsafe")); !os.IsNotExist(err) {
		t.Fatalf("rejected submodule changed its destination: err=%v", err)
	}
}

func runGitCommand(t *testing.T, gitPath, workdir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(gitPath, args...)
	cmd.Dir = workdir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return output
}
