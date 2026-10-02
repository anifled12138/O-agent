package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
)

func TestChangedProjectLFSFilesUsesChangedPathsAndPreservesRepeatedOIDs(t *testing.T) {
	const oid = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	listing := oid + " * folder/new binary.bin\n" + oid + " - folder/unchanged.bin\n"
	run := func(args []string) (string, error) {
		if len(args) != 5 || args[0] != "diff" || args[1] != "--name-only" || args[2] != "-z" {
			t.Fatalf("unexpected Git diff arguments: %#v", args)
		}
		return "folder/new binary.bin\x00", nil
	}
	changed, err := changedProjectLFSFiles(run, strings.Repeat("1", 40), strings.Repeat("2", 40), listing)
	if err != nil || len(changed) != 1 || changed[0].OID != oid || changed[0].Path != "folder/new binary.bin" || !changed[0].Hydrated {
		t.Fatalf("changed LFS files = %+v, err=%v", changed, err)
	}
}

func TestGitLFSAttributeFallbackDoesNotRequireLFSForOrdinaryRepositories(t *testing.T) {
	commit := strings.Repeat("a", 40)
	ordinary, err := gitLFSConfiguredAtCommit(func(args []string) (string, error) {
		if args[0] == "ls-tree" {
			return ".gitattributes\n", nil
		}
		if args[0] == "show" {
			return "*.txt text=auto\n", nil
		}
		return "", errors.New("unexpected Git command")
	}, commit)
	if err != nil || ordinary {
		t.Fatalf("ordinary repository LFS detection = %v err=%v", ordinary, err)
	}
	tracked, err := gitLFSConfiguredAtCommit(func(args []string) (string, error) {
		if args[0] == "ls-tree" {
			return "sub/.gitattributes\n", nil
		}
		if args[0] == "show" {
			return "*.bin filter=lfs diff=lfs merge=lfs -text\n", nil
		}
		return "", errors.New("unexpected Git command")
	}, commit)
	if err != nil || !tracked {
		t.Fatalf("Git LFS repository detection = %v err=%v", tracked, err)
	}
}

func TestOrdinaryGitDeltaWorksWhenGitLFSIsNotInstalled(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}
	ctx := context.Background()
	workdir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, workdir, "init")
	runGitTest(t, ctx, git, workdir, "config", "user.name", "Test User")
	runGitTest(t, ctx, git, workdir, "config", "user.email", "test@example.invalid")
	runGitTest(t, ctx, git, workdir, "remote", "add", "origin", "https://github.com/example/no-lfs.git")
	if err := os.WriteFile(filepath.Join(workdir, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, workdir, "add", "--all")
	runGitTest(t, ctx, git, workdir, "commit", "-m", "base")
	base := runGitTest(t, ctx, git, workdir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workdir, "README.md"), []byte("updated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Service{sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, dir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		for _, arg := range args {
			if arg == "lfs" {
				return "", "git-lfs is not installed", errors.New("git-lfs is not installed"), nil
			}
		}
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err, nil
	}}
	delta, err := service.CreateProjectDeltaBundle(ctx, domain.Project{ID: "project-without-lfs", Workdir: workdir, RemoteRepoURL: "https://github.com/example/no-lfs.git", ResolvedCommit: base}, "task_without_lfs")
	if err != nil {
		t.Fatalf("ordinary Git project was blocked by the missing optional Git LFS executable: %v", err)
	}
	if delta.Path == "" || delta.Commit == base || delta.LFSObjectsPath != "" {
		t.Fatalf("ordinary Git delta metadata = %+v", delta)
	}
}

func TestPublishProjectCommitUploadsChangedLFSObjectsBeforeGitRef(t *testing.T) {
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const remote = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const oid = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	remoteReads := 0
	service := &Service{}
	service.sandboxCommand = func(_ context.Context, _ coretools.ExecutionConfig, command string, args []string, dir string, _, _ []string, network bool, timeout time.Duration) (string, string, error, error) {
		if command != "git" || dir != workdir {
			t.Fatalf("unexpected sandbox invocation: command=%q dir=%q args=%v", command, dir, args)
		}
		joined := strings.Join(args, " ")
		commands = append(commands, joined)
		switch {
		case joined == "check-ref-format --branch main":
			return "", "", nil, nil
		case joined == "rev-parse --show-toplevel":
			return workdir, "", nil, nil
		case joined == "remote get-url origin":
			return "https://github.com/example/project.git", "", nil, nil
		case joined == "status --porcelain --untracked-files=all":
			return "", "", nil, nil
		case joined == "rev-parse --verify HEAD^{commit}":
			return commit, "", nil, nil
		case joined == "cat-file -e "+commit+"^{commit}":
			return "", "", nil, nil
		case joined == "ls-remote --heads origin refs/heads/main":
			remoteReads++
			if remoteReads == 1 {
				return remote + "\trefs/heads/main\n", "", nil, nil
			}
			return commit + "\trefs/heads/main\n", "", nil, nil
		case joined == "fetch --no-tags origin refs/heads/main":
			if !network || timeout != 60*time.Second {
				t.Fatalf("fetch sandbox options mismatch: network=%v timeout=%v", network, timeout)
			}
			return "", "", nil, nil
		case joined == "merge-base --is-ancestor "+remote+" "+commit:
			return "", "", nil, nil
		case joined == "lfs ls-files --long "+commit:
			return oid + " * assets/model.bin\n", "", nil, nil
		case joined == "diff --name-only -z "+remote+" "+commit:
			return "assets/model.bin\x00", "", nil, nil
		case joined == "lfs push --object-id origin "+oid:
			if !network || timeout != 30*time.Minute {
				t.Fatalf("LFS upload sandbox options mismatch: network=%v timeout=%v", network, timeout)
			}
			return "", "", nil, nil
		case joined == "push origin "+commit+":refs/heads/main":
			if len(commands) < 2 || commands[len(commands)-2] != "lfs push --object-id origin "+oid {
				t.Fatalf("Git ref push ran before LFS upload: %v", commands)
			}
			return "", "", nil, nil
		default:
			t.Fatalf("unexpected Git command: %q (network=%v)", joined, network)
			return "", "", errors.New("unexpected Git command"), nil
		}
	}

	remoteSHA, pushAttempted, err := service.PublishProjectCommit(context.Background(), "https://github.com/example/project.git", workdir, "main", remote, commit)
	if err != nil || !pushAttempted || remoteSHA != commit {
		t.Fatalf("publication = remote %q, attempted %v, err %v", remoteSHA, pushAttempted, err)
	}
	if len(commands) == 0 || commands[len(commands)-1] != "ls-remote --heads origin refs/heads/main" {
		t.Fatalf("publication did not read back the remote ref: %v", commands)
	}
}

func TestPublishProjectCommitDoesNotPushRefWhenLFSContentIsNotHydrated(t *testing.T) {
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const remote = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const oid = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	service := &Service{}
	service.sandboxCommand = func(_ context.Context, _ coretools.ExecutionConfig, command string, args []string, dir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)
		switch {
		case joined == "check-ref-format --branch main", joined == "status --porcelain --untracked-files=all", joined == "cat-file -e "+commit+"^{commit}", joined == "fetch --no-tags origin refs/heads/main", joined == "merge-base --is-ancestor "+remote+" "+commit:
			return "", "", nil, nil
		case joined == "rev-parse --show-toplevel":
			return dir, "", nil, nil
		case joined == "remote get-url origin":
			return "https://github.com/example/project.git", "", nil, nil
		case joined == "rev-parse --verify HEAD^{commit}":
			return commit, "", nil, nil
		case joined == "ls-remote --heads origin refs/heads/main":
			return remote + "\trefs/heads/main\n", "", nil, nil
		case joined == "lfs ls-files --long "+commit:
			return oid + " - assets/model.bin\n", "", nil, nil
		case joined == "diff --name-only -z "+remote+" "+commit:
			return "assets/model.bin\x00", "", nil, nil
		default:
			return "", "", errors.New("unexpected Git command: " + joined), nil
		}
	}

	remoteSHA, pushAttempted, err := service.PublishProjectCommit(context.Background(), "https://github.com/example/project.git", workdir, "main", remote, commit)
	if err == nil || pushAttempted || remoteSHA != "" {
		t.Fatalf("unhydrated LFS publication = remote %q, attempted %v, err %v", remoteSHA, pushAttempted, err)
	}
	for _, command := range commands {
		if strings.HasPrefix(command, "push origin ") {
			t.Fatalf("published a Git ref although LFS content was unavailable: %v", commands)
		}
	}
}

func TestPublishProjectCommitDoesNotPushRefWhenLFSUploadFails(t *testing.T) {
	const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const remote = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const oid = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	service := &Service{}
	service.sandboxCommand = func(_ context.Context, _ coretools.ExecutionConfig, command string, args []string, dir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		joined := strings.Join(args, " ")
		commands = append(commands, joined)
		switch {
		case joined == "check-ref-format --branch main", joined == "status --porcelain --untracked-files=all", joined == "cat-file -e "+commit+"^{commit}", joined == "fetch --no-tags origin refs/heads/main", joined == "merge-base --is-ancestor "+remote+" "+commit:
			return "", "", nil, nil
		case joined == "rev-parse --show-toplevel":
			return dir, "", nil, nil
		case joined == "remote get-url origin":
			return "https://github.com/example/project.git", "", nil, nil
		case joined == "rev-parse --verify HEAD^{commit}":
			return commit, "", nil, nil
		case joined == "ls-remote --heads origin refs/heads/main":
			return remote + "\trefs/heads/main\n", "", nil, nil
		case joined == "lfs ls-files --long "+commit:
			return oid + " * assets/model.bin\n", "", nil, nil
		case joined == "diff --name-only -z "+remote+" "+commit:
			return "assets/model.bin\x00", "", nil, nil
		case joined == "lfs push --object-id origin "+oid:
			return "", "simulated LFS endpoint failure", errors.New("upload failed"), nil
		default:
			return "", "", errors.New("unexpected Git command: " + joined), nil
		}
	}

	remoteSHA, pushAttempted, err := service.PublishProjectCommit(context.Background(), "https://github.com/example/project.git", workdir, "main", remote, commit)
	if err == nil || pushAttempted || remoteSHA != "" || !strings.Contains(err.Error(), "upload failed") {
		t.Fatalf("failed LFS publication = remote %q, attempted %v, err %v", remoteSHA, pushAttempted, err)
	}
	for _, command := range commands {
		if strings.HasPrefix(command, "push origin ") {
			t.Fatalf("published a Git ref after LFS upload failed: %v", commands)
		}
	}
}
