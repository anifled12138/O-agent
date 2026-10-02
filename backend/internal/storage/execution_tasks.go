package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

type ExecutionTask struct {
	ID                     string               `json:"id"`
	UserID                 string               `json:"-"`
	LogicalTaskID          string               `json:"logicalTaskId"`
	ParentTaskID           string               `json:"parentTaskId,omitempty"`
	SegmentIndex           int                  `json:"segmentIndex"`
	NodeID                 string               `json:"nodeId"`
	IdempotencyKey         string               `json:"idempotencyKey"`
	Payload                json.RawMessage      `json:"payload"`
	Result                 json.RawMessage      `json:"result,omitempty"`
	RecoveryResult         json.RawMessage      `json:"recoveryResult,omitempty"`
	ImportedProject        *domain.Project      `json:"importedProject,omitempty"`
	ContinuedConversation  *domain.Conversation `json:"continuedConversation,omitempty"`
	ContinuationTaskID     string               `json:"continuationTaskId,omitempty"`
	ContinuationTaskStatus string               `json:"continuationTaskStatus,omitempty"`
	Status                 string               `json:"status"`
	Attempt                int                  `json:"attempt"`
	Sequence               int64                `json:"sequence"`
	LeaseUntil             *time.Time           `json:"leaseUntil,omitempty"`
	CancelRequested        bool                 `json:"cancelRequested"`
	HandoffRequested       bool                 `json:"handoffRequested"`
	ProgressPhase          string               `json:"progressPhase,omitempty"`
	ProgressUpdatedAt      *time.Time           `json:"progressUpdatedAt,omitempty"`
	Error                  string               `json:"error,omitempty"`
	CreatedAt              time.Time            `json:"createdAt"`
	UpdatedAt              time.Time            `json:"updatedAt"`
}

type ExecutionTaskEvent struct {
	TaskID    string          `json:"taskId"`
	Sequence  int64           `json:"sequence"`
	Kind      string          `json:"kind"`
	Details   json.RawMessage `json:"details"`
	CreatedAt time.Time       `json:"createdAt"`
}

type ExecutionTaskArtifactLink struct {
	ArtifactID string
	Role       string
}

func (s *Store) CreateExecutionTask(ctx context.Context, task ExecutionTask, waitForNode bool, now time.Time) (ExecutionTask, bool, error) {
	return s.createExecutionTask(ctx, task, waitForNode, nil, now)
}

// CreateExecutionTaskWithArtifacts makes the task claimable in the same
// transaction that attaches all required input artifacts.
func (s *Store) CreateExecutionTaskWithArtifacts(ctx context.Context, task ExecutionTask, waitForNode bool, artifacts []ExecutionTaskArtifactLink, now time.Time) (ExecutionTask, bool, error) {
	return s.createExecutionTask(ctx, task, waitForNode, artifacts, now)
}

// CreateCloudHandoffTask activates a prepared, paused continuation conversation
// in the same transaction that makes its cloud task claimable. A worker can
// therefore observe neither an active conversation without its durable task,
// nor a claimable task whose conversation is still in preparation.
func (s *Store) CreateCloudHandoffTask(ctx context.Context, task ExecutionTask, conversationID string, now time.Time) (ExecutionTask, bool, error) {
	return s.CreateCloudHandoffTaskWithArtifacts(ctx, task, conversationID, nil, now)
}

// CreateCloudHandoffTaskWithArtifacts activates the prepared continuation
// conversation and binds durable user inputs before the worker can claim it.
func (s *Store) CreateCloudHandoffTaskWithArtifacts(ctx context.Context, task ExecutionTask, conversationID string, artifacts []ExecutionTaskArtifactLink, now time.Time) (ExecutionTask, bool, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || task.NodeID != CloudExecutionNodeID(task.UserID) {
		return ExecutionTask{}, false, domain.ErrInvalid
	}
	var payload struct {
		Kind           string `json:"kind"`
		ConversationID string `json:"conversationId"`
	}
	if json.Unmarshal(task.Payload, &payload) != nil || payload.ConversationID != conversationID || (payload.Kind != "agent_turn" && payload.Kind != "agent_continuation") {
		return ExecutionTask{}, false, domain.ErrInvalid
	}
	return s.createExecutionTaskActivatingConversation(ctx, task, false, artifacts, conversationID, now)
}

func (s *Store) createExecutionTask(ctx context.Context, task ExecutionTask, waitForNode bool, artifacts []ExecutionTaskArtifactLink, now time.Time) (ExecutionTask, bool, error) {
	return s.createExecutionTaskActivatingConversation(ctx, task, waitForNode, artifacts, "", now)
}

func (s *Store) createExecutionTaskActivatingConversation(ctx context.Context, task ExecutionTask, waitForNode bool, artifacts []ExecutionTaskArtifactLink, activateConversationID string, now time.Time) (ExecutionTask, bool, error) {
	if task.SegmentIndex < 0 || (task.ParentTaskID == "" && task.SegmentIndex != 0) || (task.ParentTaskID != "" && task.SegmentIndex == 0) {
		return ExecutionTask{}, false, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTask{}, false, err
	}
	defer tx.Rollback()
	existing, readErr := executionTaskByKey(ctx, tx, task.UserID, task.IdempotencyKey)
	if readErr == nil {
		if existing.NodeID != task.NodeID || string(existing.Payload) != string(task.Payload) || task.LogicalTaskID != "" && existing.LogicalTaskID != task.LogicalTaskID || task.ParentTaskID != "" && existing.ParentTaskID != task.ParentTaskID || task.SegmentIndex != 0 && existing.SegmentIndex != task.SegmentIndex {
			return ExecutionTask{}, false, domain.ErrConflict
		}
		if err := attachExecutionTaskArtifactsTx(ctx, tx, task.UserID, existing.ID, artifacts, now); err != nil {
			return ExecutionTask{}, false, err
		}
		if activateConversationID != "" {
			if err := activateHandoffConversationTx(ctx, tx, task.UserID, activateConversationID, now, false); err != nil {
				return ExecutionTask{}, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return ExecutionTask{}, false, err
		}
		persisted, err := s.ExecutionTask(ctx, task.UserID, existing.ID)
		if err != nil {
			return ExecutionTask{}, false, err
		}
		if err := verifyExecutionTaskArtifactLinks(ctx, s, task.UserID, existing.ID, artifacts); err != nil {
			return ExecutionTask{}, false, err
		}
		return persisted, false, nil
	}
	if !errors.Is(readErr, domain.ErrNotFound) {
		return ExecutionTask{}, false, readErr
	}
	if task.LogicalTaskID == "" {
		task.LogicalTaskID = task.ID
	}
	if task.ParentTaskID == "" && task.SegmentIndex != 0 || task.ParentTaskID != "" && task.SegmentIndex == 0 {
		return ExecutionTask{}, false, domain.ErrInvalid
	}
	if task.ParentTaskID != "" {
		var parentLogicalTaskID string
		var parentSegmentIndex int
		if err := tx.QueryRowContext(ctx, `SELECT logical_task_id,segment_index FROM execution_tasks WHERE id=? AND user_id=?`, task.ParentTaskID, task.UserID).Scan(&parentLogicalTaskID, &parentSegmentIndex); errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, false, domain.ErrNotFound
		} else if err != nil {
			return ExecutionTask{}, false, err
		}
		if task.LogicalTaskID != parentLogicalTaskID || task.SegmentIndex != parentSegmentIndex+1 {
			return ExecutionTask{}, false, domain.ErrConflict
		}
	}
	var revokedAt sql.NullTime
	var lastSeen sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT revoked_at,last_seen FROM execution_nodes WHERE id=? AND user_id=?`, task.NodeID, task.UserID).Scan(&revokedAt, &lastSeen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, false, domain.ErrNotFound
		}
		return ExecutionTask{}, false, err
	}
	if revokedAt.Valid {
		return ExecutionTask{}, false, domain.ErrConflict
	}
	online := lastSeen.Valid && now.UTC().Sub(lastSeen.Time) <= 90*time.Second
	if !online && !waitForNode {
		return ExecutionTask{}, false, domain.ErrConflict
	}
	if activateConversationID != "" {
		if err := activateHandoffConversationTx(ctx, tx, task.UserID, activateConversationID, now, true); err != nil {
			return ExecutionTask{}, false, err
		}
	}
	task.Status = "queued"
	task.Attempt = 0
	task.Sequence = 1
	task.CreatedAt = now.UTC()
	task.UpdatedAt = now.UTC()
	details, err := json.Marshal(map[string]string{"nodeId": task.NodeID})
	if err != nil {
		return ExecutionTask{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_tasks(id,user_id,logical_task_id,parent_task_id,segment_index,node_id,idempotency_key,payload_json,status,attempt,sequence,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'queued',0,1,?,?)`, task.ID, task.UserID, task.LogicalTaskID, task.ParentTaskID, task.SegmentIndex, task.NodeID, task.IdempotencyKey, []byte(task.Payload), now.UTC(), now.UTC()); err != nil {
		return ExecutionTask{}, false, err
	}
	if err := insertExecutionTaskEvent(ctx, tx, task.ID, 1, "queued", details, now); err != nil {
		return ExecutionTask{}, false, err
	}
	if err := attachExecutionTaskArtifactsTx(ctx, tx, task.UserID, task.ID, artifacts, now); err != nil {
		return ExecutionTask{}, false, err
	}
	readBack, err := executionTaskByID(ctx, tx, task.UserID, task.ID)
	if err != nil {
		return ExecutionTask{}, false, err
	}
	if readBack.Status != "queued" || string(readBack.Payload) != string(task.Payload) || readBack.NodeID != task.NodeID || readBack.LogicalTaskID != task.LogicalTaskID || readBack.ParentTaskID != task.ParentTaskID || readBack.SegmentIndex != task.SegmentIndex {
		return ExecutionTask{}, false, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTask{}, false, err
	}
	persisted, err := s.ExecutionTask(ctx, task.UserID, task.ID)
	if err != nil {
		return ExecutionTask{}, false, err
	}
	// A worker may claim the task immediately after commit. The durable task
	// identity and payload are authoritative; requiring it to remain queued
	// would turn a successful fast claim into a false submission failure.
	if persisted.ID != task.ID || persisted.NodeID != task.NodeID || persisted.Sequence < 1 || string(persisted.Payload) != string(task.Payload) || persisted.LogicalTaskID != task.LogicalTaskID || persisted.ParentTaskID != task.ParentTaskID || persisted.SegmentIndex != task.SegmentIndex {
		return ExecutionTask{}, false, domain.ErrConflict
	}
	if activateConversationID != "" {
		var executionPaused bool
		if err := s.db.QueryRowContext(ctx, `SELECT execution_paused FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL`, activateConversationID, task.UserID).Scan(&executionPaused); err != nil {
			return ExecutionTask{}, false, err
		}
		if executionPaused {
			return ExecutionTask{}, false, domain.ErrConflict
		}
	}
	if err := verifyExecutionTaskArtifactLinks(ctx, s, task.UserID, task.ID, artifacts); err != nil {
		return ExecutionTask{}, false, err
	}
	return persisted, true, nil
}

func activateHandoffConversationTx(ctx context.Context, tx *sql.Tx, userID, conversationID string, now time.Time, requirePaused bool) error {
	var paused bool
	if err := tx.QueryRowContext(ctx, `SELECT execution_paused FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL`, conversationID, userID).Scan(&paused); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if requirePaused && !paused {
		return fmt.Errorf("new cloud handoff conversation was not paused during preparation: %w", domain.ErrConflict)
	}
	if paused {
		result, err := tx.ExecContext(ctx, `UPDATE conversations SET execution_paused=0,updated_at=? WHERE id=? AND user_id=? AND deleted_at IS NULL AND execution_paused=1`, now.UTC(), conversationID, userID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return domain.ErrConflict
		}
	}
	return nil
}

func attachExecutionTaskArtifactsTx(ctx context.Context, tx *sql.Tx, userID, taskID string, artifacts []ExecutionTaskArtifactLink, now time.Time) error {
	for _, link := range artifacts {
		link.ArtifactID = strings.TrimSpace(link.ArtifactID)
		link.Role = strings.TrimSpace(link.Role)
		if link.ArtifactID == "" || link.Role == "" || len(link.Role) > 64 {
			return domain.ErrInvalid
		}
		if _, err := artifactByID(ctx, tx, userID, link.ArtifactID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO execution_task_artifacts(task_id,artifact_id,role,created_at) VALUES(?,?,?,?) ON CONFLICT(task_id,artifact_id,role) DO NOTHING`, taskID, link.ArtifactID, link.Role, now.UTC()); err != nil {
			return err
		}
		var storedRole string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM execution_task_artifacts WHERE task_id=? AND artifact_id=? AND role=?`, taskID, link.ArtifactID, link.Role).Scan(&storedRole); err != nil {
			return err
		}
		if storedRole != link.Role {
			return domain.ErrConflict
		}
	}
	return nil
}

func verifyExecutionTaskArtifactLinks(ctx context.Context, store *Store, userID, taskID string, expected []ExecutionTaskArtifactLink) error {
	if len(expected) == 0 {
		return nil
	}
	items, err := store.ExecutionTaskArtifacts(ctx, userID, taskID)
	if err != nil {
		return err
	}
	for _, want := range expected {
		found := false
		for _, item := range items {
			if item.ID == want.ArtifactID && item.Role == want.Role {
				found = true
				break
			}
		}
		if !found {
			return domain.ErrConflict
		}
	}
	return nil
}

func (s *Store) ClaimExecutionTask(ctx context.Context, nodeID, leaseHash string, leaseUntil, now time.Time) (ExecutionTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTask{}, err
	}
	defer tx.Rollback()
	var nodeUserID string
	var revokedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT user_id,revoked_at FROM execution_nodes WHERE id=?`, nodeID).Scan(&nodeUserID, &revokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, domain.ErrUnauthorized
		}
		return ExecutionTask{}, err
	}
	if revokedAt.Valid {
		return ExecutionTask{}, domain.ErrUnauthorized
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,user_id,sequence,lease_token_hash FROM execution_tasks WHERE node_id=? AND status IN ('leased','accepted','running') AND lease_until<=?`, nodeID, now.UTC())
	if err != nil {
		return ExecutionTask{}, err
	}
	type expired struct {
		id        string
		userID    string
		leaseHash string
		sequence  int64
	}
	var expiredLeases []expired
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.id, &item.userID, &item.sequence, &item.leaseHash); err != nil {
			rows.Close()
			return ExecutionTask{}, err
		}
		expiredLeases = append(expiredLeases, item)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil {
		return ExecutionTask{}, rowsErr
	}
	if closeErr != nil {
		return ExecutionTask{}, closeErr
	}
	for _, item := range expiredLeases {
		next := item.sequence + 1
		updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET status='needs_reconciliation',recovery_lease_token_hash=lease_token_hash,lease_token_hash='',lease_until=NULL,error_text='node lease expired; execution outcome must be checked',sequence=?,updated_at=? WHERE id=? AND user_id=? AND sequence=? AND lease_token_hash=? AND status IN ('leased','accepted','running')`, next, now.UTC(), item.id, item.userID, item.sequence, item.leaseHash)
		if err != nil {
			return ExecutionTask{}, err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return ExecutionTask{}, err
		}
		if changed != 1 {
			return ExecutionTask{}, domain.ErrConflict
		}
		var persistedStatus, persistedLease, persistedRecoveryLease string
		var persistedSequence int64
		if err := tx.QueryRowContext(ctx, `SELECT status,sequence,lease_token_hash,recovery_lease_token_hash FROM execution_tasks WHERE id=?`, item.id).Scan(&persistedStatus, &persistedSequence, &persistedLease, &persistedRecoveryLease); err != nil {
			return ExecutionTask{}, err
		}
		if persistedStatus != "needs_reconciliation" || persistedSequence != next || persistedLease != "" || persistedRecoveryLease != item.leaseHash {
			return ExecutionTask{}, domain.ErrConflict
		}
		details := []byte(`{"reason":"lease_expired"}`)
		if err := insertExecutionTaskEvent(ctx, tx, item.id, next, "needs_reconciliation", details, now); err != nil {
			return ExecutionTask{}, err
		}
	}
	if nodeID != CloudExecutionNodeID(nodeUserID) {
		var resourcesJSON []byte
		var lastSeen sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT resources_json,last_seen FROM execution_nodes WHERE id=? AND revoked_at IS NULL`, nodeID).Scan(&resourcesJSON, &lastSeen); err != nil {
			return ExecutionTask{}, err
		}
		if !lastSeen.Valid || now.UTC().Sub(lastSeen.Time.UTC()) > 90*time.Second {
			if err := tx.Commit(); err != nil {
				return ExecutionTask{}, err
			}
			return ExecutionTask{}, domain.ErrNotFound
		}
		var resources ExecutionNodeResources
		if err := json.Unmarshal(resourcesJSON, &resources); err != nil {
			return ExecutionTask{}, fmt.Errorf("decode execution node admission snapshot: %w", err)
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_tasks WHERE node_id=? AND status IN ('leased','accepted','running') AND lease_until>?`, nodeID, now.UTC()).Scan(&active); err != nil {
			return ExecutionTask{}, err
		}
		if active >= resources.MaxConcurrentTasks {
			if err := tx.Commit(); err != nil {
				return ExecutionTask{}, err
			}
			return ExecutionTask{}, domain.ErrNotFound
		}
	}
	var taskID, userID string
	err = tx.QueryRowContext(ctx, `SELECT id,user_id FROM execution_tasks WHERE node_id=? AND status='queued' ORDER BY created_at,id LIMIT 1`, nodeID).Scan(&taskID, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return ExecutionTask{}, err
		}
		return ExecutionTask{}, domain.ErrNotFound
	}
	if err != nil {
		return ExecutionTask{}, err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT sequence FROM execution_tasks WHERE id=?`, taskID).Scan(&sequence); err != nil {
		return ExecutionTask{}, err
	}
	next := sequence + 1
	result, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET status='leased',attempt=attempt+1,sequence=?,lease_token_hash=?,recovery_lease_token_hash='',lease_until=?,updated_at=? WHERE id=? AND node_id=? AND status='queued' AND sequence=?`, next, leaseHash, leaseUntil.UTC(), now.UTC(), taskID, nodeID, sequence)
	if err != nil {
		return ExecutionTask{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ExecutionTask{}, err
	}
	if changed != 1 {
		return ExecutionTask{}, domain.ErrConflict
	}
	details, err := json.Marshal(map[string]any{"nodeId": nodeID, "attemptIncremented": true, "leaseUntil": leaseUntil.UTC()})
	if err != nil {
		return ExecutionTask{}, err
	}
	if err := insertExecutionTaskEvent(ctx, tx, taskID, next, "leased", details, now); err != nil {
		return ExecutionTask{}, err
	}
	task, err := executionTaskByID(ctx, tx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if task.Status != "leased" || task.Attempt < 1 || task.Sequence != next {
		return ExecutionTask{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTask{}, err
	}
	persisted, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if persisted.Status != "leased" || persisted.Attempt != task.Attempt || persisted.Sequence != next {
		return ExecutionTask{}, domain.ErrConflict
	}
	return persisted, nil
}

// ValidateExecutionTaskLease is a read-only authorization check for node
// side-effects that must be restricted to the task's current live attempt.
func (s *Store) ValidateExecutionTaskLease(ctx context.Context, nodeID, taskID, leaseHash string, now time.Time) (ExecutionTask, error) {
	var userID, status, storedLease string
	var leaseUntil sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT user_id,status,lease_token_hash,lease_until FROM execution_tasks WHERE id=? AND node_id=?`, taskID, nodeID).
		Scan(&userID, &status, &storedLease, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionTask{}, domain.ErrNotFound
	}
	if err != nil {
		return ExecutionTask{}, err
	}
	if storedLease == "" || storedLease != leaseHash || !leaseUntil.Valid || !leaseUntil.Time.After(now.UTC()) {
		return ExecutionTask{}, domain.ErrUnauthorized
	}
	if status != "leased" && status != "accepted" && status != "running" {
		return ExecutionTask{}, domain.ErrConflict
	}
	return s.ExecutionTask(ctx, userID, taskID)
}

// ValidateExecutionTaskRecoveryLease grants artifact-evidence upload only
// after the active lease has expired and the task is awaiting reconciliation.
func (s *Store) ValidateExecutionTaskRecoveryLease(ctx context.Context, nodeID, taskID, leaseHash string) (ExecutionTask, error) {
	var userID, status, storedRecoveryLease, activeLease string
	var leaseUntil sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT user_id,status,recovery_lease_token_hash,lease_token_hash,lease_until FROM execution_tasks WHERE id=? AND node_id=?`, taskID, nodeID).
		Scan(&userID, &status, &storedRecoveryLease, &activeLease, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionTask{}, domain.ErrNotFound
	}
	if err != nil {
		return ExecutionTask{}, err
	}
	if status != "needs_reconciliation" || activeLease != "" || leaseUntil.Valid || storedRecoveryLease == "" || storedRecoveryLease != leaseHash {
		return ExecutionTask{}, domain.ErrUnauthorized
	}
	return s.ExecutionTask(ctx, userID, taskID)
}

func (s *Store) AttachExecutionTaskRecoveryEvidence(ctx context.Context, nodeID, taskID, recoveryLeaseHash, artifactID, role string, now time.Time) error {
	if !validExecutionTaskRecoveryEvidenceRole(role) || artifactID == "" || recoveryLeaseHash == "" {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID, status, storedRecoveryLease, activeLease string
	var sequence int64
	var leaseUntil sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT user_id,status,recovery_lease_token_hash,lease_token_hash,lease_until,sequence FROM execution_tasks WHERE id=? AND node_id=?`, taskID, nodeID).
		Scan(&userID, &status, &storedRecoveryLease, &activeLease, &leaseUntil, &sequence); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	if status != "needs_reconciliation" || activeLease != "" || leaseUntil.Valid || storedRecoveryLease == "" || storedRecoveryLease != recoveryLeaseHash {
		return domain.ErrUnauthorized
	}
	if _, err := artifactByID(ctx, tx, userID, artifactID); err != nil {
		return err
	}
	var existing int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM execution_task_artifacts WHERE task_id=? AND artifact_id=? AND role=?`, taskID, artifactID, role).Scan(&existing)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return err
		}
		persisted, err := s.ExecutionTask(ctx, userID, taskID)
		if err != nil {
			return err
		}
		items, err := s.ExecutionTaskArtifacts(ctx, userID, taskID)
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.ID == artifactID && item.Role == role && persisted.Status == "needs_reconciliation" && persisted.LeaseUntil == nil {
				return nil
			}
		}
		return domain.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := attachExecutionTaskArtifactsTx(ctx, tx, userID, taskID, []ExecutionTaskArtifactLink{{ArtifactID: artifactID, Role: role}}, now); err != nil {
		return err
	}
	next := sequence + 1
	updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET sequence=?,updated_at=? WHERE id=? AND user_id=? AND node_id=? AND status='needs_reconciliation' AND recovery_lease_token_hash=? AND lease_token_hash='' AND lease_until IS NULL AND sequence=?`, next, now.UTC(), taskID, userID, nodeID, recoveryLeaseHash, sequence)
	if err != nil {
		return err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return domain.ErrConflict
	}
	details, err := json.Marshal(map[string]string{"artifactId": artifactID, "role": role, "reason": "expired_lease_evidence"})
	if err != nil {
		return err
	}
	if err := insertExecutionTaskEvent(ctx, tx, taskID, next, "recovery_evidence_attached", details, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	persisted, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return err
	}
	items, err := s.ExecutionTaskArtifacts(ctx, userID, taskID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID == artifactID && item.Role == role && persisted.Status == "needs_reconciliation" && persisted.Sequence == next && persisted.LeaseUntil == nil {
			return nil
		}
	}
	return domain.ErrConflict
}

// SaveExecutionTaskRecoveryResult stores a verified evidence manifest separately
// from Result. Recovery evidence never turns an uncertain task into success.
func (s *Store) SaveExecutionTaskRecoveryResult(ctx context.Context, nodeID, taskID, recoveryLeaseHash string, result json.RawMessage, now time.Time) (ExecutionTask, error) {
	if len(result) == 0 || len(result) > 1<<20 || !json.Valid(result) || recoveryLeaseHash == "" {
		return ExecutionTask{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTask{}, err
	}
	defer tx.Rollback()
	var userID, status, storedRecoveryLease, activeLease string
	var sequence int64
	var leaseUntil sql.NullTime
	var existing []byte
	if err := tx.QueryRowContext(ctx, `SELECT user_id,status,recovery_lease_token_hash,lease_token_hash,lease_until,sequence,recovery_result_json FROM execution_tasks WHERE id=? AND node_id=?`, taskID, nodeID).Scan(&userID, &status, &storedRecoveryLease, &activeLease, &leaseUntil, &sequence, &existing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, domain.ErrNotFound
		}
		return ExecutionTask{}, err
	}
	if status != "needs_reconciliation" || activeLease != "" || leaseUntil.Valid || storedRecoveryLease == "" || storedRecoveryLease != recoveryLeaseHash {
		return ExecutionTask{}, domain.ErrUnauthorized
	}
	if len(existing) > 0 {
		if string(existing) != string(result) {
			return ExecutionTask{}, domain.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return ExecutionTask{}, err
		}
		persisted, err := s.ExecutionTask(ctx, userID, taskID)
		if err != nil {
			return ExecutionTask{}, err
		}
		if persisted.Status != "needs_reconciliation" || string(persisted.RecoveryResult) != string(result) {
			return ExecutionTask{}, domain.ErrConflict
		}
		return persisted, nil
	}
	var manifest struct {
		ConversationID     string `json:"conversationId"`
		AgentTurnID        string `json:"agentTurnId"`
		TranscriptArtifact *struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"transcriptArtifact"`
		HandoffCheckpointArtifact *struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"handoffCheckpointArtifact"`
	}
	if err := json.Unmarshal(result, &manifest); err != nil || manifest.TranscriptArtifact == nil || manifest.TranscriptArtifact.ID == "" || len(manifest.TranscriptArtifact.SHA256) != 64 || manifest.TranscriptArtifact.ByteSize <= 0 {
		return ExecutionTask{}, domain.ErrInvalid
	}
	var linked int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_task_artifacts ta JOIN artifacts a ON a.id=ta.artifact_id WHERE ta.task_id=? AND ta.artifact_id=? AND ta.role='recovery_transcript' AND a.sha256=? AND a.byte_size=?`, taskID, manifest.TranscriptArtifact.ID, manifest.TranscriptArtifact.SHA256, manifest.TranscriptArtifact.ByteSize).Scan(&linked); err != nil {
		return ExecutionTask{}, err
	}
	if linked != 1 {
		return ExecutionTask{}, domain.ErrConflict
	}
	if manifest.HandoffCheckpointArtifact != nil {
		checkpoint := manifest.HandoffCheckpointArtifact
		if manifest.ConversationID == "" || manifest.AgentTurnID == "" || checkpoint.ID == "" || len(checkpoint.SHA256) != 64 || checkpoint.ByteSize <= 0 {
			return ExecutionTask{}, domain.ErrInvalid
		}
		var checkpointLinked int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_task_artifacts ta JOIN artifacts a ON a.id=ta.artifact_id WHERE ta.task_id=? AND ta.artifact_id=? AND ta.role='recovery_checkpoint' AND a.sha256=? AND a.byte_size=?`, taskID, checkpoint.ID, checkpoint.SHA256, checkpoint.ByteSize).Scan(&checkpointLinked); err != nil {
			return ExecutionTask{}, err
		}
		if checkpointLinked != 1 {
			return ExecutionTask{}, domain.ErrConflict
		}
		var reboundTurnID, reboundConversationID string
		if err := tx.QueryRowContext(ctx, `SELECT source_turn_id,source_conversation_id FROM execution_task_handoff_checkpoints WHERE task_id=? AND user_id=?`, taskID, userID).Scan(&reboundTurnID, &reboundConversationID); errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, fmt.Errorf("recovery checkpoint was not rebound before publishing its manifest: %w", domain.ErrConflict)
		} else if err != nil {
			return ExecutionTask{}, err
		}
		if reboundTurnID != manifest.AgentTurnID || reboundConversationID != manifest.ConversationID {
			return ExecutionTask{}, domain.ErrConflict
		}
	}
	next := sequence + 1
	updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET recovery_result_json=?,sequence=?,updated_at=? WHERE id=? AND user_id=? AND node_id=? AND status='needs_reconciliation' AND recovery_lease_token_hash=? AND lease_token_hash='' AND lease_until IS NULL AND recovery_result_json IS NULL AND sequence=?`, []byte(result), next, now.UTC(), taskID, userID, nodeID, recoveryLeaseHash, sequence)
	if err != nil {
		return ExecutionTask{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return ExecutionTask{}, err
	}
	if changed != 1 {
		return ExecutionTask{}, domain.ErrConflict
	}
	details, err := json.Marshal(map[string]string{"source": "expired_lease_outbox", "transcriptArtifactId": manifest.TranscriptArtifact.ID})
	if err != nil {
		return ExecutionTask{}, err
	}
	if err := insertExecutionTaskEvent(ctx, tx, taskID, next, "recovery_result_verified", details, now); err != nil {
		return ExecutionTask{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTask{}, err
	}
	persisted, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if persisted.Status != "needs_reconciliation" || string(persisted.RecoveryResult) != string(result) || persisted.Sequence != next {
		return ExecutionTask{}, domain.ErrConflict
	}
	return persisted, nil
}

func validExecutionTaskRecoveryEvidenceRole(role string) bool {
	switch role {
	case "recovery_transcript", "recovery_project_delta", "recovery_project_lfs_objects", "recovery_project_submodule_deltas", "recovery_checkpoint":
		return true
	default:
		return false
	}
}

func (s *Store) ReportExecutionTask(ctx context.Context, nodeID, taskID, leaseHash, nextStatus string, result json.RawMessage, errorText string, now time.Time) (ExecutionTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTask{}, err
	}
	defer tx.Rollback()
	var userID, currentStatus string
	var currentSequence int64
	var cancelRequested int
	var storedLease string
	var leaseUntil sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT user_id,status,sequence,lease_token_hash,lease_until,cancel_requested FROM execution_tasks WHERE id=? AND node_id=?`, taskID, nodeID).Scan(&userID, &currentStatus, &currentSequence, &storedLease, &leaseUntil, &cancelRequested); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, domain.ErrNotFound
		}
		return ExecutionTask{}, err
	}
	if storedLease == "" || storedLease != leaseHash {
		return ExecutionTask{}, domain.ErrUnauthorized
	}
	if !leaseUntil.Valid || !leaseUntil.Time.After(now.UTC()) {
		return ExecutionTask{}, domain.ErrConflict
	}
	if !validTaskTransition(currentStatus, nextStatus) || (nextStatus == "cancelled" && cancelRequested == 0) {
		return ExecutionTask{}, domain.ErrConflict
	}
	if cancelRequested != 0 && currentStatus != "running" && nextStatus == "running" {
		return ExecutionTask{}, domain.ErrConflict
	}
	next := currentSequence + 1
	var resultValue any
	if len(result) > 0 {
		resultValue = []byte(result)
	}
	if len(result) > 0 && !json.Valid(result) {
		return ExecutionTask{}, domain.ErrInvalid
	}
	if nextStatus == "reported_succeeded" && len(result) == 0 {
		return ExecutionTask{}, domain.ErrInvalid
	}
	if nextStatus != "reported_succeeded" && len(result) > 0 {
		return ExecutionTask{}, domain.ErrInvalid
	}
	if (nextStatus == "reported_failed" || nextStatus == "needs_reconciliation") && strings.TrimSpace(errorText) == "" {
		return ExecutionTask{}, domain.ErrInvalid
	}
	if nextStatus != "reported_failed" && nextStatus != "needs_reconciliation" {
		errorText = ""
	}
	if len(errorText) > 4000 {
		errorText = errorText[:4000]
	}
	terminal := nextStatus == "reported_succeeded" || nextStatus == "reported_failed" || nextStatus == "needs_reconciliation" || nextStatus == "cancelled"
	terminalInt := 0
	if terminal {
		terminalInt = 1
	}
	updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET status=?,result_json=?,error_text=?,sequence=?,updated_at=?,lease_token_hash=CASE WHEN ?=1 THEN '' ELSE lease_token_hash END,lease_until=CASE WHEN ?=1 THEN NULL ELSE lease_until END WHERE id=? AND node_id=? AND sequence=? AND lease_token_hash=?`, nextStatus, resultValue, errorText, next, now.UTC(), terminalInt, terminalInt, taskID, nodeID, currentSequence, leaseHash)
	if err != nil {
		return ExecutionTask{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return ExecutionTask{}, err
	}
	if changed != 1 {
		return ExecutionTask{}, domain.ErrConflict
	}
	details, err := json.Marshal(map[string]any{"status": nextStatus, "resultBytes": len(result)})
	if err != nil {
		return ExecutionTask{}, err
	}
	if err := insertExecutionTaskEvent(ctx, tx, taskID, next, nextStatus, details, now); err != nil {
		return ExecutionTask{}, err
	}
	task, err := executionTaskByID(ctx, tx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if task.Status != nextStatus || task.Sequence != next || string(task.Result) != string(result) || task.Error != errorText {
		return ExecutionTask{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTask{}, err
	}
	persisted, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if persisted.Status != nextStatus || persisted.Sequence != next || string(persisted.Result) != string(result) || persisted.Error != errorText {
		return ExecutionTask{}, domain.ErrConflict
	}
	return persisted, nil
}

func validTaskTransition(current, next string) bool {
	switch current {
	case "leased":
		return next == "accepted" || next == "cancelled" || next == "needs_reconciliation"
	case "accepted":
		return next == "running" || next == "cancelled" || next == "needs_reconciliation"
	case "running":
		return next == "reported_succeeded" || next == "reported_failed" || next == "cancelled" || next == "needs_reconciliation"
	default:
		return false
	}
}

func (s *Store) CompleteAgentExecutionTask(ctx context.Context, nodeID, taskID, leaseHash, turnID string, result json.RawMessage, now time.Time) (ExecutionTask, error) {
	if !json.Valid(result) || len(result) == 0 || turnID == "" {
		return ExecutionTask{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTask{}, err
	}
	defer tx.Rollback()
	var userID, status, storedLease string
	var sequence int64
	var leaseUntil sql.NullTime
	var cancelRequested int
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT user_id,status,sequence,lease_token_hash,lease_until,cancel_requested,payload_json FROM execution_tasks WHERE id=? AND node_id=?`, taskID, nodeID).Scan(&userID, &status, &sequence, &storedLease, &leaseUntil, &cancelRequested, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, domain.ErrNotFound
		}
		return ExecutionTask{}, err
	}
	if status != "running" || storedLease == "" || storedLease != leaseHash || !leaseUntil.Valid || !leaseUntil.Time.After(now.UTC()) || cancelRequested != 0 {
		return ExecutionTask{}, domain.ErrConflict
	}
	var taskInput struct {
		ConversationID string `json:"conversationId"`
	}
	if err := json.Unmarshal(payload, &taskInput); err != nil || taskInput.ConversationID == "" {
		return ExecutionTask{}, domain.ErrInvalid
	}
	var turnUserID, turnConversationID, turnStatus string
	if err := tx.QueryRowContext(ctx, `SELECT user_id,conversation_id,status FROM agent_turns WHERE id=?`, turnID).Scan(&turnUserID, &turnConversationID, &turnStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, domain.ErrNotFound
		}
		return ExecutionTask{}, err
	}
	if turnUserID != userID || turnConversationID != taskInput.ConversationID || turnStatus != "completed" {
		return ExecutionTask{}, domain.ErrConflict
	}
	next := sequence + 1
	updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET status='completed',result_json=?,error_text='',sequence=?,updated_at=?,lease_token_hash='',lease_until=NULL WHERE id=? AND node_id=? AND status='running' AND sequence=? AND lease_token_hash=? AND cancel_requested=0`, []byte(result), next, now.UTC(), taskID, nodeID, sequence, leaseHash)
	if err != nil {
		return ExecutionTask{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return ExecutionTask{}, err
	}
	if changed != 1 {
		return ExecutionTask{}, domain.ErrConflict
	}
	details, err := json.Marshal(map[string]string{"agentTurnId": turnID, "status": "completed"})
	if err != nil {
		return ExecutionTask{}, err
	}
	if err := insertExecutionTaskEvent(ctx, tx, taskID, next, "agent_turn.verified", details, now); err != nil {
		return ExecutionTask{}, err
	}
	readBack, err := executionTaskByID(ctx, tx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if readBack.Status != "completed" || readBack.Sequence != next || string(readBack.Result) != string(result) || readBack.LeaseUntil != nil {
		return ExecutionTask{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTask{}, err
	}
	persisted, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if persisted.Status != "completed" || persisted.Sequence != next || string(persisted.Result) != string(result) || persisted.LeaseUntil != nil {
		return ExecutionTask{}, domain.ErrConflict
	}
	return persisted, nil
}

// PauseAgentExecutionTask records a cloud Agent budget pause as an explicit,
// resumable non-complete terminal state. Only a durable, available checkpoint
// with a safe stop reason can use this path; arbitrary node reports cannot set
// this status through ReportExecutionTask.
func (s *Store) PauseAgentExecutionTask(ctx context.Context, nodeID, taskID, leaseHash, turnID string, now time.Time) (ExecutionTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTask{}, err
	}
	defer tx.Rollback()
	var userID, status, storedLease string
	var sequence int64
	var leaseUntil sql.NullTime
	var cancelRequested int
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT user_id,status,sequence,lease_token_hash,lease_until,cancel_requested,payload_json FROM execution_tasks WHERE id=? AND node_id=?`, taskID, nodeID).Scan(&userID, &status, &sequence, &storedLease, &leaseUntil, &cancelRequested, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, domain.ErrNotFound
		}
		return ExecutionTask{}, err
	}
	if status != "running" || storedLease == "" || storedLease != leaseHash || !leaseUntil.Valid || !leaseUntil.Time.After(now.UTC()) || cancelRequested != 0 {
		return ExecutionTask{}, domain.ErrConflict
	}
	var taskInput struct {
		Kind           string `json:"kind"`
		ConversationID string `json:"conversationId"`
		Content        string `json:"content"`
		TurnID         string `json:"turnId"`
	}
	if err := json.Unmarshal(payload, &taskInput); err != nil || taskInput.ConversationID == "" || taskInput.Content == "" || (taskInput.Kind != "agent_turn" && taskInput.Kind != "agent_continuation") || (taskInput.Kind == "agent_continuation" && taskInput.TurnID == "") {
		return ExecutionTask{}, domain.ErrInvalid
	}
	var turnUserID, turnConversationID, turnStatus, stopReason, resultMessageID, outputRole, snapshotStatus, inputContent, continuedFromTurnID string
	if err := tx.QueryRowContext(ctx, `SELECT t.user_id,t.conversation_id,t.status,t.stop_reason,COALESCE(t.result_message_id,''),COALESCE(m.role,''),COALESCE(s.status,''),t.input_content_snapshot,t.continued_from_turn_id
FROM agent_turns t LEFT JOIN messages m ON m.id=t.result_message_id AND m.conversation_id=t.conversation_id
LEFT JOIN agent_continuation_snapshots s ON s.source_turn_id=t.id AND s.user_id=t.user_id
WHERE t.id=?`, turnID).Scan(&turnUserID, &turnConversationID, &turnStatus, &stopReason, &resultMessageID, &outputRole, &snapshotStatus, &inputContent, &continuedFromTurnID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, domain.ErrNotFound
		}
		return ExecutionTask{}, err
	}
	continuationSourceMatches := taskInput.Kind == "agent_turn" && continuedFromTurnID == "" || taskInput.Kind == "agent_continuation" && continuedFromTurnID == taskInput.TurnID
	if turnUserID != userID || turnConversationID != taskInput.ConversationID || turnStatus != "incomplete" || !domain.SafeAgentContinuationStopReason(stopReason) || resultMessageID == "" || outputRole != "assistant" || snapshotStatus != "available" || inputContent != taskInput.Content || !continuationSourceMatches {
		return ExecutionTask{}, domain.ErrConflict
	}
	result, err := json.Marshal(map[string]string{"agentTurnId": turnID, "conversationId": turnConversationID, "resultMessageId": resultMessageID, "agentStatus": turnStatus, "agentStopReason": stopReason, "continuationStatus": snapshotStatus})
	if err != nil {
		return ExecutionTask{}, err
	}
	next := sequence + 1
	updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET status='incomplete',result_json=?,error_text='',sequence=?,updated_at=?,lease_token_hash='',lease_until=NULL WHERE id=? AND node_id=? AND status='running' AND sequence=? AND lease_token_hash=? AND cancel_requested=0`, result, next, now.UTC(), taskID, nodeID, sequence, leaseHash)
	if err != nil {
		return ExecutionTask{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return ExecutionTask{}, err
	}
	if changed != 1 {
		return ExecutionTask{}, domain.ErrConflict
	}
	details, err := json.Marshal(map[string]string{"agentTurnId": turnID, "status": "incomplete", "stopReason": stopReason, "continuationStatus": snapshotStatus})
	if err != nil {
		return ExecutionTask{}, err
	}
	if err := insertExecutionTaskEvent(ctx, tx, taskID, next, "agent_turn.incomplete", details, now); err != nil {
		return ExecutionTask{}, err
	}
	readBack, err := executionTaskByID(ctx, tx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if readBack.Status != "incomplete" || readBack.Sequence != next || string(readBack.Result) != string(result) || readBack.Error != "" || readBack.LeaseUntil != nil {
		return ExecutionTask{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTask{}, err
	}
	persisted, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if persisted.Status != "incomplete" || persisted.Sequence != next || string(persisted.Result) != string(result) || persisted.Error != "" || persisted.LeaseUntil != nil {
		return ExecutionTask{}, domain.ErrConflict
	}
	return persisted, nil
}

func (s *Store) CancelExecutionTask(ctx context.Context, userID, taskID string, now time.Time) (ExecutionTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTask{}, err
	}
	defer tx.Rollback()
	task, err := executionTaskByID(ctx, tx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if task.Status == "completed" || task.Status == "incomplete" || task.Status == "reported_succeeded" || task.Status == "reported_failed" || task.Status == "cancelled" || task.Status == "needs_reconciliation" {
		if err := tx.Commit(); err != nil {
			return ExecutionTask{}, err
		}
		return s.ExecutionTask(ctx, userID, taskID)
	}
	if task.CancelRequested {
		if err := tx.Commit(); err != nil {
			return ExecutionTask{}, err
		}
		return s.ExecutionTask(ctx, userID, taskID)
	}
	next := task.Sequence + 1
	status := task.Status
	cancelRequested := 1
	kind := "cancel_requested"
	if task.Status == "queued" {
		status = "cancelled"
		cancelRequested = 1
		kind = "cancelled"
	}
	updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET status=?,cancel_requested=?,sequence=?,updated_at=?,lease_token_hash=CASE WHEN ?='cancelled' THEN '' ELSE lease_token_hash END,lease_until=CASE WHEN ?='cancelled' THEN NULL ELSE lease_until END WHERE id=? AND user_id=? AND sequence=? AND status=?`, status, cancelRequested, next, now.UTC(), status, status, taskID, userID, task.Sequence, task.Status)
	if err != nil {
		return ExecutionTask{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return ExecutionTask{}, err
	}
	if changed != 1 {
		return ExecutionTask{}, domain.ErrConflict
	}
	details := []byte(`{"requested":true}`)
	if err := insertExecutionTaskEvent(ctx, tx, taskID, next, kind, details, now); err != nil {
		return ExecutionTask{}, err
	}
	readBack, err := executionTaskByID(ctx, tx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if readBack.Status != status || !readBack.CancelRequested || readBack.Sequence != next {
		return ExecutionTask{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTask{}, err
	}
	persisted, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if persisted.Status != status || !persisted.CancelRequested || persisted.Sequence != next {
		return ExecutionTask{}, domain.ErrConflict
	}
	return persisted, nil
}

func (s *Store) PulseExecutionTask(ctx context.Context, nodeID, taskID, leaseHash string, now time.Time) (ExecutionTask, error) {
	return s.PulseExecutionTaskWithPhase(ctx, nodeID, taskID, leaseHash, "", now)
}

// PulseExecutionTaskWithPhase renews an active node lease and, when the
// execution phase changes, appends the phase event in the same transaction.
// The event log is the durable source of truth for the user-visible phase.
func (s *Store) PulseExecutionTaskWithPhase(ctx context.Context, nodeID, taskID, leaseHash, phase string, now time.Time) (ExecutionTask, error) {
	phase = strings.TrimSpace(phase)
	if phase != "" && !validExecutionTaskProgressPhase(phase) {
		return ExecutionTask{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTask{}, err
	}
	defer tx.Rollback()
	var userID, status, storedHash string
	var attempt int
	var leaseUntil sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT user_id,status,lease_token_hash,lease_until,attempt FROM execution_tasks WHERE id=? AND node_id=?`, taskID, nodeID).Scan(&userID, &status, &storedHash, &leaseUntil, &attempt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, domain.ErrNotFound
		}
		return ExecutionTask{}, err
	}
	if storedHash == "" || storedHash != leaseHash {
		return ExecutionTask{}, domain.ErrUnauthorized
	}
	if !leaseUntil.Valid || !leaseUntil.Time.After(now.UTC()) {
		return ExecutionTask{}, domain.ErrConflict
	}
	if status != "leased" && status != "accepted" && status != "running" {
		return ExecutionTask{}, domain.ErrConflict
	}
	currentPhase, _, err := latestExecutionTaskProgressPhase(ctx, tx, taskID, attempt)
	if err != nil {
		return ExecutionTask{}, err
	}
	if phase != "" && phase != currentPhase {
		if executionTaskProgressPhaseRank(phase) < executionTaskProgressPhaseRank(currentPhase) {
			return ExecutionTask{}, fmt.Errorf("task progress phase cannot move backward from %q to %q: %w", currentPhase, phase, domain.ErrConflict)
		}
		if phase == "safe_boundary_reached" {
			var requested int
			if err := tx.QueryRowContext(ctx, `SELECT handoff_requested FROM execution_tasks WHERE id=? AND user_id=?`, taskID, userID).Scan(&requested); err != nil {
				return ExecutionTask{}, err
			}
			if requested == 0 {
				return ExecutionTask{}, fmt.Errorf("safe boundary phase requires a durable handoff request: %w", domain.ErrConflict)
			}
		}
	}
	newUntil := now.UTC().Add(60 * time.Second)
	updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET lease_until=?,updated_at=? WHERE id=? AND node_id=? AND lease_token_hash=? AND status=?`, newUntil, now.UTC(), taskID, nodeID, leaseHash, status)
	if err != nil {
		return ExecutionTask{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return ExecutionTask{}, err
	}
	if changed != 1 {
		return ExecutionTask{}, domain.ErrConflict
	}
	sequence := int64(0)
	if phase != "" && phase != currentPhase {
		var currentSequence int64
		if err := tx.QueryRowContext(ctx, `SELECT sequence FROM execution_tasks WHERE id=? AND user_id=?`, taskID, userID).Scan(&currentSequence); err != nil {
			return ExecutionTask{}, err
		}
		sequence = currentSequence + 1
		updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET sequence=?,updated_at=? WHERE id=? AND user_id=? AND sequence=?`, sequence, now.UTC(), taskID, userID, currentSequence)
		if err != nil {
			return ExecutionTask{}, err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return ExecutionTask{}, err
		}
		if changed != 1 {
			return ExecutionTask{}, domain.ErrConflict
		}
		details, err := json.Marshal(map[string]any{"attempt": attempt, "phase": phase})
		if err != nil {
			return ExecutionTask{}, err
		}
		if err := insertExecutionTaskEvent(ctx, tx, taskID, sequence, "phase", details, now); err != nil {
			return ExecutionTask{}, err
		}
	}
	task, err := executionTaskByID(ctx, tx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if task.LeaseUntil == nil || !task.LeaseUntil.Equal(newUntil) {
		return ExecutionTask{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTask{}, err
	}
	persisted, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if persisted.LeaseUntil == nil || !persisted.LeaseUntil.Equal(newUntil) {
		return ExecutionTask{}, domain.ErrConflict
	}
	return persisted, nil
}

func validExecutionTaskProgressPhase(phase string) bool {
	switch phase {
	case "preparing", "waiting_for_safe_boundary", "agent_and_snapshot", "safe_boundary_reached", "local_result_durable", "uploading_artifacts", "artifacts_verified":
		return true
	default:
		return false
	}
}

func executionTaskProgressPhaseRank(phase string) int {
	switch phase {
	case "":
		return 0
	case "preparing":
		return 1
	case "agent_and_snapshot":
		return 2
	case "waiting_for_safe_boundary":
		return 3
	case "safe_boundary_reached":
		return 4
	case "local_result_durable":
		return 5
	case "uploading_artifacts":
		return 6
	case "artifacts_verified":
		return 7
	default:
		return -1
	}
}

func latestExecutionTaskProgressPhase(ctx context.Context, q taskQueryer, taskID string, attempt int) (string, time.Time, error) {
	var details []byte
	var updated time.Time
	err := q.QueryRowContext(ctx, `SELECT details_json,created_at FROM execution_task_events WHERE task_id=? AND kind='phase' ORDER BY sequence DESC LIMIT 1`, taskID).Scan(&details, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, err
	}
	var event struct {
		Attempt int    `json:"attempt"`
		Phase   string `json:"phase"`
	}
	if err := json.Unmarshal(details, &event); err != nil || !validExecutionTaskProgressPhase(event.Phase) {
		return "", time.Time{}, fmt.Errorf("durable task phase event is invalid: %w", domain.ErrConflict)
	}
	if event.Attempt != attempt {
		return "", time.Time{}, nil
	}
	return event.Phase, updated, nil
}

// RequestExecutionTaskHandoff requests a lease-owning local node to pause at
// the next safe Agent boundary. The request never transfers the lease or
// cancels active work.
func (s *Store) RequestExecutionTaskHandoff(ctx context.Context, userID, taskID string, now time.Time) (ExecutionTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTask{}, err
	}
	defer tx.Rollback()
	var nodeID, status string
	var sequence int64
	var leaseUntil sql.NullTime
	var cancelRequested, handoffRequested int
	var capabilities []byte
	if err := tx.QueryRowContext(ctx, `SELECT t.node_id,t.status,t.sequence,t.lease_until,t.cancel_requested,t.handoff_requested,n.capabilities_json FROM execution_tasks t JOIN execution_nodes n ON n.id=t.node_id WHERE t.id=? AND t.user_id=?`, taskID, userID).Scan(&nodeID, &status, &sequence, &leaseUntil, &cancelRequested, &handoffRequested, &capabilities); errors.Is(err, sql.ErrNoRows) {
		return ExecutionTask{}, domain.ErrNotFound
	} else if err != nil {
		return ExecutionTask{}, err
	}
	var nodeCapabilities []string
	capable := false
	if json.Unmarshal(capabilities, &nodeCapabilities) == nil {
		for _, capability := range nodeCapabilities {
			if capability == "safe-handoff" {
				capable = true
				break
			}
		}
	}
	if !capable {
		return ExecutionTask{}, fmt.Errorf("execution node does not advertise safe handoff support: %w", domain.ErrConflict)
	}
	if handoffRequested != 0 {
		if err := tx.Commit(); err != nil {
			return ExecutionTask{}, err
		}
		readBack, err := s.ExecutionTask(ctx, userID, taskID)
		if err != nil {
			return ExecutionTask{}, err
		}
		if !readBack.HandoffRequested || readBack.NodeID != nodeID || readBack.Sequence < sequence {
			return ExecutionTask{}, domain.ErrConflict
		}
		return readBack, nil
	}
	if !leaseUntil.Valid || !leaseUntil.Time.After(now.UTC()) || (status != "leased" && status != "accepted" && status != "running") || cancelRequested != 0 {
		return ExecutionTask{}, domain.ErrConflict
	}
	next := sequence + 1
	updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET handoff_requested=1,sequence=?,updated_at=? WHERE id=? AND user_id=? AND status=? AND sequence=? AND handoff_requested=0 AND cancel_requested=0 AND lease_until>?`, next, now.UTC(), taskID, userID, status, sequence, now.UTC())
	if err != nil {
		return ExecutionTask{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return ExecutionTask{}, err
	}
	if changed != 1 {
		return ExecutionTask{}, domain.ErrConflict
	}
	details, err := json.Marshal(map[string]string{"requestedBy": "user", "behavior": "pause_at_safe_boundary"})
	if err != nil {
		return ExecutionTask{}, err
	}
	if err := insertExecutionTaskEvent(ctx, tx, taskID, next, "handoff_requested", details, now); err != nil {
		return ExecutionTask{}, err
	}
	sequence = next
	if err := tx.Commit(); err != nil {
		return ExecutionTask{}, err
	}
	readBack, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if !readBack.HandoffRequested || readBack.Sequence < sequence || readBack.NodeID != nodeID {
		return ExecutionTask{}, domain.ErrConflict
	}
	return readBack, nil
}

func (s *Store) ReconcileExpiredExecutionTaskLeases(ctx context.Context, now time.Time) (int, error) {
	return s.reconcileExpiredExecutionTaskLeases(ctx, "", now)
}

// ReconcileExpiredExecutionTaskLeasesForUser limits lease recovery to one
// owner so a per-user worker cannot change another account's task state.
func (s *Store) ReconcileExpiredExecutionTaskLeasesForUser(ctx context.Context, userID string, now time.Time) (int, error) {
	if strings.TrimSpace(userID) == "" {
		return 0, domain.ErrInvalid
	}
	return s.reconcileExpiredExecutionTaskLeases(ctx, userID, now)
}

func (s *Store) reconcileExpiredExecutionTaskLeases(ctx context.Context, userID string, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	query := `SELECT id,user_id,status,sequence,lease_token_hash FROM execution_tasks WHERE status IN ('leased','accepted','running') AND lease_until<=?`
	args := []any{now.UTC()}
	if userID != "" {
		query += ` AND user_id=?`
		args = append(args, userID)
	}
	query += ` ORDER BY lease_until,id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	type expiredTask struct {
		id, userID, status, leaseHash string
		sequence                      int64
	}
	var expired []expiredTask
	for rows.Next() {
		var task expiredTask
		if err := rows.Scan(&task.id, &task.userID, &task.status, &task.sequence, &task.leaseHash); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, task)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil {
		return 0, rowsErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	for _, task := range expired {
		next := task.sequence + 1
		updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET status='needs_reconciliation',recovery_lease_token_hash=lease_token_hash,lease_token_hash='',lease_until=NULL,error_text='node lease expired; execution outcome must be checked',sequence=?,updated_at=? WHERE id=? AND user_id=? AND status=? AND sequence=? AND lease_token_hash=? AND lease_until<=?`, next, now.UTC(), task.id, task.userID, task.status, task.sequence, task.leaseHash, now.UTC())
		if err != nil {
			return 0, err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return 0, err
		}
		if changed != 1 {
			return 0, domain.ErrConflict
		}
		var status string
		var sequence int64
		var leaseHash, recoveryLeaseHash string
		var lease sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT status,sequence,lease_token_hash,recovery_lease_token_hash,lease_until FROM execution_tasks WHERE id=?`, task.id).Scan(&status, &sequence, &leaseHash, &recoveryLeaseHash, &lease); err != nil {
			return 0, err
		}
		if status != "needs_reconciliation" || sequence != next || leaseHash != "" || recoveryLeaseHash != task.leaseHash || lease.Valid {
			return 0, domain.ErrConflict
		}
		details := []byte(`{"reason":"lease_expired"}`)
		if err := insertExecutionTaskEvent(ctx, tx, task.id, next, status, details, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, task := range expired {
		readBack, err := s.ExecutionTask(ctx, task.userID, task.id)
		if err != nil {
			return len(expired), err
		}
		if readBack.Status != "needs_reconciliation" || readBack.Sequence != task.sequence+1 || readBack.LeaseUntil != nil {
			return len(expired), domain.ErrConflict
		}
	}
	return len(expired), nil
}

func (s *Store) ExecutionTask(ctx context.Context, userID, taskID string) (ExecutionTask, error) {
	task, err := executionTaskByID(ctx, s.db, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if err := s.hydrateExecutionTaskProgress(ctx, &task); err != nil {
		return ExecutionTask{}, err
	}
	return task, nil
}

// ExecutionTaskByIdempotencyKey returns the durable task bound to a user's
// submission key so callers can replay the original snapshot without
// regenerating time-varying artifacts from a later workspace state.
func (s *Store) ExecutionTaskByIdempotencyKey(ctx context.Context, userID, key string) (ExecutionTask, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(key) == "" {
		return ExecutionTask{}, domain.ErrInvalid
	}
	task, err := executionTaskByKey(ctx, s.db, userID, key)
	if err != nil {
		return ExecutionTask{}, err
	}
	if err := s.hydrateExecutionTaskProgress(ctx, &task); err != nil {
		return ExecutionTask{}, err
	}
	return task, nil
}

func (s *Store) ExecutionTasks(ctx context.Context, userID string) ([]ExecutionTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,logical_task_id,parent_task_id,segment_index,node_id,idempotency_key,payload_json,result_json,recovery_result_json,status,attempt,sequence,lease_until,cancel_requested,handoff_requested,error_text,created_at,updated_at FROM execution_tasks WHERE user_id=? ORDER BY created_at DESC,id DESC`, userID)
	if err != nil {
		return nil, err
	}
	var tasks []ExecutionTask
	for rows.Next() {
		task, err := scanExecutionTask(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range tasks {
		if err := s.hydrateExecutionTaskProgress(ctx, &tasks[index]); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

func (s *Store) hydrateExecutionTaskProgress(ctx context.Context, task *ExecutionTask) error {
	if task == nil {
		return domain.ErrInvalid
	}
	phase, updatedAt, err := latestExecutionTaskProgressPhase(ctx, s.db, task.ID, task.Attempt)
	if err != nil {
		return err
	}
	task.ProgressPhase = phase
	if phase != "" {
		task.ProgressUpdatedAt = &updatedAt
	}
	return nil
}

func (s *Store) ExecutionTaskEvents(ctx context.Context, userID, taskID string) ([]ExecutionTaskEvent, error) {
	if _, err := s.ExecutionTask(ctx, userID, taskID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT task_id,sequence,kind,details_json,created_at FROM execution_task_events WHERE task_id=? ORDER BY sequence`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]ExecutionTaskEvent, 0)
	for rows.Next() {
		var event ExecutionTaskEvent
		var details []byte
		if err := rows.Scan(&event.TaskID, &event.Sequence, &event.Kind, &details, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Details = append(json.RawMessage(nil), details...)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

type taskQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type taskScanner interface{ Scan(...any) error }

func executionTaskByKey(ctx context.Context, q taskQueryer, userID, key string) (ExecutionTask, error) {
	return scanExecutionTask(q.QueryRowContext(ctx, `SELECT id,user_id,logical_task_id,parent_task_id,segment_index,node_id,idempotency_key,payload_json,result_json,recovery_result_json,status,attempt,sequence,lease_until,cancel_requested,handoff_requested,error_text,created_at,updated_at FROM execution_tasks WHERE user_id=? AND idempotency_key=?`, userID, key))
}
func executionTaskByID(ctx context.Context, q taskQueryer, userID, id string) (ExecutionTask, error) {
	return scanExecutionTask(q.QueryRowContext(ctx, `SELECT id,user_id,logical_task_id,parent_task_id,segment_index,node_id,idempotency_key,payload_json,result_json,recovery_result_json,status,attempt,sequence,lease_until,cancel_requested,handoff_requested,error_text,created_at,updated_at FROM execution_tasks WHERE id=? AND user_id=?`, id, userID))
}
func scanExecutionTask(row taskScanner) (ExecutionTask, error) {
	var task ExecutionTask
	var payload, result, recoveryResult []byte
	var lease sql.NullTime
	var cancel, handoff int
	err := row.Scan(&task.ID, &task.UserID, &task.LogicalTaskID, &task.ParentTaskID, &task.SegmentIndex, &task.NodeID, &task.IdempotencyKey, &payload, &result, &recoveryResult, &task.Status, &task.Attempt, &task.Sequence, &lease, &cancel, &handoff, &task.Error, &task.CreatedAt, &task.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionTask{}, domain.ErrNotFound
	}
	if err != nil {
		return ExecutionTask{}, err
	}
	task.Payload = append(json.RawMessage(nil), payload...)
	if len(result) > 0 {
		task.Result = append(json.RawMessage(nil), result...)
	}
	if len(recoveryResult) > 0 {
		task.RecoveryResult = append(json.RawMessage(nil), recoveryResult...)
	}
	if lease.Valid {
		value := lease.Time
		task.LeaseUntil = &value
	}
	task.CancelRequested = cancel != 0
	task.HandoffRequested = handoff != 0
	return task, nil
}

func insertExecutionTaskEvent(ctx context.Context, tx *sql.Tx, taskID string, sequence int64, kind string, details []byte, now time.Time) error {
	if len(details) == 0 {
		details = []byte(`{}`)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_task_events(task_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?)`, taskID, sequence, kind, details, now.UTC()); err != nil {
		return err
	}
	var readKind string
	var readDetails []byte
	var readAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT kind,details_json,created_at FROM execution_task_events WHERE task_id=? AND sequence=?`, taskID, sequence).Scan(&readKind, &readDetails, &readAt); err != nil {
		return err
	}
	if readKind != kind || string(readDetails) != string(details) || !readAt.Equal(now.UTC()) {
		return domain.ErrConflict
	}
	return nil
}
