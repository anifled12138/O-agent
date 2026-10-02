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

func TestControlSnapshotVerifiesAndRestoresDatabaseVaultAndObjects(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	if err := os.WriteFile(filepath.Join(source, "master.key"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("durable user artifact")
	digest := sha256.Sum256(content)
	artifact, err := service.StoreFromReader(ctx, owner, "record.txt", "text/plain", "snapshot-test", int64(len(content)), hex.EncodeToString(digest[:]), bytesReader(content), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeSetting(ctx, "snapshot-readback", "durable"); err != nil {
		t.Fatal(err)
	}

	snapshot := filepath.Join(t.TempDir(), "backup")
	manifest, err := CreateControlSnapshot(ctx, source, snapshot, store)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ObjectCount != 1 || manifest.ObjectBytes != int64(len(content)) {
		t.Fatalf("unexpected backup inventory: %+v", manifest)
	}
	if verified, err := VerifyControlSnapshot(ctx, snapshot); err != nil || verified != manifest {
		t.Fatalf("snapshot did not verify after creation: manifest=%+v verified=%+v err=%v", manifest, verified, err)
	}
	existingRestore := filepath.Join(t.TempDir(), "existing-restore")
	if err := os.Mkdir(existingRestore, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existingRestore, "keep"), []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreControlSnapshot(ctx, snapshot, existingRestore); err == nil {
		t.Fatal("restore overwrote an existing data directory")
	}
	preserved, err := os.ReadFile(filepath.Join(existingRestore, "keep"))
	if err != nil || string(preserved) != "preserve me" {
		t.Fatalf("failed restore changed the existing directory: contents=%q err=%v", preserved, err)
	}

	restoredPath := filepath.Join(t.TempDir(), "restored")
	if err := RestoreControlSnapshot(ctx, snapshot, restoredPath); err != nil {
		t.Fatal(err)
	}
	restoredStore, err := storage.OpenExisting(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	value, err := restoredStore.RuntimeSetting(ctx, "snapshot-readback")
	if err != nil || value != "durable" {
		t.Fatalf("restored database did not read back durable state: value=%q err=%v", value, err)
	}
	restoredArtifacts, err := New(restoredPath, restoredStore)
	if err != nil {
		t.Fatal(err)
	}
	readBack, file, err := restoredArtifacts.Open(ctx, owner, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(data) != string(content) || readBack.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("restored artifact was not readable: metadata=%+v data=%q read=%v close=%v", readBack, data, readErr, closeErr)
	}
	restoredKey, err := os.ReadFile(filepath.Join(restoredPath, "master.key"))
	if err != nil || string(restoredKey) != string(key) {
		t.Fatalf("restored vault key mismatch: bytes=%d err=%v", len(restoredKey), err)
	}
}

func TestControlSnapshotRejectsActiveUploadsAndPreservesExistingBackup(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "master.key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Begin(ctx, owner, BeginRequest{FileName: "in-flight.bin", ExpectedSize: ChunkSize + 3, IdempotencyKey: "in-flight"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup")
	if _, err := CreateControlSnapshot(ctx, source, backupPath, store); err == nil {
		t.Fatal("snapshot accepted in-flight uploads without their resumable staging chunks")
	}
	if _, err := os.Lstat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("failed snapshot left a partial directory: %v", err)
	}
	if err := os.Mkdir(backupPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupPath, "keep"), []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateControlSnapshot(ctx, source, backupPath, store); err == nil {
		t.Fatal("snapshot overwrote an existing backup path")
	}
	contents, err := os.ReadFile(filepath.Join(backupPath, "keep"))
	if err != nil || string(contents) != "user data" {
		t.Fatalf("existing backup data changed: contents=%q err=%v", contents, err)
	}
}

func TestVerifyControlSnapshotDetectsObjectCorruption(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "master.key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(source, store)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("must detect damaged content")
	digest := sha256.Sum256(content)
	artifact, err := service.StoreFromReader(ctx, owner, "damaged.txt", "text/plain", "damaged-snapshot", int64(len(content)), hex.EncodeToString(digest[:]), bytesReader(content), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "backup")
	if _, err := CreateControlSnapshot(ctx, source, snapshot, store); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(snapshot, "artifacts", "objects", "sha256", artifact.SHA256[:2], artifact.SHA256)
	if err := os.WriteFile(objectPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyControlSnapshot(ctx, snapshot); err == nil {
		t.Fatal("snapshot verification accepted a corrupted content object")
	}
	restorePath := filepath.Join(t.TempDir(), "restore")
	if err := RestoreControlSnapshot(ctx, snapshot, restorePath); err == nil {
		t.Fatal("restore accepted a corrupted snapshot")
	}
	if _, err := os.Lstat(restorePath); !os.IsNotExist(err) {
		t.Fatalf("failed restore left a partial destination: %v", err)
	}
}
