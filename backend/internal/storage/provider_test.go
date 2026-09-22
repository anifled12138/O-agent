package storage_test

import (
	"context"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestProviderContextWindowRoundTrips(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	user := domain.User{ID: "usr_provider_context", Email: "provider-context@example.com", DisplayName: "Provider Context", CreatedAt: now}
	if err := store.CreateUser(ctx, user, "hash"); err != nil {
		t.Fatal(err)
	}
	p := domain.Provider{ID: "prv_context", UserID: user.ID, Name: "Context Provider", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "custom-model", ContextWindow: 262144, CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, p, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListProviders(ctx, user.ID)
	if err != nil || len(listed) != 1 || listed[0].ContextWindow != 262144 {
		t.Fatalf("provider list lost context window: %#v, %v", listed, err)
	}
	loaded, _, _, err := store.ProviderSecret(ctx, user.ID, p.ID)
	if err != nil || loaded.ContextWindow != 262144 {
		t.Fatalf("provider secret lost context window: %#v, %v", loaded, err)
	}
}
