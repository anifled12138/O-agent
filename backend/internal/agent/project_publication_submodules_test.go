package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
)

func TestPlanProjectSubmodulePublicationsReadsCommittedGitlinks(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is unavailable")
	}
	root, err := os.MkdirTemp(".", ".tmp-project-publication-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	child := filepath.Join(root, "libs", "core")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Test User"}, {"config", "user.email", "test@example.invalid"}, {"remote", "add", "origin", "https://github.com/acme/core.git"}} {
		runGitCommand(t, gitPath, child, args...)
	}
	if err := os.WriteFile(filepath.Join(child, "core.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, gitPath, child, "add", "core.txt")
	runGitCommand(t, gitPath, child, "commit", "-m", "child")
	childSHA := string(runGitCommand(t, gitPath, child, "rev-parse", "HEAD"))
	childSHA = strings.TrimSpace(childSHA)
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Test User"}, {"config", "user.email", "test@example.invalid"}, {"remote", "add", "origin", "https://github.com/acme/main.git"}} {
		runGitCommand(t, gitPath, root, args...)
	}
	manifest := "[submodule \"core\"]\n\tpath = libs/core\n\turl = https://github.com/acme/core.git\n"
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, gitPath, root, "add", ".gitmodules")
	runGitCommand(t, gitPath, root, "update-index", "--add", "--cacheinfo", "160000,"+childSHA+",libs/core")
	runGitCommand(t, gitPath, root, "commit", "-m", "parent")
	parentSHA := strings.TrimSpace(string(runGitCommand(t, gitPath, root, "rev-parse", "HEAD")))
	service := &Service{sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, workdir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		cmd := exec.CommandContext(ctx, command, args...)
		if len(args) >= 1 && args[0] == "ls-remote" {
			return "", "", nil, nil
		}
		cmd.Dir = workdir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err, nil
	}}
	plan, err := service.PlanProjectSubmodulePublications(context.Background(), "https://github.com/acme/main.git", root, parentSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Path != "libs/core" || plan[0].RepositoryURL != "https://github.com/acme/core.git" || plan[0].CommitSHA != childSHA || plan[0].Ref != "refs/heads/o-agent-objects/"+childSHA || plan[0].Status != "pending" {
		t.Fatalf("unexpected immutable submodule ref plan: %+v", plan)
	}
	preview, err := service.InspectGitPublication(context.Background(), "https://github.com/acme/main.git", root, "main")
	if err != nil || len(preview.Submodules) != 1 || preview.Submodules[0].CommitSHA != childSHA {
		t.Fatalf("publication preview did not disclose its child ref plan: %+v err=%v", preview, err)
	}
}

func TestPublishProjectSubmoduleRefUsesCreateOnlyRefAndReadsBack(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is unavailable")
	}
	root, err := os.MkdirTemp(".", ".tmp-submodule-push-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	child := filepath.Join(root, "libs", "core")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Test User"}, {"config", "user.email", "test@example.invalid"}, {"remote", "add", "origin", "https://github.com/acme/core.git"}} {
		runGitCommand(t, gitPath, child, args...)
	}
	if err := os.WriteFile(filepath.Join(child, "core.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, gitPath, child, "add", "core.txt")
	runGitCommand(t, gitPath, child, "commit", "-m", "child")
	commit := strings.TrimSpace(string(runGitCommand(t, gitPath, child, "rev-parse", "HEAD")))
	remote, err := filepath.Abs(filepath.Join(root, "remote.git"))
	if err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, gitPath, root, "init", "--bare", remote)
	var networkCalls int
	var networkCommands []string
	service := &Service{sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, workdir string, _, writePaths []string, network bool, _ time.Duration) (string, string, error, error) {
		if network {
			networkCalls++
			foundMetadata := false
			expectedMetadata, _ := filepath.Abs(filepath.Join(child, ".git"))
			for _, p := range writePaths {
				if filepath.Clean(p) == filepath.Clean(expectedMetadata) {
					foundMetadata = true
				}
			}
			if !foundMetadata {
				return "", "network Git command lacked scoped child metadata write access", os.ErrPermission, nil
			}
			for i := range args {
				if args[i] == "origin" {
					args[i] = remote
				}
			}
			networkCommands = append(networkCommands, strings.Join(args, " "))
		}
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = workdir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		return stdout.String(), stderr.String(), runErr, nil
	}}
	target := domain.ProjectPublicationSubmodule{Path: "libs/core", RepositoryURL: "https://github.com/acme/core.git", CommitSHA: commit, Ref: "refs/heads/o-agent-objects/" + commit}
	gotSHA, attempted, err := service.PublishProjectSubmoduleRef(context.Background(), root, target)
	if err != nil || !attempted || gotSHA != commit {
		t.Fatalf("publish child ref = %q attempted=%t err=%v", gotSHA, attempted, err)
	}
	remoteSHA := strings.TrimSpace(string(runGitCommand(t, gitPath, root, "--git-dir", remote, "rev-parse", target.Ref)))
	if remoteSHA != commit {
		t.Fatalf("remote did not contain exact child commit: %q", remoteSHA)
	}
	gotSHA, attempted, err = service.PublishProjectSubmoduleRef(context.Background(), root, target)
	if err != nil || attempted || gotSHA != commit {
		t.Fatalf("idempotent read-back = %q attempted=%t err=%v", gotSHA, attempted, err)
	}
	if networkCalls < 3 {
		t.Fatalf("expected remote read, create, and read-back network calls; got %d", networkCalls)
	}
	if err := os.WriteFile(filepath.Join(child, "other.txt"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, gitPath, child, "add", "other.txt")
	runGitCommand(t, gitPath, child, "commit", "-m", "other")
	otherCommit := strings.TrimSpace(string(runGitCommand(t, gitPath, child, "rev-parse", "HEAD")))
	runGitCommand(t, gitPath, child, "push", remote, otherCommit+":refs/heads/staging")
	runGitCommand(t, gitPath, root, "--git-dir", remote, "update-ref", target.Ref, otherCommit)
	runGitCommand(t, gitPath, child, "checkout", "--detach", commit)
	gotSHA, attempted, err = service.PublishProjectSubmoduleRef(context.Background(), root, target)
	if err == nil || attempted || gotSHA != otherCommit {
		t.Fatalf("conflicting commit-addressed ref was changed: remote=%q attempted=%t err=%v", gotSHA, attempted, err)
	}
	remoteSHA = strings.TrimSpace(string(runGitCommand(t, gitPath, root, "--git-dir", remote, "rev-parse", target.Ref)))
	if remoteSHA != otherCommit {
		t.Fatalf("conflicting remote ref changed after rejected publication: %q", remoteSHA)
	}
	for _, command := range networkCommands {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "push" {
			wantLease := "--force-with-lease=" + target.Ref + ":"
			wantRefspec := target.CommitSHA + ":" + target.Ref
			if len(fields) != 4 || fields[1] != wantLease || fields[2] != remote || fields[3] != wantRefspec {
				t.Fatalf("submodule publication must update only its create-only immutable ref: %q", command)
			}
		}
		if fields[0] == "fetch" && (strings.Contains(command, "--prune") || strings.Contains(command, "-p")) {
			t.Fatalf("child ref publication must not prune historical refs: %q", command)
		}
		if fields[0] == "update-ref" && len(fields) > 1 && fields[1] == "-d" {
			t.Fatalf("child ref publication must not delete historical refs: %q", command)
		}
		if fields[0] == "delete-ref" || fields[0] == "remote" && strings.Contains(command, "--prune") {
			t.Fatalf("child ref publication invoked ref cleanup: %q", command)
		}
	}
}
