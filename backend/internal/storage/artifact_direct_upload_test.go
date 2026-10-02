package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func makeLegacyArtifactUploadSchema(databasePath string) error {
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(databasePath))
	if err != nil {
		return err
	}
	_, alterErr := database.Exec(`ALTER TABLE artifact_uploads DROP COLUMN direct_upload`)
	return errors.Join(alterErr, database.Close())
}

func TestArtifactDirectUploadModePersistsAndRejectsProxyChunkMixing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("direct"))
	now := time.Now().UTC()
	upload, created, err := store.CreateArtifactUpload(ctx, ArtifactUpload{ID: "upl_direct_persist", UserID: owner, IdempotencyKey: "direct-persist", FileName: "payload.bin", MediaType: "application/octet-stream", ExpectedSize: 6, ExpectedSHA256: hex.EncodeToString(digest[:]), ChunkSize: 8 << 20, ChunkCount: 1}, now)
	if err != nil || !created || upload.DirectUpload {
		t.Fatalf("create proxy-capable upload: upload=%+v created=%v err=%v", upload, created, err)
	}
	readBack, err := store.EnableArtifactDirectUpload(ctx, owner, upload.ID, now.Add(time.Second))
	if err != nil || !readBack.DirectUpload {
		t.Fatalf("enable direct mode was not durably read back: upload=%+v err=%v", readBack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	readBack, err = store.ArtifactUpload(ctx, owner, upload.ID)
	if err != nil || !readBack.DirectUpload || readBack.Status != "uploading" {
		t.Fatalf("direct mode did not survive SQLite restart: upload=%+v err=%v", readBack, err)
	}
	if _, err := store.RecordArtifactChunk(ctx, owner, upload.ID, 0, hex.EncodeToString(digest[:]), 6, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("direct upload accepted a proxy chunk, err=%v", err)
	}
	readBack, err = store.ArtifactUpload(ctx, owner, upload.ID)
	if err != nil || !readBack.DirectUpload || readBack.Status != "uploading" {
		t.Fatalf("rejected mixed-mode mutation changed persistent upload state: upload=%+v err=%v", readBack, err)
	}
}

func TestArtifactDirectUploadModeRefusesExistingProxyChunks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("x"))
	now := time.Now().UTC()
	upload, _, err := store.CreateArtifactUpload(ctx, ArtifactUpload{ID: "upl_direct_rollback", UserID: owner, IdempotencyKey: "direct-rollback", FileName: "payload.bin", MediaType: "application/octet-stream", ExpectedSize: 1, ExpectedSHA256: hex.EncodeToString(digest[:]), ChunkSize: 8 << 20, ChunkCount: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordArtifactChunk(ctx, owner, upload.ID, 0, hex.EncodeToString(digest[:]), 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnableArtifactDirectUpload(ctx, owner, upload.ID, now.Add(time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("direct mode switch with a committed proxy chunk = %v, want conflict", err)
	}
	readBack, err := store.ArtifactUpload(ctx, owner, upload.ID)
	if err != nil || readBack.DirectUpload || readBack.Status != "uploading" {
		t.Fatalf("failed mode switch changed the upload state: upload=%+v err=%v", readBack, err)
	}
}

func TestArtifactDirectUploadMigrationAddsModeToLegacyDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("legacy"))
	upload, _, err := store.CreateArtifactUpload(ctx, ArtifactUpload{ID: "upl_direct_legacy", UserID: owner, IdempotencyKey: "direct-legacy", FileName: "payload.bin", MediaType: "application/octet-stream", ExpectedSize: 6, ExpectedSHA256: hex.EncodeToString(digest[:]), ChunkSize: 8 << 20, ChunkCount: 1}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := makeLegacyArtifactUploadSchema(filepath.Join(dir, "axiom.db")); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	readBack, err := store.ArtifactUpload(ctx, owner, upload.ID)
	if err != nil || readBack.DirectUpload {
		t.Fatalf("legacy upload did not migrate to proxy mode: upload=%+v err=%v", readBack, err)
	}
}
