package artifactstore

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestStoreDirectoryArchiveExcludesGitMetadataAndPreservesWorkspace(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("private git metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("do not follow"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err == nil {
		if _, err := service.StoreDirectoryArchive(ctx, owner, root, "task-workspace.tar.gz", "workspace-archive-unsafe", time.Now().UTC()); err == nil {
			t.Fatal("workspace archiver accepted a symlink that escapes its root")
		}
		if err := os.Remove(filepath.Join(root, "outside-link")); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Logf("symlink creation unavailable on this host: %v", err)
	}
	if err := os.Symlink(filepath.Join("src", "main.go"), filepath.Join(root, "source-link")); err != nil {
		t.Logf("in-workspace symlink creation unavailable on this host: %v", err)
	}
	artifact, err := service.StoreDirectoryArchive(ctx, owner, root, "task-workspace.tar.gz", "workspace-archive-test", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	readBack, file, err := service.Open(ctx, owner, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if readBack.ID != artifact.ID || readBack.SHA256 != artifact.SHA256 || readBack.ByteSize != artifact.ByteSize {
		t.Fatalf("workspace archive artifact did not verify on read-back: %+v %+v", artifact, readBack)
	}
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(compressed)
	entries := map[string]string{}
	links := map[string]string{}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = string(content)
		if header.Typeflag == tar.TypeSymlink {
			links[header.Name] = header.Linkname
		}
	}
	if entries["workspace/src/main.go"] != "package main\n" {
		t.Fatalf("workspace output did not appear in archive: %#v", entries)
	}
	if _, found := entries["workspace/.git/config"]; found {
		t.Fatal("Git metadata leaked into workspace output archive")
	}
	if target, found := links["workspace/source-link"]; found && target != "src/main.go" {
		t.Fatalf("archive changed the safe symbolic link target: %q", target)
	}
	if content, err := os.ReadFile(filepath.Join(root, "src", "main.go")); err != nil || string(content) != "package main\n" {
		t.Fatalf("archive modified or removed task workspace content: %q err=%v", content, err)
	}
}

func TestStoreDirectoryArchiveRejectsExternalButAllowsInternalHardlinks(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	inside := filepath.Join(root, "payload.bin")
	if err := os.WriteFile(inside, []byte("workspace content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(inside, filepath.Join(root, "payload-copy.bin")); err != nil {
		t.Skipf("hard links unavailable on this host: %v", err)
	}
	info, err := os.Stat(inside)
	if err != nil {
		t.Fatal(err)
	}
	_, _, supported := archiveFileLinkIdentity(info)
	if !supported {
		t.Skip("platform does not expose regular-file link identity")
	}
	if _, err := service.StoreDirectoryArchive(ctx, owner, root, "internal-links.tar.gz", "workspace-archive-internal-links", time.Now().UTC()); err != nil {
		t.Fatalf("archive rejected a complete in-workspace hard-link group: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "external-secret.bin")
	if err := os.WriteFile(outside, []byte("external content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(root, "external-link.bin")); err != nil {
		t.Skipf("cross-directory hard links unavailable on this host: %v", err)
	}
	if _, err := service.StoreDirectoryArchive(ctx, owner, root, "external-links.tar.gz", "workspace-archive-external-links", time.Now().UTC()); err == nil {
		t.Fatal("workspace archiver accepted a hard link whose other path is outside the workspace")
	}
}

func TestChunkUploadFinalizesAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("durable artifact payload")
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	now := time.Now().UTC()
	upload, created, err := service.Begin(ctx, owner, BeginRequest{FileName: "result.txt", MediaType: "text/plain", ExpectedSize: int64(len(content)), ExpectedSHA256: digestText, IdempotencyKey: "test-upload-1"}, now)
	if err != nil || !created {
		t.Fatalf("begin upload: created=%v upload=%+v err=%v", created, upload, err)
	}
	chunkHash := sha256.Sum256(content)
	chunk, err := service.Upload(ctx, owner, upload.ID, 0, hex.EncodeToString(chunkHash[:]), bytesReader(content), now)
	if err != nil || chunk.ByteSize != int64(len(content)) {
		t.Fatalf("upload chunk: chunk=%+v err=%v", chunk, err)
	}
	cutoff := now.Add(-48 * time.Hour)
	staleChunkTemp := filepath.Join(service.uploadDir(upload.ID), ".0-abandoned")
	freshChunkTemp := filepath.Join(service.uploadDir(upload.ID), ".0-active")
	if err := os.WriteFile(staleChunkTemp, []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleChunkTemp, cutoff, cutoff); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freshChunkTemp, []byte("possibly active"), 0o600); err != nil {
		t.Fatal(err)
	}
	orphanUploadDir := filepath.Join(service.root, "staging", "upl_0123456789abcdef0123456789abcdef")
	if err := os.MkdirAll(orphanUploadDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanUploadDir, "orphan.part"), []byte("not referenced by SQLite"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleObjectTemp := filepath.Join(service.root, "objects", ".tmp", "artifact-abandoned")
	if err := os.WriteFile(staleObjectTemp, []byte("unfinished finalize"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleObjectTemp, cutoff, cutoff); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err = New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staleChunkTemp); !os.IsNotExist(err) {
		t.Fatalf("abandoned temporary chunk survived startup recovery: %v", err)
	}
	if _, err := os.Stat(freshChunkTemp); err != nil {
		t.Fatalf("recent temporary chunk was removed without proving it stale: %v", err)
	}
	if _, err := os.Stat(orphanUploadDir); !os.IsNotExist(err) {
		t.Fatalf("staging without a durable upload record survived recovery: %v", err)
	}
	if _, err := os.Stat(staleObjectTemp); !os.IsNotExist(err) {
		t.Fatalf("abandoned object assembly file survived startup recovery: %v", err)
	}
	if _, err := os.Stat(service.chunkPath(upload.ID, 0)); err != nil {
		t.Fatalf("durable chunk for active upload was removed: %v", err)
	}
	resume, chunks, count, err := service.Status(ctx, owner, upload.ID, 0, 20)
	if err != nil || resume.Status != "uploading" || len(chunks) != 1 || count != 1 {
		t.Fatalf("upload state did not survive restart: upload=%+v chunks=%+v count=%d err=%v", resume, chunks, count, err)
	}
	artifact, err := service.Finalize(ctx, owner, upload.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := service.Capacity(ctx)
	if err != nil || capacity.Backend != "local" || capacity.RemoteObjectsAuthoritative || capacity.ObjectBytes != int64(len(content)) || capacity.StagingBytes != 0 || capacity.MetadataDatabaseBytes == 0 || !capacity.FilesystemMeasured || capacity.FilesystemFreeBytes > capacity.FilesystemTotalBytes || capacity.ObservedAt.IsZero() {
		t.Fatalf("artifact storage capacity did not reflect the committed object: capacity=%+v err=%v", capacity, err)
	}
	readBack, file, err := service.Open(ctx, owner, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read artifact: read=%v close=%v", err, closeErr)
	}
	if string(actual) != string(content) || readBack.SHA256 != digestText || readBack.ByteSize != int64(len(content)) {
		t.Fatalf("download differs from committed artifact: artifact=%+v actual=%q", readBack, actual)
	}
	if _, err := os.Stat(service.chunkPath(upload.ID, 0)); !os.IsNotExist(err) {
		t.Fatalf("completed upload staging data remains: err=%v", err)
	}
	if _, err := service.Finalize(ctx, owner, upload.ID, now.Add(2*time.Second)); err != nil {
		t.Fatalf("idempotent finalize failed: %v", err)
	}
	retried, err := service.StoreFromReader(ctx, owner, "result.txt", "text/plain", "test-upload-1", int64(len(content)), digestText, bytesReader(content), now.Add(3*time.Second))
	if err != nil || retried.ID != artifact.ID {
		t.Fatalf("completed resumable upload did not verify and read the retried body: artifact=%+v err=%v", retried, err)
	}
	completedStagingDir := service.uploadDir(upload.ID)
	if err := os.MkdirAll(completedStagingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(completedStagingDir, "leftover.part"), []byte("finalized staging residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dataDir, store); err != nil {
		t.Fatalf("startup recovery rejected a valid finalized artifact: %v", err)
	}
	if _, err := os.Stat(completedStagingDir); !os.IsNotExist(err) {
		t.Fatalf("verified finalized object did not release its redundant staging data: %v", err)
	}
}

func TestFinalizeRejectsMissingChunksAndWrongWholeObjectHash(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	content := []byte("actual bytes")
	wrong := sha256.Sum256([]byte("different bytes"))
	chunkDigest := sha256.Sum256(content)
	upload, _, err := service.Begin(ctx, owner, BeginRequest{FileName: "bad.txt", ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(wrong[:]), IdempotencyKey: "wrong-whole-hash"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Finalize(ctx, owner, upload.ID, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("finalize with a missing chunk = %v, want conflict", err)
	}
	if _, err := service.Upload(ctx, owner, upload.ID, 0, hex.EncodeToString(chunkDigest[:]), bytesReader(content), now); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Finalize(ctx, owner, upload.ID, now.Add(time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("finalize with the wrong whole-object hash = %v, want conflict", err)
	}
	state, err := store.ArtifactUpload(ctx, owner, upload.ID)
	if err != nil || state.Status != "uploading" || state.ArtifactID != "" {
		t.Fatalf("failed finalize published a partial artifact: upload=%+v err=%v", state, err)
	}
}

func TestTransferBatchLimitDoesNotCapArtifactSize(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	const projectArtifactSize = int64(500<<20) + 1
	upload, created, err := service.Begin(ctx, owner, BeginRequest{FileName: "large-project.tar", ExpectedSize: projectArtifactSize, IdempotencyKey: "large-upload"}, time.Now().UTC())
	if err != nil || !created {
		t.Fatalf("large upload was rejected: created=%v upload=%+v err=%v", created, upload, err)
	}
	if upload.ExpectedSize != projectArtifactSize || upload.ChunkCount != (projectArtifactSize+ChunkSize-1)/ChunkSize {
		t.Fatalf("transfer batching changed the durable total-size contract: %+v", upload)
	}
	if upload.ExpectedSize <= ChunkSize {
		t.Fatalf("test setup did not cross the transfer threshold: %+v", upload)
	}
}

func TestStartupRecoveryPreservesStagingWhenFinalizedObjectIsMissing(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("must remain recoverable")
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	now := time.Now().UTC()
	upload, _, err := service.Begin(ctx, owner, BeginRequest{FileName: "recover.txt", ExpectedSize: int64(len(content)), ExpectedSHA256: digestText, IdempotencyKey: "missing-object-preserve-staging"}, now)
	if err != nil {
		t.Fatal(err)
	}
	chunkDigest := sha256.Sum256(content)
	if _, err := service.Upload(ctx, owner, upload.ID, 0, hex.EncodeToString(chunkDigest[:]), bytesReader(content), now); err != nil {
		t.Fatal(err)
	}
	artifact, err := service.Finalize(ctx, owner, upload.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	stagingDir := service.uploadDir(upload.ID)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stagingEvidence := filepath.Join(stagingDir, "recoverable.part")
	if err := os.WriteFile(stagingEvidence, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(service.objectPath(artifact.SHA256)); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dataDir, store); err == nil {
		t.Fatal("startup accepted a finalized artifact whose object is missing")
	}
	if _, err := os.Stat(stagingEvidence); err != nil {
		t.Fatalf("failed recovery discarded finalized upload staging evidence: %v", err)
	}
}

type byteReader struct {
	data []byte
	done bool
}

func bytesReader(data []byte) *byteReader { return &byteReader{data: data} }
func (r *byteReader) Read(dst []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(dst, r.data), nil
}
