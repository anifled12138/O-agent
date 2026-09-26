package runfiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScopeArtifactCleanup(t *testing.T) {
	workspace := testTempDir(t, "runfiles-workspace-")
	root := testTempDir(t, "runfiles-root-")
	manager, err := NewManager(root, workspace)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := manager.NewScope()
	if err != nil {
		t.Fatal(err)
	}
	runDir := scope.Dir()
	artifact, err := scope.WriteArtifact("output", "txt", []byte("temporary output"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := scope.ReadArtifact(artifact.ID)
	if err != nil || string(data) != "temporary output" {
		t.Fatalf("read artifact = %q, %v", data, err)
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("run directory still exists, stat error = %v", err)
	}
	if _, err := scope.ReadArtifact(artifact.ID); err == nil {
		t.Fatal("expected artifact reads to fail after scope close")
	}
}

func TestNewManagerRemovesOnlyStaleOwnedRuns(t *testing.T) {
	root := testTempDir(t, "runfiles-root-")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "axiom-run-stale")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnershipMarker(filepath.Join(stale, markerName), 2147483647, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	unowned := filepath.Join(root, "axiom-run-unowned")
	if err := os.Mkdir(unowned, 0o700); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(root, "axiom-run-active")
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnershipMarker(filepath.Join(active, markerName), os.Getpid(), time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, directory := range []string{stale, unowned, active} {
		if err := os.Chtimes(directory, old, old); err != nil {
			t.Fatal(err)
		}
	}
	workspace := testTempDir(t, "runfiles-workspace-")
	if _, err := NewManager(root, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale owned run remains, stat error = %v", err)
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Fatalf("unowned directory was removed: %v", err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active run was removed: %v", err)
	}
}

func writeOwnershipMarker(path string, pid int, createdAt time.Time) error {
	value, err := json.Marshal(ownershipMarker{Version: markerVersion, PID: pid, CreatedAt: createdAt})
	if err != nil {
		return err
	}
	return os.WriteFile(path, value, 0o600)
}

func TestNewManagerRejectsWorkspaceTempDirectory(t *testing.T) {
	workspace := testTempDir(t, "runfiles-workspace-")
	root := filepath.Join(workspace, ".agent-temp")
	if _, err := NewManager(root, workspace); err == nil {
		t.Fatal("expected a workspace-contained temp directory to be rejected")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("rejected temp directory was created, stat error = %v", err)
	}
}

func testTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove test temp directory %q: %v", dir, err)
		}
	})
	return dir
}
