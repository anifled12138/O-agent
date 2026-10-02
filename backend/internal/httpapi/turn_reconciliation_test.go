package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/storage"
)

func TestTurnReconciliationAPIRecordsAndReadsBackDistinctOutcomes(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	userID := "user_reconcile_api"
	if err := store.CreateUser(ctx, domain.User{ID: userID, Email: "reconcile@example.test", DisplayName: "Reconcile", CreatedAt: now}, "test-hash"); err != nil {
		t.Fatal(err)
	}
	model := domain.Provider{ID: "provider_reconcile_api", UserID: userID, Name: "fixture", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fixture", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, model, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "conversation_reconcile_api", UserID: userID, Title: "Reconcile", ProviderID: model.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	input := domain.Message{ID: "message_reconcile_api", ConversationID: conversation.ID, Role: "user", Content: "perform external operation", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_reconcile_api", ConversationID: conversation.ID, ProviderID: model.ID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.started", json.RawMessage(`{"toolCallId":"call_prior","toolName":"project.write","effect":"workspace_write"}`), now.Add(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.completed", json.RawMessage(`{"toolCallId":"call_prior","toolName":"project.write","effect":"workspace_write"}`), now.Add(750*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.started", json.RawMessage(`{"toolCallId":"call_external","toolName":"browser.navigate","effect":"external_write"}`), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if count, err := store.RecoverInterruptedAgentTurns(ctx); err != nil || count != 1 {
		t.Fatalf("recover interrupted turn with uncertain external effect: count=%d err=%v", count, err)
	}

	server := (&Server{workspaceID: userID, store: store}).Handler()
	requestOutcome := func(outcome, note string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"outcome": outcome, "note": note})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v2/agent/turns/"+turn.ID+"/reconcile", bytes.NewReader(body))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}

	unknown := requestOutcome("still_unknown", "external target remains unreachable")
	if unknown.Code != http.StatusOK {
		t.Fatalf("record unresolved review = %d: %s", unknown.Code, unknown.Body.String())
	}
	turnReadBack, err := store.AgentTurn(ctx, userID, turn.ID)
	if err != nil || turnReadBack.Status != "needs_reconciliation" || turnReadBack.RecoveryClass != "unknown_external_effect" {
		t.Fatalf("unresolved outcome did not keep execution locked: turn=%+v err=%v", turnReadBack, err)
	}
	unsafeRetry := requestOutcome("no_effect_applied", "the pending navigation did not happen")
	if unsafeRetry.Code == http.StatusOK {
		t.Fatal("reconciliation authorized replay even though an earlier external write had completed")
	}
	turnReadBack, err = store.AgentTurn(ctx, userID, turn.ID)
	if err != nil || turnReadBack.Status != "needs_reconciliation" || turnReadBack.RecoveryClass != "unknown_external_effect" {
		t.Fatalf("rejected unsafe replay changed durable state: turn=%+v err=%v", turnReadBack, err)
	}

	applied := requestOutcome("effect_applied", "verified the remote record exists; do not repeat")
	if applied.Code != http.StatusOK {
		t.Fatalf("record confirmed effect = %d: %s", applied.Code, applied.Body.String())
	}
	turnReadBack, err = store.AgentTurn(ctx, userID, turn.ID)
	if err != nil || turnReadBack.Status != "interrupted" || turnReadBack.RecoveryClass != "external_effect_confirmed" {
		t.Fatalf("confirmed effect state did not read back: turn=%+v err=%v", turnReadBack, err)
	}
	var response struct {
		Turn    domain.AgentTurn                 `json:"turn"`
		History []domain.AgentTurnReconciliation `json:"history"`
	}
	if err := json.Unmarshal(applied.Body.Bytes(), &response); err != nil || len(response.History) != 2 || response.History[0].Decision != "still_unknown" || response.History[1].Decision != "effect_applied" || response.Turn.ReconciliationNote != "verified the remote record exists; do not repeat" || len(response.Turn.RunState.Actions) != 1 || response.Turn.RunState.Actions[0].Effect != "workspace_write" || len(response.Turn.RunState.PendingActions) != 1 || response.Turn.RunState.PendingActions[0].Effect != "external_write" {
		t.Fatalf("API did not read back the audit trail and current outcome: response=%+v err=%v", response, err)
	}

	historyRequest := httptest.NewRequest(http.MethodGet, "/api/v2/agent/turns/"+turn.ID+"/reconciliations", nil)
	historyResponse := httptest.NewRecorder()
	server.ServeHTTP(historyResponse, historyRequest)
	if historyResponse.Code != http.StatusOK || historyResponse.Body.String() == "[]\n" {
		t.Fatalf("durable reconciliation history = %d: %s", historyResponse.Code, historyResponse.Body.String())
	}
}
