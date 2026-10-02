package artifactstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/storage"
)

func TestBackupReferencedObjectsMatchesDatabaseSnapshot(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	store, err := storage.Open(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(sourceDir, store)
	if err != nil {
		t.Fatal(err)
	}
	firstBytes := []byte("snapshot object payload")
	firstHash := sha256.Sum256(firstBytes)
	firstDigest := hex.EncodeToString(firstHash[:])
	first, err := service.StoreFromReader(ctx, owner, "first.txt", "text/plain", "backup-first", int64(len(firstBytes)), firstDigest, bytesReader(firstBytes), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := service.StoreFromReader(ctx, owner, "first-copy.txt", "text/plain", "backup-first-duplicate", int64(len(firstBytes)), firstDigest, bytesReader(firstBytes), time.Now().UTC())
	if err != nil || duplicate.SHA256 != first.SHA256 {
		t.Fatalf("deduplicated source artifact was not created: artifact=%+v err=%v", duplicate, err)
	}
	backupDir := filepath.Join(t.TempDir(), "snapshot")
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backupDatabase := filepath.Join(backupDir, "axiom.db")
	if err := store.BackupDatabase(ctx, backupDatabase); err != nil {
		t.Fatal(err)
	}
	snapshotStore, err := storage.OpenExisting(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshotStore.Close()
	lateBytes := []byte("created after the database snapshot")
	lateHash := sha256.Sum256(lateBytes)
	lateArtifact, err := service.StoreFromReader(ctx, owner, "later.txt", "text/plain", "backup-late", int64(len(lateBytes)), hex.EncodeToString(lateHash[:]), bytesReader(lateBytes), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := BackupReferencedObjects(ctx, sourceDir, backupDir, snapshotStore)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ObjectCount != 1 || summary.ObjectBytes != int64(len(firstBytes)) {
		t.Fatalf("backup copied objects outside the snapshot manifest or missed content deduplication: %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "artifacts", "objects", "sha256", first.SHA256[:2], first.SHA256)); err != nil {
		t.Fatalf("snapshot-referenced object is absent from backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "artifacts", "objects", "sha256", lateArtifact.SHA256[:2], lateArtifact.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("artifact created after database snapshot leaked into snapshot object set: %v", err)
	}
	restored, err := New(backupDir, snapshotStore)
	if err != nil {
		t.Fatal(err)
	}
	readBack, file, err := restored.Open(ctx, owner, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(actual) != string(firstBytes) || readBack.ByteSize != int64(len(firstBytes)) {
		t.Fatalf("restored snapshot artifact did not read back verified content: metadata=%+v bytes=%q read=%v close=%v", readBack, actual, readErr, closeErr)
	}
}

func TestBackupReferencedObjectsFailsOnMissingSourceObject(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	store, err := storage.Open(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(sourceDir, store)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("required backup object")
	digest := sha256.Sum256(content)
	artifact, err := service.StoreFromReader(ctx, owner, "required.txt", "text/plain", "backup-missing", int64(len(content)), hex.EncodeToString(digest[:]), bytesReader(content), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	if err := os.Remove(service.objectPath(artifact.SHA256)); err != nil {
		t.Fatal(err)
	}
	if _, err := BackupReferencedObjects(ctx, sourceDir, backupDir, store); err == nil {
		t.Fatal("backup accepted a database manifest whose content object was missing")
	}
}
