package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

type ContinuationState struct {
	Version           int
	Ciphertext        []byte
	Nonce             []byte
	ContentHash       string
	UnavailableReason string
}

type AgentContinuationSnapshot struct {
	SourceTurnID      string
	ConversationID    string
	Version           int
	Ciphertext        []byte
	Nonce             []byte
	ContentHash       string
	Status            string
	UnavailableReason string
	IdempotencyKey    string
	ConsumedTurnID    string
}

func (s *Store) StartAgentTurn(ctx context.Context, userID string, turn domain.AgentTurn, input domain.Message, details json.RawMessage) error {
	return s.startAgentTurn(ctx, userID, turn, input, details, "", "", false, false)
}

func (s *Store) StartQueuedAgentTurn(ctx context.Context, userID string, turn domain.AgentTurn, input domain.Message, inboxID string, details json.RawMessage) error {
	return s.startAgentTurn(ctx, userID, turn, input, details, "", inboxID, false, false)
}

func (s *Store) StartRetryAgentTurn(ctx context.Context, userID string, turn domain.AgentTurn, input domain.Message, retryOf string, revisedContent *string, details json.RawMessage) error {
	return s.startAgentTurn(ctx, userID, turn, input, details, retryOf, "", true, revisedContent != nil)
}

func (s *Store) StartBranchAgentTurn(ctx context.Context, userID, sourceConversationID, sourceTurnID, sourceInputMessageID string, branch domain.Conversation, turn domain.AgentTurn, input domain.Message, details json.RawMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if branch.ID == "" || branch.ParentConversationID != sourceConversationID || branch.BranchFromMessageID != sourceInputMessageID || turn.ConversationID != branch.ID || turn.InputMessageID != input.ID || input.ConversationID != branch.ID || input.Role != "user" {
		return domain.ErrInvalid
	}
	if !branch.PermissionProfile.Valid() || !turn.PermissionProfile.Valid() {
		return domain.ErrInvalid
	}
	if strings.TrimSpace(input.Content) == "" {
		return domain.ErrInvalid
	}
	var parentTurnInputID, parentTurnStatus string
	var sourceProviderID, sourceProjectID, sourceGenerationID, sourceDefinitionDigest string
	var sourcePermissionProfile domain.PermissionProfile
	if err := tx.QueryRowContext(ctx, `SELECT t.input_message_id,t.status,c.provider_id,COALESCE(c.project_id,''),c.permission_profile,b.generation_id,b.definition_digest
FROM agent_turns t JOIN conversations c ON c.id=t.conversation_id AND c.user_id=t.user_id
JOIN conversation_agent_bindings b ON b.conversation_id=c.id AND b.user_id=c.user_id
WHERE t.id=? AND t.conversation_id=? AND t.user_id=?`, sourceTurnID, sourceConversationID, userID).Scan(&parentTurnInputID, &parentTurnStatus, &sourceProviderID, &sourceProjectID, &sourcePermissionProfile, &sourceGenerationID, &sourceDefinitionDigest); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if branch.UserID != userID || branch.ProviderID != sourceProviderID || branch.ProjectID != sourceProjectID || branch.PermissionProfile != sourcePermissionProfile || branch.AgentGenerationID != sourceGenerationID || branch.AgentDefinitionDigest != sourceDefinitionDigest || turn.ProviderID != sourceProviderID || turn.AgentGenerationID != sourceGenerationID || turn.AgentDefinitionDigest != sourceDefinitionDigest || turn.PermissionProfile != sourcePermissionProfile {
		return fmt.Errorf("%w: branch execution binding must match its source conversation", domain.ErrConflict)
	}
	if parentTurnInputID != sourceInputMessageID || parentTurnStatus == "running" || parentTurnStatus == "cancelling" || parentTurnStatus == "awaiting_approval" {
		return fmt.Errorf("%w: source turn is still active", domain.ErrConflict)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id,role,content,created_at FROM messages WHERE conversation_id=? ORDER BY created_at,rowid`, sourceConversationID)
	if err != nil {
		return err
	}
	type prefixMessage struct {
		id, role, content string
		createdAt         time.Time
	}
	prefix := []prefixMessage{}
	foundInput := false
	for rows.Next() {
		var message prefixMessage
		if err := rows.Scan(&message.id, &message.role, &message.content, &message.createdAt); err != nil {
			rows.Close()
			return err
		}
		if message.id == sourceInputMessageID {
			foundInput = message.role == "user"
			break
		}
		prefix = append(prefix, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !foundInput {
		return domain.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,user_id,title,provider_id,project_id,permission_profile,parent_conversation_id,branch_from_message_id,created_at,updated_at)
SELECT ?,user_id,?,?,?,?,?,?,?,? FROM conversations WHERE id=? AND user_id=?`, branch.ID, branch.Title, branch.ProviderID, branch.ProjectID, branch.PermissionProfile, sourceConversationID, sourceInputMessageID, branch.CreatedAt, branch.UpdatedAt, sourceConversationID, userID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO conversation_agent_bindings(conversation_id,user_id,generation_id,definition_digest,bound_at)
SELECT ?,user_id,generation_id,definition_digest,? FROM conversation_agent_bindings WHERE conversation_id=? AND user_id=?`, branch.ID, branch.CreatedAt, sourceConversationID, userID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: source conversation has no generation binding", domain.ErrConflict)
	}
	for index, message := range prefix {
		cloneID := fmt.Sprintf("msg_%s_branch_%06d", strings.TrimPrefix(branch.ID, "run_"), index+1)
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) VALUES(?,?,?,?,?)`, cloneID, branch.ID, message.role, message.content, message.createdAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) VALUES(?,?,?,?,?)`, input.ID, branch.ID, input.Role, input.Content, input.CreatedAt); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_turns(id,conversation_id,user_id,input_message_id,provider_id,generation_id,definition_digest,permission_profile,retry_of_turn_id,input_content_snapshot,inbox_id,status,last_sequence,started_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,'running',1,?,?)`, turn.ID, turn.ConversationID, userID, input.ID, turn.ProviderID, turn.AgentGenerationID, turn.AgentDefinitionDigest, turn.PermissionProfile, sourceTurnID, input.Content, "", turn.StartedAt, turn.UpdatedAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.ErrConflict
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turn.ID, 1), branch.ID, turn.ID, 1, "turn.started", []byte(details), turn.StartedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, input.CreatedAt, branch.ID, userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	readBack, err := s.Conversation(ctx, userID, branch.ID)
	if err != nil {
		return fmt.Errorf("read branched conversation back: %w", err)
	}
	if readBack.ID != branch.ID || readBack.ParentConversationID != sourceConversationID || len(readBack.Messages) != len(prefix)+1 || readBack.Messages[len(readBack.Messages)-1].ID != input.ID || readBack.Messages[len(readBack.Messages)-1].Content != input.Content {
		return fmt.Errorf("branched conversation read-back mismatch: %w", domain.ErrConflict)
	}
	return nil
}

// ForkCompletedConversation copies the message prefix through a completed
// assistant answer into a new conversation without creating an agent turn.
func (s *Store) ForkCompletedConversation(ctx context.Context, userID, sourceTurnID string, branch domain.Conversation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if branch.ID == "" || branch.UserID != userID || branch.ParentConversationID == "" || branch.BranchFromMessageID == "" || !branch.PermissionProfile.Valid() {
		return domain.ErrInvalid
	}
	var sourceConversationID, resultMessageID, status string
	var sourceProviderID, sourceProjectID string
	var sourcePermissionProfile domain.PermissionProfile
	var sourceGenerationID, sourceDefinitionDigest string
	err = tx.QueryRowContext(ctx, `SELECT t.conversation_id,COALESCE(t.result_message_id,''),t.status,c.provider_id,COALESCE(c.project_id,''),c.permission_profile,COALESCE(b.generation_id,''),COALESCE(b.definition_digest,'')
FROM agent_turns t JOIN conversations c ON c.id=t.conversation_id AND c.user_id=t.user_id
LEFT JOIN conversation_agent_bindings b ON b.conversation_id=c.id AND b.user_id=c.user_id
WHERE t.id=? AND t.user_id=?`, sourceTurnID, userID).Scan(&sourceConversationID, &resultMessageID, &status, &sourceProviderID, &sourceProjectID, &sourcePermissionProfile, &sourceGenerationID, &sourceDefinitionDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "completed" || resultMessageID == "" || branch.ParentConversationID != sourceConversationID || branch.BranchFromMessageID != resultMessageID {
		return fmt.Errorf("%w: only a completed assistant answer can be forked", domain.ErrConflict)
	}
	if branch.ProviderID != sourceProviderID || branch.ProjectID != sourceProjectID || branch.PermissionProfile != sourcePermissionProfile || branch.AgentGenerationID != sourceGenerationID || branch.AgentDefinitionDigest != sourceDefinitionDigest {
		return fmt.Errorf("%w: fork execution binding must match its source conversation", domain.ErrConflict)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,role,content,created_at FROM messages WHERE conversation_id=? ORDER BY created_at,rowid`, sourceConversationID)
	if err != nil {
		return err
	}
	type prefixMessage struct {
		role, content string
		createdAt     time.Time
	}
	prefix := make([]prefixMessage, 0)
	foundAnswer := false
	for rows.Next() {
		var id, role, content string
		var createdAt time.Time
		if err := rows.Scan(&id, &role, &content, &createdAt); err != nil {
			rows.Close()
			return err
		}
		prefix = append(prefix, prefixMessage{role: role, content: content, createdAt: createdAt})
		if id == resultMessageID {
			foundAnswer = role == "assistant"
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !foundAnswer || len(prefix) == 0 {
		return domain.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,user_id,title,provider_id,project_id,permission_profile,parent_conversation_id,branch_from_message_id,created_at,updated_at)
SELECT ?,user_id,?,?,?,?,?,?,?,? FROM conversations WHERE id=? AND user_id=?`, branch.ID, branch.Title, branch.ProviderID, branch.ProjectID, branch.PermissionProfile, sourceConversationID, resultMessageID, branch.CreatedAt, branch.UpdatedAt, sourceConversationID, userID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO conversation_agent_bindings(conversation_id,user_id,generation_id,definition_digest,bound_at)
SELECT ?,user_id,generation_id,definition_digest,? FROM conversation_agent_bindings WHERE conversation_id=? AND user_id=?`, branch.ID, branch.CreatedAt, sourceConversationID, userID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: source conversation has no agent generation binding", domain.ErrConflict)
	}
	idPrefix := strings.TrimPrefix(branch.ID, "conv_")
	for index, message := range prefix {
		cloneID := fmt.Sprintf("msg_%s_fork_%06d", idPrefix, index+1)
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) VALUES(?,?,?,?,?)`, cloneID, branch.ID, message.role, message.content, message.createdAt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, branch.UpdatedAt, branch.ID, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordAgentTurnReconciliation(ctx context.Context, userID, turnID, outcome, note string, at time.Time) (domain.AgentTurnReconciliation, error) {
	decision, recoveryClass, nextStatus, err := reconciliationState(outcome)
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	result := domain.AgentTurnReconciliation{ID: "reconcile_" + turnID, TurnID: turnID, Decision: decision, Note: strings.TrimSpace(note), CreatedAt: at.UTC()}
	if result.Note == "" || len([]rune(result.Note)) > 2000 {
		return domain.AgentTurnReconciliation{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	defer tx.Rollback()
	var status, currentRecovery string
	var sequence int
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id,status,recovery_class,last_sequence FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&result.ConversationID, &status, &currentRecovery, &sequence); errors.Is(err, sql.ErrNoRows) {
		return domain.AgentTurnReconciliation{}, domain.ErrNotFound
	} else if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if status != "completed" && status != "failed" && status != "cancelled" && status != "interrupted" && status != "incomplete" && status != "needs_reconciliation" {
		return domain.AgentTurnReconciliation{}, domain.ErrConflict
	}
	if currentRecovery != "unknown_external_effect" && currentRecovery != "safe_to_retry" && currentRecovery != "external_effect_confirmed" {
		return domain.AgentTurnReconciliation{}, fmt.Errorf("%w: turn has no unresolved external effect", domain.ErrConflict)
	}
	_, _, malformedJournal, _, completedExternalEffect, _, err := inspectToolJournal(ctx, tx, turnID)
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if outcome == "no_effect_applied" && (malformedJournal || completedExternalEffect) {
		return domain.AgentTurnReconciliation{}, fmt.Errorf("%w: the turn contains completed or unverifiable tool effects and cannot be replayed safely", domain.ErrConflict)
	}
	var existing domain.AgentTurnReconciliation
	readErr := tx.QueryRowContext(ctx, `SELECT id,turn_id,conversation_id,decision,note,created_at FROM agent_turn_reconciliations WHERE turn_id=? AND user_id=?`, turnID, userID).
		Scan(&existing.ID, &existing.TurnID, &existing.ConversationID, &existing.Decision, &existing.Note, &existing.CreatedAt)
	if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
		return domain.AgentTurnReconciliation{}, readErr
	}
	if readErr == nil {
		result.ID = existing.ID
		if existing.Decision == result.Decision && existing.Note == result.Note && currentRecovery == recoveryClass && status == nextStatus {
			if err := tx.Commit(); err != nil {
				return domain.AgentTurnReconciliation{}, err
			}
			return existing, nil
		}
	}
	sequence++
	traceEventID := eventID(turnID, sequence)
	details, err := json.Marshal(map[string]string{"reconciliationId": result.ID, "decision": result.Decision, "eventId": traceEventID})
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_turn_reconciliation_events(id,turn_id,user_id,conversation_id,decision,note,created_at) VALUES(?,?,?,?,?,?,?)`, traceEventID, turnID, userID, result.ConversationID, result.Decision, result.Note, result.CreatedAt); err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if errors.Is(readErr, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_turn_reconciliations(id,turn_id,user_id,conversation_id,decision,note,created_at) VALUES(?,?,?,?,?,?,?)`, result.ID, turnID, userID, result.ConversationID, result.Decision, result.Note, result.CreatedAt); err != nil {
			return domain.AgentTurnReconciliation{}, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE agent_turn_reconciliations SET decision=?,note=?,created_at=? WHERE turn_id=? AND user_id=?`, result.Decision, result.Note, result.CreatedAt, turnID, userID); err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, traceEventID, result.ConversationID, turnID, sequence, "turn.reconciled", details, result.CreatedAt); err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	updatedTurn, err := tx.ExecContext(ctx, `UPDATE agent_turns SET recovery_class=?,status=?,last_sequence=?,updated_at=? WHERE id=? AND user_id=?`, recoveryClass, nextStatus, sequence, result.CreatedAt, turnID, userID)
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	turnCount, err := updatedTurn.RowsAffected()
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if turnCount != 1 {
		return domain.AgentTurnReconciliation{}, domain.ErrNotFound
	}
	updated, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, result.CreatedAt, result.ConversationID, userID)
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	updatedCount, err := updated.RowsAffected()
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if updatedCount != 1 {
		return domain.AgentTurnReconciliation{}, domain.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	var persisted domain.AgentTurnReconciliation
	if err := s.db.QueryRowContext(ctx, `SELECT id,turn_id,conversation_id,decision,note,created_at FROM agent_turn_reconciliations WHERE id=? AND user_id=?`, result.ID, userID).
		Scan(&persisted.ID, &persisted.TurnID, &persisted.ConversationID, &persisted.Decision, &persisted.Note, &persisted.CreatedAt); err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	var persistedRecovery, persistedStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT recovery_class,status FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&persistedRecovery, &persistedStatus); err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if persisted.Decision != decision || persisted.Note != result.Note || persistedRecovery != recoveryClass || persistedStatus != nextStatus {
		return domain.AgentTurnReconciliation{}, fmt.Errorf("agent turn reconciliation did not read back as the requested outcome")
	}
	return persisted, nil
}

func reconciliationState(outcome string) (decision, recoveryClass, status string, err error) {
	switch strings.TrimSpace(outcome) {
	case "no_effect_applied":
		return "no_effect_applied", "safe_to_retry", "interrupted", nil
	case "effect_applied":
		return "effect_applied", "external_effect_confirmed", "interrupted", nil
	case "still_unknown":
		return "still_unknown", "unknown_external_effect", "needs_reconciliation", nil
	default:
		return "", "", "", domain.ErrInvalid
	}
}

func (s *Store) AgentTurnReconciliations(ctx context.Context, userID, turnID string) ([]domain.AgentTurnReconciliation, error) {
	if _, err := s.AgentTurn(ctx, userID, turnID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,turn_id,conversation_id,decision,note,created_at FROM agent_turn_reconciliation_events WHERE user_id=? AND turn_id=? ORDER BY created_at,id`, userID, turnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.AgentTurnReconciliation, 0)
	for rows.Next() {
		var item domain.AgentTurnReconciliation
		if err := rows.Scan(&item.ID, &item.TurnID, &item.ConversationID, &item.Decision, &item.Note, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) startAgentTurn(ctx context.Context, userID string, turn domain.AgentTurn, input domain.Message, details json.RawMessage, retryOf, inboxID string, existingInput, editInput bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var wasPaused bool
	if err := tx.QueryRowContext(ctx, `SELECT execution_paused FROM conversations WHERE id=? AND user_id=?`, turn.ConversationID, userID).Scan(&wasPaused); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	var unresolvedReconciliations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_turns WHERE conversation_id=? AND user_id=? AND status='needs_reconciliation'`, turn.ConversationID, userID).Scan(&unresolvedReconciliations); err != nil {
		return err
	}
	if unresolvedReconciliations > 0 {
		return fmt.Errorf("%w: conversation has an external effect that requires reconciliation", domain.ErrConflict)
	}
	if inboxID != "" {
		if wasPaused {
			return fmt.Errorf("%w: conversation execution is paused", domain.ErrConflict)
		}
	}
	var revisionPrior *string
	if existingInput {
		var currentStatus, currentRecoveryClass string
		var conversationID string
		var inputMessageID string
		if err := tx.QueryRowContext(ctx, `SELECT conversation_id,status,input_message_id,recovery_class FROM agent_turns WHERE id=? AND user_id=?`, retryOf, userID).Scan(&conversationID, &currentStatus, &inputMessageID, &currentRecoveryClass); errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		} else if err != nil {
			return err
		}
		if currentRecoveryClass == "unknown_external_effect" || currentRecoveryClass == "external_effect_confirmed" {
			return fmt.Errorf("%w: retry requires reconciliation of the previous external effect", domain.ErrConflict)
		}
		if conversationID != input.ConversationID || inputMessageID != input.ID || (currentStatus != "cancelled" && currentStatus != "failed" && currentStatus != "interrupted" && currentStatus != "incomplete") {
			return domain.ErrConflict
		}
		var latestMessageID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE conversation_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1`, input.ConversationID).Scan(&latestMessageID); errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		} else if err != nil {
			return err
		}
		if latestMessageID != input.ID {
			return fmt.Errorf("%w: only the latest user message can be retried", domain.ErrConflict)
		}
		if editInput {
			var previous string
			if err := tx.QueryRowContext(ctx, `SELECT content FROM messages WHERE id=? AND conversation_id=? AND role='user'`, input.ID, input.ConversationID).Scan(&previous); errors.Is(err, sql.ErrNoRows) {
				return domain.ErrNotFound
			} else if err != nil {
				return err
			}
			revisionPrior = &previous
			result, err := tx.ExecContext(ctx, `UPDATE messages SET content=?,created_at=? WHERE id=? AND conversation_id=? AND role='user'`, input.Content, turn.StartedAt, input.ID, input.ConversationID)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return domain.ErrNotFound
			}
		}
	} else {
		result, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at)
SELECT ?,?,?,?,? FROM conversations WHERE id=? AND user_id=?`, input.ID, input.ConversationID, input.Role, input.Content, input.CreatedAt, input.ConversationID, userID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return domain.ErrNotFound
		}
	}
	profile := turn.PermissionProfile
	if profile == "" {
		profile = domain.DefaultPermissionProfile()
	} else if !profile.Valid() {
		return domain.ErrInvalid
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_turns(id,conversation_id,user_id,input_message_id,provider_id,generation_id,definition_digest,permission_profile,retry_of_turn_id,input_content_snapshot,inbox_id,status,last_sequence,started_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,'running',1,?,?)`, turn.ID, turn.ConversationID, userID, input.ID, turn.ProviderID, turn.AgentGenerationID, turn.AgentDefinitionDigest, profile, retryOf, input.Content, inboxID, turn.StartedAt, turn.UpdatedAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.ErrConflict
		}
		return err
	}
	if revisionPrior != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_revisions(id,message_id,prior_content,revised_content,revised_by_turn_id,revised_at) VALUES(?,?,?,?,?,?)`, fmt.Sprintf("rev_%s", turn.ID), input.ID, *revisionPrior, input.Content, turn.ID, turn.StartedAt); err != nil {
			return err
		}
	}
	if inboxID != "" {
		result, err := tx.ExecContext(ctx, `UPDATE agent_inbox SET status='claimed',turn_id=?,updated_at=? WHERE id=? AND user_id=? AND conversation_id=? AND status='queued' AND content=?`, turn.ID, turn.StartedAt, inboxID, userID, turn.ConversationID, input.Content)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("%w: queued input is no longer available", domain.ErrConflict)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turn.ID, 1), turn.ConversationID, turn.ID, 1, "turn.started", []byte(details), turn.StartedAt); err != nil {
		return err
	}
	updateQuery := `UPDATE conversations SET updated_at=?,execution_paused=0 WHERE id=? AND user_id=?`
	if inboxID != "" {
		updateQuery = `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=? AND execution_paused=0`
	}
	updated, err := tx.ExecContext(ctx, updateQuery, input.CreatedAt, input.ConversationID, userID)
	if err != nil {
		return err
	}
	updatedCount, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if updatedCount != 1 {
		return domain.ErrConflict
	}
	if wasPaused && inboxID == "" {
		resumeDetails, err := json.Marshal(map[string]string{"turnId": turn.ID, "reason": "new_turn_started"})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_lifecycle_events(id,conversation_id,kind,details_json,created_at) VALUES(?,?,?,?,?)`, "conversation_resumed_"+turn.ID, turn.ConversationID, "conversation.resumed", resumeDetails, input.CreatedAt); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	readBack, err := s.AgentTurn(ctx, userID, turn.ID)
	if err != nil {
		return fmt.Errorf("read started Turn back: %w", err)
	}
	if readBack.Status != "running" || readBack.ConversationID != turn.ConversationID || readBack.InputMessageID != input.ID {
		return fmt.Errorf("started Turn read-back mismatch: %w", domain.ErrConflict)
	}
	detail, err := s.Conversation(ctx, userID, input.ConversationID)
	if err != nil {
		return fmt.Errorf("read started Turn input back: %w", err)
	}
	foundInput := false
	for _, message := range detail.Messages {
		if message.ID == input.ID && message.Role == "user" && message.Content == input.Content {
			foundInput = true
			break
		}
	}
	if !foundInput {
		return fmt.Errorf("started Turn input was not read back: %w", domain.ErrConflict)
	}
	return nil
}

func (s *Store) AppendTurnEvent(ctx context.Context, userID, turnID, kind string, details json.RawMessage, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var conversationID, status string
	var sequence int
	err = tx.QueryRowContext(ctx, `SELECT conversation_id,status,last_sequence FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&conversationID, &status, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "running" && status != "cancelling" {
		return fmt.Errorf("turn %s is %s", turnID, status)
	}
	sequence++
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, kind, []byte(details), at); err != nil {
		return err
	}
	if err = projectStepEvent(ctx, tx, turnID, kind, details, at); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_turns SET last_sequence=?,updated_at=? WHERE id=?`, sequence, at, turnID); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveAgentTurnCheckpoint commits the continuation state and its trace event in
// one SQLite transaction. A tool.completed event must never become durable
// without the corresponding tool result in the checkpoint (or vice versa).
func (s *Store) SaveAgentTurnCheckpoint(ctx context.Context, userID, turnID, kind string, details, stateCipher, stateNonce []byte, version int, resumeAllowed bool, at time.Time) error {
	if kind == "" || version != 1 || len(stateCipher) == 0 || len(stateNonce) == 0 || !json.Valid(details) {
		return domain.ErrInvalid
	}
	if len(stateCipher) > (16<<20)+(1<<10) {
		return fmt.Errorf("agent continuation checkpoint exceeds the 16 MiB limit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var conversationID, status string
	var sequence int
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id,status,last_sequence FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&conversationID, &status, &sequence); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if status != "running" && status != "cancelling" {
		return fmt.Errorf("turn %s is %s", turnID, status)
	}
	sequence++
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, kind, []byte(details), at); err != nil {
		return err
	}
	if err := projectStepEvent(ctx, tx, turnID, kind, details, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_turn_checkpoints(turn_id,checkpoint_version,state_cipher,state_nonce,resume_allowed,event_sequence,updated_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(turn_id) DO UPDATE SET checkpoint_version=excluded.checkpoint_version,state_cipher=excluded.state_cipher,state_nonce=excluded.state_nonce,resume_allowed=excluded.resume_allowed,event_sequence=excluded.event_sequence,updated_at=excluded.updated_at`, turnID, version, stateCipher, stateNonce, resumeAllowed, sequence, at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_turns SET last_sequence=?,updated_at=? WHERE id=? AND user_id=?`, sequence, at, turnID, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AgentTurnCheckpoint(ctx context.Context, userID, turnID string) (ciphertext, nonce []byte, version int, resumeAllowed bool, err error) {
	var state []byte
	err = s.db.QueryRowContext(ctx, `SELECT c.state_cipher,c.state_nonce,c.checkpoint_version,c.resume_allowed FROM agent_turn_checkpoints c JOIN agent_turns t ON t.id=c.turn_id WHERE c.turn_id=? AND t.user_id=?`, turnID, userID).Scan(&state, &nonce, &version, &resumeAllowed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, 0, false, domain.ErrNotFound
	}
	if err != nil {
		return nil, nil, 0, false, err
	}
	if len(state) == 0 || len(nonce) == 0 {
		return nil, nil, version, false, fmt.Errorf("agent turn %s has a corrupt continuation checkpoint", turnID)
	}
	return state, nonce, version, resumeAllowed, nil
}

func (s *Store) ResumableAgentTurnIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id FROM agent_turns t JOIN agent_turn_checkpoints c ON c.turn_id=t.id
WHERE t.user_id=? AND t.status='interrupted' AND t.recovery_class='checkpoint_resumable' AND c.checkpoint_version=1 AND c.resume_allowed=1 AND length(c.state_cipher)>0 AND length(c.state_nonce)>0 ORDER BY t.started_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var turnID string
		if err := rows.Scan(&turnID); err != nil {
			return nil, err
		}
		ids = append(ids, turnID)
	}
	return ids, rows.Err()
}

// ResumeAgentTurn reopens only a checkpoint-validated interrupted turn and
// records the transition before any further model or tool work begins.
func (s *Store) ResumeAgentTurn(ctx context.Context, userID, turnID string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var conversationID, status, inboxID string
	var sequence int
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id,status,last_sequence,inbox_id FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&conversationID, &status, &sequence, &inboxID); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if status != "interrupted" {
		return fmt.Errorf("turn %s is not interrupted: %w", turnID, domain.ErrConflict)
	}
	var version int
	var resumeAllowed, checkpointStored bool
	if err := tx.QueryRowContext(ctx, `SELECT checkpoint_version,resume_allowed,(length(state_cipher)>0 AND length(state_nonce)>0) FROM agent_turn_checkpoints WHERE turn_id=?`, turnID).Scan(&version, &resumeAllowed, &checkpointStored); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("turn %s has no continuation checkpoint: %w", turnID, domain.ErrConflict)
	} else if err != nil {
		return err
	} else if version != 1 || !resumeAllowed || !checkpointStored {
		return fmt.Errorf("turn %s uses an unsupported checkpoint version: %w", turnID, domain.ErrConflict)
	}
	sequence++
	details, err := json.Marshal(map[string]string{"reason": "host_restart"})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, "turn.resumed", details, at); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_turns SET status='running',recovery_class='',stop_reason='',completed_at=NULL,last_sequence=?,updated_at=? WHERE id=? AND user_id=? AND status='interrupted'`, sequence, at, turnID, userID)
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
	if inboxID != "" {
		result, err := tx.ExecContext(ctx, `UPDATE agent_inbox SET status='claimed',updated_at=? WHERE id=? AND turn_id=? AND status='interrupted'`, at, inboxID, turnID)
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("claimed inbox for resumable turn %s changed unexpectedly", turnID)
		}
	}
	return tx.Commit()
}

func projectStepEvent(ctx context.Context, tx *sql.Tx, turnID, kind string, details json.RawMessage, at time.Time) error {
	var event struct {
		Step          int               `json:"step"`
		Model         string            `json:"model"`
		ToolCallCount int               `json:"toolCallCount"`
		Usage         domain.RunMetrics `json:"usage"`
	}
	if json.Unmarshal(details, &event) != nil || event.Step < 1 {
		return nil
	}
	stepID := fmt.Sprintf("%s_step_%03d", turnID, event.Step)
	switch kind {
	case "model.requested":
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_steps(id,turn_id,ordinal,status,attempt_count,started_at) VALUES(?,?,?,'running',1,?)
ON CONFLICT(turn_id,ordinal) DO UPDATE SET status='running',attempt_count=agent_steps.attempt_count+1,completed_at=NULL`, stepID, turnID, event.Step, at); err != nil {
			return err
		}
		var attempt int
		if err := tx.QueryRowContext(ctx, `SELECT attempt_count FROM agent_steps WHERE id=?`, stepID).Scan(&attempt); err != nil {
			return err
		}
		attemptID := fmt.Sprintf("%s_attempt_%03d", stepID, attempt)
		_, err := tx.ExecContext(ctx, `INSERT INTO agent_model_attempts(id,turn_id,step_id,ordinal,status,started_at) VALUES(?,?,?,?, 'running',?)`, attemptID, turnID, stepID, attempt, at)
		return err
	case "model.completed":
		usage, _ := json.Marshal(event.Usage)
		if _, err := tx.ExecContext(ctx, `UPDATE agent_model_attempts SET status='completed',model=?,usage_json=?,completed_at=? WHERE step_id=? AND status='running'`, event.Model, usage, at, stepID); err != nil {
			return err
		}
		stepStatus := "completed"
		var completedAt any = at
		if event.ToolCallCount > 0 {
			stepStatus = "awaiting_tools"
			completedAt = nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status=?,completed_at=? WHERE id=?`, stepStatus, completedAt, stepID)
		return err
	case "model.failed":
		if _, err := tx.ExecContext(ctx, `UPDATE agent_model_attempts SET status='failed',completed_at=? WHERE step_id=? AND status='running'`, at, stepID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status='failed',completed_at=? WHERE id=?`, at, stepID)
		return err
	case "tools.completed":
		_, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status='completed',completed_at=? WHERE id=?`, at, stepID)
		return err
	}
	return nil
}

func (s *Store) FinishAgentTurn(ctx context.Context, userID, turnID, status, stopReason string, output *domain.Message, details json.RawMessage) error {
	return s.FinishAgentTurnWithContinuation(ctx, userID, turnID, status, stopReason, output, details, nil)
}

// FinishAgentTurnWithContinuation atomically closes a Turn and persists its
// encrypted budget continuation state, when the runtime has a safe checkpoint.
func (s *Store) FinishAgentTurnWithContinuation(ctx context.Context, userID, turnID, status, stopReason string, output *domain.Message, details json.RawMessage, continuation *ContinuationState) error {
	if status != "completed" && status != "incomplete" && status != "failed" && status != "cancelled" {
		return domain.ErrInvalid
	}
	if continuation != nil {
		if status != "incomplete" || continuation.Version != 1 || (len(continuation.Ciphertext) == 0) != (len(continuation.Nonce) == 0) || (len(continuation.Ciphertext) > 0 && continuation.ContentHash == "") || (len(continuation.Ciphertext) == 0 && continuation.UnavailableReason == "") {
			return domain.ErrInvalid
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var conversationID, current string
	var sequence int
	if err = tx.QueryRowContext(ctx, `SELECT conversation_id,status,last_sequence FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&conversationID, &current, &sequence); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if current != "running" && current != "cancelling" && current != "awaiting_approval" {
		return fmt.Errorf("turn %s is already %s", turnID, current)
	}
	now := time.Now().UTC()
	if continuation != nil {
		continuationStatus := "available"
		if len(continuation.Ciphertext) == 0 {
			continuationStatus = "unavailable"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_continuation_snapshots(source_turn_id,user_id,conversation_id,snapshot_version,state_cipher,state_nonce,content_sha256,status,unavailable_reason,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, turnID, userID, conversationID, continuation.Version, continuation.Ciphertext, continuation.Nonce, continuation.ContentHash, continuationStatus, continuation.UnavailableReason, now, now); err != nil {
			return err
		}
	}
	resultMessageID := ""
	if output != nil {
		resultMessageID = output.ID
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) VALUES(?,?,?,?,?)`, output.ID, conversationID, output.Role, output.Content, output.CreatedAt); err != nil {
			return err
		}
	}
	terminalRecoveryClass := ""
	sequence++
	kind := "turn." + status
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, kind, []byte(details), now); err != nil {
		return err
	}
	if continuation != nil {
		continuationStatus := "available"
		if len(continuation.Ciphertext) == 0 {
			continuationStatus = "unavailable"
		}
		continuationDetails, marshalErr := json.Marshal(map[string]string{"status": continuationStatus, "reason": continuation.UnavailableReason})
		if marshalErr != nil {
			return marshalErr
		}
		sequence++
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, "turn.continuation_saved", continuationDetails, now); err != nil {
			return err
		}
	}
	childStatus := status
	if status == "completed" {
		childStatus = "abandoned"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_model_attempts SET status=?,completed_at=? WHERE turn_id=? AND status='running'`, childStatus, now, turnID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_steps SET status=?,completed_at=? WHERE turn_id=? AND status IN ('running','awaiting_tools')`, childStatus, now, turnID); err != nil {
		return err
	}
	updatedTurn, err := tx.ExecContext(ctx, `UPDATE agent_turns SET status=?,stop_reason=?,result_message_id=NULLIF(?,''),
	recovery_class=?,last_sequence=?,updated_at=?,completed_at=? WHERE id=? AND user_id=? AND status IN ('running','cancelling','awaiting_approval')`, status, stopReason, resultMessageID, terminalRecoveryClass, sequence, now, now, turnID, userID)
	if err != nil {
		return err
	}
	turnCount, err := updatedTurn.RowsAffected()
	if err != nil {
		return err
	}
	if turnCount != 1 {
		return domain.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_approvals SET status='cancelled',decision='cancelled',resolved_at=? WHERE turn_id=? AND status='pending'`, now, turnID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_inbox SET status=?,updated_at=? WHERE turn_id=? AND status='claimed'`, status, now, turnID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM agent_turn_checkpoints WHERE turn_id=?`, turnID); err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, now, conversationID, userID)
	if err != nil {
		return err
	}
	updatedCount, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if updatedCount != 1 {
		return domain.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if continuation != nil {
		var persistedStatus, persistedHash string
		if err := s.db.QueryRowContext(ctx, `SELECT status,content_sha256 FROM agent_continuation_snapshots WHERE source_turn_id=? AND user_id=?`, turnID, userID).Scan(&persistedStatus, &persistedHash); err != nil {
			return err
		}
		expectedStatus := "available"
		if len(continuation.Ciphertext) == 0 {
			expectedStatus = "unavailable"
		}
		if persistedStatus != expectedStatus || persistedHash != continuation.ContentHash {
			return fmt.Errorf("continuation snapshot read-back mismatch for turn %q", turnID)
		}
	}
	return nil
}

func (s *Store) AgentContinuationSnapshot(ctx context.Context, userID, turnID string) (AgentContinuationSnapshot, error) {
	var snapshot AgentContinuationSnapshot
	err := s.db.QueryRowContext(ctx, `SELECT source_turn_id,conversation_id,snapshot_version,state_cipher,state_nonce,content_sha256,status,unavailable_reason,idempotency_key,consumed_turn_id
FROM agent_continuation_snapshots WHERE source_turn_id=? AND user_id=?`, turnID, userID).
		Scan(&snapshot.SourceTurnID, &snapshot.ConversationID, &snapshot.Version, &snapshot.Ciphertext, &snapshot.Nonce, &snapshot.ContentHash, &snapshot.Status, &snapshot.UnavailableReason, &snapshot.IdempotencyKey, &snapshot.ConsumedTurnID)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, domain.ErrNotFound
	}
	return snapshot, err
}

// RecordContinuationIntent persists the model's routing decision against the
// checkpoint it inspected. The checkpoint remains available until the
// continuation transaction consumes it.
func (s *Store) RecordContinuationIntent(ctx context.Context, userID, turnID string, details json.RawMessage) error {
	if userID == "" || turnID == "" || !json.Valid(details) {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var conversationID, turnStatus, snapshotStatus string
	var sequence int
	err = tx.QueryRowContext(ctx, `SELECT t.conversation_id,t.status,t.last_sequence,s.status
FROM agent_turns t JOIN agent_continuation_snapshots s ON s.source_turn_id=t.id AND s.user_id=t.user_id
WHERE t.id=? AND t.user_id=?`, turnID, userID).Scan(&conversationID, &turnStatus, &sequence, &snapshotStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if turnStatus != "incomplete" || snapshotStatus != "available" {
		return domain.ErrConflict
	}
	now := time.Now().UTC()
	sequence++
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, "continuation.intent_classified", []byte(details), now); err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE agent_turns SET last_sequence=?,updated_at=? WHERE id=? AND user_id=? AND status='incomplete'`, sequence, now, turnID, userID)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var persistedKind string
	var persistedDetails []byte
	var persistedSequence int
	err = s.db.QueryRowContext(ctx, `SELECT e.kind,e.details_json,e.sequence FROM agent_trace_events e JOIN agent_turns t ON t.id=e.turn_id WHERE e.turn_id=? AND t.user_id=? AND e.sequence=?`, turnID, userID, sequence).Scan(&persistedKind, &persistedDetails, &persistedSequence)
	if err != nil {
		return err
	}
	if persistedKind != "continuation.intent_classified" || persistedSequence != sequence || string(persistedDetails) != string(details) {
		return fmt.Errorf("continuation intent event read-back mismatch for turn %q", turnID)
	}
	return nil
}

func (s *Store) InvalidateAgentContinuationSnapshot(ctx context.Context, userID, turnID, reason string) error {
	if userID == "" || turnID == "" || reason == "" || len(reason) > 120 {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var conversationID, status string
	var sequence int
	if err := tx.QueryRowContext(ctx, `SELECT s.conversation_id,s.status,t.last_sequence FROM agent_continuation_snapshots s JOIN agent_turns t ON t.id=s.source_turn_id WHERE s.source_turn_id=? AND s.user_id=?`, turnID, userID).Scan(&conversationID, &status, &sequence); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if status == "invalidated" {
		var persistedReason string
		if err := tx.QueryRowContext(ctx, `SELECT unavailable_reason FROM agent_continuation_snapshots WHERE source_turn_id=? AND user_id=?`, turnID, userID).Scan(&persistedReason); err != nil {
			return err
		}
		if persistedReason != reason {
			return domain.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return s.verifyInvalidatedContinuation(ctx, userID, turnID, reason)
	}
	if status != "available" {
		return domain.ErrConflict
	}
	sequence++
	details, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE agent_continuation_snapshots SET status='invalidated',unavailable_reason=?,state_cipher=X'',state_nonce=X'',updated_at=? WHERE source_turn_id=? AND user_id=? AND status='available'`, reason, time.Now().UTC(), turnID, userID)
	if err != nil {
		return err
	}
	updatedCount, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if updatedCount != 1 {
		return domain.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, "turn.continuation_invalidated", details, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_turns SET last_sequence=?,updated_at=? WHERE id=? AND user_id=?`, sequence, time.Now().UTC(), turnID, userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.verifyInvalidatedContinuation(ctx, userID, turnID, reason)
}

func (s *Store) verifyInvalidatedContinuation(ctx context.Context, userID, turnID, reason string) error {
	var persistedStatus, persistedReason string
	if err := s.db.QueryRowContext(ctx, `SELECT status,unavailable_reason FROM agent_continuation_snapshots WHERE source_turn_id=? AND user_id=?`, turnID, userID).Scan(&persistedStatus, &persistedReason); err != nil {
		return err
	}
	if persistedStatus != "invalidated" || persistedReason != reason {
		return fmt.Errorf("continuation invalidation read-back mismatch for turn %q", turnID)
	}
	return nil
}

// StartContinuationAgentTurn consumes one available snapshot and creates the
// child Turn, input message, initial encrypted checkpoint, and events in one
// transaction. Repeating the same idempotency key returns the existing child.
func (s *Store) StartContinuationAgentTurn(ctx context.Context, userID, sourceTurnID, idempotencyKey string, turn domain.AgentTurn, input domain.Message, details json.RawMessage, checkpointCipher, checkpointNonce []byte) (domain.TurnReceipt, bool, error) {
	if userID == "" || sourceTurnID == "" || idempotencyKey == "" || len(idempotencyKey) > 200 || turn.ID == "" || turn.UserID != userID || turn.InputMessageID != input.ID || input.ConversationID != turn.ConversationID || input.ID == "" || input.Role != "user" || strings.TrimSpace(input.Content) == "" || !json.Valid(details) || len(checkpointCipher) == 0 || len(checkpointNonce) == 0 || len(checkpointCipher) > (16<<20)+(1<<10) {
		return domain.TurnReceipt{}, false, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.TurnReceipt{}, false, err
	}
	defer tx.Rollback()
	var conversationID, status, snapshotStatus, snapshotKey, consumedTurnID string
	var sourceProvider, sourceGeneration, sourceDigest, sourceProfile, sourceChainID string
	var sourceSequence int
	err = tx.QueryRowContext(ctx, `SELECT s.conversation_id,t.status,s.status,s.idempotency_key,s.consumed_turn_id,t.provider_id,t.generation_id,t.definition_digest,t.permission_profile,t.continuation_chain_id,t.last_sequence
FROM agent_continuation_snapshots s JOIN agent_turns t ON t.id=s.source_turn_id
WHERE s.source_turn_id=? AND s.user_id=?`, sourceTurnID, userID).
		Scan(&conversationID, &status, &snapshotStatus, &snapshotKey, &consumedTurnID, &sourceProvider, &sourceGeneration, &sourceDigest, &sourceProfile, &sourceChainID, &sourceSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TurnReceipt{}, false, domain.ErrNotFound
	}
	if err != nil {
		return domain.TurnReceipt{}, false, err
	}
	if snapshotStatus == "consumed" {
		if snapshotKey != idempotencyKey || consumedTurnID == "" {
			return domain.TurnReceipt{}, false, domain.ErrConflict
		}
		var receipt domain.TurnReceipt
		err = tx.QueryRowContext(ctx, `SELECT id,conversation_id,input_message_id,status,continued_from_turn_id,continuation_chain_id FROM agent_turns WHERE id=? AND user_id=?`, consumedTurnID, userID).
			Scan(&receipt.TurnID, &receipt.ConversationID, &receipt.InputMessageID, &receipt.Status, &receipt.ContinuedFromTurnID, &receipt.ContinuationChainID)
		if err != nil {
			return domain.TurnReceipt{}, false, err
		}
		return receipt, false, nil
	}
	if snapshotStatus != "available" || status != "incomplete" || conversationID != turn.ConversationID || turn.ProviderID != sourceProvider || turn.AgentGenerationID != sourceGeneration || turn.AgentDefinitionDigest != sourceDigest || string(turn.PermissionProfile) != sourceProfile {
		return domain.TurnReceipt{}, false, fmt.Errorf("%w: continuation snapshot is unavailable or its execution binding changed", domain.ErrConflict)
	}
	var latestMessageID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE conversation_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1`, conversationID).Scan(&latestMessageID); err != nil {
		return domain.TurnReceipt{}, false, err
	}
	var sourceResultID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(result_message_id,'') FROM agent_turns WHERE id=? AND user_id=?`, sourceTurnID, userID).Scan(&sourceResultID); err != nil {
		return domain.TurnReceipt{}, false, err
	}
	if sourceResultID == "" || latestMessageID != sourceResultID {
		reason := "conversation_changed"
		details, marshalErr := json.Marshal(map[string]string{"reason": reason})
		if marshalErr != nil {
			return domain.TurnReceipt{}, false, marshalErr
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE agent_continuation_snapshots SET status='invalidated',unavailable_reason=?,state_cipher=X'',state_nonce=X'',updated_at=? WHERE source_turn_id=? AND user_id=? AND status='available'`, reason, now, sourceTurnID, userID); err != nil {
			return domain.TurnReceipt{}, false, err
		}
		sourceSequence++
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(sourceTurnID, sourceSequence), conversationID, sourceTurnID, sourceSequence, "turn.continuation_invalidated", details, now); err != nil {
			return domain.TurnReceipt{}, false, err
		}
		updatedSource, err := tx.ExecContext(ctx, `UPDATE agent_turns SET last_sequence=?,updated_at=? WHERE id=? AND user_id=?`, sourceSequence, now, sourceTurnID, userID)
		if err != nil {
			return domain.TurnReceipt{}, false, err
		}
		updatedCount, err := updatedSource.RowsAffected()
		if err != nil {
			return domain.TurnReceipt{}, false, err
		}
		if updatedCount != 1 {
			return domain.TurnReceipt{}, false, domain.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return domain.TurnReceipt{}, false, err
		}
		if err := s.verifyInvalidatedContinuation(ctx, userID, sourceTurnID, reason); err != nil {
			return domain.TurnReceipt{}, false, err
		}
		return domain.TurnReceipt{}, false, fmt.Errorf("%w: conversation changed after the incomplete Turn", domain.ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) VALUES(?,?,?,?,?)`, input.ID, conversationID, input.Role, input.Content, input.CreatedAt); err != nil {
		return domain.TurnReceipt{}, false, err
	}
	profile := turn.PermissionProfile
	if !profile.Valid() {
		return domain.TurnReceipt{}, false, domain.ErrInvalid
	}
	chainID := sourceChainID
	if chainID == "" {
		chainID = sourceTurnID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_turns(id,conversation_id,user_id,input_message_id,provider_id,generation_id,definition_digest,permission_profile,retry_of_turn_id,continued_from_turn_id,continuation_chain_id,input_content_snapshot,inbox_id,status,last_sequence,started_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,'',?,?,?,?,'running',2,?,?)`, turn.ID, conversationID, userID, input.ID, turn.ProviderID, turn.AgentGenerationID, turn.AgentDefinitionDigest, profile, sourceTurnID, chainID, input.Content, "", turn.StartedAt, turn.UpdatedAt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.TurnReceipt{}, false, domain.ErrConflict
		}
		return domain.TurnReceipt{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turn.ID, 1), conversationID, turn.ID, 1, "turn.started", []byte(details), turn.StartedAt); err != nil {
		return domain.TurnReceipt{}, false, err
	}
	continuedDetails, err := json.Marshal(map[string]string{"continuedFromTurnId": sourceTurnID})
	if err != nil {
		return domain.TurnReceipt{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turn.ID, 2), conversationID, turn.ID, 2, "turn.continued", continuedDetails, turn.StartedAt); err != nil {
		return domain.TurnReceipt{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_turn_checkpoints(turn_id,checkpoint_version,state_cipher,state_nonce,resume_allowed,event_sequence,updated_at) VALUES(?,1,?,?,1,2,?)`, turn.ID, checkpointCipher, checkpointNonce, turn.StartedAt); err != nil {
		return domain.TurnReceipt{}, false, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE agent_continuation_snapshots SET status='consumed',idempotency_key=?,consumed_turn_id=?,updated_at=? WHERE source_turn_id=? AND user_id=? AND status='available'`, idempotencyKey, turn.ID, turn.StartedAt, sourceTurnID, userID)
	if err != nil {
		return domain.TurnReceipt{}, false, err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return domain.TurnReceipt{}, false, err
	}
	if count != 1 {
		return domain.TurnReceipt{}, false, domain.ErrConflict
	}
	conversationUpdate, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, turn.StartedAt, conversationID, userID)
	if err != nil {
		return domain.TurnReceipt{}, false, err
	}
	conversationCount, err := conversationUpdate.RowsAffected()
	if err != nil {
		return domain.TurnReceipt{}, false, err
	}
	if conversationCount != 1 {
		return domain.TurnReceipt{}, false, domain.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return domain.TurnReceipt{}, false, err
	}
	var persistedStatus, persistedChild string
	if err := s.db.QueryRowContext(ctx, `SELECT s.status,s.consumed_turn_id FROM agent_continuation_snapshots s WHERE s.source_turn_id=? AND s.user_id=?`, sourceTurnID, userID).Scan(&persistedStatus, &persistedChild); err != nil {
		return domain.TurnReceipt{}, false, err
	}
	if persistedStatus != "consumed" || persistedChild != turn.ID {
		return domain.TurnReceipt{}, false, fmt.Errorf("continuation consumption read-back mismatch")
	}
	var receipt domain.TurnReceipt
	if err := s.db.QueryRowContext(ctx, `SELECT id,conversation_id,input_message_id,status,continued_from_turn_id,continuation_chain_id FROM agent_turns WHERE id=? AND user_id=?`, turn.ID, userID).
		Scan(&receipt.TurnID, &receipt.ConversationID, &receipt.InputMessageID, &receipt.Status, &receipt.ContinuedFromTurnID, &receipt.ContinuationChainID); err != nil {
		return domain.TurnReceipt{}, false, err
	}
	if receipt.Status != "running" || receipt.ConversationID != conversationID || receipt.InputMessageID != input.ID || receipt.ContinuedFromTurnID != sourceTurnID || receipt.ContinuationChainID != chainID {
		return domain.TurnReceipt{}, false, fmt.Errorf("continued Turn read-back mismatch for source %q", sourceTurnID)
	}
	return receipt, true, nil
}

func (s *Store) RequestAgentTurnCancel(ctx context.Context, userID, turnID, reason string) error {
	raw, _ := json.Marshal(map[string]any{"reason": reason})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var conversationID, status string
	var sequence int
	if err = tx.QueryRowContext(ctx, `SELECT conversation_id,status,last_sequence FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&conversationID, &status, &sequence); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if status != "running" && status != "cancelling" && status != "awaiting_approval" {
		return fmt.Errorf("%w: turn %s is already %s", domain.ErrConflict, turnID, status)
	}
	if status == "cancelling" {
		return nil
	}
	sequence++
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, "turn.cancel_requested", raw, now); err != nil {
		return err
	}
	updatedTurn, err := tx.ExecContext(ctx, `UPDATE agent_turns SET status='cancelling',cancel_requested=1,last_sequence=?,updated_at=? WHERE id=? AND user_id=? AND status IN ('running','awaiting_approval')`, sequence, now, turnID, userID)
	if err != nil {
		return err
	}
	turnCount, err := updatedTurn.RowsAffected()
	if err != nil {
		return err
	}
	if turnCount != 1 {
		return domain.ErrConflict
	}
	updated, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, now, conversationID, userID)
	if err != nil {
		return err
	}
	updatedCount, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if updatedCount != 1 {
		return domain.ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) CancelConversationAgentWork(ctx context.Context, userID, conversationID, reason string) (domain.ConversationCancelReceipt, error) {
	receipt := domain.ConversationCancelReceipt{ConversationID: conversationID}
	details, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	defer tx.Rollback()
	var wasPaused bool
	if err := tx.QueryRowContext(ctx, `SELECT execution_paused FROM conversations WHERE id=? AND user_id=?`, conversationID, userID).Scan(&wasPaused); errors.Is(err, sql.ErrNoRows) {
		return domain.ConversationCancelReceipt{}, domain.ErrNotFound
	} else if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	var turnID, status string
	var sequence int
	err = tx.QueryRowContext(ctx, `SELECT id,status,last_sequence FROM agent_turns WHERE user_id=? AND conversation_id=? AND status IN ('running','cancelling','awaiting_approval') LIMIT 1`, userID, conversationID).Scan(&turnID, &status, &sequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.ConversationCancelReceipt{}, err
	}
	if err == nil {
		receipt.CancelledTurnID = turnID
		if status != "cancelling" {
			sequence++
			now := time.Now().UTC()
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, "turn.cancel_requested", details, now); err != nil {
				return domain.ConversationCancelReceipt{}, err
			}
			result, err := tx.ExecContext(ctx, `UPDATE agent_turns SET status='cancelling',cancel_requested=1,last_sequence=?,updated_at=? WHERE id=? AND user_id=? AND status IN ('running','awaiting_approval')`, sequence, now, turnID, userID)
			if err != nil {
				return domain.ConversationCancelReceipt{}, err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return domain.ConversationCancelReceipt{}, err
			}
			if count != 1 {
				return domain.ConversationCancelReceipt{}, domain.ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agent_approvals SET status='cancelled',decision='cancelled',resolved_at=? WHERE turn_id=? AND status='pending'`, now, turnID); err != nil {
				return domain.ConversationCancelReceipt{}, err
			}
		}
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE agent_inbox SET status='cancelled',updated_at=? WHERE user_id=? AND conversation_id=? AND status='queued'`, now, userID, conversationID)
	if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	receipt.CancelledInboxCount, err = result.RowsAffected()
	if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=?,execution_paused=1 WHERE id=? AND user_id=?`, now, conversationID, userID)
	if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	updatedCount, err := updated.RowsAffected()
	if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	if updatedCount != 1 {
		return domain.ConversationCancelReceipt{}, domain.ErrNotFound
	}
	if !wasPaused {
		pauseDetails, err := json.Marshal(map[string]any{"reason": reason, "cancelledTurnId": receipt.CancelledTurnID, "cancelledInboxCount": receipt.CancelledInboxCount})
		if err != nil {
			return domain.ConversationCancelReceipt{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_lifecycle_events(id,conversation_id,kind,details_json,created_at) VALUES(?,?,?,?,?)`, fmt.Sprintf("conversation_paused_%s_%d", conversationID, now.UnixNano()), conversationID, "conversation.paused", pauseDetails, now); err != nil {
			return domain.ConversationCancelReceipt{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) AgentTurns(ctx context.Context, userID, conversationID string) ([]domain.AgentTurn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,t.conversation_id,t.user_id,t.input_message_id,COALESCE(t.result_message_id,''),COALESCE(output.role,''),COALESCE(output.content,''),t.provider_id,t.generation_id,t.definition_digest,t.permission_profile,t.status,t.stop_reason,t.recovery_class,t.cancel_requested,t.last_sequence,t.started_at,t.updated_at,t.completed_at,COALESCE(t.retry_of_turn_id,''),COALESCE(r.note,''),COALESCE(t.continued_from_turn_id,''),COALESCE(NULLIF(t.continuation_chain_id,''),CASE WHEN s.source_turn_id IS NOT NULL THEN t.id ELSE '' END),COALESCE(s.status='available',0),COALESCE(s.unavailable_reason,'')
FROM agent_turns t LEFT JOIN messages output ON output.id=t.result_message_id AND output.conversation_id=t.conversation_id LEFT JOIN agent_turn_reconciliations r ON r.turn_id=t.id AND r.user_id=t.user_id LEFT JOIN agent_continuation_snapshots s ON s.source_turn_id=t.id AND s.user_id=t.user_id WHERE t.user_id=? AND t.conversation_id=? ORDER BY t.started_at DESC`, userID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.AgentTurn{}
	for rows.Next() {
		var turn domain.AgentTurn
		var completed sql.NullTime
		var outputRole, outputContent string
		if err := rows.Scan(&turn.ID, &turn.ConversationID, &turn.UserID, &turn.InputMessageID, &turn.ResultMessageID, &outputRole, &outputContent, &turn.ProviderID, &turn.AgentGenerationID, &turn.AgentDefinitionDigest, &turn.PermissionProfile, &turn.Status, &turn.StopReason, &turn.RecoveryClass, &turn.CancelRequested, &turn.LastSequence, &turn.StartedAt, &turn.UpdatedAt, &completed, &turn.RetryOfTurnID, &turn.ReconciliationNote, &turn.ContinuedFromTurnID, &turn.ContinuationChainID, &turn.ContinuationAvailable, &turn.ContinuationUnavailableReason); err != nil {
			return nil, err
		}
		turn.CompletionAssessment = completionAssessmentFromOutput(turn.ResultMessageID, outputRole, outputContent)
		if completed.Valid {
			turn.CompletedAt = &completed.Time
		}
		result = append(result, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range result {
		evidenceRefs, err := s.agentTurnToolEvidenceRefs(ctx, result[index].ID)
		if err != nil {
			return nil, err
		}
		result[index].CompletionAssessment.EvidenceRefs = evidenceRefs
		result[index].RunState, err = s.agentTurnRunState(ctx, result[index])
		if err != nil {
			return nil, err
		}
		if result[index].ContinuationChainID == "" {
			continue
		}
		metrics, err := s.ContinuationChainMetrics(ctx, userID, result[index].ContinuationChainID)
		if err != nil {
			return nil, err
		}
		result[index].CumulativeMetrics = metrics
	}
	return result, nil
}

func (s *Store) AgentTurn(ctx context.Context, userID, turnID string) (domain.AgentTurn, error) {
	var turn domain.AgentTurn
	var completed sql.NullTime
	var outputRole, outputContent string
	err := s.db.QueryRowContext(ctx, `SELECT t.id,t.conversation_id,t.user_id,t.input_message_id,COALESCE(t.result_message_id,''),COALESCE(output.role,''),COALESCE(output.content,''),t.provider_id,t.generation_id,t.definition_digest,t.permission_profile,t.status,t.stop_reason,t.recovery_class,t.cancel_requested,t.last_sequence,t.started_at,t.updated_at,t.completed_at,COALESCE(t.retry_of_turn_id,''),COALESCE(r.note,''),COALESCE(t.continued_from_turn_id,''),COALESCE(NULLIF(t.continuation_chain_id,''),CASE WHEN s.source_turn_id IS NOT NULL THEN t.id ELSE '' END),COALESCE(s.status='available',0),COALESCE(s.unavailable_reason,'')
FROM agent_turns t LEFT JOIN messages output ON output.id=t.result_message_id AND output.conversation_id=t.conversation_id LEFT JOIN agent_turn_reconciliations r ON r.turn_id=t.id AND r.user_id=t.user_id LEFT JOIN agent_continuation_snapshots s ON s.source_turn_id=t.id AND s.user_id=t.user_id WHERE t.id=? AND t.user_id=?`, turnID, userID).
		Scan(&turn.ID, &turn.ConversationID, &turn.UserID, &turn.InputMessageID, &turn.ResultMessageID, &outputRole, &outputContent, &turn.ProviderID, &turn.AgentGenerationID, &turn.AgentDefinitionDigest, &turn.PermissionProfile, &turn.Status, &turn.StopReason, &turn.RecoveryClass, &turn.CancelRequested, &turn.LastSequence, &turn.StartedAt, &turn.UpdatedAt, &completed, &turn.RetryOfTurnID, &turn.ReconciliationNote, &turn.ContinuedFromTurnID, &turn.ContinuationChainID, &turn.ContinuationAvailable, &turn.ContinuationUnavailableReason)
	if errors.Is(err, sql.ErrNoRows) {
		return turn, domain.ErrNotFound
	}
	if err != nil {
		return turn, err
	}
	if completed.Valid {
		turn.CompletedAt = &completed.Time
	}
	turn.CompletionAssessment = completionAssessmentFromOutput(turn.ResultMessageID, outputRole, outputContent)
	turn.CompletionAssessment.EvidenceRefs, err = s.agentTurnToolEvidenceRefs(ctx, turn.ID)
	if err != nil {
		return turn, err
	}
	turn.RunState, err = s.agentTurnRunState(ctx, turn)
	if err != nil {
		return turn, err
	}
	if turn.ContinuationChainID != "" {
		turn.CumulativeMetrics, err = s.ContinuationChainMetrics(ctx, userID, turn.ContinuationChainID)
		if err != nil {
			return turn, err
		}
	}
	return turn, nil
}

func completionAssessmentFromOutput(messageID, role, content string) domain.CompletionAssessment {
	assessment := domain.CompletionAssessment{Status: "unassessed"}
	if messageID != "" && role == "assistant" {
		assessment.Status = "unverified"
		assessment.AssistantSourceRef = "message:" + messageID + "#" + messageContentHash(content)
	}
	return assessment
}

func (s *Store) agentTurnRunState(ctx context.Context, turn domain.AgentTurn) (domain.RunState, error) {
	state := domain.RunState{
		Version: 1, Completeness: "complete",
		CompletenessNote: "execution observations only; tool outcomes do not verify that the user's goal was achieved",
		Actions:          []domain.RunStateAction{}, PendingActions: []domain.RunStateAction{},
	}
	var startDetails []byte
	err := s.db.QueryRowContext(ctx, `SELECT e.details_json FROM agent_trace_events e JOIN agent_turns t ON t.id=e.turn_id WHERE e.turn_id=? AND t.user_id=? AND e.kind='turn.started' ORDER BY e.sequence LIMIT 1`, turn.ID, turn.UserID).Scan(&startDetails)
	if errors.Is(err, sql.ErrNoRows) {
		state.Completeness = "degraded"
		state.CompletenessNote = "turn.started source details are missing"
	} else if err != nil {
		return state, err
	} else if !json.Valid(startDetails) {
		return state, fmt.Errorf("Turn %q has invalid turn.started details", turn.ID)
	} else {
		var details struct {
			InputSourceRef string `json:"inputSourceRef"`
		}
		if err := json.Unmarshal(startDetails, &details); err != nil {
			return state, err
		}
		if details.InputSourceRef != "" {
			if !strings.HasPrefix(details.InputSourceRef, "message:"+turn.InputMessageID+"#") {
				return state, fmt.Errorf("Turn %q input source does not match its input message", turn.ID)
			}
			content, _, readErr := s.ReadContextSource(ctx, turn.UserID, turn.ConversationID, details.InputSourceRef)
			if errors.Is(readErr, domain.ErrNotFound) {
				state.CurrentRequest = &domain.RunStateFact{ID: "request:" + turn.InputMessageID, Kind: "current_request", Status: "active", SourceKind: "user_message", Verification: "source_missing", SourceRef: details.InputSourceRef}
				state.Completeness = "degraded"
				state.CompletenessNote = "input message source is unavailable"
			} else if readErr != nil {
				return state, readErr
			} else {
				state.CurrentRequest = &domain.RunStateFact{ID: "request:" + turn.InputMessageID, Kind: "current_request", Statement: content, Status: "active", SourceKind: "user_message", Verification: "source_available", SourceRef: details.InputSourceRef}
			}
		} else {
			var content string
			readErr := s.db.QueryRowContext(ctx, `SELECT content FROM messages WHERE id=? AND conversation_id=? AND role='user'`, turn.InputMessageID, turn.ConversationID).Scan(&content)
			if errors.Is(readErr, sql.ErrNoRows) {
				state.Completeness = "degraded"
				state.CompletenessNote = "legacy turn has no input source reference and its message is unavailable"
			} else if readErr != nil {
				return state, readErr
			} else {
				state.CurrentRequest = &domain.RunStateFact{ID: "request:" + turn.InputMessageID, Kind: "current_request", Statement: content, Status: "active", SourceKind: "user_message", Verification: "legacy_current_message_only", SourceRef: "message:" + turn.InputMessageID + "#" + messageContentHash(content)}
				state.Completeness = "degraded"
				state.CompletenessNote = "legacy turn did not pin the exact input revision in its start event"
			}
		}
	}

	if turn.ResultMessageID != "" {
		var role, content string
		readErr := s.db.QueryRowContext(ctx, `SELECT role,content FROM messages WHERE id=? AND conversation_id=?`, turn.ResultMessageID, turn.ConversationID).Scan(&role, &content)
		if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
			return state, readErr
		}
		if readErr == nil && role == "assistant" {
			state.AssistantResponse = &domain.RunStateFact{ID: "assistant-response:" + turn.ResultMessageID, Kind: "assistant_response", Status: "assistant_reported", SourceKind: "assistant_message", Verification: "unverified", SourceRef: "message:" + turn.ResultMessageID + "#" + messageContentHash(content)}
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT e.sequence,e.kind,e.details_json FROM agent_trace_events e JOIN agent_turns t ON t.id=e.turn_id WHERE e.turn_id=? AND t.user_id=? AND e.kind IN ('tool.started','tool.completed') ORDER BY e.sequence`, turn.ID, turn.UserID)
	if err != nil {
		return state, err
	}
	type traceRow struct {
		sequence int
		kind     string
		details  []byte
	}
	events := []traceRow{}
	for rows.Next() {
		var event traceRow
		if err := rows.Scan(&event.sequence, &event.kind, &event.details); err != nil {
			rows.Close()
			return state, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return state, err
	}
	if err := rows.Close(); err != nil {
		return state, err
	}
	pending := map[string]domain.RunStateAction{}
	for _, event := range events {
		if !json.Valid(event.details) {
			return state, fmt.Errorf("Turn %q has invalid %s details", turn.ID, event.kind)
		}
		var details struct {
			ToolCallID string `json:"toolCallId"`
			Name       string `json:"name"`
			Effect     string `json:"effect"`
			SourceRef  string `json:"sourceRef"`
			OK         *bool  `json:"ok"`
		}
		if err := json.Unmarshal(event.details, &details); err != nil {
			return state, err
		}
		if details.ToolCallID == "" {
			state.Completeness = "degraded"
			state.CompletenessNote = "one or more tool events have no tool call ID"
			continue
		}
		action := domain.RunStateAction{Sequence: event.sequence, ToolCallID: details.ToolCallID, ToolName: details.Name, Effect: details.Effect, Status: "in_flight", SourceAvailable: "not_applicable"}
		if event.kind == "tool.started" {
			pending[details.ToolCallID] = action
			continue
		}
		if details.OK == nil {
			action.Status = "tool_reported_unknown"
			state.Completeness = "degraded"
			state.CompletenessNote = "one or more tool completion events have no outcome"
		} else if *details.OK {
			action.Status = "tool_reported_ok"
		} else {
			action.Status = "tool_reported_failure"
		}
		if started, ok := pending[details.ToolCallID]; ok {
			action.ToolName = details.Name
			if action.ToolName == "" {
				action.ToolName = started.ToolName
			}
			action.Effect = started.Effect
			delete(pending, details.ToolCallID)
		}
		action.SourceRef = details.SourceRef
		if action.SourceRef != "" {
			if action.SourceRef != "tool_result:"+details.ToolCallID {
				return state, fmt.Errorf("Turn %q tool source does not match call %q", turn.ID, details.ToolCallID)
			}
			var sourceType, sourceHash string
			readErr := s.db.QueryRowContext(ctx, `SELECT source_type,content_sha256 FROM context_sources WHERE source_id=? AND conversation_id=? AND user_id=?`, action.SourceRef, turn.ConversationID, turn.UserID).Scan(&sourceType, &sourceHash)
			if errors.Is(readErr, sql.ErrNoRows) {
				action.SourceAvailable = "missing"
				state.Completeness = "degraded"
				state.CompletenessNote = "one or more tool result sources are unavailable"
			} else if readErr != nil {
				return state, readErr
			} else if sourceType != "tool_result" {
				return state, fmt.Errorf("Turn %q source %q is not a tool result", turn.ID, action.SourceRef)
			} else {
				action.SourceAvailable = "available"
				action.SourceHash = sourceHash
			}
		} else {
			action.SourceAvailable = "missing"
			state.Completeness = "degraded"
			state.CompletenessNote = "one or more tool completion events have no source reference"
		}
		state.Actions = append(state.Actions, action)
	}
	for _, action := range pending {
		state.PendingActions = append(state.PendingActions, action)
	}
	sort.Slice(state.PendingActions, func(i, j int) bool { return state.PendingActions[i].Sequence < state.PendingActions[j].Sequence })
	return state, nil
}

func (s *Store) agentTurnToolEvidenceRefs(ctx context.Context, turnID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.details_json,e.conversation_id,t.user_id FROM agent_trace_events e JOIN agent_turns t ON t.id=e.turn_id WHERE e.turn_id=? AND e.kind='tool.completed' ORDER BY e.sequence`, turnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type evidenceCandidate struct{ ref, conversationID, userID string }
	candidates := []evidenceCandidate{}
	for rows.Next() {
		var raw []byte
		var conversationID, userID string
		if err := rows.Scan(&raw, &conversationID, &userID); err != nil {
			return nil, err
		}
		if !json.Valid(raw) {
			return nil, fmt.Errorf("Turn %q has an invalid tool completion event", turnID)
		}
		var details struct {
			SourceRef string `json:"sourceRef"`
		}
		if err := json.Unmarshal(raw, &details); err != nil {
			return nil, err
		}
		if details.SourceRef != "" {
			candidates = append(candidates, evidenceCandidate{ref: details.SourceRef, conversationID: conversationID, userID: userID})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	refs := []string{}
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if _, ok := seen[candidate.ref]; ok {
			continue
		}
		var sourceExists int
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM context_sources WHERE source_id=? AND conversation_id=? AND user_id=?)`, candidate.ref, candidate.conversationID, candidate.userID).Scan(&sourceExists); err != nil {
			return nil, err
		}
		if sourceExists == 1 {
			seen[candidate.ref] = struct{}{}
			refs = append(refs, candidate.ref)
		}
	}
	return refs, nil
}

func (s *Store) ContinuationChainMetrics(ctx context.Context, userID, chainID string) (domain.RunMetrics, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.kind,e.details_json FROM agent_trace_events e JOIN agent_turns t ON t.id=e.turn_id
WHERE t.user_id=? AND (t.id=? OR t.continuation_chain_id=?) AND e.kind IN ('turn.completed','turn.incomplete','turn.failed','turn.cancelled','continuation.intent_classified')`, userID, chainID, chainID)
	if err != nil {
		return domain.RunMetrics{}, err
	}
	defer rows.Close()
	var total domain.RunMetrics
	for rows.Next() {
		var kind string
		var raw []byte
		if err := rows.Scan(&kind, &raw); err != nil {
			return total, err
		}
		var details struct {
			Metrics domain.RunMetrics `json:"metrics"`
			Usage   struct {
				PromptTokens     int `json:"promptTokens"`
				CompletionTokens int `json:"completionTokens"`
				TotalTokens      int `json:"totalTokens"`
			} `json:"usage"`
			DurationMillis int64 `json:"durationMillis"`
		}
		if !json.Valid(raw) {
			return total, fmt.Errorf("continuation chain %q has invalid terminal metrics", chainID)
		}
		if err := json.Unmarshal(raw, &details); err != nil {
			return total, err
		}
		if kind == "continuation.intent_classified" {
			total.ModelCalls++
			total.PromptTokens += details.Usage.PromptTokens
			total.CompletionTokens += details.Usage.CompletionTokens
			total.TotalTokens += details.Usage.TotalTokens
			total.DurationMillis += details.DurationMillis
			continue
		}
		total.ModelCalls += details.Metrics.ModelCalls
		total.ToolCalls += details.Metrics.ToolCalls
		total.PromptTokens += details.Metrics.PromptTokens
		total.CompletionTokens += details.Metrics.CompletionTokens
		total.TotalTokens += details.Metrics.TotalTokens
		total.DurationMillis += details.Metrics.DurationMillis
	}
	return total, rows.Err()
}

func (s *Store) RetryAgentTurnSeed(ctx context.Context, userID, turnID string) (domain.AgentTurn, domain.Message, error) {
	var turn domain.AgentTurn
	var message domain.Message
	err := s.db.QueryRowContext(ctx, `SELECT t.id,t.conversation_id,t.user_id,t.input_message_id,t.provider_id,t.generation_id,t.definition_digest,t.status,t.recovery_class,
m.id,m.conversation_id,m.role,m.content,m.created_at
FROM agent_turns t JOIN messages m ON m.id=t.input_message_id WHERE t.id=? AND t.user_id=?`, turnID, userID).
		Scan(&turn.ID, &turn.ConversationID, &turn.UserID, &turn.InputMessageID, &turn.ProviderID, &turn.AgentGenerationID, &turn.AgentDefinitionDigest, &turn.Status, &turn.RecoveryClass,
			&message.ID, &message.ConversationID, &message.Role, &message.Content, &message.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return turn, message, domain.ErrNotFound
	}
	if err != nil {
		return turn, message, err
	}
	if message.Role != "user" || (turn.Status != "failed" && turn.Status != "cancelled" && turn.Status != "interrupted" && turn.Status != "incomplete") || turn.RecoveryClass == "unknown_external_effect" || turn.RecoveryClass == "external_effect_confirmed" {
		return turn, message, domain.ErrConflict
	}
	return turn, message, nil
}

func (s *Store) AgentTurnSeedForBranch(ctx context.Context, userID, turnID string) (domain.AgentTurn, domain.Message, error) {
	var turn domain.AgentTurn
	var message domain.Message
	err := s.db.QueryRowContext(ctx, `SELECT t.id,t.conversation_id,t.user_id,t.input_message_id,t.provider_id,t.generation_id,t.definition_digest,t.status,t.recovery_class,
m.id,m.conversation_id,m.role,m.content,m.created_at
FROM agent_turns t JOIN messages m ON m.id=t.input_message_id WHERE t.id=? AND t.user_id=?`, turnID, userID).
		Scan(&turn.ID, &turn.ConversationID, &turn.UserID, &turn.InputMessageID, &turn.ProviderID, &turn.AgentGenerationID, &turn.AgentDefinitionDigest, &turn.Status, &turn.RecoveryClass,
			&message.ID, &message.ConversationID, &message.Role, &message.Content, &message.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return turn, message, domain.ErrNotFound
	}
	if err != nil {
		return turn, message, err
	}
	if message.Role != "user" || (turn.Status != "completed" && turn.Status != "failed" && turn.Status != "cancelled" && turn.Status != "interrupted" && turn.Status != "incomplete") || turn.RecoveryClass == "unknown_external_effect" || turn.RecoveryClass == "external_effect_confirmed" {
		return turn, message, domain.ErrConflict
	}
	return turn, message, nil
}

func (s *Store) QueueAgentInput(ctx context.Context, userID string, input domain.InboxInput) error {
	const maxQueuedInputsPerConversation = 32
	const maxQueuedInputsPerUser = 256
	if strings.TrimSpace(input.Content) == "" {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userQueuedCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE user_id=? AND status='queued'`, userID).Scan(&userQueuedCount); err != nil {
		return err
	}
	if userQueuedCount >= maxQueuedInputsPerUser {
		return fmt.Errorf("%w: a workspace may hold at most %d queued inputs", domain.ErrBusy, maxQueuedInputsPerUser)
	}
	var queuedCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE user_id=? AND conversation_id=? AND status='queued'`, userID, input.ConversationID).Scan(&queuedCount); err != nil {
		return err
	}
	if queuedCount >= maxQueuedInputsPerConversation {
		return fmt.Errorf("%w: a conversation may hold at most %d queued inputs", domain.ErrBusy, maxQueuedInputsPerConversation)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO agent_inbox(id,conversation_id,user_id,content,status,created_at,updated_at)
SELECT ?,?,?,?,'queued',?,? FROM conversations WHERE id=? AND user_id=? AND execution_paused=0`, input.ID, input.ConversationID, userID, input.Content, input.CreatedAt, input.CreatedAt, input.ConversationID, userID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT execution_paused FROM conversations WHERE id=? AND user_id=?`, input.ConversationID, userID).Scan(&paused); errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		} else if err != nil {
			return err
		} else if paused {
			return fmt.Errorf("%w: conversation execution is paused", domain.ErrConflict)
		}
		return domain.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, input.CreatedAt, input.ConversationID, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) QueuedAgentInputs(ctx context.Context, userID, conversationID string) ([]domain.InboxInput, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,conversation_id,content,status,turn_id,created_at FROM agent_inbox
WHERE user_id=? AND conversation_id=? AND status='queued' ORDER BY created_at,rowid`, userID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.InboxInput{}
	for rows.Next() {
		var item domain.InboxInput
		if err := rows.Scan(&item.ID, &item.ConversationID, &item.Content, &item.Status, &item.TurnID, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CountQueuedAgentInputs(ctx context.Context, userID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_inbox WHERE user_id=? AND status='queued'`, userID).Scan(&count)
	return count, err
}

func (s *Store) QueuedAgentConversationIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT conversation_id FROM agent_inbox
WHERE user_id=? AND status='queued'
GROUP BY conversation_id
ORDER BY MIN(created_at),MIN(rowid)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var conversationID string
		if err := rows.Scan(&conversationID); err != nil {
			return nil, err
		}
		result = append(result, conversationID)
	}
	return result, rows.Err()
}

func (s *Store) CountOutstandingEvalExperiments(ctx context.Context, userID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM eval_experiments WHERE user_id=? AND status IN ('queued','running')`, userID).Scan(&count)
	return count, err
}

func (s *Store) AgentInboxItem(ctx context.Context, userID, inboxID string) (domain.InboxInput, error) {
	var item domain.InboxInput
	err := s.db.QueryRowContext(ctx, `SELECT id,conversation_id,content,status,turn_id,created_at FROM agent_inbox WHERE id=? AND user_id=?`, inboxID, userID).
		Scan(&item.ID, &item.ConversationID, &item.Content, &item.Status, &item.TurnID, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return item, domain.ErrNotFound
	}
	return item, err
}

func (s *Store) HasActiveAgentTurn(ctx context.Context, userID, conversationID string) (bool, error) {
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_turns WHERE user_id=? AND conversation_id=? AND status IN ('running','cancelling','awaiting_approval'))`, userID, conversationID).Scan(&active)
	return active, err
}

func (s *Store) ConversationExecutionPaused(ctx context.Context, userID, conversationID string) (bool, error) {
	var paused bool
	err := s.db.QueryRowContext(ctx, `SELECT execution_paused FROM conversations WHERE user_id=? AND id=?`, userID, conversationID).Scan(&paused)
	if errors.Is(err, sql.ErrNoRows) {
		return false, domain.ErrNotFound
	}
	return paused, err
}

func (s *Store) TurnEvents(ctx context.Context, userID, turnID string, after int) ([]domain.TraceEvent, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.conversation_id,e.turn_id,e.sequence,e.kind,e.details_json,e.created_at
FROM agent_trace_events e JOIN agent_turns t ON t.id=e.turn_id
WHERE e.turn_id=? AND t.user_id=? AND e.sequence>? ORDER BY e.sequence`, turnID, userID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.TraceEvent{}
	for rows.Next() {
		var event domain.TraceEvent
		var details []byte
		if err := rows.Scan(&event.ID, &event.ConversationID, &event.TurnID, &event.Sequence, &event.Kind, &details, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Details = details
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Store) RecoverInterruptedAgentTurns(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT t.id,t.conversation_id,t.last_sequence,t.inbox_id,
COALESCE((SELECT checkpoint_version FROM agent_turn_checkpoints c WHERE c.turn_id=t.id),0),
COALESCE((SELECT resume_allowed FROM agent_turn_checkpoints c WHERE c.turn_id=t.id),0),
COALESCE((SELECT length(state_cipher)>0 AND length(state_nonce)>0 FROM agent_turn_checkpoints c WHERE c.turn_id=t.id),0),
COALESCE((SELECT event_sequence FROM agent_turn_checkpoints c WHERE c.turn_id=t.id),0)
FROM agent_turns t WHERE t.status IN ('running','cancelling','awaiting_approval') ORDER BY t.started_at`)
	if err != nil {
		return 0, err
	}
	type recovery struct {
		id, conversationID string
		inboxID            string
		sequence           int
		checkpointVersion  int
		checkpointResume   bool
		checkpointStored   bool
		checkpointSequence int
	}
	items := []recovery{}
	for rows.Next() {
		var item recovery
		if err := rows.Scan(&item.id, &item.conversationID, &item.sequence, &item.inboxID, &item.checkpointVersion, &item.checkpointResume, &item.checkpointStored, &item.checkpointSequence); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	for _, item := range items {
		status, recoveryClass, kind := "interrupted", "safe_to_retry", "turn.interrupted"
		checkpointValid := item.checkpointVersion == 1 && item.checkpointResume && item.checkpointStored
		_, unresolvedTool, malformedToolJournal, unsafeToolEffect, _, lastToolCompleted, err := inspectToolJournal(ctx, tx, item.id)
		if err != nil {
			return 0, err
		}
		if malformedToolJournal || unsafeToolEffect {
			status, recoveryClass, kind = "needs_reconciliation", "unknown_external_effect", "turn.needs_reconciliation"
		} else if checkpointValid && !malformedToolJournal && lastToolCompleted <= item.checkpointSequence {
			recoveryClass = "checkpoint_resumable"
		} else if unresolvedTool {
			// An incomplete read-only call is safe to retry; unknown or mutating
			// effects were classified above and require explicit reconciliation.
			recoveryClass = "safe_to_retry"
		}
		details, marshalErr := json.Marshal(map[string]string{"stopReason": "host_restarted", "recoveryClass": recoveryClass})
		if marshalErr != nil {
			return 0, marshalErr
		}
		sequence := item.sequence + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(item.id, sequence), item.conversationID, item.id, sequence, kind, details, now); err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE agent_turns SET status=?,recovery_class=?,stop_reason='host_restarted',last_sequence=?,updated_at=?,completed_at=? WHERE id=? AND status IN ('running','cancelling','awaiting_approval')`, status, recoveryClass, sequence, now, now, item.id)
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if count != 1 {
			return 0, fmt.Errorf("agent turn %s changed during recovery", item.id)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_approvals SET status='cancelled',decision='cancelled',resolved_at=? WHERE turn_id=? AND status='pending'`, now, item.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_model_attempts SET status='interrupted',completed_at=? WHERE turn_id=? AND status='running'`, now, item.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status='interrupted',completed_at=? WHERE turn_id=? AND status IN ('running','awaiting_tools')`, now, item.id); err != nil {
			return 0, err
		}
		if item.inboxID != "" {
			result, err := tx.ExecContext(ctx, `UPDATE agent_inbox SET status=?,updated_at=? WHERE id=? AND turn_id=? AND status='claimed'`, status, now, item.inboxID, item.id)
			if err != nil {
				return 0, err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			if count != 1 {
				return 0, fmt.Errorf("claimed inbox %s does not match recovered turn %s", item.inboxID, item.id)
			}
		}
		updated, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=?`, now, item.conversationID)
		if err != nil {
			return 0, err
		}
		updatedCount, err := updated.RowsAffected()
		if err != nil {
			return 0, err
		}
		if updatedCount != 1 {
			return 0, fmt.Errorf("conversation %s disappeared during turn recovery", item.conversationID)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(items)), nil
}

// inspectToolJournal returns tool presence, unresolved calls, journal integrity,
// whether any started call may have changed state, and the latest completion.
// Checkpoint recovery uses the journal integrity and completion sequence; a
// pending call in the checkpoint is replayed with its original call ID.
func inspectToolJournal(ctx context.Context, tx *sql.Tx, turnID string) (bool, bool, bool, bool, bool, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT sequence,kind,details_json FROM agent_trace_events WHERE turn_id=? AND kind IN ('permission.checked','tool.started','tool.completed') ORDER BY sequence`, turnID)
	if err != nil {
		return false, false, false, false, false, 0, err
	}
	defer rows.Close()
	started := map[string]bool{}
	completed := map[string]bool{}
	effects := map[string]string{}
	hasTool, malformed, unsafeEffect, latestCompleted := false, false, false, 0
	for rows.Next() {
		var sequence int
		var kind string
		var raw []byte
		if err := rows.Scan(&sequence, &kind, &raw); err != nil {
			return false, false, false, false, false, 0, err
		}
		var details struct {
			ToolCallID string `json:"toolCallId"`
			Effect     string `json:"effect"`
		}
		if !json.Valid(raw) || json.Unmarshal(raw, &details) != nil || details.ToolCallID == "" {
			malformed = true
			continue
		}
		if kind == "permission.checked" {
			if details.Effect == "" {
				continue
			}
			if previous, exists := effects[details.ToolCallID]; exists && previous != details.Effect {
				malformed = true
			}
			effects[details.ToolCallID] = details.Effect
			continue
		}
		hasTool = true
		if kind == "tool.started" {
			started[details.ToolCallID] = true
			if details.Effect != "" {
				if previous, exists := effects[details.ToolCallID]; exists && previous != details.Effect {
					malformed = true
				}
				effects[details.ToolCallID] = details.Effect
			}
		} else {
			if !started[details.ToolCallID] {
				malformed = true
			}
			completed[details.ToolCallID] = true
			if sequence > latestCompleted {
				latestCompleted = sequence
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, false, false, false, 0, err
	}
	unresolved, completedUnsafeEffect := false, false
	for callID := range started {
		if !completed[callID] {
			unresolved = true
		}
		switch effects[callID] {
		case "read", "external_read":
		case "":
			malformed = true
			unsafeEffect = true
		default:
			unsafeEffect = true
			if completed[callID] {
				completedUnsafeEffect = true
			}
		}
	}
	return hasTool, unresolved, malformed, unsafeEffect, completedUnsafeEffect, latestCompleted, nil
}

func (s *Store) reclassifyLegacyReadOnlyTurns(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM agent_turns WHERE recovery_class='unknown_external_effect' ORDER BY started_at`)
	if err != nil {
		return err
	}
	type candidate struct {
		id string
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range candidates {
		hasTool, _, malformed, unsafeEffect, _, _, err := inspectToolJournal(ctx, tx, item.id)
		if err != nil {
			return err
		}
		if !hasTool || malformed || unsafeEffect {
			continue
		}
		result, err := tx.ExecContext(ctx, `UPDATE agent_turns SET recovery_class='safe_to_retry',status=CASE WHEN status='needs_reconciliation' THEN 'interrupted' ELSE status END WHERE id=? AND recovery_class='unknown_external_effect'`, item.id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("legacy recovery classification for turn %s changed during migration", item.id)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func eventID(turnID string, sequence int) string {
	return fmt.Sprintf("evt_%s_%06d", turnID, sequence)
}
