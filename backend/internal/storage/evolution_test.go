package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/storage"
)

func TestConversationGenerationBindingIsTransactionalAndPinned(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	userID := "usr_binding"
	if err := store.CreateUser(ctx, domain.User{ID: userID, Email: "binding@example.com", DisplayName: "Binding", CreatedAt: now}, "hash"); err != nil {
		t.Fatal(err)
	}
	provider := domain.Provider{ID: "prv_binding", UserID: userID, Name: "Binding", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	evolutionService := evolution.New(store)
	seed, err := evolutionService.EnsureSeed(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "run_pinned", UserID: userID, Title: "Pinned", ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, seed); err != nil {
		t.Fatal(err)
	}
	candidate, err := evolutionService.CreateCandidate(ctx, userID, evolution.CandidateInput{Name: "Next", Description: "next generation", ParentGenerationID: seed.ID, Spec: domain.AgentSpec{Strategy: evolution.StrategyPlanReact, MaxSteps: 12}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteAgentGeneration(ctx, userID, candidate.ID, []byte(`{"test":true}`)); err != nil {
		t.Fatal(err)
	}
	stable, err := evolutionService.Stable(ctx, userID)
	if err != nil || stable.ID != candidate.ID {
		t.Fatalf("new stable generation missing: %#v, %v", stable, err)
	}
	pinned, err := store.ConversationGeneration(ctx, userID, conversation.ID)
	if err != nil || pinned.ID != seed.ID || pinned.DefinitionDigest != seed.DefinitionDigest {
		t.Fatalf("existing conversation drifted after promotion: %#v, %v", pinned, err)
	}

	invalidConversation := domain.Conversation{ID: "run_invalid", UserID: userID, Title: "Invalid", ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	invalidGeneration := domain.AgentGeneration{ID: "missing", DefinitionDigest: "sha256:missing"}
	if err := store.CreateConversationWithGeneration(ctx, invalidConversation, invalidGeneration); err == nil {
		t.Fatal("invalid generation binding unexpectedly committed")
	}
	if _, err := store.Conversation(ctx, userID, invalidConversation.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("conversation insert was not rolled back: %v", err)
	}
}
