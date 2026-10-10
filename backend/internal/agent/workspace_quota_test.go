package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"axiom.local/agent/internal/projectquota"
	"axiom.local/agent/internal/storage"
)

type fakeWorkspaceQuotaManager struct {
	mu          sync.Mutex
	quotas      map[uint32]projectquota.WorkspaceQuota
	failApply   bool
	failRelease bool
}

func (manager *fakeWorkspaceQuotaManager) Health(context.Context) (projectquota.ProbeResult, error) {
	return projectquota.ProbeResult{Filesystem: "ext4", MountPoint: "/workspace", MountSupported: true, QuotaOptionFound: true}, nil
}

func (manager *fakeWorkspaceQuotaManager) Apply(_ context.Context, _ string, projectID uint32, limit int64) (projectquota.WorkspaceQuota, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.failApply {
		return projectquota.WorkspaceQuota{}, errors.New("injected quota apply failure")
	}
	quota := projectquota.WorkspaceQuota{Filesystem: "ext4", MountPoint: "/workspace", ProjectID: projectID, LimitBytes: limit, Applied: true}
	manager.quotas[projectID] = quota
	return quota, nil
}

func (manager *fakeWorkspaceQuotaManager) Inspect(_ context.Context, _ string, projectID uint32) (projectquota.WorkspaceQuota, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if quota, ok := manager.quotas[projectID]; ok {
		return quota, nil
	}
	return projectquota.WorkspaceQuota{Filesystem: "ext4", MountPoint: "/workspace", ProjectID: projectID}, nil
}

func (manager *fakeWorkspaceQuotaManager) Release(_ context.Context, _ string, projectID uint32) (projectquota.WorkspaceQuota, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.failRelease {
		return projectquota.WorkspaceQuota{}, errors.New("injected quota release failure")
	}
	delete(manager.quotas, projectID)
	return projectquota.WorkspaceQuota{Filesystem: "ext4", MountPoint: "/workspace", ProjectID: projectID}, nil
}

func TestScratchTaskWorkspaceRequiresAndReadsBackQuotaAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	quota := &fakeWorkspaceQuotaManager{quotas: make(map[uint32]projectquota.WorkspaceQuota)}
	service := &Service{store: store, workspaceRoot: workspaceRoot}
	if err := service.ConfigureWorkspaceQuota(quota, 128<<20); err != nil {
		t.Fatal(err)
	}
	workdir, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_quota_restart")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "result.txt"), []byte("task output"), 0o600); err != nil {
		t.Fatal(err)
	}
	binding, err := store.ExecutionTaskWorkspace(ctx, owner, "task_quota_restart")
	if err != nil || binding.Status != "ready" || binding.QuotaState != "applied" || binding.QuotaProjectID <= 0 || binding.QuotaLimitBytes != 128<<20 {
		t.Fatalf("task quota/workspace did not read back as applied: %+v err=%v", binding, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedService := &Service{store: restarted, workspaceRoot: workspaceRoot}
	if err := restartedService.ConfigureWorkspaceQuota(quota, 256<<20); err != nil {
		t.Fatal(err)
	}
	recoveredWorkdir, err := restartedService.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_quota_restart")
	if err != nil || recoveredWorkdir != workdir {
		t.Fatalf("restart did not recover the same quota-bound workspace: %q err=%v", recoveredWorkdir, err)
	}
	readBack, err := restarted.ExecutionTaskWorkspace(ctx, owner, "task_quota_restart")
	if err != nil || readBack.QuotaProjectID != binding.QuotaProjectID || readBack.QuotaLimitBytes != 128<<20 || readBack.QuotaState != "applied" {
		t.Fatalf("restart changed the immutable task quota allocation: %+v err=%v", readBack, err)
	}
}

func TestScratchQuotaFailurePersistsDegradedStateAndRetries(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	quota := &fakeWorkspaceQuotaManager{quotas: make(map[uint32]projectquota.WorkspaceQuota), failApply: true}
	service := &Service{store: store, workspaceRoot: filepath.Join(t.TempDir(), "workspaces")}
	if err := service.ConfigureWorkspaceQuota(quota, 64<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_quota_failure"); err == nil {
		t.Fatal("workspace was returned even though kernel quota application failed")
	}
	binding, err := store.ExecutionTaskWorkspace(ctx, owner, "task_quota_failure")
	if err != nil || binding.Status != "degraded" || binding.QuotaState != "degraded" || binding.QuotaProjectID <= 0 || binding.QuotaLimitBytes != 64<<20 {
		t.Fatalf("quota failure did not preserve a durable degraded allocation: %+v err=%v", binding, err)
	}
	quota.mu.Lock()
	quota.failApply = false
	quota.mu.Unlock()
	workdir, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_quota_failure")
	if err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ExecutionTaskWorkspace(ctx, owner, "task_quota_failure")
	if err != nil || readBack.Status != "ready" || readBack.QuotaState != "applied" || readBack.Workdir != workdir {
		t.Fatalf("recovered task workspace did not read back ready with its hard quota: %+v err=%v", readBack, err)
	}
}

func TestStartupQuotaReconciliationPersistsKernelMismatchWithoutDeletingWorkspace(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeWorkspaceQuotaManager{quotas: make(map[uint32]projectquota.WorkspaceQuota)}
	service := &Service{store: store, workspaceRoot: filepath.Join(t.TempDir(), "workspaces")}
	if err := service.ConfigureWorkspaceQuota(manager, 96<<20); err != nil {
		t.Fatal(err)
	}
	workdir, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_quota_reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "preserve.txt"), []byte("durable task data"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := store.ExecutionTaskWorkspace(ctx, owner, "task_quota_reconcile")
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	delete(manager.quotas, uint32(before.QuotaProjectID))
	manager.mu.Unlock()
	degraded, err := service.ReconcileWorkspaceQuotas(ctx)
	if err != nil || degraded != 1 {
		t.Fatalf("startup quota reconciliation result = %d, %v", degraded, err)
	}
	after, err := store.ExecutionTaskWorkspace(ctx, owner, before.TaskID)
	if err != nil || after.Status != "degraded" || after.QuotaState != "degraded" || after.QuotaProjectID != before.QuotaProjectID || after.QuotaLimitBytes != before.QuotaLimitBytes || after.Error == "" {
		t.Fatalf("missing kernel quota was not persisted as degraded: %+v err=%v", after, err)
	}
	if payload, readErr := os.ReadFile(filepath.Join(workdir, "preserve.txt")); readErr != nil || string(payload) != "durable task data" {
		t.Fatalf("reconciliation removed or changed task data: %q err=%v", payload, readErr)
	}
	if err := os.Remove(filepath.Join(workdir, "preserve.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, before.TaskID); err != nil {
		t.Fatalf("task did not recover with the same quota ID after helper became healthy: %v", err)
	}
	recovered, err := store.ExecutionTaskWorkspace(ctx, owner, before.TaskID)
	if err != nil || recovered.Status != "ready" || recovered.QuotaState != "applied" || recovered.QuotaProjectID != before.QuotaProjectID {
		t.Fatalf("task quota reconciliation retry did not preserve and apply the original allocation: %+v err=%v", recovered, err)
	}
}

func TestStartupQuotaReconciliationRecoversInterruptedLifecycleStates(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	manager := &fakeWorkspaceQuotaManager{quotas: make(map[uint32]projectquota.WorkspaceQuota)}
	service := &Service{store: store, workspaceRoot: workspaceRoot}
	if err := service.ConfigureWorkspaceQuota(manager, 80<<20); err != nil {
		t.Fatal(err)
	}
	reserve := func(taskID string) storage.ExecutionTaskWorkspace {
		t.Helper()
		workdir := filepath.Join(workspaceRoot, ".o-projects", "scratch-"+taskID)
		if err := os.MkdirAll(workdir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.EnsureExecutionTaskWorkspace(ctx, storage.ExecutionTaskWorkspace{UserID: owner, TaskID: taskID, Workdir: workdir}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		binding, err := store.ReserveExecutionTaskWorkspaceQuota(ctx, owner, taskID, 80<<20, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		return binding
	}
	applyFakeQuota := func(binding storage.ExecutionTaskWorkspace) {
		t.Helper()
		manager.quotas[uint32(binding.QuotaProjectID)] = projectquota.WorkspaceQuota{Filesystem: "ext4", MountPoint: "/workspace", ProjectID: uint32(binding.QuotaProjectID), LimitBytes: binding.QuotaLimitBytes, Applied: true}
	}

	preparing := reserve("task_interrupted_preparing")
	applyFakeQuota(preparing)
	degraded := reserve("task_interrupted_degraded")
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, degraded.TaskID, "preparing", "degraded", "degraded", degraded.QuotaProjectID, degraded.QuotaLimitBytes, "prior_failure", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	applyFakeQuota(degraded)
	if err := os.WriteFile(filepath.Join(degraded.Workdir, "preserve.txt"), []byte("keep this recovery evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	released := reserve("task_interrupted_released")
	applyFakeQuota(released)
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, released.TaskID, "preparing", "ready", "applied", released.QuotaProjectID, released.QuotaLimitBytes, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, released.TaskID, "ready", "released", "released", released.QuotaProjectID, released.QuotaLimitBytes, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	count, err := service.ReconcileWorkspaceQuotas(ctx)
	if err != nil || count != 2 {
		t.Fatalf("lifecycle reconciliation = %d, %v; want two preserved degraded rows", count, err)
	}
	preparingRead, err := store.ExecutionTaskWorkspace(ctx, owner, preparing.TaskID)
	if err != nil || preparingRead.Status != "degraded" || preparingRead.QuotaState != "applied" || preparingRead.QuotaProjectID != preparing.QuotaProjectID || preparingRead.Error != "startup_interrupted_workspace_preparation" {
		t.Fatalf("interrupted preparing state was not quarantined with observed quota: %+v err=%v", preparingRead, err)
	}
	degradedRead, err := store.ExecutionTaskWorkspace(ctx, owner, degraded.TaskID)
	if err != nil || degradedRead.Status != "degraded" || degradedRead.QuotaState != "applied" || degradedRead.Error != "prior_failure" {
		t.Fatalf("degraded row did not preserve its cause and kernel read-back: %+v err=%v", degradedRead, err)
	}
	if payload, readErr := os.ReadFile(filepath.Join(degraded.Workdir, "preserve.txt")); readErr != nil || string(payload) != "keep this recovery evidence" {
		t.Fatalf("reconciliation changed recovery data: %q err=%v", payload, readErr)
	}
	releasedRead, err := store.ExecutionTaskWorkspace(ctx, owner, released.TaskID)
	if err != nil || releasedRead.Status != "released" || releasedRead.QuotaState != "released" {
		t.Fatalf("released quota did not reconcile to terminal read-back: %+v err=%v", releasedRead, err)
	}
	manager.mu.Lock()
	_, quotaStillPresent := manager.quotas[uint32(released.QuotaProjectID)]
	manager.mu.Unlock()
	if quotaStillPresent {
		t.Fatal("released workspace retained a kernel project quota after reconciliation")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for taskID, wantStatus := range map[string]string{
		preparing.TaskID: "degraded",
		degraded.TaskID:  "degraded",
		released.TaskID:  "released",
	} {
		readBack, readErr := store.ExecutionTaskWorkspace(ctx, owner, taskID)
		if readErr != nil || readBack.Status != wantStatus {
			t.Fatalf("reconciled lifecycle state %s did not survive restart: %+v err=%v", taskID, readBack, readErr)
		}
	}
}

func TestStartupQuotaReconciliationDegradesReleasedRowWhenQuotaCannotBeCleared(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	workdir := filepath.Join(workspaceRoot, ".o-projects", "scratch-release-failure")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "preserve.txt"), []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	binding, _, err := store.EnsureExecutionTaskWorkspace(ctx, storage.ExecutionTaskWorkspace{UserID: owner, TaskID: "task_release_failure", Workdir: workdir}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.ReserveExecutionTaskWorkspaceQuota(ctx, owner, binding.TaskID, 80<<20, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, binding.TaskID, "preparing", "ready", "applied", binding.QuotaProjectID, binding.QuotaLimitBytes, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, binding.TaskID, "ready", "released", "released", binding.QuotaProjectID, binding.QuotaLimitBytes, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	manager := &fakeWorkspaceQuotaManager{quotas: map[uint32]projectquota.WorkspaceQuota{uint32(binding.QuotaProjectID): {Filesystem: "ext4", MountPoint: "/workspace", ProjectID: uint32(binding.QuotaProjectID), LimitBytes: binding.QuotaLimitBytes, Applied: true}}, failRelease: true}
	service := &Service{store: store, workspaceRoot: workspaceRoot}
	if err := service.ConfigureWorkspaceQuota(manager, 80<<20); err != nil {
		t.Fatal(err)
	}
	count, err := service.ReconcileWorkspaceQuotas(ctx)
	if err != nil || count != 1 {
		t.Fatalf("failed released quota recovery = %d, %v", count, err)
	}
	readBack, err := store.ExecutionTaskWorkspace(ctx, owner, binding.TaskID)
	if err != nil || readBack.Status != "degraded" || readBack.QuotaState != "degraded" || readBack.QuotaProjectID != binding.QuotaProjectID || readBack.QuotaLimitBytes != binding.QuotaLimitBytes || readBack.Error == "" {
		t.Fatalf("failed quota release did not become an explicit recoverable state: %+v err=%v", readBack, err)
	}
	if payload, readErr := os.ReadFile(filepath.Join(workdir, "preserve.txt")); readErr != nil || string(payload) != "do not delete" {
		t.Fatalf("failed quota release changed workspace evidence: %q err=%v", payload, readErr)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := store.ExecutionTaskWorkspace(ctx, owner, binding.TaskID)
	if err != nil || restarted.Status != "degraded" || restarted.QuotaState != "degraded" || restarted.QuotaProjectID != binding.QuotaProjectID || restarted.Error == "" {
		t.Fatalf("failed release recovery state did not survive restart: %+v err=%v", restarted, err)
	}
}

func TestDisablingQuotaDoesNotMisrepresentExistingKernelAllocation(t *testing.T) {
	service := &Service{}
	binding := storage.ExecutionTaskWorkspace{QuotaProjectID: 2147483648, QuotaLimitBytes: 8 << 30, QuotaState: "applied"}
	if err := service.verifyExecutionTaskQuota(context.Background(), binding); err == nil {
		t.Fatal("existing allocated workspace was treated as unmetered without its helper")
	}
	if err := service.verifyExecutionTaskQuota(context.Background(), storage.ExecutionTaskWorkspace{QuotaState: "not_configured"}); err != nil {
		t.Fatalf("new unmetered workspace was blocked: %v", err)
	}
}
