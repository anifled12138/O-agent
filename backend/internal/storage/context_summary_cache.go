package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"axiom.local/agent/internal/domain"
)

// ContextSummaryCache stores one encrypted, evidence-reviewed semantic chunk.
// Its key is derived by the caller from the conversation, source versions,
// model/configuration, and summary schema.
type ContextSummaryCache struct {
	ConversationID string
	UserID         string
	CacheKey       string
	ConfigHash     string
	SourceIDs      []string
	ContentHash    string
	Ciphertext     []byte
	Nonce          []byte
	CreatedAt      time.Time
}

const contextSummaryCacheEntriesPerConversation = 128

func (s *Store) ReadContextSummaryCache(ctx context.Context, userID, conversationID, cacheKey string) (ContextSummaryCache, error) {
	var entry ContextSummaryCache
	if userID == "" || conversationID == "" || !validSHA256Hex(cacheKey) {
		return entry, domain.ErrInvalid
	}
	var sourceRefs []byte
	err := s.db.QueryRowContext(ctx, `SELECT sc.conversation_id,sc.user_id,sc.cache_key,sc.config_sha256,sc.source_refs_json,sc.content_sha256,sc.content_cipher,sc.content_nonce,sc.created_at
FROM context_summary_cache sc JOIN conversations c ON c.id=sc.conversation_id
WHERE sc.conversation_id=? AND sc.user_id=? AND sc.cache_key=? AND c.deleted_at IS NULL`, conversationID, userID, cacheKey).
		Scan(&entry.ConversationID, &entry.UserID, &entry.CacheKey, &entry.ConfigHash, &sourceRefs, &entry.ContentHash, &entry.Ciphertext, &entry.Nonce, &entry.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return entry, domain.ErrNotFound
	}
	if err != nil {
		return entry, err
	}
	if !validSHA256Hex(entry.ConfigHash) || !validSHA256Hex(entry.ContentHash) || len(entry.Ciphertext) == 0 || len(entry.Nonce) == 0 {
		return entry, fmt.Errorf("%w: semantic summary cache entry %q has invalid metadata", domain.ErrInvalid, cacheKey)
	}
	if err := json.Unmarshal(sourceRefs, &entry.SourceIDs); err != nil {
		return entry, fmt.Errorf("%w: decode semantic summary cache source index: %v", domain.ErrInvalid, err)
	}
	if !validUniqueSourceIDs(entry.SourceIDs) {
		return entry, fmt.Errorf("%w: semantic summary cache entry %q has invalid source references", domain.ErrInvalid, cacheKey)
	}
	return entry, nil
}

// DeleteContextSummaryCacheEntry removes one invalid derived cache value. The
// expected hash prevents cleanup from deleting a concurrently replaced entry.
func (s *Store) DeleteContextSummaryCacheEntry(ctx context.Context, userID, conversationID, cacheKey, expectedContentHash string) error {
	if userID == "" || conversationID == "" || !validSHA256Hex(cacheKey) {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM context_summary_cache WHERE conversation_id=? AND user_id=? AND cache_key=? AND content_sha256=?
AND EXISTS (SELECT 1 FROM conversations c WHERE c.id=? AND c.user_id=? AND c.deleted_at IS NULL)`, conversationID, userID, cacheKey, expectedContentHash, conversationID, userID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL)`, conversationID, userID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return domain.ErrNotFound
		}
		var currentHash string
		if err := tx.QueryRowContext(ctx, `SELECT content_sha256 FROM context_summary_cache WHERE conversation_id=? AND user_id=? AND cache_key=?`, conversationID, userID, cacheKey).Scan(&currentHash); errors.Is(err, sql.ErrNoRows) {
			// Another cleanup already removed this derived value.
		} else if err != nil {
			return err
		} else if currentHash != expectedContentHash {
			return domain.ErrConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM context_summary_cache WHERE conversation_id=? AND user_id=? AND cache_key=?)`, conversationID, userID, cacheKey).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return fmt.Errorf("%w: semantic summary cache entry %q changed before deletion was verified", domain.ErrConflict, cacheKey)
	}
	return nil
}

// SaveContextSummaryCache is insert-only for a complete source/configuration
// key. Repeated writes are accepted only when the reviewed payload and source
// scope match exactly; a conflicting concurrent result is reported.
func (s *Store) SaveContextSummaryCache(ctx context.Context, entry ContextSummaryCache) error {
	if entry.UserID == "" || entry.ConversationID == "" || !validSHA256Hex(entry.CacheKey) || !validSHA256Hex(entry.ConfigHash) || !validSHA256Hex(entry.ContentHash) || !validUniqueSourceIDs(entry.SourceIDs) || len(entry.Ciphertext) == 0 || len(entry.Nonce) == 0 {
		return domain.ErrInvalid
	}
	refs, err := json.Marshal(entry.SourceIDs)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO context_summary_cache(conversation_id,user_id,cache_key,config_sha256,source_refs_json,content_sha256,content_cipher,content_nonce,created_at)
SELECT ?,?,?,?,?,?,?,?,? FROM conversations WHERE id=? AND user_id=? AND deleted_at IS NULL
ON CONFLICT(conversation_id,cache_key) DO NOTHING`, entry.ConversationID, entry.UserID, entry.CacheKey, entry.ConfigHash, refs, entry.ContentHash, entry.Ciphertext, entry.Nonce, now, entry.ConversationID, entry.UserID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		var existingUser, existingConfig, existingHash string
		var existingRefs []byte
		if err := tx.QueryRowContext(ctx, `SELECT user_id,config_sha256,source_refs_json,content_sha256 FROM context_summary_cache WHERE conversation_id=? AND cache_key=?`, entry.ConversationID, entry.CacheKey).
			Scan(&existingUser, &existingConfig, &existingRefs, &existingHash); errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		} else if err != nil {
			return err
		} else if existingUser != entry.UserID || existingConfig != entry.ConfigHash || string(existingRefs) != string(refs) || existingHash != entry.ContentHash {
			return fmt.Errorf("semantic summary cache key %q already contains an incompatible version", entry.CacheKey)
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM context_summary_cache WHERE conversation_id=? AND user_id=? AND cache_key<>?
AND cache_key NOT IN (SELECT cache_key FROM context_summary_cache WHERE conversation_id=? AND user_id=? AND cache_key<>?
ORDER BY created_at DESC,cache_key DESC LIMIT ?)`, entry.ConversationID, entry.UserID, entry.CacheKey, entry.ConversationID, entry.UserID, entry.CacheKey, contextSummaryCacheEntriesPerConversation-1)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	persisted, err := s.ReadContextSummaryCache(ctx, entry.UserID, entry.ConversationID, entry.CacheKey)
	if err != nil {
		return fmt.Errorf("read back semantic summary cache entry: %w", err)
	}
	if persisted.ConfigHash != entry.ConfigHash || persisted.ContentHash != entry.ContentHash || !sameStrings(persisted.SourceIDs, entry.SourceIDs) || (rows == 1 && (!bytes.Equal(persisted.Ciphertext, entry.Ciphertext) || !bytes.Equal(persisted.Nonce, entry.Nonce))) {
		return fmt.Errorf("semantic summary cache entry %q failed post-commit verification", entry.CacheKey)
	}
	return nil
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validUniqueSourceIDs(sourceIDs []string) bool {
	if len(sourceIDs) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		if sourceID == "" {
			return false
		}
		if _, exists := seen[sourceID]; exists {
			return false
		}
		seen[sourceID] = struct{}{}
	}
	return true
}
