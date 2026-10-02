package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/storage"
)

func TestExecutionTaskLineageMigrationBackfillsLegacyRowsAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_lineage_legacy", UserID: owner, Name: "Legacy", Platform: "linux"}, "lineage-legacy-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "lineage-legacy-token", "linux", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	const taskID = "task_lineage_legacy"
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: taskID, UserID: owner, NodeID: "node_lineage_legacy", IdempotencyKey: "lineage-legacy", Payload: []byte(`{"kind":"agent_turn"}`)}, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(dataDir, "axiom.db")
	legacyDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacyDB.Exec(`DROP INDEX idx_execution_tasks_logical_task;
ALTER TABLE execution_tasks DROP COLUMN logical_task_id;
ALTER TABLE execution_tasks DROP COLUMN parent_task_id;
ALTER TABLE execution_tasks DROP COLUMN segment_index;`); err != nil {
		_ = legacyDB.Close()
		t.Fatalf("prepare legacy execution task schema: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatalf("migrate legacy execution task schema: %v", err)
	}
	persisted, err := store.ExecutionTask(ctx, owner, taskID)
	if err != nil || persisted.LogicalTaskID != taskID || persisted.ParentTaskID != "" || persisted.SegmentIndex != 0 {
		t.Fatalf("legacy task lineage did not backfill to its root identity: task=%+v err=%v", persisted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen migrated execution task schema: %v", err)
	}
	defer store.Close()
	persisted, err = store.ExecutionTask(ctx, owner, taskID)
	if err != nil || persisted.LogicalTaskID != taskID || persisted.SegmentIndex != 0 {
		t.Fatalf("migrated task lineage did not survive restart: task=%+v err=%v", persisted, err)
	}
}

func TestTaskContinuationConversationIsAtomicDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close task continuation storage: %v", err)
		}
	}()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, _, err := store.EnsureCloudExecutionNode(ctx, owner, now); err != nil {
		t.Fatal(err)
	}
	provider := domain.Provider{ID: "provider_task_continue", UserID: owner, Name: "Test", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "test", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "project_task_continue", UserID: owner, Name: "Imported", Workdir: filepath.Join(dataDir, "project"), RemoteRepoURL: "https://github.com/example/repo.git", RemoteBranch: "codex/task-task_continue", RepositoryProvider: "github", ResolvedCommit: "0123456789012345678901234567890123456789", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	source := domain.Conversation{ID: "conversation_task_continue_source", UserID: owner, Title: "Source chat", ProviderID: provider.ID, ProjectID: "project_source", PermissionProfile: domain.DefaultPermissionProfile(), CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, domain.Project{ID: source.ProjectID, UserID: owner, Name: "Source", Workdir: filepath.Join(dataDir, "source"), RemoteRepoURL: project.RemoteRepoURL, RemoteBranch: "main", RepositoryProvider: "github", ResolvedCommit: "1123456789012345678901234567890123456789", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConversationWithGeneration(ctx, source, generation); err != nil {
		t.Fatal(err)
	}
	sourceMessages := []domain.Message{
		{ID: "message_task_source_user", ConversationID: source.ID, Role: "user", Content: "Build the feature", CreatedAt: now},
		{ID: "message_task_source_assistant", ConversationID: source.ID, Role: "assistant", Content: "I will start locally", CreatedAt: now.Add(time.Second)},
	}
	for _, message := range sourceMessages {
		if err := store.AddMessage(ctx, owner, message); err != nil {
			t.Fatal(err)
		}
	}
	branch := domain.Conversation{ID: "conversation_task_continue_cloud", UserID: owner, Title: "Source chat · 云端续接", ProviderID: provider.ID, ProjectID: project.ID, PermissionProfile: domain.DefaultPermissionProfile(), ParentConversationID: source.ID, ExecutionPaused: true, CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(5 * time.Second)}
	transcript := []domain.Message{
		{Role: "user", Content: "Build the feature", CreatedAt: now},
		{Role: "assistant", Content: "I will start locally", CreatedAt: now.Add(time.Second)},
		{Role: "user", Content: "Continue this task in the cloud", CreatedAt: now.Add(3 * time.Second)},
		{Role: "assistant", Content: "The local work is ready", CreatedAt: now.Add(4 * time.Second)},
	}
	continued, err := store.CreateTaskContinuationConversation(ctx, branch, sourceMessages[1].ID, transcript)
	if err != nil {
		t.Fatal(err)
	}
	if continued.ID != branch.ID || continued.ParentConversationID != source.ID || continued.ProjectID != project.ID || continued.BranchFromMessageID != sourceMessages[1].ID || len(continued.Messages) != len(transcript) || continued.AgentGenerationID != generation.ID {
		t.Fatalf("task continuation did not read back the intended cloud branch: %+v", continued)
	}
	if !continued.ExecutionPaused {
		t.Fatal("continuation conversation became executable before its durable cloud task existed")
	}
	importedTurn := domain.AgentTurn{ID: "turn_imported_handoff", ConversationID: continued.ID, UserID: owner, InputMessageID: continued.Messages[len(continued.Messages)-2].ID, ProviderID: provider.ID, AgentGenerationID: continued.AgentGenerationID, AgentDefinitionDigest: continued.AgentDefinitionDigest, PermissionProfile: continued.PermissionProfile, Status: "incomplete", StopReason: "step_limit"}
	const checkpointHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	importedTurn, err = store.ImportAgentContinuationTurn(ctx, owner, importedTurn, continued.Messages[len(continued.Messages)-1].ID, checkpointHash, []byte("vault-cipher-v1"), []byte("vault-nonce-v1"), now.Add(5*time.Second))
	if err != nil || importedTurn.Status != "incomplete" || !importedTurn.ContinuationAvailable || importedTurn.ResultMessageID != continued.Messages[len(continued.Messages)-1].ID {
		t.Fatalf("imported continuation turn did not read back as resumable: %+v err=%v", importedTurn, err)
	}
	retryTurn := importedTurn
	retriedTurn, err := store.ImportAgentContinuationTurn(ctx, owner, retryTurn, importedTurn.ResultMessageID, checkpointHash, []byte("different-random-cipher"), []byte("different-random-nonce"), now.Add(6*time.Second))
	if err != nil || retriedTurn.ID != importedTurn.ID {
		t.Fatalf("imported continuation retry was not idempotent: %+v err=%v", retriedTurn, err)
	}
	snapshot, err := store.AgentContinuationSnapshot(ctx, owner, importedTurn.ID)
	if err != nil || snapshot.Status != "available" || snapshot.ContentHash != checkpointHash || string(snapshot.Ciphertext) != "vault-cipher-v1" {
		t.Fatalf("imported encrypted continuation snapshot not authoritative: %+v err=%v", snapshot, err)
	}
	const collidingTaskID = "task_handoff_activation_collision"
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: collidingTaskID, UserID: owner, NodeID: storage.CloudExecutionNodeID(owner), IdempotencyKey: "handoff-collision-seed", Payload: []byte(`{}`)}, false, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	cloudPayload := []byte(`{"kind":"agent_continuation","conversationId":"conversation_task_continue_cloud","turnId":"turn_imported_handoff","content":"Continue"}`)
	if _, _, err := store.CreateCloudHandoffTask(ctx, storage.ExecutionTask{ID: collidingTaskID, UserID: owner, NodeID: storage.CloudExecutionNodeID(owner), IdempotencyKey: "handoff-activation-failure", Payload: cloudPayload}, continued.ID, now.Add(8*time.Second)); err == nil {
		t.Fatal("duplicate task ID unexpectedly activated the prepared continuation")
	}
	if _, _, err := store.CreateCloudHandoffTask(ctx, storage.ExecutionTask{ID: "task_handoff_wrong_lineage", UserID: owner, LogicalTaskID: "another-logical-task", ParentTaskID: collidingTaskID, SegmentIndex: 1, NodeID: storage.CloudExecutionNodeID(owner), IdempotencyKey: "handoff-wrong-lineage", Payload: cloudPayload}, continued.ID, now.Add(8*time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("handoff task with a forged logical task lineage = %v, want conflict", err)
	}
	stillPaused, err := store.Conversation(ctx, owner, continued.ID)
	if err != nil || !stillPaused.ExecutionPaused {
		t.Fatalf("failed task creation did not roll back conversation activation: conversation=%+v err=%v", stillPaused, err)
	}
	activatedTask, created, err := store.CreateCloudHandoffTask(ctx, storage.ExecutionTask{ID: "task_handoff_activation", UserID: owner, LogicalTaskID: collidingTaskID, ParentTaskID: collidingTaskID, SegmentIndex: 1, NodeID: storage.CloudExecutionNodeID(owner), IdempotencyKey: "cloud-handoff:source-task", Payload: cloudPayload}, continued.ID, now.Add(9*time.Second))
	if err != nil || !created || activatedTask.Status != "queued" || activatedTask.LogicalTaskID != collidingTaskID || activatedTask.ParentTaskID != collidingTaskID || activatedTask.SegmentIndex != 1 {
		t.Fatalf("handoff task was not durably queued with conversation activation: task=%+v created=%t err=%v", activatedTask, created, err)
	}
	activatedConversation, err := store.Conversation(ctx, owner, continued.ID)
	if err != nil || activatedConversation.ExecutionPaused {
		t.Fatalf("durable queued handoff did not atomically activate its conversation: conversation=%+v err=%v", activatedConversation, err)
	}
	if retryTask, retryCreated, err := store.CreateCloudHandoffTask(ctx, storage.ExecutionTask{ID: "task_handoff_activation_retry", UserID: owner, NodeID: storage.CloudExecutionNodeID(owner), IdempotencyKey: "cloud-handoff:source-task", Payload: cloudPayload}, continued.ID, now.Add(10*time.Second)); err != nil || retryCreated || retryTask.ID != activatedTask.ID {
		t.Fatalf("handoff task retry was not idempotent: task=%+v created=%t err=%v", retryTask, retryCreated, err)
	}
	retry, err := store.CreateTaskContinuationConversation(ctx, branch, sourceMessages[1].ID, transcript)
	if err != nil || retry.ID != branch.ID || len(retry.Messages) != len(transcript) {
		t.Fatalf("task continuation retry was not idempotent: %+v err=%v", retry, err)
	}
	invalid := append([]domain.Message(nil), transcript...)
	invalid[len(invalid)-1].Content = "different answer"
	if _, err := store.CreateTaskContinuationConversation(ctx, branch, sourceMessages[1].ID, invalid); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting retry error = %v, want conflict", err)
	}
	failedBranch := branch
	failedBranch.ID = "conv_task_continue_rollback"
	if err := store.AddMessage(ctx, owner, domain.Message{ID: "msg_task_continue_rollback_task_000001", ConversationID: source.ID, Role: "user", Content: "force transaction rollback", CreatedAt: now.Add(6 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTaskContinuationConversation(ctx, failedBranch, sourceMessages[1].ID, transcript); err == nil {
		t.Fatal("duplicate message ID did not fail the continuation transaction")
	}
	if _, err := store.Conversation(ctx, owner, failedBranch.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed continuation left a partial conversation: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persisted, err := store.Conversation(ctx, owner, branch.ID)
	if err != nil || persisted.ProjectID != project.ID || persisted.ExecutionPaused || len(persisted.Messages) != len(transcript) || persisted.Messages[len(persisted.Messages)-1].Content != "The local work is ready" {
		t.Fatalf("task continuation did not survive storage restart: conversation=%+v err=%v", persisted, err)
	}
	persistedTask, err := store.ExecutionTaskByIdempotencyKey(ctx, owner, "cloud-handoff:source-task")
	if err != nil || persistedTask.ID != "task_handoff_activation" || persistedTask.Status != "queued" || persistedTask.LogicalTaskID != collidingTaskID || persistedTask.ParentTaskID != collidingTaskID || persistedTask.SegmentIndex != 1 {
		t.Fatalf("activated handoff task did not survive storage restart: task=%+v err=%v", persistedTask, err)
	}
	persistedTurn, err := store.AgentTurn(ctx, owner, importedTurn.ID)
	persistedSnapshot, snapshotErr := store.AgentContinuationSnapshot(ctx, owner, importedTurn.ID)
	if err != nil || snapshotErr != nil || persistedTurn.Status != "incomplete" || !persistedTurn.ContinuationAvailable || persistedSnapshot.Status != "available" || string(persistedSnapshot.Ciphertext) != "vault-cipher-v1" {
		t.Fatalf("imported continuation did not survive storage restart: turn=%+v snapshot=%+v err=%v snapshotErr=%v", persistedTurn, persistedSnapshot, err, snapshotErr)
	}
}
