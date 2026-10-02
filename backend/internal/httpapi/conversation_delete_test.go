package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestConversationDeleteAndRestorePersistsFor24Hours(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if store != nil {
			_ = store.Close()
		}
	})

	now := time.Now().UTC()
	if err := store.CreateUser(ctx, domain.User{ID: "ws_delete", Email: "delete@axiom.local", DisplayName: "Delete Test", CreatedAt: now}, "pass"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	provider := domain.Provider{ID: "prov_delete", UserID: "ws_delete", Name: "Delete Test Provider", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	conversation := domain.Conversation{ID: "convo_delete_test", UserID: "ws_delete", Title: "删除测试会话", ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := store.AddMessage(ctx, "ws_delete", domain.Message{ID: "msg_delete_test", ConversationID: conversation.ID, Role: "user", Content: "保留到找回期限", CreatedAt: now}); err != nil {
		t.Fatalf("create message: %v", err)
	}

	server := &Server{workspaceID: "ws_delete", store: store}
	request := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(method, path, nil))
		return response
	}

	if _, _, err := store.DeleteConversation(ctx, "someone_else", conversation.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete with a different workspace should be hidden as not found, got %v", err)
	}
	if _, err := store.Conversation(ctx, "ws_delete", conversation.ID); err != nil {
		t.Fatalf("wrong-workspace delete changed the conversation: %v", err)
	}

	deleted := request(http.MethodDelete, "/api/v1/conversations/"+conversation.ID)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete returned %d: %s", deleted.Code, deleted.Body.String())
	}
	var receipt domain.ConversationDeletion
	if err := json.Unmarshal(deleted.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("decode deletion receipt: %v", err)
	}
	if receipt.ConversationID != conversation.ID || receipt.RecoverUntil.Sub(receipt.DeletedAt) != domain.ConversationRecoveryRetention {
		t.Fatalf("unexpected deletion receipt: %+v", receipt)
	}

	list := request(http.MethodGet, "/api/v1/conversations")
	if list.Code != http.StatusOK {
		t.Fatalf("conversation list returned %d: %s", list.Code, list.Body.String())
	}
	var conversations []domain.Conversation
	if err := json.Unmarshal(list.Body.Bytes(), &conversations); err != nil {
		t.Fatalf("decode conversation list: %v", err)
	}
	if len(conversations) != 0 {
		t.Fatalf("deleted conversation remained visible in normal list: %+v", conversations)
	}
	if got := request(http.MethodGet, "/api/v1/conversations/"+conversation.ID); got.Code != http.StatusNotFound {
		t.Fatalf("deleted conversation detail returned %d, want 404: %s", got.Code, got.Body.String())
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close store before restart: %v", err)
	}
	store = nil
	store, err = storage.Open(filepath.Clean(dataDir))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	server = &Server{workspaceID: "ws_delete", store: store}
	if got := request(http.MethodGet, "/api/v1/conversations"); got.Code != http.StatusOK || got.Body.String() != "[]\n" {
		t.Fatalf("deleted conversation did not remain hidden after restart: status=%d body=%s", got.Code, got.Body.String())
	}
	recovery := request(http.MethodGet, "/api/v1/recovery/conversations")
	if recovery.Code != http.StatusOK {
		t.Fatalf("recovery list returned %d: %s", recovery.Code, recovery.Body.String())
	}
	var deletedItems []domain.DeletedConversation
	if err := json.Unmarshal(recovery.Body.Bytes(), &deletedItems); err != nil {
		t.Fatalf("decode recovery list: %v", err)
	}
	if len(deletedItems) != 1 || deletedItems[0].ID != conversation.ID || deletedItems[0].Title != conversation.Title {
		t.Fatalf("deleted conversation was not recoverable after restart: %+v", deletedItems)
	}

	restored := request(http.MethodPost, "/api/v1/recovery/conversations/"+conversation.ID+"/restore")
	if restored.Code != http.StatusOK {
		t.Fatalf("restore returned %d: %s", restored.Code, restored.Body.String())
	}
	var detail domain.ConversationDetail
	if err := json.Unmarshal(restored.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode restored conversation: %v", err)
	}
	if detail.ID != conversation.ID || detail.ExecutionPaused || len(detail.Messages) != 1 || detail.Messages[0].ID != "msg_delete_test" {
		t.Fatalf("restored conversation did not read back its retained state: %+v", detail)
	}
	visibleAgain := request(http.MethodGet, "/api/v1/conversations")
	if visibleAgain.Code != http.StatusOK {
		t.Fatalf("conversation list after restore returned %d: %s", visibleAgain.Code, visibleAgain.Body.String())
	}
	if err := json.Unmarshal(visibleAgain.Body.Bytes(), &conversations); err != nil || len(conversations) != 1 || conversations[0].ID != conversation.ID {
		t.Fatalf("restored conversation did not reappear in normal list: items=%+v err=%v", conversations, err)
	}
}
