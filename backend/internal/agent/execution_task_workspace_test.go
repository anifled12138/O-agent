package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/storage"
)

func TestScratchExecutionTaskWorkspaceIsolatedDurableAndRecoverable(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(".", ".execution-task-workspace-test-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	service := &Service{store: store, workspaceRoot: root}
	first, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_scratch_one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_scratch_two")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !pathWithin(root, first) || !pathWithin(root, second) {
		t.Fatalf("scratch workspaces are not isolated within the workspace root: %q %q", first, second)
	}
	if err := os.WriteFile(filepath.Join(first, "task.txt"), []byte("durable task output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = nil
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	service.store = store
	recovered, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_scratch_one")
	if err != nil || recovered != first {
		t.Fatalf("scratch workspace did not recover its durable mapping: path=%q want=%q err=%v", recovered, first, err)
	}
	content, err := os.ReadFile(filepath.Join(recovered, "task.txt"))
	if err != nil || string(content) != "durable task output" {
		t.Fatalf("scratch task output did not survive workspace recovery: %q err=%v", content, err)
	}
	binding, err := store.ExecutionTaskWorkspace(ctx, owner, "task_scratch_one")
	if err != nil || binding.Status != "ready" || binding.Workdir != first || binding.SourceProjectID != "" {
		t.Fatalf("recovered scratch workspace binding did not read back ready: %+v err=%v", binding, err)
	}
}

func TestExecutionTaskStagesVerifiedUserInputsIntoIsolatedWorkspace(t *testing.T) {
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
	node, _, err := store.EnsureCloudExecutionNode(ctx, owner, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("user supplied instructions are untrusted\n")
	digest := sha256.Sum256(content)
	artifacts, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := artifacts.StoreFromReader(ctx, owner, "instructions.txt", "text/plain", "task-input-stage-test", int64(len(content)), hex.EncodeToString(digest[:]), bytes.NewReader(content), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	const taskID = "task_stage_user_inputs"
	if _, _, err := store.CreateExecutionTaskWithArtifacts(ctx, storage.ExecutionTask{ID: taskID, UserID: owner, NodeID: node.ID, IdempotencyKey: "stage-user-inputs", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, []storage.ExecutionTaskArtifactLink{{ArtifactID: artifact.ID, Role: "user_input"}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	service := &Service{store: store, artifacts: artifacts, workspaceRoot: root}
	workdir, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, taskID)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := service.stageExecutionTaskInputs(ctx, owner, taskID, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, artifact.SHA256) || !strings.Contains(prompt, "用户提供") {
		t.Fatalf("task runtime prompt omitted the verified attachment binding: %q", prompt)
	}
	name := taskInputName(artifact.ID, artifact.FileName)
	path := filepath.Join(executionTaskInputDir(filepath.Join(workdir, ".o-task-inputs"), taskID), name)
	readBack, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(readBack, content) {
		t.Fatalf("verified input was not staged into the isolated task workspace: %q err=%v", readBack, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("staged user input is not marked read-only: mode=%v err=%v", info.Mode(), err)
	}
	if _, err := service.stageExecutionTaskInputs(ctx, owner, taskID, workdir); err != nil {
		t.Fatalf("idempotent staging did not read back the existing verified file: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.stageExecutionTaskInputs(ctx, owner, taskID, workdir); err == nil {
		t.Fatal("tampered staged user input was accepted on task retry")
	}
}

func TestScratchExecutionTaskWorkspaceFailurePersistsDegradedState(t *testing.T) {
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
	root, err := os.MkdirTemp(".", ".execution-task-workspace-failure-test-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	// A regular file at the reserved parent path must not be followed or treated
	// as a usable task workspace directory.
	if err := os.WriteFile(filepath.Join(root, ".o-projects"), []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store, workspaceRoot: root}
	if _, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_scratch_blocked"); err == nil {
		t.Fatal("invalid workspace parent was reported as usable")
	}
	binding, err := store.ExecutionTaskWorkspace(ctx, owner, "task_scratch_blocked")
	if err != nil || binding.Status != "degraded" || binding.QuotaState != "degraded" || binding.Error != "scratch_parent_unavailable" {
		t.Fatalf("failed workspace setup was not durably marked degraded: %+v err=%v", binding, err)
	}
}

func TestSubmoduleFailureDegradesAndPersistsExistingExecutionWorkspace(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const taskID = "task_submodule_degraded"
	workspacePath := filepath.Join(t.TempDir(), "workspace")
	if _, _, err := store.EnsureExecutionTaskWorkspace(ctx, storage.ExecutionTaskWorkspace{
		UserID: owner, TaskID: taskID, SourceProjectID: "source_project", Workdir: workspacePath,
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetExecutionTaskWorkspaceState(ctx, owner, taskID, "preparing", "ready", "applied", 23, 64<<20, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store}
	if err := service.markExecutionTaskWorkspaceDegraded(ctx, owner, taskID, "task_submodule_unavailable", errors.New("submodule fetch failed")); err == nil {
		t.Fatal("submodule failure was swallowed")
	}
	readBack, err := store.ExecutionTaskWorkspace(ctx, owner, taskID)
	if err != nil || readBack.Status != "degraded" || readBack.QuotaState != "applied" || readBack.QuotaProjectID != 23 || readBack.QuotaLimitBytes != 64<<20 || readBack.Error != "task_submodule_unavailable" {
		t.Fatalf("degraded workspace did not preserve verified quota and reason: %+v err=%v", readBack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = nil
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	service.store = store
	restarted, err := store.ExecutionTaskWorkspace(ctx, owner, taskID)
	if err != nil || restarted.Status != "degraded" || restarted.Error != "task_submodule_unavailable" || restarted.QuotaState != "applied" {
		t.Fatalf("submodule failure state did not survive restart: %+v err=%v", restarted, err)
	}
}

func TestProjectlessCloudTaskFinalizesWorkspaceOutputAsDurableArtifact(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cloudNode, _, err := store.EnsureCloudExecutionNode(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_workspace_output", UserID: owner, NodeID: cloudNode.ID, IdempotencyKey: "workspace-output-test", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	provider := domain.Provider{ID: "provider_workspace_output", UserID: owner, Name: "workspace output", Kind: "openai-compatible", BaseURL: "https://example.invalid/v1", Model: "test"}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "conv_workspace_output", UserID: owner, Title: "workspace output", ProviderID: provider.ID, PermissionProfile: domain.DefaultPermissionProfile(), CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := os.MkdirTemp(".", ".task-workspace-output-root-")
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err = filepath.Abs(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workspaceRoot)
	service := &Service{store: store, workspaceRoot: workspaceRoot}
	workdir, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, "task_workspace_output")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "result.txt"), []byte("persist this task result\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archiveService, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	service.SetArtifactStore(archiveService)
	project, artifact, base, commit, err := service.FinalizeExecutionTaskWorkspace(ctx, owner, "task_workspace_output", conversation.ID)
	if err != nil || project.ID != "" || artifact == nil || artifact.FileName != "task-workspace.tar.gz" || base != "" || commit != "" {
		t.Fatalf("projectless workspace finalization = project=%+v artifact=%+v base=%q commit=%q err=%v", project, artifact, base, commit, err)
	}
	items, err := store.ExecutionTaskArtifacts(ctx, owner, "task_workspace_output")
	if err != nil || len(items) != 1 || items[0].ID != artifact.ID || items[0].Role != "workspace_output" || items[0].SHA256 != artifact.SHA256 {
		t.Fatalf("workspace output association did not read back: items=%+v err=%v", items, err)
	}
	_, file, err := archiveService.Open(ctx, owner, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	reader := tar.NewReader(compressed)
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "workspace/result.txt" && string(content) == "persist this task result\n" {
			found = true
		}
	}
	if err := errors.Join(file.Close(), compressed.Close()); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("workspace output artifact did not contain the task result")
	}
	firstID := artifact.ID
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	archiveService, err = artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	service.store = store
	service.SetArtifactStore(archiveService)
	_, recovered, _, _, err := service.FinalizeExecutionTaskWorkspace(ctx, owner, "task_workspace_output", conversation.ID)
	if err != nil || recovered == nil || recovered.ID != firstID {
		t.Fatalf("workspace output did not recover idempotently after restart: artifact=%+v err=%v", recovered, err)
	}
}
