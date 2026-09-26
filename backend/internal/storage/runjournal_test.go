package storage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
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
	if err := store.AppendTurnEvent(ctx, userID, turn.ID, "tool.started", json.RawMessage(`{"toolCallId":"call_reconcile"}`), now.Add(time.Second)); err != nil {
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
	detail, err := store.Conversation(ctx, userID, conversation.ID)
	if err != nil || detail.Messages[0].Content != input.Content {
		t.Fatalf("blocked edit changed source message: %#v, %v", detail.Messages, err)
	}
	recorded, err := store.RecordAgentTurnReconciliation(ctx, userID, turn.ID, "verified the external effect and its current state", now.Add(4*time.Second))
	if err != nil || recorded.Note == "" || recorded.Decision != "retry_authorized" {
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
		t.Fatalf("blocked direct input changed conversation state: %#v, %v", detail.Messages, err)
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
	if _, err := store.RecordAgentTurnReconciliation(ctx, userID, turn.ID, "reviewed the uncertain external effect", now.Add(time.Second)); err != nil {
		t.Fatal(err)
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
