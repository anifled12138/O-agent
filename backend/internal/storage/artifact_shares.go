package storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"axiom.local/agent/internal/domain"
)

// ArtifactShareLink is the durable, revocable metadata for a bearer download
// link. The bearer itself is returned only once by the HTTP API; only its
// SHA-256 digest is stored here.
type ArtifactShareLink struct {
	ID         string     `json:"id"`
	ArtifactID string     `json:"artifactId"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

func (s *Store) CreateArtifactShareLink(ctx context.Context, userID, artifactID, id, tokenHash string, expiresAt, now time.Time) (ArtifactShareLink, error) {
	if s == nil || userID == "" || artifactID == "" || id == "" || len(tokenHash) != 64 || !expiresAt.After(now) {
		return ArtifactShareLink{}, domain.ErrInvalid
	}
	if _, err := hex.DecodeString(tokenHash); err != nil {
		return ArtifactShareLink{}, domain.ErrInvalid
	}
	if _, err := artifactByID(ctx, s.db, userID, artifactID); err != nil {
		return ArtifactShareLink{}, err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO artifact_share_links(id,user_id,artifact_id,token_hash,created_at,expires_at) VALUES(?,?,?,?,?,?)`, id, userID, artifactID, tokenHash, now.UTC(), expiresAt.UTC())
	if err != nil {
		return ArtifactShareLink{}, err
	}
	link, err := s.ArtifactShareLink(ctx, userID, id)
	if err != nil {
		return ArtifactShareLink{}, fmt.Errorf("read back created artifact share link: %w", err)
	}
	if link.ArtifactID != artifactID || !link.ExpiresAt.Equal(expiresAt.UTC()) {
		return ArtifactShareLink{}, fmt.Errorf("created artifact share link did not read back: %w", domain.ErrConflict)
	}
	return link, nil
}

func (s *Store) ArtifactShareLink(ctx context.Context, userID, id string) (ArtifactShareLink, error) {
	if s == nil || userID == "" || id == "" {
		return ArtifactShareLink{}, domain.ErrInvalid
	}
	return scanArtifactShareLink(s.db.QueryRowContext(ctx, `SELECT id,artifact_id,created_at,expires_at,revoked_at FROM artifact_share_links WHERE user_id=? AND id=?`, userID, id))
}

func (s *Store) ArtifactShareLinks(ctx context.Context, userID, artifactID string) ([]ArtifactShareLink, error) {
	if s == nil || userID == "" || artifactID == "" {
		return nil, domain.ErrInvalid
	}
	if _, err := artifactByID(ctx, s.db, userID, artifactID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,artifact_id,created_at,expires_at,revoked_at FROM artifact_share_links WHERE user_id=? AND artifact_id=? ORDER BY created_at DESC,id`, userID, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ArtifactShareLink, 0)
	for rows.Next() {
		item, err := scanArtifactShareLink(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) RevokeArtifactShareLink(ctx context.Context, userID, id string, now time.Time) (ArtifactShareLink, error) {
	if s == nil || userID == "" || id == "" {
		return ArtifactShareLink{}, domain.ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `UPDATE artifact_share_links SET revoked_at=? WHERE user_id=? AND id=? AND revoked_at IS NULL`, now.UTC(), userID, id)
	if err != nil {
		return ArtifactShareLink{}, err
	}
	if _, err := result.RowsAffected(); err != nil {
		return ArtifactShareLink{}, err
	}
	link, err := s.ArtifactShareLink(ctx, userID, id)
	if err != nil {
		return ArtifactShareLink{}, err
	}
	if link.RevokedAt == nil {
		return ArtifactShareLink{}, fmt.Errorf("artifact share link revocation did not read back: %w", domain.ErrConflict)
	}
	return link, nil
}

func (s *Store) ArtifactByShareToken(ctx context.Context, tokenHash string, now time.Time) (Artifact, error) {
	if s == nil || len(tokenHash) != 64 {
		return Artifact{}, domain.ErrNotFound
	}
	var artifact Artifact
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.user_id,a.sha256,a.byte_size,a.file_name,a.media_type,a.storage_key,a.upload_id,a.created_at
FROM artifact_share_links l JOIN artifacts a ON a.id=l.artifact_id AND a.user_id=l.user_id
WHERE l.token_hash=? AND l.expires_at>? AND l.revoked_at IS NULL`, tokenHash, now.UTC()).
		Scan(&artifact.ID, &artifact.UserID, &artifact.SHA256, &artifact.ByteSize, &artifact.FileName, &artifact.MediaType, &artifact.StorageKey, &artifact.UploadID, &artifact.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, domain.ErrNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	if artifact.ByteSize < 0 {
		return Artifact{}, fmt.Errorf("shared artifact %q has invalid negative size", artifact.ID)
	}
	return artifact, nil
}

type artifactShareLinkScanner interface{ Scan(...any) error }

func scanArtifactShareLink(row artifactShareLinkScanner) (ArtifactShareLink, error) {
	var link ArtifactShareLink
	var revoked sql.NullTime
	err := row.Scan(&link.ID, &link.ArtifactID, &link.CreatedAt, &link.ExpiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactShareLink{}, domain.ErrNotFound
	}
	if err != nil {
		return ArtifactShareLink{}, err
	}
	if revoked.Valid {
		value := revoked.Time.UTC()
		link.RevokedAt = &value
	}
	return link, nil
}
