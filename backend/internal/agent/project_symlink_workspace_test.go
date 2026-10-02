package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestExecutionTaskWorkspaceAllowsGitSymlink(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	if err := os.Mkdir(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit := func(workdir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = workdir
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return string(output)
	}
	runGit(sourceDir, "init")
	runGit(sourceDir, "config", "user.name", "O Agent Test")
	runGit(sourceDir, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("safe target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(sourceDir, "README-link")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	runGit(sourceDir, "add", "README.md", "README-link")
	runGit(sourceDir, "commit", "-m", "add project symlink")
	commit := strings.TrimSpace(runGit(sourceDir, "rev-parse", "HEAD"))
	remote := "https://github.com/example/project.git"
	runGit(sourceDir, "remote", "add", "origin", remote)
	branch := strings.TrimSpace(runGit(sourceDir, "branch", "--show-current"))
	now := time.Now().UTC()
	source := domain.Project{ID: "project_symlink", UserID: owner, Name: "symlink project", Workdir: sourceDir, RemoteRepoURL: remote, RemoteBranch: branch, RepositoryProvider: "github", ResolvedCommit: commit, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, source); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		store:         store,
		workspaceRoot: root,
		sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, workdir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
			cmd := exec.CommandContext(ctx, command, args...)
			cmd.Dir = workdir
			output, err := cmd.CombinedOutput()
			if err != nil {
				return "", string(output), err, nil
			}
			return string(output), "", nil, nil
		},
	}
	workspace, err := service.EnsureExecutionTaskWorkspace(ctx, owner, source, "task_symlink")
	if err != nil {
		t.Fatalf("legal Git symlink blocked cloud task workspace creation: %v", err)
	}
	readBack, err := store.Project(ctx, owner, workspace.ID)
	if err != nil || readBack.ResolvedCommit != commit || readBack.MeasuredBytes < 0 {
		t.Fatalf("symlink project task binding did not durably read back: project=%+v err=%v", readBack, err)
	}
	if _, err := os.Lstat(filepath.Join(workspace.Workdir, "README-link")); err != nil {
		t.Fatalf("task worktree lost the repository symlink entry: %v", err)
	}
	binding, err := store.ExecutionTaskWorkspace(ctx, owner, "task_symlink")
	if err != nil || binding.Status != "ready" || binding.Workdir != workspace.Workdir {
		t.Fatalf("symlink task workspace did not reach durable ready state: binding=%+v err=%v", binding, err)
	}
}
