package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

type ExecutionTaskWorkspace struct {
	UserID          string    `json:"-"`
	TaskID          string    `json:"taskId"`
	SourceProjectID string    `json:"sourceProjectId,omitempty"`
	Workdir         string    `json:"workdir"`
	Status          string    `json:"status"`
	QuotaProjectID  int64     `json:"quotaProjectId,omitempty"`
	QuotaLimitBytes int64     `json:"quotaLimitBytes,omitempty"`
	QuotaState      string    `json:"quotaState"`
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// EnsureExecutionTaskWorkspace reserves the durable task-to-directory mapping
// before filesystem mutations begin. Replays are idempotent only for the same
// task, source project, and canonical path.
func (s *Store) EnsureExecutionTaskWorkspace(ctx context.Context, workspace ExecutionTaskWorkspace, now time.Time) (ExecutionTaskWorkspace, bool, error) {
	workspace.UserID = strings.TrimSpace(workspace.UserID)
	workspace.TaskID = strings.TrimSpace(workspace.TaskID)
	workspace.SourceProjectID = strings.TrimSpace(workspace.SourceProjectID)
	if s == nil || s.db == nil || workspace.UserID == "" || workspace.TaskID == "" || len(workspace.TaskID) > 200 || strings.TrimSpace(workspace.Workdir) == "" || !filepath.IsAbs(workspace.Workdir) || workspace.QuotaProjectID < 0 || workspace.QuotaLimitBytes < 0 {
		return ExecutionTaskWorkspace{}, false, domain.ErrInvalid
	}
	workspace.Workdir = filepath.Clean(workspace.Workdir)
	if workspace.Status == "" {
		workspace.Status = "preparing"
	}
	if workspace.QuotaState == "" {
		workspace.QuotaState = "not_configured"
	}
	if workspace.Status != "preparing" || workspace.QuotaState != "not_configured" {
		return ExecutionTaskWorkspace{}, false, domain.ErrInvalid
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTaskWorkspace{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO execution_task_workspaces(user_id,task_id,source_project_id,workdir,status,quota_project_id,quota_limit_bytes,quota_state,error_text,created_at,updated_at) VALUES(?,?,?,?,'preparing',0,0,'not_configured','',?,?)`, workspace.UserID, workspace.TaskID, workspace.SourceProjectID, workspace.Workdir, now, now)
	if err != nil {
		return ExecutionTaskWorkspace{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ExecutionTaskWorkspace{}, false, err
	}
	persisted, err := executionTaskWorkspaceByID(ctx, tx, workspace.UserID, workspace.TaskID)
	if err != nil {
		return ExecutionTaskWorkspace{}, false, err
	}
	if persisted.SourceProjectID != workspace.SourceProjectID || persisted.Workdir != workspace.Workdir {
		return ExecutionTaskWorkspace{}, false, fmt.Errorf("execution task workspace identity changed: %w", domain.ErrConflict)
	}
	if persisted.Status == "released" || persisted.Status == "releasing" {
		return ExecutionTaskWorkspace{}, false, fmt.Errorf("execution task workspace is being or has been released: %w", domain.ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTaskWorkspace{}, false, err
	}
	readBack, err := s.ExecutionTaskWorkspace(ctx, workspace.UserID, workspace.TaskID)
	if err != nil {
		return ExecutionTaskWorkspace{}, false, err
	}
	if readBack.SourceProjectID != workspace.SourceProjectID || readBack.Workdir != workspace.Workdir {
		return ExecutionTaskWorkspace{}, false, domain.ErrConflict
	}
	return readBack, changed == 1, nil
}

// SetExecutionTaskWorkspaceState commits a state transition and reads it back.
// The current state is checked in the update predicate so startup reconciliation
// and task retries cannot silently overwrite each other's observations.
func (s *Store) SetExecutionTaskWorkspaceState(ctx context.Context, userID, taskID, expectedStatus, status, quotaState string, quotaProjectID, quotaLimitBytes int64, errorText string, now time.Time) (ExecutionTaskWorkspace, error) {
	if s == nil || s.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(taskID) == "" || !validExecutionWorkspaceStatus(expectedStatus) || !validExecutionWorkspaceStatus(status) || !validExecutionQuotaState(quotaState) || quotaProjectID < 0 || quotaLimitBytes < 0 || len(errorText) > 4096 {
		return ExecutionTaskWorkspace{}, domain.ErrInvalid
	}
	if !validExecutionWorkspaceTransition(expectedStatus, status) {
		return ExecutionTaskWorkspace{}, domain.ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionTaskWorkspace{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE execution_task_workspaces SET status=?,quota_state=?,quota_project_id=?,quota_limit_bytes=?,error_text=?,updated_at=? WHERE user_id=? AND task_id=? AND status=?`, status, quotaState, quotaProjectID, quotaLimitBytes, errorText, now.UTC(), userID, taskID, expectedStatus)
	if err != nil {
		return ExecutionTaskWorkspace{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ExecutionTaskWorkspace{}, err
	}
	if changed != 1 {
		return ExecutionTaskWorkspace{}, domain.ErrConflict
	}
	readBack, err := executionTaskWorkspaceByID(ctx, tx, userID, taskID)
	if err != nil {
		return ExecutionTaskWorkspace{}, err
	}
	if readBack.Status != status || readBack.QuotaState != quotaState || readBack.QuotaProjectID != quotaProjectID || readBack.QuotaLimitBytes != quotaLimitBytes || readBack.Error != errorText {
		return ExecutionTaskWorkspace{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionTaskWorkspace{}, err
	}
	committed, err := s.ExecutionTaskWorkspace(ctx, userID, taskID)
	if err != nil {
		return ExecutionTaskWorkspace{}, err
	}
	if committed.Status != status || committed.QuotaState != quotaState || committed.QuotaProjectID != quotaProjectID || committed.QuotaLimitBytes != quotaLimitBytes || committed.Error != errorText {
		return ExecutionTaskWorkspace{}, domain.ErrConflict
	}
	return committed, nil
}

func (s *Store) ExecutionTaskWorkspace(ctx context.Context, userID, taskID string) (ExecutionTaskWorkspace, error) {
	if s == nil || s.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(taskID) == "" {
		return ExecutionTaskWorkspace{}, domain.ErrInvalid
	}
	return executionTaskWorkspaceByID(ctx, s.db, userID, taskID)
}

// ProjectReferencesWorkdir checks the authoritative project registry before a
// scratch task directory can be reclaimed. A database read failure must stop
// cleanup rather than be interpreted as an unreferenced path.
func (s *Store) ProjectReferencesWorkdir(ctx context.Context, workdir string) (bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(workdir) == "" || !filepath.IsAbs(workdir) {
		return false, domain.ErrInvalid
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE workdir=?`, filepath.Clean(workdir)).Scan(&count); err != nil {
		return false, err
	}
	return count != 0, nil
}

// ReserveExecutionTaskWorkspaceQuota durably allocates a unique, nonzero Linux
// project ID and immutable per-workspace byte limit before the directory is
// created. A retry for the same task returns the original allocation.
func (s *Store) ReserveExecutionTaskWorkspaceQuota(ctx context.Context, userID, taskID string, limitBytes int64, now time.Time) (ExecutionTaskWorkspace, error) {
	if s == nil || s.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(taskID) == "" || limitBytes <= 0 {
		return ExecutionTaskWorkspace{}, domain.ErrInvalid
	}
	for attempt := 0; attempt < 32; attempt++ {
		var idBytes [4]byte
		if _, err := rand.Read(idBytes[:]); err != nil {
			return ExecutionTaskWorkspace{}, fmt.Errorf("generate unique task quota project ID: %w", err)
		}
		// Keep task workspaces in the high-bit project-ID namespace. Linux
		// distributions and administrators commonly use low IDs for other
		// project quotas; isolating O allocations reduces accidental overlap.
		projectID := int64((binary.LittleEndian.Uint32(idBytes[:]) & 0x7fffffff) | 0x80000000)
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return ExecutionTaskWorkspace{}, err
		}
		rollback := func(cause error) error { return errors.Join(cause, tx.Rollback()) }
		workspace, readErr := executionTaskWorkspaceByID(ctx, tx, userID, taskID)
		if readErr != nil {
			return ExecutionTaskWorkspace{}, rollback(readErr)
		}
		if workspace.Status != "preparing" {
			return ExecutionTaskWorkspace{}, rollback(fmt.Errorf("cannot allocate project quota after workspace status %s: %w", workspace.Status, domain.ErrConflict))
		}
		if workspace.QuotaProjectID != 0 {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				return ExecutionTaskWorkspace{}, rollbackErr
			}
			return workspace, nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE execution_task_workspaces SET quota_project_id=?,quota_limit_bytes=?,quota_state='preparing',updated_at=? WHERE user_id=? AND task_id=? AND status='preparing' AND quota_project_id=0 AND quota_state IN ('not_configured','preparing','degraded')`, projectID, limitBytes, now.UTC(), userID, taskID)
		if err != nil {
			rollbackErr := tx.Rollback()
			if isUniqueConstraintError(err) {
				if rollbackErr != nil {
					return ExecutionTaskWorkspace{}, errors.Join(err, rollbackErr)
				}
				continue
			}
			return ExecutionTaskWorkspace{}, errors.Join(err, rollbackErr)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return ExecutionTaskWorkspace{}, rollback(err)
		}
		if changed != 1 {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				return ExecutionTaskWorkspace{}, rollbackErr
			}
			continue
		}
		persisted, err := executionTaskWorkspaceByID(ctx, tx, userID, taskID)
		if err != nil {
			return ExecutionTaskWorkspace{}, rollback(err)
		}
		if persisted.QuotaProjectID != projectID || persisted.QuotaLimitBytes != limitBytes || persisted.QuotaState != "preparing" {
			return ExecutionTaskWorkspace{}, rollback(domain.ErrConflict)
		}
		if err := tx.Commit(); err != nil {
			return ExecutionTaskWorkspace{}, err
		}
		readBack, err := s.ExecutionTaskWorkspace(ctx, userID, taskID)
		if err != nil {
			return ExecutionTaskWorkspace{}, err
		}
		if readBack.QuotaProjectID != projectID || readBack.QuotaLimitBytes != limitBytes || readBack.QuotaState != "preparing" {
			return ExecutionTaskWorkspace{}, domain.ErrConflict
		}
		return readBack, nil
	}
	return ExecutionTaskWorkspace{}, errors.New("could not allocate a unique task quota project ID after 32 attempts")
}

func isUniqueConstraintError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

func executionTaskWorkspaceByID(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID, taskID string) (ExecutionTaskWorkspace, error) {
	var workspace ExecutionTaskWorkspace
	err := queryer.QueryRowContext(ctx, `SELECT user_id,task_id,source_project_id,workdir,status,quota_project_id,quota_limit_bytes,quota_state,error_text,created_at,updated_at FROM execution_task_workspaces WHERE user_id=? AND task_id=?`, userID, taskID).
		Scan(&workspace.UserID, &workspace.TaskID, &workspace.SourceProjectID, &workspace.Workdir, &workspace.Status, &workspace.QuotaProjectID, &workspace.QuotaLimitBytes, &workspace.QuotaState, &workspace.Error, &workspace.CreatedAt, &workspace.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionTaskWorkspace{}, domain.ErrNotFound
	}
	return workspace, err
}

func validExecutionWorkspaceStatus(status string) bool {
	switch status {
	case "preparing", "ready", "degraded", "releasing", "released":
		return true
	default:
		return false
	}
}

func validExecutionQuotaState(status string) bool {
	switch status {
	case "not_configured", "unavailable", "preparing", "applied", "degraded", "released":
		return true
	default:
		return false
	}
}

func validExecutionWorkspaceTransition(current, next string) bool {
	if current == next {
		return true
	}
	switch current {
	case "preparing":
		return next == "ready" || next == "degraded"
	case "degraded":
		return next == "preparing" || next == "ready" || next == "releasing" || next == "released"
	case "ready":
		return next == "degraded" || next == "releasing" || next == "released"
	case "releasing":
		return next == "degraded" || next == "released"
	case "released":
		// Startup reconciliation may discover that a supposedly released
		// kernel quota is still present. Reopen it as degraded for explicit
		// recovery while preserving the workspace and task binding.
		return next == "degraded"
	default:
		return false
	}
}
