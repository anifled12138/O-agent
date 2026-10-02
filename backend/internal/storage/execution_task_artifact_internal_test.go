package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func TestExecutionTaskArtifactAssociationIsAtomicAndDurable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.RegisterExecutionNode(ctx, ExecutionNode{ID: "node_artifact_test", UserID: owner, Name: "node", Platform: "linux"}, "node-artifact-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "node-artifact-token", "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	task, _, err := store.CreateExecutionTask(ctx, ExecutionTask{ID: "task_artifact_test", UserID: owner, NodeID: "node_artifact_test", IdempotencyKey: "artifact-task-key", Payload: []byte(`{"kind":"agent_turn"}`)}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	upload, _, err := store.CreateArtifactUpload(ctx, ArtifactUpload{ID: "upl_artifact_test", UserID: owner, IdempotencyKey: "artifact-upload-key", FileName: "empty.txt", MediaType: "text/plain", ExpectedSize: 0, ChunkSize: 8 << 20, ChunkCount: 0}, now)
	if err != nil {
		t.Fatal(err)
	}
	artifact, _, err := store.FinalizeArtifactUpload(ctx, owner, upload.ID, "art_task_result", digest, "sha256/e3/"+digest, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_task_artifact BEFORE INSERT ON execution_task_artifacts BEGIN SELECT RAISE(ABORT,'forced task artifact failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskArtifact(ctx, owner, task.ID, artifact.ID, "output", now); err == nil {
		t.Fatal("association unexpectedly committed despite injected transaction failure")
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_task_artifacts WHERE task_id=?`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed association left partial state: count=%d err=%v", count, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_task_artifact`); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskArtifact(ctx, owner, task.ID, artifact.ID, "output", now); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskArtifact(ctx, owner, task.ID, artifact.ID, "output", now.Add(time.Second)); err != nil {
		t.Fatalf("idempotent association retry failed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	items, err := store.ExecutionTaskArtifacts(ctx, owner, task.ID)
	if err != nil || len(items) != 1 || items[0].ID != artifact.ID || items[0].Role != "output" {
		t.Fatalf("task artifact link did not survive restart: items=%+v err=%v", items, err)
	}
}

func TestRecoveryResultWriteRollsBackOnJournalFailure(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.RegisterExecutionNode(ctx, ExecutionNode{ID: "node_recovery_result", UserID: owner, Name: "node", Platform: "linux"}, "node-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "node-token", "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExecutionTask(ctx, ExecutionTask{ID: "task_recovery_result", UserID: owner, NodeID: "node_recovery_result", IdempotencyKey: "recovery-result-key", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_recovery_result", "lease-token-hash", now.Add(time.Second), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	content := []byte("verified transcript")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	upload, _, err := store.CreateArtifactUpload(ctx, ArtifactUpload{ID: "upload_recovery_result", UserID: owner, IdempotencyKey: "recovery-result-artifact", FileName: "transcript.json", MediaType: "application/json", ExpectedSize: int64(len(content)), ExpectedSHA256: digestHex, ChunkSize: 8 << 20, ChunkCount: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordArtifactChunk(ctx, owner, upload.ID, 0, digestHex, int64(len(content)), now); err != nil {
		t.Fatal(err)
	}
	artifact, _, err := store.FinalizeArtifactUpload(ctx, owner, upload.ID, "artifact_recovery_result", digestHex, "sha256/"+digestHex[:2]+"/"+digestHex, int64(len(content)), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskRecoveryEvidence(ctx, "node_recovery_result", "task_recovery_result", "lease-token-hash", artifact.ID, "recovery_transcript", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{"transcriptArtifact": map[string]any{"id": artifact.ID, "sha256": artifact.SHA256, "byteSize": artifact.ByteSize}})
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_recovery_result BEFORE INSERT ON execution_task_events WHEN NEW.kind='recovery_result_verified' BEGIN SELECT RAISE(ABORT,'forced recovery result audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveExecutionTaskRecoveryResult(ctx, "node_recovery_result", "task_recovery_result", "lease-token-hash", manifest, now.Add(4*time.Second)); err == nil {
		t.Fatal("recovery manifest succeeded without its durable audit event")
	}
	task, err := store.ExecutionTask(ctx, owner, "task_recovery_result")
	if err != nil || len(task.RecoveryResult) != 0 || task.Status != "needs_reconciliation" || task.Sequence != 4 {
		t.Fatalf("failed manifest transaction partially changed task: %+v err=%v", task, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_recovery_result`); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.SaveExecutionTaskRecoveryResult(ctx, "node_recovery_result", "task_recovery_result", "lease-token-hash", manifest, now.Add(5*time.Second))
	if err != nil || readBack.Status != "needs_reconciliation" || readBack.Sequence != 5 || string(readBack.RecoveryResult) != string(manifest) {
		t.Fatalf("manifest retry did not durably read back: %+v err=%v", readBack, err)
	}
}

func TestRecoveryEvidenceAssociationRollsBackWhenAuditEventFails(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.RegisterExecutionNode(ctx, ExecutionNode{ID: "node_recovery_rollback", UserID: owner, Name: "Offline node", Platform: "linux"}, "recovery-rollback-node-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "recovery-rollback-node-token", "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExecutionTask(ctx, ExecutionTask{ID: "task_recovery_rollback", UserID: owner, NodeID: "node_recovery_rollback", IdempotencyKey: "recovery-rollback", Payload: []byte(`{"kind":"agent_turn"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_recovery_rollback", "recovery-rollback-lease", now.Add(time.Second), now); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, now.Add(2*time.Second)); err != nil || count != 1 {
		t.Fatalf("expire recovery test task: count=%d err=%v", count, err)
	}
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	upload, _, err := store.CreateArtifactUpload(ctx, ArtifactUpload{ID: "upl_recovery_rollback", UserID: owner, IdempotencyKey: "recovery-rollback-upload", FileName: "transcript.json", MediaType: "application/json", ExpectedSize: 0, ChunkSize: 8 << 20, ChunkCount: 0}, now)
	if err != nil {
		t.Fatal(err)
	}
	artifact, _, err := store.FinalizeArtifactUpload(ctx, owner, upload.ID, "art_recovery_rollback", digest, "sha256/e3/"+digest, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_recovery_evidence_event BEFORE INSERT ON execution_task_events WHEN NEW.kind='recovery_evidence_attached' BEGIN SELECT RAISE(ABORT,'injected recovery evidence journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskRecoveryEvidence(ctx, "node_recovery_rollback", "task_recovery_rollback", "recovery-rollback-lease", artifact.ID, "recovery_transcript", now.Add(3*time.Second)); err == nil {
		t.Fatal("recovery evidence reported success after its audit event failed")
	}
	task, err := store.ExecutionTask(ctx, owner, "task_recovery_rollback")
	if err != nil || task.Status != "needs_reconciliation" || task.Sequence != 3 || task.LeaseUntil != nil {
		t.Fatalf("failed evidence attachment changed task state: task=%+v err=%v", task, err)
	}
	items, err := store.ExecutionTaskArtifacts(ctx, owner, task.ID)
	if err != nil || len(items) != 0 {
		t.Fatalf("failed evidence attachment left a partial artifact link: items=%+v err=%v", items, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_recovery_evidence_event`); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskRecoveryEvidence(ctx, "node_recovery_rollback", task.ID, "recovery-rollback-lease", artifact.ID, "recovery_transcript", now.Add(4*time.Second)); err != nil {
		t.Fatalf("evidence retry failed after removing injected failure: %v", err)
	}
	task, err = store.ExecutionTask(ctx, owner, task.ID)
	items, itemsErr := store.ExecutionTaskArtifacts(ctx, owner, task.ID)
	if err != nil || itemsErr != nil || task.Status != "needs_reconciliation" || task.Sequence != 4 || len(items) != 1 || items[0].ID != artifact.ID || items[0].Role != "recovery_transcript" {
		t.Fatalf("evidence retry did not durably read back: task=%+v items=%+v err=%v itemsErr=%v", task, items, err, itemsErr)
	}
}
