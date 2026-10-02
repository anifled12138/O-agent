package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"axiom.local/agent/internal/domain"
)

type ExecutionTaskHandoffCheckpoint struct {
	TaskID               string                   `json:"taskId"`
	UserID               string                   `json:"-"`
	NodeID               string                   `json:"nodeId"`
	ArtifactID           string                   `json:"artifactId"`
	ArtifactSHA256       string                   `json:"artifactSha256"`
	ArtifactByteSize     int64                    `json:"artifactByteSize"`
	SourceTurnID         string                   `json:"sourceTurnId"`
	SourceConversationID string                   `json:"sourceConversationId"`
	SourceInputMessageID string                   `json:"sourceInputMessageId"`
	ProviderID           string                   `json:"providerId"`
	GenerationID         string                   `json:"generationId"`
	DefinitionDigest     string                   `json:"definitionDigest"`
	PermissionProfile    domain.PermissionProfile `json:"permissionProfile"`
	ProjectID            string                   `json:"projectId,omitempty"`
	ProjectCommit        string                   `json:"projectCommit,omitempty"`
	ContentSHA256        string                   `json:"contentSha256"`
	Version              int                      `json:"version"`
	Ciphertext           []byte                   `json:"-"`
	Nonce                []byte                   `json:"-"`
	CreatedAt            time.Time                `json:"createdAt"`
	UpdatedAt            time.Time                `json:"updatedAt"`
}

// SaveExecutionTaskHandoffCheckpoint rewraps a lease-authenticated node
// capsule under the cloud vault key while the originating node still owns its
// live task lease. Repeated identical offers retain the first durable cipher.
func (s *Store) SaveExecutionTaskHandoffCheckpoint(ctx context.Context, handoff ExecutionTaskHandoffCheckpoint, leaseHash string, now time.Time) (ExecutionTaskHandoffCheckpoint, error) {
	if handoff.TaskID == "" || handoff.NodeID == "" || handoff.ArtifactID == "" || handoff.ArtifactByteSize <= 0 || handoff.SourceTurnID == "" || handoff.SourceConversationID == "" || handoff.SourceInputMessageID == "" || handoff.ProviderID == "" || handoff.GenerationID == "" || handoff.DefinitionDigest == "" || !handoff.PermissionProfile.Valid() || len(handoff.ContentSHA256) != 64 || handoff.Version != 1 || len(handoff.Ciphertext) == 0 || len(handoff.Ciphertext) > (16<<20)+(1<<10) || len(handoff.Nonce) == 0 || leaseHash == "" {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	defer tx.Rollback()
	var userID, status, storedLease string
	var leaseUntil sql.NullTime
	var cancelRequested int
	if err := tx.QueryRowContext(ctx, `SELECT user_id,status,lease_token_hash,lease_until,cancel_requested FROM execution_tasks WHERE id=? AND node_id=?`, handoff.TaskID, handoff.NodeID).Scan(&userID, &status, &storedLease, &leaseUntil, &cancelRequested); errors.Is(err, sql.ErrNoRows) {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrNotFound
	} else if err != nil {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	if storedLease != leaseHash || !leaseUntil.Valid || !leaseUntil.Time.After(now.UTC()) || (status != "leased" && status != "accepted" && status != "running") || cancelRequested != 0 {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrUnauthorized
	}
	var artifactHash string
	var artifactSize int64
	if err := tx.QueryRowContext(ctx, `SELECT a.sha256,a.byte_size FROM execution_task_artifacts ta JOIN artifacts a ON a.id=ta.artifact_id WHERE ta.task_id=? AND ta.artifact_id=? AND ta.role='continuation_checkpoint' AND a.user_id=?`, handoff.TaskID, handoff.ArtifactID, userID).Scan(&artifactHash, &artifactSize); errors.Is(err, sql.ErrNoRows) {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrNotFound
	} else if err != nil {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	if artifactHash != handoff.ArtifactSHA256 || artifactSize != handoff.ArtifactByteSize {
		return ExecutionTaskHandoffCheckpoint{}, fmt.Errorf("handoff checkpoint artifact manifest changed: %w", domain.ErrConflict)
	}
	handoff.UserID = userID
	handoff.CreatedAt = now.UTC()
	handoff.UpdatedAt = now.UTC()
	var existing ExecutionTaskHandoffCheckpoint
	err = scanExecutionTaskHandoffCheckpoint(tx.QueryRowContext(ctx, `SELECT task_id,user_id,node_id,artifact_id,artifact_sha256,artifact_byte_size,source_turn_id,source_conversation_id,source_input_message_id,provider_id,generation_id,definition_digest,permission_profile,project_id,project_commit,content_sha256,checkpoint_version,state_cipher,state_nonce,created_at,updated_at FROM execution_task_handoff_checkpoints WHERE task_id=?`, handoff.TaskID), &existing)
	if err == nil {
		if !sameHandoffCheckpoint(existing, handoff) {
			return ExecutionTaskHandoffCheckpoint{}, domain.ErrConflict
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO execution_task_handoff_checkpoints(task_id,user_id,node_id,artifact_id,artifact_sha256,artifact_byte_size,source_turn_id,source_conversation_id,source_input_message_id,provider_id,generation_id,definition_digest,permission_profile,project_id,project_commit,content_sha256,checkpoint_version,state_cipher,state_nonce,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, handoff.TaskID, userID, handoff.NodeID, handoff.ArtifactID, handoff.ArtifactSHA256, handoff.ArtifactByteSize, handoff.SourceTurnID, handoff.SourceConversationID, handoff.SourceInputMessageID, handoff.ProviderID, handoff.GenerationID, handoff.DefinitionDigest, handoff.PermissionProfile, handoff.ProjectID, handoff.ProjectCommit, handoff.ContentSHA256, handoff.Version, handoff.Ciphertext, handoff.Nonce, handoff.CreatedAt, handoff.UpdatedAt); err != nil {
			return ExecutionTaskHandoffCheckpoint{}, err
		}
	} else {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	readBack, err := s.ExecutionTaskHandoffCheckpoint(ctx, userID, handoff.TaskID)
	if err != nil {
		return ExecutionTaskHandoffCheckpoint{}, fmt.Errorf("read cloud-encrypted handoff checkpoint back: %w", err)
	}
	if !sameHandoffCheckpoint(readBack, handoff) || len(readBack.Ciphertext) == 0 || len(readBack.Nonce) == 0 {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrConflict
	}
	return readBack, nil
}

// SaveRecoveredExecutionTaskHandoffCheckpoint rewraps an expired task's
// lease-authenticated checkpoint after recovery authorization has verified
// the original lease token. It never restores execution ownership.
func (s *Store) SaveRecoveredExecutionTaskHandoffCheckpoint(ctx context.Context, handoff ExecutionTaskHandoffCheckpoint, recoveryLeaseHash string, now time.Time) (ExecutionTaskHandoffCheckpoint, error) {
	if handoff.TaskID == "" || handoff.NodeID == "" || handoff.ArtifactID == "" || handoff.ArtifactByteSize <= 0 || handoff.SourceTurnID == "" || handoff.SourceConversationID == "" || handoff.SourceInputMessageID == "" || handoff.ProviderID == "" || handoff.GenerationID == "" || handoff.DefinitionDigest == "" || !handoff.PermissionProfile.Valid() || len(handoff.ContentSHA256) != 64 || handoff.Version != 1 || len(handoff.Ciphertext) == 0 || len(handoff.Ciphertext) > (16<<20)+(1<<10) || len(handoff.Nonce) == 0 || recoveryLeaseHash == "" {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	defer tx.Rollback()
	var userID, status, storedRecoveryLease, activeLease string
	var sequence int64
	var leaseUntil sql.NullTime
	var recoveryResult []byte
	if err := tx.QueryRowContext(ctx, `SELECT user_id,status,recovery_lease_token_hash,lease_token_hash,lease_until,sequence,recovery_result_json FROM execution_tasks WHERE id=? AND node_id=?`, handoff.TaskID, handoff.NodeID).Scan(&userID, &status, &storedRecoveryLease, &activeLease, &leaseUntil, &sequence, &recoveryResult); errors.Is(err, sql.ErrNoRows) {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrNotFound
	} else if err != nil {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	if status != "needs_reconciliation" || activeLease != "" || leaseUntil.Valid || storedRecoveryLease == "" || storedRecoveryLease != recoveryLeaseHash {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrUnauthorized
	}
	if len(recoveryResult) > 0 {
		var manifest struct {
			HandoffCheckpointArtifact *struct {
				ID       string `json:"id"`
				SHA256   string `json:"sha256"`
				ByteSize int64  `json:"byteSize"`
			} `json:"handoffCheckpointArtifact"`
		}
		if err := json.Unmarshal(recoveryResult, &manifest); err != nil || manifest.HandoffCheckpointArtifact == nil || manifest.HandoffCheckpointArtifact.ID != handoff.ArtifactID || manifest.HandoffCheckpointArtifact.SHA256 != handoff.ArtifactSHA256 || manifest.HandoffCheckpointArtifact.ByteSize != handoff.ArtifactByteSize {
			return ExecutionTaskHandoffCheckpoint{}, domain.ErrConflict
		}
	}
	var artifactHash string
	var artifactSize int64
	if err := tx.QueryRowContext(ctx, `SELECT a.sha256,a.byte_size FROM execution_task_artifacts ta JOIN artifacts a ON a.id=ta.artifact_id WHERE ta.task_id=? AND ta.artifact_id=? AND ta.role='recovery_checkpoint' AND a.user_id=?`, handoff.TaskID, handoff.ArtifactID, userID).Scan(&artifactHash, &artifactSize); errors.Is(err, sql.ErrNoRows) {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrNotFound
	} else if err != nil {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	if artifactHash != handoff.ArtifactSHA256 || artifactSize != handoff.ArtifactByteSize {
		return ExecutionTaskHandoffCheckpoint{}, fmt.Errorf("recovery checkpoint artifact manifest changed: %w", domain.ErrConflict)
	}
	handoff.UserID = userID
	handoff.CreatedAt = now.UTC()
	handoff.UpdatedAt = now.UTC()
	var existing ExecutionTaskHandoffCheckpoint
	err = scanExecutionTaskHandoffCheckpoint(tx.QueryRowContext(ctx, `SELECT task_id,user_id,node_id,artifact_id,artifact_sha256,artifact_byte_size,source_turn_id,source_conversation_id,source_input_message_id,provider_id,generation_id,definition_digest,permission_profile,project_id,project_commit,content_sha256,checkpoint_version,state_cipher,state_nonce,created_at,updated_at FROM execution_task_handoff_checkpoints WHERE task_id=?`, handoff.TaskID), &existing)
	if err == nil {
		if !sameHandoffCheckpoint(existing, handoff) && !sameHandoffCheckpointBinding(existing, handoff) {
			return ExecutionTaskHandoffCheckpoint{}, domain.ErrConflict
		}
		// A checkpoint may have been rewrapped while the lease was active,
		// before the node could deliver its terminal result. Keep that first
		// cloud-vault copy when recovery later proves the same checkpoint.
		handoff = existing
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO execution_task_handoff_checkpoints(task_id,user_id,node_id,artifact_id,artifact_sha256,artifact_byte_size,source_turn_id,source_conversation_id,source_input_message_id,provider_id,generation_id,definition_digest,permission_profile,project_id,project_commit,content_sha256,checkpoint_version,state_cipher,state_nonce,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, handoff.TaskID, userID, handoff.NodeID, handoff.ArtifactID, handoff.ArtifactSHA256, handoff.ArtifactByteSize, handoff.SourceTurnID, handoff.SourceConversationID, handoff.SourceInputMessageID, handoff.ProviderID, handoff.GenerationID, handoff.DefinitionDigest, handoff.PermissionProfile, handoff.ProjectID, handoff.ProjectCommit, handoff.ContentSHA256, handoff.Version, handoff.Ciphertext, handoff.Nonce, handoff.CreatedAt, handoff.UpdatedAt); err != nil {
			return ExecutionTaskHandoffCheckpoint{}, err
		}
		next := sequence + 1
		updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET sequence=?,updated_at=? WHERE id=? AND user_id=? AND node_id=? AND status='needs_reconciliation' AND recovery_lease_token_hash=? AND lease_token_hash='' AND lease_until IS NULL AND sequence=?`, next, now.UTC(), handoff.TaskID, userID, handoff.NodeID, recoveryLeaseHash, sequence)
		if err != nil {
			return ExecutionTaskHandoffCheckpoint{}, err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return ExecutionTaskHandoffCheckpoint{}, err
		}
		if changed != 1 {
			return ExecutionTaskHandoffCheckpoint{}, domain.ErrConflict
		}
		details, err := json.Marshal(map[string]string{"source": "expired_lease_outbox", "artifactId": handoff.ArtifactID})
		if err != nil {
			return ExecutionTaskHandoffCheckpoint{}, err
		}
		if err := insertExecutionTaskEvent(ctx, tx, handoff.TaskID, next, "recovery_checkpoint_rebound", details, now); err != nil {
			return ExecutionTaskHandoffCheckpoint{}, err
		}
	} else {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTaskHandoffCheckpoint{}, err
	}
	readBack, err := s.ExecutionTaskHandoffCheckpoint(ctx, userID, handoff.TaskID)
	if err != nil {
		return ExecutionTaskHandoffCheckpoint{}, fmt.Errorf("read cloud-vault recovery checkpoint back: %w", err)
	}
	if !sameHandoffCheckpointBinding(readBack, handoff) || len(readBack.Ciphertext) == 0 || len(readBack.Nonce) == 0 {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrConflict
	}
	return readBack, nil
}

func (s *Store) ExecutionTaskHandoffCheckpoint(ctx context.Context, userID, taskID string) (ExecutionTaskHandoffCheckpoint, error) {
	var handoff ExecutionTaskHandoffCheckpoint
	err := scanExecutionTaskHandoffCheckpoint(s.db.QueryRowContext(ctx, `SELECT task_id,user_id,node_id,artifact_id,artifact_sha256,artifact_byte_size,source_turn_id,source_conversation_id,source_input_message_id,provider_id,generation_id,definition_digest,permission_profile,project_id,project_commit,content_sha256,checkpoint_version,state_cipher,state_nonce,created_at,updated_at FROM execution_task_handoff_checkpoints WHERE task_id=? AND user_id=?`, taskID, userID), &handoff)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionTaskHandoffCheckpoint{}, domain.ErrNotFound
	}
	return handoff, err
}

type handoffCheckpointScanner interface {
	Scan(dest ...any) error
}

func scanExecutionTaskHandoffCheckpoint(scanner handoffCheckpointScanner, handoff *ExecutionTaskHandoffCheckpoint) error {
	return scanner.Scan(&handoff.TaskID, &handoff.UserID, &handoff.NodeID, &handoff.ArtifactID, &handoff.ArtifactSHA256, &handoff.ArtifactByteSize, &handoff.SourceTurnID, &handoff.SourceConversationID, &handoff.SourceInputMessageID, &handoff.ProviderID, &handoff.GenerationID, &handoff.DefinitionDigest, &handoff.PermissionProfile, &handoff.ProjectID, &handoff.ProjectCommit, &handoff.ContentSHA256, &handoff.Version, &handoff.Ciphertext, &handoff.Nonce, &handoff.CreatedAt, &handoff.UpdatedAt)
}

func sameHandoffCheckpoint(left, right ExecutionTaskHandoffCheckpoint) bool {
	return left.TaskID == right.TaskID && left.UserID == right.UserID && left.NodeID == right.NodeID && left.ArtifactID == right.ArtifactID && left.ArtifactSHA256 == right.ArtifactSHA256 && left.ArtifactByteSize == right.ArtifactByteSize && left.SourceTurnID == right.SourceTurnID && left.SourceConversationID == right.SourceConversationID && left.SourceInputMessageID == right.SourceInputMessageID && left.ProviderID == right.ProviderID && left.GenerationID == right.GenerationID && left.DefinitionDigest == right.DefinitionDigest && left.PermissionProfile == right.PermissionProfile && left.ProjectID == right.ProjectID && left.ProjectCommit == right.ProjectCommit && left.ContentSHA256 == right.ContentSHA256 && left.Version == right.Version
}

func sameHandoffCheckpointBinding(left, right ExecutionTaskHandoffCheckpoint) bool {
	return left.TaskID == right.TaskID && left.UserID == right.UserID && left.NodeID == right.NodeID && left.SourceTurnID == right.SourceTurnID && left.SourceConversationID == right.SourceConversationID && left.SourceInputMessageID == right.SourceInputMessageID && left.ProviderID == right.ProviderID && left.GenerationID == right.GenerationID && left.DefinitionDigest == right.DefinitionDigest && left.PermissionProfile == right.PermissionProfile && left.ProjectID == right.ProjectID && left.ProjectCommit == right.ProjectCommit && left.ContentSHA256 == right.ContentSHA256 && left.Version == right.Version
}
