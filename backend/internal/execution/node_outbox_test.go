package execution

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"crypto/sha256"
)

func TestNodeOutboxRecoveryCleansOnlyRecognizedIncompleteAtomicWrites(t *testing.T) {
	root := filepath.Join(t.TempDir(), "outbox")
	outbox, err := newNodeTaskOutbox(root)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("task interrupted while persisting"))
	orphan := filepath.Join(root, hex.EncodeToString(digest[:]))
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "project-delta.bundle"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "project-lfs-objects.tar"), []byte("partial LFS archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, ".record-temporary"), []byte("partial metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := outbox.List()
	if err != nil || len(records) != 0 {
		t.Fatalf("recognized incomplete atomic write prevented outbox recovery: records=%+v err=%v", records, err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("incomplete outbox files were not durably removed: %v", err)
	}
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "user-data"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.List(); err == nil {
		t.Fatal("outbox recovery silently removed an unrecognized file")
	}
	if content, err := os.ReadFile(filepath.Join(orphan, "user-data")); err != nil || string(content) != "keep" {
		t.Fatalf("unrecognized outbox content was changed: content=%q err=%v", content, err)
	}
}
