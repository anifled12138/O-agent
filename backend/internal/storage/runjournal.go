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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at)
SELECT ?,?,?,?,? FROM conversations WHERE id=? AND user_id=?`, input.ID, input.ConversationID, input.Role, input.Content, input.CreatedAt, input.ConversationID, userID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return domain.ErrNotFound
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_turns(id,conversation_id,user_id,input_message_id,provider_id,generation_id,definition_digest,status,last_sequence,started_at,updated_at)
VALUES(?,?,?,?,?,?,?,'running',1,?,?)`, turn.ID, turn.ConversationID, userID, input.ID, turn.ProviderID, turn.AgentGenerationID, turn.AgentDefinitionDigest, turn.StartedAt, turn.UpdatedAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.ErrConflict
		}
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turn.ID, 1), turn.ConversationID, turn.ID, 1, "turn.started", []byte(details), turn.StartedAt); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=?`, input.CreatedAt, input.ConversationID); err != nil {
		return err
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
	if status != "completed" && status != "failed" && status != "cancelled" && status != "needs_reconciliation" {
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
	if current != "running" && current != "cancelling" {
		return fmt.Errorf("turn %s is already %s", turnID, current)
	}
	now := time.Now().UTC()
	resultMessageID := ""
	if output != nil {
		resultMessageID = output.ID
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) VALUES(?,?,?,?,?)`, output.ID, conversationID, output.Role, output.Content, output.CreatedAt); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=?`, output.CreatedAt, conversationID); err != nil {
			return err
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
	_, err = tx.ExecContext(ctx, `UPDATE agent_turns SET status=?,stop_reason=?,result_message_id=NULLIF(?,''),last_sequence=?,updated_at=?,completed_at=? WHERE id=?`, status, stopReason, resultMessageID, sequence, now, now, turnID)
	if err != nil {
		return err
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
	if status != "running" && status != "cancelling" {
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
	if _, err = tx.ExecContext(ctx, `UPDATE agent_turns SET status='cancelling',cancel_requested=1,last_sequence=?,updated_at=? WHERE id=?`, sequence, now, turnID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AgentTurns(ctx context.Context, userID, conversationID string) ([]domain.AgentTurn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,conversation_id,user_id,input_message_id,COALESCE(result_message_id,''),provider_id,generation_id,definition_digest,status,stop_reason,recovery_class,cancel_requested,last_sequence,started_at,updated_at,completed_at
FROM agent_turns WHERE user_id=? AND conversation_id=? ORDER BY started_at DESC`, userID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.AgentTurn{}
	for rows.Next() {
		var turn domain.AgentTurn
		var completed sql.NullTime
		if err := rows.Scan(&turn.ID, &turn.ConversationID, &turn.UserID, &turn.InputMessageID, &turn.ResultMessageID, &turn.ProviderID, &turn.AgentGenerationID, &turn.AgentDefinitionDigest, &turn.Status, &turn.StopReason, &turn.RecoveryClass, &turn.CancelRequested, &turn.LastSequence, &turn.StartedAt, &turn.UpdatedAt, &completed); err != nil {
			return nil, err
		}
		if completed.Valid {
			turn.CompletedAt = &completed.Time
		}
		result = append(result, turn)
	}
	return result, rows.Err()
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
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE agent_turns AS t SET
 status=CASE WHEN EXISTS (
   SELECT 1 FROM agent_trace_events started
   WHERE started.turn_id=t.id AND started.kind='tool.started'
     AND NOT EXISTS (
       SELECT 1 FROM agent_trace_events completed
       WHERE completed.turn_id=t.id AND completed.kind='tool.completed'
         AND json_extract(completed.details_json,'$.toolCallId')=json_extract(started.details_json,'$.toolCallId')
     )
 ) THEN 'needs_reconciliation' ELSE 'interrupted' END,
 recovery_class=CASE WHEN EXISTS (
   SELECT 1 FROM agent_trace_events started
   WHERE started.turn_id=t.id AND started.kind='tool.started'
     AND NOT EXISTS (
       SELECT 1 FROM agent_trace_events completed
       WHERE completed.turn_id=t.id AND completed.kind='tool.completed'
         AND json_extract(completed.details_json,'$.toolCallId')=json_extract(started.details_json,'$.toolCallId')
     )
 ) THEN 'unknown_external_effect' ELSE 'safe_to_retry' END,
 stop_reason='host_restarted',updated_at=?,completed_at=?
 WHERE status IN ('running','cancelling')`, now, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func eventID(turnID string, sequence int) string {
	return fmt.Sprintf("evt_%s_%06d", turnID, sequence)
}
