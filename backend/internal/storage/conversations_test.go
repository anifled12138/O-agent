package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestConversationDeleteRollsBackAndExpiredDeletePurgesChildren(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	if err := store.CreateUser(ctx, domain.User{ID: "ws_storage_delete", Email: "storage-delete@axiom.local", DisplayName: "Storage Delete", CreatedAt: now}, "pass"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	provider := domain.Provider{ID: "prov_storage_delete", UserID: "ws_storage_delete", Name: "Storage Delete", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	conversation := domain.Conversation{ID: "convo_storage_delete", UserID: "ws_storage_delete", Title: "Storage delete", ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := store.AddMessage(ctx, conversation.UserID, domain.Message{ID: "msg_storage_delete", ConversationID: conversation.ID, Role: "user", Content: "keep until expiry", CreatedAt: now}); err != nil {
		t.Fatalf("create message: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER force_conversation_delete_failure
BEFORE INSERT ON conversation_lifecycle_events
WHEN NEW.kind='conversation.deleted'
BEGIN SELECT RAISE(ABORT, 'forced delete failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if _, _, err := store.DeleteConversation(ctx, conversation.UserID, conversation.ID); err == nil {
		t.Fatal("delete should fail when durable lifecycle event cannot be written")
	}
	readBack, err := store.Conversation(ctx, conversation.UserID, conversation.ID)
	if err != nil {
		t.Fatalf("conversation disappeared after rolled-back delete: %v", err)
	}
	if readBack.ExecutionPaused || len(readBack.Messages) != 1 {
		t.Fatalf("failed delete left partial state: %+v", readBack)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER force_conversation_delete_failure`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}

	if _, _, err := store.DeleteConversation(ctx, conversation.UserID, conversation.ID); err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE conversations SET recover_until=? WHERE id=?`, now.Add(-48*time.Hour), conversation.ID); err != nil {
		t.Fatalf("expire deleted conversation: %v", err)
	}
	deleted, err := store.PurgeExpiredConversations(ctx)
	if err != nil {
		t.Fatalf("purge expired conversation: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("purge removed %d conversations, want 1", deleted)
	}
	if _, err := store.RestoreConversation(ctx, conversation.UserID, conversation.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired conversation restore error = %v, want not found", err)
	}
	for _, table := range []string{"conversations", "messages", "conversation_lifecycle_events"} {
		var count int
		query := `SELECT COUNT(*) FROM ` + table + ` WHERE `
		var err error
		if table == "conversations" {
			err = store.db.QueryRowContext(ctx, query+`id=?`, conversation.ID).Scan(&count)
		} else {
			err = store.db.QueryRowContext(ctx, query+`conversation_id=?`, conversation.ID).Scan(&count)
		}
		if err != nil && err != sql.ErrNoRows {
			t.Fatalf("count purged rows in %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("expired conversation purge left %d related rows in %s", count, table)
		}
	}

	pausedConversation := domain.Conversation{ID: "convo_storage_paused", UserID: conversation.UserID, Title: "Paused before delete", ProviderID: provider.ID, ExecutionPaused: true, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversation(ctx, pausedConversation); err != nil {
		t.Fatalf("create paused conversation: %v", err)
	}
	if _, _, err := store.DeleteConversation(ctx, pausedConversation.UserID, pausedConversation.ID); err != nil {
		t.Fatalf("delete previously paused conversation: %v", err)
	}
	restoredPaused, err := store.RestoreConversation(ctx, pausedConversation.UserID, pausedConversation.ID)
	if err != nil {
		t.Fatalf("restore previously paused conversation: %v", err)
	}
	if !restoredPaused.ExecutionPaused {
		t.Fatalf("restore lost the conversation's original paused state: %+v", restoredPaused.Conversation)
	}
}
