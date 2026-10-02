package storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

// ImportAgentContinuationTurn records a verified cross-node safe boundary in
// an already-created cloud continuation conversation. The source turn,
// encrypted resumable snapshot, and trace are committed atomically. Retrying
// the same task-derived turn ID is idempotent and never replaces its cipher.
func (s *Store) ImportAgentContinuationTurn(ctx context.Context, userID string, turn domain.AgentTurn, resultMessageID string, contentHash string, checkpointCipher, checkpointNonce []byte, now time.Time) (domain.AgentTurn, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(turn.ID) == "" || strings.TrimSpace(turn.ConversationID) == "" || turn.UserID != userID || strings.TrimSpace(turn.InputMessageID) == "" || strings.TrimSpace(resultMessageID) == "" || turn.ProviderID == "" || turn.AgentGenerationID == "" || turn.AgentDefinitionDigest == "" || !turn.PermissionProfile.Valid() || turn.Status != "incomplete" || !domain.SafeAgentContinuationStopReason(turn.StopReason) || len(contentHash) != 64 || len(checkpointCipher) == 0 || len(checkpointCipher) > (16<<20)+(1<<10) || len(checkpointNonce) == 0 {
		return domain.AgentTurn{}, domain.ErrInvalid
	}
	if _, err := hex.DecodeString(contentHash); err != nil {
		return domain.AgentTurn{}, domain.ErrInvalid
	}
	contentHash = strings.ToLower(contentHash)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.AgentTurn{}, err
	}
	defer tx.Rollback()
	var providerID, generationID, definitionDigest string
	var profile domain.PermissionProfile
	if err := tx.QueryRowContext(ctx, `SELECT c.provider_id,c.permission_profile,b.generation_id,b.definition_digest FROM conversations c JOIN conversation_agent_bindings b ON b.conversation_id=c.id AND b.user_id=c.user_id WHERE c.id=? AND c.user_id=? AND c.deleted_at IS NULL`, turn.ConversationID, userID).Scan(&providerID, &profile, &generationID, &definitionDigest); errors.Is(err, sql.ErrNoRows) {
		return domain.AgentTurn{}, domain.ErrNotFound
	} else if err != nil {
		return domain.AgentTurn{}, err
	}
	if providerID != turn.ProviderID || profile != turn.PermissionProfile || generationID != turn.AgentGenerationID || definitionDigest != turn.AgentDefinitionDigest {
		return domain.AgentTurn{}, fmt.Errorf("imported continuation runtime binding changed: %w", domain.ErrConflict)
	}
	var inputRole, inputContent, resultRole string
	if err := tx.QueryRowContext(ctx, `SELECT role,content FROM messages WHERE id=? AND conversation_id=?`, turn.InputMessageID, turn.ConversationID).Scan(&inputRole, &inputContent); errors.Is(err, sql.ErrNoRows) {
		return domain.AgentTurn{}, domain.ErrNotFound
	} else if err != nil {
		return domain.AgentTurn{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT role FROM messages WHERE id=? AND conversation_id=?`, resultMessageID, turn.ConversationID).Scan(&resultRole); errors.Is(err, sql.ErrNoRows) {
		return domain.AgentTurn{}, domain.ErrNotFound
	} else if err != nil {
		return domain.AgentTurn{}, err
	}
	if inputRole != "user" || strings.TrimSpace(inputContent) == "" || resultRole != "assistant" {
		return domain.AgentTurn{}, fmt.Errorf("imported continuation messages have invalid roles: %w", domain.ErrConflict)
	}
	turn.ResultMessageID = resultMessageID
	turn.ContinuationAvailable = true
	turn.ContinuationUnavailableReason = ""
	turn.LastSequence = 3
	turn.StartedAt = now.UTC()
	turn.UpdatedAt = now.UTC()
	var existing domain.AgentTurn
	existing, err = agentTurnByID(ctx, tx, userID, turn.ID)
	if err == nil {
		if existing.ConversationID != turn.ConversationID || existing.InputMessageID != turn.InputMessageID || existing.ResultMessageID != resultMessageID || existing.ProviderID != turn.ProviderID || existing.AgentGenerationID != turn.AgentGenerationID || existing.AgentDefinitionDigest != turn.AgentDefinitionDigest || existing.PermissionProfile != turn.PermissionProfile || existing.Status != "incomplete" || existing.StopReason != turn.StopReason {
			return domain.AgentTurn{}, domain.ErrConflict
		}
		var persistedHash string
		var persistedCipher, persistedNonce []byte
		if err := tx.QueryRowContext(ctx, `SELECT content_sha256,state_cipher,state_nonce FROM agent_continuation_snapshots WHERE source_turn_id=? AND user_id=? AND status IN ('available','consumed')`, turn.ID, userID).Scan(&persistedHash, &persistedCipher, &persistedNonce); err != nil {
			return domain.AgentTurn{}, err
		}
		if persistedHash != contentHash || len(persistedCipher) == 0 || len(persistedNonce) == 0 {
			return domain.AgentTurn{}, domain.ErrConflict
		}
	} else if errors.Is(err, domain.ErrNotFound) {
		var latestMessageID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE conversation_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1`, turn.ConversationID).Scan(&latestMessageID); err != nil {
			return domain.AgentTurn{}, err
		}
		if latestMessageID != resultMessageID {
			return domain.AgentTurn{}, fmt.Errorf("imported continuation result is not the latest conversation message: %w", domain.ErrConflict)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO agent_turns(id,conversation_id,user_id,input_message_id,result_message_id,provider_id,generation_id,definition_digest,permission_profile,retry_of_turn_id,input_content_snapshot,inbox_id,continued_from_turn_id,continuation_chain_id,status,stop_reason,recovery_class,cancel_requested,last_sequence,started_at,updated_at,completed_at) VALUES(?,?,?,?,?,?,?,?,?,'',?,'','',?,'incomplete',?,'',0,?,?,?,NULL)`, turn.ID, turn.ConversationID, userID, turn.InputMessageID, resultMessageID, turn.ProviderID, turn.AgentGenerationID, turn.AgentDefinitionDigest, turn.PermissionProfile, inputContent, turn.ID, turn.StopReason, turn.LastSequence, turn.StartedAt, turn.UpdatedAt)
		if err != nil {
			return domain.AgentTurn{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_continuation_snapshots(source_turn_id,user_id,conversation_id,snapshot_version,state_cipher,state_nonce,content_sha256,status,unavailable_reason,idempotency_key,consumed_turn_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'available','','','',?,?)`, turn.ID, userID, turn.ConversationID, 1, checkpointCipher, checkpointNonce, strings.ToLower(contentHash), now.UTC(), now.UTC()); err != nil {
			return domain.AgentTurn{}, err
		}
		startedDetails, err := json.Marshal(map[string]string{"source": "verified_node_handoff"})
		if err != nil {
			return domain.AgentTurn{}, err
		}
		stoppedDetails, err := json.Marshal(map[string]string{"status": "incomplete", "stopReason": turn.StopReason})
		if err != nil {
			return domain.AgentTurn{}, err
		}
		checkpointDetails, err := json.Marshal(map[string]string{"status": "available", "source": "verified_node_handoff"})
		if err != nil {
			return domain.AgentTurn{}, err
		}
		for sequence, event := range []struct {
			kind    string
			details []byte
		}{{"turn.started", startedDetails}, {"turn.incomplete", stoppedDetails}, {"turn.continuation_saved", checkpointDetails}} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID(turn.ID, sequence+1), turn.ConversationID, turn.ID, sequence+1, event.kind, event.details, now.UTC()); err != nil {
				return domain.AgentTurn{}, err
			}
		}
	} else {
		return domain.AgentTurn{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.AgentTurn{}, err
	}
	readBack, err := s.AgentTurn(ctx, userID, turn.ID)
	if err != nil {
		return domain.AgentTurn{}, fmt.Errorf("read imported continuation turn back: %w", err)
	}
	snapshot, err := s.AgentContinuationSnapshot(ctx, userID, turn.ID)
	if err != nil {
		return domain.AgentTurn{}, fmt.Errorf("read imported continuation snapshot back: %w", err)
	}
	if readBack.ConversationID != turn.ConversationID || readBack.InputMessageID != turn.InputMessageID || readBack.ResultMessageID != resultMessageID || readBack.Status != "incomplete" || readBack.StopReason != turn.StopReason || readBack.ContinuationAvailable != (snapshot.Status == "available") || (snapshot.Status != "available" && snapshot.Status != "consumed") || snapshot.Version != 1 || snapshot.ContentHash != strings.ToLower(contentHash) || len(snapshot.Ciphertext) == 0 || len(snapshot.Nonce) == 0 {
		return domain.AgentTurn{}, domain.ErrConflict
	}
	return readBack, nil
}

type continuationTurnScanner interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func agentTurnByID(ctx context.Context, query continuationTurnScanner, userID, turnID string) (domain.AgentTurn, error) {
	var turn domain.AgentTurn
	err := query.QueryRowContext(ctx, `SELECT id,conversation_id,user_id,input_message_id,COALESCE(result_message_id,''),provider_id,generation_id,definition_digest,permission_profile,status,stop_reason,recovery_class,cancel_requested,last_sequence,started_at,updated_at,completed_at FROM agent_turns WHERE id=? AND user_id=?`, turnID, userID).Scan(&turn.ID, &turn.ConversationID, &turn.UserID, &turn.InputMessageID, &turn.ResultMessageID, &turn.ProviderID, &turn.AgentGenerationID, &turn.AgentDefinitionDigest, &turn.PermissionProfile, &turn.Status, &turn.StopReason, &turn.RecoveryClass, &turn.CancelRequested, &turn.LastSequence, &turn.StartedAt, &turn.UpdatedAt, &turn.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentTurn{}, domain.ErrNotFound
	}
	return turn, err
}
