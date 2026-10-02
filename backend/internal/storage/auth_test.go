package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestInitialCredentialsAndSessionsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetInitialCredentials(ctx, owner, "owner@example.com", "Owner", "pbkdf2-sha256$test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetInitialCredentials(ctx, owner, "attacker@example.com", "Attacker", "pbkdf2-sha256$changed"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second setup = %v, want conflict", err)
	}
	user, hash, err := store.AuthUserByID(ctx, owner)
	if err != nil || user.Email != "owner@example.com" || hash != "pbkdf2-sha256$test" {
		t.Fatalf("failed setup changed account: user=%+v hash=%q err=%v", user, hash, err)
	}
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	if err := store.CreateAuthSession(ctx, "session-hash", owner, expires); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, hash, err = store.AuthUserByEmail(ctx, "owner@example.com")
	if err != nil || user.ID != owner || hash != "pbkdf2-sha256$test" {
		t.Fatalf("credentials did not survive restart: user=%+v hash=%q err=%v", user, hash, err)
	}
	if persistedUser, err := store.AuthSessionUser(ctx, "session-hash", time.Now().UTC()); err != nil || persistedUser != owner {
		t.Fatalf("session did not survive restart: user=%q err=%v", persistedUser, err)
	}
}
