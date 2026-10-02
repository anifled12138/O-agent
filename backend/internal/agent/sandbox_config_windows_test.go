//go:build windows

package agent

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/sandbox"
	"axiom.local/agent/internal/storage"
)

func newSandboxConfigTestService(t *testing.T, dataDir string, backend sandbox.Backend) (*Service, *storage.Store) {
	t.Helper()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(dataDir, "windows-sandbox")
	service := &Service{
		store: store,
		sandboxExecution: coretools.ExecutionConfig{
			Backend: backend, InstallDir: installDir,
			RunnerPath: filepath.Join(installDir, "axiom-command-runner.exe"),
		},
	}
	return service, store
}

func TestSandboxDefaultBackendPersistsAcrossServiceRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	service, store := newSandboxConfigTestService(t, dataDir, sandbox.BackendAppContainer)
	configuration, err := service.SetSandboxDefaultBackend(ctx, string(sandbox.BackendAppContainer))
	if err != nil {
		t.Fatalf("persist AppContainer as the selected backend: %v", err)
	}
	if configuration.DefaultBackend != string(sandbox.BackendAppContainer) {
		t.Fatalf("backend mutation read back %q", configuration.DefaultBackend)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first service store: %v", err)
	}

	restarted, reopenedStore := newSandboxConfigTestService(t, dataDir, sandbox.BackendAppContainer)
	defer reopenedStore.Close()
	readBack, err := restarted.SandboxConfiguration(ctx)
	if err != nil {
		t.Fatalf("read sandbox configuration after restart: %v", err)
	}
	if readBack.DefaultBackend != string(sandbox.BackendAppContainer) {
		t.Fatalf("backend did not survive service restart: got %q", readBack.DefaultBackend)
	}
	if effective := restarted.executionConfigSnapshot().Backend; effective != sandbox.BackendAppContainer {
		t.Fatalf("restarted execution path selected %q", effective)
	}
}

func TestSandboxDefaultBackendMutationRollsBackOnStorageFailure(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	service, store := newSandboxConfigTestService(t, dataDir, sandbox.BackendWindowsNative)
	defer store.Close()
	if err := store.SetRuntimeSetting(ctx, "sandbox.default_backend", string(sandbox.BackendWindowsNative)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "axiom.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_appcontainer_backend BEFORE INSERT ON runtime_settings WHEN NEW.key='sandbox.default_backend' AND NEW.value='appcontainer' BEGIN SELECT RAISE(ABORT,'injected sandbox setting failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := service.SetSandboxDefaultBackend(ctx, string(sandbox.BackendAppContainer)); err == nil {
		t.Fatal("backend mutation reported success after the storage write failed")
	}
	value, err := store.RuntimeSetting(ctx, "sandbox.default_backend")
	if err != nil || value != string(sandbox.BackendWindowsNative) {
		t.Fatalf("durable backend was not restored: value=%q err=%v", value, err)
	}
	if effective := service.executionConfigSnapshot().Backend; effective != sandbox.BackendWindowsNative {
		t.Fatalf("failed mutation left runtime backend at %q", effective)
	}
	configuration, err := service.SandboxConfiguration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.DefaultBackend != string(sandbox.BackendWindowsNative) || configuration.Native.Health != "unhealthy" {
		t.Fatalf("rollback did not preserve the authoritative prior config and health: %+v", configuration)
	}
	if effective := service.executionConfigSnapshot().Backend; effective != sandbox.BackendWindowsNative {
		t.Fatalf("runtime setting read-back no longer selects the restored backend: %q", effective)
	}
}
