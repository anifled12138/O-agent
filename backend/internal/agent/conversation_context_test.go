package agent

import (
	"context"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestImportConversationContextPreservesHistoryAndCreatesLocalMessageIDs(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.UpsertProvider(ctx, domain.Provider{ID: "provider_context_copy", UserID: owner, Name: "Context copy", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "test", CreatedAt: now, UpdatedAt: now}, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConversation(ctx, domain.Conversation{ID: "conversation_context_copy", UserID: owner, Title: "Local task", ProviderID: "provider_context_copy", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store}
	source := []domain.Message{
		{ID: "source_user", ConversationID: "conversation_cloud", Role: "user", Content: "first request"},
		{ID: "source_assistant", ConversationID: "conversation_cloud", Role: "assistant", Content: "first answer"},
	}
	if err := service.ImportConversationContext(ctx, owner, "conversation_context_copy", source); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.Conversation(ctx, owner, "conversation_context_copy")
	if err != nil {
		t.Fatal(err)
	}
	if len(readBack.Messages) != len(source) {
		t.Fatalf("imported message count = %d, want %d", len(readBack.Messages), len(source))
	}
	for i := range source {
		got := readBack.Messages[i]
		if got.ID == source[i].ID || got.ConversationID != "conversation_context_copy" || got.Role != source[i].Role || got.Content != source[i].Content {
			t.Fatalf("imported message %d did not retain history with local identity: got=%+v source=%+v", i, got, source[i])
		}
	}
	if err := service.ImportConversationContext(ctx, owner, "conversation_context_copy", source); err == nil {
		t.Fatal("context import accepted a non-empty target conversation")
	}
}
