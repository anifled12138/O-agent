package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestPrepareProjectSnapshotRejectsUnpinnedProjectBeforeFilesystemEffects(t *testing.T) {
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
	workspace := filepath.Join(t.TempDir(), "node-workspace")
	service := &Service{store: store, workspaceRoot: workspace}

	_, err = service.PrepareProjectSnapshot(ctx, owner, domain.Project{
		ID: "cloud-project-1", Name: "example", RemoteRepoURL: "https://github.com/example/project",
		ResolvedCommit: "", RemoteBranch: "main",
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("PrepareProjectSnapshot error = %v, want invalid source snapshot", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".o-projects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project cache root exists after rejecting invalid snapshot: stat error=%v", err)
	}
	if _, err := store.Project(ctx, owner, "cloud-project-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("invalid project metadata was persisted: err=%v", err)
	}
}

func TestCheckoutGitCommitRejectsInvalidCommitBeforeTouchingWorkspace(t *testing.T) {
	service := &Service{}
	workdir := filepath.Join(t.TempDir(), "nonexistent-worktree")
	err := service.CheckoutGitCommit(context.Background(), "https://github.com/example/project", workdir, "not-a-commit")
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("CheckoutGitCommit error = %v, want invalid commit", err)
	}
	if _, err := os.Stat(workdir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid commit validation touched the worktree: stat error=%v", err)
	}
}
