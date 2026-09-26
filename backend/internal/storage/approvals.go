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

func (s *Store) CreateApprovalAndPause(ctx context.Context, userID string, request domain.ApprovalRequest, argsHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var sequence int
	if err = tx.QueryRowContext(ctx, `SELECT status,last_sequence FROM agent_turns WHERE id=? AND user_id=? AND conversation_id=?`, request.TurnID, userID, request.ConversationID).Scan(&status, &sequence); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if status != "running" {
		return fmt.Errorf("%w: turn is %s", domain.ErrConflict, status)
	}
	now := request.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if request.ExpiresAt.IsZero() {
		request.ExpiresAt = now.Add(15 * time.Minute)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_approvals(id,conversation_id,turn_id,user_id,tool_call_id,tool_name,source,effect,permission_profile,plugin_id,release_id,resource,impact,reason,arguments_preview,arguments_sha256,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'pending',?,?)`, request.ID, request.ConversationID, request.TurnID, userID, request.ToolCallID, request.ToolName, request.Source, request.Effect, request.PermissionProfile, request.PluginID, request.ReleaseID, request.Resource, request.Impact, request.Reason, request.Arguments, argsHash, now, request.ExpiresAt); err != nil {
		return err
	}
	sequence++
	details, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(request.TurnID, sequence), request.ConversationID, request.TurnID, sequence, "approval.requested", details, now); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_turns SET status='awaiting_approval',last_sequence=?,updated_at=? WHERE id=? AND status='running'`, sequence, now, request.TurnID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var persistedApproval, persistedTurn string
	if err := s.db.QueryRowContext(ctx, `SELECT a.status,t.status FROM agent_approvals a JOIN agent_turns t ON t.id=a.turn_id WHERE a.id=? AND a.user_id=?`, request.ID, userID).Scan(&persistedApproval, &persistedTurn); err != nil {
		return err
	}
	if persistedApproval != "pending" || persistedTurn != "awaiting_approval" {
		return fmt.Errorf("approval pause read-back mismatch: approval=%s turn=%s", persistedApproval, persistedTurn)
	}
	return nil
}

// ResolvedToolApproval looks up a persisted decision for the exact call ID and
// argument digest. A restart may occur after approval is committed but before
// the approved call reaches tool.started; the resumed loop must consume that
// decision instead of prompting twice.
func (s *Store) ResolvedToolApproval(ctx context.Context, userID, turnID, callID, argsHash string) (status, decision string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT status,decision FROM agent_approvals WHERE user_id=? AND turn_id=? AND tool_call_id=? AND arguments_sha256=? ORDER BY created_at DESC LIMIT 1`, userID, turnID, callID, argsHash).Scan(&status, &decision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return status, decision, true, nil
}

func (s *Store) ResolveApproval(ctx context.Context, userID, approvalID, choice string) (domain.ApprovalRequest, error) {
	if choice != "approve" && choice != "deny" {
		return domain.ApprovalRequest{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ApprovalRequest{}, err
	}
	defer tx.Rollback()
	var a domain.ApprovalRequest
	var status string
	err = tx.QueryRowContext(ctx, `SELECT id,conversation_id,turn_id,tool_call_id,tool_name,source,effect,permission_profile,plugin_id,release_id,resource,impact,reason,arguments_preview,status,created_at,expires_at FROM agent_approvals WHERE id=? AND user_id=?`, approvalID, userID).Scan(&a.ID, &a.ConversationID, &a.TurnID, &a.ToolCallID, &a.ToolName, &a.Source, &a.Effect, &a.PermissionProfile, &a.PluginID, &a.ReleaseID, &a.Resource, &a.Impact, &a.Reason, &a.Arguments, &status, &a.CreatedAt, &a.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, domain.ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if status != "pending" {
		return a, fmt.Errorf("%w: approval is %s", domain.ErrConflict, status)
	}
	now := time.Now().UTC()
	decision := choice
	newStatus := "approved"
	if choice == "deny" {
		newStatus = "denied"
	}
	if !now.Before(a.ExpiresAt) {
		newStatus, decision = "expired", "deny"
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_approvals SET status=?,decision=?,resolved_at=? WHERE id=? AND user_id=? AND status='pending'`, newStatus, decision, now, approvalID, userID)
	if err != nil {
		return a, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return a, err
	} else if n != 1 {
		return a, domain.ErrConflict
	}
	var current string
	var sequence int
	if err := tx.QueryRowContext(ctx, `SELECT status,last_sequence FROM agent_turns WHERE id=? AND user_id=?`, a.TurnID, userID).Scan(&current, &sequence); err != nil {
		return a, err
	}
	if current != "awaiting_approval" {
		return a, fmt.Errorf("%w: turn is %s", domain.ErrConflict, current)
	}
	sequence++
	a.Status = newStatus
	details, err := json.Marshal(map[string]any{"approvalId": a.ID, "decision": decision, "status": newStatus})
	if err != nil {
		return a, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(a.TurnID, sequence), a.ConversationID, a.TurnID, sequence, "approval.resolved", details, now); err != nil {
		return a, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE agent_turns SET status='running',last_sequence=?,updated_at=? WHERE id=? AND status='awaiting_approval'`, sequence, now, a.TurnID)
	if err != nil {
		return a, err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return a, err
	} else if changed != 1 {
		return a, domain.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return a, err
	}
	var persistedStatus, persistedTurn string
	if err := s.db.QueryRowContext(ctx, `SELECT a.status,t.status FROM agent_approvals a JOIN agent_turns t ON t.id=a.turn_id WHERE a.id=? AND a.user_id=?`, approvalID, userID).Scan(&persistedStatus, &persistedTurn); err != nil {
		return a, err
	}
	if persistedStatus != newStatus || persistedTurn != "running" {
		return a, fmt.Errorf("approval decision read-back mismatch: approval=%s turn=%s", persistedStatus, persistedTurn)
	}
	return a, nil
}

func (s *Store) PendingApprovals(ctx context.Context, userID, conversationID string) ([]domain.ApprovalRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,conversation_id,turn_id,tool_call_id,tool_name,source,effect,permission_profile,plugin_id,release_id,resource,impact,reason,arguments_preview,status,created_at,expires_at FROM agent_approvals WHERE user_id=? AND conversation_id=? AND status='pending' ORDER BY created_at`, userID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []domain.ApprovalRequest{}
	for rows.Next() {
		var a domain.ApprovalRequest
		if err := rows.Scan(&a.ID, &a.ConversationID, &a.TurnID, &a.ToolCallID, &a.ToolName, &a.Source, &a.Effect, &a.PermissionProfile, &a.PluginID, &a.ReleaseID, &a.Resource, &a.Impact, &a.Reason, &a.Arguments, &a.Status, &a.CreatedAt, &a.ExpiresAt); err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return items, rows.Err()
}
