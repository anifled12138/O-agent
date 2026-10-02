package storage

import (
	"context"
	"errors"

	"axiom.local/agent/internal/domain"
)

// ExecutionTaskWorkspaces returns the durable registry used by startup quota
// reconciliation. Rows are ordered by their original creation time and stable
// task identity so diagnostics are repeatable across restarts.
func (s *Store) ExecutionTaskWorkspaces(ctx context.Context) (workspaces []ExecutionTaskWorkspace, retErr error) {
	if s == nil || s.db == nil {
		return nil, domain.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT user_id,task_id,source_project_id,workdir,status,quota_project_id,quota_limit_bytes,quota_state,error_text,created_at,updated_at FROM execution_task_workspaces ORDER BY created_at, user_id, task_id`)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		var workspace ExecutionTaskWorkspace
		if err := rows.Scan(&workspace.UserID, &workspace.TaskID, &workspace.SourceProjectID, &workspace.Workdir, &workspace.Status, &workspace.QuotaProjectID, &workspace.QuotaLimitBytes, &workspace.QuotaState, &workspace.Error, &workspace.CreatedAt, &workspace.UpdatedAt); err != nil {
			return nil, err
		}
		workspaces = append(workspaces, workspace)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return workspaces, nil
}
