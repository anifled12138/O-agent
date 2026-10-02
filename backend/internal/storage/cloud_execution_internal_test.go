package storage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/storage"
)

func TestCloudTaskCompletionRequiresAndReadsBackCompletedAgentTurn(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	rollbackDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(root, "axiom.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertProvider(ctx, domain.Provider{ID: "prv_cloud_task", UserID: owner, Name: "Cloud", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake"}, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := domain.Conversation{ID: "run_cloud_task", UserID: owner, Title: "Cloud task", ProviderID: "prv_cloud_task", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	node, credentialHash, err := store.EnsureCloudExecutionNode(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	secondNode, secondHash, err := store.EnsureCloudExecutionNode(ctx, owner, now.Add(time.Second))
	if err != nil || secondNode.ID != node.ID || secondHash != credentialHash {
		t.Fatalf("cloud node ensure was not idempotent: node=%+v hash-match=%v err=%v", secondNode, secondHash == credentialHash, err)
	}
	payload, _ := json.Marshal(map[string]string{"kind": "agent_turn", "conversationId": conversation.ID, "content": "complete the task"})
	task, created, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_cloud_verified", UserID: owner, NodeID: node.ID, IdempotencyKey: "cloud-verified-1", Payload: payload}, false, now.Add(time.Second))
	if err != nil || !created {
		t.Fatalf("create task: task=%+v created=%v err=%v", task, created, err)
	}
	leaseHash := "cloud-task-lease-hash"
	if _, err := store.ClaimExecutionTask(ctx, node.ID, leaseHash, now.Add(61*time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, task.ID, leaseHash, "accepted", nil, "", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, task.ID, leaseHash, "running", nil, "", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	input := domain.Message{ID: "msg_cloud_verified_input", ConversationID: conversation.ID, Role: "user", Content: "complete the task", CreatedAt: now.Add(4 * time.Second)}
	turn := domain.AgentTurn{ID: "turn_cloud_verified", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now.Add(4 * time.Second), UpdatedAt: now.Add(4 * time.Second)}
	if err := store.StartAgentTurn(ctx, owner, turn, input, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "msg_cloud_verified_output", ConversationID: conversation.ID, Role: "assistant", Content: "Task finished", CreatedAt: now.Add(5 * time.Second)}
	if err := store.FinishAgentTurn(ctx, owner, turn.ID, "completed", "assistant_response", &output, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := rollbackDB.ExecContext(ctx, `CREATE TRIGGER reject_verified_task BEFORE UPDATE OF status ON execution_tasks WHEN NEW.id='task_cloud_verified' AND NEW.status='completed' BEGIN SELECT RAISE(ABORT,'forced task completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"agentTurnId":"turn_cloud_verified","status":"completed"}`)
	if _, err := store.CompleteAgentExecutionTask(ctx, node.ID, task.ID, leaseHash, turn.ID, result, now.Add(6*time.Second)); err == nil {
		t.Fatal("cloud task completed despite an injected transaction failure")
	}
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || readBack.Status != "running" || len(readBack.Result) != 0 || readBack.LeaseUntil == nil {
		t.Fatalf("failed finalization left a partial task state: task=%+v err=%v", readBack, err)
	}
	if _, err := rollbackDB.ExecContext(ctx, `DROP TRIGGER reject_verified_task`); err != nil {
		t.Fatal(err)
	}
	if err := rollbackDB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAgentExecutionTask(ctx, node.ID, task.ID, leaseHash, "missing-turn", result, now.Add(7*time.Second)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("completion without an authoritative turn = %v", err)
	}
	completed, err := store.CompleteAgentExecutionTask(ctx, node.ID, task.ID, leaseHash, turn.ID, result, now.Add(8*time.Second))
	if err != nil || completed.Status != "completed" || string(completed.Result) != string(result) || completed.LeaseUntil != nil {
		t.Fatalf("verified task completion failed: task=%+v err=%v", completed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	completed, err = store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || completed.Status != "completed" || string(completed.Result) != string(result) {
		t.Fatalf("verified completion was not durable across restart: task=%+v err=%v", completed, err)
	}
}

func TestSafeIncompleteCloudTaskRollsBackAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, owner, conversation, generation := runJournalFixtureAt(t, root, "cloud_task_incomplete")
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	now := time.Now().UTC().Truncate(time.Microsecond)
	node, _, err := store.EnsureCloudExecutionNode(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{"kind": "agent_turn", "conversationId": conversation.ID, "content": "continue the work"})
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_cloud_incomplete", UserID: owner, NodeID: node.ID, IdempotencyKey: "cloud-incomplete", Payload: payload}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	leaseHash := "cloud-incomplete-lease"
	if _, err := store.ClaimExecutionTask(ctx, node.ID, leaseHash, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, task.ID, leaseHash, "accepted", nil, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, task.ID, leaseHash, "running", nil, "", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	turn := domain.AgentTurn{ID: "turn_cloud_incomplete", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second)}
	input := domain.Message{ID: "msg_cloud_incomplete_input", ConversationID: conversation.ID, Role: "user", Content: "continue the work", CreatedAt: now.Add(3 * time.Second)}
	if err := store.StartAgentTurn(ctx, owner, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "msg_cloud_incomplete_output", ConversationID: conversation.ID, Role: "assistant", Content: "Paused at the budget boundary.", CreatedAt: now.Add(4 * time.Second)}
	checkpoint := &storage.ContinuationState{Version: 1, Ciphertext: []byte("encrypted-checkpoint"), Nonce: []byte("nonce"), ContentHash: "checkpoint-hash"}
	if err := store.FinishAgentTurnWithContinuation(ctx, owner, turn.ID, "incomplete", "step_limit", &output, json.RawMessage(`{}`), checkpoint); err != nil {
		t.Fatal(err)
	}

	db := openRunJournalDB(t, root)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_incomplete_task BEFORE INSERT ON execution_task_events WHEN NEW.task_id='task_cloud_incomplete' AND NEW.kind='agent_turn.incomplete' BEGIN SELECT RAISE(ABORT,'forced task journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PauseAgentExecutionTask(ctx, node.ID, task.ID, leaseHash, turn.ID, now.Add(5*time.Second)); err == nil {
		t.Fatal("safe pause reported success after the task journal update failed")
	}
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || readBack.Status != "running" || readBack.LeaseUntil == nil || len(readBack.Result) != 0 {
		t.Fatalf("failed safe-pause transaction left partial task state: task=%+v err=%v", readBack, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER reject_incomplete_task`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	paused, err := store.PauseAgentExecutionTask(ctx, node.ID, task.ID, leaseHash, turn.ID, now.Add(6*time.Second))
	if err != nil || paused.Status != "incomplete" || paused.LeaseUntil != nil || paused.Error != "" {
		t.Fatalf("safe budget pause did not persist as a non-error terminal state: task=%+v err=%v", paused, err)
	}
	var result map[string]string
	if err := json.Unmarshal(paused.Result, &result); err != nil || result["agentStatus"] != "incomplete" || result["agentStopReason"] != "step_limit" || result["continuationStatus"] != "available" || result["resultMessageId"] != output.ID {
		t.Fatalf("safe pause lost its authoritative continuation evidence: result=%s err=%v", paused.Result, err)
	}
	events, err := store.ExecutionTaskEvents(ctx, owner, task.ID)
	if err != nil || len(events) != 5 || events[len(events)-1].Kind != "agent_turn.incomplete" {
		t.Fatalf("safe pause event was not committed: events=%+v err=%v", events, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = nil
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	readBack, err = store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || readBack.Status != "incomplete" || string(readBack.Result) != string(paused.Result) || readBack.LeaseUntil != nil {
		t.Fatalf("safe non-complete state did not survive database restart: task=%+v err=%v", readBack, err)
	}
	unchanged, err := store.CancelExecutionTask(ctx, owner, task.ID, time.Now().UTC())
	if err != nil || unchanged.Status != "incomplete" || unchanged.CancelRequested || unchanged.Sequence != readBack.Sequence {
		t.Fatalf("terminal safe pause changed under a cancellation request: task=%+v err=%v", unchanged, err)
	}
}
