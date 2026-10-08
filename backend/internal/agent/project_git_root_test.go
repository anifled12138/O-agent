package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
)

func TestCreateProjectDeltaRejectsOtherGitRootWithoutChangingSource(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is unavailable")
	}
	ctx := context.Background()
	workdir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, workdir, "init")
	runGitTest(t, ctx, git, workdir, "config", "user.name", "Test User")
	runGitTest(t, ctx, git, workdir, "config", "user.email", "test@example.invalid")
	runGitTest(t, ctx, git, workdir, "remote", "add", "origin", "https://github.com/example/project.git")
	if err := os.WriteFile(filepath.Join(workdir, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, workdir, "add", "README.md")
	runGitTest(t, ctx, git, workdir, "commit", "-m", "base")
	base := runGitTest(t, ctx, git, workdir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workdir, "README.md"), []byte("pending user changes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := runGitTest(t, ctx, git, workdir, "status", "--porcelain=v1", "-z")
	for _, root := range []string{filepath.Dir(workdir), filepath.Join(workdir, "nested-repository")} {
		t.Run(filepath.Base(root), func(t *testing.T) {
			service := &Service{sandboxCommand: func(_ context.Context, _ coretools.ExecutionConfig, _ string, args []string, _ string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
				if len(args) != 2 || args[0] != "rev-parse" || args[1] != "--show-toplevel" {
					t.Fatalf("unexpected command after a conflicting repository root: %v", args)
				}
				return root, "", nil, nil
			}}
			project := domain.Project{ID: "project_root_conflict", Workdir: workdir, RemoteRepoURL: "https://github.com/example/project.git", ResolvedCommit: base}
			if _, err := service.CreateProjectDeltaBundle(ctx, project, "task_root_conflict"); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("different repository root must be rejected: %v", err)
			}
			if head := runGitTest(t, ctx, git, workdir, "rev-parse", "HEAD"); head != base {
				t.Fatalf("rejected delta changed the source commit: %s", head)
			}
			if after := runGitTest(t, ctx, git, workdir, "status", "--porcelain=v1", "-z"); after != before {
				t.Fatalf("rejected delta changed pending work: before=%q after=%q", before, after)
			}
			bundle := filepath.Join(workdir, ".git", "o-agent-delta-task_root_conflict.bundle")
			if _, err := os.Lstat(bundle); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected delta left a bundle behind: %v", err)
			}
		})
	}
}
