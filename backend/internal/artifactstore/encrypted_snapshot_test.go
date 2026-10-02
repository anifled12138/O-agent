package artifactstore

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/storage"
)

func TestEncryptedControlSnapshotAuthenticatesAndRestoresAllDurableData(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	vaultKey := bytes.Repeat([]byte("v"), 32)
	if err := os.WriteFile(filepath.Join(source, "master.key"), vaultKey, 0o600); err != nil {
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
	secretArtifact := []byte("artifact plaintext must never be visible in the encrypted backup")
	digest := sha256.Sum256(secretArtifact)
	artifact, err := service.StoreFromReader(ctx, owner, "private-report.txt", "text/plain", "encrypted-backup", int64(len(secretArtifact)), hex.EncodeToString(digest[:]), bytesReader(secretArtifact), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeSetting(ctx, "encrypted-backup-readback", "persisted value"); err != nil {
		t.Fatal(err)
	}
	linkedSnapshot := filepath.Join(t.TempDir(), "linked-snapshot")
	if _, err := createControlSnapshot(ctx, source, linkedSnapshot, store, true); err != nil {
		t.Fatal(err)
	}
	sourceObject := filepath.Join(source, "artifacts", "objects", "sha256", artifact.SHA256[:2], artifact.SHA256)
	linkedObject := filepath.Join(linkedSnapshot, "artifacts", "objects", "sha256", artifact.SHA256[:2], artifact.SHA256)
	sourceInfo, err := os.Stat(sourceObject)
	if err != nil {
		t.Fatal(err)
	}
	linkedInfo, err := os.Stat(linkedObject)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceInfo, linkedInfo) {
		t.Fatal("same-filesystem encrypted snapshot staging duplicated finalized artifact bytes")
	}

	keyPath := filepath.Join(t.TempDir(), "backup.key")
	if err := GenerateControlSnapshotKey(keyPath); err != nil {
		t.Fatal(err)
	}
	key, err := LoadControlSnapshotKey(keyPath)
	if err != nil || len(key) != controlSnapshotKeyBytes {
		t.Fatalf("generated key did not read back: length=%d err=%v", len(key), err)
	}
	archive := filepath.Join(t.TempDir(), "control.oabk")
	manifest, err := CreateEncryptedControlSnapshot(ctx, source, archive, store, key)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ObjectCount != 1 || manifest.ObjectBytes != int64(len(secretArtifact)) {
		t.Fatalf("unexpected encrypted snapshot manifest: %+v", manifest)
	}
	ciphertext, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, secretArtifact) || bytes.Contains(ciphertext, vaultKey) {
		t.Fatal("encrypted snapshot exposed plaintext artifact or vault key bytes")
	}
	verified, err := VerifyEncryptedControlSnapshot(ctx, archive, key)
	if err != nil || verified != manifest {
		t.Fatalf("encrypted snapshot failed full read-back verification: got=%+v want=%+v err=%v", verified, manifest, err)
	}

	plainSnapshot := filepath.Join(t.TempDir(), "decrypted-snapshot")
	if err := DecryptControlSnapshot(ctx, archive, plainSnapshot, key); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyControlSnapshot(ctx, plainSnapshot); err != nil {
		t.Fatalf("decrypted snapshot is not a valid control snapshot: %v", err)
	}

	restoredPath := filepath.Join(t.TempDir(), "restored-data")
	if err := RestoreEncryptedControlSnapshot(ctx, archive, restoredPath, key); err != nil {
		t.Fatal(err)
	}
	restoredStore, err := storage.OpenExisting(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	value, err := restoredStore.RuntimeSetting(ctx, "encrypted-backup-readback")
	if err != nil || value != "persisted value" {
		t.Fatalf("restored database did not retain durable setting: value=%q err=%v", value, err)
	}
	restoredArtifacts, err := New(restoredPath, restoredStore)
	if err != nil {
		t.Fatal(err)
	}
	metadata, body, err := restoredArtifacts.Open(ctx, owner, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(contents, secretArtifact) || metadata.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("restored artifact failed read-back: metadata=%+v body=%q read=%v close=%v", metadata, contents, readErr, closeErr)
	}
}

func TestEncryptedControlSnapshotRejectsWrongKeysTruncationAndTampering(t *testing.T) {
	ctx := context.Background()
	source, store := newSnapshotTestSource(t, ctx)
	defer store.Close()
	snapshot := filepath.Join(t.TempDir(), "source-snapshot")
	if _, err := CreateControlSnapshot(ctx, source, snapshot, store); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x4d}, controlSnapshotKeyBytes)
	archive := filepath.Join(t.TempDir(), "control.oabk")
	if err := EncryptControlSnapshot(ctx, snapshot, archive, key); err != nil {
		t.Fatal(err)
	}
	wrongKey := bytes.Repeat([]byte{0x4e}, controlSnapshotKeyBytes)
	if _, err := VerifyEncryptedControlSnapshot(ctx, archive, wrongKey); err == nil {
		t.Fatal("encrypted snapshot verified with the wrong key")
	}
	before, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"truncate-final-tag": func(data []byte) []byte { return append([]byte(nil), data[:len(data)-1]...) },
		"tamper-final-tag": func(data []byte) []byte {
			copy := append([]byte(nil), data...)
			copy[len(copy)-1] ^= 0x80
			return copy
		},
	} {
		t.Run(name, func(t *testing.T) {
			badArchive := filepath.Join(t.TempDir(), "bad.oabk")
			if err := os.WriteFile(badArchive, mutate(before), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyEncryptedControlSnapshot(ctx, badArchive, key); err == nil {
				t.Fatal("tampered or truncated snapshot passed verification")
			}
			destination := filepath.Join(t.TempDir(), "restored")
			if err := RestoreEncryptedControlSnapshot(ctx, badArchive, destination, key); err == nil {
				t.Fatal("tampered or truncated snapshot restored successfully")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("failed restore published partial destination: %v", err)
			}
		})
	}
}

func TestControlSnapshotKeyGenerationIsExclusiveAndValidatesInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := GenerateControlSnapshotKey(path); err != nil {
		t.Fatal(err)
	}
	first, err := LoadControlSnapshotKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := GenerateControlSnapshotKey(path); err == nil {
		t.Fatal("key generation overwrote an existing backup key")
	}
	second, err := LoadControlSnapshotKey(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("existing backup key changed after rejected generation: err=%v", err)
	}
	shortKey := filepath.Join(t.TempDir(), "short.key")
	if err := os.WriteFile(shortKey, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadControlSnapshotKey(shortKey); err == nil {
		t.Fatal("short key file was accepted")
	}
}

func TestEncryptedSnapshotFramesStreamLargeContentAndAuthenticateOrder(t *testing.T) {
	key := bytes.Repeat([]byte{0x73}, controlSnapshotKeyBytes)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("frame-boundary-check-"), backupFrameBytes/20+17)
	var encrypted bytes.Buffer
	writer, err := newBackupFrameWriter(&encrypted, aead)
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(payload); {
		end := offset + 7777
		if end > len(payload) {
			end = len(payload)
		}
		if _, err := writer.Write(payload[offset:end]); err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	encoded := encrypted.Bytes()
	if string(encoded[:len(backupHeader)]) != backupHeader {
		t.Fatal("encrypted frame stream is missing its versioned header")
	}
	reader := &backupFrameReader{ctx: context.Background(), reader: bytes.NewReader(encoded[len(backupHeader):]), aead: aead}
	plain, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(plain, payload) || !reader.finalSeen {
		t.Fatalf("large framed stream did not authenticate and read back: length=%d expected=%d final=%t err=%v", len(plain), len(payload), reader.finalSeen, err)
	}
	if len(encoded) <= backupFrameBytes {
		t.Fatal("large input did not span multiple authenticated frames")
	}

	frameStart := len(backupHeader)
	firstLength := int(binary.BigEndian.Uint32(encoded[frameStart+1 : frameStart+5]))
	secondStart := frameStart + 1 + 4 + aead.NonceSize() + firstLength
	secondLength := int(binary.BigEndian.Uint32(encoded[secondStart+1 : secondStart+5]))
	framesOnly := append([]byte(nil), encoded[len(backupHeader):]...)
	firstFrame := append([]byte(nil), framesOnly[:secondStart-frameStart]...)
	secondFrame := append([]byte(nil), framesOnly[secondStart-frameStart:secondStart-frameStart+1+4+aead.NonceSize()+secondLength]...)
	ordered := append(secondFrame, firstFrame...)
	ordered = append(ordered, framesOnly[len(firstFrame)+len(secondFrame):]...)
	reorderedReader := &backupFrameReader{ctx: context.Background(), reader: bytes.NewReader(ordered), aead: aead}
	if _, err := io.ReadAll(reorderedReader); err == nil {
		t.Fatal("reordered authenticated frames were accepted")
	}
}

func TestEncryptedSnapshotRejectsArchivePathTraversal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	archivePath := filepath.Join(root, "malicious.oabk")
	key := bytes.Repeat([]byte{0x2a}, controlSnapshotKeyBytes)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, backupHeader); err != nil {
		file.Close()
		t.Fatal(err)
	}
	frames, err := newBackupFrameWriter(file, aead)
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	archive := tar.NewWriter(frames)
	if err := archive.WriteHeader(&tar.Header{Name: "../../outside.txt", Mode: 0o600, Size: 4, Typeflag: tar.TypeReg}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("evil")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := frames.Close(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "decrypted")
	if err := DecryptControlSnapshot(ctx, archivePath, destination, key); err == nil {
		t.Fatal("archive path traversal was accepted")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed path validation left a published destination: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "outside.txt")); !os.IsNotExist(err) {
		t.Fatalf("archive wrote outside destination: %v", err)
	}
}

func TestEncryptedSnapshotRejectsDestinationInsideSourceTreeWithoutMutation(t *testing.T) {
	ctx := context.Background()
	source, store := newSnapshotTestSource(t, ctx)
	defer store.Close()
	sourceDestination := filepath.Join(source, "backups", "snapshot.oabk")
	if _, err := CreateEncryptedControlSnapshot(ctx, source, sourceDestination, store, bytes.Repeat([]byte{0x3c}, controlSnapshotKeyBytes)); err == nil {
		t.Fatal("encrypted snapshot was created inside the live source data directory")
	}
	if _, err := os.Lstat(filepath.Dir(sourceDestination)); !os.IsNotExist(err) {
		t.Fatalf("rejected source-overlapping destination left a new directory: %v", err)
	}
	snapshot := filepath.Join(t.TempDir(), "source-snapshot")
	if _, err := CreateControlSnapshot(ctx, source, snapshot, store); err != nil {
		t.Fatal(err)
	}
	insideSnapshot := filepath.Join(snapshot, "encrypted.oabk")
	if err := EncryptControlSnapshot(ctx, snapshot, insideSnapshot, bytes.Repeat([]byte{0x3d}, controlSnapshotKeyBytes)); err == nil {
		t.Fatal("encrypted snapshot was created inside its plaintext source snapshot")
	}
	if _, err := os.Lstat(insideSnapshot); !os.IsNotExist(err) {
		t.Fatalf("rejected nested encrypted destination was created: %v", err)
	}
}

func newSnapshotTestSource(t *testing.T, ctx context.Context) (string, *storage.Store) {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "master.key"), bytes.Repeat([]byte{0x11}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureLocalWorkspaceOwner(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	return source, store
}
