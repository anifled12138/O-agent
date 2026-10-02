package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupDatabaseCreatesVerifiedOnlineSnapshot(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetRuntimeSetting(ctx, "backup-snapshot-proof", "before-snapshot"); err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "axiom.db")
	if err := store.BackupDatabase(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeSetting(ctx, "backup-snapshot-proof", "after-snapshot"); err != nil {
		t.Fatal(err)
	}
	backupStore, err := OpenExisting(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	defer backupStore.Close()
	value, err := backupStore.RuntimeSetting(ctx, "backup-snapshot-proof")
	if err != nil || value != "before-snapshot" {
		t.Fatalf("backup did not preserve its consistent snapshot: value=%q err=%v", value, err)
	}
	liveValue, err := store.RuntimeSetting(ctx, "backup-snapshot-proof")
	if err != nil || liveValue != "after-snapshot" {
		t.Fatalf("live database changed while backup ran: value=%q err=%v", liveValue, err)
	}
}

func TestBackupDatabaseRefusesExistingDestinationWithoutChangingIt(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backupPath := filepath.Join(t.TempDir(), "already-owned.db")
	const contents = "preserve existing backup"
	if err := os.WriteFile(backupPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.BackupDatabase(context.Background(), backupPath); err == nil {
		t.Fatal("backup overwrote an existing destination")
	}
	readBack, err := os.ReadFile(backupPath)
	if err != nil || string(readBack) != contents {
		t.Fatalf("failed backup changed the pre-existing file: contents=%q err=%v", readBack, err)
	}
}
