package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/storage"
)

func TestRunJournalCommitsInputEventsAndOutputAtomically(t *testing.T) {
	ctx := context.Background()
	store, userID, conversation, generation := runJournalFixture(t, "atomic")
	defer store.Close()
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_input", ConversationID: conversation.ID, Role: "user", Content: "work", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_atomic", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{"generationId":"gen"}`)); err != nil {
		t.Fatal(err)
	}

	duplicateInput := domain.Message{ID: "msg_duplicate", ConversationID: conversation.ID, Role: "user", Content: "must roll back", CreatedAt: now.Add(time.Second)}
	duplicateTurn := turn
	duplicateTurn.ID = "turn_duplicate"
	if err := store.StartAgentTurn(ctx, userID, duplicateTurn, duplicateInput, json.RawMessage(`{}`)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second active turn error = %v", err)
	}
	detail, err := store.Conversation(ctx, userID, conversation.ID)
	if err != nil || len(detail.Messages) != 1 || detail.Messages[0].ID != input.ID {
		t.Fatalf("duplicate input was not rolled back: %#v, %v", detail.Messages, err)
	}

	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "model.requested", json.RawMessage(`{"step":1}`), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "model.completed", json.RawMessage(`{"step":1,"model":"fake","usage":{"totalTokens":7}}`), now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "msg_output", ConversationID: conversation.ID, Role: "assistant", Content: "done", CreatedAt: now.Add(4 * time.Second)}
	if err := store.FinishAgentTurn(ctx, userID, turn.ID, "completed", "assistant_response", &output, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}

	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].Status != "completed" || turns[0].LastSequence != 4 || turns[0].ResultMessageID != output.ID {
		t.Fatalf("turn projection = %#v, %v", turns, err)
	}
	events, err := store.TraceEvents(ctx, userID, conversation.ID)
	if err != nil || len(events) != 4 || events[0].Kind != "turn.started" || events[3].Kind != "turn.completed" {
		t.Fatalf("events = %#v, %v", events, err)
	}
	resumed, err := store.TurnEvents(ctx, userID, turn.ID, 2)
	if err != nil || len(resumed) != 2 || resumed[0].Sequence != 3 || resumed[1].Sequence != 4 {
		t.Fatalf("resumed events = %#v, %v", resumed, err)
	}
	detail, err = store.Conversation(ctx, userID, conversation.ID)
	if err != nil || len(detail.Messages) != 2 || detail.Messages[1].ID != output.ID {
		t.Fatalf("output was not committed with the turn: %#v, %v", detail.Messages, err)
	}
}

func TestRunJournalPersistsStepLimitAsIncomplete(t *testing.T) {
	ctx := context.Background()
	store, userID, conversation, generation := runJournalFixture(t, "incomplete")
	defer store.Close()
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_incomplete_input", ConversationID: conversation.ID, Role: "user", Content: "long task", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_incomplete", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "msg_incomplete_output", ConversationID: conversation.ID, Role: "assistant", Content: "partial summary", CreatedAt: now.Add(time.Second)}
	if err := store.FinishAgentTurn(ctx, userID, turn.ID, "incomplete", "step_limit", &output, json.RawMessage(`{"metrics":{"reachedStepLimit":true}}`)); err != nil {
		t.Fatal(err)
	}
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].Status != "incomplete" || turns[0].StopReason != "step_limit" {
		t.Fatalf("incomplete turn projection = %#v, %v", turns, err)
	}
	events, err := store.TurnEvents(ctx, userID, turn.ID, 0)
	if err != nil || events[len(events)-1].Kind != "turn.incomplete" {
		t.Fatalf("incomplete terminal event = %#v, %v", events, err)
	}
}

func TestRunJournalRecoveryDoesNotReplayUnknownToolEffects(t *testing.T) {
	ctx := context.Background()
	store, userID, conversation, generation := runJournalFixture(t, "recovery")
	defer store.Close()
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_recovery", ConversationID: conversation.ID, Role: "user", Content: "work", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_recovery", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "model.requested", json.RawMessage(`{"step":1}`), now); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.started", json.RawMessage(`{"step":1,"toolCallId":"call_1"}`), now); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RecoverInterruptedAgentTurns(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered = %d, %v", recovered, err)
	}
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || turns[0].Status != "needs_reconciliation" || turns[0].RecoveryClass != "unknown_external_effect" {
		t.Fatalf("unsafe recovery projection = %#v, %v", turns, err)
	}
}

func TestRunJournalCancellationIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, userID, conversation, generation := runJournalFixture(t, "cancel")
	defer store.Close()
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_cancel", ConversationID: conversation.ID, Role: "user", Content: "work", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_cancel", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestAgentTurnCancel(ctx, userID, turn.ID, "user_requested"); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestAgentTurnCancel(ctx, userID, turn.ID, "duplicate"); err != nil {
		t.Fatalf("duplicate cancellation must be idempotent: %v", err)
	}
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || turns[0].Status != "cancelling" || !turns[0].CancelRequested || turns[0].LastSequence != 2 {
		t.Fatalf("cancellation projection = %#v, %v", turns, err)
	}
	if err := store.FinishAgentTurn(ctx, userID, turn.ID, "cancelled", "cancelled", nil, json.RawMessage(`{"error":"context canceled"}`)); err != nil {
		t.Fatal(err)
	}
	turns, err = store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || turns[0].Status != "cancelled" || turns[0].LastSequence != 3 {
		t.Fatalf("cancelled projection = %#v, %v", turns, err)
	}
}

func runJournalFixture(t *testing.T, suffix string) (*storage.Store, string, domain.Conversation, domain.AgentGeneration) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	userID := "usr_" + suffix
	if err := store.CreateUser(ctx, domain.User{ID: userID, Email: suffix + "@example.com", DisplayName: suffix, CreatedAt: now}, "hash"); err != nil {
		t.Fatal(err)
	}
	provider := domain.Provider{ID: "prv_" + suffix, UserID: userID, Name: suffix, Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "run_" + suffix, UserID: userID, Title: suffix, ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	return store, userID, conversation, generation
}
