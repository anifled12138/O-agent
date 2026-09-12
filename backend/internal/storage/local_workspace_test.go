package storage_test

import (
	"context"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestEnsureLocalWorkspaceOwnerCreatesStableOwner(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := store.EnsureLocalWorkspaceOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnsureLocalWorkspaceOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "local-workspace" || second != first {
		t.Fatalf("owner is not stable: first=%q second=%q", first, second)
	}
}

func TestEnsureLocalWorkspaceOwnerAdoptsExistingDataOwner(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	existing := domain.User{ID: "existing-owner", Email: "existing@example.com", DisplayName: "Existing", CreatedAt: time.Now().UTC()}
	if err := store.CreateUser(ctx, existing, "legacy-password-hash"); err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if owner != existing.ID {
		t.Fatalf("existing data owner was not adopted: got %q", owner)
	}
}
