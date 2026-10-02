package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestExecutionTaskWorkspaceBindingPersistsAndTransitionsAtomically(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := ExecutionTaskWorkspace{UserID: owner, TaskID: "cloud_task_workspace_1", Workdir: filepath.Join(t.TempDir(), "task-workspace")}
	created, wasCreated, err := store.EnsureExecutionTaskWorkspace(ctx, request, now)
	if err != nil || !wasCreated || created.Status != "preparing" || created.QuotaState != "not_configured" {
		t.Fatalf("initial durable workspace reservation = %+v created=%t err=%v", created, wasCreated, err)
	}
	retry, wasCreated, err := store.EnsureExecutionTaskWorkspace(ctx, request, now.Add(time.Second))
	if err != nil || wasCreated || retry.Workdir != created.Workdir || retry.TaskID != created.TaskID {
		t.Fatalf("workspace reservation retry = %+v created=%t err=%v", retry, wasCreated, err)
	}
	conflicting := request
	conflicting.Workdir = filepath.Join(t.TempDir(), "different-workspace")
	if _, _, err := store.EnsureExecutionTaskWorkspace(ctx, conflicting, now.Add(2*time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("workspace identity conflict = %v, want conflict", err)
	}

	ready, err := store.SetExecutionTaskWorkspaceState(ctx, owner, request.TaskID, "preparing", "ready", "not_configured", 0, 0, "", now.Add(3*time.Second))
	if err != nil || ready.Status != "ready" || ready.QuotaState != "not_configured" {
		t.Fatalf("workspace ready transition did not read back: %+v err=%v", ready, err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER force_task_workspace_state_failure BEFORE UPDATE ON execution_task_workspaces WHEN NEW.status='degraded' BEGIN SELECT RAISE(ABORT,'forced workspace state failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, request.TaskID, "ready", "degraded", "degraded", 0, 0, "injected failure", now.Add(4*time.Second)); err == nil {
		t.Fatal("workspace state trigger failure was swallowed")
	}
	persistedAfterFailure, err := store.ExecutionTaskWorkspace(ctx, owner, request.TaskID)
	if err != nil || persistedAfterFailure.Status != "ready" || persistedAfterFailure.QuotaState != "not_configured" {
		t.Fatalf("failed workspace state mutation left partial state: %+v err=%v", persistedAfterFailure, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	afterRestart, err := restarted.ExecutionTaskWorkspace(ctx, owner, request.TaskID)
	if err != nil || afterRestart.Status != "ready" || afterRestart.Workdir != request.Workdir {
		t.Fatalf("workspace binding did not survive database restart: %+v err=%v", afterRestart, err)
	}
}

func TestProjectCannotAcquireWorkspaceAfterReleaseBegins(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	workdir := filepath.Join(t.TempDir(), ".o-projects", "scratch-release-race")
	workspace, _, err := store.EnsureExecutionTaskWorkspace(ctx, ExecutionTaskWorkspace{UserID: owner, TaskID: "task_release_race", Workdir: workdir}, now)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.SetExecutionTaskWorkspaceState(ctx, owner, workspace.TaskID, "preparing", "ready", "not_configured", 0, 0, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, workspace.TaskID, "ready", "releasing", "not_configured", 0, 0, "", now); err != nil {
		t.Fatal(err)
	}
	retry := ExecutionTaskWorkspace{UserID: owner, TaskID: workspace.TaskID, Workdir: workdir}
	if _, _, err := store.EnsureExecutionTaskWorkspace(ctx, retry, now.Add(time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("execution retry reused a workspace while cleanup was releasing it: err=%v", err)
	}
	project := domain.Project{ID: "project_release_race", UserID: owner, Name: "Released workspace", Workdir: workdir, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, project); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("project acquired a workspace after cleanup began: err=%v", err)
	}
}

func TestExecutionTaskWorkspaceQuotaIDsAreUnique(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, _, err := store.EnsureExecutionTaskWorkspace(ctx, ExecutionTaskWorkspace{UserID: owner, TaskID: "quota_task_a", Workdir: filepath.Join(t.TempDir(), "a")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, first.TaskID, "preparing", "ready", "applied", 423001, 64<<20, "", now); err != nil {
		t.Fatal(err)
	}
	second, _, err := store.EnsureExecutionTaskWorkspace(ctx, ExecutionTaskWorkspace{UserID: owner, TaskID: "quota_task_b", Workdir: filepath.Join(t.TempDir(), "b")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, second.TaskID, "preparing", "ready", "applied", 423001, 64<<20, "", now); err == nil {
		t.Fatal("database accepted a project quota ID already allocated to another task")
	}
	readBack, err := store.ExecutionTaskWorkspace(ctx, owner, second.TaskID)
	if err != nil || readBack.Status != "preparing" || readBack.QuotaProjectID != 0 {
		t.Fatalf("quota ID uniqueness failure partially mutated the second workspace: %+v err=%v", readBack, err)
	}
}

func TestExecutionTaskWorkspaceQuotaAllocationPersistsAndRetriesIdempotently(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, taskID := range []string{"quota_reservation_a", "quota_reservation_b"} {
		if _, _, err := store.EnsureExecutionTaskWorkspace(ctx, ExecutionTaskWorkspace{UserID: owner, TaskID: taskID, Workdir: filepath.Join(t.TempDir(), taskID)}, now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ReserveExecutionTaskWorkspaceQuota(ctx, owner, "quota_reservation_a", 512<<20, now)
	if err != nil || first.QuotaProjectID < 0x80000000 || first.QuotaProjectID > int64(^uint32(0)) || first.QuotaLimitBytes != 512<<20 || first.QuotaState != "preparing" {
		t.Fatalf("quota allocation was not persisted: %+v err=%v", first, err)
	}
	retry, err := store.ReserveExecutionTaskWorkspaceQuota(ctx, owner, "quota_reservation_a", 512<<20, now.Add(time.Second))
	if err != nil || retry.QuotaProjectID != first.QuotaProjectID || retry.QuotaLimitBytes != first.QuotaLimitBytes {
		t.Fatalf("quota allocation retry was not idempotent: first=%+v retry=%+v err=%v", first, retry, err)
	}
	changedConfig, err := store.ReserveExecutionTaskWorkspaceQuota(ctx, owner, "quota_reservation_a", 1<<30, now.Add(2*time.Second))
	if err != nil || changedConfig.QuotaProjectID != first.QuotaProjectID || changedConfig.QuotaLimitBytes != first.QuotaLimitBytes {
		t.Fatalf("quota retry did not preserve the durable original allocation: %+v err=%v", changedConfig, err)
	}
	second, err := store.ReserveExecutionTaskWorkspaceQuota(ctx, owner, "quota_reservation_b", 512<<20, now)
	if err != nil || second.QuotaProjectID < 0x80000000 || second.QuotaProjectID > int64(^uint32(0)) || second.QuotaProjectID == first.QuotaProjectID {
		t.Fatalf("separate task received duplicate project quota ID: first=%+v second=%+v err=%v", first, second, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	afterRestart, err := restarted.ExecutionTaskWorkspace(ctx, owner, first.TaskID)
	if err != nil || afterRestart.QuotaProjectID != first.QuotaProjectID || afterRestart.QuotaLimitBytes != first.QuotaLimitBytes || afterRestart.QuotaState != "preparing" {
		t.Fatalf("quota assignment did not survive storage restart: %+v err=%v", afterRestart, err)
	}
}
