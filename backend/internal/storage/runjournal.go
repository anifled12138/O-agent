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
	return tx.Commit()
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

func (s *Store) RecordAgentTurnReconciliation(ctx context.Context, userID, turnID, note string, at time.Time) (domain.AgentTurnReconciliation, error) {
	var result domain.AgentTurnReconciliation
	result.ID = "reconcile_" + turnID
	result.TurnID = turnID
	result.Decision = "retry_authorized"
	result.Note = strings.TrimSpace(note)
	result.CreatedAt = at
	if result.Note == "" || len([]rune(result.Note)) > 2000 {
		return domain.AgentTurnReconciliation{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	defer tx.Rollback()
	var status, recoveryClass, inputMessageID string
	var sequence int
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id,status,recovery_class,input_message_id,last_sequence FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&result.ConversationID, &status, &recoveryClass, &inputMessageID, &sequence); errors.Is(err, sql.ErrNoRows) {
		return domain.AgentTurnReconciliation{}, domain.ErrNotFound
	} else if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if status != "needs_reconciliation" && status != "completed" && status != "failed" && status != "cancelled" && status != "interrupted" && status != "incomplete" {
		return domain.AgentTurnReconciliation{}, domain.ErrConflict
	}
	if recoveryClass != "unknown_external_effect" {
		return domain.AgentTurnReconciliation{}, fmt.Errorf("%w: turn has no tool effect to reconcile", domain.ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_turn_reconciliations(id,turn_id,user_id,conversation_id,decision,note,created_at) VALUES(?,?,?,?,?,?,?)`, result.ID, turnID, userID, result.ConversationID, result.Decision, result.Note, at); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.AgentTurnReconciliation{}, domain.ErrConflict
		}
		return domain.AgentTurnReconciliation{}, err
	}
	sequence++
	details, err := json.Marshal(map[string]string{"reconciliationId": result.ID, "decision": result.Decision})
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), result.ConversationID, turnID, sequence, "turn.reconciled", details, at); err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	updatedTurn, err := tx.ExecContext(ctx, `UPDATE agent_turns SET recovery_class='manually_reconciled',last_sequence=?,updated_at=? WHERE id=? AND user_id=?`, sequence, at, turnID, userID)
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
	updated, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=? AND user_id=?`, at, result.ConversationID, userID)
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
	err = s.db.QueryRowContext(ctx, `SELECT id,turn_id,conversation_id,decision,note,created_at FROM agent_turn_reconciliations WHERE id=? AND user_id=?`, result.ID, userID).
		Scan(&persisted.ID, &persisted.TurnID, &persisted.ConversationID, &persisted.Decision, &persisted.Note, &persisted.CreatedAt)
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	return persisted, nil
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
	if inboxID != "" {
		if wasPaused {
			return fmt.Errorf("%w: conversation execution is paused", domain.ErrConflict)
		}
	}
	var revisionPrior *string
	if existingInput {
		var currentStatus string
		var conversationID string
		var inputMessageID string
		if err := tx.QueryRowContext(ctx, `SELECT conversation_id,status,input_message_id FROM agent_turns WHERE id=? AND user_id=?`, retryOf, userID).Scan(&conversationID, &currentStatus, &inputMessageID); errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		} else if err != nil {
			return err
		}
		if conversationID != input.ConversationID || inputMessageID != input.ID || (currentStatus != "cancelled" && currentStatus != "failed" && currentStatus != "interrupted" && currentStatus != "incomplete" && currentStatus != "needs_reconciliation") {
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
	return tx.Commit()
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
	var conversationID, status, recoveryClass, inboxID string
	var sequence int
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id,status,recovery_class,last_sequence,inbox_id FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&conversationID, &status, &recoveryClass, &sequence, &inboxID); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if status != "interrupted" || recoveryClass != "checkpoint_resumable" {
		return fmt.Errorf("turn %s is not checkpoint-resumable: %w", turnID, domain.ErrConflict)
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
	details, err := json.Marshal(map[string]string{"reason": "host_restart", "recoveryClass": "checkpoint_resumable"})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, "turn.resumed", details, at); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_turns SET status='running',recovery_class='',stop_reason='',completed_at=NULL,last_sequence=?,updated_at=? WHERE id=? AND user_id=? AND status='interrupted' AND recovery_class='checkpoint_resumable'`, sequence, at, turnID, userID)
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
	if status != "completed" && status != "incomplete" && status != "failed" && status != "cancelled" {
		return domain.ErrInvalid
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
	resultMessageID := ""
	if output != nil {
		resultMessageID = output.ID
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) VALUES(?,?,?,?,?)`, output.ID, conversationID, output.Role, output.Content, output.CreatedAt); err != nil {
			return err
		}
	}
	terminalRecoveryClass := ""
	if status == "completed" {
		terminalRecoveryClass = "not_replayable"
	} else if status == "failed" || status == "cancelled" || status == "incomplete" {
		_, _, malformedToolJournal, unsafeToolEffect, _, inspectErr := inspectToolJournal(ctx, tx, turnID)
		if inspectErr != nil {
			return inspectErr
		}
		if malformedToolJournal || unsafeToolEffect {
			terminalRecoveryClass = "unknown_external_effect"
		} else {
			terminalRecoveryClass = "safe_to_retry"
		}
	}
	sequence++
	kind := "turn." + status
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turnID, sequence), conversationID, turnID, sequence, kind, []byte(details), now); err != nil {
		return err
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
recovery_class=CASE WHEN ?<>'' THEN ? ELSE recovery_class END,
	last_sequence=?,updated_at=?,completed_at=? WHERE id=? AND user_id=? AND status IN ('running','cancelling','awaiting_approval')`, status, stopReason, resultMessageID, terminalRecoveryClass, terminalRecoveryClass, sequence, now, now, turnID, userID)
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
	return tx.Commit()
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
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,t.conversation_id,t.user_id,t.input_message_id,COALESCE(t.result_message_id,''),t.provider_id,t.generation_id,t.definition_digest,t.permission_profile,t.status,t.stop_reason,t.recovery_class,t.cancel_requested,t.last_sequence,t.started_at,t.updated_at,t.completed_at,COALESCE(t.retry_of_turn_id,''),COALESCE(r.note,'')
FROM agent_turns t LEFT JOIN agent_turn_reconciliations r ON r.turn_id=t.id AND r.user_id=t.user_id WHERE t.user_id=? AND t.conversation_id=? ORDER BY t.started_at DESC`, userID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.AgentTurn{}
	for rows.Next() {
		var turn domain.AgentTurn
		var completed sql.NullTime
		if err := rows.Scan(&turn.ID, &turn.ConversationID, &turn.UserID, &turn.InputMessageID, &turn.ResultMessageID, &turn.ProviderID, &turn.AgentGenerationID, &turn.AgentDefinitionDigest, &turn.PermissionProfile, &turn.Status, &turn.StopReason, &turn.RecoveryClass, &turn.CancelRequested, &turn.LastSequence, &turn.StartedAt, &turn.UpdatedAt, &completed, &turn.RetryOfTurnID, &turn.ReconciliationNote); err != nil {
			return nil, err
		}
		if completed.Valid {
			turn.CompletedAt = &completed.Time
		}
		result = append(result, turn)
	}
	return result, rows.Err()
}

func (s *Store) AgentTurn(ctx context.Context, userID, turnID string) (domain.AgentTurn, error) {
	var turn domain.AgentTurn
	var completed sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT t.id,t.conversation_id,t.user_id,t.input_message_id,COALESCE(t.result_message_id,''),t.provider_id,t.generation_id,t.definition_digest,t.permission_profile,t.status,t.stop_reason,t.recovery_class,t.cancel_requested,t.last_sequence,t.started_at,t.updated_at,t.completed_at,COALESCE(t.retry_of_turn_id,''),COALESCE(r.note,'')
FROM agent_turns t LEFT JOIN agent_turn_reconciliations r ON r.turn_id=t.id AND r.user_id=t.user_id WHERE t.id=? AND t.user_id=?`, turnID, userID).
		Scan(&turn.ID, &turn.ConversationID, &turn.UserID, &turn.InputMessageID, &turn.ResultMessageID, &turn.ProviderID, &turn.AgentGenerationID, &turn.AgentDefinitionDigest, &turn.PermissionProfile, &turn.Status, &turn.StopReason, &turn.RecoveryClass, &turn.CancelRequested, &turn.LastSequence, &turn.StartedAt, &turn.UpdatedAt, &completed, &turn.RetryOfTurnID, &turn.ReconciliationNote)
	if errors.Is(err, sql.ErrNoRows) {
		return turn, domain.ErrNotFound
	}
	if err != nil {
		return turn, err
	}
	if completed.Valid {
		turn.CompletedAt = &completed.Time
	}
	return turn, nil
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
	if message.Role != "user" || (turn.Status != "failed" && turn.Status != "cancelled" && turn.Status != "interrupted" && turn.Status != "incomplete" && turn.Status != "needs_reconciliation") {
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
	if message.Role != "user" || (turn.Status != "completed" && turn.Status != "failed" && turn.Status != "cancelled" && turn.Status != "interrupted" && turn.Status != "incomplete" && turn.Status != "needs_reconciliation") {
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
		_, unresolvedTool, malformedToolJournal, unsafeToolEffect, lastToolCompleted, err := inspectToolJournal(ctx, tx, item.id)
		if err != nil {
			return 0, err
		}
		if checkpointValid && !unresolvedTool && !malformedToolJournal && lastToolCompleted <= item.checkpointSequence {
			recoveryClass = "checkpoint_resumable"
		} else if malformedToolJournal || unsafeToolEffect {
			recoveryClass = "unknown_external_effect"
		}
		details, _ := json.Marshal(map[string]string{"stopReason": "host_restarted", "recoveryClass": recoveryClass})
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
// Read and external-read tools can be retried; unknown and mutating effects fail closed.
func inspectToolJournal(ctx context.Context, tx *sql.Tx, turnID string) (bool, bool, bool, bool, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT sequence,kind,details_json FROM agent_trace_events WHERE turn_id=? AND kind IN ('permission.checked','tool.started','tool.completed') ORDER BY sequence`, turnID)
	if err != nil {
		return false, false, false, false, 0, err
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
			return false, false, false, false, 0, err
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
			if started[details.ToolCallID] {
				malformed = true
			}
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
		return false, false, false, false, 0, err
	}
	unresolved := false
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
		}
	}
	return hasTool, unresolved, malformed, unsafeEffect, latestCompleted, nil
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
		hasTool, _, malformed, unsafeEffect, _, err := inspectToolJournal(ctx, tx, item.id)
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
