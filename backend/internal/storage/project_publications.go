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

func scanProjectPublication(row interface{ Scan(...any) error }) (domain.ProjectPublication, error) {
	var p domain.ProjectPublication
	var submodulesJSON []byte
	err := row.Scan(&p.ID, &p.UserID, &p.ProjectID, &p.IdempotencyKey, &p.TargetBranch,
		&p.ExpectedRemoteSHA, &p.CommitSHA, &p.RemoteSHA, &p.Status, &p.Error, &submodulesJSON, &p.CreatedAt, &p.UpdatedAt)
	if err == nil {
		if len(submodulesJSON) == 0 {
			submodulesJSON = []byte("[]")
		}
		err = json.Unmarshal(submodulesJSON, &p.Submodules)
	}
	return p, err
}

const projectPublicationSelect = `SELECT id,user_id,project_id,idempotency_key,target_branch,expected_remote_sha,commit_sha,remote_sha,status,error_text,submodules_json,created_at,updated_at FROM project_publications`

func publicationSubmodulesJSON(items []domain.ProjectPublicationSubmodule) ([]byte, error) {
	if items == nil {
		items = []domain.ProjectPublicationSubmodule{}
	}
	return json.Marshal(items)
}

func publicationIdentityMatches(existing, requested domain.ProjectPublication) bool {
	if existing.ProjectID != requested.ProjectID || existing.TargetBranch != requested.TargetBranch || existing.ExpectedRemoteSHA != requested.ExpectedRemoteSHA || existing.CommitSHA != requested.CommitSHA {
		return false
	}
	if len(existing.Submodules) != len(requested.Submodules) {
		return false
	}
	for i := range existing.Submodules {
		left, right := existing.Submodules[i], requested.Submodules[i]
		if left.Path != right.Path || left.RepositoryURL != right.RepositoryURL || left.CommitSHA != right.CommitSHA || left.Ref != right.Ref {
			return false
		}
	}
	return true
}

// BeginProjectPublication durably reserves a push before any external Git
// side effect. A repeated idempotency key returns the original record only if
// its complete request identity matches.
func (s *Store) BeginProjectPublication(ctx context.Context, p domain.ProjectPublication) (domain.ProjectPublication, bool, error) {
	var existing domain.ProjectPublication
	existing, err := scanProjectPublication(s.db.QueryRowContext(ctx, projectPublicationSelect+` WHERE user_id=? AND idempotency_key=?`, p.UserID, p.IdempotencyKey))
	if err == nil {
		if !publicationIdentityMatches(existing, p) {
			return existing, false, domain.ErrConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return p, false, err
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	submodulesJSON, err := publicationSubmodulesJSON(p.Submodules)
	if err != nil {
		return p, false, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO project_publications(id,user_id,project_id,idempotency_key,target_branch,expected_remote_sha,commit_sha,remote_sha,status,error_text,submodules_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.UserID, p.ProjectID, p.IdempotencyKey, p.TargetBranch, p.ExpectedRemoteSHA, p.CommitSHA, "", "publishing", "", submodulesJSON, now, now)
	if err != nil {
		// Resolve a concurrent duplicate key to its durable winner.
		existing, readErr := scanProjectPublication(s.db.QueryRowContext(ctx, projectPublicationSelect+` WHERE user_id=? AND idempotency_key=?`, p.UserID, p.IdempotencyKey))
		if readErr == nil {
			if !publicationIdentityMatches(existing, p) {
				return existing, false, domain.ErrConflict
			}
			return existing, false, nil
		}
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
			return p, false, domain.ErrConflict
		}
		return p, false, fmt.Errorf("reserve project publication: %w", err)
	}
	readBack, err := scanProjectPublication(s.db.QueryRowContext(ctx, projectPublicationSelect+` WHERE id=? AND user_id=?`, p.ID, p.UserID))
	if err != nil {
		return p, false, fmt.Errorf("read reserved project publication: %w", err)
	}
	if readBack.IdempotencyKey != p.IdempotencyKey || !publicationIdentityMatches(readBack, p) || readBack.Status != "publishing" {
		return readBack, false, domain.ErrConflict
	}
	return readBack, true, nil
}

func (s *Store) ProjectPublication(ctx context.Context, userID, projectID, id string) (domain.ProjectPublication, error) {
	p, err := scanProjectPublication(s.db.QueryRowContext(ctx, projectPublicationSelect+` WHERE user_id=? AND project_id=? AND id=?`, userID, projectID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, domain.ErrNotFound
	}
	return p, err
}

func (s *Store) ProjectPublicationByIdempotencyKey(ctx context.Context, userID, key string) (domain.ProjectPublication, error) {
	p, err := scanProjectPublication(s.db.QueryRowContext(ctx, projectPublicationSelect+` WHERE user_id=? AND idempotency_key=?`, userID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return p, domain.ErrNotFound
	}
	return p, err
}

func (s *Store) ListProjectPublications(ctx context.Context, userID, projectID string) ([]domain.ProjectPublication, error) {
	rows, err := s.db.QueryContext(ctx, projectPublicationSelect+` WHERE user_id=? AND project_id=? ORDER BY created_at DESC`, userID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []domain.ProjectPublication{}
	for rows.Next() {
		p, err := scanProjectPublication(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, p)
	}
	return items, rows.Err()
}

// SetProjectPublicationState applies a compare-and-swap state transition and
// verifies the durable row. External side effects are never retried here.
func (s *Store) SetProjectPublicationState(ctx context.Context, userID, projectID, id, status, remoteSHA, message string) (domain.ProjectPublication, error) {
	if status != "published" && status != "failed" && status != "needs_reconciliation" {
		return domain.ProjectPublication{}, domain.ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `UPDATE project_publications SET status=?,remote_sha=?,error_text=?,updated_at=? WHERE user_id=? AND project_id=? AND id=? AND status IN ('publishing','needs_reconciliation')`, status, remoteSHA, message, time.Now().UTC(), userID, projectID, id)
	if err != nil {
		return domain.ProjectPublication{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return domain.ProjectPublication{}, err
	}
	if n == 0 {
		current, readErr := s.ProjectPublication(ctx, userID, projectID, id)
		if readErr != nil {
			return current, readErr
		}
		if current.Status == status && current.RemoteSHA == remoteSHA && current.Error == message {
			return current, nil
		}
		return current, domain.ErrConflict
	}
	readBack, err := s.ProjectPublication(ctx, userID, projectID, id)
	if err != nil {
		return readBack, err
	}
	if readBack.Status != status || readBack.RemoteSHA != remoteSHA || readBack.Error != message {
		return readBack, domain.ErrConflict
	}
	return readBack, nil
}

// SetProjectPublicationSubmoduleState records authoritative child-ref
// read-back evidence while preserving the immutable publication plan.
func (s *Store) SetProjectPublicationSubmoduleState(ctx context.Context, userID, projectID, id, path, status, remoteSHA, message string) (domain.ProjectPublication, error) {
	if status != "publishing" && status != "published" && status != "failed" && status != "needs_reconciliation" {
		return domain.ProjectPublication{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ProjectPublication{}, err
	}
	defer tx.Rollback()
	p, err := scanProjectPublication(tx.QueryRowContext(ctx, projectPublicationSelect+` WHERE user_id=? AND project_id=? AND id=?`, userID, projectID, id))
	if err != nil {
		return p, err
	}
	if p.Status != "publishing" && p.Status != "needs_reconciliation" {
		return p, domain.ErrConflict
	}
	found := false
	for i := range p.Submodules {
		if p.Submodules[i].Path == path {
			current := p.Submodules[i]
			if current.Status == "published" {
				if status == "published" && (remoteSHA != current.CommitSHA || message != "") {
					return p, domain.ErrConflict
				}
				if status != "published" && status != "needs_reconciliation" {
					return p, domain.ErrConflict
				}
			}
			if current.Status == "failed" && (status != current.Status || remoteSHA != current.RemoteSHA || message != current.Error) {
				return p, domain.ErrConflict
			}
			if status == "publishing" && current.Status != "pending" && current.Status != "publishing" {
				return p, domain.ErrConflict
			}
			if status == "published" && remoteSHA != p.Submodules[i].CommitSHA {
				return p, domain.ErrConflict
			}
			p.Submodules[i].Status = status
			p.Submodules[i].RemoteSHA = remoteSHA
			p.Submodules[i].Error = message
			found = true
			break
		}
	}
	if !found {
		return p, domain.ErrNotFound
	}
	payload, err := publicationSubmodulesJSON(p.Submodules)
	if err != nil {
		return p, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_publications SET submodules_json=?,updated_at=? WHERE user_id=? AND project_id=? AND id=? AND status IN ('publishing','needs_reconciliation')`, payload, time.Now().UTC(), userID, projectID, id); err != nil {
		return p, err
	}
	p, err = scanProjectPublication(tx.QueryRowContext(ctx, projectPublicationSelect+` WHERE user_id=? AND project_id=? AND id=?`, userID, projectID, id))
	if err != nil {
		return p, err
	}
	for _, item := range p.Submodules {
		if item.Path == path && (item.Status != status || item.RemoteSHA != remoteSHA || item.Error != message) {
			return p, domain.ErrConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return p, err
	}
	return s.ProjectPublication(ctx, userID, projectID, id)
}

// CompleteProjectPublication atomically marks the remote-confirmed push as
// published and advances the project baseline when publishing its configured
// branch. It rejects any state that does not match the immutable reservation.
func (s *Store) CompleteProjectPublication(ctx context.Context, userID, projectID, id, remoteSHA string) (domain.ProjectPublication, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ProjectPublication{}, err
	}
	defer tx.Rollback()
	p, err := scanProjectPublication(tx.QueryRowContext(ctx, projectPublicationSelect+` WHERE user_id=? AND project_id=? AND id=?`, userID, projectID, id))
	if err != nil {
		return p, err
	}
	if remoteSHA == "" || remoteSHA != p.CommitSHA {
		return p, domain.ErrConflict
	}
	for _, submodule := range p.Submodules {
		if submodule.Status != "published" || submodule.RemoteSHA != submodule.CommitSHA {
			return p, fmt.Errorf("submodule %q has no authoritative published-ref read-back: %w", submodule.Path, domain.ErrConflict)
		}
	}
	if p.Status != "publishing" && p.Status != "needs_reconciliation" {
		if p.Status == "published" && p.RemoteSHA == remoteSHA {
			project, readErr := projectTx(ctx, tx, userID, projectID)
			if readErr != nil {
				return p, readErr
			}
			if project.RemoteBranch == p.TargetBranch && project.ResolvedCommit != remoteSHA {
				return p, domain.ErrConflict
			}
			return p, nil
		}
		return p, domain.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_publications SET status='published',remote_sha=?,error_text='',updated_at=? WHERE id=? AND user_id=? AND status IN ('publishing','needs_reconciliation')`, remoteSHA, time.Now().UTC(), id, userID); err != nil {
		return p, err
	}
	project, err := projectTx(ctx, tx, userID, projectID)
	if err != nil {
		return p, err
	}
	if project.RemoteBranch == p.TargetBranch {
		if _, err = tx.ExecContext(ctx, `UPDATE projects SET resolved_commit=?,updated_at=? WHERE id=? AND user_id=?`, remoteSHA, time.Now().UTC(), projectID, userID); err != nil {
			return p, err
		}
	}
	p, err = scanProjectPublication(tx.QueryRowContext(ctx, projectPublicationSelect+` WHERE user_id=? AND project_id=? AND id=?`, userID, projectID, id))
	if err != nil || p.Status != "published" || p.RemoteSHA != remoteSHA {
		if err != nil {
			return p, err
		}
		return p, domain.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return p, err
	}
	publicationReadBack, err := s.ProjectPublication(ctx, userID, projectID, id)
	if err != nil {
		return publicationReadBack, err
	}
	if p.TargetBranch == project.RemoteBranch {
		projectReadBack, projectErr := s.Project(ctx, userID, projectID)
		if projectErr != nil {
			return publicationReadBack, projectErr
		}
		if projectReadBack.ResolvedCommit != remoteSHA {
			return publicationReadBack, domain.ErrConflict
		}
	}
	return publicationReadBack, nil
}
