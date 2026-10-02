package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestImportAgentContinuationTurnRollsBackLinkedRecordsOnTraceFailure(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	userID, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	provider := domain.Provider{ID: "provider_import_rollback", UserID: userID, Name: "Test", Kind: "openai-compatible", BaseURL: "http://127.0.0.1/v1", Model: "test", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	definitionDigest := "sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	if _, err := store.db.ExecContext(ctx, `INSERT INTO agent_definitions(user_id,digest,api_version,name,description,parent_digest,spec_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, userID, definitionDigest, "v1", "Test", "Test definition", "", []byte(`{}`), now); err != nil {
		t.Fatal(err)
	}
	generation := domain.AgentGeneration{ID: "generation_import_rollback", UserID: userID, Number: 1, Scope: "user", Status: "active", DefinitionDigest: definitionDigest, CreatedAt: now, UpdatedAt: now}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO agent_generations(id,user_id,generation_number,scope,scope_key,status,definition_digest,evidence_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, generation.ID, userID, generation.Number, generation.Scope, "", generation.Status, generation.DefinitionDigest, []byte(`{}`), now, now); err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "conversation_import_rollback", UserID: userID, Title: "Import rollback", ProviderID: provider.ID, PermissionProfile: domain.DefaultPermissionProfile(), CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	input := domain.Message{ID: "message_import_rollback_user", ConversationID: conversation.ID, Role: "user", Content: "Continue safely", CreatedAt: now}
	result := domain.Message{ID: "message_import_rollback_assistant", ConversationID: conversation.ID, Role: "assistant", Content: "Paused safely", CreatedAt: now.Add(time.Millisecond)}
	for _, message := range []domain.Message{input, result} {
		if err := store.AddMessage(ctx, userID, message); err != nil {
			t.Fatal(err)
		}
	}
	turn := domain.AgentTurn{ID: "turn_import_rollback", ConversationID: conversation.ID, UserID: userID, InputMessageID: input.ID, ProviderID: provider.ID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, PermissionProfile: conversation.PermissionProfile, Status: "incomplete", StopReason: "step_limit"}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_imported_handoff_event BEFORE INSERT ON agent_trace_events WHEN NEW.turn_id='turn_import_rollback' BEGIN SELECT RAISE(ABORT,'forced handoff trace failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportAgentContinuationTurn(ctx, userID, turn, result.ID, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", []byte("cipher"), []byte("nonce"), now.Add(time.Second)); err == nil {
		t.Fatal("import succeeded despite injected trace transaction failure")
	}
	if _, err := store.AgentTurn(ctx, userID, turn.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed import left a partial turn: %v", err)
	}
	if _, err := store.AgentContinuationSnapshot(ctx, userID, turn.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed import left a partial continuation snapshot: %v", err)
	}
	var eventCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_trace_events WHERE turn_id=?`, turn.ID).Scan(&eventCount); err != nil || eventCount != 0 {
		t.Fatalf("failed import left partial trace records: count=%d err=%v", eventCount, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_imported_handoff_event`); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ImportAgentContinuationTurn(ctx, userID, turn, result.ID, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", []byte("cipher"), []byte("nonce"), now.Add(2*time.Second))
	if err != nil || readBack.ID != turn.ID || !readBack.ContinuationAvailable {
		t.Fatalf("continuation import did not recover after rolled-back failure: turn=%+v err=%v", readBack, err)
	}
}
