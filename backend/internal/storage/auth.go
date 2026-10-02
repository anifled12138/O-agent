package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

const authLoginWindow = 15 * time.Minute
const authLoginBlock = 15 * time.Minute
const authLoginMaxAttempts = 5

func (s *Store) AuthLoginAllowed(ctx context.Context, bucketHash string, now time.Time) (bool, time.Duration, error) {
	if bucketHash == "" {
		return false, 0, domain.ErrInvalid
	}
	var blockedUntil sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT blocked_until FROM auth_login_limits WHERE bucket_hash=?`, bucketHash).Scan(&blockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return true, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if !blockedUntil.Valid || !blockedUntil.Time.After(now.UTC()) {
		return true, 0, nil
	}
	return false, blockedUntil.Time.Sub(now.UTC()), nil
}

func (s *Store) RecordAuthLoginFailure(ctx context.Context, bucketHash string, now time.Time) (time.Duration, error) {
	if bucketHash == "" {
		return 0, domain.ErrInvalid
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var windowStarted time.Time
	var attempts int
	var blockedUntil sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT window_started_at,attempts,blocked_until FROM auth_login_limits WHERE bucket_hash=?`, bucketHash).Scan(&windowStarted, &attempts, &blockedUntil)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && now.Sub(windowStarted) >= authLoginWindow) || (err == nil && blockedUntil.Valid && !blockedUntil.Time.After(now)) {
		windowStarted, attempts, blockedUntil = now, 0, sql.NullTime{}
	} else if err != nil {
		return 0, err
	}
	attempts++
	if attempts >= authLoginMaxAttempts {
		until := now.Add(authLoginBlock)
		blockedUntil = sql.NullTime{Time: until, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_login_limits(bucket_hash,window_started_at,attempts,blocked_until,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(bucket_hash) DO UPDATE SET window_started_at=excluded.window_started_at,attempts=excluded.attempts,blocked_until=excluded.blocked_until,updated_at=excluded.updated_at`, bucketHash, windowStarted, attempts, nullTimeValue(blockedUntil), now); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_login_limits WHERE updated_at<?`, now.Add(-24*time.Hour)); err != nil {
		return 0, err
	}
	var persistedAttempts int
	var persistedWindow time.Time
	var persistedBlocked sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT window_started_at,attempts,blocked_until FROM auth_login_limits WHERE bucket_hash=?`, bucketHash).Scan(&persistedWindow, &persistedAttempts, &persistedBlocked); err != nil {
		return 0, err
	}
	if persistedAttempts != attempts || !persistedWindow.Equal(windowStarted) || persistedBlocked.Valid != blockedUntil.Valid || persistedBlocked.Valid && !persistedBlocked.Time.Equal(blockedUntil.Time) {
		return 0, fmt.Errorf("login failure state did not read back: %w", domain.ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if persistedBlocked.Valid {
		return authLoginBlock, nil
	}
	return 0, nil
}

func nullTimeValue(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func (s *Store) ClearAuthLoginFailures(ctx context.Context, bucketHash string) error {
	if bucketHash == "" {
		return domain.ErrInvalid
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_login_limits WHERE bucket_hash=?`, bucketHash); err != nil {
		return err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_login_limits WHERE bucket_hash=?`, bucketHash).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return domain.ErrConflict
	}
	return nil
}

func (s *Store) AuthUserByEmail(ctx context.Context, email string) (domain.User, string, error) {
	var user domain.User
	var passwordHash string
	err := s.db.QueryRowContext(ctx, `SELECT id,email,display_name,password_hash,created_at FROM users WHERE email=?`, email).
		Scan(&user.ID, &user.Email, &user.DisplayName, &passwordHash, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, "", domain.ErrNotFound
	}
	return user, passwordHash, err
}

func (s *Store) AuthUserByID(ctx context.Context, id string) (domain.User, string, error) {
	var user domain.User
	var passwordHash string
	err := s.db.QueryRowContext(ctx, `SELECT id,email,display_name,password_hash,created_at FROM users WHERE id=?`, id).
		Scan(&user.ID, &user.Email, &user.DisplayName, &passwordHash, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, "", domain.ErrNotFound
	}
	return user, passwordHash, err
}

// SetInitialCredentials initializes the local owner or replaces a legacy hash.
// A PBKDF2 hash marks setup complete and cannot be overwritten by this path.
func (s *Store) SetInitialCredentials(ctx context.Context, userID, email, displayName, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE users SET email=?,display_name=?,password_hash=? WHERE id=? AND (password_hash='' OR password_hash NOT LIKE 'pbkdf2-sha256$%')`, email, displayName, passwordHash, userID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.ErrConflict
		}
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return domain.ErrConflict
	}
	var persisted string
	if err := tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&persisted); err != nil {
		return err
	}
	if persisted != passwordHash {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&persisted); err != nil {
		return err
	}
	if persisted != passwordHash {
		return domain.ErrConflict
	}
	return nil
}

func (s *Store) CreateAuthSession(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at<=?`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions(token_hash,user_id,expires_at,created_at) VALUES(?,?,?,?)`, tokenHash, userID, expiresAt.UTC(), now); err != nil {
		return err
	}
	var storedUser string
	var storedExpiry time.Time
	if err := tx.QueryRowContext(ctx, `SELECT user_id,expires_at FROM auth_sessions WHERE token_hash=?`, tokenHash).Scan(&storedUser, &storedExpiry); err != nil {
		return err
	}
	if storedUser != userID || !storedExpiry.Equal(expiresAt.UTC()) {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	err = s.db.QueryRowContext(ctx, `SELECT user_id FROM auth_sessions WHERE token_hash=? AND expires_at>?`, tokenHash, time.Now().UTC()).Scan(&storedUser)
	if err == nil && storedUser == userID {
		return nil
	}
	cleanupErr := s.DeleteAuthSession(ctx, tokenHash)
	if err != nil {
		return errors.Join(err, cleanupErr)
	}
	return errors.Join(domain.ErrConflict, cleanupErr)
}

func (s *Store) AuthSessionUser(ctx context.Context, tokenHash string, now time.Time) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM auth_sessions WHERE token_hash=? AND expires_at>?`, tokenHash, now.UTC()).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrUnauthorized
	}
	return userID, err
}

func (s *Store) DeleteAuthSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash=?`, tokenHash); err != nil {
		return err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE token_hash=?`, tokenHash).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return domain.ErrConflict
	}
	return nil
}
