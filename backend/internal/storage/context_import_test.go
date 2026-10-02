package storage

import (
	"context"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestImportConversationMessagesIsAtomicAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.UpsertProvider(ctx, domain.Provider{ID: "provider_context_import", UserID: owner, Name: "Context import", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "test", CreatedAt: now, UpdatedAt: now}, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"conversation_context_target", "conversation_context_collision"} {
		if err := store.CreateConversation(ctx, domain.Conversation{ID: id, UserID: owner, Title: id, ProviderID: "provider_context_import", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddMessage(ctx, owner, domain.Message{ID: "message_duplicate_context_id", ConversationID: "conversation_context_collision", Role: "user", Content: "existing", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	bad := []domain.Message{
		{ID: "message_context_first", ConversationID: "conversation_context_target", Role: "user", Content: "first", CreatedAt: now.Add(time.Nanosecond)},
		{ID: "message_duplicate_context_id", ConversationID: "conversation_context_target", Role: "assistant", Content: "collides", CreatedAt: now.Add(2 * time.Nanosecond)},
	}
	if err := store.ImportConversationMessages(ctx, owner, "conversation_context_target", bad); err == nil {
		t.Fatal("context import with a later message-ID conflict succeeded")
	}
	readBack, err := store.Conversation(ctx, owner, "conversation_context_target")
	if err != nil || len(readBack.Messages) != 0 {
		t.Fatalf("failed multi-message import left a partial conversation: messages=%+v err=%v", readBack.Messages, err)
	}
	want := []domain.Message{
		{ID: "message_context_user", ConversationID: "conversation_context_target", Role: "user", Content: "earlier request", CreatedAt: now.Add(time.Nanosecond)},
		{ID: "message_context_assistant", ConversationID: "conversation_context_target", Role: "assistant", Content: "earlier response", CreatedAt: now.Add(2 * time.Nanosecond)},
	}
	if err := store.ImportConversationMessages(ctx, owner, "conversation_context_target", want); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	readBack, err = restarted.Conversation(ctx, owner, "conversation_context_target")
	if err != nil || len(readBack.Messages) != len(want) {
		t.Fatalf("imported context did not survive restart: messages=%+v err=%v", readBack.Messages, err)
	}
	for i := range want {
		if readBack.Messages[i].ID != want[i].ID || readBack.Messages[i].Role != want[i].Role || readBack.Messages[i].Content != want[i].Content {
			t.Fatalf("imported message %d mismatch after restart: got=%+v want=%+v", i, readBack.Messages[i], want[i])
		}
	}
}
