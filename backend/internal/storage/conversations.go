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

func (s *Store) PurgeExpiredConversations(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE deleted_at IS NOT NULL AND recover_until<=?`, now)
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	var remaining int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE deleted_at IS NOT NULL AND recover_until<=?`, now).Scan(&remaining); err != nil {
		return 0, err
	}
	if remaining != 0 {
		return deleted, fmt.Errorf("conversation purge read-back found %d expired records", remaining)
	}
	return deleted, nil
}

func (s *Store) DeleteConversation(ctx context.Context, userID, id string) (domain.ConversationDeletion, string, error) {
	if id == "" || userID == "" {
		return domain.ConversationDeletion{}, "", domain.ErrInvalid
	}
	if _, err := s.PurgeExpiredConversations(ctx); err != nil {
		return domain.ConversationDeletion{}, "", fmt.Errorf("purge expired conversations before delete: %w", err)
	}

	now := time.Now().UTC()
	recoverUntil := now.Add(domain.ConversationRecoveryRetention)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ConversationDeletion{}, "", err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL`, id, userID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return domain.ConversationDeletion{}, "", domain.ErrNotFound
	} else if err != nil {
		return domain.ConversationDeletion{}, "", err
	}

	var activeTurnID, activeStatus string
	var lastSequence int
	err = tx.QueryRowContext(ctx, `SELECT id,status,last_sequence FROM agent_turns
WHERE user_id=? AND conversation_id=? AND status IN ('running','cancelling','awaiting_approval') LIMIT 1`, userID, id).Scan(&activeTurnID, &activeStatus, &lastSequence)
	if errors.Is(err, sql.ErrNoRows) {
		activeTurnID = ""
	} else if err != nil {
		return domain.ConversationDeletion{}, "", err
	}
	if activeTurnID != "" {
		if activeStatus != "cancelling" {
			lastSequence++
			details, marshalErr := json.Marshal(map[string]string{"reason": "conversation_deleted"})
			if marshalErr != nil {
				return domain.ConversationDeletion{}, "", marshalErr
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at)
VALUES(?,?,?,?,?,?,?)`, eventID(activeTurnID, lastSequence), id, activeTurnID, lastSequence, "turn.cancel_requested", details, now); err != nil {
				return domain.ConversationDeletion{}, "", err
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE agent_turns SET status='cancelling',cancel_requested=1,last_sequence=?,updated_at=?
WHERE id=? AND user_id=? AND conversation_id=? AND status IN ('running','cancelling','awaiting_approval')`, lastSequence, now, activeTurnID, userID, id)
		if err != nil {
			return domain.ConversationDeletion{}, "", err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return domain.ConversationDeletion{}, "", err
		}
		if rows != 1 {
			return domain.ConversationDeletion{}, "", domain.ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_approvals SET status='cancelled',decision='cancelled',resolved_at=?
WHERE user_id=? AND conversation_id=? AND status='pending'`, now, userID, id); err != nil {
		return domain.ConversationDeletion{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_inbox SET status='cancelled',updated_at=?
WHERE user_id=? AND conversation_id=? AND status='queued'`, now, userID, id); err != nil {
		return domain.ConversationDeletion{}, "", err
	}
	result, err := tx.ExecContext(ctx, `UPDATE conversations SET deleted_at=?,recover_until=?,restore_execution_paused=execution_paused,execution_paused=1,updated_at=?
WHERE id=? AND user_id=? AND deleted_at IS NULL`, now, recoverUntil, now, id, userID)
	if err != nil {
		return domain.ConversationDeletion{}, "", err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return domain.ConversationDeletion{}, "", err
	}
	if rows != 1 {
		return domain.ConversationDeletion{}, "", domain.ErrNotFound
	}
	lifecycle, err := json.Marshal(map[string]any{"recoverUntil": recoverUntil, "cancelledTurnId": activeTurnID})
	if err != nil {
		return domain.ConversationDeletion{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_lifecycle_events(id,conversation_id,kind,details_json,created_at)
VALUES(?,?,?,?,?)`, fmt.Sprintf("conversation_deleted_%s_%d", id, now.UnixNano()), id, "conversation.deleted", lifecycle, now); err != nil {
		return domain.ConversationDeletion{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return domain.ConversationDeletion{}, "", err
	}

	var readDeletedAt, readRecoverUntil sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT deleted_at,recover_until FROM conversations WHERE id=? AND user_id=?`, id, userID).Scan(&readDeletedAt, &readRecoverUntil); err != nil {
		return domain.ConversationDeletion{}, "", err
	}
	if !readDeletedAt.Valid || !readRecoverUntil.Valid || !readDeletedAt.Time.Equal(now) || !readRecoverUntil.Time.Equal(recoverUntil) {
		return domain.ConversationDeletion{}, "", fmt.Errorf("conversation delete read-back mismatch: %w", domain.ErrConflict)
	}
	return domain.ConversationDeletion{ConversationID: id, DeletedAt: readDeletedAt.Time.UTC(), RecoverUntil: readRecoverUntil.Time.UTC()}, activeTurnID, nil
}

func (s *Store) ListDeletedConversations(ctx context.Context, userID string) ([]domain.DeletedConversation, error) {
	if _, err := s.PurgeExpiredConversations(ctx); err != nil {
		return nil, fmt.Errorf("purge expired conversations before listing recovery items: %w", err)
	}
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.user_id,c.title,c.provider_id,COALESCE(b.generation_id,''),COALESCE(b.definition_digest,''),
COALESCE(c.project_id,''),c.permission_profile,COALESCE(c.parent_conversation_id,''),COALESCE(c.branch_from_message_id,''),
c.execution_paused,c.created_at,c.updated_at,c.deleted_at,c.recover_until
FROM conversations c LEFT JOIN conversation_agent_bindings b ON b.conversation_id=c.id
WHERE c.user_id=? AND c.deleted_at IS NOT NULL AND c.recover_until>?
ORDER BY c.deleted_at DESC`, userID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.DeletedConversation, 0)
	for rows.Next() {
		var item domain.DeletedConversation
		var deletedAt, recoverUntil sql.NullTime
		if err := rows.Scan(&item.ID, &item.UserID, &item.Title, &item.ProviderID, &item.AgentGenerationID, &item.AgentDefinitionDigest,
			&item.ProjectID, &item.PermissionProfile, &item.ParentConversationID, &item.BranchFromMessageID, &item.ExecutionPaused,
			&item.CreatedAt, &item.UpdatedAt, &deletedAt, &recoverUntil); err != nil {
			return nil, err
		}
		if !deletedAt.Valid || !recoverUntil.Valid {
			return nil, fmt.Errorf("deleted conversation %q has incomplete recovery metadata", item.ID)
		}
		item.DeletedAt = deletedAt.Time.UTC()
		item.RecoverUntil = recoverUntil.Time.UTC()
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) RestoreConversation(ctx context.Context, userID, id string) (domain.ConversationDetail, error) {
	if _, err := s.PurgeExpiredConversations(ctx); err != nil {
		return domain.ConversationDetail{}, fmt.Errorf("purge expired conversations before restore: %w", err)
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE conversations SET deleted_at=NULL,recover_until=NULL,execution_paused=restore_execution_paused,restore_execution_paused=0,updated_at=?
WHERE id=? AND user_id=? AND deleted_at IS NOT NULL AND recover_until>?`, now, id, userID, now)
	if err != nil {
		return domain.ConversationDetail{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return domain.ConversationDetail{}, err
	}
	if rows != 1 {
		return domain.ConversationDetail{}, domain.ErrNotFound
	}
	readBack, err := s.Conversation(ctx, userID, id)
	if err != nil {
		return domain.ConversationDetail{}, fmt.Errorf("read restored conversation back: %w", err)
	}
	if readBack.ID != id {
		return domain.ConversationDetail{}, fmt.Errorf("restored conversation read-back mismatch: %w", domain.ErrConflict)
	}
	return readBack, nil
}

// ImportConversationMessages copies historical user/assistant messages into a
// newly-created target conversation as one durable mutation. The caller
// supplies destination message IDs and timestamps; source IDs remain in the
// immutable context artifact attached to the task.
func (s *Store) ImportConversationMessages(ctx context.Context, userID, conversationID string, messages []domain.Message) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(conversationID) == "" {
		return domain.ErrInvalid
	}
	for _, message := range messages {
		if message.ID == "" || message.ConversationID != conversationID || (message.Role != "user" && message.Role != "assistant") || message.Content == "" || message.CreatedAt.IsZero() {
			return domain.ErrInvalid
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE conversation_id=?`, conversationID).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return domain.ErrConflict
	}
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL`, conversationID, userID).Scan(&owner); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	for _, message := range messages {
		result, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) VALUES(?,?,?,?,?)`, message.ID, conversationID, message.Role, message.Content, message.CreatedAt.UTC())
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return domain.ErrConflict
		}
	}
	if len(messages) > 0 {
		last := messages[len(messages)-1].CreatedAt.UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, last, conversationID, userID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	readBack, err := s.Conversation(ctx, userID, conversationID)
	if err != nil {
		return err
	}
	if len(readBack.Messages) != len(messages) {
		return domain.ErrConflict
	}
	for i, want := range messages {
		got := readBack.Messages[i]
		if got.ID != want.ID || got.Role != want.Role || got.Content != want.Content || !got.CreatedAt.Equal(want.CreatedAt.UTC()) {
			return domain.ErrConflict
		}
	}
	return nil
}

// CreateTaskContinuationConversation durably forks a cloud conversation onto
// an imported project and appends the verified local task exchange atomically.
// conversation.ID is task-derived, so retries read back the same branch.
func (s *Store) CreateTaskContinuationConversation(ctx context.Context, branch domain.Conversation, sourceMessageID string, messages []domain.Message) (domain.ConversationDetail, error) {
	if strings.TrimSpace(branch.ID) == "" || strings.TrimSpace(branch.UserID) == "" || strings.TrimSpace(branch.ParentConversationID) == "" || strings.TrimSpace(branch.ProviderID) == "" || !branch.PermissionProfile.Valid() || len(messages) < 2 {
		return domain.ConversationDetail{}, domain.ErrInvalid
	}
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" || message.CreatedAt.IsZero() {
			return domain.ConversationDetail{}, domain.ErrInvalid
		}
	}
	if messages[len(messages)-2].Role != "user" || messages[len(messages)-1].Role != "assistant" || strings.TrimSpace(messages[len(messages)-2].Content) == "" || strings.TrimSpace(messages[len(messages)-1].Content) == "" {
		return domain.ConversationDetail{}, domain.ErrInvalid
	}
	if existing, err := s.Conversation(ctx, branch.UserID, branch.ID); err == nil {
		if !sameTaskContinuation(existing, branch, sourceMessageID, messages) {
			return domain.ConversationDetail{}, fmt.Errorf("existing task continuation does not match the requested branch: %w", domain.ErrConflict)
		}
		return existing, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.ConversationDetail{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ConversationDetail{}, err
	}
	defer tx.Rollback()
	var providerID string
	var profile domain.PermissionProfile
	if err := tx.QueryRowContext(ctx, `SELECT provider_id,permission_profile FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL`, branch.ParentConversationID, branch.UserID).Scan(&providerID, &profile); errors.Is(err, sql.ErrNoRows) {
		return domain.ConversationDetail{}, domain.ErrNotFound
	} else if err != nil {
		return domain.ConversationDetail{}, err
	}
	if providerID != branch.ProviderID || profile != branch.PermissionProfile {
		return domain.ConversationDetail{}, fmt.Errorf("task continuation provider or permission profile differs from its source: %w", domain.ErrConflict)
	}
	if branch.ProjectID != "" {
		var projectExists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=? AND user_id=?`, branch.ProjectID, branch.UserID).Scan(&projectExists); errors.Is(err, sql.ErrNoRows) {
			return domain.ConversationDetail{}, domain.ErrNotFound
		} else if err != nil {
			return domain.ConversationDetail{}, err
		}
	}
	if sourceMessageID != "" {
		var messageExists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE id=? AND conversation_id=?`, sourceMessageID, branch.ParentConversationID).Scan(&messageExists); errors.Is(err, sql.ErrNoRows) {
			return domain.ConversationDetail{}, domain.ErrConflict
		} else if err != nil {
			return domain.ConversationDetail{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,user_id,title,provider_id,project_id,permission_profile,parent_conversation_id,branch_from_message_id,execution_paused,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, branch.ID, branch.UserID, strings.TrimSpace(branch.Title), branch.ProviderID, branch.ProjectID, branch.PermissionProfile, branch.ParentConversationID, sourceMessageID, branch.ExecutionPaused, branch.CreatedAt.UTC(), branch.UpdatedAt.UTC()); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.ConversationDetail{}, domain.ErrConflict
		}
		return domain.ConversationDetail{}, err
	}
	binding, err := tx.ExecContext(ctx, `INSERT INTO conversation_agent_bindings(conversation_id,user_id,generation_id,definition_digest,bound_at)
SELECT ?,user_id,generation_id,definition_digest,? FROM conversation_agent_bindings WHERE conversation_id=? AND user_id=?`, branch.ID, branch.CreatedAt.UTC(), branch.ParentConversationID, branch.UserID)
	if err != nil {
		return domain.ConversationDetail{}, err
	}
	bound, err := binding.RowsAffected()
	if err != nil {
		return domain.ConversationDetail{}, err
	}
	if bound != 1 {
		return domain.ConversationDetail{}, fmt.Errorf("source conversation has no generation binding: %w", domain.ErrConflict)
	}
	for index, message := range messages {
		messageID := fmt.Sprintf("msg_%s_task_%06d", strings.TrimPrefix(branch.ID, "conv_"), index+1)
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) VALUES(?,?,?,?,?)`, messageID, branch.ID, message.Role, message.Content, message.CreatedAt.UTC()); err != nil {
			return domain.ConversationDetail{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, branch.UpdatedAt.UTC(), branch.ID, branch.UserID); err != nil {
		return domain.ConversationDetail{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ConversationDetail{}, err
	}
	readBack, err := s.Conversation(ctx, branch.UserID, branch.ID)
	if err != nil {
		return domain.ConversationDetail{}, fmt.Errorf("read task continuation back after commit: %w", err)
	}
	if !sameTaskContinuation(readBack, branch, sourceMessageID, messages) {
		return domain.ConversationDetail{}, fmt.Errorf("task continuation did not read back with its source, project and task exchange: %w", domain.ErrConflict)
	}
	return readBack, nil
}

func sameTaskContinuation(existing domain.ConversationDetail, branch domain.Conversation, sourceMessageID string, messages []domain.Message) bool {
	if existing.ID != branch.ID || existing.ParentConversationID != branch.ParentConversationID || existing.ProjectID != branch.ProjectID || existing.BranchFromMessageID != sourceMessageID || existing.ProviderID != branch.ProviderID || existing.PermissionProfile != branch.PermissionProfile || len(existing.Messages) < len(messages) {
		return false
	}
	for index, message := range messages {
		actual := existing.Messages[index]
		if actual.Role != message.Role || actual.Content != message.Content {
			return false
		}
	}
	return true
}
