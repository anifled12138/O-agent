package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/projectquota"
	"axiom.local/agent/internal/storage"
)

func TestCreateProjectDeltaBundlePreservesWorktreeAndAppliesOnPinnedBase(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}
	ctx := context.Background()
	testRoot, err := os.MkdirTemp(".", ".project-delta-test-")
	if err != nil {
		t.Fatal(err)
	}
	testRoot, err = filepath.Abs(testRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(testRoot); err != nil {
			t.Errorf("remove project delta test root: %v", err)
		}
	}()
	workdir := filepath.Join(testRoot, "source")
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
	if err := os.WriteFile(filepath.Join(workdir, "remove.txt"), []byte("delete me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, workdir, "add", "--all")
	runGitTest(t, ctx, git, workdir, "commit", "-m", "base")
	base := runGitTest(t, ctx, git, workdir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workdir, "README.md"), []byte("updated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workdir, "remove.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "new.txt"), []byte("new file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, workdir, "add", "--all")
	runGitTest(t, ctx, git, workdir, "commit", "-m", "user commit during task")
	committedHead := runGitTest(t, ctx, git, workdir, "rev-parse", "HEAD")
	tree := runGitTest(t, ctx, git, workdir, "rev-parse", "HEAD^{tree}")
	divergent := runGitTest(t, ctx, git, workdir, "-c", "user.name=Test User", "-c", "user.email=test@example.invalid", "commit-tree", tree, "-m", "unrelated history")
	runGitTest(t, ctx, git, workdir, "checkout", "--detach", divergent)
	divergentService := &Service{sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, dir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err, nil
	}}
	if _, err := divergentService.CreateProjectDeltaBundle(ctx, domain.Project{ID: "project-divergent", Workdir: workdir, RemoteRepoURL: "https://github.com/example/project.git", ResolvedCommit: base}, "task_delta_divergent"); err == nil {
		t.Fatal("project delta accepted a HEAD outside the pinned base history")
	}
	runGitTest(t, ctx, git, workdir, "checkout", "--detach", committedHead)
	if err := os.WriteFile(filepath.Join(workdir, "README.md"), []byte("updated after commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "after-commit.txt"), []byte("uncommitted task output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	taskHead := runGitTest(t, ctx, git, workdir, "rev-parse", "HEAD")
	statusBefore := runGitTest(t, ctx, git, workdir, "status", "--porcelain=v1", "-z")
	failBundleCreate := false
	failWorktreeAdd := false
	failExecutionWorktreeAdd := false
	service := &Service{sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, dir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err == nil && failBundleCreate && len(args) > 2 && args[2] == "bundle" && args[3] == "create" {
			failBundleCreate = false
			return stdout.String(), "injected post-write bundle failure", fmt.Errorf("injected bundle failure"), nil
		}
		if err == nil && failWorktreeAdd && len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
			failWorktreeAdd = false
			return stdout.String(), "injected post-create worktree failure", fmt.Errorf("injected worktree failure"), nil
		}
		if err == nil && failExecutionWorktreeAdd && len(args) > 2 && args[0] == "worktree" && args[1] == "add" && args[2] == "-b" {
			failExecutionWorktreeAdd = false
			return stdout.String(), "injected post-create execution worktree failure", fmt.Errorf("injected execution worktree failure"), nil
		}
		return stdout.String(), stderr.String(), err, nil
	}}
	project := domain.Project{ID: "project-delta-test", Workdir: workdir, RemoteRepoURL: "https://github.com/example/project.git", ResolvedCommit: base}
	delta, err := service.CreateProjectDeltaBundle(ctx, project, "task_delta_test")
	if err != nil {
		t.Fatal(err)
	}
	if delta.Path == "" || delta.BaseCommit != base || delta.Commit == "" || delta.Commit == base {
		t.Fatalf("delta bundle metadata = %+v", delta)
	}
	deltaRetry, err := service.CreateProjectDeltaBundle(ctx, project, "task_delta_test")
	if err != nil || deltaRetry.Path != delta.Path || deltaRetry.BaseCommit != delta.BaseCommit || deltaRetry.Commit != delta.Commit {
		t.Fatalf("existing task bundle did not reconcile idempotently: first=%+v retry=%+v err=%v", delta, deltaRetry, err)
	}
	if got := runGitTest(t, ctx, git, workdir, "rev-parse", "HEAD"); got != taskHead {
		t.Fatalf("creating delta changed the user's HEAD: got %s want %s", got, taskHead)
	}
	if got := runGitTest(t, ctx, git, workdir, "status", "--porcelain=v1", "-z"); got != statusBefore {
		t.Fatalf("creating delta changed the user's index/worktree state: before=%q after=%q", statusBefore, got)
	}
	receiver := filepath.Join(testRoot, "receiver")
	if err := os.MkdirAll(receiver, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, receiver, "init")
	runGitTest(t, ctx, git, receiver, "fetch", workdir, base+":refs/heads/base")
	runGitTest(t, ctx, git, receiver, "checkout", "-b", "main", base)
	runGitTest(t, ctx, git, receiver, "fetch", delta.Path, "refs/o-agent/task-deltas/task_delta_test:refs/heads/task-delta")
	runGitTest(t, ctx, git, receiver, "checkout", "task-delta")
	if got, err := os.ReadFile(filepath.Join(receiver, "README.md")); err != nil || strings.ReplaceAll(string(got), "\r\n", "\n") != "updated after commit\n" {
		t.Fatalf("updated tracked file did not arrive from delta: content=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(receiver, "new.txt")); err != nil || strings.ReplaceAll(string(got), "\r\n", "\n") != "new file\n" {
		t.Fatalf("untracked file did not arrive from delta: content=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(receiver, "after-commit.txt")); err != nil || strings.ReplaceAll(string(got), "\r\n", "\n") != "uncommitted task output\n" {
		t.Fatalf("uncommitted file after a user commit did not arrive from delta: content=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(receiver, "remove.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file was not removed by delta: stat error=%v", err)
	}
	if strings.TrimSpace(runGitTest(t, ctx, git, receiver, "rev-parse", "HEAD")) != delta.Commit {
		t.Fatal("receiver did not read back the bundled commit")
	}
	runGitTest(t, ctx, git, receiver, "checkout", "main")
	runGitTest(t, ctx, git, receiver, "remote", "add", "origin", project.RemoteRepoURL)
	cloudDataDir := filepath.Join(testRoot, "cloud-data")
	if err := os.MkdirAll(cloudDataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(cloudDataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close cloud project storage: %v", err)
		}
	}()
	ownerID, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cloudProject := domain.Project{ID: project.ID, UserID: ownerID, Name: "source", Workdir: receiver, RemoteRepoURL: project.RemoteRepoURL, RemoteBranch: "main", RepositoryProvider: "github", ResolvedCommit: base}
	if err := store.CreateProject(ctx, cloudProject); err != nil {
		t.Fatal(err)
	}
	bundleBytes, err := os.ReadFile(delta.Path)
	if err != nil {
		t.Fatal(err)
	}
	bundleDigest := sha256.Sum256(bundleBytes)
	service.store = store
	service.workspaceRoot = testRoot
	workspaceQuota := &fakeWorkspaceQuotaManager{quotas: make(map[uint32]projectquota.WorkspaceQuota)}
	if err := service.ConfigureWorkspaceQuota(workspaceQuota, 256<<20); err != nil {
		t.Fatal(err)
	}
	failExecutionWorktreeAdd = true
	if _, err := service.EnsureExecutionTaskWorkspace(ctx, cloudProject.UserID, cloudProject, "task_execution_rollback"); err == nil {
		t.Fatal("post-create execution worktree failure was reported as successful")
	}
	if failExecutionWorktreeAdd {
		t.Fatal("execution workspace test did not reach injected worktree failure")
	}
	executionRollbackKey := sha256.Sum256([]byte(cloudProject.ID + "\x00task_execution_rollback"))
	rollbackWorkspace := filepath.Join(service.workspaceRoot, ".o-projects", "execution-"+hex.EncodeToString(executionRollbackKey[:12]))
	if _, err := os.Stat(rollbackWorkspace); !os.IsNotExist(err) {
		t.Fatalf("failed execution workspace creation left its worktree behind: %v", err)
	}
	if _, err := store.Project(ctx, cloudProject.UserID, ProjectDeltaProjectID(cloudProject.ID, "task_execution_rollback")); err == nil {
		t.Fatal("failed execution workspace creation persisted its project mapping")
	}
	rollbackBinding, err := store.ExecutionTaskWorkspace(ctx, cloudProject.UserID, "task_execution_rollback")
	if err != nil || rollbackBinding.Status != "degraded" || rollbackBinding.Error != "worktree_creation_rolled_back" || rollbackBinding.QuotaState != "released" || rollbackBinding.QuotaProjectID <= 0 {
		t.Fatalf("rolled-back execution workspace failure was not durably recorded for recovery: %+v err=%v", rollbackBinding, err)
	}
	workspaceQuota.mu.Lock()
	_, rollbackQuotaStillPresent := workspaceQuota.quotas[uint32(rollbackBinding.QuotaProjectID)]
	workspaceQuota.mu.Unlock()
	if rollbackQuotaStillPresent {
		t.Fatal("rolled-back Git worktree left its kernel quota allocation assigned")
	}
	if got := runGitTest(t, ctx, git, receiver, "for-each-ref", "--format=%(refname)", "refs/heads/codex/task-task_execution_rollback"); got != "" {
		t.Fatalf("failed execution workspace creation left its branch behind: %q", got)
	}
	lfsRollbackDelta, err := service.CreateProjectDeltaBundle(ctx, project, "task_delta_lfs_rollback")
	if err != nil {
		t.Fatal(err)
	}
	lfsRollbackBundle, err := os.ReadFile(lfsRollbackDelta.Path)
	if err != nil {
		t.Fatal(err)
	}
	lfsRollbackBundleSHA := sha256.Sum256(lfsRollbackBundle)
	unexpectedArchive := []byte("unexpected LFS archive")
	unexpectedArchiveSHA := sha256.Sum256(unexpectedArchive)
	if _, _, err := service.ImportProjectDeltaBundleWithLFS(ctx, cloudProject.UserID, cloudProject, "task_delta_lfs_rollback", bytes.NewReader(lfsRollbackBundle), int64(len(lfsRollbackBundle)), hex.EncodeToString(lfsRollbackBundleSHA[:]), lfsRollbackDelta.BaseCommit, lfsRollbackDelta.Commit, bytes.NewReader(unexpectedArchive), int64(len(unexpectedArchive)), hex.EncodeToString(unexpectedArchiveSHA[:])); err == nil {
		t.Fatal("task project import reported success after its Git LFS archive failed validation")
	}
	lfsRollbackKey := sha256.Sum256([]byte(cloudProject.ID + "\x00task_delta_lfs_rollback"))
	lfsRollbackWorktree := filepath.Join(service.workspaceRoot, ".o-projects", "task-"+hex.EncodeToString(lfsRollbackKey[:12]))
	if _, err := os.Lstat(lfsRollbackWorktree); !os.IsNotExist(err) {
		t.Fatalf("failed Git LFS validation exposed a partial task worktree: %v", err)
	}
	if _, err := store.Project(ctx, cloudProject.UserID, ProjectDeltaProjectID(cloudProject.ID, "task_delta_lfs_rollback")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed Git LFS validation persisted a usable task project: err=%v", err)
	}
	imported, created, err := service.ImportProjectDeltaBundle(ctx, cloudProject.UserID, cloudProject, "task_delta_test", bytes.NewReader(bundleBytes), int64(len(bundleBytes)), hex.EncodeToString(bundleDigest[:]), base, delta.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if !created || imported.ID == cloudProject.ID || imported.ResolvedCommit != delta.Commit || imported.RemoteBranch != "codex/task-task_delta_test" {
		t.Fatalf("cloud project import result = %+v, created=%v", imported, created)
	}
	if got := runGitTest(t, ctx, git, receiver, "rev-parse", "HEAD"); got != base {
		t.Fatalf("cloud source project was moved by isolated import: got %s want %s", got, base)
	}
	if content, err := os.ReadFile(filepath.Join(imported.Workdir, "README.md")); err != nil || strings.ReplaceAll(string(content), "\r\n", "\n") != "updated after commit\n" {
		t.Fatalf("imported worktree does not contain the local result: content=%q err=%v", content, err)
	}
	executionWorkspace, err := service.EnsureExecutionTaskWorkspace(ctx, cloudProject.UserID, cloudProject, "task_execution_isolation")
	if err != nil {
		t.Fatalf("create durable execution task worktree: %v", err)
	}
	if executionWorkspace.Workdir == cloudProject.Workdir || executionWorkspace.ID != ProjectDeltaProjectID(cloudProject.ID, "task_execution_isolation") || executionWorkspace.RemoteBranch != "codex/task-task_execution_isolation" || !strings.EqualFold(executionWorkspace.ResolvedCommit, base) {
		t.Fatalf("execution task worktree did not bind to an isolated pinned project: %+v", executionWorkspace)
	}
	quotaBinding, err := store.ExecutionTaskWorkspace(ctx, cloudProject.UserID, "task_execution_isolation")
	if err != nil || quotaBinding.Status != "ready" || quotaBinding.QuotaState != "applied" || quotaBinding.QuotaProjectID <= 0 || quotaBinding.QuotaLimitBytes != 256<<20 {
		t.Fatalf("Git worktree was exposed without a durable task hard quota: %+v err=%v", quotaBinding, err)
	}
	if err := os.WriteFile(filepath.Join(executionWorkspace.Workdir, "isolated-task.txt"), []byte("task-only output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cloudProject.Workdir, "isolated-task.txt")); !os.IsNotExist(err) {
		t.Fatalf("task output leaked into the shared source project: %v", err)
	}
	workspaceRetry, err := service.EnsureExecutionTaskWorkspace(ctx, cloudProject.UserID, cloudProject, "task_execution_isolation")
	if err != nil || workspaceRetry.Workdir != executionWorkspace.Workdir {
		t.Fatalf("execution task workspace retry did not read back its durable mapping: workspace=%+v err=%v", workspaceRetry, err)
	}
	if err := os.WriteFile(filepath.Join(imported.Workdir, "continued.txt"), []byte("continued from imported worktree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedWorktreeStatus := runGitTest(t, ctx, git, imported.Workdir, "status", "--porcelain=v1", "-z")
	linkedWorktreeDelta, err := service.CreateProjectDeltaBundle(ctx, imported, "task_delta_linked_worktree")
	if err != nil {
		t.Fatalf("create project delta from an imported linked worktree: %v", err)
	}
	if linkedWorktreeDelta.BaseCommit != delta.Commit || linkedWorktreeDelta.Commit == delta.Commit {
		t.Fatalf("linked-worktree delta metadata = %+v", linkedWorktreeDelta)
	}
	if got := runGitTest(t, ctx, git, imported.Workdir, "rev-parse", "HEAD"); got != delta.Commit {
		t.Fatalf("linked-worktree delta changed HEAD: got %s want %s", got, delta.Commit)
	}
	if got := runGitTest(t, ctx, git, imported.Workdir, "status", "--porcelain=v1", "-z"); got != linkedWorktreeStatus {
		t.Fatalf("linked-worktree delta changed index/worktree state: before=%q after=%q", linkedWorktreeStatus, got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(cloudDataDir)
	if err != nil {
		t.Fatal(err)
	}
	service.store = store
	persisted, err := store.Project(ctx, cloudProject.UserID, imported.ID)
	if err != nil || persisted.ResolvedCommit != delta.Commit || persisted.Workdir != imported.Workdir {
		t.Fatalf("cloud task project did not survive storage restart: project=%+v err=%v", persisted, err)
	}
	persistedWorkspace, err := service.EnsureExecutionTaskWorkspace(ctx, cloudProject.UserID, cloudProject, "task_execution_isolation")
	if err != nil || persistedWorkspace.Workdir != executionWorkspace.Workdir {
		t.Fatalf("execution task workspace did not survive storage restart: project=%+v err=%v", persistedWorkspace, err)
	}
	if content, err := os.ReadFile(filepath.Join(persistedWorkspace.Workdir, "isolated-task.txt")); err != nil || strings.ReplaceAll(string(content), "\r\n", "\n") != "task-only output\n" {
		t.Fatalf("task output did not survive workspace reattachment: content=%q err=%v", content, err)
	}
	duplicate, duplicateCreated, err := service.ImportProjectDeltaBundle(ctx, cloudProject.UserID, cloudProject, "task_delta_test", bytes.NewReader(bundleBytes), int64(len(bundleBytes)), hex.EncodeToString(bundleDigest[:]), base, delta.Commit)
	if err != nil || duplicateCreated || duplicate.ID != imported.ID {
		t.Fatalf("idempotent import = %+v, created=%v, err=%v", duplicate, duplicateCreated, err)
	}
	rollbackDelta, err := service.CreateProjectDeltaBundle(ctx, project, "task_delta_rollback")
	if err != nil {
		t.Fatal(err)
	}
	rollbackBytes, err := os.ReadFile(rollbackDelta.Path)
	if err != nil {
		t.Fatal(err)
	}
	rollbackDigest := sha256.Sum256(rollbackBytes)
	rollbackKey := sha256.Sum256([]byte(cloudProject.ID + "\x00task_delta_rollback"))
	rollbackWorktree := filepath.Join(service.workspaceRoot, ".o-projects", "task-"+hex.EncodeToString(rollbackKey[:12]))
	failWorktreeAdd = true
	if _, _, err := service.ImportProjectDeltaBundle(ctx, cloudProject.UserID, cloudProject, "task_delta_rollback", bytes.NewReader(rollbackBytes), int64(len(rollbackBytes)), hex.EncodeToString(rollbackDigest[:]), base, rollbackDelta.Commit); err == nil {
		t.Fatal("injected post-create worktree failure was reported as successful")
	}
	if failWorktreeAdd {
		t.Fatal("test did not reach the injected worktree failure")
	}
	rollbackProjectID := ProjectDeltaProjectID(cloudProject.ID, "task_delta_rollback")
	if _, err := store.Project(ctx, cloudProject.UserID, rollbackProjectID); err == nil {
		t.Fatal("failed import persisted a cloud task project")
	}
	if _, err := os.Stat(rollbackWorktree); !os.IsNotExist(err) {
		t.Fatalf("failed import left a worktree behind: %v", err)
	}
	if got := runGitTest(t, ctx, git, receiver, "for-each-ref", "--format=%(refname)", "refs/heads/codex/task-task_delta_rollback", "refs/o-agent/imports/task_delta_rollback"); got != "" {
		t.Fatalf("failed import left branch/import ref behind: %q", got)
	}
	if err := os.WriteFile(filepath.Join(workdir, "another.txt"), []byte("another file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failureStatusBefore := runGitTest(t, ctx, git, workdir, "status", "--porcelain=v1", "-z")
	failBundleCreate = true
	if _, err := service.CreateProjectDeltaBundle(ctx, project, "task_delta_failure"); err == nil {
		t.Fatal("injected bundle failure was reported as successful")
	}
	if failBundleCreate {
		t.Fatal("test did not reach the injected bundle creation failure")
	}
	if got := runGitTest(t, ctx, git, workdir, "status", "--porcelain=v1", "-z"); got != failureStatusBefore {
		t.Fatalf("failed bundle creation changed the user's index/worktree state: before=%q after=%q", failureStatusBefore, got)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git", "o-agent-delta-task_delta_failure.bundle")); !os.IsNotExist(err) {
		t.Fatalf("failed bundle left a partial output file: stat error=%v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(workdir, ".git", ".o-agent-delta-task_delta_failure-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("failed bundle left temporary Git metadata: leftovers=%v err=%v", leftovers, err)
	}
}

func TestCreateProjectDeltaBundleRetainsGitLFSObjectsInRepositoryStorage(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}
	ctx := context.Background()
	testRoot, err := os.MkdirTemp(".", ".project-lfs-test-")
	if err != nil {
		t.Fatal(err)
	}
	testRoot, err = filepath.Abs(testRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		deadline := time.Now().Add(time.Second)
		for {
			err := os.RemoveAll(testRoot)
			// A finished Git LFS filter can briefly retain a directory handle
			// on Windows. Retry only that sharing violation, never other errors.
			if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if err != nil {
				t.Errorf("remove project LFS test root: %v", err)
			} else if _, err := os.Lstat(testRoot); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("project LFS test root still exists after cleanup: %v", err)
			}
			return
		}
	}()
	workdir := filepath.Join(testRoot, "source")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, git, "lfs", "version").CombinedOutput(); err != nil {
		t.Skipf("Git LFS is unavailable: %v: %s", err, output)
	}
	runGitTest(t, ctx, git, workdir, "init")
	runGitTest(t, ctx, git, workdir, "config", "user.name", "Test User")
	runGitTest(t, ctx, git, workdir, "config", "user.email", "test@example.invalid")
	runGitTest(t, ctx, git, workdir, "remote", "add", "origin", "https://github.com/example/project.git")
	runGitTest(t, ctx, git, workdir, "lfs", "install", "--local")
	runGitTest(t, ctx, git, workdir, "lfs", "track", "*.bin")
	runGitTest(t, ctx, git, workdir, "add", ".gitattributes")
	runGitTest(t, ctx, git, workdir, "commit", "-m", "configure LFS")
	base := runGitTest(t, ctx, git, workdir, "rev-parse", "HEAD")
	content := bytes.Repeat([]byte("task-local-lfs-payload\n"), 8192)
	if err := os.WriteFile(filepath.Join(workdir, "generated.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "project-lfs-delta", Workdir: workdir, RemoteRepoURL: "https://github.com/example/project.git", ResolvedCommit: base}
	service := &Service{sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, dir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err, nil
	}}
	delta, err := service.CreateProjectDeltaBundle(ctx, project, "task_lfs_delta")
	if err != nil {
		t.Fatalf("create LFS-backed task delta: %v", err)
	}
	if delta.Path == "" || delta.Commit == base {
		t.Fatalf("LFS-backed task delta was not created: %+v", delta)
	}
	if delta.LFSObjectsPath == "" {
		t.Fatal("LFS-backed task delta did not produce a transferable object archive")
	}
	digest := sha256.Sum256(content)
	oid := hex.EncodeToString(digest[:])
	objectPath := filepath.Join(workdir, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	stored, err := os.ReadFile(objectPath)
	if err != nil {
		t.Fatalf("delta generation discarded the LFS object from its temporary Git metadata instead of retaining it in repository storage: %v", err)
	}
	if !bytes.Equal(stored, content) {
		t.Fatalf("repository LFS object does not match pointer OID %s", oid)
	}
	archiveBytes, err := os.ReadFile(delta.LFSObjectsPath)
	if err != nil || len(archiveBytes) == 0 {
		t.Fatalf("LFS task object archive is missing or empty: size=%d err=%v", len(archiveBytes), err)
	}
	receiver := filepath.Join(testRoot, "receiver")
	if err := os.MkdirAll(receiver, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, receiver, "init")
	runGitTest(t, ctx, git, receiver, "remote", "add", "origin", project.RemoteRepoURL)
	runGitTest(t, ctx, git, receiver, "fetch", workdir, base)
	runGitTest(t, ctx, git, receiver, "fetch", delta.Path, "refs/o-agent/task-deltas/task_lfs_delta")
	runGitTest(t, ctx, git, receiver, "config", "filter.lfs.smudge", "git-lfs smudge --skip -- %f")
	previousSkipSmudge, hadSkipSmudge := os.LookupEnv("GIT_LFS_SKIP_SMUDGE")
	if err := os.Setenv("GIT_LFS_SKIP_SMUDGE", "1"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if hadSkipSmudge {
			_ = os.Setenv("GIT_LFS_SKIP_SMUDGE", previousSkipSmudge)
		} else {
			_ = os.Unsetenv("GIT_LFS_SKIP_SMUDGE")
		}
	}()
	runGitTest(t, ctx, git, receiver, "checkout", "-b", "received-task", delta.Commit)
	transferService := &Service{sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, dir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err, nil
	}}
	projectAtReceiver := domain.Project{ID: "project-lfs-receiver", Workdir: receiver}
	archiveDigest := sha256.Sum256(archiveBytes)
	if err := transferService.ImportProjectLFSObjects(ctx, projectAtReceiver, delta.BaseCommit, delta.Commit, bytes.NewReader(archiveBytes), int64(len(archiveBytes)), strings.Repeat("0", 64)); err == nil {
		t.Fatal("Git LFS archive with a mismatched outer digest was accepted")
	}
	if pointer, err := os.ReadFile(filepath.Join(receiver, "generated.bin")); err != nil || bytes.Equal(pointer, content) {
		t.Fatalf("failed LFS archive verification exposed a completed hydrated workspace: size=%d err=%v", len(pointer), err)
	}
	if err := transferService.ImportProjectLFSObjects(ctx, projectAtReceiver, delta.BaseCommit, delta.Commit, bytes.NewReader(archiveBytes), int64(len(archiveBytes)), hex.EncodeToString(archiveDigest[:])); err != nil {
		t.Fatalf("receive and hydrate Git LFS objects across nodes: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(receiver, "generated.bin")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("received Git LFS project file is not hydrated: size=%d err=%v", len(got), err)
	}
	if err := transferService.ImportProjectLFSObjects(ctx, projectAtReceiver, delta.BaseCommit, delta.Commit, bytes.NewReader(archiveBytes), int64(len(archiveBytes)), hex.EncodeToString(archiveDigest[:])); err != nil {
		t.Fatalf("idempotent Git LFS object import failed: %v", err)
	}
	retry, err := service.CreateProjectDeltaBundle(ctx, project, "task_lfs_delta")
	if err != nil || retry.Commit != delta.Commit || retry.Path != delta.Path {
		t.Fatalf("retry did not read back the same LFS-backed delta: initial=%+v retry=%+v err=%v", delta, retry, err)
	}
	stored, err = os.ReadFile(objectPath)
	if err != nil || !bytes.Equal(stored, content) {
		t.Fatalf("delta recovery discarded or changed the durable Git LFS object: size=%d err=%v", len(stored), err)
	}
}

func runGitTest(t *testing.T, ctx context.Context, git, directory string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Dir = directory
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
