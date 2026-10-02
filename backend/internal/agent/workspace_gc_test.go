package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/projectquota"
	"axiom.local/agent/internal/storage"
)

func TestCompletedScratchWorkspaceGCVerifiesDurableOutputBeforeRelease(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-45 * 24 * time.Hour)
	node, _, err := store.EnsureCloudExecutionNode(ctx, owner, base)
	if err != nil {
		t.Fatal(err)
	}
	provider := domain.Provider{ID: "provider_workspace_gc", UserID: owner, Name: "workspace gc", Kind: "openai-compatible", BaseURL: "https://example.invalid/v1", Model: "test"}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "conversation_workspace_gc", UserID: owner, Title: "workspace gc", ProviderID: provider.ID, CreatedAt: base, UpdatedAt: base}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"kind": "agent_turn", "conversationId": conversation.ID, "content": "write a result"})
	const taskID = "task_workspace_gc"
	task, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: taskID, UserID: owner, NodeID: node.ID, IdempotencyKey: "workspace-gc", Payload: payload}, true, base)
	if err != nil {
		t.Fatal(err)
	}
	leaseHash := "durable-test-lease-hash"
	if _, err := store.ClaimExecutionTask(ctx, node.ID, leaseHash, base.Add(time.Minute), base); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, task.ID, leaseHash, "accepted", nil, "", base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, task.ID, leaseHash, "running", nil, "", base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	turn := domain.AgentTurn{ID: "turn_workspace_gc", ConversationID: conversation.ID, ProviderID: provider.ID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: base, UpdatedAt: base}
	input := domain.Message{ID: "input_workspace_gc", ConversationID: conversation.ID, Role: "user", Content: "write a result", CreatedAt: base}
	if err := store.StartAgentTurn(ctx, owner, turn, input, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "output_workspace_gc", ConversationID: conversation.ID, Role: "assistant", Content: "done", CreatedAt: base.Add(3 * time.Second)}
	if err := store.FinishAgentTurn(ctx, owner, turn.ID, "completed", "assistant_response", &output, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	quota := &fakeWorkspaceQuotaManager{quotas: make(map[uint32]projectquota.WorkspaceQuota)}
	service := &Service{store: store, workspaceRoot: workspaceRoot}
	if err := service.ConfigureWorkspaceQuota(quota, 1<<30); err != nil {
		t.Fatal(err)
	}
	workdir, err := service.EnsureExecutionTaskScratchWorkspace(ctx, owner, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "result.txt"), []byte("durable result\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	service.SetArtifactStore(artifacts)
	_, artifact, _, _, err := service.FinalizeExecutionTaskWorkspace(ctx, owner, taskID, conversation.ID)
	if err != nil || artifact == nil {
		t.Fatalf("finalize durable task result: artifact=%+v err=%v", artifact, err)
	}
	result, _ := json.Marshal(map[string]any{
		"agentTurnId":             turn.ID,
		"agentStatus":             "completed",
		"workspaceOutputArtifact": map[string]any{"id": artifact.ID, "sha256": artifact.SHA256, "byteSize": artifact.ByteSize},
	})
	completed, err := store.CompleteAgentExecutionTask(ctx, node.ID, taskID, leaseHash, turn.ID, result, base.Add(4*time.Second))
	if err != nil || completed.Status != "completed" || completed.LeaseUntil != nil {
		t.Fatalf("durably complete source task: %+v err=%v", completed, err)
	}

	reapTime := time.Now().UTC().Add(60 * 24 * time.Hour)
	quota.mu.Lock()
	quota.failRelease = true
	quota.mu.Unlock()
	removed, err := service.ReapCompletedScratchWorkspaces(ctx, reapTime, 30*24*time.Hour)
	if err == nil || removed != 0 {
		t.Fatalf("failed quota release should leave an explicit recoverable cleanup state: %d, %v", removed, err)
	}
	if _, err := os.Lstat(workdir); !os.IsNotExist(err) {
		t.Fatalf("verified output scratch directory remains after cleanup was attempted: %v", err)
	}
	workspace, err := store.ExecutionTaskWorkspace(ctx, owner, taskID)
	if err != nil || workspace.Status != "degraded" || workspace.QuotaState != "degraded" {
		t.Fatalf("failed quota release did not persist recoverable state: %+v err=%v", workspace, err)
	}
	quota.mu.Lock()
	quota.failRelease = false
	quota.mu.Unlock()
	removed, err = service.ReapCompletedScratchWorkspaces(ctx, reapTime.Add(time.Hour), 0)
	if err != nil || removed != 1 {
		t.Fatalf("resumed scratch cleanup = %d, %v", removed, err)
	}
	workspace, err = store.ExecutionTaskWorkspace(ctx, owner, taskID)
	if err != nil || workspace.Status != "released" || workspace.QuotaState != "released" {
		t.Fatalf("workspace release did not persist/read back after retry: %+v err=%v", workspace, err)
	}
	readBackArtifact, file, err := artifacts.Open(ctx, owner, artifact.ID)
	if err != nil {
		t.Fatalf("durable output artifact was lost with scratch directory: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if readBackArtifact.SHA256 != artifact.SHA256 || readBackArtifact.ByteSize != artifact.ByteSize {
		t.Fatalf("durable output artifact changed after cleanup: %+v", readBackArtifact)
	}
}
