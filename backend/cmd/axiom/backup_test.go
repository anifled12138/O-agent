package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"axiom.local/agent/internal/storage"
)

func TestEncryptedBackupCommandsCreateVerifyAndRestoreDurableState(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "master.key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeSetting(ctx, "cli-backup-readback", "persisted"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("O_DATA_DIR", dataDir)
	keyPath := filepath.Join(t.TempDir(), "backup.key")
	if err := runBackupCommand([]string{"keygen", keyPath}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "control.oabk")
	if err := runBackupCommand([]string{"create-encrypted", archive, keyPath}); err != nil {
		t.Fatal(err)
	}
	if err := runBackupCommand([]string{"verify-encrypted", archive, keyPath}); err != nil {
		t.Fatal(err)
	}
	restoredDir := filepath.Join(t.TempDir(), "restored")
	if err := runBackupCommand([]string{"restore-encrypted", archive, keyPath, restoredDir}); err != nil {
		t.Fatal(err)
	}
	restored, err := storage.OpenExisting(restoredDir)
	if err != nil {
		t.Fatal(err)
	}
	value, err := restored.RuntimeSetting(ctx, "cli-backup-readback")
	closeErr := restored.Close()
	if err != nil || closeErr != nil || value != "persisted" {
		t.Fatalf("CLI-restored database did not read back durable state: value=%q read=%v close=%v", value, err, closeErr)
	}
	if err := runBackupCommand([]string{"restore-encrypted", archive, keyPath, restoredDir}); err == nil {
		t.Fatal("encrypted restore command overwrote an existing destination")
	}
	preserved, err := storage.OpenExisting(restoredDir)
	if err != nil {
		t.Fatalf("existing restored state was damaged after rejected overwrite: %v", err)
	}
	defer preserved.Close()
	value, err = preserved.RuntimeSetting(ctx, "cli-backup-readback")
	if err != nil || value != "persisted" {
		t.Fatalf("existing restore target changed after rejected overwrite: value=%q err=%v", value, err)
	}
}
