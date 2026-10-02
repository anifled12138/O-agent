package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestExecutionTaskHandoffCheckpointLeaseIdempotenceAndRestart(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.RegisterExecutionNode(ctx, ExecutionNode{ID: "node_handoff_test", UserID: owner, Name: "Worker", Platform: "linux"}, "node-handoff-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "node-handoff-token", "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	task, _, err := store.CreateExecutionTask(ctx, ExecutionTask{ID: "task_handoff_test", UserID: owner, NodeID: "node_handoff_test", IdempotencyKey: "handoff-key", Payload: []byte(`{"sourceConversationId":"source"}`)}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	const artifactBody = "sealed-node-checkpoint"
	digest := sha256.Sum256([]byte(artifactBody))
	sha := hex.EncodeToString(digest[:])
	upload, _, err := store.CreateArtifactUpload(ctx, ArtifactUpload{ID: "upl_handoff_test", UserID: owner, IdempotencyKey: "handoff-upload", FileName: "checkpoint.json", MediaType: "application/json", ExpectedSize: int64(len(artifactBody)), ChunkSize: 1024, ChunkCount: 1, ExpectedSHA256: sha}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordArtifactChunk(ctx, owner, upload.ID, 0, sha, int64(len(artifactBody)), now); err != nil {
		t.Fatal(err)
	}
	artifact, _, err := store.FinalizeArtifactUpload(ctx, owner, upload.ID, "art_handoff_test", sha, "sha256/"+sha, int64(len(artifactBody)), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskArtifact(ctx, owner, task.ID, artifact.ID, "continuation_checkpoint", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, task.NodeID, "active-lease-hash", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	handoff := ExecutionTaskHandoffCheckpoint{TaskID: task.ID, NodeID: task.NodeID, ArtifactID: artifact.ID, ArtifactSHA256: sha, ArtifactByteSize: int64(len(artifactBody)), SourceTurnID: "turn_source", SourceConversationID: "conversation_source", SourceInputMessageID: "message_source", ProviderID: "provider_source", GenerationID: "generation_source", DefinitionDigest: "definition_digest", PermissionProfile: domain.DefaultPermissionProfile(), ContentSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Version: 1, Ciphertext: []byte("cloud-vault-ciphertext"), Nonce: []byte("cloud-vault-nonce")}
	readBack, err := store.SaveExecutionTaskHandoffCheckpoint(ctx, handoff, "active-lease-hash", now.Add(time.Second))
	if err != nil || string(readBack.Ciphertext) != string(handoff.Ciphertext) || len(readBack.Nonce) == 0 {
		t.Fatalf("checkpoint was not durably saved and read back: %+v err=%v", readBack, err)
	}
	// A retried offer may be re-encrypted with a new nonce; the first stored
	// durable cloud cipher remains authoritative and is returned unchanged.
	retry := handoff
	retry.Ciphertext = []byte("new-random-ciphertext")
	retry.Nonce = []byte("new-random-nonce")
	retried, err := store.SaveExecutionTaskHandoffCheckpoint(ctx, retry, "active-lease-hash", now.Add(2*time.Second))
	if err != nil || string(retried.Ciphertext) != string(handoff.Ciphertext) || string(retried.Nonce) != string(handoff.Nonce) {
		t.Fatalf("identical checkpoint retry replaced durable ciphertext: %+v err=%v", retried, err)
	}
	conflict := retry
	conflict.ContentSHA256 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if _, err := store.SaveExecutionTaskHandoffCheckpoint(ctx, conflict, "active-lease-hash", now.Add(2*time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting checkpoint retry = %v, want conflict", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.ExecutionTaskHandoffCheckpoint(ctx, owner, task.ID)
	if err != nil || string(persisted.Ciphertext) != string(handoff.Ciphertext) || string(persisted.Nonce) != string(handoff.Nonce) || persisted.ContentSHA256 != handoff.ContentSHA256 {
		t.Fatalf("encrypted handoff checkpoint did not survive restart: %+v err=%v", persisted, err)
	}
	if _, err := store.SaveExecutionTaskHandoffCheckpoint(ctx, handoff, "active-lease-hash", now.Add(2*time.Minute)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired lease persisted checkpoint: %v", err)
	}
	recoveryNow := now.Add(3 * time.Minute)
	if _, err := store.RegisterExecutionNode(ctx, ExecutionNode{ID: "node_handoff_recovery", UserID: owner, Name: "Recovery worker", Platform: "linux"}, "recovery-node-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "recovery-node-token", "linux", nil, recoveryNow); err != nil {
		t.Fatal(err)
	}
	recoveryTask, _, err := store.CreateExecutionTask(ctx, ExecutionTask{ID: "task_handoff_recovery", UserID: owner, NodeID: "node_handoff_recovery", IdempotencyKey: "handoff-recovery-key", Payload: []byte(`{"sourceConversationId":"source"}`)}, false, recoveryNow)
	if err != nil {
		t.Fatal(err)
	}
	const recoveryLeaseHash = "expired-recovery-lease-hash"
	if _, err := store.ClaimExecutionTask(ctx, recoveryTask.NodeID, recoveryLeaseHash, recoveryNow.Add(time.Minute), recoveryNow); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, recoveryNow.Add(2*time.Minute)); err != nil || count != 2 {
		t.Fatalf("expire recovery checkpoint lease: count=%d err=%v", count, err)
	}
	if err := store.AttachExecutionTaskRecoveryEvidence(ctx, recoveryTask.NodeID, recoveryTask.ID, recoveryLeaseHash, artifact.ID, "recovery_transcript", recoveryNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskRecoveryEvidence(ctx, recoveryTask.NodeID, recoveryTask.ID, recoveryLeaseHash, artifact.ID, "recovery_checkpoint", recoveryNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	recoveryResult := []byte(`{"transcriptArtifact":{"id":"` + artifact.ID + `","sha256":"` + sha + `","byteSize":` + fmt.Sprint(len(artifactBody)) + `},"handoffCheckpointArtifact":{"id":"` + artifact.ID + `","sha256":"` + sha + `","byteSize":` + fmt.Sprint(len(artifactBody)) + `}}`)
	recoveredHandoff := handoff
	recoveredHandoff.TaskID = recoveryTask.ID
	recoveredHandoff.NodeID = recoveryTask.NodeID
	recoveredHandoff.ArtifactID = artifact.ID
	recoveredHandoff.ArtifactSHA256 = sha
	recoveredHandoff.ArtifactByteSize = int64(len(artifactBody))
	recoveredHandoff.Ciphertext = []byte("cloud-vault-recovery-ciphertext")
	recoveredHandoff.Nonce = []byte("cloud-vault-recovery-nonce")
	beforeRejectedRecovery, err := store.ExecutionTask(ctx, owner, recoveryTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_recovery_checkpoint_event BEFORE INSERT ON execution_task_events WHEN NEW.kind='recovery_checkpoint_rebound' BEGIN SELECT RAISE(ABORT,'injected recovery checkpoint journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveRecoveredExecutionTaskHandoffCheckpoint(ctx, recoveredHandoff, recoveryLeaseHash, recoveryNow.Add(5*time.Minute)); err == nil {
		t.Fatal("recovery checkpoint persisted despite failed audit event")
	}
	if _, err := store.ExecutionTaskHandoffCheckpoint(ctx, owner, recoveryTask.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed recovery checkpoint write left a durable checkpoint row: %v", err)
	}
	afterRejectedRecovery, err := store.ExecutionTask(ctx, owner, recoveryTask.ID)
	if err != nil || afterRejectedRecovery.Sequence != beforeRejectedRecovery.Sequence || afterRejectedRecovery.Status != "needs_reconciliation" || len(afterRejectedRecovery.Result) != 0 {
		t.Fatalf("failed recovery checkpoint write changed task state: before=%+v after=%+v err=%v", beforeRejectedRecovery, afterRejectedRecovery, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_recovery_checkpoint_event`); err != nil {
		t.Fatal(err)
	}
	recoveredReadBack, err := store.SaveRecoveredExecutionTaskHandoffCheckpoint(ctx, recoveredHandoff, recoveryLeaseHash, recoveryNow.Add(5*time.Minute))
	if err != nil || string(recoveredReadBack.Ciphertext) != string(recoveredHandoff.Ciphertext) || len(recoveredReadBack.Nonce) == 0 {
		t.Fatalf("recovered checkpoint was not durably rewrapped: %+v err=%v", recoveredReadBack, err)
	}
	if _, err := store.SaveExecutionTaskRecoveryResult(ctx, recoveryTask.NodeID, recoveryTask.ID, recoveryLeaseHash, recoveryResult, recoveryNow.Add(5*time.Minute)); err == nil {
		t.Fatal("recovery manifest became visible without matching checkpoint identity")
	}
	recoveryResult, _ = json.Marshal(map[string]any{"conversationId": recoveredHandoff.SourceConversationID, "agentTurnId": recoveredHandoff.SourceTurnID, "transcriptArtifact": map[string]any{"id": artifact.ID, "sha256": sha, "byteSize": len(artifactBody)}, "handoffCheckpointArtifact": map[string]any{"id": artifact.ID, "sha256": sha, "byteSize": len(artifactBody)}})
	if _, err := store.SaveExecutionTaskRecoveryResult(ctx, recoveryTask.NodeID, recoveryTask.ID, recoveryLeaseHash, recoveryResult, recoveryNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveRecoveredExecutionTaskHandoffCheckpoint(ctx, recoveredHandoff, "wrong-recovery-lease", recoveryNow.Add(5*time.Minute)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("recovery checkpoint accepted an invalid lease token: %v", err)
	}
	wrongArtifact := recoveredHandoff
	wrongArtifact.ArtifactSHA256 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if _, err := store.SaveRecoveredExecutionTaskHandoffCheckpoint(ctx, wrongArtifact, recoveryLeaseHash, recoveryNow.Add(5*time.Minute)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("recovery checkpoint accepted an artifact outside its manifest: %v", err)
	}
	retryHandoff := recoveredHandoff
	retryHandoff.Ciphertext = []byte("different-retry-ciphertext")
	retryHandoff.Nonce = []byte("different-retry-nonce")
	retryReadBack, err := store.SaveRecoveredExecutionTaskHandoffCheckpoint(ctx, retryHandoff, recoveryLeaseHash, recoveryNow.Add(6*time.Minute))
	if err != nil || string(retryReadBack.Ciphertext) != string(recoveredHandoff.Ciphertext) {
		t.Fatalf("recovered checkpoint retry replaced durable ciphertext: %+v err=%v", retryReadBack, err)
	}
	recoveryTaskReadBack, err := store.ExecutionTask(ctx, owner, recoveryTask.ID)
	if err != nil || recoveryTaskReadBack.Status != "needs_reconciliation" || len(recoveryTaskReadBack.Result) != 0 || string(recoveryTaskReadBack.RecoveryResult) != string(recoveryResult) {
		t.Fatalf("checkpoint recovery changed original task outcome: task=%+v err=%v", recoveryTaskReadBack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
