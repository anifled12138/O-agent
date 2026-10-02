package artifactstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

const controlSnapshotVersion = 1

// ControlSnapshotManifest describes the control database, vault key, and
// exactly the finalized content objects referenced by that database.
// Snapshots contain sensitive user data and must be stored in a private path.
type ControlSnapshotManifest struct {
	Version      int       `json:"version"`
	CreatedAt    time.Time `json:"createdAt"`
	DatabaseHash string    `json:"databaseSha256"`
	VaultKeyHash string    `json:"vaultKeySha256"`
	ObjectCount  int       `json:"objectCount"`
	ObjectBytes  int64     `json:"objectBytes"`
}

// CreateControlSnapshot builds a durable control-plane snapshot at a new
// directory. It includes SQLite, the encryption key required to read secrets,
// and finalized artifact objects. It intentionally does not claim to back up
// project worktrees, external Git remotes, or runtime configuration.
func CreateControlSnapshot(ctx context.Context, sourceDataDir, destination string, live *storage.Store) (manifest ControlSnapshotManifest, retErr error) {
	return createControlSnapshot(ctx, sourceDataDir, destination, live, false)
}

func createControlSnapshot(ctx context.Context, sourceDataDir, destination string, live *storage.Store, linkObjects bool) (manifest ControlSnapshotManifest, retErr error) {
	if live == nil || strings.TrimSpace(sourceDataDir) == "" || strings.TrimSpace(destination) == "" {
		return ControlSnapshotManifest{}, domain.ErrInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ControlSnapshotManifest{}, err
	}
	sourceRoot, err := filepath.Abs(sourceDataDir)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	destinationRoot, err := filepath.Abs(destination)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	if rootsOverlap(sourceRoot, destinationRoot) {
		return ControlSnapshotManifest{}, domain.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(destinationRoot), 0o700); err != nil {
		return ControlSnapshotManifest{}, err
	}
	if err := os.Mkdir(destinationRoot, 0o700); err != nil {
		return ControlSnapshotManifest{}, err
	}
	keep := false
	defer func() {
		if !keep {
			retErr = errors.Join(retErr, os.RemoveAll(destinationRoot), syncDirectory(filepath.Dir(destinationRoot)))
		}
	}()

	databasePath := filepath.Join(destinationRoot, "axiom.db")
	if err := live.BackupDatabase(ctx, databasePath); err != nil {
		return ControlSnapshotManifest{}, fmt.Errorf("snapshot control database: %w", err)
	}
	vaultSource := filepath.Join(sourceRoot, "master.key")
	vaultDestination := filepath.Join(destinationRoot, "master.key")
	if err := copyPrivateVaultKey(vaultSource, vaultDestination); err != nil {
		return ControlSnapshotManifest{}, fmt.Errorf("snapshot vault key: %w", err)
	}
	snapshotStore, err := storage.OpenExisting(destinationRoot)
	if err != nil {
		return ControlSnapshotManifest{}, fmt.Errorf("open snapshot database: %w", err)
	}
	activeUploads, err := snapshotStore.ActiveArtifactUploadCount(ctx)
	if err != nil {
		return ControlSnapshotManifest{}, errors.Join(err, snapshotStore.Close())
	}
	if activeUploads != 0 {
		return ControlSnapshotManifest{}, errors.Join(fmt.Errorf("snapshot contains %d incomplete artifact uploads; retry after uploads finish", activeUploads), snapshotStore.Close())
	}
	var objects ObjectBackupSummary
	var backupErr error
	if linkObjects {
		objects, backupErr = LinkReferencedObjects(ctx, sourceRoot, destinationRoot, snapshotStore)
		if backupErr != nil {
			linkErr := backupErr
			objects, backupErr = BackupReferencedObjects(ctx, sourceRoot, destinationRoot, snapshotStore)
			if backupErr != nil {
				backupErr = errors.Join(linkErr, backupErr)
			}
		}
	} else {
		objects, backupErr = BackupReferencedObjects(ctx, sourceRoot, destinationRoot, snapshotStore)
	}
	closeErr := snapshotStore.Close()
	if err := errors.Join(backupErr, closeErr); err != nil {
		return ControlSnapshotManifest{}, fmt.Errorf("snapshot referenced artifact objects: %w", err)
	}
	databaseHash, _, err := hashFile(databasePath)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	vaultHash, _, err := hashFile(vaultDestination)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	manifest = ControlSnapshotManifest{
		Version: controlSnapshotVersion, CreatedAt: time.Now().UTC(),
		DatabaseHash: databaseHash, VaultKeyHash: vaultHash,
		ObjectCount: objects.ObjectCount, ObjectBytes: objects.ObjectBytes,
	}
	if err := writeSnapshotManifest(destinationRoot, manifest); err != nil {
		return ControlSnapshotManifest{}, err
	}
	if _, err := VerifyControlSnapshot(ctx, destinationRoot); err != nil {
		return ControlSnapshotManifest{}, fmt.Errorf("verify completed control snapshot: %w", err)
	}
	if err := syncDirectory(destinationRoot); err != nil {
		return ControlSnapshotManifest{}, err
	}
	if err := syncDirectory(filepath.Dir(destinationRoot)); err != nil {
		return ControlSnapshotManifest{}, err
	}
	keep = true
	return manifest, nil
}

// VerifyControlSnapshot checks the manifest, SQLite integrity, vault key, and
// every finalized object referenced by the snapshot database.
func VerifyControlSnapshot(ctx context.Context, root string) (manifest ControlSnapshotManifest, retErr error) {
	if strings.TrimSpace(root) == "" {
		return ControlSnapshotManifest{}, domain.ErrInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	manifestPath := filepath.Join(root, "snapshot.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	if !manifestInfo.Mode().IsRegular() {
		return ControlSnapshotManifest{}, fmt.Errorf("snapshot manifest is not a regular file")
	}
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	info, statErr := manifestFile.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return ControlSnapshotManifest{}, errors.Join(fmt.Errorf("snapshot manifest is not a regular file"), statErr, manifestFile.Close())
	}
	decoder := json.NewDecoder(io.LimitReader(manifestFile, 64<<10))
	decodeErr := decoder.Decode(&manifest)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				decodeErr = fmt.Errorf("snapshot manifest contains trailing JSON data")
			} else {
				decodeErr = err
			}
		}
	}
	closeErr := manifestFile.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return ControlSnapshotManifest{}, err
	}
	if manifest.Version != controlSnapshotVersion || manifest.CreatedAt.IsZero() || !validDigest(manifest.DatabaseHash) || !validDigest(manifest.VaultKeyHash) || manifest.ObjectCount < 0 || manifest.ObjectBytes < 0 {
		return ControlSnapshotManifest{}, fmt.Errorf("invalid control snapshot manifest")
	}
	databaseHash, _, err := hashFile(filepath.Join(root, "axiom.db"))
	if err != nil || databaseHash != manifest.DatabaseHash {
		return ControlSnapshotManifest{}, errors.Join(fmt.Errorf("snapshot database hash mismatch"), err)
	}
	vaultHash, vaultSize, err := hashFile(filepath.Join(root, "master.key"))
	if err != nil || vaultSize != 32 || vaultHash != manifest.VaultKeyHash {
		return ControlSnapshotManifest{}, errors.Join(fmt.Errorf("snapshot vault key is missing, invalid, or mismatched"), err)
	}
	snapshotStore, err := storage.OpenExisting(root)
	if err != nil {
		return ControlSnapshotManifest{}, fmt.Errorf("open snapshot database: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, snapshotStore.Close()) }()
	if err := snapshotStore.IntegrityCheck(ctx); err != nil {
		return ControlSnapshotManifest{}, fmt.Errorf("snapshot database integrity check: %w", err)
	}
	activeUploads, err := snapshotStore.ActiveArtifactUploadCount(ctx)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	if activeUploads != 0 {
		return ControlSnapshotManifest{}, fmt.Errorf("snapshot contains %d incomplete artifact uploads", activeUploads)
	}
	artifacts, err := snapshotStore.ArtifactManifests(ctx)
	if err != nil {
		return ControlSnapshotManifest{}, err
	}
	seen := make(map[string]int64, len(artifacts))
	var count int
	var total int64
	for _, item := range artifacts {
		if err := ctx.Err(); err != nil {
			return ControlSnapshotManifest{}, err
		}
		if !validDigest(item.SHA256) || item.StorageKey != artifactStorageKey(item.SHA256) || item.ByteSize < 0 {
			return ControlSnapshotManifest{}, fmt.Errorf("invalid artifact manifest %q in snapshot", item.ID)
		}
		if priorSize, ok := seen[item.SHA256]; ok {
			if priorSize != item.ByteSize {
				return ControlSnapshotManifest{}, fmt.Errorf("conflicting sizes for snapshot object %s", item.SHA256)
			}
			continue
		}
		objectPath := filepath.Join(root, "artifacts", "objects", "sha256", item.SHA256[:2], item.SHA256)
		actualHash, actualSize, err := hashFile(objectPath)
		if err != nil || actualHash != item.SHA256 || actualSize != item.ByteSize {
			return ControlSnapshotManifest{}, errors.Join(fmt.Errorf("snapshot object %s failed verification", item.SHA256), err)
		}
		if total > int64(^uint64(0)>>1)-actualSize {
			return ControlSnapshotManifest{}, fmt.Errorf("snapshot object byte count overflow")
		}
		seen[item.SHA256] = item.ByteSize
		count++
		total += item.ByteSize
	}
	if count != manifest.ObjectCount || total != manifest.ObjectBytes {
		return ControlSnapshotManifest{}, fmt.Errorf("snapshot object inventory mismatch: manifest count=%d bytes=%d; actual count=%d bytes=%d", manifest.ObjectCount, manifest.ObjectBytes, count, total)
	}
	return manifest, nil
}

// RestoreControlSnapshot restores a verified snapshot into a new data
// directory. Existing destinations are never overwritten. The restore is
// staged beside the destination and made visible only after full read-back.
func RestoreControlSnapshot(ctx context.Context, snapshotRoot, destinationDataDir string) (retErr error) {
	if strings.TrimSpace(snapshotRoot) == "" || strings.TrimSpace(destinationDataDir) == "" {
		return domain.ErrInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	snapshotRoot, err := filepath.Abs(snapshotRoot)
	if err != nil {
		return err
	}
	destinationRoot, err := filepath.Abs(destinationDataDir)
	if err != nil {
		return err
	}
	if rootsOverlap(snapshotRoot, destinationRoot) {
		return domain.ErrInvalid
	}
	if _, err := VerifyControlSnapshot(ctx, snapshotRoot); err != nil {
		return fmt.Errorf("refuse to restore unverified snapshot: %w", err)
	}
	if _, err := os.Lstat(destinationRoot); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(destinationRoot)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	stagingRoot, err := os.MkdirTemp(parent, ".o-restore-*")
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(stagingRoot), syncDirectory(parent))
		}
	}()
	if err := copyPrivateFile(filepath.Join(snapshotRoot, "axiom.db"), filepath.Join(stagingRoot, "axiom.db")); err != nil {
		return err
	}
	if err := copyPrivateFile(filepath.Join(snapshotRoot, "master.key"), filepath.Join(stagingRoot, "master.key")); err != nil {
		return err
	}
	if err := copyPrivateFile(filepath.Join(snapshotRoot, "snapshot.json"), filepath.Join(stagingRoot, "snapshot.json")); err != nil {
		return err
	}
	snapshotStore, err := storage.OpenExisting(snapshotRoot)
	if err != nil {
		return err
	}
	objects, backupErr := BackupReferencedObjects(ctx, snapshotRoot, stagingRoot, snapshotStore)
	closeErr := snapshotStore.Close()
	if err := errors.Join(backupErr, closeErr); err != nil {
		return err
	}
	verified, err := VerifyControlSnapshot(ctx, stagingRoot)
	if err != nil {
		return fmt.Errorf("verify staged restore: %w", err)
	}
	if objects.ObjectCount != verified.ObjectCount || objects.ObjectBytes != verified.ObjectBytes {
		return fmt.Errorf("staged restore object copy did not match snapshot manifest")
	}
	if err := os.Remove(filepath.Join(stagingRoot, "snapshot.json")); err != nil {
		return err
	}
	if err := syncDirectory(stagingRoot); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(stagingRoot, destinationRoot); err != nil {
		return err
	}
	if err := syncDirectory(parent); err != nil {
		return err
	}
	return nil
}

func writeSnapshotManifest(root string, manifest ControlSnapshotManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(root, "snapshot.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return syncDirectory(root)
}

func copyPrivateVaultKey(source, destination string) error {
	linkInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !linkInfo.Mode().IsRegular() {
		return fmt.Errorf("vault key is not a regular file")
	}
	key, err := os.Open(source)
	if err != nil {
		return err
	}
	info, statErr := key.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		return errors.Join(fmt.Errorf("vault key is not a regular file"), statErr, key.Close())
	}
	if info.Size() != 32 {
		return errors.Join(fmt.Errorf("vault key must contain exactly 32 bytes"), key.Close())
	}
	return errors.Join(copyToPrivateFile(key, destination), key.Close())
}

func copyPrivateFile(source, destination string) error {
	linkInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !linkInfo.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		return errors.Join(fmt.Errorf("source is not a regular file"), statErr, file.Close())
	}
	if info.Size() == 0 {
		return errors.Join(fmt.Errorf("source file is empty"), file.Close())
	}
	return errors.Join(copyToPrivateFile(file, destination), file.Close())
}

func copyToPrivateFile(source io.Reader, destination string) (retErr error) {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, source)
	syncErr := file.Sync()
	closeErr := file.Close()
	if retErr = errors.Join(copyErr, syncErr, closeErr); retErr != nil {
		retErr = errors.Join(retErr, os.Remove(destination))
	}
	return retErr
}

func hashFile(path string) (string, int64, error) {
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !linkInfo.Mode().IsRegular() {
		return "", 0, fmt.Errorf("file is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		return "", 0, errors.Join(fmt.Errorf("file is not a regular file"), statErr, file.Close())
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
