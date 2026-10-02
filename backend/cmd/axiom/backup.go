package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/config"
	"axiom.local/agent/internal/storage"
)

func runBackupCommand(args []string) (retErr error) {
	if len(args) == 0 {
		return fmt.Errorf("usage: axiom backup <keygen KEY_FILE | create DESTINATION [DATA_DIR] | verify SNAPSHOT | restore SNAPSHOT NEW_DATA_DIR | create-encrypted DESTINATION KEY_FILE [DATA_DIR] | verify-encrypted SNAPSHOT KEY_FILE | restore-encrypted SNAPSHOT KEY_FILE NEW_DATA_DIR>")
	}
	ctx := context.Background()
	switch args[0] {
	case "keygen":
		if len(args) != 2 {
			return fmt.Errorf("usage: axiom backup keygen KEY_FILE")
		}
		if err := artifactstore.GenerateControlSnapshotKey(args[1]); err != nil {
			return err
		}
		absolute, err := filepath.Abs(args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Printf("created and verified independent control snapshot key at %s\n", absolute)
		return err
	case "create":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("usage: axiom backup create DESTINATION [DATA_DIR]")
		}
		dataDir := config.Load().DataDir
		if len(args) == 3 {
			dataDir = args[2]
		}
		store, err := storage.OpenExisting(dataDir)
		if err != nil {
			return fmt.Errorf("open existing O data directory: %w", err)
		}
		manifest, backupErr := artifactstore.CreateControlSnapshot(ctx, dataDir, args[1], store)
		closeErr := store.Close()
		if err := errors.Join(backupErr, closeErr); err != nil {
			return err
		}
		return printBackupManifest(manifest)
	case "create-encrypted":
		if len(args) < 3 || len(args) > 4 {
			return fmt.Errorf("usage: axiom backup create-encrypted DESTINATION KEY_FILE [DATA_DIR]")
		}
		key, err := artifactstore.LoadControlSnapshotKey(args[2])
		if err != nil {
			return fmt.Errorf("load control snapshot key: %w", err)
		}
		dataDir := config.Load().DataDir
		if len(args) == 4 {
			dataDir = args[3]
		}
		store, err := storage.OpenExisting(dataDir)
		if err != nil {
			return fmt.Errorf("open existing O data directory: %w", err)
		}
		manifest, backupErr := artifactstore.CreateEncryptedControlSnapshot(ctx, dataDir, args[1], store, key)
		closeErr := store.Close()
		if err := errors.Join(backupErr, closeErr); err != nil {
			return err
		}
		return printBackupManifest(manifest)
	case "verify":
		if len(args) != 2 {
			return fmt.Errorf("usage: axiom backup verify SNAPSHOT")
		}
		manifest, err := artifactstore.VerifyControlSnapshot(ctx, args[1])
		if err != nil {
			return err
		}
		return printBackupManifest(manifest)
	case "restore":
		if len(args) != 3 {
			return fmt.Errorf("usage: axiom backup restore SNAPSHOT NEW_DATA_DIR")
		}
		if err := artifactstore.RestoreControlSnapshot(ctx, args[1], args[2]); err != nil {
			return err
		}
		absolute, err := filepath.Abs(args[2])
		if err != nil {
			return err
		}
		info, err := os.Stat(filepath.Join(absolute, "axiom.db"))
		if err != nil || !info.Mode().IsRegular() {
			return errors.Join(fmt.Errorf("restored database did not read back as a regular file"), err)
		}
		_, err = fmt.Printf("restored verified control snapshot to %s\n", absolute)
		return err
	case "verify-encrypted":
		if len(args) != 3 {
			return fmt.Errorf("usage: axiom backup verify-encrypted SNAPSHOT KEY_FILE")
		}
		key, err := artifactstore.LoadControlSnapshotKey(args[2])
		if err != nil {
			return fmt.Errorf("load control snapshot key: %w", err)
		}
		manifest, err := artifactstore.VerifyEncryptedControlSnapshot(ctx, args[1], key)
		if err != nil {
			return err
		}
		return printBackupManifest(manifest)
	case "restore-encrypted":
		if len(args) != 4 {
			return fmt.Errorf("usage: axiom backup restore-encrypted SNAPSHOT KEY_FILE NEW_DATA_DIR")
		}
		key, err := artifactstore.LoadControlSnapshotKey(args[2])
		if err != nil {
			return fmt.Errorf("load control snapshot key: %w", err)
		}
		if err := artifactstore.RestoreEncryptedControlSnapshot(ctx, args[1], args[3], key); err != nil {
			return err
		}
		absolute, err := filepath.Abs(args[3])
		if err != nil {
			return err
		}
		restored, err := storage.OpenExisting(absolute)
		if err != nil {
			return fmt.Errorf("open restored control data for read-back: %w", err)
		}
		checkErr := restored.IntegrityCheck(ctx)
		closeErr := restored.Close()
		if err := errors.Join(checkErr, closeErr); err != nil {
			return fmt.Errorf("read back restored control database: %w", err)
		}
		_, err = fmt.Printf("restored verified encrypted control snapshot to %s\n", absolute)
		return err
	default:
		return fmt.Errorf("unknown backup operation %q; usage: axiom backup <keygen|create|verify|restore|create-encrypted|verify-encrypted|restore-encrypted>", args[0])
	}
}

func printBackupManifest(manifest artifactstore.ControlSnapshotManifest) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(manifest)
}
