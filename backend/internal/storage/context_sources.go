package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

// ContextSourceCiphertext is an encrypted, conversation-scoped source artifact.
// Conversation messages remain in the messages table and are resolved there.
type ContextSourceCiphertext struct {
	SourceID    string
	SourceType  string
	ContentHash string
	Ciphertext  []byte
	Nonce       []byte
}

type ContextCompactionState struct {
	ConversationID         string
	UserID                 string
	StateVersion           int
	Strategy               string
	ProviderID             string
	Model                  string
	ProtocolVersion        string
	CanonicalSourceID      string
	CoveredSourceIDs       []string
	TokensAtLastCompaction int
	UpdatedAt              time.Time
}

type ContextTailState struct {
	ConversationID string
	UserID         string
	SourceIDs      []string
	UpdatedAt      time.Time
}

// SaveContextSource durably archives a complete tool result before the runtime
// may replace it with a trace preview or a compacted projection. Repeated saves
// are idempotent only when they refer to the same bytes.
func (s *Store) SaveContextSource(ctx context.Context, userID, conversationID string, source ContextSourceCiphertext) error {
	if userID == "" || conversationID == "" || source.SourceID == "" || source.SourceType == "" || source.ContentHash == "" || len(source.Ciphertext) == 0 || len(source.Nonce) == 0 {
		return domain.ErrInvalid
	}
	if len(source.ContentHash) != sha256.Size*2 {
		return domain.ErrInvalid
	}
	if _, err := hex.DecodeString(source.ContentHash); err != nil {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO context_sources(conversation_id,user_id,source_id,source_type,content_sha256,content_cipher,content_nonce,created_at)
SELECT ?,?,?,?,?,?,?,? FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL
ON CONFLICT(conversation_id,source_id) DO NOTHING`, conversationID, userID, source.SourceID, source.SourceType, source.ContentHash, source.Ciphertext, source.Nonce, time.Now().UTC(), conversationID, userID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		var existing ContextSourceCiphertext
		err = tx.QueryRowContext(ctx, `SELECT cs.source_id,cs.source_type,cs.content_sha256,cs.content_cipher,cs.content_nonce FROM context_sources cs
JOIN conversations c ON c.id=cs.conversation_id
WHERE cs.conversation_id=? AND cs.user_id=? AND cs.source_id=? AND c.deleted_at IS NULL`, conversationID, userID, source.SourceID).
			Scan(&existing.SourceID, &existing.SourceType, &existing.ContentHash, &existing.Ciphertext, &existing.Nonce)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if existing.SourceType != source.SourceType || existing.ContentHash != source.ContentHash {
			return fmt.Errorf("context source %q already exists with different content", source.SourceID)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var persistedHash string
	if err := s.db.QueryRowContext(ctx, `SELECT content_sha256 FROM context_sources WHERE conversation_id=? AND user_id=? AND source_id=?`, conversationID, userID, source.SourceID).Scan(&persistedHash); err != nil {
		return err
	}
	if persistedHash != source.ContentHash {
		return fmt.Errorf("context source %q failed read-back verification", source.SourceID)
	}
	return nil
}

// ReadContextSource returns a stored user message or encrypted tool artifact.
// Callers must still verify the artifact hash after decrypting its ciphertext.
func (s *Store) ReadContextSource(ctx context.Context, userID, conversationID, sourceID string) (content string, source ContextSourceCiphertext, err error) {
	if userID == "" || conversationID == "" || sourceID == "" {
		return "", source, domain.ErrInvalid
	}
	if strings.HasPrefix(sourceID, "message:") {
		messageRef := strings.TrimPrefix(sourceID, "message:")
		messageID, expectedHash, hasVersion := strings.Cut(messageRef, "#")
		if messageID == "" || (hasVersion && (len(expectedHash) != 64 || strings.Trim(expectedHash, "0123456789abcdef") != "")) {
			return "", source, domain.ErrInvalid
		}
		err = s.db.QueryRowContext(ctx, `SELECT m.content FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=? AND m.conversation_id=? AND c.user_id=? AND c.deleted_at IS NULL`, messageID, conversationID, userID).Scan(&content)
		if errors.Is(err, sql.ErrNoRows) {
			return "", source, domain.ErrNotFound
		}
		if err != nil || !hasVersion {
			return content, source, err
		}
		if messageContentHash(content) == expectedHash {
			return content, source, nil
		}
		rows, queryErr := s.db.QueryContext(ctx, `SELECT prior_content FROM message_revisions WHERE message_id=?
UNION SELECT revised_content FROM message_revisions WHERE message_id=?`, messageID, messageID)
		if queryErr != nil {
			return "", source, queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var revision string
			if scanErr := rows.Scan(&revision); scanErr != nil {
				return "", source, scanErr
			}
			if messageContentHash(revision) == expectedHash {
				return revision, source, nil
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return "", source, rowsErr
		}
		return "", source, fmt.Errorf("message source version %q is unavailable: %w", sourceID, domain.ErrNotFound)
	}
	err = s.db.QueryRowContext(ctx, `SELECT cs.source_id,cs.source_type,cs.content_sha256,cs.content_cipher,cs.content_nonce FROM context_sources cs
JOIN conversations c ON c.id=cs.conversation_id
WHERE cs.source_id=? AND cs.conversation_id=? AND cs.user_id=? AND c.deleted_at IS NULL`, sourceID, conversationID, userID).
		Scan(&source.SourceID, &source.SourceType, &source.ContentHash, &source.Ciphertext, &source.Nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return "", source, domain.ErrNotFound
	}
	return "", source, err
}

func messageContentHash(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

// SaveContextCompactionCheckpoint atomically stores a canonical compaction
// artifact and its compatibility/source metadata. A failed state write cannot
// leave a half-committed checkpoint behind.
func (s *Store) SaveContextCompactionCheckpoint(ctx context.Context, state ContextCompactionState, source ContextSourceCiphertext) error {
	expectedSourceType := ""
	switch state.Strategy {
	case "semantic":
		expectedSourceType = "semantic_summary"
	case "provider_native":
		expectedSourceType = "responses_native_state"
	}
	if state.UserID == "" || state.ConversationID == "" || state.StateVersion < 1 || expectedSourceType == "" || state.ProviderID == "" || state.Model == "" || state.ProtocolVersion == "" || state.CanonicalSourceID == "" || state.CanonicalSourceID != source.SourceID || len(state.CoveredSourceIDs) == 0 || source.SourceType != expectedSourceType || source.ContentHash == "" || len(source.Ciphertext) == 0 || len(source.Nonce) == 0 {
		return domain.ErrInvalid
	}
	if len(source.ContentHash) != sha256.Size*2 {
		return domain.ErrInvalid
	}
	if _, err := hex.DecodeString(source.ContentHash); err != nil {
		return domain.ErrInvalid
	}
	seenSources := make(map[string]struct{}, len(state.CoveredSourceIDs))
	for _, sourceID := range state.CoveredSourceIDs {
		if sourceID == "" {
			return domain.ErrInvalid
		}
		if _, exists := seenSources[sourceID]; exists {
			return domain.ErrInvalid
		}
		seenSources[sourceID] = struct{}{}
	}
	covered, err := json.Marshal(state.CoveredSourceIDs)
	if err != nil {
		return err
	}
	state.UpdatedAt = time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO context_sources(conversation_id,user_id,source_id,source_type,content_sha256,content_cipher,content_nonce,created_at)
SELECT ?,?,?,?,?,?,?,? FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL
ON CONFLICT(conversation_id,source_id) DO NOTHING`, state.ConversationID, state.UserID, source.SourceID, source.SourceType, source.ContentHash, source.Ciphertext, source.Nonce, state.UpdatedAt, state.ConversationID, state.UserID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		var existingType, existingHash string
		if err := tx.QueryRowContext(ctx, `SELECT source_type,content_sha256 FROM context_sources WHERE conversation_id=? AND user_id=? AND source_id=?`, state.ConversationID, state.UserID, source.SourceID).Scan(&existingType, &existingHash); errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		} else if err != nil {
			return err
		} else if existingType != source.SourceType || existingHash != source.ContentHash {
			return fmt.Errorf("context source %q already exists with different content", source.SourceID)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO context_compaction_states(conversation_id,user_id,state_version,strategy,provider_id,model,protocol_version,canonical_source_id,covered_sources_json,tokens_at_last_compaction,updated_at)
SELECT ?,?,?,?,?,?,?,?,?,?,? FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL
ON CONFLICT(conversation_id) DO UPDATE SET user_id=excluded.user_id,state_version=excluded.state_version,strategy=excluded.strategy,provider_id=excluded.provider_id,model=excluded.model,protocol_version=excluded.protocol_version,canonical_source_id=excluded.canonical_source_id,covered_sources_json=excluded.covered_sources_json,tokens_at_last_compaction=excluded.tokens_at_last_compaction,updated_at=excluded.updated_at`, state.ConversationID, state.UserID, state.StateVersion, state.Strategy, state.ProviderID, state.Model, state.ProtocolVersion, state.CanonicalSourceID, covered, state.TokensAtLastCompaction, state.UpdatedAt, state.ConversationID, state.UserID)
	if err != nil {
		return err
	}
	var readBackID string
	var readBackRefs []byte
	var readBackTokens int
	if err := tx.QueryRowContext(ctx, `SELECT canonical_source_id,covered_sources_json,tokens_at_last_compaction FROM context_compaction_states WHERE conversation_id=? AND user_id=?`, state.ConversationID, state.UserID).Scan(&readBackID, &readBackRefs, &readBackTokens); err != nil {
		return err
	}
	if readBackID != state.CanonicalSourceID || string(readBackRefs) != string(covered) || readBackTokens != state.TokensAtLastCompaction {
		return fmt.Errorf("context compaction checkpoint read-back mismatch for conversation %q", state.ConversationID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	persisted, err := s.ContextCompactionState(ctx, state.UserID, state.ConversationID)
	if err != nil {
		return err
	}
	if persisted.StateVersion != state.StateVersion || persisted.CanonicalSourceID != state.CanonicalSourceID || persisted.Strategy != state.Strategy || persisted.ProtocolVersion != state.ProtocolVersion || persisted.ProviderID != state.ProviderID || persisted.Model != state.Model || persisted.TokensAtLastCompaction != state.TokensAtLastCompaction || !sameStrings(persisted.CoveredSourceIDs, state.CoveredSourceIDs) {
		return fmt.Errorf("context compaction checkpoint failed post-commit verification")
	}
	return nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *Store) ContextCompactionState(ctx context.Context, userID, conversationID string) (ContextCompactionState, error) {
	var state ContextCompactionState
	var covered []byte
	err := s.db.QueryRowContext(ctx, `SELECT conversation_id,user_id,state_version,strategy,provider_id,model,protocol_version,canonical_source_id,covered_sources_json,tokens_at_last_compaction,updated_at FROM context_compaction_states WHERE conversation_id=? AND user_id=?`, conversationID, userID).
		Scan(&state.ConversationID, &state.UserID, &state.StateVersion, &state.Strategy, &state.ProviderID, &state.Model, &state.ProtocolVersion, &state.CanonicalSourceID, &covered, &state.TokensAtLastCompaction, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return state, domain.ErrNotFound
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(covered, &state.CoveredSourceIDs); err != nil {
		return state, fmt.Errorf("decode context compaction state source index: %w", err)
	}
	return state, nil
}

// SaveContextTail records the ordered full sources that remain outside the
// canonical summary so later turns can restore recent tool interactions.
func (s *Store) SaveContextTail(ctx context.Context, userID, conversationID string, sourceIDs []string) error {
	if userID == "" || conversationID == "" {
		return domain.ErrInvalid
	}
	unique := make([]string, 0, len(sourceIDs))
	seen := map[string]struct{}{}
	for _, sourceID := range sourceIDs {
		if sourceID == "" || strings.HasPrefix(sourceID, "message:") || strings.HasPrefix(sourceID, "semantic_summary:") {
			return domain.ErrInvalid
		}
		if _, exists := seen[sourceID]; exists {
			continue
		}
		seen[sourceID] = struct{}{}
		unique = append(unique, sourceID)
	}
	encoded, err := json.Marshal(unique)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL`, conversationID, userID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	} else if err != nil {
		return err
	}
	for _, sourceID := range unique {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM context_sources WHERE conversation_id=? AND user_id=? AND source_id=?`, conversationID, userID, sourceID).Scan(&sourceType); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("context tail source %q is unavailable: %w", sourceID, domain.ErrNotFound)
		} else if err != nil {
			return err
		} else if sourceType != "tool_result" && sourceType != "assistant_tool_calls" && sourceType != "planner_brief" {
			return fmt.Errorf("context tail source %q has unsupported type %q", sourceID, sourceType)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_context_tails(conversation_id,user_id,source_ids_json,updated_at)
VALUES(?,?,?,?) ON CONFLICT(conversation_id) DO UPDATE SET user_id=excluded.user_id,source_ids_json=excluded.source_ids_json,updated_at=excluded.updated_at`, conversationID, userID, encoded, now)
	if err != nil {
		return err
	}
	var readBack []byte
	if err := tx.QueryRowContext(ctx, `SELECT source_ids_json FROM conversation_context_tails WHERE conversation_id=? AND user_id=?`, conversationID, userID).Scan(&readBack); err != nil {
		return err
	}
	if string(readBack) != string(encoded) {
		return fmt.Errorf("context tail read-back mismatch for conversation %q", conversationID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	state, err := s.ContextTail(ctx, userID, conversationID)
	if err != nil {
		return err
	}
	if len(state.SourceIDs) != len(unique) {
		return fmt.Errorf("context tail failed post-commit verification for conversation %q", conversationID)
	}
	for index := range unique {
		if state.SourceIDs[index] != unique[index] {
			return fmt.Errorf("context tail failed post-commit verification for conversation %q", conversationID)
		}
	}
	return nil
}

func (s *Store) ContextTail(ctx context.Context, userID, conversationID string) (ContextTailState, error) {
	var state ContextTailState
	var sources []byte
	err := s.db.QueryRowContext(ctx, `SELECT t.conversation_id,t.user_id,t.source_ids_json,t.updated_at FROM conversation_context_tails t
JOIN conversations c ON c.id=t.conversation_id WHERE t.conversation_id=? AND t.user_id=? AND c.deleted_at IS NULL`, conversationID, userID).
		Scan(&state.ConversationID, &state.UserID, &sources, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return state, domain.ErrNotFound
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(sources, &state.SourceIDs); err != nil {
		return state, fmt.Errorf("decode conversation context tail: %w", err)
	}
	return state, nil
}
