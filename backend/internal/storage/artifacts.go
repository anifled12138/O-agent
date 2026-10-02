package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

type ArtifactUpload struct {
	ID             string    `json:"id"`
	UserID         string    `json:"-"`
	IdempotencyKey string    `json:"-"`
	FileName       string    `json:"fileName"`
	MediaType      string    `json:"mediaType"`
	ExpectedSize   int64     `json:"expectedSize"`
	ExpectedSHA256 string    `json:"expectedSha256,omitempty"`
	ChunkSize      int64     `json:"chunkSize"`
	ChunkCount     int64     `json:"chunkCount"`
	DirectUpload   bool      `json:"directUpload"`
	Status         string    `json:"status"`
	ArtifactID     string    `json:"artifactId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type ArtifactChunk struct {
	UploadID  string    `json:"uploadId"`
	Index     int64     `json:"index"`
	SHA256    string    `json:"sha256"`
	ByteSize  int64     `json:"byteSize"`
	CreatedAt time.Time `json:"createdAt"`
}

type Artifact struct {
	ID         string    `json:"id"`
	UserID     string    `json:"-"`
	SHA256     string    `json:"sha256"`
	ByteSize   int64     `json:"byteSize"`
	FileName   string    `json:"fileName"`
	MediaType  string    `json:"mediaType"`
	StorageKey string    `json:"-"`
	UploadID   string    `json:"uploadId"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (s *Store) CreateArtifactUpload(ctx context.Context, upload ArtifactUpload, now time.Time) (ArtifactUpload, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArtifactUpload{}, false, err
	}
	defer tx.Rollback()
	existing, err := artifactUploadByKey(ctx, tx, upload.UserID, upload.IdempotencyKey)
	if err == nil {
		if existing.FileName != upload.FileName || existing.MediaType != upload.MediaType || existing.ExpectedSize != upload.ExpectedSize || existing.ExpectedSHA256 != upload.ExpectedSHA256 || existing.ChunkSize != upload.ChunkSize || existing.ChunkCount != upload.ChunkCount {
			return ArtifactUpload{}, false, domain.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return ArtifactUpload{}, false, err
		}
		readBack, err := s.ArtifactUpload(ctx, upload.UserID, existing.ID)
		return readBack, false, err
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return ArtifactUpload{}, false, err
	}
	upload.Status = "uploading"
	upload.CreatedAt = now.UTC()
	upload.UpdatedAt = now.UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO artifact_uploads(id,user_id,idempotency_key,file_name,media_type,expected_size,expected_sha256,chunk_size,chunk_count,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'uploading',?,?)`, upload.ID, upload.UserID, upload.IdempotencyKey, upload.FileName, upload.MediaType, upload.ExpectedSize, upload.ExpectedSHA256, upload.ChunkSize, upload.ChunkCount, now.UTC(), now.UTC())
	if err != nil {
		return ArtifactUpload{}, false, err
	}
	readBack, err := artifactUploadByID(ctx, tx, upload.UserID, upload.ID)
	if err != nil {
		return ArtifactUpload{}, false, err
	}
	if readBack.Status != "uploading" || readBack.ExpectedSize != upload.ExpectedSize || readBack.ChunkCount != upload.ChunkCount {
		return ArtifactUpload{}, false, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ArtifactUpload{}, false, err
	}
	readBack, err = s.ArtifactUpload(ctx, upload.UserID, upload.ID)
	if err != nil {
		return ArtifactUpload{}, false, err
	}
	if readBack.Status != "uploading" || readBack.ExpectedSize != upload.ExpectedSize || readBack.ChunkCount != upload.ChunkCount {
		return ArtifactUpload{}, false, domain.ErrConflict
	}
	return readBack, true, nil
}

func (s *Store) ArtifactUpload(ctx context.Context, userID, id string) (ArtifactUpload, error) {
	return artifactUploadByID(ctx, s.db, userID, id)
}

// ArtifactUploadByID is an internal recovery lookup for filesystem staging
// reconciliation. Callers must not expose its user-owned fields across API
// boundaries; upload IDs are globally unique but are not authorization.
func (s *Store) ArtifactUploadByID(ctx context.Context, id string) (ArtifactUpload, error) {
	return scanArtifactUpload(s.db.QueryRowContext(ctx, `SELECT id,user_id,idempotency_key,file_name,media_type,expected_size,expected_sha256,chunk_size,chunk_count,direct_upload,status,artifact_id,created_at,updated_at FROM artifact_uploads WHERE id=?`, id))
}

// EnableArtifactDirectUpload durably selects the remote whole-object transfer
// path before a signed PUT capability is returned. It cannot be changed after
// a proxy chunk has been recorded or after finalization starts.
func (s *Store) EnableArtifactDirectUpload(ctx context.Context, userID, uploadID string, now time.Time) (ArtifactUpload, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArtifactUpload{}, err
	}
	defer tx.Rollback()
	upload, err := artifactUploadByID(ctx, tx, userID, uploadID)
	if err != nil {
		return ArtifactUpload{}, err
	}
	if upload.Status != "uploading" || upload.ExpectedSHA256 == "" {
		return ArtifactUpload{}, domain.ErrConflict
	}
	var chunks int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_upload_chunks WHERE upload_id=?`, uploadID).Scan(&chunks); err != nil {
		return ArtifactUpload{}, err
	}
	if chunks != 0 && !upload.DirectUpload {
		return ArtifactUpload{}, domain.ErrConflict
	}
	if !upload.DirectUpload {
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_uploads SET direct_upload=1,updated_at=? WHERE id=? AND user_id=? AND status='uploading' AND direct_upload=0`, now.UTC(), uploadID, userID); err != nil {
			return ArtifactUpload{}, err
		}
	}
	readBack, err := artifactUploadByID(ctx, tx, userID, uploadID)
	if err != nil || !readBack.DirectUpload {
		return ArtifactUpload{}, errors.Join(domain.ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return ArtifactUpload{}, err
	}
	readBack, err = s.ArtifactUpload(ctx, userID, uploadID)
	if err != nil {
		return ArtifactUpload{}, err
	}
	if !readBack.DirectUpload || readBack.Status != "uploading" {
		return ArtifactUpload{}, domain.ErrConflict
	}
	return readBack, nil
}

// ArtifactByUploadKey reads a finalized artifact through its stable internal
// upload idempotency key. It is used to reconcile a crash between object
// finalization and attaching that object to its owning task.
func (s *Store) ArtifactByUploadKey(ctx context.Context, userID, idempotencyKey string) (Artifact, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return Artifact{}, domain.ErrInvalid
	}
	var artifactID string
	err := s.db.QueryRowContext(ctx, `SELECT artifact_id FROM artifact_uploads WHERE user_id=? AND idempotency_key=? AND status='complete'`, userID, idempotencyKey).Scan(&artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, domain.ErrNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	return s.Artifact(ctx, userID, artifactID)
}

func (s *Store) ArtifactUploadChunks(ctx context.Context, userID, uploadID string) ([]ArtifactChunk, error) {
	if _, err := s.ArtifactUpload(ctx, userID, uploadID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT upload_id,chunk_index,sha256,byte_size,created_at FROM artifact_upload_chunks WHERE upload_id=? ORDER BY chunk_index`, uploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chunks := make([]ArtifactChunk, 0)
	for rows.Next() {
		var chunk ArtifactChunk
		if err := rows.Scan(&chunk.UploadID, &chunk.Index, &chunk.SHA256, &chunk.ByteSize, &chunk.CreatedAt); err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chunks, nil
}

func (s *Store) ArtifactUploadChunksPage(ctx context.Context, userID, uploadID string, offset, limit int64) ([]ArtifactChunk, error) {
	if offset < 0 || limit < 1 || limit > 1000 {
		return nil, domain.ErrInvalid
	}
	if _, err := s.ArtifactUpload(ctx, userID, uploadID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT upload_id,chunk_index,sha256,byte_size,created_at FROM artifact_upload_chunks WHERE upload_id=? ORDER BY chunk_index LIMIT ? OFFSET ?`, uploadID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chunks := make([]ArtifactChunk, 0)
	for rows.Next() {
		var chunk ArtifactChunk
		if err := rows.Scan(&chunk.UploadID, &chunk.Index, &chunk.SHA256, &chunk.ByteSize, &chunk.CreatedAt); err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chunks, nil
}

func (s *Store) ArtifactUploadChunkCount(ctx context.Context, userID, uploadID string) (int64, error) {
	if _, err := s.ArtifactUpload(ctx, userID, uploadID); err != nil {
		return 0, err
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_upload_chunks WHERE upload_id=?`, uploadID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) ArtifactChunk(ctx context.Context, userID, uploadID string, index int64) (ArtifactChunk, error) {
	var chunk ArtifactChunk
	err := s.db.QueryRowContext(ctx, `SELECT c.upload_id,c.chunk_index,c.sha256,c.byte_size,c.created_at FROM artifact_upload_chunks c JOIN artifact_uploads u ON u.id=c.upload_id WHERE u.user_id=? AND c.upload_id=? AND c.chunk_index=?`, userID, uploadID, index).Scan(&chunk.UploadID, &chunk.Index, &chunk.SHA256, &chunk.ByteSize, &chunk.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactChunk{}, domain.ErrNotFound
	}
	return chunk, err
}

func (s *Store) RecordArtifactChunk(ctx context.Context, userID, uploadID string, index int64, digest string, size int64, now time.Time) (ArtifactChunk, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArtifactChunk{}, err
	}
	defer tx.Rollback()
	upload, err := artifactUploadByID(ctx, tx, userID, uploadID)
	if err != nil {
		return ArtifactChunk{}, err
	}
	if upload.Status != "uploading" || upload.DirectUpload || index < 0 || index >= upload.ChunkCount {
		return ArtifactChunk{}, domain.ErrConflict
	}
	var expected int64
	if index < upload.ChunkCount-1 {
		expected = upload.ChunkSize
	} else {
		expected = upload.ExpectedSize - index*upload.ChunkSize
	}
	if size != expected {
		return ArtifactChunk{}, domain.ErrInvalid
	}
	existing, readErr := artifactChunkByIndex(ctx, tx, uploadID, index)
	if readErr == nil {
		if existing.SHA256 != digest || existing.ByteSize != size {
			return ArtifactChunk{}, domain.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return ArtifactChunk{}, err
		}
		return s.ArtifactChunk(ctx, userID, uploadID, index)
	}
	if !errors.Is(readErr, domain.ErrNotFound) {
		return ArtifactChunk{}, readErr
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_upload_chunks(upload_id,chunk_index,sha256,byte_size,created_at) VALUES(?,?,?,?,?)`, uploadID, index, digest, size, now.UTC()); err != nil {
		return ArtifactChunk{}, err
	}
	readBack, err := artifactChunkByIndex(ctx, tx, uploadID, index)
	if err != nil {
		return ArtifactChunk{}, err
	}
	if readBack.SHA256 != digest || readBack.ByteSize != size {
		return ArtifactChunk{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ArtifactChunk{}, err
	}
	readBack, err = s.ArtifactChunk(ctx, userID, uploadID, index)
	if err != nil {
		return ArtifactChunk{}, err
	}
	if readBack.SHA256 != digest || readBack.ByteSize != size {
		return ArtifactChunk{}, domain.ErrConflict
	}
	return readBack, nil
}

func (s *Store) FinalizeArtifactUpload(ctx context.Context, userID, uploadID, artifactID, digest, storageKey string, byteSize int64, now time.Time) (Artifact, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Artifact{}, false, err
	}
	defer tx.Rollback()
	upload, err := artifactUploadByID(ctx, tx, userID, uploadID)
	if err != nil {
		return Artifact{}, false, err
	}
	if upload.Status == "complete" {
		artifact, err := artifactByID(ctx, tx, userID, upload.ArtifactID)
		if err != nil {
			return Artifact{}, false, err
		}
		if artifact.SHA256 != digest || artifact.ByteSize != byteSize || artifact.StorageKey != storageKey {
			return Artifact{}, false, domain.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return Artifact{}, false, err
		}
		readBack, err := s.Artifact(ctx, userID, artifact.ID)
		return readBack, false, err
	}
	if upload.Status != "uploading" || byteSize != upload.ExpectedSize || upload.ChunkCount < 0 {
		return Artifact{}, false, domain.ErrConflict
	}
	if upload.ExpectedSHA256 != "" && upload.ExpectedSHA256 != digest {
		return Artifact{}, false, domain.ErrConflict
	}
	if upload.DirectUpload {
		var count int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_upload_chunks WHERE upload_id=?`, uploadID).Scan(&count); err != nil {
			return Artifact{}, false, err
		}
		if count != 0 || upload.ExpectedSHA256 == "" || upload.ExpectedSHA256 != digest {
			return Artifact{}, false, domain.ErrConflict
		}
	} else {
		var count, total int64
		var minIndex, maxIndex sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(byte_size),0),MIN(chunk_index),MAX(chunk_index) FROM artifact_upload_chunks WHERE upload_id=?`, uploadID).Scan(&count, &total, &minIndex, &maxIndex); err != nil {
			return Artifact{}, false, err
		}
		if count != upload.ChunkCount || total != upload.ExpectedSize {
			return Artifact{}, false, domain.ErrConflict
		}
		if count > 0 && (!minIndex.Valid || !maxIndex.Valid || minIndex.Int64 != 0 || maxIndex.Int64 != upload.ChunkCount-1) {
			return Artifact{}, false, domain.ErrConflict
		}
	}
	artifact := Artifact{ID: artifactID, UserID: userID, SHA256: digest, ByteSize: byteSize, FileName: upload.FileName, MediaType: upload.MediaType, StorageKey: storageKey, UploadID: uploadID, CreatedAt: now.UTC()}
	if artifact.ID == "" {
		return Artifact{}, false, domain.ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts(id,user_id,sha256,byte_size,file_name,media_type,storage_key,upload_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, artifact.ID, userID, digest, byteSize, artifact.FileName, artifact.MediaType, storageKey, uploadID, now.UTC()); err != nil {
		return Artifact{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE artifact_uploads SET status='complete',artifact_id=?,updated_at=? WHERE id=? AND user_id=? AND status='uploading'`, artifact.ID, now.UTC(), uploadID, userID)
	if err != nil {
		return Artifact{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Artifact{}, false, err
	}
	if changed != 1 {
		return Artifact{}, false, domain.ErrConflict
	}
	updated, err := artifactUploadByID(ctx, tx, userID, uploadID)
	if err != nil {
		return Artifact{}, false, err
	}
	if updated.Status != "complete" || updated.ArtifactID != artifact.ID {
		return Artifact{}, false, domain.ErrConflict
	}
	readBack, err := artifactByID(ctx, tx, userID, artifact.ID)
	if err != nil {
		return Artifact{}, false, err
	}
	if readBack.SHA256 != digest || readBack.ByteSize != byteSize || readBack.StorageKey != storageKey {
		return Artifact{}, false, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return Artifact{}, false, err
	}
	readBack, err = s.Artifact(ctx, userID, artifact.ID)
	if err != nil {
		return Artifact{}, false, err
	}
	if readBack.SHA256 != digest || readBack.ByteSize != byteSize || readBack.StorageKey != storageKey {
		return Artifact{}, false, domain.ErrConflict
	}
	return readBack, true, nil
}

func (s *Store) Artifact(ctx context.Context, userID, id string) (Artifact, error) {
	return artifactByID(ctx, s.db, userID, id)
}

// ArtifactManifests returns internal metadata needed to copy exactly the
// content objects referenced by a database snapshot. It is not a user API.
func (s *Store) ArtifactManifests(ctx context.Context) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,sha256,byte_size,file_name,media_type,storage_key,upload_id,created_at FROM artifacts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Artifact, 0)
	for rows.Next() {
		var item Artifact
		if err := rows.Scan(&item.ID, &item.UserID, &item.SHA256, &item.ByteSize, &item.FileName, &item.MediaType, &item.StorageKey, &item.UploadID, &item.CreatedAt); err != nil {
			return nil, err
		}
		if item.ByteSize < 0 {
			return nil, fmt.Errorf("artifact %q has invalid negative size", item.ID)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// ActiveArtifactUploadCount reports whether a snapshot would contain upload
// records whose chunk files are transient and intentionally outside the
// finalized artifact object set.
func (s *Store) ActiveArtifactUploadCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifact_uploads WHERE status='uploading'`).Scan(&count)
	return count, err
}

func (s *Store) AttachExecutionTaskArtifact(ctx context.Context, userID, taskID, artifactID, role string, now time.Time) error {
	role = strings.TrimSpace(role)
	if role == "" || len(role) > 64 {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := executionTaskByID(ctx, tx, userID, taskID); err != nil {
		return err
	}
	if _, err := artifactByID(ctx, tx, userID, artifactID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_task_artifacts(task_id,artifact_id,role,created_at) VALUES(?,?,?,?) ON CONFLICT(task_id,artifact_id,role) DO NOTHING`, taskID, artifactID, role, now.UTC()); err != nil {
		return err
	}
	var persistedRole string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM execution_task_artifacts WHERE task_id=? AND artifact_id=? AND role=?`, taskID, artifactID, role).Scan(&persistedRole); err != nil {
		return err
	}
	if persistedRole != role {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	artifacts, err := s.ExecutionTaskArtifacts(ctx, userID, taskID)
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if artifact.ID == artifactID && artifact.Role == role {
			return nil
		}
	}
	return domain.ErrConflict
}

// AttachExecutionTaskArtifactForLease publishes an artifact produced by a
// node only while its task lease is current. The lease check and association
// insert share one transaction so revocation cannot race the publication.
func (s *Store) AttachExecutionTaskArtifactForLease(ctx context.Context, nodeID, taskID, leaseHash, artifactID, role string, now time.Time) error {
	role = strings.TrimSpace(role)
	if role == "" || len(role) > 64 || strings.TrimSpace(leaseHash) == "" {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID, status, storedLease string
	var leaseUntil sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT user_id,status,lease_token_hash,lease_until FROM execution_tasks WHERE id=? AND node_id=?`, taskID, nodeID).
		Scan(&userID, &status, &storedLease, &leaseUntil); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	if storedLease != leaseHash || !leaseUntil.Valid || !leaseUntil.Time.After(now.UTC()) {
		return domain.ErrUnauthorized
	}
	if status != "leased" && status != "accepted" && status != "running" {
		return domain.ErrConflict
	}
	if _, err := artifactByID(ctx, tx, userID, artifactID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_task_artifacts(task_id,artifact_id,role,created_at) VALUES(?,?,?,?) ON CONFLICT(task_id,artifact_id,role) DO NOTHING`, taskID, artifactID, role, now.UTC()); err != nil {
		return err
	}
	var persistedRole string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM execution_task_artifacts WHERE task_id=? AND artifact_id=? AND role=?`, taskID, artifactID, role).Scan(&persistedRole); err != nil {
		return err
	}
	if persistedRole != role {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	items, err := s.ExecutionTaskArtifacts(ctx, userID, taskID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID == artifactID && item.Role == role {
			return nil
		}
	}
	return domain.ErrConflict
}

type ExecutionTaskArtifact struct {
	Artifact
	Role string `json:"role"`
}

func (s *Store) ExecutionTaskArtifacts(ctx context.Context, userID, taskID string) ([]ExecutionTaskArtifact, error) {
	if _, err := s.ExecutionTask(ctx, userID, taskID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.user_id,a.sha256,a.byte_size,a.file_name,a.media_type,a.storage_key,a.upload_id,a.created_at,ta.role
FROM execution_task_artifacts ta JOIN execution_tasks t ON t.id=ta.task_id JOIN artifacts a ON a.id=ta.artifact_id
WHERE t.id=? AND t.user_id=? AND a.user_id=t.user_id ORDER BY ta.created_at,a.id`, taskID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ExecutionTaskArtifact, 0)
	for rows.Next() {
		var item ExecutionTaskArtifact
		if err := rows.Scan(&item.ID, &item.UserID, &item.SHA256, &item.ByteSize, &item.FileName, &item.MediaType, &item.StorageKey, &item.UploadID, &item.CreatedAt, &item.Role); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

type artifactQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func artifactUploadByKey(ctx context.Context, q artifactQueryer, userID, key string) (ArtifactUpload, error) {
	return scanArtifactUpload(q.QueryRowContext(ctx, `SELECT id,user_id,idempotency_key,file_name,media_type,expected_size,expected_sha256,chunk_size,chunk_count,direct_upload,status,artifact_id,created_at,updated_at FROM artifact_uploads WHERE user_id=? AND idempotency_key=?`, userID, key))
}
func artifactUploadByID(ctx context.Context, q artifactQueryer, userID, id string) (ArtifactUpload, error) {
	return scanArtifactUpload(q.QueryRowContext(ctx, `SELECT id,user_id,idempotency_key,file_name,media_type,expected_size,expected_sha256,chunk_size,chunk_count,direct_upload,status,artifact_id,created_at,updated_at FROM artifact_uploads WHERE id=? AND user_id=?`, id, userID))
}
func scanArtifactUpload(row taskScanner) (ArtifactUpload, error) {
	var upload ArtifactUpload
	err := row.Scan(&upload.ID, &upload.UserID, &upload.IdempotencyKey, &upload.FileName, &upload.MediaType, &upload.ExpectedSize, &upload.ExpectedSHA256, &upload.ChunkSize, &upload.ChunkCount, &upload.DirectUpload, &upload.Status, &upload.ArtifactID, &upload.CreatedAt, &upload.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactUpload{}, domain.ErrNotFound
	}
	return upload, err
}
func artifactChunkByIndex(ctx context.Context, q artifactQueryer, uploadID string, index int64) (ArtifactChunk, error) {
	var chunk ArtifactChunk
	err := q.QueryRowContext(ctx, `SELECT upload_id,chunk_index,sha256,byte_size,created_at FROM artifact_upload_chunks WHERE upload_id=? AND chunk_index=?`, uploadID, index).Scan(&chunk.UploadID, &chunk.Index, &chunk.SHA256, &chunk.ByteSize, &chunk.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactChunk{}, domain.ErrNotFound
	}
	return chunk, err
}
func artifactByID(ctx context.Context, q artifactQueryer, userID, id string) (Artifact, error) {
	var artifact Artifact
	err := q.QueryRowContext(ctx, `SELECT id,user_id,sha256,byte_size,file_name,media_type,storage_key,upload_id,created_at FROM artifacts WHERE id=? AND user_id=?`, id, userID).
		Scan(&artifact.ID, &artifact.UserID, &artifact.SHA256, &artifact.ByteSize, &artifact.FileName, &artifact.MediaType, &artifact.StorageKey, &artifact.UploadID, &artifact.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, domain.ErrNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	if artifact.ByteSize < 0 {
		return Artifact{}, fmt.Errorf("artifact %q has invalid negative size", id)
	}
	return artifact, nil
}
