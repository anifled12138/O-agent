package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"axiom.local/agent/internal/domain"
)

func CloudExecutionNodeID(userID string) string {
	digest := sha256.Sum256([]byte("o-cloud-runtime-node:" + userID))
	return "node_cloud_" + hex.EncodeToString(digest[:8])
}

func (s *Store) EnsureCloudExecutionNode(ctx context.Context, userID string, now time.Time) (ExecutionNode, string, error) {
	nodeID := CloudExecutionNodeID(userID)
	tokenHashBytes := sha256.Sum256([]byte("o-cloud-runtime-credential:" + userID))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])
	want := ExecutionNode{ID: nodeID, UserID: userID, Name: "O Cloud Runtime", Platform: "linux", Capabilities: []string{"agent-runtime", "cloud-execution", "sandbox"}}
	node, err := s.ExecutionNode(ctx, userID, nodeID)
	if errors.Is(err, domain.ErrNotFound) {
		node, err = s.RegisterExecutionNode(ctx, want, tokenHash)
	}
	if err != nil {
		return ExecutionNode{}, "", err
	}
	if node.Name != want.Name || node.Platform != want.Platform || !equalStrings(node.Capabilities, want.Capabilities) || node.RevokedAt != nil {
		return ExecutionNode{}, "", domain.ErrConflict
	}
	if _, err := s.HeartbeatExecutionNode(ctx, tokenHash, want.Platform, want.Capabilities, now.UTC()); err != nil {
		return ExecutionNode{}, "", err
	}
	readBack, err := s.ExecutionNode(ctx, userID, nodeID)
	if err != nil {
		return ExecutionNode{}, "", err
	}
	if readBack.LastSeen == nil || !readBack.LastSeen.Equal(now.UTC()) || readBack.RevokedAt != nil {
		return ExecutionNode{}, "", domain.ErrConflict
	}
	return readBack, tokenHash, nil
}

type ExecutionNode struct {
	ID           string                 `json:"id"`
	UserID       string                 `json:"-"`
	Name         string                 `json:"name"`
	Platform     string                 `json:"platform"`
	Capabilities []string               `json:"capabilities"`
	Resources    ExecutionNodeResources `json:"resources"`
	LastSeen     *time.Time             `json:"lastSeen,omitempty"`
	RevokedAt    *time.Time             `json:"revokedAt,omitempty"`
	CreatedAt    time.Time              `json:"createdAt"`
	UpdatedAt    time.Time              `json:"updatedAt"`
}

// ExecutionNodeResources is the latest bounded resource snapshot reported by
// a node. Zero memory/CPU fields mean an older node client did not report
// metrics; MaxConcurrentTasks remains the authoritative admission ceiling.
type ExecutionNodeResources struct {
	MemoryTotalBytes     int64 `json:"memoryTotalBytes"`
	MemoryAvailableBytes int64 `json:"memoryAvailableBytes"`
	LogicalCPUs          int   `json:"logicalCpus"`
	MaxConcurrentTasks   int   `json:"maxConcurrentTasks"`
}

func (r ExecutionNodeResources) Validate() error {
	if r.MemoryTotalBytes < 0 || r.MemoryAvailableBytes < 0 || r.LogicalCPUs < 0 || r.MaxConcurrentTasks < 0 || r.MaxConcurrentTasks > domain.MaxLocalNodeWorkerConcurrency {
		return domain.ErrInvalid
	}
	if r.MemoryTotalBytes == 0 {
		if r.MemoryAvailableBytes != 0 || r.LogicalCPUs != 0 {
			return domain.ErrInvalid
		}
	} else if r.MemoryAvailableBytes > r.MemoryTotalBytes || r.LogicalCPUs < 1 {
		return domain.ErrInvalid
	} else if r.MaxConcurrentTasks > domain.LocalNodeTaskSlotsForCapacity(r.MemoryAvailableBytes, r.LogicalCPUs) {
		return domain.ErrInvalid
	}
	return nil
}

func (s *Store) RegisterExecutionNode(ctx context.Context, node ExecutionNode, tokenHash string) (ExecutionNode, error) {
	caps, err := json.Marshal(node.Capabilities)
	if err != nil {
		return ExecutionNode{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionNode{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_nodes(id,user_id,name,platform,capabilities_json,token_hash,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, node.ID, node.UserID, node.Name, node.Platform, caps, tokenHash, now, now); err != nil {
		return ExecutionNode{}, err
	}
	readBack, err := executionNodeByID(ctx, tx, node.UserID, node.ID)
	if err != nil {
		return ExecutionNode{}, err
	}
	if readBack.Name != node.Name || readBack.Platform != node.Platform || !equalStrings(readBack.Capabilities, node.Capabilities) {
		return ExecutionNode{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionNode{}, err
	}
	readBack, err = s.ExecutionNode(ctx, node.UserID, node.ID)
	if err != nil {
		return ExecutionNode{}, err
	}
	if readBack.Name != node.Name || readBack.Platform != node.Platform || !equalStrings(readBack.Capabilities, node.Capabilities) {
		return ExecutionNode{}, domain.ErrConflict
	}
	return readBack, nil
}

func (s *Store) ExecutionNodes(ctx context.Context, userID string) ([]ExecutionNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,name,platform,capabilities_json,resources_json,last_seen,revoked_at,created_at,updated_at FROM execution_nodes WHERE user_id=? ORDER BY created_at,id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []ExecutionNode
	for rows.Next() {
		var node ExecutionNode
		var caps, resources []byte
		var lastSeen, revokedAt sql.NullTime
		if err := rows.Scan(&node.ID, &node.UserID, &node.Name, &node.Platform, &caps, &resources, &lastSeen, &revokedAt, &node.CreatedAt, &node.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(caps, &node.Capabilities); err != nil {
			return nil, fmt.Errorf("decode execution node capabilities %q: %w", node.ID, err)
		}
		if len(resources) != 0 {
			if err := json.Unmarshal(resources, &node.Resources); err != nil {
				return nil, fmt.Errorf("decode execution node resources %q: %w", node.ID, err)
			}
		}
		if lastSeen.Valid {
			value := lastSeen.Time
			node.LastSeen = &value
		}
		if revokedAt.Valid {
			value := revokedAt.Time
			node.RevokedAt = &value
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

// ExecutionTaskForNode returns a task only when it belongs to the authenticated
// active node. It is used after node restarts to reconcile durable local work
// without granting node credentials access to the user's task collection.
func (s *Store) ExecutionTaskForNode(ctx context.Context, nodeID, taskID string) (ExecutionTask, error) {
	var userID string
	var revokedAt sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT user_id,revoked_at FROM execution_nodes WHERE id=?`, nodeID).Scan(&userID, &revokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionTask{}, domain.ErrNotFound
		}
		return ExecutionTask{}, err
	}
	if revokedAt.Valid {
		return ExecutionTask{}, domain.ErrUnauthorized
	}
	task, err := s.ExecutionTask(ctx, userID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	if task.NodeID != nodeID {
		return ExecutionTask{}, domain.ErrNotFound
	}
	return task, nil
}

func (s *Store) ExecutionNode(ctx context.Context, userID, id string) (ExecutionNode, error) {
	return executionNodeByID(ctx, s.db, userID, id)
}

func (s *Store) HeartbeatExecutionNode(ctx context.Context, tokenHash, platform string, capabilities []string, now time.Time) (ExecutionNode, error) {
	return s.HeartbeatExecutionNodeWithResources(ctx, tokenHash, platform, capabilities, ExecutionNodeResources{MaxConcurrentTasks: 1}, now)
}

func (s *Store) HeartbeatExecutionNodeWithResources(ctx context.Context, tokenHash, platform string, capabilities []string, resources ExecutionNodeResources, now time.Time) (ExecutionNode, error) {
	if err := resources.Validate(); err != nil {
		return ExecutionNode{}, err
	}
	caps, err := json.Marshal(capabilities)
	if err != nil {
		return ExecutionNode{}, err
	}
	resourceJSON, err := json.Marshal(resources)
	if err != nil {
		return ExecutionNode{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionNode{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE execution_nodes SET platform=?,capabilities_json=?,resources_json=?,last_seen=?,updated_at=? WHERE token_hash=? AND revoked_at IS NULL`, platform, caps, resourceJSON, now.UTC(), now.UTC(), tokenHash)
	if err != nil {
		return ExecutionNode{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ExecutionNode{}, err
	}
	if changed != 1 {
		return ExecutionNode{}, domain.ErrUnauthorized
	}
	var userID, nodeID string
	if err := tx.QueryRowContext(ctx, `SELECT user_id,id FROM execution_nodes WHERE token_hash=? AND revoked_at IS NULL`, tokenHash).Scan(&userID, &nodeID); err != nil {
		return ExecutionNode{}, err
	}
	node, err := executionNodeByID(ctx, tx, userID, nodeID)
	if err != nil {
		return ExecutionNode{}, err
	}
	if node.Platform != platform || !equalStrings(node.Capabilities, capabilities) || node.Resources != resources || node.LastSeen == nil || !node.LastSeen.Equal(now.UTC()) {
		return ExecutionNode{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionNode{}, err
	}
	node, err = s.ExecutionNode(ctx, userID, nodeID)
	if err != nil {
		return ExecutionNode{}, err
	}
	if node.Platform != platform || !equalStrings(node.Capabilities, capabilities) || node.Resources != resources || node.LastSeen == nil || !node.LastSeen.Equal(now.UTC()) {
		return ExecutionNode{}, domain.ErrConflict
	}
	return node, nil
}

func (s *Store) ExecutionNodeForToken(ctx context.Context, tokenHash string) (ExecutionNode, error) {
	var node ExecutionNode
	var caps []byte
	var lastSeen, revokedAt sql.NullTime
	var resources []byte
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,name,platform,capabilities_json,resources_json,last_seen,revoked_at,created_at,updated_at FROM execution_nodes WHERE token_hash=? AND revoked_at IS NULL`, tokenHash).
		Scan(&node.ID, &node.UserID, &node.Name, &node.Platform, &caps, &resources, &lastSeen, &revokedAt, &node.CreatedAt, &node.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionNode{}, domain.ErrUnauthorized
	}
	if err != nil {
		return ExecutionNode{}, err
	}
	if err := json.Unmarshal(caps, &node.Capabilities); err != nil {
		return ExecutionNode{}, err
	}
	if len(resources) != 0 {
		if err := json.Unmarshal(resources, &node.Resources); err != nil {
			return ExecutionNode{}, fmt.Errorf("decode execution node resources %q: %w", node.ID, err)
		}
	}
	if lastSeen.Valid {
		value := lastSeen.Time
		node.LastSeen = &value
	}
	return node, nil
}

func (s *Store) RevokeExecutionNode(ctx context.Context, userID, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE execution_nodes SET token_hash='revoked:'||id,revoked_at=?,updated_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`, now.UTC(), now.UTC(), id, userID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		node, readErr := executionNodeByID(ctx, tx, userID, id)
		if readErr != nil {
			return readErr
		}
		if node.RevokedAt == nil {
			return domain.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		persisted, readErr := s.ExecutionNode(ctx, userID, id)
		if readErr != nil {
			return readErr
		}
		if persisted.RevokedAt == nil {
			return domain.ErrConflict
		}
		return nil
	}
	if changed != 1 {
		return domain.ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,status,sequence FROM execution_tasks WHERE node_id=? AND user_id=? AND status IN ('queued','leased','accepted','running','reported_succeeded','reported_failed') ORDER BY created_at,id`, id, userID)
	if err != nil {
		return err
	}
	type pendingTask struct {
		id       string
		status   string
		sequence int64
	}
	var pending []pendingTask
	for rows.Next() {
		var task pendingTask
		if err := rows.Scan(&task.id, &task.status, &task.sequence); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, task)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil {
		return rowsErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, task := range pending {
		next := task.sequence + 1
		status := "needs_reconciliation"
		errorText := "execution node was revoked; verify whether external effects occurred"
		if task.status == "queued" {
			status = "cancelled"
			errorText = ""
		}
		updated, err := tx.ExecContext(ctx, `UPDATE execution_tasks SET status=?,cancel_requested=1,lease_token_hash='',lease_until=NULL,error_text=?,sequence=?,updated_at=? WHERE id=? AND user_id=? AND status=? AND sequence=?`, status, errorText, next, now.UTC(), task.id, userID, task.status, task.sequence)
		if err != nil {
			return err
		}
		taskRows, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if taskRows != 1 {
			return domain.ErrConflict
		}
		var readStatus string
		var readSequence int64
		var readError string
		if err := tx.QueryRowContext(ctx, `SELECT status,sequence,error_text FROM execution_tasks WHERE id=?`, task.id).Scan(&readStatus, &readSequence, &readError); err != nil {
			return err
		}
		if readStatus != status || readSequence != next || readError != errorText {
			return domain.ErrConflict
		}
		details, err := json.Marshal(map[string]string{"reason": "node_revoked"})
		if err != nil {
			return err
		}
		if err := insertExecutionTaskEvent(ctx, tx, task.id, next, status, details, now); err != nil {
			return err
		}
	}
	var tokenHash string
	var revokedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT token_hash,revoked_at FROM execution_nodes WHERE id=? AND user_id=?`, id, userID).Scan(&tokenHash, &revokedAt); err != nil {
		return err
	}
	if tokenHash != "revoked:"+id || !revokedAt.Equal(now.UTC()) {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_nodes WHERE id=? AND user_id=? AND revoked_at IS NULL`, id, userID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return domain.ErrConflict
	}
	persisted, err := s.ExecutionNode(ctx, userID, id)
	if err != nil {
		return err
	}
	if persisted.RevokedAt == nil {
		return domain.ErrConflict
	}
	for _, task := range pending {
		readBack, err := s.ExecutionTask(ctx, userID, task.id)
		if err != nil {
			return err
		}
		want := "needs_reconciliation"
		if task.status == "queued" {
			want = "cancelled"
		}
		if readBack.Status != want || !readBack.CancelRequested {
			return domain.ErrConflict
		}
	}
	return nil
}

type nodeQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func executionNodeByID(ctx context.Context, q nodeQueryer, userID, id string) (ExecutionNode, error) {
	var node ExecutionNode
	var caps, resources []byte
	var lastSeen, revokedAt sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT id,user_id,name,platform,capabilities_json,resources_json,last_seen,revoked_at,created_at,updated_at FROM execution_nodes WHERE id=? AND user_id=?`, id, userID).
		Scan(&node.ID, &node.UserID, &node.Name, &node.Platform, &caps, &resources, &lastSeen, &revokedAt, &node.CreatedAt, &node.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionNode{}, domain.ErrNotFound
	}
	if err != nil {
		return ExecutionNode{}, err
	}
	if err := json.Unmarshal(caps, &node.Capabilities); err != nil {
		return ExecutionNode{}, err
	}
	if len(resources) != 0 {
		if err := json.Unmarshal(resources, &node.Resources); err != nil {
			return ExecutionNode{}, fmt.Errorf("decode execution node resources %q: %w", node.ID, err)
		}
	}
	if lastSeen.Valid {
		value := lastSeen.Time
		node.LastSeen = &value
	}
	if revokedAt.Valid {
		value := revokedAt.Time
		node.RevokedAt = &value
	}
	return node, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
