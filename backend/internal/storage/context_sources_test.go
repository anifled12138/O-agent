package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestContextSourceSurvivesRestartAndRemainsConversationScoped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	user := domain.User{ID: "context_source_user", Email: "context-source@axiom.local", DisplayName: "Context source", CreatedAt: now}
	if err := store.CreateUser(ctx, user, "test"); err != nil {
		t.Fatal(err)
	}
	provider := domain.Provider{ID: "context_source_provider", UserID: user.ID, Name: "Context source", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "test", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "context_source_conversation", UserID: user.ID, Title: "Context source", ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	message := domain.Message{ID: "context_source_message", ConversationID: conversation.ID, Role: "user", Content: "original request", CreatedAt: now}
	if err := store.AddMessage(ctx, user.ID, message); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(message.Content))
	messageRef := "message:" + message.ID + "#" + hex.EncodeToString(digest[:])
	gotMessage, _, err := store.ReadContextSource(ctx, user.ID, conversation.ID, messageRef)
	if err != nil || gotMessage != message.Content {
		t.Fatalf("versioned message reference did not read its content: %q, %v", gotMessage, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE messages SET content=? WHERE id=?`, "changed request", message.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadContextSource(ctx, user.ID, conversation.ID, messageRef); err == nil {
		t.Fatal("message source hash silently resolved to changed content")
	}
	archivedPlaintext := []byte("original bytes held under test encryption")
	archivedDigest := sha256.Sum256(archivedPlaintext)
	want := ContextSourceCiphertext{SourceID: "file_snapshot:versioned", SourceType: "file_snapshot", ContentHash: hex.EncodeToString(archivedDigest[:]), Ciphertext: []byte("encrypted-snapshot"), Nonce: []byte("nonce")}
	if err := store.SaveContextSource(ctx, user.ID, conversation.ID, want); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_context_source_insert
BEFORE INSERT ON context_sources WHEN NEW.source_id='file_snapshot:forced_failure'
BEGIN SELECT RAISE(ABORT, 'forced source persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	failing := want
	failing.SourceID = "file_snapshot:forced_failure"
	if err := store.SaveContextSource(ctx, user.ID, conversation.ID, failing); err == nil {
		t.Fatal("source save succeeded despite a forced database failure")
	}
	var failedRows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_sources WHERE conversation_id=? AND source_id=?`, conversation.ID, failing.SourceID).Scan(&failedRows); err != nil || failedRows != 0 {
		t.Fatalf("failed source save left durable partial state: count=%d err=%v", failedRows, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, got, err := store.ReadContextSource(ctx, user.ID, conversation.ID, want.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceID != want.SourceID || got.SourceType != want.SourceType || got.ContentHash != want.ContentHash || string(got.Ciphertext) != string(want.Ciphertext) || string(got.Nonce) != string(want.Nonce) {
		t.Fatalf("source did not survive restart intact: got=%+v", got)
	}
	if _, _, err := store.ReadContextSource(ctx, "another_user", conversation.ID, want.SourceID); err == nil {
		t.Fatal("source was readable by a different user")
	}
}
