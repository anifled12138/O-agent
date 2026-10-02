package storage_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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
	startDetails, _ := json.Marshal(map[string]string{"generationId": "gen", "inputSourceRef": runJournalMessageSourceID(input.ID, input.Content)})
	if err := store.StartAgentTurn(ctx, userID, turn, input, startDetails); err != nil {
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
	output := domain.Message{ID: "msg_output", ConversationID: conversation.ID, Role: "assistant", Content: "All tests passed.", CreatedAt: now.Add(4 * time.Second)}
	if err := store.FinishAgentTurn(ctx, userID, turn.ID, "completed", "assistant_response", &output, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}

	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].Status != "completed" || turns[0].LastSequence != 4 || turns[0].ResultMessageID != output.ID {
		t.Fatalf("turn projection = %#v, %v", turns, err)
	}
	if turns[0].CompletionAssessment.Status != "unverified" || !strings.HasPrefix(turns[0].CompletionAssessment.AssistantSourceRef, "message:"+output.ID+"#") || len(turns[0].CompletionAssessment.EvidenceRefs) != 0 {
		t.Fatalf("assistant final text was treated as verified completion: %#v", turns[0].CompletionAssessment)
	}
	if turns[0].RunState.CurrentRequest == nil || turns[0].RunState.CurrentRequest.Statement != input.Content || turns[0].RunState.CurrentRequest.Verification != "source_available" || turns[0].RunState.AssistantResponse == nil || turns[0].RunState.AssistantResponse.Verification != "unverified" {
		t.Fatalf("run state did not reconstruct source-linked input and unverified assistant report: %#v", turns[0].RunState)
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

func TestContinuationIntentDecisionIsDurableAndKeepsCheckpointAvailable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "continuation_intent")
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_intent_input", ConversationID: conversation.ID, Role: "user", Content: "finish the implementation", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_intent", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "msg_intent_output", ConversationID: conversation.ID, Role: "assistant", Content: "I stopped at the step limit.", CreatedAt: now.Add(time.Second)}
	continuation := &storage.ContinuationState{Version: 1, Ciphertext: []byte("encrypted-checkpoint"), Nonce: []byte("nonce"), ContentHash: "checkpoint-hash"}
	if err := store.FinishAgentTurnWithContinuation(ctx, userID, turn.ID, "incomplete", "step_limit", &output, json.RawMessage(`{}`), continuation); err != nil {
		t.Fatal(err)
	}
	details := json.RawMessage(`{"decision":"resume","inputSha256":"abc","planHash":"plan","usage":{"promptTokens":4,"completionTokens":5,"totalTokens":9},"durationMillis":12}`)
	if err := store.RecordContinuationIntent(ctx, userID, turn.ID, details); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].Status != "incomplete" || !turns[0].ContinuationAvailable {
		t.Fatalf("continuation state after route decision = %#v, %v", turns, err)
	}
	events, err := store.TurnEvents(ctx, userID, turn.ID, 0)
	if err != nil || len(events) != 4 || events[3].Kind != "continuation.intent_classified" || string(events[3].Details) != string(details) {
		t.Fatalf("intent decision event after restart = %#v, %v", events, err)
	}
	snapshot, err := store.AgentContinuationSnapshot(ctx, userID, turn.ID)
	if err != nil || snapshot.Status != "available" || string(snapshot.Ciphertext) != "encrypted-checkpoint" {
		t.Fatalf("checkpoint after route decision = %#v, %v", snapshot, err)
	}
	metrics, err := store.ContinuationChainMetrics(ctx, userID, turn.ID)
	if err != nil || metrics.ModelCalls != 1 || metrics.PromptTokens != 4 || metrics.CompletionTokens != 5 || metrics.TotalTokens != 9 || metrics.DurationMillis != 12 {
		t.Fatalf("intent classifier usage missing from continuation metrics: %#v, %v", metrics, err)
	}
}

func TestDecliningContinuationInvalidatesCheckpointDurably(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "continuation_declined")
	defer store.Close()
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_declined_input", ConversationID: conversation.ID, Role: "user", Content: "finish the task", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_declined", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "msg_declined_output", ConversationID: conversation.ID, Role: "assistant", Content: "The task stopped at its step limit.", CreatedAt: now.Add(time.Second)}
	continuation := &storage.ContinuationState{Version: 1, Ciphertext: []byte("encrypted-checkpoint"), Nonce: []byte("nonce"), ContentHash: "checkpoint-hash"}
	if err := store.FinishAgentTurnWithContinuation(ctx, userID, turn.ID, "incomplete", "step_limit", &output, json.RawMessage(`{}`), continuation); err != nil {
		t.Fatal(err)
	}
	if err := store.InvalidateAgentContinuationSnapshot(ctx, userID, turn.ID, "user_started_new_task"); err != nil {
		t.Fatal(err)
	}
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].ContinuationAvailable || turns[0].ContinuationUnavailableReason != "user_started_new_task" {
		t.Fatalf("declined checkpoint state = %#v, %v", turns, err)
	}
	snapshot, err := store.AgentContinuationSnapshot(ctx, userID, turn.ID)
	if err != nil || snapshot.Status != "invalidated" || len(snapshot.Ciphertext) != 0 {
		t.Fatalf("declined checkpoint snapshot = %#v, %v", snapshot, err)
	}
	events, err := store.TurnEvents(ctx, userID, turn.ID, 0)
	if err != nil || len(events) != 4 || events[3].Kind != "turn.continuation_invalidated" {
		t.Fatalf("declined checkpoint event = %#v, %v", events, err)
	}
}

func TestContinuationIntentWriteRollsBackEventWhenTurnUpdateFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "continuation_intent_rollback")
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_intent_rollback_input", ConversationID: conversation.ID, Role: "user", Content: "finish the task", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_intent_rollback", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "msg_intent_rollback_output", ConversationID: conversation.ID, Role: "assistant", Content: "stopped", CreatedAt: now.Add(time.Second)}
	continuation := &storage.ContinuationState{Version: 1, Ciphertext: []byte("encrypted-checkpoint"), Nonce: []byte("nonce"), ContentHash: "checkpoint-hash"}
	if err := store.FinishAgentTurnWithContinuation(ctx, userID, turn.ID, "incomplete", "step_limit", &output, json.RawMessage(`{}`), continuation); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := openRunJournalDB(t, root)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_continuation_route_update BEFORE UPDATE OF last_sequence ON agent_turns WHEN NEW.id='turn_intent_rollback' BEGIN SELECT RAISE(ABORT,'forced continuation route failure'); END`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordContinuationIntent(ctx, userID, turn.ID, json.RawMessage(`{"decision":"resume"}`)); err == nil {
		t.Fatal("route write unexpectedly succeeded despite the forced turn update failure")
	}
	events, err := store.TurnEvents(ctx, userID, turn.ID, 0)
	if err != nil || len(events) != 3 || events[2].Kind != "turn.continuation_saved" {
		t.Fatalf("route event was not rolled back: %#v, %v", events, err)
	}
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].LastSequence != 3 || !turns[0].ContinuationAvailable {
		t.Fatalf("turn/checkpoint state changed after rollback: %#v, %v", turns, err)
	}
}

func TestRunStateRebuildsToolObservationsAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "run_state")
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_run_state_input", ConversationID: conversation.ID, Role: "user", Content: "run tests and report the result", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_run_state", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	startDetails, err := json.Marshal(map[string]string{"inputSourceRef": runJournalMessageSourceID(input.ID, input.Content)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartAgentTurn(ctx, userID, turn, input, startDetails); err != nil {
		t.Fatal(err)
	}
	started, _ := json.Marshal(map[string]any{"toolCallId": "call_tests", "name": "exec_command"})
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.started", started, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	result := []byte(`{"ok":true,"exitCode":0,"summary":"tests passed"}`)
	digest := sha256.Sum256(result)
	sourceID := "tool_result:call_tests"
	if err := store.SaveContextSource(ctx, userID, conversation.ID, storage.ContextSourceCiphertext{SourceID: sourceID, SourceType: "tool_result", ContentHash: hex.EncodeToString(digest[:]), Ciphertext: []byte("encrypted-result"), Nonce: []byte("nonce")}); err != nil {
		t.Fatal(err)
	}
	completed, _ := json.Marshal(map[string]any{"toolCallId": "call_tests", "name": "exec_command", "sourceRef": sourceID, "ok": true})
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.completed", completed, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "msg_run_state_output", ConversationID: conversation.ID, Role: "assistant", Content: "Tests passed.", CreatedAt: now.Add(3 * time.Second)}
	if err := store.FinishAgentTurn(ctx, userID, turn.ID, "completed", "assistant_response", &output, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.AgentTurn(ctx, userID, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunState.CurrentRequest == nil || got.RunState.CurrentRequest.SourceRef != runJournalMessageSourceID(input.ID, input.Content) {
		t.Fatalf("current request source did not survive restart: %#v", got.RunState)
	}
	if len(got.RunState.Actions) != 1 || got.RunState.Actions[0].ToolCallID != "call_tests" || got.RunState.Actions[0].Status != "tool_reported_ok" || got.RunState.Actions[0].SourceRef != sourceID || got.RunState.Actions[0].SourceHash != hex.EncodeToString(digest[:]) || got.RunState.Actions[0].SourceAvailable != "available" {
		t.Fatalf("tool observation did not rebuild from durable trace/source: %#v", got.RunState.Actions)
	}
	if got.RunState.AssistantResponse == nil || got.RunState.AssistantResponse.Verification != "unverified" || got.CompletionAssessment.Status != "unverified" {
		t.Fatalf("assistant statement was incorrectly promoted to verified completion: run=%#v completion=%#v", got.RunState.AssistantResponse, got.CompletionAssessment)
	}
}

func runJournalMessageSourceID(messageID, content string) string {
	digest := sha256.Sum256([]byte(content))
	return "message:" + messageID + "#" + hex.EncodeToString(digest[:])
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
	if len(turns[0].RunState.PendingActions) != 1 || turns[0].RunState.PendingActions[0].ToolCallID != "call_1" {
		t.Fatalf("pending external effect was not reconstructed from the durable start event: %#v", turns[0].RunState.PendingActions)
	}
}

func TestNeedsReconciliationStatusAndRunStateSurviveRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "reconcile_restart")
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_reconcile_restart", ConversationID: conversation.ID, Role: "user", Content: "apply the requested change", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_reconcile_restart", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	startDetails, err := json.Marshal(map[string]string{"inputSourceRef": runJournalMessageSourceID(input.ID, input.Content)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartAgentTurn(ctx, userID, turn, input, startDetails); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.started", json.RawMessage(`{"toolCallId":"call_unresolved","name":"exec_command"}`), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := store.RecoverInterruptedAgentTurns(ctx); err != nil || count != 1 {
		t.Fatalf("recovered count = %d, %v", count, err)
	}
	got, err := store.AgentTurn(ctx, userID, turn.ID)
	if err != nil || got.Status != "needs_reconciliation" || got.RecoveryClass != "unknown_external_effect" || len(got.RunState.PendingActions) != 1 {
		t.Fatalf("unknown external effect projection = %#v, %v", got, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err = store.AgentTurn(ctx, userID, turn.ID)
	if err != nil || got.Status != "needs_reconciliation" || len(got.RunState.PendingActions) != 1 || got.RunState.PendingActions[0].ToolCallID != "call_unresolved" {
		t.Fatalf("reconciliation status or pending action was lost on restart: %#v, %v", got, err)
	}
}

func TestLegacyReconciliationMigrationRollsBackStatusesWithMarker(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "reconcile_migration_rollback")
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_reconcile_migration", ConversationID: conversation.ID, Role: "user", Content: "work", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_reconcile_migration", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	inbox := domain.InboxInput{ID: "inbox_reconcile_migration", ConversationID: conversation.ID, Content: "queued", CreatedAt: now}
	if err := store.QueueAgentInput(ctx, userID, inbox); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := openRunJournalDB(t, root)
	if _, err := db.ExecContext(ctx, `UPDATE agent_turns SET status='needs_reconciliation' WHERE id=?`, turn.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE agent_inbox SET status='needs_reconciliation' WHERE id=?`, inbox.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM runtime_settings WHERE key='migration.needs_reconciliation_status.v1'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_reconciliation_migration_marker BEFORE INSERT ON runtime_settings WHEN NEW.key='migration.needs_reconciliation_status.v1' BEGIN SELECT RAISE(ABORT,'forced reconciliation migration failure'); END`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, openErr := storage.Open(root)
	if openErr == nil {
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("database opened even though the reconciliation migration marker write failed")
	}
	db = openRunJournalDB(t, root)
	defer db.Close()
	var turnStatus, inboxStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM agent_turns WHERE id=?`, turn.ID).Scan(&turnStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM agent_inbox WHERE id=?`, inbox.ID).Scan(&inboxStatus); err != nil {
		t.Fatal(err)
	}
	var markerCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_settings WHERE key='migration.needs_reconciliation_status.v1'`).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if turnStatus != "needs_reconciliation" || inboxStatus != "needs_reconciliation" || markerCount != 0 {
		t.Fatalf("failed migration left partial state: turn=%q inbox=%q marker=%d", turnStatus, inboxStatus, markerCount)
	}
}

func TestAgentTurnReconciliationRollsBackWhenAuditAppendFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "reconcile_rollback")
	defer store.Close()
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_reconcile_rollback", ConversationID: conversation.ID, Role: "user", Content: "uncertain action", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_reconcile_rollback", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.started", json.RawMessage(`{"toolCallId":"call_rollback","effect":"external_write"}`), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAgentTurn(ctx, userID, turn.ID, "failed", "runtime_error", nil, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	db := openRunJournalDB(t, root)
	if _, err := db.ExecContext(ctx, `UPDATE agent_turns SET status='needs_reconciliation',recovery_class='unknown_external_effect' WHERE id=?`, turn.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_reconciliation_audit BEFORE INSERT ON agent_turn_reconciliation_events BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordAgentTurnReconciliation(ctx, userID, turn.ID, "no_effect_applied", "test evidence", now.Add(2*time.Second)); err == nil || !strings.Contains(err.Error(), "injected audit failure") {
		t.Fatalf("reconciliation did not return the injected audit insert failure: %v", err)
	}
	readBack, err := store.AgentTurn(ctx, userID, turn.ID)
	if err != nil || readBack.Status != "needs_reconciliation" || readBack.RecoveryClass != "unknown_external_effect" || readBack.ReconciliationNote != "" {
		t.Fatalf("failed reconciliation partially changed the turn: turn=%+v err=%v", readBack, err)
	}
	history, err := store.AgentTurnReconciliations(ctx, userID, turn.ID)
	if err != nil || len(history) != 0 {
		t.Fatalf("failed reconciliation left an audit record: history=%+v err=%v", history, err)
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

func TestRunJournalConversationStopPausesAndExplicitTurnResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "pause_resume")
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_pause_resume", ConversationID: conversation.ID, Role: "user", Content: "active", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_pause_resume", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	queued := []domain.InboxInput{
		{ID: "inbox_pause_1", ConversationID: conversation.ID, Content: "later 1", CreatedAt: now.Add(time.Second)},
		{ID: "inbox_pause_2", ConversationID: conversation.ID, Content: "later 2", CreatedAt: now.Add(2 * time.Second)},
	}
	for _, item := range queued {
		if err := store.QueueAgentInput(ctx, userID, item); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := store.CancelConversationAgentWork(ctx, userID, conversation.ID, "user_requested")
	if err != nil || receipt.CancelledTurnID != turn.ID || receipt.CancelledInboxCount != 2 {
		t.Fatalf("cancel receipt = %#v, %v", receipt, err)
	}
	for _, item := range queued {
		persisted, err := store.AgentInboxItem(ctx, userID, item.ID)
		if err != nil || persisted.Status != "cancelled" {
			t.Fatalf("cancelled inbox = %#v, %v", persisted, err)
		}
	}
	detail, err := store.Conversation(ctx, userID, conversation.ID)
	if err != nil || !detail.ExecutionPaused || len(detail.LifecycleEvents) != 1 || detail.LifecycleEvents[0].Kind != "conversation.paused" {
		t.Fatalf("paused conversation = %#v, %v", detail, err)
	}
	if err := store.QueueAgentInput(ctx, userID, domain.InboxInput{ID: "inbox_rejected", ConversationID: conversation.ID, Content: "must not queue", CreatedAt: now.Add(3 * time.Second)}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("queue while paused error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if count, err := store.RecoverInterruptedAgentTurns(ctx); err != nil || count != 1 {
		t.Fatalf("recovered active turn count = %d, %v", count, err)
	}
	detail, err = store.Conversation(ctx, userID, conversation.ID)
	if err != nil || !detail.ExecutionPaused || len(detail.LifecycleEvents) != 1 {
		t.Fatalf("paused state after restart = %#v, %v", detail, err)
	}
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].Status != "interrupted" || turns[0].RecoveryClass != "safe_to_retry" {
		t.Fatalf("turn recovery after restart = %#v, %v", turns, err)
	}
	resumedInput := domain.Message{ID: "msg_resume", ConversationID: conversation.ID, Role: "user", Content: "continue", CreatedAt: now.Add(4 * time.Second)}
	resumedTurn := domain.AgentTurn{ID: "turn_resume", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now.Add(4 * time.Second), UpdatedAt: now.Add(4 * time.Second)}
	if err := store.StartAgentTurn(ctx, userID, resumedTurn, resumedInput, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	detail, err = store.Conversation(ctx, userID, conversation.ID)
	if err != nil || detail.ExecutionPaused || len(detail.LifecycleEvents) != 2 || detail.LifecycleEvents[1].Kind != "conversation.resumed" {
		t.Fatalf("resumed state = %#v, %v", detail, err)
	}
}

func TestRunJournalManualReconciliationUnlocksFailedToolRetryAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "reconcile")
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_reconcile", ConversationID: conversation.ID, Role: "user", Content: "do work", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_reconcile", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.started", json.RawMessage(`{"toolCallId":"call_reconcile","effect":"external_write"}`), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAgentTurn(ctx, userID, turn.ID, "failed", "runtime_error", nil, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := openRunJournalDB(t, root)
	if _, err := db.ExecContext(ctx, `UPDATE agent_turns SET recovery_class='' WHERE id=?`, turn.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].RecoveryClass != "unknown_external_effect" {
		t.Fatalf("legacy recovery class migration = %#v, %v", turns, err)
	}
	retry := domain.AgentTurn{ID: "turn_retry_blocked", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second)}
	if err := store.StartRetryAgentTurn(ctx, userID, retry, input, turn.ID, nil, json.RawMessage(`{}`)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("retry before reconciliation error = %v", err)
	}
	revised := "reviewed and revised task"
	failedEdit := domain.AgentTurn{ID: turn.ID, ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second)}
	if err := store.StartRetryAgentTurn(ctx, userID, failedEdit, domain.Message{ID: input.ID, ConversationID: input.ConversationID, Role: "user", Content: revised, CreatedAt: now.Add(3 * time.Second)}, turn.ID, &revised, json.RawMessage(`{}`)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("edited retry before reconciliation error = %v", err)
	}
	if _, _, err := store.AgentTurnSeedForBranch(ctx, userID, turn.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("branch retry bypassed unresolved external effect: %v", err)
	}
	detail, err := store.Conversation(ctx, userID, conversation.ID)
	if err != nil || detail.Messages[0].Content != input.Content {
		t.Fatalf("blocked edit changed source message: %#v, %v", detail.Messages, err)
	}
	recorded, err := store.RecordAgentTurnReconciliation(ctx, userID, turn.ID, "no_effect_applied", "checked the target system; no effect was applied", now.Add(4*time.Second))
	if err != nil || recorded.Note == "" || recorded.Decision != "no_effect_applied" {
		t.Fatalf("reconciliation = %#v, %v", recorded, err)
	}
	retry = domain.AgentTurn{ID: "turn_retry_allowed", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now.Add(5 * time.Second), UpdatedAt: now.Add(5 * time.Second)}
	if err := store.StartRetryAgentTurn(ctx, userID, retry, domain.Message{ID: input.ID, ConversationID: input.ConversationID, Role: "user", Content: revised, CreatedAt: now.Add(5 * time.Second)}, turn.ID, &revised, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("retry after reconciliation: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	history, err := store.AgentTurnReconciliations(ctx, userID, turn.ID)
	if err != nil || len(history) != 1 || history[0].Decision != "no_effect_applied" || history[0].Note != recorded.Note {
		t.Fatalf("reconciliation audit history after restart = %#v, %v", history, err)
	}
	if count, err := store.RecoverInterruptedAgentTurns(ctx); err != nil || count != 1 {
		t.Fatalf("recovered retry count = %d, %v", count, err)
	}
	detail, err = store.Conversation(ctx, userID, conversation.ID)
	if err != nil || detail.Messages[0].Content != revised {
		t.Fatalf("edited input after restart = %#v, %v", detail.Messages, err)
	}
	turns, err = store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 2 || turns[0].Status != "interrupted" || turns[0].RecoveryClass != "safe_to_retry" || turns[0].RetryOfTurnID != turn.ID || turns[1].ReconciliationNote != recorded.Note {
		t.Fatalf("turns after restart = %#v, %v", turns, err)
	}
	db = openRunJournalDB(t, root)
	defer db.Close()
	var prior, replacement string
	var revisionCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(prior_content),MAX(revised_content) FROM message_revisions WHERE message_id=?`, input.ID).Scan(&revisionCount, &prior, &replacement); err != nil || revisionCount != 1 || prior != input.Content || replacement != revised {
		t.Fatalf("durable message revisions (count=%d) = %q -> %q, %v", revisionCount, prior, replacement, err)
	}
}

func TestRunJournalBranchCopiesOnlyPrefixAndRollsBackFailedStart(t *testing.T) {
	ctx := context.Background()
	store, userID, conversation, generation := runJournalFixture(t, "branch")
	defer store.Close()
	now := time.Now().UTC()
	priorUser := domain.Message{ID: "msg_branch_prior_user", ConversationID: conversation.ID, Role: "user", Content: "earlier question", CreatedAt: now}
	priorAssistant := domain.Message{ID: "msg_branch_prior_assistant", ConversationID: conversation.ID, Role: "assistant", Content: "earlier answer", CreatedAt: now.Add(time.Second)}
	for _, message := range []domain.Message{priorUser, priorAssistant} {
		if err := store.AddMessage(ctx, userID, message); err != nil {
			t.Fatal(err)
		}
	}
	target := domain.Message{ID: "msg_branch_target", ConversationID: conversation.ID, Role: "user", Content: "old request", CreatedAt: now.Add(2 * time.Second)}
	sourceTurn := domain.AgentTurn{ID: "turn_branch_source", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: target.CreatedAt, UpdatedAt: target.CreatedAt}
	if err := store.StartAgentTurn(ctx, userID, sourceTurn, target, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	answer := domain.Message{ID: "msg_branch_answer", ConversationID: conversation.ID, Role: "assistant", Content: "answer", CreatedAt: now.Add(3 * time.Second)}
	if err := store.FinishAgentTurn(ctx, userID, sourceTurn.ID, "completed", "assistant_response", &answer, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	branchAt := now.Add(4 * time.Second)
	branch := domain.Conversation{ID: "run_branch_copy", UserID: userID, Title: "branch", ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, PermissionProfile: domain.DefaultPermissionProfile(), ParentConversationID: conversation.ID, BranchFromMessageID: target.ID, CreatedAt: branchAt, UpdatedAt: branchAt}
	branchInput := domain.Message{ID: "msg_branch_input", ConversationID: branch.ID, Role: "user", Content: "edited request", CreatedAt: branchAt}
	branchTurn := domain.AgentTurn{ID: "turn_branch_run", ConversationID: branch.ID, InputMessageID: branchInput.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, PermissionProfile: domain.DefaultPermissionProfile(), StartedAt: branchAt, UpdatedAt: branchAt}
	if err := store.StartBranchAgentTurn(ctx, userID, conversation.ID, sourceTurn.ID, target.ID, branch, branchTurn, branchInput, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	branchDetail, err := store.Conversation(ctx, userID, branch.ID)
	if err != nil || len(branchDetail.Messages) != 3 || branchDetail.Messages[0].Content != priorUser.Content || branchDetail.Messages[1].Content != priorAssistant.Content || branchDetail.Messages[2].Content != branchInput.Content || branchDetail.ParentConversationID != conversation.ID || branchDetail.BranchFromMessageID != target.ID || branchDetail.AgentGenerationID != generation.ID {
		t.Fatalf("branch prefix/readback = %#v, %v", branchDetail, err)
	}
	original, err := store.Conversation(ctx, userID, conversation.ID)
	if err != nil || len(original.Messages) != 4 || original.Messages[2].ID != target.ID || original.Messages[2].Content != target.Content || original.Messages[3].ID != answer.ID {
		t.Fatalf("source conversation was changed: %#v, %v", original.Messages, err)
	}
	failedBranch := branch
	failedBranch.ID = "run_branch_rollback"
	failedBranchInput := branchInput
	failedBranchInput.ID = "msg_branch_rollback_input"
	failedBranchInput.ConversationID = failedBranch.ID
	failedBranchTurn := branchTurn
	failedBranchTurn.ID = sourceTurn.ID // Force the linked insert to fail after branch rows have been staged.
	failedBranchTurn.ConversationID = failedBranch.ID
	failedBranchTurn.InputMessageID = failedBranchInput.ID
	if err := store.StartBranchAgentTurn(ctx, userID, conversation.ID, sourceTurn.ID, target.ID, failedBranch, failedBranchTurn, failedBranchInput, json.RawMessage(`{}`)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate branch turn error = %v", err)
	}
	if _, err := store.Conversation(ctx, userID, failedBranch.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed branch transaction left a conversation behind: %v", err)
	}
}

func TestRunJournalRecoveryUpdatesClaimedInboxWithoutReplaying(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "inbox_recovery")
	now := time.Now().UTC()
	item := domain.InboxInput{ID: "inbox_recovery_item", ConversationID: conversation.ID, Content: "queued work", CreatedAt: now}
	if err := store.QueueAgentInput(ctx, userID, item); err != nil {
		t.Fatal(err)
	}
	turn := domain.AgentTurn{ID: "turn_inbox_recovery", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	input := domain.Message{ID: "msg_inbox_recovery", ConversationID: conversation.ID, Role: "user", Content: item.Content, CreatedAt: now}
	if err := store.StartQueuedAgentTurn(ctx, userID, turn, input, item.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.AgentInboxItem(ctx, userID, item.ID)
	if err != nil || claimed.Status != "claimed" || claimed.TurnID != turn.ID {
		t.Fatalf("claimed item = %#v, %v", claimed, err)
	}
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.started", json.RawMessage(`{"toolCallId":"call_1"}`), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if count, err := store.RecoverInterruptedAgentTurns(ctx); err != nil || count != 1 {
		t.Fatalf("recovered count = %d, %v", count, err)
	}
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].Status != "needs_reconciliation" || turns[0].RecoveryClass != "unknown_external_effect" {
		t.Fatalf("recovered turn = %#v, %v", turns, err)
	}
	if len(turns[0].RunState.PendingActions) != 1 || turns[0].RunState.PendingActions[0].ToolCallID != "call_1" {
		t.Fatalf("pending inbox effect was not reconstructed after restart: %#v", turns[0].RunState.PendingActions)
	}
	item, err = store.AgentInboxItem(ctx, userID, item.ID)
	if err != nil || item.Status != "needs_reconciliation" || item.TurnID != turn.ID {
		t.Fatalf("recovered inbox item = %#v, %v", item, err)
	}
	directTurn := domain.AgentTurn{ID: "turn_direct_blocked", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)}
	directInput := domain.Message{ID: "msg_direct_blocked", ConversationID: conversation.ID, Role: "user", Content: "must wait for review", CreatedAt: now.Add(time.Second)}
	if err := store.StartAgentTurn(ctx, userID, directTurn, directInput, json.RawMessage(`{}`)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("direct turn started before reconciliation: %v", err)
	}
	detail, err := store.Conversation(ctx, userID, conversation.ID)
	if err != nil || len(detail.Messages) != 1 || detail.Messages[0].ID != input.ID {
		t.Fatalf("blocked direct input changed the durable transcript: %#v, %v", detail.Messages, err)
	}
	completedItem := domain.InboxInput{ID: "inbox_recovery_completed", ConversationID: conversation.ID, Content: "follow-up after recovery", CreatedAt: now.Add(2 * time.Second)}
	if err := store.QueueAgentInput(ctx, userID, completedItem); err != nil {
		t.Fatal(err)
	}
	completedTurn := domain.AgentTurn{ID: "turn_inbox_completed", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second)}
	completedInput := domain.Message{ID: "msg_inbox_completed", ConversationID: conversation.ID, Role: "user", Content: completedItem.Content, CreatedAt: completedItem.CreatedAt}
	if err := store.StartQueuedAgentTurn(ctx, userID, completedTurn, completedInput, completedItem.ID, json.RawMessage(`{}`)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("queued follow-up started before manual reconciliation: %v", err)
	}
	queuedReadback, err := store.AgentInboxItem(ctx, userID, completedItem.ID)
	if err != nil || queuedReadback.Status != "queued" || queuedReadback.TurnID != "" {
		t.Fatalf("blocked queued item changed state: %#v, %v", queuedReadback, err)
	}
	if _, err := store.RecordAgentTurnReconciliation(ctx, userID, turn.ID, "effect_applied", "confirmed the remote operation succeeded; do not repeat", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RetryAgentTurnSeed(ctx, userID, turn.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("retry replayed a confirmed external effect: %v", err)
	}
	if _, _, err := store.AgentTurnSeedForBranch(ctx, userID, turn.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("branch retry replayed a confirmed external effect: %v", err)
	}
	if err := store.StartQueuedAgentTurn(ctx, userID, completedTurn, completedInput, completedItem.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	output := domain.Message{ID: "msg_inbox_completed_output", ConversationID: conversation.ID, Role: "assistant", Content: "done", CreatedAt: now.Add(3 * time.Second)}
	if err := store.FinishAgentTurn(ctx, userID, completedTurn.ID, "completed", "assistant_response", &output, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	completedItem, err = store.AgentInboxItem(ctx, userID, completedItem.ID)
	if err != nil || completedItem.Status != "completed" || completedItem.TurnID != completedTurn.ID {
		t.Fatalf("completed inbox item = %#v, %v", completedItem, err)
	}
}

func TestRunJournalConversationStopRollsBackAllLifecycleWritesOnEventFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, userID, conversation, generation := runJournalFixtureAt(t, root, "cancel_rollback")
	defer store.Close()
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_cancel_rollback", ConversationID: conversation.ID, Role: "user", Content: "active", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_cancel_rollback", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	item := domain.InboxInput{ID: "inbox_cancel_rollback", ConversationID: conversation.ID, Content: "queued", CreatedAt: now.Add(time.Second)}
	if err := store.QueueAgentInput(ctx, userID, item); err != nil {
		t.Fatal(err)
	}
	db := openRunJournalDB(t, root)
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_conversation_pause BEFORE INSERT ON conversation_lifecycle_events WHEN NEW.kind='conversation.paused' BEGIN SELECT RAISE(ABORT,'forced event failure'); END`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := store.CancelConversationAgentWork(ctx, userID, conversation.ID, "test"); err == nil {
		db.Close()
		t.Fatal("expected injected lifecycle event failure")
	}
	turns, err := store.AgentTurns(ctx, userID, conversation.ID)
	if err != nil || len(turns) != 1 || turns[0].Status != "running" {
		db.Close()
		t.Fatalf("turn write was not rolled back: %#v, %v", turns, err)
	}
	persisted, err := store.AgentInboxItem(ctx, userID, item.ID)
	if err != nil || persisted.Status != "queued" {
		db.Close()
		t.Fatalf("inbox write was not rolled back: %#v, %v", persisted, err)
	}
	paused, err := store.ConversationExecutionPaused(ctx, userID, conversation.ID)
	if err != nil || paused {
		db.Close()
		t.Fatalf("pause state was not rolled back: %v, %v", paused, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_conversation_pause`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func runJournalFixture(t *testing.T, suffix string) (*storage.Store, string, domain.Conversation, domain.AgentGeneration) {
	t.Helper()
	return runJournalFixtureAt(t, t.TempDir(), suffix)
}

func runJournalFixtureAt(t *testing.T, root, suffix string) (*storage.Store, string, domain.Conversation, domain.AgentGeneration) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(root)
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

func openRunJournalDB(t *testing.T, root string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(root, "axiom.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	return db
}
