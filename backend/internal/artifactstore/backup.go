package artifactstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

type ObjectBackupSummary struct {
	ObjectCount int   `json:"objectCount"`
	ObjectBytes int64 `json:"objectBytes"`
}

// BackupReferencedObjects copies and verifies exactly the content-addressed
// objects referenced by the supplied database snapshot. The destination is a
// separate backup data directory; staging uploads and unreferenced objects are
// intentionally excluded because they are not finalized durable artifacts.
func BackupReferencedObjects(ctx context.Context, sourceDataDir, destinationDataDir string, snapshot *storage.Store) (summary ObjectBackupSummary, retErr error) {
	if snapshot == nil || strings.TrimSpace(sourceDataDir) == "" || strings.TrimSpace(destinationDataDir) == "" {
		return ObjectBackupSummary{}, domain.ErrInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sourceRoot, err := filepath.Abs(sourceDataDir)
	if err != nil {
		return ObjectBackupSummary{}, err
	}
	destinationRoot, err := filepath.Abs(destinationDataDir)
	if err != nil {
		return ObjectBackupSummary{}, err
	}
	if rootsOverlap(sourceRoot, destinationRoot) {
		return ObjectBackupSummary{}, domain.ErrInvalid
	}
	manifests, err := snapshot.ArtifactManifests(ctx)
	if err != nil {
		return ObjectBackupSummary{}, err
	}
	reader := &Service{store: snapshot, root: filepath.Join(sourceRoot, "artifacts")}
	destinationArtifactRoot := filepath.Join(destinationRoot, "artifacts")
	if err := os.Mkdir(destinationArtifactRoot, 0o700); err != nil {
		return ObjectBackupSummary{}, err
	}
	keepDestination := false
	defer func() {
		if !keepDestination {
			retErr = errors.Join(retErr, os.RemoveAll(destinationArtifactRoot), syncDirectory(destinationRoot))
		}
	}()
	destinationObjects := filepath.Join(destinationArtifactRoot, "objects", "sha256")
	if err := os.MkdirAll(destinationObjects, 0o700); err != nil {
		return ObjectBackupSummary{}, err
	}
	summary = ObjectBackupSummary{}
	seen := make(map[string]int64, len(manifests))
	for _, manifest := range manifests {
		if err := ctx.Err(); err != nil {
			return ObjectBackupSummary{}, err
		}
		if !validDigest(manifest.SHA256) || manifest.StorageKey != artifactStorageKey(manifest.SHA256) || manifest.ByteSize < 0 {
			return ObjectBackupSummary{}, fmt.Errorf("artifact %s has an invalid object manifest", manifest.ID)
		}
		if priorSize, ok := seen[manifest.SHA256]; ok {
			if priorSize != manifest.ByteSize {
				return ObjectBackupSummary{}, fmt.Errorf("content-addressed object %s has conflicting manifest sizes", manifest.SHA256)
			}
			continue
		}
		if summary.ObjectBytes > int64(^uint64(0)>>1)-manifest.ByteSize {
			return ObjectBackupSummary{}, fmt.Errorf("artifact backup byte count overflow")
		}
		objectDir := filepath.Join(destinationObjects, manifest.SHA256[:2])
		if err := os.MkdirAll(objectDir, 0o700); err != nil {
			return ObjectBackupSummary{}, err
		}
		destination := filepath.Join(objectDir, manifest.SHA256)
		if err := backupOneObject(ctx, reader, manifest, destination); err != nil {
			return ObjectBackupSummary{}, fmt.Errorf("backup artifact object %s: %w", manifest.SHA256, err)
		}
		seen[manifest.SHA256] = manifest.ByteSize
		summary.ObjectCount++
		summary.ObjectBytes += manifest.ByteSize
	}
	keepDestination = true
	return summary, nil
}

// LinkReferencedObjects creates a private snapshot view of immutable
// content-addressed objects without duplicating their bytes on the same
// filesystem. The encrypted snapshot writer hashes bytes again as it streams
// them, so a changed or replaced source object cannot produce a valid archive.
func LinkReferencedObjects(ctx context.Context, sourceDataDir, destinationDataDir string, snapshot *storage.Store) (summary ObjectBackupSummary, retErr error) {
	if snapshot == nil || strings.TrimSpace(sourceDataDir) == "" || strings.TrimSpace(destinationDataDir) == "" {
		return ObjectBackupSummary{}, domain.ErrInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sourceRoot, err := filepath.Abs(sourceDataDir)
	if err != nil {
		return ObjectBackupSummary{}, err
	}
	destinationRoot, err := filepath.Abs(destinationDataDir)
	if err != nil {
		return ObjectBackupSummary{}, err
	}
	if rootsOverlap(sourceRoot, destinationRoot) {
		return ObjectBackupSummary{}, domain.ErrInvalid
	}
	manifests, err := snapshot.ArtifactManifests(ctx)
	if err != nil {
		return ObjectBackupSummary{}, err
	}
	destinationArtifactRoot := filepath.Join(destinationRoot, "artifacts")
	if err := os.Mkdir(destinationArtifactRoot, 0o700); err != nil {
		return ObjectBackupSummary{}, err
	}
	keepDestination := false
	defer func() {
		if !keepDestination {
			retErr = errors.Join(retErr, os.RemoveAll(destinationArtifactRoot), syncDirectory(destinationRoot))
		}
	}()
	destinationObjects := filepath.Join(destinationArtifactRoot, "objects", "sha256")
	if err := os.MkdirAll(destinationObjects, 0o700); err != nil {
		return ObjectBackupSummary{}, err
	}
	seen := make(map[string]int64, len(manifests))
	directories := map[string]bool{destinationObjects: true, filepath.Dir(destinationObjects): true, destinationArtifactRoot: true}
	for _, manifest := range manifests {
		if err := ctx.Err(); err != nil {
			return ObjectBackupSummary{}, err
		}
		if !validDigest(manifest.SHA256) || manifest.StorageKey != artifactStorageKey(manifest.SHA256) || manifest.ByteSize < 0 {
			return ObjectBackupSummary{}, fmt.Errorf("artifact %s has an invalid object manifest", manifest.ID)
		}
		if priorSize, ok := seen[manifest.SHA256]; ok {
			if priorSize != manifest.ByteSize {
				return ObjectBackupSummary{}, fmt.Errorf("content-addressed object %s has conflicting manifest sizes", manifest.SHA256)
			}
			continue
		}
		if summary.ObjectBytes > int64(^uint64(0)>>1)-manifest.ByteSize {
			return ObjectBackupSummary{}, fmt.Errorf("artifact backup byte count overflow")
		}
		source := filepath.Join(sourceRoot, "artifacts", "objects", "sha256", manifest.SHA256[:2], manifest.SHA256)
		info, err := os.Lstat(source)
		if err != nil {
			return ObjectBackupSummary{}, err
		}
		if !info.Mode().IsRegular() {
			return ObjectBackupSummary{}, fmt.Errorf("artifact object %s is not a regular file", manifest.SHA256)
		}
		digest, size, err := hashFile(source)
		if err != nil || digest != manifest.SHA256 || size != manifest.ByteSize {
			return ObjectBackupSummary{}, errors.Join(fmt.Errorf("artifact object %s failed snapshot verification", manifest.SHA256), err)
		}
		objectDirectory := filepath.Join(destinationObjects, manifest.SHA256[:2])
		if err := os.MkdirAll(objectDirectory, 0o700); err != nil {
			return ObjectBackupSummary{}, err
		}
		directories[objectDirectory] = true
		if err := os.Link(source, filepath.Join(objectDirectory, manifest.SHA256)); err != nil {
			return ObjectBackupSummary{}, fmt.Errorf("create same-filesystem snapshot link for artifact %s: %w", manifest.SHA256, err)
		}
		seen[manifest.SHA256] = manifest.ByteSize
		summary.ObjectCount++
		summary.ObjectBytes += manifest.ByteSize
	}
	directoryList := make([]string, 0, len(directories))
	for directory := range directories {
		directoryList = append(directoryList, directory)
	}
	sort.Slice(directoryList, func(i, j int) bool { return len(directoryList[i]) > len(directoryList[j]) })
	for _, directory := range directoryList {
		if err := syncDirectory(directory); err != nil {
			return ObjectBackupSummary{}, err
		}
	}
	keepDestination = true
	return summary, nil
}

func artifactStorageKey(digest string) string { return "sha256/" + digest[:2] + "/" + digest }

func backupOneObject(ctx context.Context, source *Service, manifest storage.Artifact, destination string) (retErr error) {
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("existing artifact backup object is not a regular file")
		}
		existing, err := os.Open(destination)
		if err != nil {
			return err
		}
		digest, size, hashErr := hashReader(existing)
		closeErr := existing.Close()
		if hashErr != nil || closeErr != nil {
			return errors.Join(hashErr, closeErr)
		}
		if digest != manifest.SHA256 || size != manifest.ByteSize {
			return domain.ErrConflict
		}
		return syncDirectory(filepath.Dir(destination))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, input, err := source.Open(ctx, manifest.UserID, manifest.ID)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".o-backup-object-*")
	if err != nil {
		return errors.Join(err, input.Close())
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			retErr = errors.Join(retErr, os.Remove(temporary.Name()))
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(err, input.Close(), temporary.Close())
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), contextReader{ctx: ctx, reader: input})
	inputCloseErr := input.Close()
	if copyErr != nil || inputCloseErr != nil {
		return errors.Join(copyErr, inputCloseErr, temporary.Close())
	}
	if written != manifest.ByteSize || hex.EncodeToString(hasher.Sum(nil)) != manifest.SHA256 {
		return errors.Join(domain.ErrConflict, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return backupOneObject(ctx, source, manifest, destination)
		}
		return err
	}
	removeTemporary = false
	return syncDirectory(filepath.Dir(destination))
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func rootsOverlap(first, second string) bool {
	firstToSecond, err := filepath.Rel(first, second)
	if err != nil {
		return true
	}
	secondToFirst, err := filepath.Rel(second, first)
	if err != nil {
		return true
	}
	return isWithinRelative(firstToSecond) || isWithinRelative(secondToFirst)
}

func isWithinRelative(relative string) bool {
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}
