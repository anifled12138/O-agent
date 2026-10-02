package artifactstore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

const ChunkSize int64 = 8 << 20
const DirectUploadBatchMaxBytes int64 = 500_000_000

type Service struct {
	store   *storage.Store
	root    string
	remote  *s3ObjectStore
	locksMu sync.Mutex
	locks   map[string]*chunkLock
}

type chunkLock struct {
	mu   sync.Mutex
	refs int
}

// ArtifactFile provides a seekable verified artifact body. Remote downloads
// are staged locally only for the lifetime of the returned reader.
type ArtifactFile struct {
	*os.File
	removeOnClose bool
}

type PresignedArtifactDownload struct {
	URL       string
	ExpiresAt time.Time
}

type PresignedArtifactUpload struct {
	URL             string
	RequiredHeaders map[string]string
	ExpiresAt       time.Time
}

func (file *ArtifactFile) Close() error {
	if file == nil || file.File == nil {
		return nil
	}
	closeErr := file.File.Close()
	if file.removeOnClose {
		return errors.Join(closeErr, os.Remove(file.Name()))
	}
	return closeErr
}

type BeginRequest struct {
	FileName       string
	MediaType      string
	ExpectedSize   int64
	ExpectedSHA256 string
	IdempotencyKey string
}

func New(dataDir string, store *storage.Store) (*Service, error) {
	return newService(dataDir, store, nil)
}

func NewWithS3(dataDir string, store *storage.Store, config S3Config) (*Service, error) {
	remote, err := newS3ObjectStore(config)
	if err != nil {
		return nil, fmt.Errorf("configure S3 artifact storage: %w", err)
	}
	return newService(dataDir, store, remote)
}

func newService(dataDir string, store *storage.Store, remote *s3ObjectStore) (*Service, error) {
	if store == nil || strings.TrimSpace(dataDir) == "" {
		return nil, domain.ErrInvalid
	}
	root := filepath.Join(dataDir, "artifacts")
	for _, dir := range []string{filepath.Join(root, "staging"), filepath.Join(root, "objects", "sha256"), filepath.Join(root, "objects", ".tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	service := &Service{store: store, root: root, remote: remote, locks: make(map[string]*chunkLock)}
	if err := service.recoverFilesystem(context.Background(), time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("recover artifact staging filesystem: %w", err)
	}
	return service, nil
}

const artifactTempRecoveryAge = 24 * time.Hour

// recoverFilesystem reconciles only staging paths that the database proves
// are unreferenced or already durably finalized. Active uploads keep their
// committed chunks; only old, uncommitted temporary chunk files are removable.
// Content-addressed objects are never collected here because they may still
// be referenced by task, conversation, or checkpoint records.
func (s *Service) recoverFilesystem(ctx context.Context, now time.Time) error {
	stagingRoot := filepath.Join(s.root, "staging")
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() || !validUploadDirectoryName(entry.Name()) {
			continue
		}
		directory := filepath.Join(stagingRoot, entry.Name())
		upload, err := s.store.ArtifactUploadByID(ctx, entry.Name())
		if errors.Is(err, domain.ErrNotFound) {
			if err := removeStagingDirectory(stagingRoot, directory); err != nil {
				return fmt.Errorf("remove orphan artifact staging directory %s: %w", entry.Name(), err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("read artifact upload %s: %w", entry.Name(), err)
		}
		switch upload.Status {
		case "uploading":
			if err := s.removeStaleChunkTemps(directory, now.Add(-artifactTempRecoveryAge)); err != nil {
				return fmt.Errorf("clean abandoned temporary chunks for %s: %w", upload.ID, err)
			}
		case "complete":
			artifact, err := s.store.Artifact(ctx, upload.UserID, upload.ArtifactID)
			if err != nil {
				return fmt.Errorf("read finalized artifact %s before staging cleanup: %w", upload.ID, err)
			}
			if err := s.verifyObject(ctx, artifact); err != nil {
				return fmt.Errorf("preserve staging for finalized upload %s whose object failed verification: %w", upload.ID, err)
			}
			if err := removeStagingDirectory(stagingRoot, directory); err != nil {
				return fmt.Errorf("remove finalized artifact staging directory %s: %w", upload.ID, err)
			}
		default:
			return fmt.Errorf("artifact upload %s has unsupported durable status %q", upload.ID, upload.Status)
		}
	}
	return s.removeStaleObjectTemps(filepath.Join(s.root, "objects", ".tmp"), now.Add(-artifactTempRecoveryAge))
}

func (s *Service) removeStaleChunkTemps(directory string, cutoff time.Time) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !isChunkTempName(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(directory)
	}
	return nil
}

func (s *Service) removeStaleObjectTemps(directory string, cutoff time.Time) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "artifact-") || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(directory)
	}
	return nil
}

func removeStagingDirectory(root, directory string) error {
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	resolvedDirectory, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedDirectory)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		if err != nil {
			return errors.Join(domain.ErrInvalid, err)
		}
		return domain.ErrInvalid
	}
	if err := os.RemoveAll(resolvedDirectory); err != nil {
		return err
	}
	return syncDirectory(resolvedRoot)
}

func validUploadDirectoryName(name string) bool {
	if !strings.HasPrefix(name, "upl_") || len(name) != len("upl_")+32 {
		return false
	}
	decoded, err := hex.DecodeString(name[len("upl_"):])
	return err == nil && len(decoded) == 16
}

func isChunkTempName(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	index, suffix, ok := strings.Cut(strings.TrimPrefix(name, "."), "-")
	if !ok || suffix == "" {
		return false
	}
	chunkIndex, err := strconv.ParseInt(index, 10, 64)
	return err == nil && chunkIndex >= 0
}

func (s *Service) Begin(ctx context.Context, userID string, input BeginRequest, now time.Time) (storage.ArtifactUpload, bool, error) {
	name, err := safeFileName(input.FileName)
	if err != nil {
		return storage.ArtifactUpload{}, false, err
	}
	mediaType := strings.TrimSpace(input.MediaType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	} else {
		parsed, _, err := mime.ParseMediaType(mediaType)
		if err != nil {
			return storage.ArtifactUpload{}, false, domain.ErrInvalid
		}
		mediaType = parsed
	}
	key := strings.TrimSpace(input.IdempotencyKey)
	if input.ExpectedSize < 0 || key == "" || len(key) > 128 {
		return storage.ArtifactUpload{}, false, domain.ErrInvalid
	}
	digest := strings.ToLower(strings.TrimSpace(input.ExpectedSHA256))
	if digest != "" && !validDigest(digest) {
		return storage.ArtifactUpload{}, false, domain.ErrInvalid
	}
	count := input.ExpectedSize / ChunkSize
	if input.ExpectedSize%ChunkSize != 0 {
		count++
	}
	rawID := make([]byte, 16)
	if _, err := rand.Read(rawID); err != nil {
		return storage.ArtifactUpload{}, false, err
	}
	uploadID := "upl_" + hex.EncodeToString(rawID)
	upload := storage.ArtifactUpload{ID: uploadID, UserID: userID, IdempotencyKey: key, FileName: name, MediaType: mediaType, ExpectedSize: input.ExpectedSize, ExpectedSHA256: digest, ChunkSize: ChunkSize, ChunkCount: count}
	saved, created, err := s.store.CreateArtifactUpload(ctx, upload, now)
	if err != nil {
		return storage.ArtifactUpload{}, false, err
	}
	if saved.Status == "uploading" {
		if err := os.MkdirAll(s.uploadDir(saved.ID), 0o700); err != nil {
			return saved, created, err
		}
	}
	return saved, created, nil
}

// CreateDirectUploadCapability selects the durable S3 direct-upload mode and
// returns a checksum-bound, short-lived PUT URL for one private staging key.
// The provider must be explicitly opted in after verifying its checksum and
// conditional-copy behavior; otherwise uploads keep using resumable proxy chunks.
func (s *Service) CreateDirectUploadCapability(ctx context.Context, userID, uploadID string, now time.Time) (storage.ArtifactUpload, PresignedArtifactUpload, bool, error) {
	if s == nil || s.store == nil {
		return storage.ArtifactUpload{}, PresignedArtifactUpload{}, false, nil
	}
	upload, err := s.store.ArtifactUpload(ctx, userID, uploadID)
	if err != nil {
		return storage.ArtifactUpload{}, PresignedArtifactUpload{}, false, err
	}
	if s.remote == nil || (!s.remote.directUpload && !upload.DirectUpload) {
		return upload, PresignedArtifactUpload{}, false, nil
	}
	if upload.ExpectedSize > DirectUploadBatchMaxBytes || upload.ExpectedSHA256 == "" || upload.Status != "uploading" {
		return upload, PresignedArtifactUpload{}, false, nil
	}
	if upload.DirectUpload {
		var count int64
		count, err = s.store.ArtifactUploadChunkCount(ctx, userID, uploadID)
		if err != nil {
			return storage.ArtifactUpload{}, PresignedArtifactUpload{}, false, err
		}
		if count != 0 {
			return storage.ArtifactUpload{}, PresignedArtifactUpload{}, false, domain.ErrConflict
		}
	}
	stageKey := s.directUploadStageKey(upload.ID)
	url, expiresAt, checksum, err := s.remote.presignedPutURL(stageKey, upload.ExpectedSHA256, 5*time.Minute)
	if err != nil {
		return storage.ArtifactUpload{}, PresignedArtifactUpload{}, false, err
	}
	if !upload.DirectUpload {
		upload, err = s.store.EnableArtifactDirectUpload(ctx, userID, uploadID, now)
		if err != nil {
			return storage.ArtifactUpload{}, PresignedArtifactUpload{}, false, err
		}
	}
	readBack, err := s.store.ArtifactUpload(ctx, userID, uploadID)
	if err != nil || !readBack.DirectUpload || readBack.ExpectedSHA256 != upload.ExpectedSHA256 || readBack.ExpectedSize != upload.ExpectedSize {
		return storage.ArtifactUpload{}, PresignedArtifactUpload{}, false, errors.Join(domain.ErrConflict, err)
	}
	return readBack, PresignedArtifactUpload{URL: url, RequiredHeaders: map[string]string{"x-amz-checksum-sha256": checksum}, ExpiresAt: expiresAt}, true, nil
}

func (s *Service) directUploadStageKey(uploadID string) string {
	return "staging/uploads/" + uploadID + "/payload"
}

// StoreFromReader durably stores an internally-produced object through the same
// resumable chunk path used by remote uploads. The caller supplies a stable
// idempotency key and the expected whole-object digest.
func (s *Service) StoreFromReader(ctx context.Context, userID, name, mediaType, key string, size int64, digest string, reader io.Reader, now time.Time) (storage.Artifact, error) {
	upload, _, err := s.Begin(ctx, userID, BeginRequest{FileName: name, MediaType: mediaType, ExpectedSize: size, ExpectedSHA256: digest, IdempotencyKey: key}, now)
	if err != nil {
		return storage.Artifact{}, err
	}
	if upload.Status == "complete" {
		artifact, err := s.Finalize(ctx, userID, upload.ID, now)
		if err != nil {
			return storage.Artifact{}, err
		}
		digest, actualSize, err := hashReader(reader)
		if err != nil {
			return storage.Artifact{}, err
		}
		if digest != artifact.SHA256 || actualSize != artifact.ByteSize || actualSize != size {
			return storage.Artifact{}, domain.ErrConflict
		}
		return artifact, nil
	}
	for index := int64(0); index < upload.ChunkCount; index++ {
		if err := ctx.Err(); err != nil {
			return storage.Artifact{}, err
		}
		want := upload.ChunkSize
		if index == upload.ChunkCount-1 {
			want = upload.ExpectedSize - index*upload.ChunkSize
		}
		chunk := make([]byte, want)
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return storage.Artifact{}, fmt.Errorf("read artifact chunk %d: %w", index, err)
		}
		if _, err := s.Upload(ctx, userID, upload.ID, index, "", bytes.NewReader(chunk), now); err != nil {
			return storage.Artifact{}, err
		}
	}
	var extra [1]byte
	if n, err := io.ReadFull(reader, extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return storage.Artifact{}, errors.Join(domain.ErrConflict, err)
	}
	return s.Finalize(ctx, userID, upload.ID, now)
}

// StoreDirectoryArchive creates a durable, resumably chunked gzip-tar artifact
// from a task workspace. It never follows symlinks and excludes Git metadata;
// the workspace itself remains intact for inspection or continuation.
func (s *Service) StoreDirectoryArchive(ctx context.Context, userID, root, fileName, key string, now time.Time) (storage.Artifact, error) {
	if s == nil || strings.TrimSpace(root) == "" || strings.TrimSpace(key) == "" {
		return storage.Artifact{}, domain.ErrInvalid
	}
	resolvedRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return storage.Artifact{}, err
	}
	resolvedRoot, err = filepath.EvalSymlinks(resolvedRoot)
	if err != nil {
		return storage.Artifact{}, err
	}
	rootInfo, err := os.Lstat(resolvedRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return storage.Artifact{}, errors.Join(fmt.Errorf("task output root is not a real directory: %w", domain.ErrConflict), err)
	}
	workspaceHardlinks, err := inspectWorkspaceHardlinks(ctx, resolvedRoot)
	if err != nil {
		return storage.Artifact{}, fmt.Errorf("inspect task workspace hard links: %w", err)
	}
	digest := sha256.New()
	count := &byteCounter{}
	if err := writeWorkspaceArchive(ctx, resolvedRoot, workspaceHardlinks, io.MultiWriter(digest, count)); err != nil {
		return storage.Artifact{}, fmt.Errorf("measure task workspace archive: %w", err)
	}
	if count.bytes <= 0 {
		return storage.Artifact{}, fmt.Errorf("task workspace archive is empty: %w", domain.ErrConflict)
	}
	pipeReader, pipeWriter := io.Pipe()
	archiveDone := make(chan error, 1)
	go func() {
		archiveErr := writeWorkspaceArchive(ctx, resolvedRoot, workspaceHardlinks, pipeWriter)
		_ = pipeWriter.CloseWithError(archiveErr)
		archiveDone <- archiveErr
	}()
	artifact, storeErr := s.StoreFromReader(ctx, userID, fileName, "application/gzip", key, count.bytes, hex.EncodeToString(digest.Sum(nil)), pipeReader, now)
	if storeErr != nil {
		_ = pipeReader.CloseWithError(storeErr)
	}
	archiveErr := <-archiveDone
	closeErr := pipeReader.Close()
	if err := errors.Join(storeErr, archiveErr, closeErr); err != nil {
		return storage.Artifact{}, fmt.Errorf("stream task workspace archive to durable artifact storage: %w", err)
	}
	return artifact, nil
}

type byteCounter struct{ bytes int64 }

func (counter *byteCounter) Write(payload []byte) (int, error) {
	counter.bytes += int64(len(payload))
	return len(payload), nil
}

func writeWorkspaceArchive(ctx context.Context, root string, workspaceHardlinks map[string]uint64, destination io.Writer) error {
	compressor := gzip.NewWriter(destination)
	compressor.Name = ""
	compressor.ModTime = time.Time{}
	archive := tar.NewWriter(compressor)
	writeErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.Join(fmt.Errorf("task output path escaped its workspace: %w", domain.ErrConflict), err)
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		for _, part := range parts {
			if part == ".git" {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
				return fmt.Errorf("task output symlink escapes its workspace: %q: %w", relative, domain.ErrConflict)
			}
			target = filepath.ToSlash(target)
			resolvedTarget := pathpkg.Clean(pathpkg.Join(pathpkg.Dir(filepath.ToSlash(relative)), target))
			if pathpkg.IsAbs(target) || resolvedTarget == ".." || strings.HasPrefix(resolvedTarget, "../") {
				return fmt.Errorf("task output symlink escapes its workspace: %q: %w", relative, domain.ErrConflict)
			}
			header, err := tar.FileInfoHeader(info, target)
			if err != nil {
				return err
			}
			header.Name = pathpkg.Join("workspace", filepath.ToSlash(relative))
			header.Mode &^= 0o7000
			header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""
			return archive.WriteHeader(header)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("task output contains unsupported special file %q: %w", relative, domain.ErrInvalid)
		}
		if info.Mode().IsRegular() {
			identity, links, supported := archiveFileLinkIdentity(info)
			if supported && links > 1 && workspaceHardlinks[identity] != links {
				return fmt.Errorf("task output file %q has hard links outside its workspace: %w", relative, domain.ErrConflict)
			}
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = pathpkg.Join("workspace", filepath.ToSlash(relative))
		header.Mode &^= 0o7000
		header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
			closeErr := file.Close()
			return errors.Join(fmt.Errorf("task output file changed while archiving: %w", domain.ErrConflict), statErr, closeErr)
		}
		written, copyErr := io.CopyN(archive, file, info.Size())
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || written != info.Size() {
			return errors.Join(fmt.Errorf("archive task output file %q: %w", relative, domain.ErrConflict), copyErr, closeErr)
		}
		return nil
	})
	return errors.Join(writeErr, archive.Close(), compressor.Close())
}

func (s *Service) Upload(ctx context.Context, userID, uploadID string, index int64, expectedDigest string, reader io.Reader, now time.Time) (storage.ArtifactChunk, error) {
	unlock := s.lockChunk(uploadID, index)
	defer unlock()
	upload, err := s.store.ArtifactUpload(ctx, userID, uploadID)
	if err != nil {
		return storage.ArtifactChunk{}, err
	}
	if upload.Status != "uploading" || upload.DirectUpload || index < 0 || index >= upload.ChunkCount {
		return storage.ArtifactChunk{}, domain.ErrConflict
	}
	wantSize := upload.ChunkSize
	if index == upload.ChunkCount-1 {
		wantSize = upload.ExpectedSize - index*upload.ChunkSize
	}
	tmp, err := os.CreateTemp(s.uploadDir(uploadID), fmt.Sprintf(".%d-*", index))
	if err != nil {
		return storage.ArtifactChunk{}, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(reader, wantSize+1))
	if copyErr != nil {
		closeErr := tmp.Close()
		return storage.ArtifactChunk{}, errors.Join(copyErr, closeErr)
	}
	if written != wantSize {
		closeErr := tmp.Close()
		return storage.ArtifactChunk{}, errors.Join(domain.ErrInvalid, closeErr)
	}
	if err := tmp.Sync(); err != nil {
		closeErr := tmp.Close()
		return storage.ArtifactChunk{}, errors.Join(err, closeErr)
	}
	if err := tmp.Close(); err != nil {
		return storage.ArtifactChunk{}, err
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	expectedDigest = strings.ToLower(strings.TrimSpace(expectedDigest))
	if expectedDigest != "" && (!validDigest(expectedDigest) || expectedDigest != digest) {
		return storage.ArtifactChunk{}, domain.ErrConflict
	}
	if previous, err := s.store.ArtifactChunk(ctx, userID, uploadID, index); err == nil {
		if previous.SHA256 != digest || previous.ByteSize != written {
			return storage.ArtifactChunk{}, domain.ErrConflict
		}
		if err := s.verifyChunk(uploadID, index, previous); err == nil {
			return previous, nil
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return storage.ArtifactChunk{}, err
	}
	partPath := s.chunkPath(uploadID, index)
	if err := os.Remove(partPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return storage.ArtifactChunk{}, err
	}
	if err := os.Rename(tmpName, partPath); err != nil {
		return storage.ArtifactChunk{}, err
	}
	if err := os.Chmod(partPath, 0o600); err != nil {
		return storage.ArtifactChunk{}, err
	}
	if err := syncDirectory(s.uploadDir(uploadID)); err != nil {
		return storage.ArtifactChunk{}, err
	}
	chunk, err := s.store.RecordArtifactChunk(ctx, userID, uploadID, index, digest, written, now)
	if err != nil {
		return storage.ArtifactChunk{}, err
	}
	if err := s.verifyChunk(uploadID, index, chunk); err != nil {
		return storage.ArtifactChunk{}, err
	}
	return chunk, nil
}

func (s *Service) Status(ctx context.Context, userID, uploadID string, offset, limit int64) (storage.ArtifactUpload, []storage.ArtifactChunk, int64, error) {
	upload, err := s.store.ArtifactUpload(ctx, userID, uploadID)
	if err != nil {
		return storage.ArtifactUpload{}, nil, 0, err
	}
	if offset < 0 || limit < 1 || limit > 1000 {
		return storage.ArtifactUpload{}, nil, 0, domain.ErrInvalid
	}
	chunks, err := s.store.ArtifactUploadChunksPage(ctx, userID, uploadID, offset, limit)
	if err != nil {
		return storage.ArtifactUpload{}, nil, 0, err
	}
	count, err := s.store.ArtifactUploadChunkCount(ctx, userID, uploadID)
	if err != nil {
		return storage.ArtifactUpload{}, nil, 0, err
	}
	return upload, chunks, count, nil
}

func (s *Service) Finalize(ctx context.Context, userID, uploadID string, now time.Time) (storage.Artifact, error) {
	unlock := s.lockChunk(uploadID, -1)
	defer unlock()
	upload, err := s.store.ArtifactUpload(ctx, userID, uploadID)
	if err != nil {
		return storage.Artifact{}, err
	}
	if upload.Status == "complete" {
		artifact, err := s.store.Artifact(ctx, userID, upload.ArtifactID)
		if err != nil {
			return storage.Artifact{}, err
		}
		if err := s.verifyObject(ctx, artifact); err != nil {
			return storage.Artifact{}, err
		}
		if upload.DirectUpload && s.remote != nil {
			if err := s.cleanupDirectUploadStage(ctx, upload.ID); err != nil {
				return storage.Artifact{}, err
			}
		}
		if err := os.RemoveAll(s.uploadDir(uploadID)); err != nil {
			return storage.Artifact{}, err
		}
		return artifact, nil
	}
	if upload.DirectUpload {
		return s.finalizeDirectUpload(ctx, upload, now)
	}
	chunkCount, err := s.store.ArtifactUploadChunkCount(ctx, userID, uploadID)
	if err != nil {
		return storage.Artifact{}, err
	}
	if chunkCount != upload.ChunkCount {
		return storage.Artifact{}, domain.ErrConflict
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "objects", ".tmp"), "artifact-*")
	if err != nil {
		return storage.Artifact{}, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	totalHash := sha256.New()
	var total int64
	for index := int64(0); index < upload.ChunkCount; index++ {
		meta, err := s.store.ArtifactChunk(ctx, userID, uploadID, index)
		if err != nil {
			closeErr := tmp.Close()
			return storage.Artifact{}, errors.Join(err, closeErr)
		}
		if err := s.verifyChunk(uploadID, index, meta); err != nil {
			closeErr := tmp.Close()
			return storage.Artifact{}, errors.Join(err, closeErr)
		}
		file, err := os.Open(s.chunkPath(uploadID, index))
		if err != nil {
			closeErr := tmp.Close()
			return storage.Artifact{}, errors.Join(err, closeErr)
		}
		written, copyErr := io.Copy(io.MultiWriter(tmp, totalHash), file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			fileErr := errors.Join(copyErr, closeErr)
			tmpErr := tmp.Close()
			return storage.Artifact{}, errors.Join(fileErr, tmpErr)
		}
		if written != meta.ByteSize {
			tmpErr := tmp.Close()
			return storage.Artifact{}, errors.Join(domain.ErrConflict, tmpErr)
		}
		total += written
	}
	if total != upload.ExpectedSize {
		closeErr := tmp.Close()
		return storage.Artifact{}, errors.Join(domain.ErrConflict, closeErr)
	}
	if err := tmp.Sync(); err != nil {
		closeErr := tmp.Close()
		return storage.Artifact{}, errors.Join(err, closeErr)
	}
	if err := tmp.Close(); err != nil {
		return storage.Artifact{}, err
	}
	digest := hex.EncodeToString(totalHash.Sum(nil))
	if upload.ExpectedSHA256 != "" && upload.ExpectedSHA256 != digest {
		return storage.Artifact{}, domain.ErrConflict
	}
	storageKey := "sha256/" + digest[:2] + "/" + digest
	if s.remote != nil {
		if err := s.remote.putObject(ctx, storageKey, tmpName, total); err != nil {
			return storage.Artifact{}, fmt.Errorf("publish artifact to S3-compatible storage: %w", err)
		}
		if err := s.verifyRemoteObject(ctx, storageKey, digest, total); err != nil {
			return storage.Artifact{}, fmt.Errorf("verify S3 artifact before finalizing upload: %w", err)
		}
	} else {
		destination := s.objectPath(digest)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return storage.Artifact{}, err
		}
		if existing, err := os.Open(destination); err == nil {
			actual, actualSize, verifyErr := hashReader(existing)
			closeErr := existing.Close()
			if verifyErr == nil && closeErr == nil && actual == digest && actualSize == total {
				if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
					return storage.Artifact{}, err
				}
			} else {
				if err := os.Remove(destination); err != nil {
					return storage.Artifact{}, errors.Join(verifyErr, closeErr, err)
				}
				if err := os.Rename(tmpName, destination); err != nil {
					return storage.Artifact{}, err
				}
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(tmpName, destination); err != nil {
				return storage.Artifact{}, err
			}
		} else {
			return storage.Artifact{}, err
		}
		if err := os.Chmod(destination, 0o600); err != nil {
			return storage.Artifact{}, err
		}
		if err := syncDirectory(filepath.Dir(destination)); err != nil {
			return storage.Artifact{}, err
		}
	}
	artifactID, err := newID("art_")
	if err != nil {
		return storage.Artifact{}, err
	}
	artifact, created, err := s.store.FinalizeArtifactUpload(ctx, userID, uploadID, artifactID, digest, storageKey, total, now)
	if err != nil {
		return storage.Artifact{}, err
	}
	if !created && artifact.ID == "" {
		return storage.Artifact{}, domain.ErrConflict
	}
	if artifact.SHA256 != digest || artifact.ByteSize != total || artifact.StorageKey != storageKey {
		return storage.Artifact{}, domain.ErrConflict
	}
	if err := s.verifyObject(ctx, artifact); err != nil {
		return storage.Artifact{}, err
	}
	if err := os.RemoveAll(s.uploadDir(uploadID)); err != nil {
		return storage.Artifact{}, err
	}
	return artifact, nil
}

func (s *Service) finalizeDirectUpload(ctx context.Context, upload storage.ArtifactUpload, now time.Time) (storage.Artifact, error) {
	if s.remote == nil || !upload.DirectUpload || upload.ExpectedSHA256 == "" || !validDigest(upload.ExpectedSHA256) {
		return storage.Artifact{}, domain.ErrConflict
	}
	stageKey := s.directUploadStageKey(upload.ID)
	size, etag, err := s.remote.headObject(ctx, stageKey)
	if err != nil {
		return storage.Artifact{}, fmt.Errorf("read direct-upload object before verification: %w", err)
	}
	if size != upload.ExpectedSize {
		return storage.Artifact{}, domain.ErrConflict
	}
	body, err := s.remote.openObject(ctx, stageKey)
	if err != nil {
		return storage.Artifact{}, fmt.Errorf("read direct-upload bytes for SHA-256 verification: %w", err)
	}
	digest, readSize, hashErr := hashReader(body)
	closeErr := body.Close()
	if hashErr != nil || closeErr != nil || digest != upload.ExpectedSHA256 || readSize != upload.ExpectedSize {
		return storage.Artifact{}, errors.Join(fmt.Errorf("direct-upload bytes do not match the declared full object: %w", domain.ErrConflict), hashErr, closeErr)
	}
	storageKey := "sha256/" + digest[:2] + "/" + digest
	if err := s.verifyRemoteObject(ctx, storageKey, digest, readSize); errors.Is(err, errArtifactRemoteNotFound) {
		if err := s.remote.copyObject(ctx, stageKey, storageKey, etag); err != nil {
			return storage.Artifact{}, fmt.Errorf("promote verified direct-upload object: %w", err)
		}
	} else if err != nil {
		return storage.Artifact{}, fmt.Errorf("existing content-addressed object failed verification: %w", err)
	}
	if err := s.verifyRemoteObject(ctx, storageKey, digest, readSize); err != nil {
		return storage.Artifact{}, fmt.Errorf("read back promoted direct-upload object: %w", err)
	}
	artifactID, err := newID("art_")
	if err != nil {
		return storage.Artifact{}, err
	}
	artifact, created, err := s.store.FinalizeArtifactUpload(ctx, upload.UserID, upload.ID, artifactID, digest, storageKey, readSize, now)
	if err != nil {
		return storage.Artifact{}, err
	}
	if !created && artifact.ID == "" || artifact.SHA256 != digest || artifact.ByteSize != readSize || artifact.StorageKey != storageKey {
		return storage.Artifact{}, domain.ErrConflict
	}
	if err := s.verifyObject(ctx, artifact); err != nil {
		return storage.Artifact{}, err
	}
	if err := s.cleanupDirectUploadStage(ctx, upload.ID); err != nil {
		return storage.Artifact{}, err
	}
	if err := os.RemoveAll(s.uploadDir(upload.ID)); err != nil {
		return storage.Artifact{}, err
	}
	return artifact, nil
}

func (s *Service) cleanupDirectUploadStage(ctx context.Context, uploadID string) error {
	if s.remote == nil {
		return nil
	}
	stageKey := s.directUploadStageKey(uploadID)
	if err := s.remote.deleteObject(ctx, stageKey); err != nil {
		return fmt.Errorf("remove direct-upload staging object: %w", err)
	}
	if _, _, err := s.remote.headObject(ctx, stageKey); err == nil {
		return fmt.Errorf("direct-upload staging object remains after removal: %w", domain.ErrConflict)
	} else if !errors.Is(err, errArtifactRemoteNotFound) {
		return fmt.Errorf("verify direct-upload staging cleanup: %w", err)
	}
	return nil
}

func (s *Service) Open(ctx context.Context, userID, artifactID string) (storage.Artifact, *ArtifactFile, error) {
	artifact, err := s.store.Artifact(ctx, userID, artifactID)
	if err != nil {
		return storage.Artifact{}, nil, err
	}
	if !validDigest(artifact.SHA256) {
		return storage.Artifact{}, nil, domain.ErrConflict
	}
	if s.remote != nil {
		if artifact.StorageKey != "sha256/"+artifact.SHA256[:2]+"/"+artifact.SHA256 {
			return storage.Artifact{}, nil, domain.ErrConflict
		}
		body, err := s.remote.openObject(ctx, artifact.StorageKey)
		if errors.Is(err, errArtifactRemoteNotFound) {
			if migrateErr := s.migrateLocalObjectToRemote(ctx, artifact); migrateErr != nil {
				return storage.Artifact{}, nil, fmt.Errorf("migrate legacy local artifact to S3-compatible storage: %w", migrateErr)
			}
			body, err = s.remote.openObject(ctx, artifact.StorageKey)
		}
		if err != nil {
			return storage.Artifact{}, nil, fmt.Errorf("download artifact from S3-compatible storage: %w", err)
		}
		file, err := os.CreateTemp(filepath.Join(s.root, "objects", ".tmp"), "artifact-read-*")
		if err != nil {
			return storage.Artifact{}, nil, errors.Join(err, body.Close())
		}
		hasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(file, hasher), body)
		bodyCloseErr := body.Close()
		if copyErr != nil || bodyCloseErr != nil || written != artifact.ByteSize || hex.EncodeToString(hasher.Sum(nil)) != artifact.SHA256 {
			fileCloseErr := file.Close()
			removeErr := os.Remove(file.Name())
			return storage.Artifact{}, nil, errors.Join(fmt.Errorf("downloaded S3 artifact did not match the committed object digest and size: %w", domain.ErrConflict), copyErr, bodyCloseErr, fileCloseErr, removeErr)
		}
		if err := file.Sync(); err != nil {
			closeErr := file.Close()
			removeErr := os.Remove(file.Name())
			return storage.Artifact{}, nil, errors.Join(err, closeErr, removeErr)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			closeErr := file.Close()
			removeErr := os.Remove(file.Name())
			return storage.Artifact{}, nil, errors.Join(err, closeErr, removeErr)
		}
		return artifact, &ArtifactFile{File: file, removeOnClose: true}, nil
	}
	file, err := os.Open(s.objectPath(artifact.SHA256))
	if err != nil {
		return storage.Artifact{}, nil, err
	}
	actual, size, verifyErr := hashReader(file)
	if verifyErr != nil {
		closeErr := file.Close()
		return storage.Artifact{}, nil, errors.Join(verifyErr, closeErr)
	}
	if actual != artifact.SHA256 || size != artifact.ByteSize {
		closeErr := file.Close()
		return storage.Artifact{}, nil, errors.Join(domain.ErrConflict, closeErr)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		closeErr := file.Close()
		return storage.Artifact{}, nil, errors.Join(err, closeErr)
	}
	return artifact, &ArtifactFile{File: file}, nil
}

// CreatePresignedDownloadURL grants short-lived, read-only access to an
// authoritative remote object. Callers must still validate the returned
// digest and byte count while consuming the object. Local stores keep using
// the authenticated application-server download route.
func (s *Service) CreatePresignedDownloadURL(ctx context.Context, userID, artifactID string, ttl time.Duration) (storage.Artifact, PresignedArtifactDownload, bool, error) {
	return s.CreatePresignedDownloadURLWithDisposition(ctx, userID, artifactID, "", ttl)
}

// CreatePresignedDownloadURLWithDisposition issues a short-lived, single-object
// read capability. The provider applies the attachment disposition only after
// SigV4 validation; the filename is escaped by the HTTP caller before it is
// passed here.
func (s *Service) CreatePresignedDownloadURLWithDisposition(ctx context.Context, userID, artifactID, disposition string, ttl time.Duration) (storage.Artifact, PresignedArtifactDownload, bool, error) {
	if s == nil || s.store == nil {
		return storage.Artifact{}, PresignedArtifactDownload{}, false, fmt.Errorf("artifact storage is unavailable")
	}
	artifact, err := s.store.Artifact(ctx, userID, artifactID)
	if err != nil {
		return storage.Artifact{}, PresignedArtifactDownload{}, false, err
	}
	if s.remote == nil {
		return artifact, PresignedArtifactDownload{}, false, nil
	}
	if err := s.verifyObject(ctx, artifact); err != nil {
		return storage.Artifact{}, PresignedArtifactDownload{}, false, fmt.Errorf("verify artifact before issuing a direct download capability: %w", err)
	}
	url, expiresAt, err := s.remote.presignedGetURL(artifact.StorageKey, ttl, disposition)
	if err != nil {
		return storage.Artifact{}, PresignedArtifactDownload{}, false, err
	}
	return artifact, PresignedArtifactDownload{URL: url, ExpiresAt: expiresAt}, true, nil
}

func (s *Service) uploadDir(id string) string { return filepath.Join(s.root, "staging", id) }
func (s *Service) chunkPath(uploadID string, index int64) string {
	return filepath.Join(s.uploadDir(uploadID), fmt.Sprintf("%d.part", index))
}
func (s *Service) objectPath(digest string) string {
	return filepath.Join(s.root, "objects", "sha256", digest[:2], digest)
}

func (s *Service) verifyChunk(uploadID string, index int64, meta storage.ArtifactChunk) error {
	file, err := os.Open(s.chunkPath(uploadID, index))
	if err != nil {
		return err
	}
	digest, size, hashErr := hashReader(file)
	closeErr := file.Close()
	if hashErr != nil || closeErr != nil {
		return errors.Join(hashErr, closeErr)
	}
	if digest != meta.SHA256 || size != meta.ByteSize {
		return domain.ErrConflict
	}
	return nil
}

func (s *Service) verifyObject(ctx context.Context, artifact storage.Artifact) error {
	if !validDigest(artifact.SHA256) {
		return domain.ErrConflict
	}
	if s.remote != nil {
		if artifact.StorageKey != "sha256/"+artifact.SHA256[:2]+"/"+artifact.SHA256 {
			return domain.ErrConflict
		}
		if err := s.remote.verifyObjectHead(ctx, artifact.StorageKey, artifact.ByteSize); errors.Is(err, errArtifactRemoteNotFound) {
			return s.migrateLocalObjectToRemote(ctx, artifact)
		} else {
			return err
		}
	}
	file, err := os.Open(s.objectPath(artifact.SHA256))
	if err != nil {
		return err
	}
	digest, size, hashErr := hashReader(file)
	closeErr := file.Close()
	if hashErr != nil || closeErr != nil {
		return errors.Join(hashErr, closeErr)
	}
	if digest != artifact.SHA256 || size != artifact.ByteSize {
		return domain.ErrConflict
	}
	return nil
}

// migrateLocalObjectToRemote supports switching an existing data directory
// from local storage to S3 without invalidating its committed artifact records.
// It only runs when the remote provider definitively reports a missing object;
// network errors and corrupted remote objects never fall back to local data.
func (s *Service) migrateLocalObjectToRemote(ctx context.Context, artifact storage.Artifact) error {
	if s.remote == nil || !validDigest(artifact.SHA256) || artifact.ByteSize < 0 || artifact.StorageKey != "sha256/"+artifact.SHA256[:2]+"/"+artifact.SHA256 {
		return domain.ErrConflict
	}
	file, err := os.Open(s.objectPath(artifact.SHA256))
	if err != nil {
		return fmt.Errorf("open legacy local artifact: %w", err)
	}
	digest, size, hashErr := hashReader(file)
	closeErr := file.Close()
	if hashErr != nil || closeErr != nil || digest != artifact.SHA256 || size != artifact.ByteSize {
		return errors.Join(fmt.Errorf("legacy local artifact failed digest and size verification: %w", domain.ErrConflict), hashErr, closeErr)
	}
	if err := s.remote.putObject(ctx, artifact.StorageKey, s.objectPath(artifact.SHA256), artifact.ByteSize); err != nil {
		return fmt.Errorf("upload legacy artifact: %w", err)
	}
	if err := s.verifyRemoteObject(ctx, artifact.StorageKey, artifact.SHA256, artifact.ByteSize); err != nil {
		return fmt.Errorf("verify migrated artifact: %w", err)
	}
	return nil
}

func (s *Service) verifyRemoteObject(ctx context.Context, storageKey, expectedDigest string, expectedSize int64) error {
	body, err := s.remote.openObject(ctx, storageKey)
	if err != nil {
		return err
	}
	digest, size, hashErr := hashReader(body)
	closeErr := body.Close()
	if hashErr != nil || closeErr != nil || digest != expectedDigest || size != expectedSize {
		return errors.Join(fmt.Errorf("remote artifact did not read back with its committed digest and size: %w", domain.ErrConflict), hashErr, closeErr)
	}
	return nil
}

func (s *Service) lockChunk(uploadID string, index int64) func() {
	key := fmt.Sprintf("%s:%d", uploadID, index)
	s.locksMu.Lock()
	lock := s.locks[key]
	if lock == nil {
		lock = &chunkLock{}
		s.locks[key] = lock
	}
	lock.refs++
	s.locksMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.locksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.locks, key)
		}
		s.locksMu.Unlock()
	}
}

func newID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}
func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}
func hashReader(reader io.Reader) (string, int64, error) {
	hasher := sha256.New()
	size, err := io.Copy(hasher, reader)
	if err != nil {
		return "", size, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}
func syncDirectory(directory string) error {
	// Go's standard os.File.Sync cannot flush directory handles on Windows.
	// File contents are still synced before every rename; Unix filesystems get
	// the additional directory-entry durability barrier here.
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(syncErr, closeErr)
}
func safeFileName(value string) (string, error) {
	value = strings.ReplaceAll(value, "\\", "/")
	value = pathpkg.Base(strings.TrimSpace(value))
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || len(value) > 255 {
		return "", domain.ErrInvalid
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", domain.ErrInvalid
		}
	}
	return value, nil
}
